package controller

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"strings"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/cigate"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/credentials"
	"github.com/valthaide/conops-private/internal/deploypolicy"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/store"
)

type fakeRuntime struct {
	snapshot     map[string]compose.ProjectRuntimeState
	snapshotErr  error
	applyCalls   []string
	applyCommits []string
	applyErr     error
}

func (f *fakeRuntime) Apply(
	_ context.Context,
	appID, _ string,
	_ map[string]string,
	_, _, _, commit string,
	_ string,
	_ []byte,
	_ func(string),
) (string, error) {
	f.applyCalls = append(f.applyCalls, appID)
	f.applyCommits = append(f.applyCommits, commit)
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

func testReconciler(t *testing.T, reg *Registry, rt AppRuntime, cfg ReconcilerConfig) *Reconciler {
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

type scriptedRuntime struct {
	fakeRuntime
	badCommit string
}

func (s *scriptedRuntime) ProjectHealth(context.Context, string) (compose.ProjectRuntimeState, error) {
	rev := ""
	if n := len(s.applyCommits); n > 0 {
		rev = s.applyCommits[n-1]
	}
	if rev == s.badCommit {
		return compose.ProjectRuntimeState{ContainerCount: 1, RunningCount: 1, UnhealthyCount: 1}, nil
	}
	return compose.ProjectRuntimeState{ContainerCount: 1, RunningCount: 1}, nil
}

func TestRevisionToApplySkipsRejected(t *testing.T) {
	if got := revisionToApply(&App{LastSeenCommit: "b", RejectedCommit: "b", LastHealthyCommit: "a"}); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := revisionToApply(&App{LastSeenCommit: "c", RejectedCommit: "b", LastHealthyCommit: "a"}); got != "c" {
		t.Fatalf("new commit: got %q", got)
	}
}

func TestRollbackUnhealthyRevision(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "web", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "aaaaaaaa", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateSyncResult(app.ID, "synced", time.Now(), "aaaaaaaa", "good", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateRollback(app.ID, "aaaaaaaa", ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{HealthTimeout: "40ms"})); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateCommitWithMessage(app.ID, "bbbbbbbb", "bad"); err != nil {
		t.Fatal(err)
	}

	rt := &scriptedRuntime{badCommit: "bbbbbbbb"}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCommits) != 2 || rt.applyCommits[0] != "bbbbbbbb" || rt.applyCommits[1] != "aaaaaaaa" {
		t.Fatalf("apply commits: %v", rt.applyCommits)
	}
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "synced" || got.LastSyncedCommit != "aaaaaaaa" || got.RejectedCommit != "bbbbbbbb" {
		t.Fatalf("after rollback: status=%s synced=%s rejected=%s err=%s", got.Status, got.LastSyncedCommit, got.RejectedCommit, got.LastSyncError)
	}
	if !strings.Contains(got.LastSyncError, "bbbbbbbb") {
		t.Fatalf("error %q does not mention failed revision", got.LastSyncError)
	}

	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCommits) != 2 {
		t.Fatalf("rejected revision reapplied: %v", rt.applyCommits)
	}

	if err := reg.UpdateCommitWithMessage(app.ID, "cccccccc", "fix"); err != nil {
		t.Fatal(err)
	}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCommits) != 3 || rt.applyCommits[2] != "cccccccc" {
		t.Fatalf("commit C: %v", rt.applyCommits)
	}
	got, err = reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSyncedCommit != "cccccccc" || got.RejectedCommit != "" || got.LastSyncError != "" {
		t.Fatalf("after C: synced=%s rejected=%s err=%s", got.LastSyncedCommit, got.RejectedCommit, got.LastSyncError)
	}
}

type reloadRuntime struct {
	fakeRuntime
	hashes   map[string]string
	reloads  []string
	restarts []string
}

func (r *reloadRuntime) WatchFilesHashForApp(string, []deploypolicy.WatchFile) (string, error) {
	return "", nil
}

func (r *reloadRuntime) WatchEntryHashesForApp(_ string, _ []deploypolicy.WatchFile) (map[string]string, error) {
	return r.hashes, nil
}

func (r *reloadRuntime) HTTPReload(_ context.Context, url string) error {
	r.reloads = append(r.reloads, url)
	return nil
}

func (r *reloadRuntime) RestartService(_ context.Context, _, _, service string) (string, error) {
	r.restarts = append(r.restarts, service)
	return "", nil
}

type scriptGate struct {
	results []cigate.Result
	calls   int
}

func (s *scriptGate) CheckCommit(context.Context, string, string) cigate.Result {
	s.calls++
	if len(s.results) == 0 {
		return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Reason: cigate.ReasonOK, Status: "success"}
	}
	i := s.calls - 1
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	return s.results[i]
}

// CI finishes in the moment ConOps starts waiting. The next reconcile deploys.
// It must not sit in syncing until the sync-timeout recovery.
func TestCISuccessOnNextCycleAfterWait(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "web", RepoURL: "https://git.example/org/web.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", LastSyncedCommit: "aaaaaaaa", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{RequirePipelineSuccess: true})); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateSyncResult(app.ID, "synced", time.Now().Add(-time.Hour), "aaaaaaaa", "old", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateCommitWithMessage(app.ID, "bbbbbbbb", "new"); err != nil {
		t.Fatal(err)
	}

	gate := &scriptGate{results: []cigate.Result{
		{Allowed: false, Outcome: cigate.OutcomeWait, Reason: cigate.ReasonCIPending, Status: "running"},
		{Allowed: true, Outcome: cigate.OutcomeAllow, Reason: cigate.ReasonOK, Status: "success"},
	}}
	rt := &fakeRuntime{}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{SyncTimeout: 5 * time.Minute})
	rec.Gate = gate

	started := time.Now()
	rec.reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("deployed while CI was running: %v", rt.applyCalls)
	}
	waiting, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != "pending" {
		t.Fatalf("after CI wait status=%q, want pending", waiting.Status)
	}
	if got := testutil.ToFloat64(metrics.DeployBlockedTotal.WithLabelValues(app.Name, cigate.ReasonCIPending)); got != 1 {
		t.Fatalf("deploy_blocked ci_pending=%v, want 1", got)
	}

	rec.reconcileOnce()
	if time.Since(started) > 30*time.Second {
		t.Fatalf("deploy waited %s, want the next cycle not the sync timeout", time.Since(started))
	}
	if len(rt.applyCalls) != 1 || len(rt.applyCommits) != 1 || rt.applyCommits[0] != "bbbbbbbb" {
		t.Fatalf("next cycle apply commits=%v calls=%v", rt.applyCommits, rt.applyCalls)
	}
	deployed, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deployed.Status != "synced" || deployed.LastSyncedCommit != "bbbbbbbb" {
		t.Fatalf("after next cycle status=%q synced=%q", deployed.Status, deployed.LastSyncedCommit)
	}
	if got := testutil.ToFloat64(metrics.DeployBlockedTotal.WithLabelValues(app.Name, cigate.ReasonCIPending)); got != 0 {
		t.Fatalf("deploy_blocked ci_pending=%v after deploy, want 0", got)
	}
	if gate.calls != 2 {
		t.Fatalf("CI checks=%d, want 2", gate.calls)
	}
}

func TestSyncProgressDoesNotForceSyncing(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "web", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateStatus(app.ID, "pending", nil); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateSyncProgress(app.ID, time.Now(), "waiting for CI: status=running reason=ci_pending"); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("CI wait was forced back to %q", got.Status)
	}
}

func TestHealthFailureNamesContainer(t *testing.T) {
	err := healthFailure("bbbbbbbb", 120*time.Second, compose.ProjectRuntimeState{
		ContainerCount: 1, RunningCount: 1, UnhealthyCount: 1, NotReady: []string{"monitoring-prometheus-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "unhealthy:") || !strings.Contains(err.Error(), "monitoring-prometheus-1") || !strings.Contains(err.Error(), "2m0s") {
		t.Fatal(err)
	}
	err = healthFailure("bbbbbbbb", 120*time.Second, compose.ProjectRuntimeState{
		ContainerCount: 1, RunningCount: 1, StartingCount: 1, NotReady: []string{"monitoring-prometheus-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "health timeout:") {
		t.Fatal(err)
	}
}

func TestSyncAppliesOnlyChangedWatchFile(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "monitoring", RepoURL: "https://example.com/monitoring.git", Branch: "main",
		ComposePath: "docker-compose.yml", PollInterval: "30s",
		LastSeenCommit: "df8f922", Status: "synced",
	}
	addPublicApp(t, reg, app)
	policy := deploypolicy.Encode(deploypolicy.Policy{WatchFiles: []deploypolicy.WatchFile{
		{Path: "prometheus/prometheus.yml", Action: "reload", ReloadURL: "http://prometheus:9090/-/reload"},
		{Path: "alertmanager/alertmanager.yml", Action: "restart", Service: "alertmanager"},
		{Path: "grafana/provisioning", Action: "restart", Service: "grafana"},
	}})
	if err := reg.UpdateDeployPolicy(app.ID, policy); err != nil {
		t.Fatal(err)
	}
	prev := encodeWatchHashes(map[string]string{
		"prometheus/prometheus.yml":     "old",
		"alertmanager/alertmanager.yml": "am",
		"grafana/provisioning":          "gf",
	})
	if err := reg.UpdateWatchHash(app.ID, prev); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	rt := &reloadRuntime{hashes: map[string]string{
		"prometheus/prometheus.yml":     "new",
		"alertmanager/alertmanager.yml": "am",
		"grafana/provisioning":          "gf",
	}}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{})
	rec.recordAppliedWatchHash(got)
	if len(rt.reloads) != 1 || rt.reloads[0] != "http://prometheus:9090/-/reload" {
		t.Fatalf("reloads %v", rt.reloads)
	}
	if len(rt.restarts) != 0 {
		t.Fatalf("restarts %v", rt.restarts)
	}
}

func TestOperatorSuspendSurvivesFleetAndRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.NewServiceFromEnv(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(db, creds)
	app := &api.App{
		Name: "web", RepoURL: "https://example.com/a.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "abc", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.SetOperatorSuspended(app.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetSuspended(app.ID, false); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	reg = NewRegistry(db, creds)
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.OperatorSuspended || !got.Suspended || got.FleetSuspended {
		t.Fatalf("pause lost across reopen: %+v", got)
	}
	rt := &fakeRuntime{}
	testReconciler(t, reg, rt, ReconcilerConfig{}).reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("suspended app was reconciled: %v", rt.applyCalls)
	}
	if err := reg.SetOperatorSuspended(app.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Suspended || got.Status != "pending" {
		t.Fatalf("resume: suspended=%v status=%s", got.Suspended, got.Status)
	}
}
