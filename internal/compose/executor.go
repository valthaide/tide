package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/valthaide/conops-private/internal/fleetcheck"
	"github.com/valthaide/conops-private/internal/fsperm"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/redact"
	"github.com/valthaide/conops-private/internal/repoauth"
	"github.com/valthaide/conops-private/internal/secrets"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// ComposeExecutor handles Docker Compose operations.
type ComposeExecutor struct {
	WorkDir  string
	ToolsDir string
	Logger   *slog.Logger
	Secrets  *secrets.Manager

	toolchainMu      sync.Mutex
	dockerResolution dockerCommandResolution
	resolutionAt     time.Time

	projectsMu sync.Mutex
	projects   map[string]string
	portsMu    sync.Mutex
	portOwner  map[int]string
	digestMu   sync.Mutex
	expected   map[string]map[string]string
	PullGate   SlotGate
}

// ProjectRuntimeState summarizes runtime state for one compose project.
type ProjectRuntimeState struct {
	ContainerCount int
	RunningCount   int
	ExitedCount    int
	UnhealthyCount int
	StartingCount  int
	// FlappingCount is containers that stay up only by restarting.
	FlappingCount int
	// NotReady lists container names that are not ready (unhealthy, starting, exited, or flapping).
	NotReady []string
	// Failure is a specific reason, such as "oneshot_failed: init exit 1" or "restarting: php 4 restarts".
	Failure string
	// Oneshot is a short status such as "migrate completed exit 0".
	Oneshot string
}

// Ready reports whether every service container is running and past healthcheck start.
// Containers without a healthcheck are ready once they are up.
func (s ProjectRuntimeState) Ready() bool {
	if s.ContainerCount == 0 {
		return false
	}
	return s.RunningCount == s.ContainerCount && s.ExitedCount == 0 && s.UnhealthyCount == 0 && s.StartingCount == 0 && s.FlappingCount == 0
}

// IsHealthy reports whether all tracked service containers are running and healthy.
func (s ProjectRuntimeState) IsHealthy() bool {
	if s.ContainerCount == 0 {
		return false
	}
	return s.RunningCount == s.ContainerCount && s.ExitedCount == 0 && s.UnhealthyCount == 0 && s.FlappingCount == 0
}

// NewComposeExecutor creates a new executor.
func NewComposeExecutor(logger *slog.Logger) *ComposeExecutor {
	return &ComposeExecutor{
		WorkDir:  "./.conops-runtime",
		ToolsDir: "./.conops-tools",
		Logger:   logger,
	}
}

// Apply executes the compose file.
func (e *ComposeExecutor) Apply(
	ctx context.Context,
	appID, content string,
	envVars map[string]string,
	repoURL, branch, composePath, commitHash string,
	repoAuthMethod string,
	repoSecret []byte,
	onProgress func(string),
) (string, error) {
	return e.ApplyImages(ctx, appID, content, envVars, repoURL, branch, composePath, commitHash, repoAuthMethod, repoSecret, ApplyOptions{}, onProgress)
}

// ApplyImages executes the compose file with fleet vars and the build policy.
func (e *ComposeExecutor) ApplyImages(
	ctx context.Context,
	appID, content string,
	envVars map[string]string,
	repoURL, branch, composePath, commitHash string,
	repoAuthMethod string,
	repoSecret []byte,
	opts ApplyOptions,
	onProgress func(string),
) (string, error) {
	// Git, compose config, registry login and image inspect stay on the sync
	// deadline. Pull and one-shot waits have their own clocks, started later.
	callerCtx := ctx
	if opts.SyncTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(callerCtx, opts.SyncTimeout)
		defer cancel()
	}
	var syncLog strings.Builder
	emitProgress := func() {
		if onProgress != nil {
			onProgress(strings.TrimSpace(syncLog.String()))
		}
	}

	appDir := filepath.Join(e.WorkDir, appID)
	appDirAbs, err := filepath.Abs(appDir)
	if err != nil {
		appendLogSection(&syncLog, "Sync setup")
		appendLogLine(&syncLog, "failed to resolve runtime directory")
		appendLogLine(&syncLog, err.Error())
		emitProgress()
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("resolve app dir failed: %w", err)
	}
	if err := fsperm.EnsureSharedDir(appDirAbs); err != nil {
		appendLogSection(&syncLog, "Sync setup")
		appendLogLine(&syncLog, "failed to create runtime directory")
		appendLogLine(&syncLog, err.Error())
		emitProgress()
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("failed to create app dir: %w", err)
	}

	if strings.TrimSpace(repoURL) == "" {
		appendLogSection(&syncLog, "Validation")
		appendLogLine(&syncLog, "repo url is empty")
		emitProgress()
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("repo url is empty")
	}
	if strings.TrimSpace(composePath) == "" {
		appendLogSection(&syncLog, "Validation")
		appendLogLine(&syncLog, "compose path is empty")
		emitProgress()
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("compose path is empty")
	}
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}

	appendLogSection(&syncLog, "Sync started")
	appendLogLine(&syncLog, fmt.Sprintf("app_id: %s", appID))
	appendLogLine(&syncLog, fmt.Sprintf("repository: %s", redact.StripUserinfoFromURL(repoURL)))
	appendLogLine(&syncLog, fmt.Sprintf("branch: %s", branch))
	if strings.TrimSpace(commitHash) != "" {
		appendLogLine(&syncLog, fmt.Sprintf("target_commit: %s", commitHash))
	} else {
		appendLogLine(&syncLog, "target_commit: latest on branch")
	}
	emitProgress()

	appendLogSection(&syncLog, "Docker preflight")
	preflight, err := e.ensureDockerPreflight(ctx)
	if err != nil {
		appendLogLine(&syncLog, "failed")
		appendLogLine(&syncLog, err.Error())
		emitProgress()
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("docker preflight failed: %w", err)
	}
	for _, line := range preflight.LogLines() {
		appendLogLine(&syncLog, line)
	}
	emitProgress()

	repoDir := filepath.Join(appDirAbs, "repo")
	e.Logger.Info("Preparing repo", "app_id", appID, "repo", repoURL, "branch", branch, "commit", commitHash, "dir", repoDir)
	repoLog, err := e.prepareRepo(ctx, appDirAbs, repoDir, repoURL, branch, commitHash, repoAuthMethod, repoSecret)
	appendLogBlock(&syncLog, repoLog)
	emitProgress()
	if err != nil {
		return redact.String(strings.TrimSpace(syncLog.String())), syncerr.Wrap(syncerr.CodeGitFetch, syncerr.StageFetch, fmt.Errorf("prepare repo failed: %w", err))
	}
	if permErr := fsperm.RelaxCheckoutTree(repoDir); permErr != nil && e.Logger != nil {
		e.Logger.Warn("Failed to relax checkout permissions", "app_id", appID, "error", permErr)
	}
	for _, private := range []string{filepath.Join(appDirAbs, ".ssh"), filepath.Join(appDirAbs, ".envs")} {
		_ = fsperm.HardenPrivateSubtree(private)
	}

	if e.Secrets != nil {
		appendLogSection(&syncLog, "Secrets")
		resolved, resErr := e.Secrets.ResolveEnvs(ctx, envVars)
		if resErr != nil {
			appendLogLine(&syncLog, resErr.Error())
			emitProgress()
			return redact.String(strings.TrimSpace(syncLog.String())), syncerr.Wrap(syncerr.CodeSecretsVault, syncerr.StageSecrets, resErr)
		}
		envVars = resolved
		if prepErr := e.Secrets.PrepareRepo(ctx, repoDir); prepErr != nil {
			appendLogLine(&syncLog, prepErr.Error())
			emitProgress()
			return redact.String(strings.TrimSpace(syncLog.String())), syncerr.Wrap(syncerr.CodeSecretsSOPS, syncerr.StageSecrets, prepErr)
		}
		appendLogLine(&syncLog, "ok")
		emitProgress()
	}

	composeFullPath := filepath.Join(repoDir, composePath)
	composeDir := filepath.Dir(composeFullPath)
	if _, err := os.Stat(composeDir); err != nil {
		appendLogSection(&syncLog, "Compose file")
		appendLogLine(&syncLog, fmt.Sprintf("compose directory does not exist: %s", composeDir))
		appendLogLine(&syncLog, err.Error())
		emitProgress()
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("compose dir not found: %w", err)
	}

	// Prepare env var override files if needed (must happen after composeDir is determined)
	var envFilesCleanup func()
	var overrideArgs []string

	wroteCompose := false
	if strings.TrimSpace(content) != "" {
		if err := os.WriteFile(composeFullPath, []byte(content), 0644); err != nil {
			appendLogSection(&syncLog, "Compose file")
			appendLogLine(&syncLog, fmt.Sprintf("failed to write compose file: %s", composeFullPath))
			appendLogLine(&syncLog, err.Error())
			emitProgress()
			return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("failed to write compose file: %w", err)
		}
		wroteCompose = true
	} else {
		if _, err := os.Stat(composeFullPath); err != nil {
			appendLogSection(&syncLog, "Compose file")
			appendLogLine(&syncLog, fmt.Sprintf("compose file not found: %s", composeFullPath))
			appendLogLine(&syncLog, err.Error())
			emitProgress()
			return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("compose file not found: %w", err)
		}
	}
	composeFileName := filepath.Base(composeFullPath)
	projectName := strings.TrimSpace(opts.Project)
	if projectName == "" {
		projectName = e.projectFor(appID)
	}
	e.RememberProject(appID, projectName)

	// Prepare env var override files if needed (must use composeDir as base)
	if len(envVars) > 0 {
		var envErr error
		overrideArgs, envFilesCleanup, envErr = e.prepareEnvOverrides(composeDir, envVars)
		if envErr != nil {
			appendLogSection(&syncLog, "Environment preparation")
			appendLogLine(&syncLog, "failed to prepare environment variables")
			appendLogLine(&syncLog, envErr.Error())
			emitProgress()
			return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("env prepare failed: %w", envErr)
		}
		defer envFilesCleanup()
	}

	appendLogSection(&syncLog, "Compose file")
	appendLogLine(&syncLog, fmt.Sprintf("path: %s", composeFullPath))
	appendLogLine(&syncLog, fmt.Sprintf("written_from_request: %t", wroteCompose))
	emitProgress()

	e.Logger.Info(
		"Compose file ready",
		"app_id", appID,
		"path", composeFullPath,
		"bytes", len(content),
		"written", wroteCompose,
	)

	prep, prepErr := e.prepareImageSync(ctx, &syncLog, projectName, composeFileName, composeDir, composeFullPath, overrideArgs, opts, func(string) {})
	emitProgress()
	if prepErr != nil {
		prep.clean()
		return redact.String(strings.TrimSpace(syncLog.String())), prepErr
	}
	defer prep.clean()

	// Pull images. Login, when configured, lives only in prep.env for this pull.
	appendLogSection(&syncLog, "Docker image pull")
	if e.pinnedDigestsLocal(ctx, prep.images) {
		appendLogLine(&syncLog, "pinned digests already local; pull skipped")
		if e.Logger != nil {
			e.Logger.Info("Pull skipped; pinned digests are local", "app_id", appID)
		}
	} else {
		if e.Logger != nil {
			e.Logger.Info("Pulling images", "app_id", appID)
		}
		pullArgs := []string{"compose", "-p", projectName, "-f", composeFileName}
		if len(overrideArgs) > 0 {
			pullArgs = append(pullArgs, overrideArgs...)
		}
		pullArgs = append(pullArgs, "pull")

		// The idle clock starts after a pull slot is acquired. Time spent
		// waiting for CONOPS_PULL_PARALLEL is not pull silence.
		pullCtx, pullRelease, bumpPull, waitErr := e.beginPull(callerCtx, opts.PullIdle)
		if waitErr != nil {
			return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("waiting for pull slot: %w", waitErr)
		}
		pullOut, err := e.runCommandWithTranscript(
			pullCtx,
			&syncLog,
			"docker",
			pullArgs,
			composeDir,
			prep.env,
			func(chunk string) {
				bumpPull()
				if onProgress != nil {
					onProgress(chunk)
				}
			},
		)
		pullRelease()
		if err != nil {
			prep.clean()
			if pullCtx.Err() != nil && callerCtx.Err() == nil {
				msg := "timeout: pull " + strings.Join(prep.images, ",") + " after " + opts.PullIdle.String()
				return redact.String(strings.TrimSpace(syncLog.String())), syncerr.New(syncerr.CodeTimeout, syncerr.StagePull, msg)
			}
			return redact.String(strings.TrimSpace(syncLog.String())), pullFailedError(pullOut+"\n"+err.Error(), prep.images)
		}
	}
	// Drop the docker config before up. Images are already local.
	prep.clean()
	prep.cleanup = func() {}
	if prep.env != nil {
		delete(prep.env, "DOCKER_CONFIG")
	}

	upCtx := ctx
	if opts.SyncTimeout > 0 {
		var upCancel context.CancelFunc
		upCtx, upCancel = context.WithTimeout(context.Background(), opts.SyncTimeout)
		defer upCancel()
	}

	// Up detached
	appendLogSection(&syncLog, "Compose apply")
	if prep.noBuild {
		appendLogLine(&syncLog, "starting pulled images without build")
	} else {
		appendLogLine(&syncLog, "build output appears below when services require a build")
	}
	e.Logger.Info("Applying configuration", "app_id", appID)

	upArgs := []string{"compose", "-p", projectName, "-f", composeFileName}
	if len(overrideArgs) > 0 {
		upArgs = append(upArgs, overrideArgs...)
	}
	upArgs = append(upArgs, "up", "-d", "--remove-orphans")
	if !prep.noBuild {
		upArgs = append(upArgs, "--build")
	}

	_, err = e.runCommandWithTranscript(
		upCtx,
		&syncLog,
		"docker",
		upArgs,
		composeDir,
		prep.env,
		onProgress,
	)
	if err != nil {
		if reason := e.oneshotFailure(upCtx, projectName); reason != "" {
			return redact.String(strings.TrimSpace(syncLog.String())), syncerr.New(syncerr.CodeOneshotFailed, syncerr.StageCompose, reason)
		}
		return redact.String(strings.TrimSpace(syncLog.String())), fmt.Errorf("up failed: %w", err)
	}

	if len(prep.rerun) > 0 {
		reArgs := []string{"compose", "-p", projectName, "-f", composeFileName}
		if len(overrideArgs) > 0 {
			reArgs = append(reArgs, overrideArgs...)
		}
		reArgs = append(reArgs, "up", "-d", "--no-deps", "--force-recreate")
		reArgs = append(reArgs, prep.rerun...)
		if _, rerunErr := e.runCommandWithTranscript(upCtx, &syncLog, "docker", reArgs, composeDir, prep.env, onProgress); rerunErr != nil && e.Logger != nil {
			e.Logger.Warn("one-shot recreate failed", "project", projectName, "error", rerunErr)
		}
	}
	shotTimeout := opts.OneshotTimeout
	if shotTimeout <= 0 {
		shotTimeout = 10 * time.Minute
	}
	shotCtx, shotCancel := context.WithTimeout(context.Background(), shotTimeout)
	defer shotCancel()
	if err := e.waitOneshots(shotCtx, projectName, shotTimeout); err != nil {
		return redact.String(strings.TrimSpace(syncLog.String())), err
	}

	appendLogSection(&syncLog, "Sync completed")
	appendLogLine(&syncLog, "application reconciled successfully")
	emitProgress()
	return redact.String(strings.TrimSpace(syncLog.String())), nil
}

// RestartService restarts one compose service for an app (config bind-mount refresh).
func (e *ComposeExecutor) RestartService(ctx context.Context, appID, composePath, service string) (string, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		return "", fmt.Errorf("service name is required for restart")
	}
	projectName := e.projectFor(appID)
	appDirAbs, err := filepath.Abs(filepath.Join(e.WorkDir, appID))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(composePath) == "" {
		composePath = "compose.yaml"
	}
	composeFullPath := filepath.Join(appDirAbs, "repo", composePath)
	composeDir := filepath.Dir(composeFullPath)
	composeFileName := filepath.Base(composeFullPath)
	out, err := e.runCommand(
		ctx,
		"docker",
		[]string{"compose", "-p", projectName, "-f", composeFileName, "restart", service},
		composeDir,
		nil,
		nil,
	)
	return redact.String(out), err
}

// HTTPReload POSTs to a reload endpoint (e.g. Prometheus /-/reload).
func (e *ComposeExecutor) HTTPReload(ctx context.Context, reloadURL string) error {
	reloadURL = strings.TrimSpace(reloadURL)
	if reloadURL == "" {
		return fmt.Errorf("reload_url is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reloadURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("reload %s returned %s", reloadURL, resp.Status)
	}
	return nil
}

// Destroy tears down app containers and networks.
// Images that no remaining container references are removed.
// Named volumes are removed only when pruneVolumes is true.
func (e *ComposeExecutor) Destroy(ctx context.Context, appID, composePath string, envVars map[string]string, pruneVolumes bool) (string, error) {
	projectName := e.projectFor(appID)
	defer e.ReleasePorts(projectName)
	appDirAbs, err := filepath.Abs(filepath.Join(e.WorkDir, appID))
	if err != nil {
		return "", fmt.Errorf("resolve app dir failed: %w", err)
	}

	repoDir := filepath.Join(appDirAbs, "repo")
	if strings.TrimSpace(composePath) == "" {
		composePath = "compose.yaml"
	}
	composeFullPath := filepath.Join(repoDir, composePath)
	composeDir := filepath.Dir(composeFullPath)
	composeFileName := filepath.Base(composeFullPath)

	var outputs []string
	downAttempted := false
	imageRefs := e.projectImageRefs(ctx, projectName)
	var keptVolumes []string
	if !pruneVolumes {
		keptVolumes = e.projectVolumeNames(ctx, projectName)
	}

	if fileInfo, statErr := os.Stat(composeFullPath); statErr == nil && !fileInfo.IsDir() {
		downAttempted = true
		e.Logger.Info("Stopping app stack", "app_id", appID, "project", projectName, "compose_file", composeFullPath, "prune_volumes", pruneVolumes)
		downOut, downErr := e.runCommand(
			ctx,
			"docker",
			composeDownArgs(projectName, composeFileName, pruneVolumes),
			composeDir,
			envVars,
			nil,
		)
		if strings.TrimSpace(downOut) != "" {
			outputs = append(outputs, downOut)
		}
		if downErr != nil && e.Logger != nil {
			e.Logger.Warn("Compose down failed; proceeding to force cleanup", "app_id", appID, "error", downErr)
		}
	}
	if !pruneVolumes && len(keptVolumes) > 0 && e.Logger != nil {
		e.Logger.Info("Named volumes kept", "app_id", appID, "volumes", strings.Join(keptVolumes, ","))
	}

	// Fallback cleanup for any lingering containers (including legacy runs before project naming).
	legacyWorkingDir := composeDir
	containerIDs, listErr := e.listContainerIDsForCleanup(ctx, projectName, legacyWorkingDir)
	if listErr != nil {
		return strings.Join(outputs, "\n"), listErr
	}
	if len(containerIDs) > 0 {
		e.Logger.Info("Removing lingering containers", "app_id", appID, "containers", len(containerIDs))
		args := append([]string{"rm", "-f"}, containerIDs...)
		rmOut, rmErr := e.runCommand(ctx, "docker", args, appDirAbs, nil, nil)
		if strings.TrimSpace(rmOut) != "" {
			outputs = append(outputs, rmOut)
		}
		if rmErr != nil {
			return strings.Join(outputs, "\n"), fmt.Errorf("docker rm failed: %w", rmErr)
		}
	}

	if rmOut := e.removeUnusedImages(ctx, appID, imageRefs); strings.TrimSpace(rmOut) != "" {
		outputs = append(outputs, rmOut)
	}

	if !downAttempted && len(containerIDs) == 0 && e.Logger != nil {
		e.Logger.Info("No running resources found for app", "app_id", appID, "project", projectName)
	}
	if removeErr := os.RemoveAll(appDirAbs); removeErr != nil {
		e.Logger.Warn("Failed to remove app runtime directory", "app_id", appID, "dir", appDirAbs, "error", removeErr)
	}

	return strings.Join(outputs, "\n"), nil
}

func (e *ComposeExecutor) projectVolumeNames(ctx context.Context, projectName string) []string {
	out, err := e.runCommand(
		ctx,
		"docker",
		[]string{"volume", "ls", "-q", "--filter", "label=com.docker.compose.project=" + projectName},
		e.runtimeWorkDir(),
		nil,
		nil,
	)
	if err != nil {
		return nil
	}
	return uniqueImageRefs(out)
}

func (e *ComposeExecutor) projectImageRefs(ctx context.Context, projectName string) []string {
	out, err := e.runCommand(
		ctx,
		"docker",
		[]string{
			"ps", "-a",
			"--filter", "label=com.docker.compose.project=" + projectName,
			"--format", "{{.Image}}",
		},
		e.runtimeWorkDir(),
		nil,
		nil,
	)
	if err != nil {
		if e.Logger != nil {
			e.Logger.Warn("Failed to list project images before cleanup", "project", projectName, "error", err)
		}
		return nil
	}
	return uniqueImageRefs(out)
}

// removeUnusedImages deletes refs that no remaining container uses.
// A docker error while checking a ref keeps that image.
func (e *ComposeExecutor) removeUnusedImages(ctx context.Context, appID string, refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	var still []string
	for _, ref := range refs {
		out, err := e.runCommand(
			ctx,
			"docker",
			[]string{"ps", "-a", "--filter", "ancestor=" + ref, "-q"},
			e.runtimeWorkDir(),
			nil,
			nil,
		)
		if err != nil || strings.TrimSpace(out) != "" {
			still = append(still, ref)
			if err != nil && e.Logger != nil {
				e.Logger.Warn("Skipping image remove; could not check other containers", "app_id", appID, "image", ref, "error", err)
			}
		}
	}
	var outputs []string
	for _, ref := range imagesSafeToRemove(refs, still) {
		if e.Logger != nil {
			e.Logger.Info("Removing image used only by deleted app", "app_id", appID, "image", ref)
		}
		out, err := e.runCommand(ctx, "docker", []string{"rmi", ref}, e.runtimeWorkDir(), nil, nil)
		if strings.TrimSpace(out) != "" {
			outputs = append(outputs, out)
		}
		if err != nil && e.Logger != nil {
			e.Logger.Warn("Failed to remove image", "app_id", appID, "image", ref, "error", err)
		}
	}
	return strings.Join(outputs, "\n")
}

// RememberProject records the compose project name for an app id.
func (e *ComposeExecutor) RememberProject(appID, project string) {
	appID = strings.TrimSpace(appID)
	project = strings.TrimSpace(project)
	if e == nil || appID == "" || project == "" {
		return
	}
	e.projectsMu.Lock()
	if e.projects == nil {
		e.projects = map[string]string{}
	}
	previous := e.projects[appID]
	e.projects[appID] = project
	e.projectsMu.Unlock()
	if previous != "" && previous != project {
		e.ReleasePorts(previous)
	}
}

func (e *ComposeExecutor) projectFor(appID string) string {
	if e != nil {
		e.projectsMu.Lock()
		project := ""
		if e.projects != nil {
			project = e.projects[appID]
		}
		e.projectsMu.Unlock()
		if project != "" {
			return project
		}
	}
	return composeProjectName(appID)
}

// ProjectHealth returns runtime health for one compose project.
// project is the compose project name, not the app id.
func (e *ComposeExecutor) ProjectHealth(ctx context.Context, project string) (ProjectRuntimeState, error) {
	snap, err := e.SnapshotProjects(ctx)
	if err != nil {
		return ProjectRuntimeState{}, err
	}
	return snap[strings.TrimSpace(project)], nil
}

// SnapshotProjects captures compose runtime status from Docker for all projects.
func (e *ComposeExecutor) SnapshotProjects(ctx context.Context) (map[string]ProjectRuntimeState, error) {
	obs, err := e.observeContainers(ctx)
	if err != nil {
		return nil, err
	}
	return foldObservations(obs, time.Now()), nil
}

func (e *ComposeExecutor) prepareRepo(ctx context.Context, appDir, repoDir, repoURL, branch, commitHash, repoAuthMethod string, repoSecret []byte) (string, error) {
	var repoLog strings.Builder
	appendLogSection(&repoLog, "Repository sync")

	cloneURL, gitEnv, cleanup, err := e.buildGitAuth(appDir, repoURL, repoAuthMethod, repoSecret)
	if err != nil {
		appendLogLine(&repoLog, "failed to configure git auth environment")
		appendLogLine(&repoLog, redact.String(err.Error()))
		noteGitFetch(repoURL, err)
		return redact.String(strings.TrimSpace(repoLog.String())), err
	}
	defer cleanup()

	gitDir := filepath.Join(repoDir, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		appendLogLine(&repoLog, "repository cache missing; cloning fresh copy")
		err := e.cloneRepo(ctx, &repoLog, appDir, repoDir, cloneURL, branch, commitHash, gitEnv)
		noteGitFetch(repoURL, err)
		return redact.String(strings.TrimSpace(repoLog.String())), err
	}

	if cleanErr := e.ensureCleanOriginURL(ctx, repoDir, cloneURL, gitEnv); cleanErr != nil {
		appendLogLine(&repoLog, "failed to sanitize remote.origin.url")
		appendLogLine(&repoLog, redact.String(cleanErr.Error()))
		return redact.String(strings.TrimSpace(repoLog.String())), cleanErr
	}

	appendLogLine(&repoLog, "repository cache found; fetching latest refs")
	if err := e.updateRepo(ctx, &repoLog, repoDir, branch, commitHash, gitEnv); err != nil {
		if isDirtyWorktreeErr(err) {
			appendLogLine(&repoLog, redact.String(err.Error()))
			return redact.String(strings.TrimSpace(repoLog.String())), err
		}
		// The cache exists but is unusable: a partially deleted checkout, an
		// interrupted clone from a previous timeout, or a corrupt object store.
		// Discarding and re-cloning recovers from all of them, where returning
		// here leaves the app stuck in error until someone syncs it by hand.
		appendLogLine(&repoLog, "repository cache unusable; discarding and re-cloning")
		appendLogLine(&repoLog, err.Error())
		if rmErr := os.RemoveAll(repoDir); rmErr != nil {
			appendLogLine(&repoLog, "failed to discard repository cache")
			noteGitFetch(repoURL, err)
			return redact.String(strings.TrimSpace(repoLog.String())), fmt.Errorf("discard repo cache %s: %w", repoDir, rmErr)
		}
		cloneErr := e.cloneRepo(ctx, &repoLog, appDir, repoDir, cloneURL, branch, commitHash, gitEnv)
		noteGitFetch(repoURL, cloneErr)
		return redact.String(strings.TrimSpace(repoLog.String())), cloneErr
	}

	noteGitFetch(repoURL, nil)
	return redact.String(strings.TrimSpace(repoLog.String())), nil
}

func noteGitFetch(repoURL string, err error) {
	if isDirtyWorktreeErr(err) {
		return
	}
	metrics.ObserveGitFetch(repoURL, err)
}

// ensureCleanOriginURL rewrites remote.origin.url if a previous clone embedded userinfo.
func (e *ComposeExecutor) ensureCleanOriginURL(ctx context.Context, repoDir, cleanURL string, gitEnv map[string]string) error {
	cleanURL = repoauth.CleanHTTPSURL(cleanURL)
	if cleanURL == "" {
		return nil
	}
	out, err := e.runCommand(ctx, "git", []string{"config", "--get", "remote.origin.url"}, repoDir, gitEnv, nil)
	if err != nil {
		// Missing origin is fine; clone/fetch paths set it.
		return nil
	}
	current := strings.TrimSpace(out)
	if current == "" || current == cleanURL {
		return nil
	}
	if !strings.Contains(current, "@") {
		return nil
	}
	_, setErr := e.runCommand(ctx, "git", []string{"remote", "set-url", "origin", cleanURL}, repoDir, gitEnv, nil)
	return setErr
}

// cloneRepo clones into repoDir, which must not already exist.
func (e *ComposeExecutor) cloneRepo(ctx context.Context, repoLog *strings.Builder, appDir, repoDir, repoURL, branch, commitHash string, gitEnv map[string]string) error {
	cloneArgs := []string{"clone", "--branch", branch}
	if commitHash == "" {
		cloneArgs = append(cloneArgs, "--depth", "1")
	}
	cloneArgs = append(cloneArgs, repoURL, repoDir)
	if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", cloneArgs, appDir, gitEnv, nil); err != nil {
		return err
	}
	if commitHash != "" {
		if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"checkout", commitHash}, repoDir, gitEnv, nil); err != nil {
			return err
		}
	}
	return nil
}

// updateRepo brings an existing checkout up to date. Any error here is treated
// by the caller as "this cache is not salvageable" rather than a hard failure,
// except dirty_worktree (refuse mode) which must not trigger re-clone.
func (e *ComposeExecutor) updateRepo(ctx context.Context, repoLog *strings.Builder, repoDir, branch, commitHash string, gitEnv map[string]string) error {
	if err := e.handleDirtyWorktree(ctx, repoLog, repoDir, gitEnv); err != nil {
		return err
	}
	if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"fetch", "origin"}, repoDir, gitEnv, nil); err != nil {
		return err
	}

	rev := "origin/" + branch
	if commitHash != "" {
		if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"fetch", "origin", commitHash}, repoDir, gitEnv, nil); err != nil {
			return err
		}
		rev = commitHash
	}
	// Update tracked files in place so a single-file bind mount keeps its inode.
	if err := e.materializeInPlace(ctx, repoLog, repoDir, rev, gitEnv); err != nil {
		return err
	}
	if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"reset", "--mixed", rev}, repoDir, gitEnv, nil); err != nil {
		return err
	}

	if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"clean", "-fd"}, repoDir, gitEnv, nil); err != nil {
		return err
	}

	return nil
}

func isDirtyWorktreeErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "dirty worktree")
}

func (e *ComposeExecutor) handleDirtyWorktree(ctx context.Context, repoLog *strings.Builder, repoDir string, gitEnv map[string]string) error {
	out, err := e.runCommand(ctx, "git", []string{"status", "--porcelain"}, repoDir, gitEnv, nil)
	if err != nil {
		return nil // if status fails, let fetch/reset surface the real problem
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	appHint := filepath.Base(filepath.Dir(repoDir))
	appendLogLine(repoLog, "dirty worktree; resetting checkout to the requested revision")
	metrics.ObserveDirtyWorktree(appHint, "reset")
	if e.Logger != nil {
		e.Logger.Info("Dirty worktree; resetting checkout to the requested revision", "app", appHint)
	}
	return nil
}

// prepareEnvOverrides creates a compose override file with environment variables.
// It parses the raw env content and injects it into the "environment" section of the override file.
// This ensures that ConOps values take precedence over default values defined in the base compose file's "environment" section.
func (e *ComposeExecutor) prepareEnvOverrides(appDir string, serviceEnvs map[string]string) ([]string, func(), error) {
	if len(serviceEnvs) == 0 {
		return nil, func() {}, nil
	}

	envDir := filepath.Join(appDir, ".envs")
	if err := os.MkdirAll(envDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("failed to create env dir: %w", err)
	}

	var filesToRemove []string
	cleanup := func() {
		for _, f := range filesToRemove {
			_ = os.Remove(f)
		}
		_ = os.Remove(envDir) // remove dir if empty
	}

	// Create the docker-compose.override.yml
	// We construct it manually to avoid external yaml dependencies.
	// We use the "environment" list syntax to ensure precedence over base config.

	var override strings.Builder
	override.WriteString("services:\n")

	for serviceName, rawEnv := range serviceEnvs {
		if strings.TrimSpace(rawEnv) == "" {
			continue
		}

		override.WriteString(fmt.Sprintf("  %s:\n", serviceName))
		override.WriteString("    environment:\n")

		// Parse lines from the env content
		lines := strings.Split(rawEnv, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			// Simple key=value validation
			// We quote the whole string to be safe in YAML list
			// But we need to escape double quotes inside the string if we use double quotes
			// Using single quotes is safer for shell-like vars unless they contain single quotes

			// Actually, raw list item in YAML:
			// - KEY=VALUE
			// If VALUE contains special chars, we might need quoting.
			// Let's rely on the fact that these are env vars.

			// We'll use double quotes and escape internal double quotes.
			escaped := strings.ReplaceAll(line, "\"", "\\\"")
			override.WriteString(fmt.Sprintf("      - \"%s\"\n", escaped))
		}
	}

	overridePath := filepath.Join(envDir, "docker-compose.override.yml")
	if err := os.WriteFile(overridePath, []byte(override.String()), 0600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to write override file: %w", err)
	}
	filesToRemove = append(filesToRemove, overridePath)

	overrideAbs, err := filepath.Abs(overridePath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	return []string{"-f", overrideAbs}, cleanup, nil
}

// buildGitAuth returns a clean clone URL (never embeds credentials), git env, and cleanup.
func (e *ComposeExecutor) buildGitAuth(appDir, repoURL, method string, secret []byte) (cloneURL string, env map[string]string, cleanup func(), err error) {
	cloneURL = repoauth.CleanHTTPSURL(repoURL)
	method = repoauth.NormalizeMethod(method)
	noop := func() {}

	switch method {
	case repoauth.MethodToken:
		token := repoauth.ResolveGitToken(string(secret))
		if token == "" {
			return "", nil, noop, fmt.Errorf("git token is empty")
		}
		redact.Default.AddSecret(token)
		clean, authEnv, authErr := repoauth.HTTPSAuthEnv(repoURL, token)
		if authErr != nil {
			return "", nil, noop, authErr
		}
		return clean, authEnv, noop, nil
	case repoauth.MethodDeployKey:
		if len(secret) == 0 {
			return "", nil, noop, fmt.Errorf("deploy key is empty")
		}
		knownHostsPath, khErr := repoauth.ResolveKnownHostsForRepo(repoURL)
		if khErr != nil {
			return "", nil, noop, khErr
		}
		sshDir := filepath.Join(appDir, ".ssh")
		if err := os.MkdirAll(sshDir, 0700); err != nil {
			return "", nil, noop, fmt.Errorf("failed to create ssh dir: %w", err)
		}
		keyPath := filepath.Join(sshDir, "deploy_key")
		if err := os.WriteFile(keyPath, secret, 0600); err != nil {
			return "", nil, noop, fmt.Errorf("failed to write deploy key file: %w", err)
		}
		cleanup = func() { _ = os.Remove(keyPath) }
		env = map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
			"GIT_SSH_COMMAND":     repoauth.BuildSSHCommand(keyPath, knownHostsPath),
		}
		return cloneURL, env, cleanup, nil
	default:
		return cloneURL, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, noop, nil
	}
}

func appendLogSection(builder *strings.Builder, title string) {
	if builder.Len() > 0 {
		builder.WriteString("\n\n")
	}
	builder.WriteString("=== ")
	builder.WriteString(strings.TrimSpace(title))
	builder.WriteString(" ===\n")
}

func appendLogLine(builder *strings.Builder, line string) {
	builder.WriteString(strings.TrimSpace(line))
	builder.WriteString("\n")
}

func appendLogBlock(builder *strings.Builder, block string) {
	block = strings.TrimSpace(block)
	if block == "" {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n\n")
	}
	builder.WriteString(block)
}

func appendCommandOutput(builder *strings.Builder, output string) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		builder.WriteString("(no output)\n")
		return
	}
	builder.WriteString(trimmed)
	builder.WriteString("\n")
}

func formatCommand(cmd string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, cmd)
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t") {
			parts = append(parts, fmt.Sprintf("%q", arg))
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

func (e *ComposeExecutor) runCommandWithTranscript(
	ctx context.Context,
	transcript *strings.Builder,
	cmd string,
	args []string,
	workDir string,
	env map[string]string,
	onProgress func(string),
) (string, error) {
	var mu sync.Mutex
	appendToTranscript := func(value string) {
		mu.Lock()
		transcript.WriteString(value)
		mu.Unlock()
	}
	snapshot := func() string {
		mu.Lock()
		value := strings.TrimSpace(transcript.String())
		mu.Unlock()
		return value
	}

	command := redact.String(formatCommand(cmd, args))
	appendToTranscript("$ " + command + "\n")
	if onProgress != nil {
		onProgress(redact.String(snapshot()))
	}

	sawOutput := false
	output, err := e.runCommand(ctx, cmd, args, workDir, env, func(chunk string) {
		if chunk == "" {
			return
		}
		appendToTranscript(redact.String(chunk))
		mu.Lock()
		sawOutput = true
		mu.Unlock()
		if onProgress != nil {
			onProgress(redact.String(snapshot()))
		}
	})
	mu.Lock()
	noOutput := !sawOutput
	mu.Unlock()
	if noOutput {
		appendToTranscript("(no output)\n")
		if onProgress != nil {
			onProgress(redact.String(snapshot()))
		}
	}
	if err != nil {
		appendToTranscript("ERROR: " + redact.String(err.Error()) + "\n")
		if onProgress != nil {
			onProgress(redact.String(snapshot()))
		}
	}
	return redact.String(output), err
}

func (e *ComposeExecutor) runCommand(
	ctx context.Context,
	cmd string,
	args []string,
	workDir string,
	env map[string]string,
	onOutput func(string),
) (string, error) {
	isDocker := cmd == "docker"
	callerEnv := env

	if isDocker {
		resolution, err := e.resolveDockerCommand(ctx)
		if err != nil {
			return "", err
		}
		cmd = resolution.Path
		env = mergeCommandEnv(callerEnv, resolution.Env)
	}

	output, err := e.execProcess(ctx, cmd, args, workDir, env, onOutput)

	// If a docker/compose command failed because the compose plugin's
	// embedded API version is older than the daemon's minimum, upgrade the
	// plugin and retry the command once. This covers the edge-case where
	// probeComposeDaemonCompatibility passes (e.g. "compose ls" exits 0)
	// but heavier operations like "compose pull" are rejected.
	if err != nil && isDocker && outputLooksLikeDockerAPIMismatch(output) {
		if e.Logger != nil {
			e.Logger.Warn(
				"Docker compose API version mismatch detected, upgrading plugin and retrying",
				"original_cmd", formatCommand(cmd, args),
			)
		}
		if refreshErr := e.forceRefreshComposePlugin(ctx); refreshErr != nil {
			if e.Logger != nil {
				e.Logger.Error("Compose plugin upgrade failed", "error", refreshErr)
			}
			return output, dockerAPIIncompatibleError("", "")
		}
		resolution, resErr := e.resolveDockerCommand(ctx)
		if resErr != nil {
			if e.Logger != nil {
				e.Logger.Error("Docker re-resolution failed after compose upgrade", "error", resErr)
			}
			return output, dockerAPIIncompatibleError("", "")
		}
		if onOutput != nil {
			onOutput("\n--- compose plugin upgraded, retrying command ---\n")
		}
		retryOut, retryErr := e.execProcess(
			ctx,
			resolution.Path,
			args,
			workDir,
			mergeCommandEnv(callerEnv, resolution.Env),
			onOutput,
		)
		if retryErr != nil && outputLooksLikeDockerAPIMismatch(retryOut+retryErr.Error()) {
			return retryOut, dockerAPIIncompatibleError("", "")
		}
		return retryOut, retryErr
	}

	return output, err
}

// execProcess runs a single process, streams output through the callback,
// and returns the combined output.
func (e *ComposeExecutor) execProcess(
	ctx context.Context,
	cmd string,
	args []string,
	workDir string,
	env map[string]string,
	onOutput func(string),
) (string, error) {
	start := time.Now()
	command := exec.CommandContext(ctx, cmd, args...)
	command.Dir = workDir

	if len(env) > 0 {
		mergedEnv := append([]string{}, os.Environ()...)
		for key, value := range env {
			mergedEnv = append(mergedEnv, fmt.Sprintf("%s=%s", key, value))
		}
		command.Env = mergedEnv
	}

	e.logCommand(cmd, args, "Executing command",
		"cmd", redact.String(formatCommand(cmd, args)),
		"dir", workDir,
		"env_keys", sortedKeys(env),
	)

	pipeReader, pipeWriter := io.Pipe()
	command.Stdout = pipeWriter
	command.Stderr = pipeWriter

	var outputMu sync.Mutex
	var outputBuilder strings.Builder
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := pipeReader.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				outputMu.Lock()
				outputBuilder.WriteString(chunk)
				outputMu.Unlock()
				if onOutput != nil {
					onOutput(chunk)
				}
			}

			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					readDone <- nil
				} else {
					readDone <- readErr
				}
				return
			}
		}
	}()

	if err := command.Start(); err != nil {
		_ = pipeWriter.Close()
		_ = <-readDone
		_ = pipeReader.Close()
		return "", fmt.Errorf("command start failed: %w", err)
	}

	waitErr := command.Wait()
	_ = pipeWriter.Close()
	streamErr := <-readDone
	_ = pipeReader.Close()

	cmdDisplay := redact.String(formatCommand(cmd, args))
	// Health checks cancel docker ps on their own deadline. That is not a docker failure;
	// the rollback WARN already states the reason.
	stoppedByCaller := quietCommandOutput(cmd, args) && contextStopped(ctx)
	if streamErr != nil && e.Logger != nil && !stoppedByCaller {
		e.Logger.Warn("Command output stream failed", "cmd", cmdDisplay, "error", streamErr)
	}

	outputMu.Lock()
	output := outputBuilder.String()
	outputMu.Unlock()
	trimmed := strings.TrimSpace(output)
	if trimmed != "" && e.Logger != nil {
		msg := redact.String(truncateOutput(trimmed))
		if quietCommandOutput(cmd, args) {
			e.Logger.Debug("Command output", "cmd", cmdDisplay, "output", msg)
		} else {
			e.Logger.Info("Command output", "cmd", cmdDisplay, "output", msg)
		}
	}

	if waitErr != nil {
		if !stoppedByCaller && e.Logger != nil {
			exitCode := -1
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
			if contextStopped(ctx) {
				e.Logger.Error("Command context ended", "cmd", cmdDisplay, "context_err", ctx.Err())
			}
			e.Logger.Error(
				"Command failed",
				"cmd", cmdDisplay,
				"dir", workDir,
				"exit_code", exitCode,
				"elapsed_ms", time.Since(start).Milliseconds(),
			)
		}
		return redact.String(output), fmt.Errorf("command failed: %w", waitErr)
	}

	e.logCommand(cmd, args,
		"Command succeeded",
		"cmd", cmdDisplay,
		"elapsed_ms", time.Since(start).Milliseconds(),
	)

	return redact.String(output), nil
}

func (e *ComposeExecutor) listContainerIDsForCleanup(ctx context.Context, projectName, workingDir string) ([]string, error) {
	idSet := make(map[string]struct{})
	workDir := e.runtimeWorkDir()

	byProject, err := e.listContainerIDsByFilter(ctx, []string{fmt.Sprintf("label=com.docker.compose.project=%s", projectName)}, workDir)
	if err != nil {
		return nil, err
	}
	for _, id := range byProject {
		idSet[id] = struct{}{}
	}

	if strings.TrimSpace(workingDir) != "" {
		byWorkingDir, err := e.listContainerIDsByFilter(ctx, []string{fmt.Sprintf("label=com.docker.compose.project.working_dir=%s", workingDir)}, workDir)
		if err != nil {
			return nil, err
		}
		for _, id := range byWorkingDir {
			idSet[id] = struct{}{}
		}
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (e *ComposeExecutor) runtimeWorkDir() string {
	workDir := strings.TrimSpace(e.WorkDir)
	if workDir == "" {
		return "."
	}
	if _, err := os.Stat(workDir); err != nil {
		return "."
	}
	return workDir
}

func (e *ComposeExecutor) listContainerIDsByFilter(ctx context.Context, filters []string, workDir string) ([]string, error) {
	args := []string{"ps", "-aq"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}

	output, err := e.runCommand(ctx, "docker", args, workDir, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		id := strings.TrimSpace(line)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func composeProjectName(appID string) string {
	const fallback = "conops-app"
	raw := strings.ToLower(strings.TrimSpace(appID))
	if raw == "" {
		return fallback
	}

	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}

	project := strings.Trim(b.String(), "-_")
	if project == "" {
		project = fallback
	}
	if !startsWithAlphaNum(project) {
		project = "app-" + project
	}
	if len(project) > 63 {
		project = project[:63]
		project = strings.Trim(project, "-_")
	}
	if project == "" {
		return fallback
	}
	return project
}

// ServiceContainer represents runtime details for one container in a compose project.
type ServiceContainer struct {
	Service string
	Name    string
	Image   string
	Status  string // "running" or "exited"
	Health  string // "healthy", "unhealthy", "starting", or "" (no healthcheck)
	Ports   string
}

// InspectProjectContainers returns detailed container information for a compose project.
func (e *ComposeExecutor) InspectProjectContainers(ctx context.Context, projectName string) ([]ServiceContainer, error) {
	if strings.TrimSpace(projectName) == "" {
		return nil, nil
	}

	output, err := e.runCommand(
		ctx,
		"docker",
		[]string{
			"ps", "-a",
			"--filter", fmt.Sprintf("label=com.docker.compose.project=%s", projectName),
			"--filter", "label=com.docker.compose.oneoff=False",
			"--format", `{{.Label "com.docker.compose.service"}}|{{.Image}}|{{.Status}}|{{.Ports}}|{{.Names}}`,
		},
		e.runtimeWorkDir(),
		nil,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w", err)
	}

	var containers []ServiceContainer
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 5)
		if len(parts) != 5 {
			continue
		}

		rawStatus := strings.TrimSpace(parts[2])
		health := ""
		lowerStatus := strings.ToLower(rawStatus)
		if strings.Contains(lowerStatus, "(healthy)") {
			health = "healthy"
		} else if strings.Contains(lowerStatus, "(unhealthy)") {
			health = "unhealthy"
		} else if strings.Contains(lowerStatus, "(health: starting)") {
			health = "starting"
		}

		state := "exited"
		if dockerStatusIsRunning(rawStatus) {
			state = "running"
		}

		containers = append(containers, ServiceContainer{
			Service: strings.TrimSpace(parts[0]),
			Image:   strings.TrimSpace(parts[1]),
			Status:  state,
			Health:  health,
			Ports:   strings.TrimSpace(parts[3]),
			Name:    strings.TrimSpace(parts[4]),
		})
	}

	slices.SortFunc(containers, func(a, b ServiceContainer) int {
		return strings.Compare(a.Service, b.Service)
	})

	return containers, nil
}

// ProjectNameForApp returns the deterministic compose project name for an app ID.
func ProjectNameForApp(appID string) string {
	return composeProjectName(appID)
}

func (e *ComposeExecutor) oneshotFailure(ctx context.Context, project string) string {
	obs, err := e.observeContainers(ctx)
	if err != nil {
		return ""
	}
	reason := foldObservations(obs, time.Now())[project].Failure
	if strings.HasPrefix(reason, "oneshot_failed:") {
		return withPreviousRevision(obs, project, reason)
	}
	return ""
}

func (e *ComposeExecutor) observeContainers(ctx context.Context) ([]containerObs, error) {
	output, err := e.runCommand(
		ctx,
		"docker",
		[]string{
			"ps", "-a", "--no-trunc",
			"--format",
			`{{.ID}}|{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.service"}}|{{.Label "com.docker.compose.oneoff"}}|{{.Image}}|{{.Status}}|{{.Names}}`,
		},
		e.runtimeWorkDir(),
		nil,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w", err)
	}
	var obs []containerObs
	ids := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 7)
		if len(parts) < 6 {
			continue
		}
		c := containerObs{
			ID:       strings.TrimSpace(parts[0]),
			Project:  strings.TrimSpace(parts[1]),
			Service:  strings.TrimSpace(parts[2]),
			OneOff:   strings.EqualFold(strings.TrimSpace(parts[3]), "true") || strings.TrimSpace(parts[3]) == "1",
			Image:    strings.TrimSpace(parts[4]),
			PSStatus: strings.TrimSpace(parts[5]),
		}
		if len(parts) == 7 {
			c.Name = strings.TrimSpace(parts[6])
		}
		c.Running = dockerStatusIsRunning(c.PSStatus)
		if strings.HasPrefix(strings.ToLower(c.PSStatus), "exited (0)") {
			c.State = "exited"
			c.ExitCode = 0
		}
		if c.Project == "" {
			continue
		}
		obs = append(obs, c)
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	e.enrichObservations(ctx, ids, obs)
	return obs, nil
}

func (e *ComposeExecutor) enrichObservations(ctx context.Context, ids []string, obs []containerObs) {
	if len(ids) == 0 {
		return
	}
	args := append([]string{"inspect"}, ids...)
	output, err := e.runCommand(ctx, "docker", args, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		if e.Logger != nil {
			e.Logger.Warn("docker inspect failed; restart and oneshot details unavailable", "error", err)
		}
		return
	}
	if err := applyContainerInspectJSON(obs, []byte(output)); err != nil {
		if e.Logger != nil {
			e.Logger.Warn("docker inspect json rejected", "error", err)
		}
		return
	}
	refs := imageRefs(obs)
	if len(refs) == 0 {
		return
	}
	imgArgs := append([]string{"image", "inspect"}, refs...)
	imgOut, imgErr := e.runCommand(ctx, "docker", imgArgs, e.runtimeWorkDir(), nil, nil)
	if imgErr != nil {
		if e.Logger != nil {
			e.Logger.Warn("docker image inspect failed; digests unavailable", "error", imgErr)
		}
		return
	}
	if err := applyImageInspectJSON(obs, []byte(imgOut)); err != nil && e.Logger != nil {
		e.Logger.Warn("docker image inspect json rejected", "error", err)
	}
}

func imageRefs(obs []containerObs) []string {
	seen := map[string]struct{}{}
	var refs []string
	for _, c := range obs {
		ref := strings.TrimSpace(c.ImageID)
		if ref == "" {
			ref = strings.TrimSpace(c.Image)
		}
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func containerKey(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "sha256:")
	return id
}

func dockerStatusIsRunning(status string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(status)), "up ")
}

func quietCommandOutput(cmd string, args []string) bool {
	if filepath.Base(cmd) != "docker" || len(args) == 0 {
		return false
	}
	switch args[0] {
	case "ps", "inspect":
		return true
	case "image":
		return len(args) > 1 && args[1] == "inspect"
	default:
		return false
	}
}

func contextStopped(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled)
}

func (e *ComposeExecutor) logCommand(cmd string, args []string, msg string, keysAndValues ...any) {
	if e == nil || e.Logger == nil {
		return
	}
	if quietCommandOutput(cmd, args) {
		e.Logger.Debug(msg, keysAndValues...)
		return
	}
	e.Logger.Info(msg, keysAndValues...)
}

func startsWithAlphaNum(value string) bool {
	if value == "" {
		return false
	}
	for _, first := range value {
		return unicode.IsLetter(first) || unicode.IsDigit(first)
	}
	return false
}

func sortedKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func truncateOutput(value string) string {
	const maxLen = 2000
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "...(truncated)"
}

// beginPull waits for a pull slot, then starts the idle clock.
// Queue time does not count as pull silence.
func (e *ComposeExecutor) beginPull(ctx context.Context, idle time.Duration) (context.Context, func(), func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e != nil && e.PullGate != nil {
		if err := e.PullGate.Acquire(ctx); err != nil {
			return nil, func() {}, func() {}, err
		}
	}
	pullCtx, cancel, bump := idleContext(context.Background(), idle)
	release := func() {
		cancel()
		if e != nil && e.PullGate != nil {
			e.PullGate.Release()
		}
	}
	return pullCtx, release, bump, nil
}

// ReleasePorts drops published-port claims for a compose project that is gone.
func (e *ComposeExecutor) ReleasePorts(project string) {
	if e == nil {
		return
	}
	project = strings.TrimSpace(project)
	e.portsMu.Lock()
	defer e.portsMu.Unlock()
	for port, owner := range e.portOwner {
		if owner == project {
			delete(e.portOwner, port)
		}
	}
}

func idleContext(parent context.Context, idle time.Duration) (context.Context, context.CancelFunc, func()) {
	if idle <= 0 {
		if parent == nil {
			parent = context.Background()
		}
		return parent, func() {}, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(idle, cancel)
	var mu sync.Mutex
	return ctx, func() {
			timer.Stop()
			cancel()
		}, func() {
			mu.Lock()
			timer.Reset(idle)
			mu.Unlock()
		}
}

func (e *ComposeExecutor) claimPorts(app string, ports []int, foreign map[int]string) error {
	live := e.liveForeignPorts(app)
	e.portsMu.Lock()
	defer e.portsMu.Unlock()
	if e.portOwner == nil {
		e.portOwner = map[int]string{}
	}
	for _, port := range ports {
		if owner := foreign[port]; owner != "" && owner != app {
			return syncerr.New(syncerr.CodePortConflict, syncerr.StageValidate, fleetcheck.PortConflict(app, port, owner).Message)
		}
		if owner := live[port]; owner != "" && owner != app {
			return syncerr.New(syncerr.CodePortConflict, syncerr.StageValidate, fleetcheck.PortConflict(app, port, owner).Message)
		}
		if owner := e.portOwner[port]; owner != "" && owner != app {
			return syncerr.New(syncerr.CodePortConflict, syncerr.StageValidate, fleetcheck.PortConflict(app, port, owner).Message)
		}
	}
	for port, owner := range e.portOwner {
		if owner == app {
			delete(e.portOwner, port)
		}
	}
	for _, port := range ports {
		e.portOwner[port] = app
	}
	return nil
}

func (e *ComposeExecutor) liveForeignPorts(self string) map[int]string {
	out := map[int]string{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := e.runCommand(ctx, "docker", []string{"ps", "--format", `{{.Label "com.docker.compose.project"}}|{{.Names}}|{{.Ports}}`}, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(parts) < 3 {
			continue
		}
		owner := strings.TrimSpace(parts[0])
		if owner == "" {
			owner = strings.TrimSpace(parts[1])
		}
		if owner == "" || owner == self {
			continue
		}
		for _, port := range hostPorts(parts[2]) {
			out[port] = owner
		}
	}
	return out
}

func hostPorts(field string) []int {
	var ports []int
	for _, match := range regexp.MustCompile(`:(\d+)->`).FindAllStringSubmatch(field, -1) {
		if len(match) == 2 {
			n, err := strconv.Atoi(match[1])
			if err == nil && n > 0 {
				ports = append(ports, n)
			}
		}
	}
	return ports
}

func (e *ComposeExecutor) volumeExists(ctx context.Context, name string) bool {
	_, err := e.runCommand(ctx, "docker", []string{"volume", "inspect", name}, e.runtimeWorkDir(), nil, nil)
	return err == nil
}

func (e *ComposeExecutor) waitOneshots(ctx context.Context, project string, timeout time.Duration) error {
	obs, err := e.observeContainers(ctx)
	if err != nil {
		return err
	}
	if runningOneshot(filterProject(obs, project)) == "" && !oneshotPending(obs, project) {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		obs, err = e.observeContainers(ctx)
		if err != nil {
			return err
		}
		mine := filterProject(obs, project)
		state := foldObservations(mine, time.Now())[project]
		if strings.HasPrefix(state.Failure, "oneshot_failed:") {
			return syncerr.New(syncerr.CodeOneshotFailed, syncerr.StageCompose, withPreviousRevision(mine, project, state.Failure))
		}
		pending := runningOneshot(mine)
		if pending == "" {
			return nil
		}
		if time.Now().After(deadline) {
			e.stopServiceContainer(ctx, project, pending)
			return syncerr.New(syncerr.CodeOneshotTimeout, syncerr.StageCompose, "oneshot_timeout: "+pending)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if pending != "" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				e.stopServiceContainer(context.Background(), project, pending)
				return syncerr.New(syncerr.CodeOneshotTimeout, syncerr.StageCompose, "oneshot_timeout: "+pending)
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func filterProject(obs []containerObs, project string) []containerObs {
	var mine []containerObs
	for _, c := range obs {
		if c.Project == project {
			mine = append(mine, c)
		}
	}
	return mine
}

func oneshotPending(obs []containerObs, project string) bool {
	return runningOneshot(filterProject(obs, project)) != ""
}

func runningOneshot(obs []containerObs) string {
	oneshot := oneshotServices(obs)
	for _, c := range obs {
		if _, ok := oneshot[c.Service]; !ok || !c.Running {
			continue
		}
		return c.Service
	}
	return ""
}

func withPreviousRevision(obs []containerObs, project, reason string) string {
	if strings.Contains(reason, "previous revision still running") {
		return reason
	}
	oneshot := oneshotServices(obs)
	for _, c := range obs {
		if c.Project != "" && c.Project != project {
			continue
		}
		if _, ok := oneshot[c.Service]; ok || !c.Running {
			continue
		}
		return reason + "; previous revision still running"
	}
	return reason
}

func (e *ComposeExecutor) stopServiceContainer(ctx context.Context, project, service string) {
	obs, err := e.observeContainers(ctx)
	if err != nil {
		return
	}
	for _, c := range obs {
		if c.Project == project && c.Service == service && c.Name != "" {
			_, _ = e.runCommand(ctx, "docker", []string{"stop", c.Name}, e.runtimeWorkDir(), nil, nil)
		}
	}
}

func (e *ComposeExecutor) pinnedDigestsLocal(ctx context.Context, images []string) bool {
	if len(images) == 0 {
		return false
	}
	for _, image := range images {
		if !strings.Contains(image, "@sha256:") {
			return false
		}
		if _, err := e.runCommand(ctx, "docker", []string{"image", "inspect", image}, e.runtimeWorkDir(), nil, nil); err != nil {
			return false
		}
	}
	return true
}

func (e *ComposeExecutor) rememberDigests(project string, images map[string]string) {
	if e == nil {
		return
	}
	kept := map[string]string{}
	for svc, ref := range images {
		if digest := fleetcheck.ImageDigest(ref); digest != "" {
			kept[svc] = digest
		}
	}
	e.digestMu.Lock()
	defer e.digestMu.Unlock()
	if e.expected == nil {
		e.expected = map[string]map[string]string{}
	}
	e.expected[project] = kept
}

// ExpectedDigests returns pinned digests from the last compose config for a project.
func (e *ComposeExecutor) ExpectedDigests(project string) map[string]string {
	if e == nil {
		return nil
	}
	e.digestMu.Lock()
	defer e.digestMu.Unlock()
	src := e.expected[project]
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// ProjectVolumes lists named volumes labeled for a compose project.
func (e *ComposeExecutor) ProjectVolumes(ctx context.Context, project string) ([]VolumeInfo, error) {
	raw, err := e.runCommand(ctx, "docker", []string{"volume", "ls", "--filter", "label=com.docker.compose.project=" + project, "--format", "{{.Name}}"}, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		return nil, err
	}
	var out []VolumeInfo
	for _, line := range strings.Split(raw, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		out = append(out, VolumeInfo{Name: name})
	}
	return out, nil
}

// VolumeInfo is one named volume belonging to a compose project.
type VolumeInfo struct {
	Name       string
	Containers string
}

func policyMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CONOPS_POLICY_MODE"))) {
	case "warn":
		return "warn"
	case "enforce":
		return "enforce"
	default:
		return "off"
	}
}
