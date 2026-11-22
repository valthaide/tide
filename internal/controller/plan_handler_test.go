package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
)

type fakePlanner struct {
	plan        *compose.AppPlan
	err         error
	gotFleetRef string
	gotApp      *api.App
}

func (f *fakePlanner) PlanApp(_ context.Context, app *api.App, fleetRef string) (*compose.AppPlan, error) {
	f.gotApp = app
	f.gotFleetRef = fleetRef
	if f.err != nil {
		return nil, f.err
	}
	return f.plan, nil
}

func planTestRouter(h *Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/api/v1/apps/{id}/plan", h.PlanApp)
	return router
}

func TestPlanAppOK(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	planner := &fakePlanner{plan: &compose.AppPlan{
		Project: "tide-backend",
		Services: []compose.ServicePlan{
			{Service: "web", Action: compose.PlanActionRecreate, Reasons: []string{"image digest changed (aaa -> bbb)"}},
			{Service: "nginx", Action: compose.PlanActionNoop},
		},
	}}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Planner = planner

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/plan", nil)
	rr := httptest.NewRecorder()
	planTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"recreate"`) || !strings.Contains(body, "image digest changed") {
		t.Fatalf("body=%s", body)
	}
	if planner.gotApp == nil || planner.gotApp.ID != app.ID {
		t.Fatalf("planner got app %+v", planner.gotApp)
	}
}

func TestPlanAppFleetRefPassthrough(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	planner := &fakePlanner{plan: &compose.AppPlan{Project: "tide-backend"}}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Planner = planner

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.ID+"/plan?fleet_ref=abc123", nil)
	rr := httptest.NewRecorder()
	planTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if planner.gotFleetRef != "abc123" {
		t.Fatalf("fleet_ref=%q, want abc123", planner.gotFleetRef)
	}
}

func TestPlanAppErrors(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	cases := []struct {
		name    string
		planner AppPlanner
		path    string
		want    int
	}{
		{"not configured", nil, "/api/v1/apps/" + app.ID + "/plan", http.StatusServiceUnavailable},
		{"unknown app", &fakePlanner{plan: &compose.AppPlan{}}, "/api/v1/apps/nope/plan", http.StatusNotFound},
		{"planner failure", &fakePlanner{err: errors.New("boom")}, "/api/v1/apps/" + app.ID + "/plan", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(reg, nil, nil, nil, nil, "test")
			h.Planner = tc.planner
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			planTestRouter(h).ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status=%d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestPlanAppByName(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "backend", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	planner := &fakePlanner{plan: &compose.AppPlan{Project: "tide-backend"}}
	h := NewHandler(reg, nil, nil, nil, nil, "test")
	h.Planner = planner

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+app.Name+"/plan", nil)
	rr := httptest.NewRecorder()
	planTestRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 for plan by app name", rr.Code, rr.Body.String())
	}
	if planner.gotApp == nil || planner.gotApp.ID != app.ID {
		t.Fatalf("planner got app %+v", planner.gotApp)
	}
}
