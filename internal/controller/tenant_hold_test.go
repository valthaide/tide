package controller

import (
	"context"
	"testing"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/syncerr"
	"github.com/valthaide/tide/internal/tenant"
)

// predprod-1: tenant in hold, frontends lack image, pin appears, hold clears without manual release.
func TestPredprodHoldClearsWhenPendingFrontendsSync(t *testing.T) {
	reg := testRegistry(t)
	backend := &api.App{
		Name: "backend", RepoURL: "https://git.example/club/backend.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", LastSyncedCommit: "bbbbbbbb", Status: "synced",
	}
	front := &api.App{
		Name: "frontend", RepoURL: "https://git.example/club/frontend.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "ffffffff", Status: "pending",
	}
	addPublicApp(t, reg, backend)
	addPublicApp(t, reg, front)
	_ = reg.UpdateDeployPolicy(backend.ID, deploypolicy.Encode(deploypolicy.Policy{
		Tenant: "predprod-1", TenantFailurePolicy: "hold",
	}))
	_ = reg.UpdateDeployPolicy(front.ID, deploypolicy.Encode(deploypolicy.Policy{
		Tenant: "predprod-1", TenantFailurePolicy: "hold", Vars: map[string]string{"APP_TAG": "1"},
	}))

	rt := &missRuntime{containers: 0}
	r := testReconciler(t, reg, rt, ReconcilerConfig{Interval: time.Hour, SyncTimeout: time.Minute, MaxConcurrent: 2})
	r.Tenants = tenant.NewManager(t.TempDir())
	r.Tenants.UpdateSpecs(map[string]tenant.Spec{
		"predprod-1": {Name: "predprod-1", FailurePolicy: tenant.PolicyHold},
	})

	r.reconcileOnce() // apply fails → pending+image_not_found
	r.reconcileOnce() // evaluate tenants sees the failure and arms auto hold
	gotFront, _ := reg.Get(front.ID)
	if gotFront.Status != "pending" || gotFront.LastSyncErrorCode != syncerr.CodeImageNotFound {
		t.Fatalf("frontend: status=%s code=%s", gotFront.Status, gotFront.LastSyncErrorCode)
	}
	st, ok := r.Tenants.Hold("predprod-1")
	if !ok || st.Reason != tenant.ReasonAuto {
		t.Fatalf("expected auto hold, got %+v ok=%v", st, ok)
	}
	gotBackend, _ := reg.Get(backend.ID)
	if blocked, _ := r.Tenants.UpdatesBlocked(appView(gotBackend)); !blocked {
		t.Fatalf("healthy backend updates must be blocked while held (tenant=%q status=%s)", gotBackend.Policy().Tenant, gotBackend.Status)
	}
	if blocked, _ := r.Tenants.UpdatesBlocked(appView(gotFront)); blocked {
		t.Fatal("pending frontend must keep reconciling under hold")
	}

	// Pin appears: vars change breaks image-miss hold and apply succeeds.
	rt2 := &fakeRuntime{}
	r.Executor = rt2
	_ = reg.UpdateDeployPolicy(front.ID, deploypolicy.Encode(deploypolicy.Policy{
		Tenant: "predprod-1", TenantFailurePolicy: "hold", Vars: map[string]string{"APP_TAG": "2"},
	}))
	_ = reg.MarkPending(front.ID)
	r.rememberNext(front.ID, time.Now().Add(-time.Second), time.Now())
	r.reconcileOnce()
	r.reconcileOnce()

	gotFront, _ = reg.Get(front.ID)
	if gotFront.Status != "synced" {
		t.Fatalf("frontend should sync after pin appears, status=%s err=%s", gotFront.Status, gotFront.LastSyncError)
	}
	if _, ok := r.Tenants.Hold("predprod-1"); ok {
		t.Fatal("auto hold must clear without tidectl release")
	}
}

func TestSourceSwitchWithoutContainersAdoptsPending(t *testing.T) {
	reg := testRegistry(t)
	const oldRepo = "https://git.example/app.git"
	const newSHA = "aaaabbbbccccddddeeeeffff0000111122223333"
	app := &api.App{
		Name: "fresh", RepoURL: oldRepo, Branch: "main",
		ComposePath: "docker-compose.yml", PollInterval: "30s", Status: "pending",
	}
	addPublicApp(t, reg, app)
	rt := &fakeMigRuntime{
		noContainers: true,
		prepErr:      syncerr.New(syncerr.CodeImageNotFound, syncerr.StagePull, "image_not_found: registry.example/app:missing"),
	}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 1})
	rec.ResolveMigrationCommit = func(string, string, []byte, string, string) (string, error) {
		return newSHA, nil
	}
	plan, err := rec.MigrateRepo(context.Background(), app, api.RepoMigrationRequest{
		RepoURL: oldRepo, Template: "web", AppDir: "backend", FromFleet: true,
		Actor: "fleet", Reason: "fleet template updated",
	}, deploypolicy.Encode(deploypolicy.Policy{Tenant: "dev"}))
	if err != nil {
		t.Fatalf("adopt pending must not fail: %v", err)
	}
	if !plan.Applied || plan.Reason != "image_not_found_pending" {
		t.Fatalf("plan=%+v", plan)
	}
	got, _ := reg.Get(app.ID)
	if got.Template != "web" || got.AppDir != "backend" || got.ComposePath != "" {
		t.Fatalf("desired source not adopted: template=%q app_dir=%q compose=%q", got.Template, got.AppDir, got.ComposePath)
	}
	if got.Status != "pending" || got.LastSyncErrorCode != syncerr.CodeImageNotFound {
		t.Fatalf("status=%s code=%s", got.Status, got.LastSyncErrorCode)
	}
}

func TestSourceSwitchWithContainersKeepsOldSourceOnMissingImage(t *testing.T) {
	reg := testRegistry(t)
	const oldRepo = "https://git.example/app.git"
	app := &api.App{
		Name: "live", RepoURL: oldRepo, Branch: "main",
		ComposePath: "docker-compose.yml", PollInterval: "30s",
		LastSeenCommit: "aaaaaaaa", LastSyncedCommit: "aaaaaaaa", Status: "synced",
	}
	addPublicApp(t, reg, app)
	rt := &fakeMigRuntime{
		prepErr: syncerr.New(syncerr.CodeImageNotFound, syncerr.StagePull, "image_not_found: registry.example/app:missing"),
	}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 1})
	rec.ResolveMigrationCommit = func(string, string, []byte, string, string) (string, error) {
		return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
	}
	if _, err := rec.MigrateRepo(context.Background(), app, api.RepoMigrationRequest{
		RepoURL: oldRepo, Template: "web", FromFleet: true,
		Actor: "fleet", Reason: "fleet template updated",
	}, ""); err == nil {
		t.Fatal("expected migration to fail while containers exist")
	}
	got, _ := reg.Get(app.ID)
	if got.Template != "" || got.ComposePath != "docker-compose.yml" {
		t.Fatalf("old source must stay: %+v", got)
	}
	if got.Status != "error" || got.LastSyncErrorCode != syncerr.CodeImageNotFound {
		t.Fatalf("status=%s code=%s", got.Status, got.LastSyncErrorCode)
	}
}

func TestManualHoldSurvivesHealthyCycle(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "web", RepoURL: "https://git.example/web.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "aaaaaaaa", LastSyncedCommit: "aaaaaaaa", Status: "synced",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{Tenant: "lab"}))
	r := testReconciler(t, reg, &fakeRuntime{}, ReconcilerConfig{Interval: time.Hour, MaxConcurrent: 1})
	r.Tenants = tenant.NewManager(t.TempDir())
	r.Tenants.UpdateSpecs(map[string]tenant.Spec{"lab": {Name: "lab", FailurePolicy: tenant.PolicyHold}})
	r.Tenants.ManualHold("lab", "experiment")
	r.reconcileOnce()
	st, ok := r.Tenants.Hold("lab")
	if !ok || !st.Manual || st.Reason != tenant.ReasonManual {
		t.Fatalf("manual hold must not auto-clear: %+v ok=%v", st, ok)
	}
}

var _ = compose.ProjectRuntimeState{}
