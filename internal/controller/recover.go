package controller

import (
	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/deploypolicy"
	"github.com/valthaide/conops-private/internal/syncerr"
)

func (r *Reconciler) forcing(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.forceApply[id]
	return ok
}

func (r *Reconciler) noteDrift(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.driftStreak[id]++
	return r.driftStreak[id]
}

func (r *Reconciler) clearDrift(id string) {
	r.mu.Lock()
	delete(r.driftStreak, id)
	r.mu.Unlock()
}

func (r *Reconciler) maybeRecover(app *App, snapshot map[string]compose.ProjectRuntimeState) bool {
	if app == nil || snapshot == nil || !healthClass(app) {
		return false
	}
	if app.Status != "error" && app.Status != "failed" {
		return false
	}
	state, ok := snapshot[app.ComposeProject()]
	if !ok || !state.Ready() || !appliedMatchesDesired(app) {
		r.mu.Lock()
		delete(r.recoverStreak, app.ID)
		r.mu.Unlock()
		return false
	}
	r.mu.Lock()
	r.recoverStreak[app.ID]++
	n := r.recoverStreak[app.ID]
	r.mu.Unlock()
	if n < 2 {
		return false
	}
	now := time.Now()
	desired := deploypolicy.EncodeVars(app.Policy().Vars)
	_, _ = r.Registry.FinishSync(app.ID, mustGen(r, app.ID), "synced", now, app.LastSyncedCommit, app.LastSyncedCommitMessage, app.LastSyncOutput, "")
	_ = r.Registry.UpdateRollback(app.ID, app.LastSyncedCommit, "", desired, "")
	_ = r.Registry.ClearRetry(app.ID)
	r.mu.Lock()
	delete(r.recoverStreak, app.ID)
	r.mu.Unlock()
	app.Status = "synced"
	app.LastSyncError = ""
	app.LastHealthyCommit = app.LastSyncedCommit
	if r.Logger != nil {
		r.Logger.Info("Recovered: containers healthy, status error -> synced", "app", app.Name)
	}
	r.noteDeploy(app, "recovered", app.LastSyncedCommit, desired, "", "reconcile")
	return true
}

func mustGen(r *Reconciler, id string) int64 {
	gen, err := r.Registry.DesiredGeneration(id)
	if err != nil {
		return 0
	}
	return gen
}

func appliedMatchesDesired(app *App) bool {
	seen := strings.TrimSpace(app.LastSeenCommit)
	synced := strings.TrimSpace(app.LastSyncedCommit)
	if seen == "" || seen != synced {
		return false
	}
	desired := app.Policy().Vars
	if strings.TrimSpace(app.RejectedCommit) == seen && strings.TrimSpace(app.RejectedVars) != "" {
		return deploypolicy.VarsEqual(desired, deploypolicy.DecodeVars(app.RejectedVars))
	}
	return deploypolicy.VarsEqual(desired, deploypolicy.DecodeVars(app.LastHealthyVars))
}

func healthClass(app *App) bool {
	if app == nil {
		return false
	}
	switch app.LastSyncErrorCode {
	case syncerr.CodeValidation, syncerr.CodePortConflict, syncerr.CodeVolumeMissing, syncerr.CodeVolumeNameRejected, syncerr.CodeDigestRequired, syncerr.CodeRegistryDenied, syncerr.CodeBuildForbidden, syncerr.CodeUnsetVar, syncerr.CodeDockerAPI:
		return false
	}
	msg := strings.ToLower(app.LastSyncError + " " + app.LastSyncErrorCode)
	return strings.Contains(msg, "unhealthy") || strings.Contains(msg, "restarting") || strings.Contains(msg, "health timeout") || app.LastSyncErrorCode == syncerr.CodeUnhealthy
}

func retryMode(app *App) string {
	if app == nil {
		return "error_only"
	}
	switch strings.ToLower(strings.TrimSpace(app.Policy().RetryRejected)) {
	case "always", "never", "error_only":
		return strings.ToLower(strings.TrimSpace(app.Policy().RetryRejected))
	default:
		return "error_only"
	}
}

func (r *Reconciler) shouldRetryRejected(app *App, snapshot map[string]compose.ProjectRuntimeState) bool {
	mode := retryMode(app)
	if app == nil || mode == "never" || !healthClass(app) {
		return false
	}
	if app.Status == "rolled_back" && mode != "always" {
		return false
	}
	if strings.TrimSpace(app.LastHealthyCommit) != "" && snapshot != nil {
		if state, ok := snapshot[app.ComposeProject()]; ok && state.Ready() {
			if mode != "always" || app.LastSeenCommit == app.LastSyncedCommit {
				return false
			}
		}
	}
	_, next, err := r.Registry.RetryState(app.ID)
	if err != nil {
		return false
	}
	if !next.IsZero() && time.Now().Before(next) {
		return false
	}
	if next.IsZero() {
		return false
	}
	return true
}

func (r *Reconciler) scheduleHealthRetry(app *App) {
	if app == nil {
		return
	}
	attempt, _, _ := r.Registry.RetryState(app.ID)
	attempt++
	wait := reconcileBackoff(attempt)
	next := time.Now().Add(wait)
	_ = r.Registry.SetRetry(app.ID, attempt, next)
	if r.Logger != nil {
		r.Logger.Info("Retry scheduled", "app", app.Name, "attempt", attempt, "next", wait.Round(time.Second).String())
	}
}

func reconcileBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := time.Minute
	for i := 1; i < attempt; i++ {
		wait *= 2
		if wait >= 30*time.Minute {
			wait = 30 * time.Minute
			break
		}
	}
	span := wait / 10
	if span > 0 {
		delta := time.Duration(rand.Int63n(int64(span)*2+1)) - span
		wait += delta
	}
	if wait < 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

// SyncAppNow runs the same sync path as the dispatcher, including health and rollback.
// force applies the seen commit even when it was previously rejected.
func (r *Reconciler) SyncAppNow(app *api.App) *syncerr.Error {
	if r == nil || app == nil {
		return syncerr.New(syncerr.CodeUnknown, syncerr.StageUnknown, "app is nil")
	}
	if app.Status == "syncing" || r.inFlightApp(app.ID) {
		return syncerr.New("sync_in_progress", syncerr.StageUnknown, "sync already in progress")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.Config.SyncTimeout)
	defer cancel()
	r.mu.Lock()
	r.manualWaiting++
	r.mu.Unlock()
	errAcquire := r.AcquireSync(ctx)
	r.mu.Lock()
	r.manualWaiting--
	r.mu.Unlock()
	if errAcquire != nil {
		return syncerr.New(syncerr.CodeTimeout, syncerr.StageUnknown, "sync queue timeout")
	}
	r.enqueue(app, "manual", "manual", appPriority(app))
	r.mu.Lock()
	r.inFlight[app.ID] = struct{}{}
	r.forceApply[app.ID] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.dequeue(app.ID)
		r.mu.Lock()
		delete(r.inFlight, app.ID)
		delete(r.forceApply, app.ID)
		r.mu.Unlock()
		r.ReleaseSync()
	}()
	r.markQueueStart(app.ID)
	_ = r.Registry.ClearRetry(app.ID)
	err := r.syncApp(app)
	if err == nil {
		return nil
	}
	if se, ok := err.(*syncerr.Error); ok {
		return se
	}
	return syncerr.Classify(err)
}
