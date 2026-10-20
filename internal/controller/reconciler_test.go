package controller

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/credentials"
	"github.com/valthaide/conops-private/internal/store"
)

type fakeRuntime struct {
	snapshot   map[string]compose.ProjectRuntimeState
	snapshotErr error
	applyCalls []string
	applyErr   error
}

func (f *fakeRuntime) Apply(
	_ context.Context,
	appID, _ string,
	_ map[string]string,
	_, _, _, _ string,
	_ string,
	_ []byte,
	_ func(string),
) (string, error) {
	f.applyCalls = append(f.applyCalls, appID)
	if f.applyErr != nil {
		return "failed", f.applyErr
	}
	return "ok", nil
}

func (f *fakeRuntime) SnapshotProjects(_ context.Context) (map[string]compose.ProjectRuntimeState, error) {
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	return f.snapshot, nil
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	creds, err := credentials.NewServiceFromEnv(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	return NewRegistry(db, creds)
}

func testReconciler(t *testing.T, reg *Registry, rt *fakeRuntime, cfg ReconcilerConfig) *Reconciler {
	t.Helper()
	if cfg.Interval == 0 {
		cfg.Interval = time.Hour
	}
	if cfg.SyncTimeout == 0 {
		cfg.SyncTimeout = time.Minute
	}
	return NewReconciler(reg, rt, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
}

func addPublicApp(t *testing.T, reg *Registry, app *api.App) {
	t.Helper()
	app.RepoAuthMethod = "public"
	if err := reg.AddWithDeployKeyAndEnvs(app, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDriftReason(t *testing.T) {
	r := &Reconciler{}
	appID := "app-1"
	project := compose.ProjectNameForApp(appID)

	if got := r.runtimeDriftReason(appID, map[string]compose.ProjectRuntimeState{}); got != "runtime_missing" {
		t.Fatalf("empty snapshot: got %q", got)
	}
	if got := r.runtimeDriftReason(appID, map[string]compose.ProjectRuntimeState{
		project: {ContainerCount: 2, RunningCount: 2, UnhealthyCount: 1},
	}); got != "runtime_unhealthy" {
		t.Fatalf("unhealthy: got %q", got)
	}
	if got := r.runtimeDriftReason(appID, map[string]compose.ProjectRuntimeState{
		project: {ContainerCount: 2, RunningCount: 1, ExitedCount: 1},
	}); got != "runtime_exited" {
		t.Fatalf("exited: got %q", got)
	}
	if got := r.runtimeDriftReason(appID, map[string]compose.ProjectRuntimeState{
		project: {ContainerCount: 2, RunningCount: 1},
	}); got != "runtime_not_running" {
		t.Fatalf("not running: got %q", got)
	}
	if got := r.runtimeDriftReason(appID, map[string]compose.ProjectRuntimeState{
		project: {ContainerCount: 2, RunningCount: 2},
	}); got != "" {
		t.Fatalf("healthy: got %q", got)
	}
}

func TestSyncLooksStale(t *testing.T) {
	r := &Reconciler{Config: ReconcilerConfig{SyncTimeout: time.Minute}}
	if !r.syncLooksStale(nil) {
		t.Fatal("nil should be stale")
	}
	if !r.syncLooksStale(&App{}) {
		t.Fatal("zero LastSyncAt should be stale")
	}
	fresh := &App{LastSyncAt: time.Now()}
	if r.syncLooksStale(fresh) {
		t.Fatal("fresh sync should not be stale")
	}
	old := &App{LastSyncAt: time.Now().Add(-10 * time.Minute)}
	if !r.syncLooksStale(old) {
		t.Fatal("old sync should be stale")
	}
}

func TestReconcileSkipsSuspended(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "s1", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "pending", Suspended: true,
	}
	addPublicApp(t, reg, app)
	_ = reg.SetSuspended(app.ID, true)

	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("expected no apply, got %v", rt.applyCalls)
	}
}

func TestReconcileSyncsPending(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "p1", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "pending",
	}
	addPublicApp(t, reg, app)

	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCalls) != 1 || rt.applyCalls[0] != app.ID {
		t.Fatalf("expected apply for %s, got %v", app.ID, rt.applyCalls)
	}
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "synced" {
		t.Fatalf("status=%q want synced", got.Status)
	}
}

func TestReconcileSkipsErrorUnlessRetry(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "e1", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "error",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateStatus(app.ID, "error", nil)

	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{RetryErrors: false}).reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("without retry: got apply %v", rt.applyCalls)
	}

	testReconciler(t, reg, rt, ReconcilerConfig{RetryErrors: true}).reconcileOnce()
	if len(rt.applyCalls) != 1 {
		t.Fatalf("with retry: got apply %v", rt.applyCalls)
	}
}

func TestReconcileRecoversStaleSyncing(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "stale", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "pending",
	}
	addPublicApp(t, reg, app)
	old := time.Now().Add(-time.Hour)
	_ = reg.UpdateStatus(app.ID, "syncing", &old)

	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{SyncTimeout: time.Minute}).reconcileOnce()
	if len(rt.applyCalls) != 1 {
		t.Fatalf("expected recover+sync, got %v", rt.applyCalls)
	}
}

func TestReconcileSkipsActiveSyncing(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "active", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "pending",
	}
	addPublicApp(t, reg, app)
	now := time.Now()
	_ = reg.UpdateStatus(app.ID, "syncing", &now)

	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{SyncTimeout: 5 * time.Minute}).reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("active syncing should be skipped, got %v", rt.applyCalls)
	}
}

func TestReconcileRequeuesRuntimeDriftAndSyncs(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "drift", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", LastSyncedCommit: "abc", Status: "pending",
	}
	addPublicApp(t, reg, app)
	now := time.Now()
	_ = reg.UpdateSyncResult(app.ID, "synced", now, "abc", "msg", "ok", "")

	rt := &fakeRuntime{
		snapshot: map[string]compose.ProjectRuntimeState{}, // missing → drift
	}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCalls) != 1 {
		t.Fatalf("drift should requeue and sync, got %v", rt.applyCalls)
	}
}

func TestReconcileSkipsWithoutSeenCommit(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "noseen", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		Status: "pending",
	}
	addPublicApp(t, reg, app)

	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("no commit → no sync, got %v", rt.applyCalls)
	}
}

func TestLoadReconcilerConfigFromEnv(t *testing.T) {
	t.Setenv("CONOPS_RECONCILE_INTERVAL", "15s")
	t.Setenv("CONOPS_SYNC_TIMEOUT", "2m")
	t.Setenv("CONOPS_RETRY_ERRORS", "true")
	cfg, err := LoadReconcilerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 15*time.Second || cfg.SyncTimeout != 2*time.Minute || !cfg.RetryErrors {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
}
