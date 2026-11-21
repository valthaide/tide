package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/metrics"
)

// rolloutGate reports why a pending template change must wait, or "" when the
// app may apply it now. It only gates template rollouts (fleet template or
// override changes); commit, env, and watch syncs are never held.
// consumeGrant is true only inside syncApp, where a one-shot manual apply
// grant is spent by the sync that uses it.
func (r *Reconciler) rolloutGate(app *App, consumeGrant bool) string {
	if app == nil || strings.TrimSpace(app.Template) == "" {
		return ""
	}
	strategy := deploypolicy.NormalizeRollout(app.Policy().Rollout)
	if strategy == "" {
		return ""
	}
	if r.templateDriftReason(app) == "" {
		return ""
	}
	if r.forcing(app.ID) {
		if consumeGrant {
			r.consumeForceApply(app.ID)
		}
		return ""
	}
	switch strategy {
	case deploypolicy.RolloutManual:
		return "manual rollout: template change waits for tidectl apply"
	case deploypolicy.RolloutCanary:
		if app.Policy().RolloutCanary {
			return ""
		}
		return r.canaryHoldReason(app)
	}
	return ""
}

// canaryHoldReason stages a canary rollout: every canary applies first and
// must be healthy, then the remaining apps roll out one at a time.
func (r *Reconciler) canaryHoldReason(app *App) string {
	apps := r.Registry.List()
	for _, other := range apps {
		if other == nil || other.ID == app.ID || other.Suspended {
			continue
		}
		op := other.Policy()
		if deploypolicy.NormalizeRollout(op.Rollout) != deploypolicy.RolloutCanary || !op.RolloutCanary {
			continue
		}
		if r.templateDriftReason(other) != "" {
			return "canary " + other.Name + " has not applied the template yet"
		}
		switch other.Status {
		case "synced":
		case "syncing", "verifying", "pending":
			return "canary " + other.Name + " is applying the template"
		default:
			return "canary " + other.Name + " is not healthy (status " + other.Status + ")"
		}
	}
	for _, other := range apps {
		if other == nil || other.ID == app.ID || other.Suspended {
			continue
		}
		op := other.Policy()
		if deploypolicy.NormalizeRollout(op.Rollout) != deploypolicy.RolloutCanary || op.RolloutCanary {
			continue
		}
		if other.Status == "held" {
			continue
		}
		busy := r.inFlightApp(other.ID) || other.Status == "syncing" || other.Status == "verifying" || other.Status == "pending"
		if busy && r.templateDriftReason(other) != "" {
			return "waiting for " + other.Name + " to finish its template rollout"
		}
	}
	return ""
}

// holdRollout parks an app whose template change is gated: it leaves the
// queue and waits as held until maybeReleaseRolloutHold lets it through.
func (r *Reconciler) holdRollout(app *App, reason string) {
	r.dequeue(app.ID)
	if app.Status != "held" && r.Logger != nil {
		r.Logger.Info("Rollout holds template apply", "app", app.Name, "reason", reason)
	}
	_ = r.Registry.UpdateStatus(app.ID, "held", nil)
	_ = r.Registry.UpdateSyncProgress(app.ID, time.Now(), "rollout: "+reason)
	metrics.ObserveDeployBlocked(app.Name, "rollout_"+deploypolicy.NormalizeRollout(app.Policy().Rollout), "")
}

// maybeReleaseRolloutHold rechecks the gate of a held app; once it opens
// (canaries healthy, the previous app done, or a manual apply granted) the
// app goes back to the queue by itself.
func (r *Reconciler) maybeReleaseRolloutHold(app *App) {
	if app == nil || app.Status != "held" {
		return
	}
	if reason := r.rolloutGate(app, false); reason != "" {
		_ = r.Registry.UpdateSyncProgress(app.ID, time.Now(), "rollout: "+reason)
		return
	}
	metrics.ClearDeployBlocked(app.Name)
	if r.Logger != nil {
		r.Logger.Info("Rollout releases app", "app", app.Name)
	}
	r.requeuePending(app, "template_changed")
}

func (r *Reconciler) consumeForceApply(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.forceApply, id)
}

// ApplyTemplate grants one manual template apply and queues the app, the
// operator action behind POST /api/v1/apps/{id}/apply and `tidectl apply`.
func (r *Reconciler) ApplyTemplate(app *api.App) error {
	if r == nil || app == nil {
		return fmt.Errorf("app is nil")
	}
	if app.Suspended {
		return fmt.Errorf("app is suspended")
	}
	if r.inFlightApp(app.ID) {
		return fmt.Errorf("sync already in progress")
	}
	r.mu.Lock()
	r.forceApply[app.ID] = struct{}{}
	r.mu.Unlock()
	if r.Registry != nil {
		_ = r.Registry.ClearRetry(app.ID)
	}
	r.enqueue(app, "manual", "apply", appPriority(app))
	if r.Registry != nil {
		if err := r.Registry.UpdateStatus(app.ID, "pending", nil); err != nil {
			r.mu.Lock()
			delete(r.forceApply, app.ID)
			r.mu.Unlock()
			return err
		}
	}
	select {
	case r.nudge <- struct{}{}:
	default:
	}
	if r.Logger != nil {
		r.Logger.Info("Template apply granted", "app", app.Name)
	}
	return nil
}
