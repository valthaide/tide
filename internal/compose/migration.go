package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/valthaide/tide/internal/fleetcheck"
	"github.com/valthaide/tide/internal/fsperm"
	"github.com/valthaide/tide/internal/syncerr"
)

// MigrationPrepParams describes the target repository and options for preflight validation.
type MigrationPrepParams struct {
	AppID          string
	AppName        string
	OldComposePath string
	NewRepoURL     string
	NewAuthMethod  string
	NewRepoSecret  []byte
	NewBranch      string
	TargetCommit   string
	NewComposePath string
	EnvVars        map[string]string
	Opts           ApplyOptions
	DryRun         bool
}

// MigrationPrepared holds the verified staging artifacts, equivalence diff, and plan summary before cutover.
type MigrationPrepared struct {
	StagingDir        string
	ProjectName       string
	Equivalent        bool
	Mode              string
	Strategy          string
	Reason            string
	ChangedServices   []string
	UnchangedServices []string
	DiffSummary       []string
	ToStop            []string
	ToRemove          []string
	KeptVolumes       []string
	ToCreate          []string
	ImagesVerified    []string
	Prep              imagePrep
}

// Release cleans up temporary staging files when dry-running or aborting before cutover.
func (m *MigrationPrepared) Release() {
	if m != nil && m.StagingDir != "" {
		_ = os.RemoveAll(m.StagingDir)
		m.StagingDir = ""
	}
}

// PrepareRepoMigration validates the current app directory for persistent data binds,
// clones and validates the new repository in an isolated staging directory, verifies
// (or pulls) every required container image, and compares the rendered Compose spec
// against the currently running containers BEFORE touching the live application.
func (e *ComposeExecutor) PrepareRepoMigration(ctx context.Context, p MigrationPrepParams) (MigrationPrepared, error) {
	var out MigrationPrepared

	projectName := strings.TrimSpace(p.Opts.Project)
	if projectName == "" {
		projectName = e.projectFor(p.AppID)
	}
	out.ProjectName = projectName

	appDirAbs, err := filepath.Abs(filepath.Join(e.WorkDir, p.AppID))
	if err != nil {
		return out, fmt.Errorf("resolve app dir: %w", err)
	}

	// 1. Verify no persistent data or writable bind mounts point to data inside appDirAbs.
	if err := e.verifyNoDataInAppDir(ctx, appDirAbs, projectName, p.OldComposePath); err != nil {
		return out, err
	}

	// 2. Stage and verify the new repository in a temporary directory without touching the live app.
	stagingDir, err := os.MkdirTemp("", "tide-repo-stage-")
	if err != nil {
		return out, fmt.Errorf("create staging dir: %w", err)
	}
	out.StagingDir = stagingDir
	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			out.Release()
		}
	}()

	if err := fsperm.EnsureSharedDir(stagingDir); err != nil {
		return out, err
	}
	stagingRepoDir := filepath.Join(stagingDir, "repo")
	if _, err := e.prepareRepo(ctx, stagingDir, stagingRepoDir, p.NewRepoURL, p.NewBranch, p.TargetCommit, p.NewAuthMethod, p.NewRepoSecret); err != nil {
		return out, syncerr.Wrap(syncerr.CodeGitFetch, syncerr.StageFetch, fmt.Errorf("new repository unreadable: %w", err))
	}
	_ = fsperm.RelaxCheckoutTree(stagingRepoDir)

	envVars := p.EnvVars
	if e.Secrets != nil {
		resolved, resErr := e.Secrets.ResolveEnvs(ctx, envVars)
		if resErr != nil {
			return out, syncerr.Wrap(syncerr.CodeSecretsVault, syncerr.StageSecrets, resErr)
		}
		envVars = resolved
		if prepErr := e.Secrets.PrepareRepo(ctx, stagingRepoDir); prepErr != nil {
			return out, syncerr.Wrap(syncerr.CodeSecretsSOPS, syncerr.StageSecrets, prepErr)
		}
	}

	composeRel := strings.TrimSpace(p.NewComposePath)
	if composeRel == "" {
		composeRel = "docker-compose.yml"
	}
	composeFullPath := filepath.Join(stagingRepoDir, composeRel)
	composeDir := filepath.Dir(composeFullPath)
	composeFileName := filepath.Base(composeFullPath)
	if info, statErr := os.Stat(composeFullPath); statErr != nil || info.IsDir() {
		return out, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, fmt.Sprintf("compose file not found in new repository: %s", composeRel))
	}

	// Verify any external host env_files referenced by compose exist before proceeding.
	if _, extErr := ExternalEnvHash(stagingRepoDir, composeRel); extErr != nil {
		return out, extErr
	}

	var overrideArgs []string
	if len(envVars) > 0 {
		var cleanupOverride func()
		var envErr error
		overrideArgs, cleanupOverride, envErr = e.prepareEnvOverrides(composeDir, envVars)
		if envErr != nil {
			return out, fmt.Errorf("prepare env override: %w", envErr)
		}
		defer cleanupOverride()
	}

	var syncLog strings.Builder
	opts := p.Opts
	opts.Project = projectName
	prep, prepErr := e.prepareImageSync(ctx, &syncLog, projectName, composeFileName, composeDir, composeFullPath, overrideArgs, opts, func(string) {})
	if prepErr != nil {
		prep.clean()
		return out, prepErr
	}
	defer prep.clean()

	// Ensure the new compose configuration does not bind-mount persistent data inside the managed app directory.
	if err := checkComposeBindsAgainstAppDir(composeFullPath, stagingRepoDir, appDirAbs, opts.Vars); err != nil {
		return out, err
	}

	// 3. Verify that all images (including digest pins) are reachable BEFORE touching the running stack.
	if len(prep.images) > 0 && !e.pinnedDigestsLocal(ctx, prep.images) {
		if err := e.verifyOrPullImages(ctx, composeDir, projectName, composeFileName, overrideArgs, prep, opts, p.DryRun); err != nil {
			return out, err
		}
	}
	out.Prep = prep
	out.ImagesVerified = append([]string(nil), prep.images...)

	// 4. Compare rendered Compose configuration against the currently running containers.
	targetAppRepoDir := filepath.Join(appDirAbs, "repo")
	desiredImages := e.inspectDesiredImages(ctx, prep.images, prep.env)
	desiredSpec, specErr := ParseRenderedComposeSpec(prep.configJSON, projectName, stagingRepoDir, targetAppRepoDir, func(imageRef string) string {
		if img, ok := desiredImages[imageRef]; ok {
			for _, rd := range img.RepoDigests {
				if d := extractDigestHex(rd); d != "" {
					return d
				}
			}
			return img.ID
		}
		return ""
	})
	var diff MigrationDiffResult
	if specErr != nil {
		diff = MigrationDiffResult{
			Equivalent:  false,
			Mode:        MigrationModeRestarted,
			Strategy:    MigrationStrategyComposeUp,
			Reason:      "config_parse_error",
			DiffSummary: []string{specErr.Error()},
		}
	} else {
		runningSpec, runningHealthy, runErr := e.InspectRunningProjectSpec(ctx, projectName, targetAppRepoDir)
		if runErr != nil {
			diff = MigrationDiffResult{
				Equivalent:  false,
				Mode:        MigrationModeRestarted,
				Strategy:    MigrationStrategyComposeUp,
				Reason:      "runtime_inspect_error",
				DiffSummary: []string{runErr.Error()},
			}
		} else {
			diff = CompareMigrationSpecs(desiredSpec, runningSpec, runningHealthy, desiredImages)
		}
	}

	out.Equivalent = diff.Equivalent
	out.Mode = diff.Mode
	out.Strategy = diff.Strategy
	out.Reason = diff.Reason
	out.ChangedServices = append([]string(nil), diff.ChangedServices...)
	out.UnchangedServices = append([]string(nil), diff.UnchangedServices...)
	out.DiffSummary = append([]string(nil), diff.DiffSummary...)

	// 5. Populate migration plan details (what will be stopped/recreated, kept, removed, and created).
	out.KeptVolumes = e.projectVolumeNames(ctx, projectName)
	sort.Strings(out.KeptVolumes)

	if diff.Equivalent {
		// Zero-downtime source adoption: no containers are stopped, removed, or recreated.
		out.ToStop = nil
		out.ToRemove = nil
		out.ToCreate = nil
	} else {
		affected := map[string]bool{}
		for _, s := range diff.ChangedServices {
			affected[s] = true
		}
		for _, s := range diff.RemovedServices {
			affected[s] = true
		}
		if containers, cErr := e.InspectProjectContainers(ctx, projectName); cErr == nil {
			for _, c := range containers {
				if len(affected) > 0 && !affected[c.Service] {
					continue
				}
				label := c.Service
				if c.Name != "" {
					label = fmt.Sprintf("%s (%s)", c.Service, c.Name)
				}
				out.ToStop = append(out.ToStop, label)
			}
		}
		for _, s := range diff.RemovedServices {
			out.ToRemove = append(out.ToRemove, "service:"+s)
		}
		if len(diff.ChangedServices) > 0 {
			for _, s := range diff.ChangedServices {
				if desSvc, ok := desiredSpec.Services[s]; ok && desSvc.Image != "" {
					imgRef := desSvc.Image
					if desSvc.Digest != "" {
						imgRef = imgRef + "@sha256:" + desSvc.Digest
					}
					out.ToCreate = append(out.ToCreate, fmt.Sprintf("%s (%s)", s, imgRef))
				} else {
					out.ToCreate = append(out.ToCreate, s)
				}
			}
		} else {
			out.ToCreate = append([]string(nil), prep.images...)
		}
		sort.Strings(out.ToCreate)
	}

	if p.DryRun {
		out.Release()
	} else {
		cleanupOnErr = false
	}
	return out, nil
}

// CommitRepoMigration activates the staged repository checkout in-place (preserving file inodes
// for any read-only tracked bind mounts and keeping all volumes and running containers intact).
// It does NOT run `docker compose down`, allowing either zero-restart source adoption when
// Equivalent==true or targeted service recreation via standard `docker compose up -d --remove-orphans`
// when Equivalent==false.
func (e *ComposeExecutor) CommitRepoMigration(ctx context.Context, appID, oldComposePath string, envVars map[string]string, prepared *MigrationPrepared) error {
	if prepared == nil || prepared.StagingDir == "" {
		return fmt.Errorf("migration staging state is missing")
	}
	defer prepared.Release()
	_ = envVars

	projectName := prepared.ProjectName
	if projectName == "" {
		projectName = e.projectFor(appID)
	}
	appDirAbs, err := filepath.Abs(filepath.Join(e.WorkDir, appID))
	if err != nil {
		return err
	}

	// Re-verify no persistent data lives inside appDirAbs before updating checkout files.
	if err := e.verifyNoDataInAppDir(ctx, appDirAbs, projectName, oldComposePath); err != nil {
		return err
	}

	if err := fsperm.EnsureSharedDir(appDirAbs); err != nil {
		return fmt.Errorf("prepare app dir: %w", err)
	}

	targetRepoDir := filepath.Join(appDirAbs, "repo")
	stagedRepoDir := filepath.Join(prepared.StagingDir, "repo")
	if err := swapStagedCheckoutInPlace(ctx, e, stagedRepoDir, targetRepoDir, appDirAbs); err != nil {
		return err
	}
	_ = fsperm.RelaxCheckoutTree(targetRepoDir)
	return nil
}

// swapStagedCheckoutInPlace updates targetRepoDir to match stagedRepoDir while preserving
// existing regular file inodes (safe for live containers with single-file read-only bind mounts)
// and replacing `.git` with the staged repository's `.git` metadata.
func swapStagedCheckoutInPlace(ctx context.Context, e *ComposeExecutor, stagedRepoDir, targetRepoDir, appDirAbs string) error {
	if st, err := os.Stat(targetRepoDir); os.IsNotExist(err) || (err == nil && !st.IsDir()) {
		_ = os.RemoveAll(targetRepoDir)
		if renErr := os.Rename(stagedRepoDir, targetRepoDir); renErr != nil {
			if _, cloneErr := e.runCommand(ctx, "git", []string{"clone", stagedRepoDir, targetRepoDir}, appDirAbs, nil, nil); cloneErr != nil {
				return fmt.Errorf("activate staged repository: %w (rename err: %v)", cloneErr, renErr)
			}
		}
		return nil
	}

	// Copy/truncate tracked working-tree files in-place so existing file inodes stay intact.
	want := map[string]struct{}{}
	walkErr := filepath.WalkDir(stagedRepoDir, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d == nil {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(stagedRepoDir, path)
		if relErr != nil {
			return relErr
		}
		want[rel] = struct{}{}
		dest := filepath.Join(targetRepoDir, rel)
		if d.Type()&os.ModeSymlink != 0 {
			linkTarget, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			_ = os.Remove(dest)
			if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
				return mkErr
			}
			return os.Symlink(linkTarget, dest)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		mode := os.FileMode(0o644)
		if info, statErr := d.Info(); statErr == nil {
			mode = info.Mode().Perm()
		}
		return writeFileInPlacePortable(dest, content, mode)
	})
	if walkErr != nil {
		return fmt.Errorf("sync staged worktree files: %w", walkErr)
	}

	// Remove files from targetRepoDir that no longer exist in stagedRepoDir.
	_ = filepath.WalkDir(targetRepoDir, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d == nil || path == targetRepoDir {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(targetRepoDir, path)
		if relErr != nil {
			return nil
		}
		if _, ok := want[rel]; !ok {
			_ = os.Remove(path)
		}
		return nil
	})

	// Replace .git directory with the staged repository's .git directory.
	oldGit := filepath.Join(targetRepoDir, ".git")
	stagedGit := filepath.Join(stagedRepoDir, ".git")
	backupGit := filepath.Join(appDirAbs, ".git-prev-mig")
	_ = os.RemoveAll(backupGit)
	hadOldGit := false
	if _, statErr := os.Stat(oldGit); statErr == nil {
		if renErr := os.Rename(oldGit, backupGit); renErr == nil {
			hadOldGit = true
		} else {
			_ = os.RemoveAll(oldGit)
		}
	}
	if err := moveOrCopyDir(stagedGit, oldGit); err != nil {
		if hadOldGit {
			_ = os.RemoveAll(oldGit)
			_ = os.Rename(backupGit, oldGit)
		}
		return fmt.Errorf("swap repository .git metadata: %w", err)
	}
	_ = os.RemoveAll(backupGit)
	_ = fsperm.HardenPrivateSubtree(oldGit)
	return nil
}

func writeFileInPlacePortable(dest string, content []byte, mode os.FileMode) error {
	if info, err := os.Lstat(dest); err == nil && info.Mode().IsRegular() {
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		_, werr := f.Write(content)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
		return os.Chmod(dest, mode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, content, mode)
}

func moveOrCopyDir(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil {
			return wErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, lErr := os.Readlink(path)
			if lErr != nil {
				return lErr
			}
			return os.Symlink(link, target)
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		mode := os.FileMode(0o600)
		if info, iErr := d.Info(); iErr == nil {
			mode = info.Mode().Perm()
		}
		return os.WriteFile(target, data, mode)
	})
}

func (e *ComposeExecutor) verifyOrPullImages(
	ctx context.Context,
	composeDir, projectName, composeFileName string,
	overrideArgs []string,
	prep imagePrep,
	opts ApplyOptions,
	dryRun bool,
) error {
	if dryRun {
		for _, img := range prep.images {
			if _, err := e.runCommand(ctx, "docker", []string{"image", "inspect", img}, composeDir, prep.env, nil); err == nil {
				continue
			}
			out, err := e.runCommand(ctx, "docker", []string{"manifest", "inspect", img}, composeDir, prep.env, nil)
			if err != nil {
				return classifyPullError(err, out, false, opts.PullIdle, []string{img})
			}
		}
		return nil
	}

	pullCtx, pullRelease, bumpPull, waitErr := e.beginPull(ctx, opts.PullIdle)
	if waitErr != nil {
		return fmt.Errorf("waiting for pull slot: %w", waitErr)
	}
	defer pullRelease()

	pullArgs := []string{"compose", "-p", projectName, "-f", composeFileName}
	if len(overrideArgs) > 0 {
		pullArgs = append(pullArgs, overrideArgs...)
	}
	pullArgs = append(pullArgs, "pull")
	var transcript strings.Builder
	pullOut, err := e.runCommandWithTranscript(pullCtx, &transcript, "docker", pullArgs, composeDir, prep.env, func(string) {
		bumpPull()
	})
	idleFired := pullCtx.Err() != nil
	if err != nil {
		return classifyPullError(err, pullOut, idleFired, opts.PullIdle, prep.images)
	}
	return nil
}

func (e *ComposeExecutor) verifyNoDataInAppDir(ctx context.Context, appDirAbs, projectName, composePath string) error {
	info, err := os.Stat(appDirAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}

	// Check for unexpected entries directly inside appDirAbs (only "repo", ".ssh", and ".envs" are controller-managed).
	entries, err := os.ReadDir(appDirAbs)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "repo" || name == ".ssh" || name == ".envs" || strings.HasPrefix(name, ".env.override") {
			continue
		}
		return syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
			fmt.Sprintf("app_dir_contains_data: unexpected path %s inside %s", name, appDirAbs))
	}

	// Check running container bind mounts targeting paths inside appDirAbs.
	if mounts, mErr := e.inspectProjectBindMounts(ctx, projectName); mErr == nil {
		repoDir := filepath.Join(appDirAbs, "repo")
		for _, m := range mounts {
			if !pathWithin(m.Source, appDirAbs) {
				continue
			}
			// Writable bind mount inside appDirAbs or bind mount outside tracked git files is treated as persistent data.
			if m.RW || !pathWithin(m.Source, repoDir) || !isGitTrackedPath(ctx, e, repoDir, m.Source) {
				return syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
					fmt.Sprintf("data_bind_in_app_dir: bind mount %s -> %s inside app directory", m.Source, m.Destination))
			}
		}
	}

	// Check untracked files inside existing repo checkout (e.g. SQLite DB or uploads dropped into checkout).
	repoDir := filepath.Join(appDirAbs, "repo")
	if st, statErr := os.Stat(filepath.Join(repoDir, ".git")); statErr == nil && st.IsDir() {
		out, statusErr := e.runCommand(ctx, "git", []string{"status", "--porcelain", "--untracked-files=all"}, repoDir, nil, nil)
		if statusErr == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "?? ") {
					untracked := strings.TrimSpace(strings.TrimPrefix(line, "?? "))
					if untracked == ".envs" || strings.HasPrefix(untracked, ".envs/") {
						continue
					}
					return syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
						fmt.Sprintf("app_dir_contains_data: untracked data %s inside checkout", untracked))
				}
			}
		}
	}

	_ = composePath
	_ = slices.Sort[[]string]
	return nil
}

type containerMount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Type        string `json:"Type"`
	RW          bool   `json:"RW"`
}

func (e *ComposeExecutor) inspectProjectBindMounts(ctx context.Context, projectName string) ([]containerMount, error) {
	if strings.TrimSpace(projectName) == "" {
		return nil, nil
	}
	ids, err := e.listContainerIDsByFilter(ctx, []string{"label=com.docker.compose.project=" + projectName}, e.runtimeWorkDir())
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	args := append([]string{"inspect"}, ids...)
	raw, err := e.runCommand(ctx, "docker", args, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		return nil, err
	}
	var docs []struct {
		Mounts []containerMount `json:"Mounts"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &docs); err != nil {
		return nil, err
	}
	var out []containerMount
	for _, d := range docs {
		for _, m := range d.Mounts {
			if strings.EqualFold(m.Type, "bind") && strings.TrimSpace(m.Source) != "" {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func checkComposeBindsAgainstAppDir(composeFullPath, stagingRepoDir, targetAppDir string, vars map[string]string) error {
	data, err := os.ReadFile(composeFullPath)
	if err != nil {
		return nil
	}
	_ = vars
	cleanTarget := filepath.Clean(targetAppDir)
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, cleanTarget) {
			return syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
				fmt.Sprintf("data_bind_in_app_dir: compose references app runtime directory %s", cleanTarget))
		}
	}
	_ = stagingRepoDir
	_ = fleetcheck.Summary{}
	return nil
}

func isGitTrackedPath(ctx context.Context, e *ComposeExecutor, repoDir, target string) bool {
	rel, err := filepath.Rel(repoDir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	_, err = e.runCommand(ctx, "git", []string{"ls-files", "--error-unmatch", rel}, repoDir, nil, nil)
	return err == nil
}

func pathWithin(candidate, root string) bool {
	cAbs, err1 := filepath.Abs(candidate)
	rAbs, err2 := filepath.Abs(root)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(rAbs, cAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
