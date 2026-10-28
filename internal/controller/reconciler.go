package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/cigate"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/deploypolicy"
	"github.com/valthaide/conops-private/internal/fleetcheck"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/syncerr"
)

type imageApplier interface {
	ApplyImages(
		ctx context.Context,
		appID, content string,
		envVars map[string]string,
		repoURL, branch, composePath, commitHash string,
		repoAuthMethod string,
		repoSecret []byte,
		opts compose.ApplyOptions,
		onProgress func(string),
	) (string, error)
}

// gateWait is returned when CI is still running; sync should stay pending and retry.
type gateWait struct {
	msg        string
	reason     string
	status     string
	source     string
	pipelineID int64
}

func (e *gateWait) Error() string { return e.msg }

// ReconcilerConfig controls how the monolith applies desired state.
type ReconcilerConfig struct {
	Interval             time.Duration
	SyncTimeout          time.Duration
	RetryErrors          bool
	MaxConcurrent        int
	PullParallel         int
	AppReconcileInterval time.Duration
	AppReconcileOff      bool
	ReconcileJitter      time.Duration
	QueueAging           time.Duration
	QueueWarnAfter       time.Duration
	SlotHealthHold       time.Duration
	SlotHealthRelease    bool
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
	parallelEnv := "CONOPS_RECONCILE_CONCURRENCY"
	value := strings.TrimSpace(os.Getenv(parallelEnv))
	if value == "" {
		parallelEnv = "CONOPS_SYNC_PARALLEL"
		value = strings.TrimSpace(os.Getenv(parallelEnv))
	}
	if value == "" {
		parallelEnv = "CONOPS_MAX_CONCURRENT_SYNCS"
		value = strings.TrimSpace(os.Getenv(parallelEnv))
	}
	if value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return ReconcilerConfig{}, fmt.Errorf("invalid %s: %s", parallelEnv, value)
		}
		maxConcurrent = n
	}

	pullParallel := 0
	if value := strings.TrimSpace(os.Getenv("CONOPS_PULL_PARALLEL")); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_PULL_PARALLEL: %s", value)
		}
		pullParallel = n
	}

	appInterval := 5 * time.Minute
	appOff := false
	if value := strings.TrimSpace(os.Getenv("CONOPS_APP_RECONCILE_INTERVAL")); value != "" {
		if value == "off" || value == "0" {
			appOff = true
		} else {
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed < 30*time.Second {
				return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_APP_RECONCILE_INTERVAL: %s", value)
			}
			appInterval = parsed
		}
	}
	var jitter time.Duration
	if value := strings.TrimSpace(os.Getenv("CONOPS_RECONCILE_JITTER")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < 0 {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_RECONCILE_JITTER: %s", value)
		}
		jitter = parsed
	}
	aging := 10 * time.Minute
	if value := strings.TrimSpace(os.Getenv("CONOPS_QUEUE_AGING")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_QUEUE_AGING: %s", value)
		}
		aging = parsed
	}
	warnAfter := 15 * time.Minute
	if value := strings.TrimSpace(os.Getenv("CONOPS_QUEUE_WARN_AFTER")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_QUEUE_WARN_AFTER: %s", value)
		}
		warnAfter = parsed
	}
	slotRelease := false
	var slotHold time.Duration
	if value := strings.TrimSpace(os.Getenv("CONOPS_SLOT_HEALTH_HOLD")); value != "" && value != "until_healthy" {
		slotRelease = true
		if value != "0" {
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed < 0 {
				return ReconcilerConfig{}, fmt.Errorf("invalid CONOPS_SLOT_HEALTH_HOLD: %s", value)
			}
			slotHold = parsed
		}
	}

	return ReconcilerConfig{
		Interval:             interval,
		SyncTimeout:          timeout,
		RetryErrors:          retryErrors,
		MaxConcurrent:        maxConcurrent,
		PullParallel:         pullParallel,
		AppReconcileInterval: appInterval,
		AppReconcileOff:      appOff,
		ReconcileJitter:      jitter,
		QueueAging:           aging,
		QueueWarnAfter:       warnAfter,
		SlotHealthHold:       slotHold,
		SlotHealthRelease:    slotRelease,
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
	WatchEntryHashesForApp(appID string, files []deploypolicy.WatchFile) (map[string]string, error)
	RestartService(ctx context.Context, appID, composePath, service string) (string, error)
	HTTPReload(ctx context.Context, reloadURL string) error
}

// pipelineGate is the CI check. Tests substitute it; production leaves it nil
// and queries GitLab.
type pipelineGate interface {
	CheckCommit(ctx context.Context, repoURL, commitSHA string) cigate.Result
}

// Reconciler applies desired state directly on the host (monolith mode).
type Reconciler struct {
	Registry *Registry
	Executor AppRuntime
	Logger   *slog.Logger
	Config   ReconcilerConfig
	Gate     pipelineGate

	mu               sync.Mutex
	scheduling       bool
	sem              chan struct{}
	pullSem          chan struct{}
	dispatchCursor   int
	inFlight         map[string]struct{}
	nudge            chan struct{}
	retryMu          sync.Mutex
	retryAfter       map[string]transientRetry
	wg               sync.WaitGroup
	watchHashErrSeen map[string]string    // appID -> last error text (rate-limit logs)
	ciWaitSince      map[string]time.Time // appID -> when CI wait started
	forceApply       map[string]struct{}
	driftStreak      map[string]int
	recoverStreak    map[string]int
	lastGC           time.Time
	limit            int
	manualWaiting    int
	queue            map[string]*QueueItem
	nextAt           map[string]time.Time
	lastAt           map[string]time.Time
	running          []compose.RunningImage
	warnedQueue      map[string]time.Time
	skipCI           map[string]struct{}
}

// NewReconciler creates a new reconciler.
func NewReconciler(registry *Registry, executor AppRuntime, logger *slog.Logger, cfg ReconcilerConfig) *Reconciler {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 2
	}
	capN := cfg.MaxConcurrent
	if capN < 64 {
		capN = 64
	}
	return &Reconciler{
		Registry:         registry,
		Executor:         executor,
		Logger:           logger,
		Config:           cfg,
		sem:              make(chan struct{}, capN),
		limit:            cfg.MaxConcurrent,
		inFlight:         make(map[string]struct{}),
		nudge:            make(chan struct{}, 1),
		retryAfter:       map[string]transientRetry{},
		watchHashErrSeen: make(map[string]string),
		ciWaitSince:      make(map[string]time.Time),
		forceApply:       map[string]struct{}{},
		driftStreak:      map[string]int{},
		recoverStreak:    map[string]int{},
		queue:            map[string]*QueueItem{},
		nextAt:           map[string]time.Time{},
		lastAt:           map[string]time.Time{},
		warnedQueue:      map[string]time.Time{},
		skipCI:           map[string]struct{}{},
	}
}

// AcquireSync slot for force-sync and reconcile (shared concurrency limit).
func (r *Reconciler) AcquireSync(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		full := r.limit > 0 && len(r.inFlight) >= r.limit
		r.mu.Unlock()
		if !full {
			select {
			case r.sem <- struct{}{}:
				return nil
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ReleaseSync releases a previously acquired sync slot.
func (r *Reconciler) ReleaseSync() {
	select {
	case <-r.sem:
	default:
	}
}

// Run starts the reconciliation loop. A sync that is still running does not
// block the next pass, so other apps leave pending as slots free.
func (r *Reconciler) Run(ctx context.Context) {
	r.dispatch()

	ticker := time.NewTicker(r.Config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.wg.Wait()
			if r.Logger != nil {
				r.Logger.Info("Reconciler stopped")
			}
			return
		case <-ticker.C:
			r.dispatch()
		case <-r.nudge:
			r.dispatch()
		}
	}
}

func (r *Reconciler) reconcileOnce() {
	r.dispatch()
	r.wg.Wait()
}

func (r *Reconciler) dispatch() {
	r.mu.Lock()
	if r.scheduling {
		r.mu.Unlock()
		return
	}
	r.scheduling = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.scheduling = false
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
	sort.SliceStable(apps, func(i, j int) bool {
		return apps[i].Name < apps[j].Name
	})
	blocked := r.blockedApps(apps)
	r.noteProjects(apps)
	r.publishRunningImages(apps)
	r.publishPublicVars(apps)
	r.runPeriodic(apps, runtimeSnapshot)
	apps = r.orderByQueue(apps)
	for _, app := range apps {
		if app.Suspended {
			continue
		}
		if msg, ok := blocked[app.Name]; ok {
			r.declareOnce(app, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, msg))
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

		if r.maybeRecover(app, runtimeSnapshot) {
			continue
		}
		if app.Status == "synced" && runtimeSnapshot != nil {
			if reason := r.runtimeDriftReason(app.ComposeProject(), runtimeSnapshot); reason != "" {
				if r.noteDrift(app.ID) >= 2 {
					r.clearDrift(app.ID)
					r.requeuePending(app, reason)
				}
			} else {
				r.clearDrift(app.ID)
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
		if app.Status == "synced" || app.Status == "failed" {
			if reason := r.varsDriftReason(app); reason != "" {
				r.requeuePending(app, reason)
			}
		}

		switch app.Status {
		case "pending":
		case "error":
			// Always re-check CI-blocked apps until the seen commit is deployed
			// (pipeline may flip pending→success or failed→success after retry).
			if app.LastSyncErrorCode == syncerr.CodeBlockedByCI &&
				app.LastSeenCommit != "" && app.LastSeenCommit != app.LastSyncedCommit {
				// fall through
			} else if r.shouldRetryRejected(app, runtimeSnapshot) {
				// fall through
			} else if !r.Config.RetryErrors {
				continue
			}
		case "failed":
			if !r.transientReady(app) {
				continue
			}
		default:
			continue
		}

		if r.inFlightApp(app.ID) {
			continue
		}
		if r.inTransientPause(app) {
			continue
		}
		if r.configHold(app) {
			if !r.inTransientPause(app) {
				r.scheduleTransient(app.ID)
			}
			continue
		}
		appCopy := app
		if !r.tryStart(appCopy.ID) {
			if r.Logger != nil {
				r.Logger.Debug("Sync slot busy; leaving app pending", "app_id", appCopy.ID)
			}
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.finishSync(appCopy.ID)
			r.markQueueStart(appCopy.ID)
			if err := r.syncApp(appCopy); err != nil && r.Logger != nil {
				r.Logger.Error("App sync failed", "app_id", appCopy.ID, "error", redactAppText(appCopy, err.Error()))
			}
		}()
	}
	r.publishQueue()
	r.maybeGC()
}

func (r *Reconciler) inFlightApp(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.inFlight[id]
	return ok
}

func (r *Reconciler) tryStart(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.inFlight[id]; ok {
		return false
	}
	if r.manualWaiting > 0 {
		return false
	}
	if r.limit > 0 && len(r.inFlight) >= r.limit {
		return false
	}
	select {
	case r.sem <- struct{}{}:
		r.inFlight[id] = struct{}{}
		return true
	default:
		return false
	}
}

func (r *Reconciler) finishSync(id string) {
	r.dequeue(id)
	r.mu.Lock()
	delete(r.inFlight, id)
	r.mu.Unlock()
	r.ReleaseSync()
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

func (r *Reconciler) captureRuntimeSnapshot() (map[string]compose.ProjectRuntimeState, error) {
	if r.Executor == nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.Executor.SnapshotProjects(ctx)
}

func (r *Reconciler) runtimeDriftReason(project string, snapshot map[string]compose.ProjectRuntimeState) string {
	state, ok := snapshot[project]
	if !ok || state.ContainerCount == 0 {
		return "runtime_missing"
	}
	if state.FlappingCount > 0 {
		return "runtime_restarting"
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
	extra, missing := fleetEnvHash(app.Policy().EnvFiles)
	if missing != "" {
		se := syncerr.New(syncerr.CodeEnvFileMissing, syncerr.StageValidate, "env_file_missing: "+missing)
		r.declareOnce(app, se)
		return ""
	}
	if extra != "" {
		if hash == "" {
			hash = extra
		} else {
			hash = hash + "+" + extra
		}
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
	r.enqueue(app, triggerForReason(reason), reason, appPriority(app))
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
	gen, _ := r.Registry.DesiredGeneration(app.ID)
	if err := r.Registry.UpdateStatus(app.ID, "syncing", &syncStartedAt); err != nil && r.Logger != nil {
		r.Logger.Warn("Failed to mark app syncing", "app_id", app.ID, "error", err)
	}
	target := revisionToApply(app, r.forcing(app.ID))

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
				metrics.ObserveDeployBlocked(app.Name, wait.reason, wait.source)
				r.noteCIWait(app, wait.status, wait.reason)
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
			if se.Code != syncerr.CodeBlockedByCI {
				metrics.ObserveDeployBlocked(app.Name, reason, "")
			}
			r.noteCIFinished(app, "failed")
			return se
		}
	}
	r.noteCIFinished(app, "success")
	metrics.ClearDeployBlocked(app.Name)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	policy := app.Policy()
	syncTimeout := r.Config.SyncTimeout
	if policy.SyncTimeout != "" {
		parsed, issue := fleetcheck.ParseDuration(policy.SyncTimeout)
		if issue != nil {
			se := syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issue.Message)
			r.declareOnce(app, se)
			return nil
		}
		syncTimeout = parsed
	}
	pullIdle := syncTimeout
	if policy.PullTimeout != "" {
		parsed, issue := fleetcheck.ParseDuration(policy.PullTimeout)
		if issue != nil {
			se := syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issue.Message)
			r.declareOnce(app, se)
			return nil
		}
		pullIdle = parsed
	}
	oneshotTimeout := 10 * time.Minute
	if policy.OneshotTimeout != "" {
		parsed, issue := fleetcheck.ParseDuration(policy.OneshotTimeout)
		if issue != nil {
			se := syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issue.Message)
			r.declareOnce(app, se)
			return nil
		}
		oneshotTimeout = parsed
	}

	opts := compose.ApplyOptions{
		Vars:              policy.Vars,
		BuildMode:         policy.EffectiveBuild(),
		Project:           app.ComposeProject(),
		RequireDigest:     policy.RequireDigest,
		PullIdle:          pullIdle,
		OneshotTimeout:    oneshotTimeout,
		SyncTimeout:       syncTimeout,
		BindPrefixes:      bindPrefixes(),
		AllowedRegistries: policy.AllowedRegistries,
		PolicyExceptions:  policy.PolicyExceptions,
		CreateVolumes:     policy.CreateVolumes,
		VolumeNamePattern: policy.VolumeNamePattern,
		OnVolumeCreated: func(name string) {
			if r.Logger != nil {
				r.Logger.Info("Created external volume", "app", app.Name, "volume", name)
			}
			r.noteDeploy(app, "volume_created", target, deploypolicy.EncodeVars(policy.Vars), name, "volume")
		},
	}
	if target != strings.TrimSpace(app.LastSeenCommit) {
		opts.Vars = deploypolicy.DecodeVars(app.LastHealthyVars)
		opts.NoBuild = true
		opts.BuildMode = deploypolicy.BuildForbidden
	}
	fileVars, missing := readEnvFiles(policy.EnvFiles)
	if missing != "" {
		se := syncerr.New(syncerr.CodeEnvFileMissing, syncerr.StageValidate, "env_file_missing: "+missing)
		r.declareOnce(app, se)
		return nil
	}
	fleetVars := opts.Vars
	opts.Vars = mergeInterpolation(fileVars, fleetVars, app.Name, app.ComposeProject())

	progress := newSyncProgressReporter(r.Registry, r.Logger, app.ID, syncProgressFlushInterval)
	output, err := r.applyRuntime(ctx, app, envVars, repoSecret, target, opts, progress.Update)
	progress.Flush()
	if err != nil {
		se := syncerr.Classify(err)
		status := "error"
		if declaredFailure(se) {
			status = "failed"
		}
		attempt, wait := 0, time.Duration(0)
		if transientFailure(se) {
			attempt, wait = r.scheduleTransient(app.ID)
		} else {
			r.clearTransient(app.ID)
		}
		if r.Logger != nil {
			if declaredFailure(se) {
				if attempt > 1 {
					r.Logger.Info("Retrying transient failure", "app", app.Name, "reason", se.Message, "attempt", attempt, "next", wait.String())
				} else {
					r.Logger.Error("App sync failed", "app", app.Name, "reason", redactAppText(app, se.Message))
				}
			} else {
				r.Logger.Error("Sync apply failed", "app_id", app.ID, "commit", target, "code", se.Code, "stage", se.Stage, "output", truncateOutput(output))
			}
		}
		now := time.Now()
		superseded, _ := r.Registry.FinishSync(
			app.ID,
			gen,
			status,
			now,
			app.LastSyncedCommit,
			app.LastSyncedCommitMessage,
			output,
			syncerr.Encode(se),
		)
		if superseded && r.Logger != nil {
			r.Logger.Info("Desired state changed during sync; queued again", "app", app.Name, "skipped", 1)
		}
		if transientFailure(se) {
			// Keep the attempted vars as rejected so the next loop does not
			// treat the same fleet vars as a new revision and skip the backoff.
			_ = r.Registry.UpdateRollback(app.ID, app.LastHealthyCommit, app.RejectedCommit, app.LastHealthyVars, deploypolicy.EncodeVars(fleetVars))
		}
		metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
		if declaredFailure(se) {
			if strings.TrimSpace(app.LastSyncError) != strings.TrimSpace(se.Message) {
				r.noteDeploy(app, "failed", target, deploypolicy.EncodeVars(policy.Vars), "", deployTrigger(app, target))
			}
			return nil
		}
		return se
	}

	if r.releaseHealthSlot(app, target, output, syncStartedAt, gen) {
		return nil
	}
	if err := r.confirmHealthy(ctx, app, target); err != nil {
		metrics.ObserveReconcile(app.Name, "rolled_back", r.currentTrigger(app.ID))
		return r.rollbackUnhealthy(ctx, app, target, output, err, syncStartedAt, gen)
	}
	metrics.ObserveReconcile(app.Name, "applied", r.currentTrigger(app.ID))

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
	superseded, err := r.Registry.FinishSync(
		app.ID,
		gen,
		"synced",
		now,
		target,
		syncedMessage,
		output,
		syncErr,
	)
	if err != nil && r.Logger != nil {
		r.Logger.Warn("Failed to update app status", "app_id", app.ID, "error", err)
	}
	if superseded && r.Logger != nil {
		r.Logger.Info("Desired state changed during sync; queued again", "app", app.Name, "skipped", 1)
	}
	if r.Logger != nil && syncErr == "" {
		r.Logger.Info("Applied", "app", app.Name, "revision", revisionLabel(target, fleetVars, policy.PublicVars), "duration", time.Since(syncStartedAt).Round(time.Millisecond).String())
	}
	rejectedVars := ""
	if rejected != "" {
		rejectedVars = app.RejectedVars
	}
	_ = r.Registry.UpdateRollback(app.ID, target, rejected, deploypolicy.EncodeVars(fleetVars), rejectedVars)
	r.recordAppliedEnvHash(app)
	r.recordAppliedWatchHash(app)
	if syncErr == "" {
		r.clearTransient(app.ID)
		_ = r.Registry.RecordRevision(app.ID, target, deploypolicy.EncodeVars(policy.Vars), metrics.FleetSHA(), revisionImages(r, app))
		r.noteDeploy(app, "applied", target, deploypolicy.EncodeVars(policy.Vars), revisionImages(r, app), deployTrigger(app, target))
		metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), nil)
		metrics.ObserveReconcileSuccess()
		metrics.SetLastSyncTimestamp(app.Name)
	}
	return nil
}

type transientRetry struct {
	attempts int
	next     time.Time
}

func transientFailure(se *syncerr.Error) bool {
	if se == nil {
		return false
	}
	switch se.GetCode() {
	case syncerr.CodePullFailed, syncerr.CodeRegistryAuth, syncerr.CodeDigestRequired, syncerr.CodeRegistryDenied, syncerr.CodeOneshotFailed, syncerr.CodeOneshotTimeout:
		return true
	case syncerr.CodeValidation:
		return strings.Contains(strings.ToLower(se.Message), "policy_violation")
	default:
		return false
	}
}

func transientBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := 15 * time.Second
	for i := 1; i < attempt; i++ {
		wait *= 2
		if wait >= 5*time.Minute {
			return 5 * time.Minute
		}
	}
	return wait
}

func (r *Reconciler) transientReady(app *App) bool {
	if app == nil || !transientFailure(appTransientError(app)) {
		return false
	}
	r.retryMu.Lock()
	defer r.retryMu.Unlock()
	st := r.retryAfter[app.ID]
	return st.next.IsZero() || !time.Now().Before(st.next)
}

func (r *Reconciler) inTransientPause(app *App) bool {
	if app == nil || !transientFailure(appTransientError(app)) {
		return false
	}
	r.retryMu.Lock()
	defer r.retryMu.Unlock()
	st := r.retryAfter[app.ID]
	return !st.next.IsZero() && time.Now().Before(st.next)
}

func appTransientError(app *App) *syncerr.Error {
	if app == nil {
		return nil
	}
	return &syncerr.Error{Code: app.LastSyncErrorCode, Message: app.LastSyncError}
}

func (r *Reconciler) scheduleTransient(appID string) (int, time.Duration) {
	r.retryMu.Lock()
	defer r.retryMu.Unlock()
	if r.retryAfter == nil {
		r.retryAfter = map[string]transientRetry{}
	}
	st := r.retryAfter[appID]
	st.attempts++
	wait := transientBackoff(st.attempts)
	st.next = time.Now().Add(wait)
	r.retryAfter[appID] = st
	return st.attempts, wait
}

func (r *Reconciler) clearTransient(appID string) {
	r.retryMu.Lock()
	delete(r.retryAfter, appID)
	r.retryMu.Unlock()
}

func (r *Reconciler) noteProjects(apps []*App) {
	type namer interface {
		RememberProject(appID, project string)
	}
	rec, ok := r.Executor.(namer)
	if !ok {
		return
	}
	for _, app := range apps {
		if app == nil {
			continue
		}
		rec.RememberProject(app.ID, app.ComposeProject())
	}
}

// revisionToApply returns the git commit to deploy.
// A revision that failed its health timeout is not applied again until git moves on.
func revisionToApply(app *App, force bool) string {
	if app == nil {
		return ""
	}
	seen := strings.TrimSpace(app.LastSeenCommit)
	if force {
		return seen
	}
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

	var lastState compose.ProjectRuntimeState
	var sawState bool
	var lastProbeErr error
	for {
		state, err := probe.ProjectHealth(hctx, app.ComposeProject())
		if err != nil {
			if hctx.Err() == nil {
				lastProbeErr = err
			}
		} else {
			sawState = true
			lastState = state
			if state.Ready() {
				return nil
			}
		}
		select {
		case <-hctx.Done():
			if sawState {
				return healthFailure(revision, timeout, lastState)
			}
			if lastProbeErr != nil {
				return fmt.Errorf("health timeout after %s: %v", timeout, lastProbeErr)
			}
			return fmt.Errorf("health timeout after %s: no container status", timeout)
		case <-ticker.C:
		}
	}
}

func healthFailure(revision string, timeout time.Duration, state compose.ProjectRuntimeState) error {
	if reason := strings.TrimSpace(state.Failure); reason != "" {
		return fmt.Errorf("%s (revision %s)", reason, revision)
	}
	names := strings.Join(state.NotReady, ", ")
	if names == "" {
		names = "unknown"
	}
	if state.UnhealthyCount > 0 {
		return fmt.Errorf("unhealthy: container %s after %s (revision %s)", names, timeout, revision)
	}
	if state.FlappingCount > 0 {
		return fmt.Errorf("restarting: %s (revision %s)", names, revision)
	}
	return fmt.Errorf("health timeout: container %s after %s (revision %s)", names, timeout, revision)
}

func (r *Reconciler) rollbackUnhealthy(ctx context.Context, app *App, failedRevision, output string, healthErr error, syncStartedAt time.Time, gen int64) error {
	msg := healthErr.Error()
	prev := strings.TrimSpace(app.LastHealthyCommit)
	prevVars := deploypolicy.DecodeVars(app.LastHealthyVars)
	failedVars := deploypolicy.EncodeVars(app.Policy().Vars)
	label := revisionLabel(prev, prevVars, app.Policy().PublicVars)
	varsDiffer := !deploypolicy.VarsEqual(prevVars, app.Policy().Vars)
	if prev != "" && (prev != failedRevision || varsDiffer) {
		if r.Logger != nil {
			r.Logger.Warn("Revision failed health timeout; rolling back", "app", app.Name, "failed", failedRevision, "rollback", label, "error", redactAppText(app, healthErr.Error()))
		}
		repoSecret, err := r.Registry.GetRepoSecret(app.ID)
		if err != nil {
			msg = fmt.Sprintf("%s; rollback to %s failed: %v", msg, label, err)
		} else {
			defer zeroBytes(repoSecret)
			envVars, envErr := r.Registry.GetAppEnvs(app.ID)
			if envErr != nil {
				msg = fmt.Sprintf("%s; rollback to %s failed: %v", msg, label, envErr)
			} else {
				rbCtx, cancel := context.WithTimeout(context.Background(), r.Config.SyncTimeout)
				rbOut, rbErr := r.applyRuntime(rbCtx, app, envVars, repoSecret, prev, compose.ApplyOptions{
					Vars:              mergeInterpolation(fileVarsOf(app), prevVars, app.Name, app.ComposeProject()),
					BuildMode:         deploypolicy.BuildForbidden,
					NoBuild:           true,
					Project:           app.ComposeProject(),
					RequireDigest:     app.Policy().RequireDigest,
					AllowedRegistries: app.Policy().AllowedRegistries,
					CreateVolumes:     app.Policy().CreateVolumes,
					VolumeNamePattern: app.Policy().VolumeNamePattern,
					SyncTimeout:       r.Config.SyncTimeout,
				}, nil)
				cancel()
				if strings.TrimSpace(rbOut) != "" {
					output = output + "\n" + rbOut
				}
				if rbErr != nil {
					msg = fmt.Sprintf("%s; rollback to %s failed: %v", msg, label, rbErr)
				} else if err := r.confirmHealthy(ctx, app, prev); err != nil {
					msg = fmt.Sprintf("%s; rollback to %s is not healthy: %v", msg, label, err)
				} else {
					msg = fmt.Sprintf("%s; rolled back to %s", healthErr.Error(), label)
					if migrationWarning(output) {
						msg += "; migration may have been partially applied"
					}
					se := syncerr.New(syncerr.CodeUnhealthy, syncerr.StageHealth, msg)
					now := time.Now()
					_, _ = r.Registry.FinishSync(app.ID, gen, "rolled_back", now, prev, app.LastSyncedCommitMessage, output, syncerr.Encode(se))
					_ = r.Registry.UpdateRollback(app.ID, prev, failedRevision, app.LastHealthyVars, failedVars)
					r.noteDeploy(app, "rolled_back", prev, app.LastHealthyVars, revisionImages(r, app), "reconcile")
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
	_, _ = r.Registry.FinishSync(app.ID, gen, "error", now, failedRevision, app.LastSeenCommitMessage, output, syncerr.Encode(se))
	_ = r.Registry.UpdateRollback(app.ID, prev, failedRevision, app.LastHealthyVars, failedVars)
	r.scheduleHealthRetry(app)
	metrics.ObserveSync(app.ID, app.Name, time.Since(syncStartedAt), se)
	return se
}

func (r *Reconciler) applyRuntime(ctx context.Context, app *App, envVars map[string]string, repoSecret []byte, commit string, opts compose.ApplyOptions, onProgress func(string)) (string, error) {
	if app == nil {
		return "", fmt.Errorf("app is nil")
	}
	if ia, ok := r.Executor.(imageApplier); ok {
		return ia.ApplyImages(ctx, app.ID, "", envVars, app.RepoURL, app.Branch, app.ComposePath, commit, app.RepoAuthMethod, repoSecret, opts, onProgress)
	}
	return r.Executor.Apply(ctx, app.ID, "", envVars, app.RepoURL, app.Branch, app.ComposePath, commit, app.RepoAuthMethod, repoSecret, onProgress)
}

func (r *Reconciler) varsDriftReason(app *App) string {
	if app == nil {
		return ""
	}
	desired := app.Policy().Vars
	applied := deploypolicy.DecodeVars(app.LastHealthyVars)
	if deploypolicy.VarsEqual(desired, applied) {
		return ""
	}
	rejected := deploypolicy.DecodeVars(app.RejectedVars)
	if deploypolicy.VarsEqual(desired, rejected) &&
		strings.TrimSpace(app.RejectedCommit) != "" &&
		strings.TrimSpace(app.LastSeenCommit) == strings.TrimSpace(app.RejectedCommit) {
		return ""
	}
	// A failed pull of these same vars is not a new revision. Retry only when
	// the transient backoff elapses. A different tag still queues at once.
	if transientFailure(appTransientError(app)) {
		if len(rejected) > 0 && deploypolicy.VarsEqual(desired, rejected) {
			return ""
		}
		if len(rejected) == 0 && !r.transientReady(app) {
			return ""
		}
	}
	return "vars_changed"
}

func (r *Reconciler) publishRunningImages(apps []*App) {
	type imageReporter interface {
		RunningImages(ctx context.Context) ([]compose.RunningImage, error)
	}
	reporter, ok := r.Executor.(imageReporter)
	if !ok || r.Executor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	images, err := reporter.RunningImages(ctx)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.running = images
	r.mu.Unlock()
	byProject := make(map[string]string, len(apps))
	for _, app := range apps {
		if app == nil {
			continue
		}
		name := app.Name
		if name == "" {
			name = app.ID
		}
		byProject[app.ComposeProject()] = name
	}
	samples := make([]metrics.ImageSample, 0, len(images))
	for _, img := range images {
		appName := byProject[img.Project]
		if appName == "" {
			continue
		}
		samples = append(samples, metrics.ImageSample{
			App:     appName,
			Service: img.Service,
			Image:   img.Image,
			Digest:  img.Digest,
		})
	}
	metrics.SetAppImages(samples)
	r.publishImageDrift(apps, images)
}

func (r *Reconciler) publishImageDrift(apps []*App, images []compose.RunningImage) {
	type digestSource interface {
		ExpectedDigests(project string) map[string]string
	}
	source, ok := r.Executor.(digestSource)
	if !ok {
		return
	}
	byProject := map[string]string{}
	for _, app := range apps {
		if app == nil {
			continue
		}
		name := app.Name
		if name == "" {
			name = app.ID
		}
		byProject[app.ComposeProject()] = name
	}
	var drift []metrics.ImageSample
	for _, img := range images {
		expected := source.ExpectedDigests(img.Project)[img.Service]
		running := strings.TrimSpace(img.Digest)
		if expected == "" || running == "" || running == "unknown" {
			continue
		}
		if expected == running || strings.HasSuffix(running, expected) || strings.HasSuffix(expected, running) {
			continue
		}
		appName := byProject[img.Project]
		if appName == "" {
			continue
		}
		drift = append(drift, metrics.ImageSample{App: appName, Service: img.Service})
	}
	metrics.SetImageDrift(drift)
}

func (r *Reconciler) publishPublicVars(apps []*App) {
	var samples []metrics.VarSample
	for _, app := range apps {
		if app == nil {
			continue
		}
		policy := app.Policy()
		for key, value := range policy.Vars {
			if !fleetcheck.PublicVar(key, policy.PublicVars) {
				continue
			}
			name := app.Name
			if name == "" {
				name = app.ID
			}
			samples = append(samples, metrics.VarSample{App: name, Name: key, Value: value})
		}
	}
	metrics.SetPublicVars(samples)
}

func (r *Reconciler) blockedApps(apps []*App) map[string]string {
	blocked := map[string]string{}
	var names []fleetcheck.AppName
	for _, app := range apps {
		if app == nil || app.Suspended {
			continue
		}
		names = append(names, fleetcheck.AppName{Name: app.Name, Project: app.ComposeProject()})
		if issue := fleetcheck.CheckProject(app.Policy().Project); issue != nil {
			blocked[app.Name] = issue.Message
		}
	}
	for _, issue := range fleetcheck.ProjectConflicts(names) {
		if _, exists := blocked[issue.App]; !exists {
			blocked[issue.App] = issue.Message
		}
	}
	return blocked
}

func (r *Reconciler) declareOnce(app *App, se *syncerr.Error) {
	if app == nil || se == nil {
		return
	}
	if app.Status == "failed" && app.LastSyncError == se.Message {
		return
	}
	_ = r.Registry.UpdateSyncResultErr(app.ID, "failed", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
	app.Status = "failed"
	app.LastSyncError = se.Message
	app.LastSyncErrorCode = se.Code
	if r.Logger != nil {
		r.Logger.Error("App sync failed", "app", app.Name, "reason", redactAppText(app, se.Message))
	}
}

func (r *Reconciler) EnablePullLimit(n int) {
	if n < 1 {
		return
	}
	r.pullSem = make(chan struct{}, n)
}

func (r *Reconciler) Acquire(ctx context.Context) error {
	if r == nil || r.pullSem == nil {
		return nil
	}
	select {
	case r.pullSem <- struct{}{}:
		metrics.PullRunning.Inc()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Reconciler) Release() {
	if r == nil || r.pullSem == nil {
		return
	}
	select {
	case <-r.pullSem:
		metrics.PullRunning.Dec()
	default:
	}
}

func bindPrefixes() []string {
	raw := strings.TrimSpace(os.Getenv("CONOPS_ALLOWED_BIND_PREFIXES"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func declaredFailure(se *syncerr.Error) bool {
	if se == nil {
		return false
	}
	switch se.GetCode() {
	case syncerr.CodeBuildForbidden, syncerr.CodeRegistryAuth, syncerr.CodePullFailed, syncerr.CodeUnsetVar, syncerr.CodeDockerAPI,
		syncerr.CodeValidation, syncerr.CodePortConflict, syncerr.CodeVolumeMissing, syncerr.CodeVolumeNameRejected, syncerr.CodeDigestRequired,
		syncerr.CodeOneshotFailed, syncerr.CodeOneshotTimeout, syncerr.CodeRegistryDenied:
		return true
	default:
		return false
	}
}

func revisionLabel(commit string, vars map[string]string, listed []string) string {
	var parts []string
	if label := deploypolicy.VarsLabel(fleetcheck.PublicVars(vars, listed)); label != "" {
		parts = append(parts, label)
	}
	commit = strings.TrimSpace(commit)
	if commit != "" {
		if len(commit) > 12 {
			commit = commit[:12]
		}
		parts = append(parts, "commit "+commit)
	}
	if len(parts) == 0 {
		return "previous revision"
	}
	return strings.Join(parts, ", ")
}

func (r *Reconciler) checkPipelineGate(app *App) error {
	if app == nil {
		return nil
	}
	policy := app.Policy()
	if r.consumeSkipCI(app.ID) {
		return nil
	}
	if !policy.RequirePipelineSuccess {
		return nil
	}
	// Only gate when deploying a new commit (or first sync).
	if app.LastSeenCommit != "" && app.LastSeenCommit == app.LastSyncedCommit {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var res cigate.Result
	if r.Gate != nil {
		res = r.Gate.CheckCommit(ctx, app.RepoURL, app.LastSeenCommit)
	} else {
		res = (&cigate.Checker{
			Token:   cigate.TokenFromEnv(),
			Sources: policy.RequirePipelineSources,
		}).CheckCommit(ctx, app.RepoURL, app.LastSeenCommit)
	}
	switch res.Outcome {
	case cigate.OutcomeAllow, "":
		if res.Allowed {
			if len(res.Ignored) > 0 && r.Logger != nil {
				r.Logger.Info("Deploy allowed without a counted pipeline", "app", app.Name, "commit", app.LastSeenCommit, "ignored_pipelines", cigate.FormatPipelines(res.Ignored))
			}
			return nil
		}
	case cigate.OutcomeWait:
		msg := fmt.Sprintf("deploy waiting: pipeline id=%d source=%s status=%s reason=%s", res.PipelineID, res.Source, res.Status, res.Reason)
		return &gateWait{msg: msg, reason: res.Reason, status: res.Status, source: res.Source, pipelineID: res.PipelineID}
	}
	reason := res.Reason
	if reason == "" {
		reason = cigate.ReasonCIFailed
	}
	msg := fmt.Sprintf("deploy blocked: pipeline id=%d source=%s status=%s reason=%s", res.PipelineID, res.Source, res.Status, reason)
	metrics.ObserveDeployBlocked(app.Name, reason, res.Source)
	if r.Logger != nil {
		r.Logger.Warn("Deploy blocked by CI gate", "app", app.Name, "commit", app.LastSeenCommit, "reason", reason, "pipeline_id", res.PipelineID, "pipeline_source", res.Source, "pipeline_status", res.Status)
	}
	return syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate, msg)
}

func (r *Reconciler) noteCIWait(app *App, status, reason string) {
	if app == nil {
		return
	}
	r.mu.Lock()
	_, waiting := r.ciWaitSince[app.ID]
	if !waiting {
		r.ciWaitSince[app.ID] = time.Now()
	}
	r.mu.Unlock()
	if r.Logger == nil {
		return
	}
	if waiting {
		r.Logger.Debug("Deploy waiting for CI", "app", app.Name, "commit", app.LastSeenCommit, "pipeline_status", status, "reason", reason)
		return
	}
	r.Logger.Info("Deploy waiting for CI", "app", app.Name, "commit", app.LastSeenCommit, "pipeline_status", status, "reason", reason)
}

func (r *Reconciler) noteCIFinished(app *App, result string) {
	if app == nil {
		return
	}
	r.mu.Lock()
	since, waiting := r.ciWaitSince[app.ID]
	delete(r.ciWaitSince, app.ID)
	r.mu.Unlock()
	if !waiting || r.Logger == nil {
		return
	}
	r.Logger.Info("Deploy left CI wait", "app", app.Name, "result", result, "waited", time.Since(since).Round(time.Millisecond).String())
}

func (r *Reconciler) handleWatchFileDrift(app *App) {
	r.reconcileWatchFiles(app)
}

func (r *Reconciler) reconcileWatchFiles(app *App) {
	mgr, ok := r.Executor.(WatchFileManager)
	if !ok || app == nil {
		return
	}
	policy := app.Policy()
	if len(policy.WatchFiles) == 0 {
		return
	}
	hashes, err := mgr.WatchEntryHashesForApp(app.ID, policy.WatchFiles)
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
	if len(hashes) == 0 {
		return
	}

	encoded := encodeWatchHashes(hashes)
	prev, legacy := decodeWatchHashes(app.LastAppliedWatchHash)
	if strings.TrimSpace(app.LastAppliedWatchHash) == "" {
		_ = r.Registry.UpdateWatchHash(app.ID, encoded)
		app.LastAppliedWatchHash = encoded
		return
	}

	var changed []deploypolicy.WatchFile
	if len(prev) > 0 {
		for _, w := range policy.WatchFiles {
			if prev[w.Path] != hashes[w.Path] {
				changed = append(changed, w)
			}
		}
	} else if legacy != "" {
		combined, combErr := mgr.WatchFilesHashForApp(app.ID, policy.WatchFiles)
		if combErr == nil && combined == legacy {
			_ = r.Registry.UpdateWatchHash(app.ID, encoded)
			app.LastAppliedWatchHash = encoded
			return
		}
		changed = append(changed, policy.WatchFiles...)
	}
	if len(changed) == 0 {
		if encoded != app.LastAppliedWatchHash {
			_ = r.Registry.UpdateWatchHash(app.ID, encoded)
			app.LastAppliedWatchHash = encoded
		}
		return
	}
	if r.Logger != nil {
		paths := make([]string, 0, len(changed))
		for _, w := range changed {
			paths = append(paths, w.Path)
		}
		r.Logger.Info("watch_files changed", "app", app.Name, "app_id", app.ID, "paths", strings.Join(paths, ","))
	}
	r.applyWatchActions(app, mgr, changed)
	_ = r.Registry.UpdateWatchHash(app.ID, encoded)
	app.LastAppliedWatchHash = encoded
}

func (r *Reconciler) applyWatchActions(app *App, mgr WatchFileManager, files []deploypolicy.WatchFile) {
	if app == nil || mgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, w := range files {
		switch w.Action {
		case "reload":
			if w.ReloadURL != "" {
				err := mgr.HTTPReload(ctx, w.ReloadURL)
				r.logWatchResult(app.Name, "reload", w.Path, w.ReloadURL, "", err)
			} else if w.Service != "" {
				_, err := mgr.RestartService(ctx, app.ID, app.ComposePath, w.Service)
				r.logWatchResult(app.Name, "restart", w.Path, "", w.Service, err)
			}
		default:
			svc := w.Service
			if svc == "" {
				continue
			}
			_, err := mgr.RestartService(ctx, app.ID, app.ComposePath, svc)
			r.logWatchResult(app.Name, "restart", w.Path, "", svc, err)
		}
	}
	metrics.ObserveWatchAction(app.Name)
}

func (r *Reconciler) logWatchResult(app, action, path, reloadURL, service string, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	metrics.ObserveAppReload(app, result)
	if r.Logger == nil {
		return
	}
	if err != nil {
		r.Logger.Error("watch_files action failed", "app", app, "action", action, "path", path, "url", reloadURL, "service", service, "result", result, "error", err)
		return
	}
	r.Logger.Info("watch_files action applied", "app", app, "action", action, "path", path, "url", reloadURL, "service", service, "result", result)
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
	r.reconcileWatchFiles(app)
}

func encodeWatchHashes(hashes map[string]string) string {
	if len(hashes) == 0 {
		return ""
	}
	keys := make([]string, 0, len(hashes))
	for k := range hashes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(hashes))
	for _, k := range keys {
		ordered[k] = hashes[k]
	}
	data, err := json.Marshal(ordered)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeWatchHashes(raw string) (map[string]string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			return m, ""
		}
	}
	return nil, raw
}

func truncateOutput(value string) string {
	const maxLen = 2000
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= maxLen {
		return trimmed
	}
	return trimmed[:maxLen] + "...(truncated)"
}

func revisionImages(r *Reconciler, app *App) string {
	if r == nil || app == nil || r.Executor == nil {
		return ""
	}
	type refSource interface {
		ExpectedRefs(project string) map[string]string
	}
	if source, ok := r.Executor.(refSource); ok {
		raw, err := json.Marshal(source.ExpectedRefs(app.ComposeProject()))
		if err == nil && len(raw) > 0 && string(raw) != "null" && string(raw) != "{}" {
			return string(raw)
		}
	}
	type digestSource interface {
		ExpectedDigests(project string) map[string]string
	}
	source, ok := r.Executor.(digestSource)
	if !ok {
		return ""
	}
	raw, err := json.Marshal(source.ExpectedDigests(app.ComposeProject()))
	if err != nil || len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return ""
	}
	return string(raw)
}

func (r *Reconciler) noteDeploy(app *App, result, gitSHA, varsJSON, images, trigger string) {
	if r == nil || app == nil {
		return
	}
	public := deploypolicy.EncodeVars(fleetcheck.PublicVars(deploypolicy.DecodeVars(varsJSON), app.Policy().PublicVars))
	_ = r.Registry.RecordDeploy(api.DeployEvent{
		AppID:    app.ID,
		App:      app.Name,
		Project:  app.ComposeProject(),
		GitSHA:   gitSHA,
		FleetSHA: metrics.FleetSHA(),
		VarsJSON: public,
		Images:   images,
		Result:   result,
		Trigger:  trigger,
	})
	metrics.DeploysTotal.WithLabelValues(app.Name, deployResult(result)).Inc()
}

func deployTrigger(app *App, target string) string {
	if app != nil && target != "" && target != strings.TrimSpace(app.LastSeenCommit) {
		return "git"
	}
	return "fleet"
}

func deployResult(result string) string {
	switch result {
	case "applied", "rolled_back", "recovered", "volume_created":
		return result
	default:
		return "failed"
	}
}

func redactAppText(app *App, text string) string {
	if app == nil || text == "" {
		return text
	}
	policy := app.Policy()
	listed := policy.PublicVars
	text = fleetcheck.RedactText(text, policy.Vars, listed)
	text = fleetcheck.RedactText(text, deploypolicy.DecodeVars(app.LastHealthyVars), listed)
	text = fleetcheck.RedactText(text, deploypolicy.DecodeVars(app.RejectedVars), listed)
	return text
}
