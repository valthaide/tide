package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
)

type fakeRestarter struct {
	calls []string
	err   error
}

func (f *fakeRestarter) RestartService(_ context.Context, appID, composePath, service string) (string, error) {
	f.calls = append(f.calls, appID+"|"+composePath+"|"+service)
	if f.err != nil {
		return "", f.err
	}
	return "restarted", nil
}

type fakeInspector struct {
	containers []compose.ServiceContainer
	err        error
}

func (f *fakeInspector) InspectProjectContainers(_ context.Context, _ string) ([]compose.ServiceContainer, error) {
	return f.containers, f.err
}

func restartTestRouter(h *Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/api/v1/apps/{id}/restart", h.RestartServiceApp)
	return router
}

func TestRestartServiceAppOK(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	restarter := &fakeRestarter{}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = restarter
	h.Inspector = &fakeInspector{containers: []compose.ServiceContainer{{Service: "web"}, {Service: "php-consume"}}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart", strings.NewReader(`{"service":"php-consume"}`))
	rr := httptest.NewRecorder()
	restartTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if len(restarter.calls) != 1 || restarter.calls[0] != app.ID+"|compose.yaml|php-consume" {
		t.Fatalf("restarter calls %+v", restarter.calls)
	}
}

func TestRestartServiceAppQueryParam(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	restarter := &fakeRestarter{}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = restarter
	h.Inspector = &fakeInspector{containers: []compose.ServiceContainer{{Service: "web"}}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart?service=web", nil)
	rr := httptest.NewRecorder()
	restartTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if len(restarter.calls) != 1 {
		t.Fatalf("restarter calls %+v", restarter.calls)
	}
}

func TestRestartServiceAppValidation(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	restarter := &fakeRestarter{}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = restarter
	h.Inspector = &fakeInspector{containers: []compose.ServiceContainer{{Service: "web"}}}
	router := restartTestRouter(h)

	cases := []struct {
		name   string
		url    string
		body   string
		status int
	}{
		{"unknown app", "/api/v1/apps/nope/restart", `{"service":"web"}`, http.StatusNotFound},
		{"whole app without actions", "/api/v1/apps/" + app.ID + "/restart", `{}`, http.StatusServiceUnavailable},
		{"invalid service", "/api/v1/apps/" + app.ID + "/restart", `{"service":"web; rm -rf /"}`, http.StatusBadRequest},
		{"unknown service", "/api/v1/apps/" + app.ID + "/restart", `{"service":"redis"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status %d want %d body %s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
	if len(restarter.calls) != 0 {
		t.Fatalf("restarter must not be called: %+v", restarter.calls)
	}
}

func TestRestartServiceAppNoContainers(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = &fakeRestarter{}
	h.Inspector = &fakeInspector{}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart", strings.NewReader(`{"service":"web"}`))
	rr := httptest.NewRecorder()
	restartTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestRestartServiceAppFailureAudited(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = &fakeRestarter{err: fmt.Errorf("docker daemon gone")}
	h.Inspector = &fakeInspector{containers: []compose.ServiceContainer{{Service: "web"}}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart", strings.NewReader(`{"service":"web"}`))
	rr := httptest.NewRecorder()
	restartTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	events, err := reg.ListAudit(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "service_restart" || events[0].Decision != "restart_failed" {
		t.Fatalf("audit %+v", events)
	}
}

type optRestartRuntime struct {
	fakeRuntime
	opts    []compose.ApplyOptions
	actions []string
}

func (o *optRestartRuntime) ComposeAction(_ context.Context, _, _, action string, opts compose.ApplyOptions, services ...string) (string, error) {
	o.opts = append(o.opts, opts)
	o.actions = append(o.actions, strings.TrimSpace(action+" "+strings.Join(services, " ")))
	return "ok", nil
}

func TestRestartServiceAppUsesDeployOptions(t *testing.T) {
	reg := testRegistry(t)
	envFile := filepath.Join(t.TempDir(), "admin.env")
	if err := os.WriteFile(envFile, []byte("ADMIN_ENV_FILE=/srv/admin.env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app := &api.App{Name: "test-ru", RepoURL: "https://example/repo.git", Branch: "main", Template: "tenant"}
	addPublicApp(t, reg, app)
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{
		EnvFiles: []string{envFile},
		Project:  "tenko-test-ru",
		Vars:     map[string]string{"APP_TAG": "1"},
	}))

	rt := &optRestartRuntime{}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = testReconciler(t, reg, rt, ReconcilerConfig{})
	h.Inspector = &fakeInspector{containers: []compose.ServiceContainer{{Service: "php-consume"}}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart", strings.NewReader(`{"service":"php-consume"}`))
	rr := httptest.NewRecorder()
	restartTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if len(rt.opts) != 1 {
		t.Fatalf("restart calls %+v", rt.opts)
	}
	got := rt.opts[0]
	fresh, _ := reg.Get(app.ID)
	if got.Template != "tenant" || got.Project != fresh.ComposeProject() {
		t.Fatalf("opts %+v", got)
	}
	if got.Vars["ADMIN_ENV_FILE"] != "/srv/admin.env" || got.Vars["APP_TAG"] != "1" || got.Vars["TIDE_PROJECT"] != fresh.ComposeProject() {
		t.Fatalf("vars %+v", got.Vars)
	}
}

func TestRestartWholeAppAndStopStart(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "test-en", RepoURL: "https://example/repo.git", Branch: "main", Template: "tenant"}
	addPublicApp(t, reg, app)

	rt := &optRestartRuntime{}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{})
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Restarter = rec
	h.Actions = rec
	h.Inspector = &fakeInspector{containers: []compose.ServiceContainer{{Service: "web"}, {Service: "php-consume"}}}
	router := restartTestRouter(h)
	router.Post("/api/v1/apps/{id}/stop", h.StopApp)
	router.Post("/api/v1/apps/{id}/start", h.StartApp)

	for _, step := range []struct {
		url       string
		body      string
		suspended bool
	}{
		{"/api/v1/apps/" + app.ID + "/restart", `{}`, false},
		{"/api/v1/apps/" + app.ID + "/stop", "", true},
		{"/api/v1/apps/" + app.ID + "/start", "", false},
	} {
		req := httptest.NewRequest(http.MethodPost, step.url, strings.NewReader(step.body))
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status %d body %s", step.url, rr.Code, rr.Body.String())
		}
		fresh, _ := reg.Get(app.ID)
		if fresh.Suspended != step.suspended {
			t.Fatalf("%s suspended %v want %v", step.url, fresh.Suspended, step.suspended)
		}
		if (fresh.Status == "stopped") != step.suspended {
			t.Fatalf("%s status %q", step.url, fresh.Status)
		}
	}
	if strings.Join(rt.actions, ",") != "restart,stop,start" {
		t.Fatalf("actions %v", rt.actions)
	}
	for _, opts := range rt.opts {
		if opts.Template != "tenant" || opts.Vars["TIDE_APP_NAME"] != "test-en" {
			t.Fatalf("opts %+v", opts)
		}
	}
	events, err := reg.ListAudit(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, e := range events {
		kinds[e.Kind] = e.Decision
	}
	if kinds["service_restart"] != "restarted" || kinds["app_stop"] != "stopped" || kinds["app_start"] != "started" {
		t.Fatalf("audit %+v", events)
	}
}

func TestRestartWholeAppHoldsRuntimeDrift(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "test-en", RepoURL: "https://example/repo.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "aaaaaaaa", Status: "synced",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateSyncResult(app.ID, "synced", time.Now(), "aaaaaaaa", "ok", "", ""); err != nil {
		t.Fatal(err)
	}
	fresh, _ := reg.Get(app.ID)
	rt := &optRestartRuntime{}
	rt.snapshot = map[string]compose.ProjectRuntimeState{
		fresh.ComposeProject(): {ContainerCount: 2, RunningCount: 1, FlappingCount: 1},
	}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{})
	if _, err := rec.AppAction(context.Background(), app.ID, "restart"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rec.reconcileOnce()
	}
	if len(rt.applyCalls) != 0 || rec.queued(app.ID) {
		t.Fatalf("sync after restart applies=%v", rt.applyCalls)
	}
	if reason, _ := rec.cheapDrift(fresh, rt.snapshot); reason != "" {
		t.Fatalf("drift after restart %q", reason)
	}
	got, _ := reg.Get(app.ID)
	if got.Status != "synced" {
		t.Fatalf("status %q", got.Status)
	}
	if _, err := rec.AppAction(context.Background(), app.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if reason, _ := rec.cheapDrift(fresh, rt.snapshot); reason != "runtime_restarting" {
		t.Fatalf("drift after stop %q", reason)
	}
}
