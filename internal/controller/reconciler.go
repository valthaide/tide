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

	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// ReconcilerConfig controls how the monolith applies desired state.
type ReconcilerConfig struct {
	Interval       time.Duration
	SyncTimeout    time.Duration
	RetryErrors    bool
	MaxConcurrent  int
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

// Reconciler applies desired state directly on the host (monolith mode).
type Reconciler struct {
	Registry *Registry
	Executor AppRuntime
	Logger   *slog.Logger
	Config   ReconcilerConfig

	mu      sync.Mutex
	running bool
	sem     chan struct{}
}

// NewReconciler creates a new reconciler.
func NewReconciler(registry *Registry, executor AppRuntime, logger *slog.Logger, cfg ReconcilerConfig) *Reconciler {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 2
	}
	return &Reconciler{
		Registry: registry,
		Executor: executor,
		Logger:   logger,
		Config:   cfg,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
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

		switch app.Status {
		case "pending":
		case "error":
			if !r.Config.RetryErrors {
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
		app.LastSeenCommit,
		app.RepoAuthMethod,
		repoSecret,
		progress.Update,
	)
	progress.Flush()
	if err != nil {
		se := syncerr.Classify(err)
		if r.Logger != nil {
			r.Logger.Error("Sync apply failed", "app_id", app.ID, "commit", app.LastSeenCommit, "code", se.Code, "stage", se.Stage, "output", truncateOutput(output))
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

	now := time.Now()
	if err := r.Registry.UpdateSyncResult(
		app.ID,
		"synced",
		now,
		app.LastSeenCommit,
		app.LastSeenCommitMessage,
		output,
		"",
	); err != nil && r.Logger != nil {
		r.Logger.Warn("Failed to update app status", "app_id", app.ID, "error", err)
	}
	metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), nil)
	return nil
}

func truncateOutput(value string) string {
	const maxLen = 2000
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "...(truncated)"
}
