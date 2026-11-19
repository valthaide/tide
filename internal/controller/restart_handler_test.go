package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
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
		{"missing service", "/api/v1/apps/" + app.ID + "/restart", `{}`, http.StatusBadRequest},
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
