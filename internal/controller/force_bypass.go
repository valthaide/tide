package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/cigate"
)

const (
	defaultBypassTTL = 5 * time.Minute
	maxBypassTTL     = 15 * time.Minute
)

// BypassRequest authorizes a single, short-lived CI gate bypass for one app and exact commit SHA.
type BypassRequest struct {
	AppID  string
	SHA    string
	Reason string
	Actor  string
	TTL    time.Duration
}

type ciBypassGrant struct {
	AppID     string
	SHA       string
	Reason    string
	Actor     string
	ExpiresAt time.Time
}

// GrantBypass validates and registers a single-use, SHA-bound CI bypass and records an immutable audit event.
func (r *Reconciler) GrantBypass(req BypassRequest) error {
	if r == nil || r.Registry == nil {
		return fmt.Errorf("reconciler is not initialized")
	}
	appID := strings.TrimSpace(req.AppID)
	sha := strings.TrimSpace(req.SHA)
	reason := strings.TrimSpace(req.Reason)
	actor := strings.TrimSpace(req.Actor)
	if appID == "" {
		return fmt.Errorf("app_id is required")
	}
	if sha == "" {
		return fmt.Errorf("exact commit sha is required for --force bypass")
	}
	if len(reason) < 3 {
		return fmt.Errorf("non-empty --reason is required for --force bypass")
	}
	if actor == "" {
		return fmt.Errorf("authenticated actor is required for --force bypass")
	}

	app, err := r.Registry.Get(appID)
	if err != nil || app == nil {
		return fmt.Errorf("app not found")
	}
	if seen := strings.TrimSpace(app.LastSeenCommit); seen != "" && !strings.EqualFold(seen, sha) {
		_ = r.Registry.RecordAudit(api.AuditEvent{
			Kind:     "ci_force_bypass",
			Actor:    actor,
			AppID:    app.ID,
			AppName:  app.Name,
			RepoURL:  app.RepoURL,
			GitSHA:   sha,
			Reason:   reason,
			Decision: "bypass_rejected_sha_mismatch",
		})
		return fmt.Errorf("force bypass sha %s does not match current app commit %s", sha, seen)
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultBypassTTL
	}
	if ttl > maxBypassTTL {
		ttl = maxBypassTTL
	}
	expiresAt := time.Now().Add(ttl)

	if err := r.Registry.RecordAudit(api.AuditEvent{
		Kind:           "ci_force_bypass",
		Actor:          actor,
		AppID:          app.ID,
		AppName:        app.Name,
		RepoURL:        app.RepoURL,
		GitSHA:         sha,
		PipelineID:     app.CIPipelineID,
		PipelineSource: app.CISource,
		PipelineStatus: app.CIStatus,
		PipelineURL:    app.CIURL,
		Reason:         reason,
		Decision:       "bypass_granted",
	}); err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}

	r.mu.Lock()
	if r.bypasses == nil {
		r.bypasses = map[string]ciBypassGrant{}
	}
	r.bypasses[app.ID] = ciBypassGrant{
		AppID:     app.ID,
		SHA:       sha,
		Reason:    reason,
		Actor:     actor,
		ExpiresAt: expiresAt,
	}
	r.mu.Unlock()

	if r.Logger != nil {
		r.Logger.Warn("CI gate bypass granted",
			"app", app.Name,
			"app_id", app.ID,
			"actor", actor,
			"sha", sha,
			"reason", reason,
			"expires_at", expiresAt.UTC().Format(time.RFC3339),
		)
	}
	return nil
}

// consumeBypass checks and atomically consumes a single-use bypass grant for (app, commitSHA).
// If the commit SHA changed or the TTL expired, the grant is revoked and false is returned.
func (r *Reconciler) consumeBypass(app *App, commitSHA string, ciRes cigate.Result) (ciBypassGrant, bool) {
	if r == nil || app == nil {
		return ciBypassGrant{}, false
	}
	now := time.Now()
	r.mu.Lock()
	grant, ok := r.bypasses[app.ID]
	if ok {
		delete(r.bypasses, app.ID)
	}
	r.mu.Unlock()
	if !ok {
		return ciBypassGrant{}, false
	}

	if !strings.EqualFold(strings.TrimSpace(grant.SHA), strings.TrimSpace(commitSHA)) {
		if r.Registry != nil {
			_ = r.Registry.RecordAudit(api.AuditEvent{
				Kind:           "ci_force_bypass",
				Actor:          grant.Actor,
				AppID:          app.ID,
				AppName:        app.Name,
				RepoURL:        app.RepoURL,
				GitSHA:         commitSHA,
				PipelineID:     ciRes.PipelineID,
				PipelineSource: ciRes.Source,
				PipelineStatus: ciRes.Status,
				PipelineURL:    ciRes.WebURL,
				Reason:         grant.Reason,
				Decision:       "bypass_revoked_sha_changed",
			})
		}
		if r.Logger != nil {
			r.Logger.Warn("CI gate bypass revoked because commit SHA changed",
				"app", app.Name,
				"granted_sha", grant.SHA,
				"current_sha", commitSHA,
			)
		}
		return ciBypassGrant{}, false
	}

	if now.After(grant.ExpiresAt) {
		if r.Registry != nil {
			_ = r.Registry.RecordAudit(api.AuditEvent{
				Kind:           "ci_force_bypass",
				Actor:          grant.Actor,
				AppID:          app.ID,
				AppName:        app.Name,
				RepoURL:        app.RepoURL,
				GitSHA:         commitSHA,
				PipelineID:     ciRes.PipelineID,
				PipelineSource: ciRes.Source,
				PipelineStatus: ciRes.Status,
				PipelineURL:    ciRes.WebURL,
				Reason:         grant.Reason,
				Decision:       "bypass_expired",
			})
		}
		return ciBypassGrant{}, false
	}

	if r.Registry != nil {
		_ = r.Registry.RecordAudit(api.AuditEvent{
			Kind:           "ci_force_bypass",
			Actor:          grant.Actor,
			AppID:          app.ID,
			AppName:        app.Name,
			RepoURL:        app.RepoURL,
			GitSHA:         commitSHA,
			PipelineID:     ciRes.PipelineID,
			PipelineSource: ciRes.Source,
			PipelineStatus: ciRes.Status,
			PipelineURL:    ciRes.WebURL,
			Reason:         grant.Reason,
			Decision:       "bypass_consumed",
		})
	}
	return grant, true
}
