package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/tide/internal/api"
)

// rolloutTemplateHasher makes every app report a pending template drift.
type rolloutTemplateHasher struct {
	drift map[string]bool
}

func (h *rolloutTemplateHasher) TemplateHashForApp(appID, _, _ string) (string, error) {
	if h.drift[appID] {
		return "new-hash", nil
	}
	return "applied-hash", nil
}

type rolloutRuntime struct {
	*fakePlanRuntime
	hasher *rolloutTemplateHasher
}

func (e *rolloutRuntime) TemplateHashForApp(appID, template, appDir string) (string, error) {
	return e.hasher.TemplateHashForApp(appID, template, appDir)
}

func rolloutApp(t *testing.T, reg *Registry, name, policy string) *api.App {
	t.Helper()
	app := &api.App{
		Name:     name,
		RepoURL:  "https://example/" + name + ".git",
		Branch:   "main",
		Template: "backend",
	}
	addPublicApp(t, reg, app)
	// CreateApp does not persist the policy JSON, so store it explicitly.
	if err := reg.UpdateDeployPolicy(app.ID, policy); err != nil {
		t.Fatal(err)
	}
	fresh, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func setAppStatus(t *testing.T, reg *Registry, app *api.App, status string) {
	t.Helper()
	if err := reg.UpdateStatus(app.ID, status, nil); err != nil {
		t.Fatal(err)
	}
	app.Status = status
}

func rolloutReconciler(reg *Registry, drift map[string]bool) *Reconciler {
	exec := &rolloutRuntime{fakePlanRuntime: &fakePlanRuntime{}, hasher: &rolloutTemplateHasher{drift: drift}}
	return NewReconciler(reg, exec, nil, ReconcilerConfig{})
}

// markTemplateApplied baselines the template hash so only drifted apps report
// template_changed; drifted apps keep the stale baseline.
func markTemplateApplied(t *testing.T, r *Reconciler, app *api.App, applied bool) {
	t.Helper()
	hash := "applied-hash"
	if !applied {
		hash = "stale-hash"
	}
	if err := r.Registry.UpdateTemplateHash(app.ID, hash); err != nil {
		t.Fatal(err)
	}
	fresh, err := r.Registry.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	*app = *fresh
}

func TestRolloutGateDefaultDoesNotHold(t *testing.T) {
	reg := testRegistry(t)
	app := rolloutApp(t, reg, "plain", `{}`)
	r := rolloutReconciler(reg, map[string]bool{app.ID: true})
	markTemplateApplied(t, r, app, false)

	if reason := r.rolloutGate(app, true); reason != "" {
		t.Fatalf("gate=%q, want open without a rollout policy", reason)
	}
}

func TestRolloutGateManualHoldsUntilGranted(t *testing.T) {
	reg := testRegistry(t)
	app := rolloutApp(t, reg, "manual-app", `{"rollout":"manual"}`)
	r := rolloutReconciler(reg, map[string]bool{app.ID: true})
	markTemplateApplied(t, r, app, false)

	if reason := r.rolloutGate(app, true); !strings.Contains(reason, "manual rollout") {
		t.Fatalf("gate=%q, want a manual rollout hold", reason)
	}

	if err := r.ApplyTemplate(app); err != nil {
		t.Fatal(err)
	}
	// The grant lets exactly one sync through and is spent by it.
	if reason := r.rolloutGate(app, true); reason != "" {
		t.Fatalf("gate=%q, want open with the apply grant", reason)
	}
	if reason := r.rolloutGate(app, true); !strings.Contains(reason, "manual rollout") {
		t.Fatalf("gate=%q, want held again after the grant was spent", reason)
	}
}

func TestRolloutGateCanaryStaging(t *testing.T) {
	reg := testRegistry(t)
	canary := rolloutApp(t, reg, "canary", `{"rollout":"canary","rollout_canary":true}`)
	rest1 := rolloutApp(t, reg, "rest-1", `{"rollout":"canary"}`)
	rest2 := rolloutApp(t, reg, "rest-2", `{"rollout":"canary"}`)
	drift := map[string]bool{canary.ID: true, rest1.ID: true, rest2.ID: true}
	r := rolloutReconciler(reg, drift)
	markTemplateApplied(t, r, canary, false)
	markTemplateApplied(t, r, rest1, false)
	markTemplateApplied(t, r, rest2, false)

	// The canary itself is never gated.
	if reason := r.rolloutGate(canary, true); reason != "" {
		t.Fatalf("canary gate=%q, want open", reason)
	}
	// The rest wait while the canary has not applied the template.
	for _, other := range []*api.App{rest1, rest2} {
		if reason := r.rolloutGate(other, true); !strings.Contains(reason, "canary "+canary.Name) {
			t.Fatalf("%s gate=%q, want waiting on the canary", other.Name, reason)
		}
	}

	// Canary applied and healthy: the parked apps go one at a time.
	setAppStatus(t, reg, canary, "synced")
	markTemplateApplied(t, r, canary, true)
	delete(drift, canary.ID)

	// rest-2 is parked (held), so rest-1 may go first.
	setAppStatus(t, reg, rest2, "held")
	if reason := r.rolloutGate(rest1, true); reason != "" {
		t.Fatalf("rest-1 gate=%q, want open once the canary is healthy", reason)
	}
	// rest-1 starts its rollout: rest-2 must wait for it.
	setAppStatus(t, reg, rest1, "pending")
	if reason := r.rolloutGate(rest2, true); !strings.Contains(reason, rest1.Name) {
		t.Fatalf("rest-2 gate=%q, want waiting on rest-1", reason)
	}

	// rest-1 done: rest-2 proceeds.
	setAppStatus(t, reg, rest1, "synced")
	markTemplateApplied(t, r, rest1, true)
	delete(drift, rest1.ID)
	if reason := r.rolloutGate(rest2, true); reason != "" {
		t.Fatalf("rest-2 gate=%q, want open after rest-1", reason)
	}
}

func TestRolloutGateCanaryBrokenHoldsRest(t *testing.T) {
	reg := testRegistry(t)
	canary := rolloutApp(t, reg, "canary", `{"rollout":"canary","rollout_canary":true}`)
	rest := rolloutApp(t, reg, "rest", `{"rollout":"canary"}`)
	drift := map[string]bool{canary.ID: true, rest.ID: true}
	r := rolloutReconciler(reg, drift)
	markTemplateApplied(t, r, canary, false)
	markTemplateApplied(t, r, rest, false)

	// The canary applied the broken template and failed: no drift left, but
	// it is not healthy, so the rest stay held.
	delete(drift, canary.ID)
	markTemplateApplied(t, r, canary, true)
	setAppStatus(t, reg, canary, "failed")

	reason := r.rolloutGate(rest, true)
	if !strings.Contains(reason, "canary "+canary.Name) || !strings.Contains(reason, "not healthy") {
		t.Fatalf("rest gate=%q, want waiting on the unhealthy canary", reason)
	}
}

func TestRolloutGateSkipsComposePathApps(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name:             "no-template",
		RepoURL:          "https://example/x.git",
		Branch:           "main",
		ComposePath:      "compose.yaml",
		DeployPolicyJSON: `{"rollout":"manual"}`,
	}
	addPublicApp(t, reg, app)
	r := rolloutReconciler(reg, nil)

	if reason := r.rolloutGate(app, true); reason != "" {
		t.Fatalf("gate=%q, want open for compose_path apps", reason)
	}
}

func TestHoldAndReleaseRollout(t *testing.T) {
	reg := testRegistry(t)
	app := rolloutApp(t, reg, "manual-app", `{"rollout":"manual"}`)
	r := rolloutReconciler(reg, map[string]bool{app.ID: true})
	markTemplateApplied(t, r, app, false)

	app.Status = "pending"
	r.holdRollout(app, "manual rollout: template change waits for tidectl apply")

	fresh, err := r.Registry.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != "held" {
		t.Fatalf("status=%q, want held", fresh.Status)
	}
	if r.queued(app.ID) {
		t.Fatal("a held app must leave the queue")
	}

	// Still gated: the release check keeps it held.
	fresh.Status = "held"
	r.maybeReleaseRolloutHold(fresh)
	fresh, _ = r.Registry.Get(app.ID)
	if fresh.Status != "held" {
		t.Fatalf("status=%q, want still held", fresh.Status)
	}

	// Granted: the release check requeues the app by itself.
	if err := r.ApplyTemplate(app); err != nil {
		t.Fatal(err)
	}
	fresh, _ = r.Registry.Get(app.ID)
	fresh.Status = "held"
	r.maybeReleaseRolloutHold(fresh)
	fresh, _ = r.Registry.Get(app.ID)
	if fresh.Status != "pending" {
		t.Fatalf("status=%q, want pending after the grant", fresh.Status)
	}
	if !r.queued(app.ID) {
		t.Fatal("a released app must be queued")
	}
}

func TestApplyTemplateRejectsBadState(t *testing.T) {
	reg := testRegistry(t)
	app := rolloutApp(t, reg, "manual-app", `{"rollout":"manual"}`)
	r := rolloutReconciler(reg, nil)

	app.Suspended = true
	if err := r.ApplyTemplate(app); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("err=%v, want suspended", err)
	}
	app.Suspended = false
	r.mu.Lock()
	r.inFlight[app.ID] = struct{}{}
	r.mu.Unlock()
	if err := r.ApplyTemplate(app); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("err=%v, want in progress", err)
	}
}

func TestRolloutGateIgnoresNonTemplateDrift(t *testing.T) {
	reg := testRegistry(t)
	app := rolloutApp(t, reg, "manual-app", `{"rollout":"manual"}`)
	r := rolloutReconciler(reg, nil)
	// No template drift (baseline equals the current hash): even a manual
	// policy does not hold regular syncs.
	markTemplateApplied(t, r, app, true)

	if reason := r.rolloutGate(app, true); reason != "" {
		t.Fatalf("gate=%q, want open without template drift", reason)
	}
}

func TestApplyTemplateHandler(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", Template: "backend"}
	addPublicApp(t, reg, app)

	h := NewHandler(reg, nil, nil, nil, nil, "test")
	gate := &fakeApplyGate{apps: map[string]*api.App{app.ID: app}}
	h.SyncGate = gate

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/apply", nil)
	rr := httptest.NewRecorder()
	router := applyTestRouter(h)
	router.ServeHTTP(rr, req)

	if rr.Code != 202 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gate.got != app.ID {
		t.Fatalf("gate got %q, want %q", gate.got, app.ID)
	}
	audits, err := reg.ListAudit(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audits {
		if a.Kind == "template_apply" {
			found = true
		}
	}
	if !found {
		t.Fatal("template_apply audit event is missing")
	}
}

type fakeApplyGate struct {
	apps map[string]*api.App
	got  string
}

func (f *fakeApplyGate) AcquireSync(ctx context.Context) error { return nil }
func (f *fakeApplyGate) ReleaseSync()                          {}
func (f *fakeApplyGate) ApplyTemplate(app *api.App) error {
	f.got = app.ID
	return nil
}

func applyTestRouter(h *Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/api/v1/apps/{id}/apply", h.ApplyTemplateApp)
	return router
}
