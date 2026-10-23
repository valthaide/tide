package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valthaide/conops-private/internal/cigate"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/deploypolicy"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// gateWait is returned when CI is still running; sync should stay pending and retry.
type gateWait struct {
	msg    string
	reason string
	status string
}

func (e *gateWait) Error() string { return e.msg }

// ReconcilerConfig controls how the monolith applies desired state.
type ReconcilerConfig struct {
	Interval      time.Duration
	SyncTimeout   time.Duration
	RetryErrors   bool
	MaxConcurrent int
}

// LoadReconcilerConfigFromEnv loads reconciler config from environment variables.
func LoadReconcilerConfigFromEnv() (ReconcilerConfig, error) {
	interval := 10 * time.Second
	if value := strings.TrimSpace(os.Getenv("CONOPS_RECONCILE_INTERVAL")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_RECONCILE_INTERVAL: %s", value)
		}
		if parsed > 0 {
			interval = parsed
		}
	}

	timeout := 5 * time.Minute
	if value := strings.TrimSpace(os.Getenv("CONOPS_SYNC_TIMEOUT")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_SYNC_TIMEOUT: %s", value)
		}
		if parsed > 0 {
			timeout = parsed
		}
	}

	retryErrors := strings.EqualFold(os.Getenv("CONOPS_RETRY_ERRORS"), "true")

	maxConcurrent := 2
	if value := strings.TrimSpace(os.Getenv("CONOPS_MAX_CONCURRENT_SYNCS")); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_MAX_CONCURRENT_SYNCS: %s", value)
		}
		maxConcurrent = n
	}

	return ReconcilerConfig{
		Interval:      interval,
		SyncTimeout:   timeout,
		RetryErrors:   retryErrors,
		MaxConcurrent: maxConcurrent,
	}, nil
}

// AppRuntime applies compose state and inspects running projects.
// *compose.ComposeExecutor implements this; tests use a fake.
type AppRuntime interface {
	Apply(
		ctx context.Context,
		appID, content string,
		envVars map[string]string,
		repoURL, branch, composePath, commitHash string,
		repoAuthMethod string,
		repoSecret []byte,
		onProgress func(string),
	) (string, error)
	SnapshotProjects(ctx context.Context) (map[string]compose.ProjectRuntimeState, error)
}

// EnvFileHasher optionally hashes absolute compose env_file paths outside the repo.
type EnvFileHasher interface {
	ExternalEnvHashForApp(appID, composePath string) (string, error)
}

// WatchFileManager hashes watch_files and applies restart/reload actions.
type WatchFileManager interface {
	WatchFilesHashForApp(appID string, files []deploypolicy.WatchFile) (string, error)
	RestartService(ctx context.Context, appID, composePath, service string) (string, error)
	HTTPReload(ctx context.Context, reloadURL string) error
}

// Reconciler applies desired state directly on the host (monolith mode).
type Reconciler struct {
	Registry *Registry
	Executor AppRuntime
	Logger   *slog.Logger
	Config   ReconcilerConfig

	mu               sync.Mutex
	running          bool
	sem              chan struct{}
	watchHashErrSeen map[string]string // appID -> last error text (rate-limit logs)
}

// NewReconciler creates a new reconciler.
func NewReconciler(registry *Registry, executor AppRuntime, logger *slog.Logger, cfg ReconcilerConfig) *Reconciler {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 2
	}
	return &Reconciler{
		Registry:         registry,
		Executor:         executor,
		Logger:           logger,
		Config:           cfg,
		sem:              make(chan struct{}, cfg.MaxConcurrent),
		watchHashErrSeen: make(map[string]string),
	}
}

// AcquireSync slot for force-sync and reconcile (shared concurrency limit).
func (r *Reconciler) AcquireSync(ctx context.Context) error {
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseSync releases a previously acquired sync slot.
func (r *Reconciler) ReleaseSync() {
	select {
	case <-r.sem:
	default:
	}
}

// Run starts the reconciliation loop.
func (r *Reconciler) Run(ctx context.Context) {
	r.reconcileOnce()

	ticker := time.NewTicker(r.Config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if r.Logger != nil {
				r.Logger.Info("Reconciler stopped")
			}
			return
		case <-ticker.C:
			r.reconcileOnce()
		}
	}
}

func (r *Reconciler) reconcileOnce() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	metrics.ReconcileLoopsTotal.Inc()

	var runtimeSnapshot map[string]compose.ProjectRuntimeState
	if snapshot, err := r.captureRuntimeSnapshot(); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("Failed to capture runtime snapshot; skipping runtime drift checks", "error", err)
		}
	} else {
		runtimeSnapshot = snapshot
	}

	apps := r.Registry.List()
	var wg sync.WaitGroup
	for _, app := range apps {
		if app.Suspended {
			continue
		}
		if app.Status == "syncing" {
			if r.syncLooksStale(app) {
				r.requeuePending(app, "recovering_interrupted_sync")
			} else {
				continue
			}
		}

		if app.LastSeenCommit == "" {
			continue
		}

		if app.Status == "synced" && runtimeSnapshot != nil {
			if reason := r.runtimeDriftReason(app.ID, runtimeSnapshot); reason != "" {
				r.requeuePending(app, reason)
			}
		}
		if app.Status == "synced" {
			if reason := r.envFileDriftReason(app); reason != "" {
				r.requeuePending(app, reason)
			}
		}
		if app.Status == "synced" {
			r.handleWatchFileDrift(app)
		}

		switch app.Status {
		case "pending":
		case "error":
			// Always re-check CI-blocked apps until the seen commit is deployed
			// (pipeline may flip pending→success or failed→success after retry).
			if app.LastSyncErrorCode == syncerr.CodeBlockedByCI &&
				app.LastSeenCommit != "" && app.LastSeenCommit != app.LastSyncedCommit {
				// fall through
			} else if !r.Config.RetryErrors {
				continue
			}
		default:
			continue
		}

		appCopy := app
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), r.Config.SyncTimeout+time.Minute)
			defer cancel()
			if err := r.AcquireSync(ctx); err != nil {
				if r.Logger != nil {
					r.Logger.Warn("Sync slot wait failed", "app_id", appCopy.ID, "error", err)
				}
				return
			}
			defer r.ReleaseSync()
			if err := r.syncApp(appCopy); err != nil && r.Logger != nil {
				r.Logger.Error("App sync failed", "app_id", appCopy.ID, "error", err)
			}
		}()
	}
	wg.Wait()
}

func (r *Reconciler) captureRuntimeSnapshot() (map[string]compose.ProjectRuntimeState, error) {
	if r.Executor == nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.Executor.SnapshotProjects(ctx)
}

func (r *Reconciler) runtimeDriftReason(appID string, snapshot map[string]compose.ProjectRuntimeState) string {
	projectName := compose.ProjectNameForApp(appID)
	state, ok := snapshot[projectName]
	if !ok || state.ContainerCount == 0 {
		return "runtime_missing"
	}
	if state.UnhealthyCount > 0 {
		return "runtime_unhealthy"
	}
	if state.ExitedCount > 0 {
		return "runtime_exited"
	}
	if state.RunningCount < state.ContainerCount {
		return "runtime_not_running"
	}
	return ""
}

func (r *Reconciler) envFileDriftReason(app *App) string {
	hasher, ok := r.Executor.(EnvFileHasher)
	if !ok || app == nil {
		return ""
	}
	hash, err := hasher.ExternalEnvHashForApp(app.ID, app.ComposePath)
	if err != nil {
		se := syncerr.Classify(err)
		if se.Code == syncerr.CodeEnvFileMissing {
			_ = r.Registry.UpdateSyncResultErr(app.ID, "error", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
			metrics.SetEnvDrift(app.Name, true)
			if r.Logger != nil {
				r.Logger.Error("env_file missing", "app_id", app.ID, "code", se.Code)
			}
		}
		return ""
	}
	if hash == "" {
		metrics.SetEnvDrift(app.Name, false)
		return ""
	}
	if app.LastAppliedEnvHash == "" {
		// First observation after upgrade: record without forcing a redeploy.
		_ = r.Registry.UpdateEnvHash(app.ID, hash)
		metrics.SetEnvDrift(app.Name, false)
		metrics.SetEnvHashApplied(app.ID, app.Name, time.Now())
		return ""
	}
	if hash != app.LastAppliedEnvHash {
		metrics.SetEnvDrift(app.Name, true)
		if r.Logger != nil {
			r.Logger.Info("env_file changed", "app_id", app.ID)
		}
		return "env_file_changed"
	}
	metrics.SetEnvDrift(app.Name, false)
	return ""
}

func (r *Reconciler) requeuePending(app *App, reason string) {
	if app.Status == "pending" {
		return
	}
	if err := r.Registry.UpdateStatus(app.ID, "pending", nil); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("Failed to requeue app for reconciliation", "app_id", app.ID, "reason", reason, "error", err)
		}
		return
	}
	app.Status = "pending"
	metrics.ObserveRequeue(app.ID, app.Name, reason)
	if r.Logger != nil {
		r.Logger.Info("Requeued app for reconciliation", "app_id", app.ID, "reason", reason)
	}
}

func (r *Reconciler) syncLooksStale(app *App) bool {
	if app == nil {
		return true
	}
	if app.LastSyncAt.IsZero() {
		return true
	}

	staleAfter := r.Config.SyncTimeout + 30*time.Second
	if staleAfter <= 0 {
		staleAfter = 5*time.Minute + 30*time.Second
	}
	return time.Since(app.LastSyncAt) > staleAfter
}

func (r *Reconciler) syncApp(app *App) error {
	syncStartedAt := time.Now()
	if err := r.Registry.UpdateStatus(app.ID, "syncing", &syncStartedAt); err != nil && r.Logger != nil {
		r.Logger.Warn("Failed to mark app syncing", "app_id", app.ID, "error", err)
	}
	target := revisionToApply(app)

	if target == strings.TrimSpace(app.LastSeenCommit) {
		if err := r.checkPipelineGate(app); err != nil {
			var wait *gateWait
			if asWait, ok := err.(*gateWait); ok {
				wait = asWait
			}
			if wait != nil {
				now := time.Now()
				_ = r.Registry.UpdateStatus(app.ID, "pending", &now)
				_ = r.Registry.UpdateSyncProgress(app.ID, now, fmt.Sprintf("waiting for CI: status=%s reason=%s commit=%s", wait.status, wait.reason, app.LastSeenCommit))
				metrics.ObserveDeployBlocked(app.Name, wait.reason)
				if r.Logger != nil {
					r.Logger.Info("Deploy waiting for CI", "app", app.Name, "commit", app.LastSeenCommit, "pipeline_status", wait.status, "reason", wait.reason)
				}
				return nil
			}
			se := syncerr.Classify(err)
			_ = r.Registry.UpdateSyncResultErr(app.ID, "error", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
			metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
			reason := se.Code
			if strings.Contains(se.Message, cigate.ReasonCIFailed) {
				reason = cigate.ReasonCIFailed
			} else if strings.Contains(se.Message, cigate.ReasonCIError) {
				reason = cigate.ReasonCIError
			}
			metrics.ObserveDeployBlocked(app.Name, reason)
			return se
		}
	}

	repoSecret, err := r.Registry.GetRepoSecret(app.ID)
	if err != nil {
		se := syncerr.Wrap(syncerr.CodeCredentials, syncerr.StageCredentials, err)
		_ = r.Registry.UpdateSyncResultErr(app.ID, "error", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
		metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
		return se
	}
	defer zeroBytes(repoSecret)

	envVars, err := r.Registry.GetAppEnvs(app.ID)
	if err != nil {
		se := syncerr.Wrap(syncerr.CodeCredentials, syncerr.StageCredentials, err)
		_ = r.Registry.UpdateSyncResultErr(app.ID, "error", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
		metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
		return se
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.Config.SyncTimeout)
	defer cancel()

	progress := newSyncProgressReporter(r.Registry, r.Logger, app.ID, syncProgressFlushInterval)
	output, err := r.Executor.Apply(
		ctx,
		app.ID,
		"",
		envVars,
		app.RepoURL,
		app.Branch,
		app.ComposePath,
		target,
		app.RepoAuthMethod,
		repoSecret,
		progress.Update,
	)
	progress.Flush()
	if err != nil {
		se := syncerr.Classify(err)
		if r.Logger != nil {
			r.Logger.Error("Sync apply failed", "app_id", app.ID, "commit", target, "code", se.Code, "stage", se.Stage, "output", truncateOutput(output))
		}
		now := time.Now()
		_ = r.Registry.UpdateSyncResultErr(
			app.ID,
			"error",
			now,
			app.LastSyncedCommit,
			app.LastSyncedCommitMessage,
			output,
			se,
		)
		metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
		return se
	}

	if err := r.confirmHealthy(ctx, app, target); err != nil {
		return r.rollbackUnhealthy(ctx, app, target, output, err, syncStartedAt)
	}

	now := time.Now()
	syncErr := ""
	syncedMessage := app.LastSeenCommitMessage
	rejected := ""
	if target != strings.TrimSpace(app.LastSeenCommit) {
		// Re-applied the last healthy revision while git still points at a rejected commit.
		syncErr = syncerr.Encode(syncerr.New(app.LastSyncErrorCode, app.LastSyncErrorStage, app.LastSyncError))
		syncedMessage = app.LastSyncedCommitMessage
		rejected = app.RejectedCommit
		if rejected == "" {
			rejected = app.LastSeenCommit
		}
	}
	if err := r.Registry.UpdateSyncResult(
		app.ID,
		"synced",
		now,
		target,
		syncedMessage,
		output,
		syncErr,
	); err != nil && r.Logger != nil {
		r.Logger.Warn("Failed to update app status", "app_id", app.ID, "error", err)
	}
	_ = r.Registry.UpdateRollback(app.ID, target, rejected)
	r.recordAppliedEnvHash(app)
	r.recordAppliedWatchHash(app)
	if syncErr == "" {
		metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), nil)
		metrics.ObserveReconcileSuccess()
		metrics.SetLastSyncTimestamp(app.Name)
	}
	return nil
}

// revisionToApply returns the git commit to deploy.
// A revision that failed its health timeout is not applied again until git moves on.
func revisionToApply(app *App) string {
	if app == nil {
		return ""
	}
	seen := strings.TrimSpace(app.LastSeenCommit)
	rejected := strings.TrimSpace(app.RejectedCommit)
	healthy := strings.TrimSpace(app.LastHealthyCommit)
	if rejected != "" && seen == rejected && healthy != "" {
		return healthy
	}
	return seen
}

type projectHealthProbe interface {
	ProjectHealth(ctx context.Context, appID string) (compose.ProjectRuntimeState, error)
}

func healthPollInterval(timeout time.Duration) time.Duration {
	poll := 2 * time.Second
	if timeout < poll {
		poll = timeout / 4
	}
	if poll < 5*time.Millisecond {
		poll = 5 * time.Millisecond
	}
	return poll
}

// confirmHealthy waits until compose healthchecks pass, or returns when the app has no probe.
func (r *Reconciler) confirmHealthy(ctx context.Context, app *App, revision string) error {
	probe, ok := r.Executor.(projectHealthProbe)
	if !ok || app == nil {
		return nil
	}
	timeout := app.HealthTimeoutDuration()
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(healthPollInterval(timeout))
	defer ticker.Stop()

	var lastErr error
	for {
		state, err := probe.ProjectHealth(hctx, app.ID)
		if err != nil {
			lastErr = err
		} else if state.Ready() {
			return nil
		} else {
			lastErr = fmt.Errorf("running=%d/%d exited=%d unhealthy=%d starting=%d",
				state.RunningCount, state.ContainerCount, state.ExitedCount, state.UnhealthyCount, state.StartingCount)
		}
		select {
		case <-hctx.Done():
			if lastErr == nil {
				lastErr = hctx.Err()
			}
			return fmt.Errorf("revision %s not healthy within %s: %v", revision, timeout, lastErr)
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) rollbackUnhealthy(ctx context.Context, app *App, failedRevision, output string, healthErr error, syncStartedAt time.Time) error {
	msg := healthErr.Error()
	prev := strings.TrimSpace(app.LastHealthyCommit)
	if prev != "" && prev != failedRevision {
		if r.Logger != nil {
			r.Logger.Warn("Revision failed health timeout; rolling back", "app_id", app.ID, "failed", failedRevision, "rollback", prev, "error", healthErr)
		}
		repoSecret, err := r.Registry.GetRepoSecret(app.ID)
		if err != nil {
			msg = fmt.Sprintf("%s; rollback to %s failed: %v", msg, prev, err)
		} else {
			defer zeroBytes(repoSecret)
			envVars, envErr := r.Registry.GetAppEnvs(app.ID)
			if envErr != nil {
				msg = fmt.Sprintf("%s; rollback to %s failed: %v", msg, prev, envErr)
			} else {
				rbCtx, cancel := context.WithTimeout(context.Background(), r.Config.SyncTimeout)
				rbOut, rbErr := r.Executor.Apply(
					rbCtx,
					app.ID,
					"",
					envVars,
					app.RepoURL,
					app.Branch,
					app.ComposePath,
					prev,
					app.RepoAuthMethod,
					repoSecret,
					nil,
				)
				cancel()
				if strings.TrimSpace(rbOut) != "" {
					output = output + "\n" + rbOut
				}
				if rbErr != nil {
					msg = fmt.Sprintf("%s; rollback to %s failed: %v", msg, prev, rbErr)
				} else if err := r.confirmHealthy(ctx, app, prev); err != nil {
					msg = fmt.Sprintf("%s; rollback to %s is not healthy: %v", msg, prev, err)
				} else {
					msg = fmt.Sprintf("%s; rolled back to %s", healthErr.Error(), prev)
					se := syncerr.New(syncerr.CodeUnhealthy, syncerr.StageHealth, msg)
					now := time.Now()
					_ = r.Registry.UpdateSyncResult(app.ID, "synced", now, prev, app.LastSyncedCommitMessage, output, syncerr.Encode(se))
					_ = r.Registry.UpdateRollback(app.ID, prev, failedRevision)
					r.recordAppliedEnvHash(app)
					r.recordAppliedWatchHash(app)
					metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
					return se
				}
			}
		}
	}
	se := syncerr.New(syncerr.CodeUnhealthy, syncerr.StageHealth, msg)
	now := time.Now()
	_ = r.Registry.UpdateSyncResultErr(app.ID, "error", now, failedRevision, app.LastSeenCommitMessage, output, se)
	_ = r.Registry.UpdateRollback(app.ID, prev, failedRevision)
	metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
	return se
}

func (r *Reconciler) checkPipelineGate(app *App) error {
	if app == nil {
		return nil
	}
	policy := app.Policy()
	if !policy.RequirePipelineSuccess {
		return nil
	}
	// Only gate when deploying a new commit (or first sync).
	if app.LastSeenCommit != "" && app.LastSeenCommit == app.LastSyncedCommit {
		return nil
	}
	token := cigate.TokenFromEnv()
	checker := &cigate.Checker{Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := checker.CheckCommit(ctx, app.RepoURL, app.LastSeenCommit)
	switch res.Outcome {
	case cigate.OutcomeAllow, "":
		if res.Allowed {
			return nil
		}
	case cigate.OutcomeWait:
		msg := fmt.Sprintf("deploy waiting: pipeline status=%s reason=%s", res.Status, res.Reason)
		return &gateWait{msg: msg, reason: res.Reason, status: res.Status}
	}
	reason := res.Reason
	if reason == "" {
		reason = cigate.ReasonCIFailed
	}
	msg := fmt.Sprintf("deploy blocked: pipeline status=%s reason=%s", res.Status, reason)
	if r.Logger != nil {
		r.Logger.Warn("Deploy blocked by CI gate", "app", app.Name, "commit", app.LastSeenCommit, "reason", reason, "pipeline_status", res.Status)
	}
	return syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate, msg)
}

func (r *Reconciler) handleWatchFileDrift(app *App) {
	mgr, ok := r.Executor.(WatchFileManager)
	if !ok || app == nil {
		return
	}
	policy := app.Policy()
	if len(policy.WatchFiles) == 0 {
		return
	}
	hash, err := mgr.WatchFilesHashForApp(app.ID, policy.WatchFiles)
	if err != nil {
		metrics.SetWatchHashError(app.Name, true)
		msg := err.Error()
		r.mu.Lock()
		prev := r.watchHashErrSeen[app.ID]
		r.watchHashErrSeen[app.ID] = msg
		r.mu.Unlock()
		if prev != msg && r.Logger != nil {
			r.Logger.Error("watch_files hash failed", "app", app.Name, "app_id", app.ID, "error", err)
		}
		return
	}
	metrics.SetWatchHashError(app.Name, false)
	r.mu.Lock()
	delete(r.watchHashErrSeen, app.ID)
	r.mu.Unlock()
	if hash == "" {
		return
	}
	if app.LastAppliedWatchHash == "" {
		_ = r.Registry.UpdateWatchHash(app.ID, hash)
		return
	}
	if hash == app.LastAppliedWatchHash {
		return
	}
	if r.Logger != nil {
		r.Logger.Info("watch_files changed", "app_id", app.ID, "app", app.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, w := range policy.WatchFiles {
		switch w.Action {
		case "reload":
			if w.ReloadURL != "" {
				if err := mgr.HTTPReload(ctx, w.ReloadURL); err != nil && r.Logger != nil {
					r.Logger.Error("watch_files reload failed", "app", app.Name, "path", w.Path, "error", err)
				} else if r.Logger != nil {
					r.Logger.Info("watch_files reload applied", "app", app.Name, "path", w.Path, "url", w.ReloadURL)
				}
			} else if w.Service != "" {
				// Fallback: restart service if reload_url missing
				if _, err := mgr.RestartService(ctx, app.ID, app.ComposePath, w.Service); err != nil && r.Logger != nil {
					r.Logger.Error("watch_files restart failed", "app", app.Name, "service", w.Service, "error", err)
				}
			}
		default: // restart
			svc := w.Service
			if svc == "" {
				continue
			}
			if _, err := mgr.RestartService(ctx, app.ID, app.ComposePath, svc); err != nil && r.Logger != nil {
				r.Logger.Error("watch_files restart failed", "app", app.Name, "service", svc, "error", err)
			} else if r.Logger != nil {
				r.Logger.Info("watch_files restart applied", "app", app.Name, "service", svc, "path", w.Path)
			}
		}
	}
	_ = r.Registry.UpdateWatchHash(app.ID, hash)
	metrics.ObserveWatchAction(app.Name)
}

func (r *Reconciler) recordAppliedEnvHash(app *App) {
	hasher, ok := r.Executor.(EnvFileHasher)
	if !ok || app == nil {
		return
	}
	hash, err := hasher.ExternalEnvHashForApp(app.ID, app.ComposePath)
	if err != nil || hash == "" {
		return
	}
	_ = r.Registry.UpdateEnvHash(app.ID, hash)
	metrics.SetEnvDrift(app.Name, false)
	metrics.SetEnvHashApplied(app.ID, app.Name, time.Now())
}

func (r *Reconciler) recordAppliedWatchHash(app *App) {
	mgr, ok := r.Executor.(WatchFileManager)
	if !ok || app == nil {
		return
	}
	policy := app.Policy()
	if len(policy.WatchFiles) == 0 {
		return
	}
	hash, err := mgr.WatchFilesHashForApp(app.ID, policy.WatchFiles)
	if err != nil || hash == "" {
		return
	}
	_ = r.Registry.UpdateWatchHash(app.ID, hash)
}

func truncateOutput(value string) string {
	const maxLen = 2000
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "...(truncated)"
}
