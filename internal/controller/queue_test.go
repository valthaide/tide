package controller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/valthaide/conops-private/internal/api"
)

func TestQueueOrdersManualBeforePeriodic(t *testing.T) {
	reg := testRegistry(t)
	r := testReconciler(t, reg, nil, ReconcilerConfig{MaxConcurrent: 2})
	older := time.Now().Add(-time.Minute)
	r.queue["periodic"] = &QueueItem{AppID: "periodic", Name: "periodic", Trigger: "periodic", Class: 4, QueuedAt: older}
	r.queue["manual"] = &QueueItem{AppID: "manual", Name: "manual", Trigger: "manual", Class: 0, QueuedAt: time.Now(), Priority: 1}
	r.queue["fleet"] = &QueueItem{AppID: "fleet", Name: "fleet", Trigger: "fleet", Class: 1, QueuedAt: older, Priority: 10}
	items := r.Snapshot()
	if len(items) != 3 {
		t.Fatalf("queue = %d", len(items))
	}
	if items[0].Name != "manual" || items[1].Name != "fleet" || items[2].Name != "periodic" {
		t.Fatalf("order = %s %s %s", items[0].Name, items[1].Name, items[2].Name)
	}
}

func TestAppReconcileIntervalRejectsShort(t *testing.T) {
	t.Setenv("CONOPS_APP_RECONCILE_INTERVAL", "10s")
	if _, err := LoadReconcilerConfigFromEnv(); err == nil {
		t.Fatal("expected short interval to be rejected")
	}
	t.Setenv("CONOPS_APP_RECONCILE_INTERVAL", "off")
	cfg, err := LoadReconcilerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AppReconcileOff {
		t.Fatal("off did not disable periodic reconcile")
	}
}

func TestRetryModeDefaultsErrorOnly(t *testing.T) {
	if retryMode(nil) != "error_only" {
		t.Fatal("nil")
	}
	always := "always"
	app := &api.App{}
	if retryMode(app) != "error_only" {
		t.Fatal("empty")
	}
	app.DeployPolicyJSON = `{"retry_rejected":"` + always + `"}`
	if retryMode(app) != "always" {
		t.Fatal(retryMode(app))
	}
}

func TestEnqueueDuringSyncStaysQueued(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "dev-2-backend", RepoURL: "https://example.com/dev.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", LastSyncedCommit: "aaaaaaaa", Status: "syncing",
	}
	addPublicApp(t, reg, app)
	r := testReconciler(t, reg, &fakeRuntime{}, ReconcilerConfig{MaxConcurrent: 2})
	r.mu.Lock()
	r.inFlight[app.ID] = struct{}{}
	r.mu.Unlock()
	if !r.enqueue(app, "fleet", "vars_changed", 0) {
		t.Fatal("expected a new queue row")
	}
	r.mu.Lock()
	item := r.queue[app.ID]
	r.mu.Unlock()
	if item == nil || !item.FollowUp {
		t.Fatal("enqueue during sync must be marked for another pass")
	}
	r.finishSync(app.ID)
	if !r.queued(app.ID) {
		t.Fatal("follow-up enqueue was dropped when the sync finished")
	}
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("status %s, want pending", got.Status)
	}
}

func TestSettledQueueEntryIsDroppedWithoutSync(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "dev-2-backend", RepoURL: "https://example.com/dev.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateSyncResult(app.ID, "synced", time.Now(), "bbbbbbbb", "applied", "", ""); err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	r := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 2, QueueWarnAfter: time.Hour})
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.enqueue(got, "fleet", "vars_changed", 0)
	r.dispatch()
	r.wg.Wait()
	if r.queued(got.ID) {
		t.Fatal("queue row remained after desired state was already applied")
	}
	if len(rt.applyCalls) != 0 {
		t.Fatalf("synced an already applied app: %v", rt.applyCalls)
	}
}

func TestPendingQueueDropsWhenAlreadyApplied(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "dev-2-backend", RepoURL: "https://example.com/dev.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateSyncResult(app.ID, "synced", time.Now(), "bbbbbbbb", "applied", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateStatus(app.ID, "pending", nil); err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	r := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 2, QueueWarnAfter: time.Hour})
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.enqueue(got, "fleet", "vars_changed", 0)
	r.dispatch()
	r.wg.Wait()
	if r.queued(got.ID) {
		t.Fatal("applied app stayed queued")
	}
	if len(rt.applyCalls) != 0 {
		t.Fatalf("synced an already applied app: %v", rt.applyCalls)
	}
	got, err = reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "synced" {
		t.Fatalf("status %s, want synced", got.Status)
	}
}

func TestStuckQueueIsPutBackToWork(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "dev-2-backend", RepoURL: "https://example.com/dev.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", Status: "pending",
	}
	addPublicApp(t, reg, app)
	if err := reg.UpdateSyncResult(app.ID, "synced", time.Now(), "aaaaaaaa", "old", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateCommitWithMessage(app.ID, "bbbbbbbb", "new"); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateStatus(app.ID, "synced", nil); err != nil {
		t.Fatal(err)
	}
	r := testReconciler(t, reg, &fakeRuntime{}, ReconcilerConfig{MaxConcurrent: 2, QueueWarnAfter: 15 * time.Minute})
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.enqueue(got, "fleet", "vars_changed", 0)
	r.mu.Lock()
	r.queue[got.ID].QueuedAt = time.Now().Add(-20 * time.Minute)
	r.mu.Unlock()
	r.publishQueue()
	got, err = reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("stuck app status %s, want pending", got.Status)
	}
	if !r.queued(app.ID) {
		t.Fatal("stuck app left the work queue")
	}
}

func TestDuplicateEnqueueIsCollapsed(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "coach", RepoURL: "https://example.com/coach.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "bbbbbbbb", LastSyncedCommit: "aaaaaaaa", Status: "failed",
	}
	addPublicApp(t, reg, app)
	var buf bytes.Buffer
	r := testReconciler(t, reg, &fakeRuntime{}, ReconcilerConfig{MaxConcurrent: 2})
	r.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	got, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.rememberNext(got.ID, time.Now().Add(-time.Second), time.Now())
	r.runPeriodic([]*App{got}, nil)
	first := strings.Count(buf.String(), "Enqueued app")
	if first != 1 {
		t.Fatalf("first pass logs:\n%s", buf.String())
	}
	r.scheduleTransient(got.ID)
	r.noteRetrySeen(got.ID, got.LastSeenCommit)
	buf.Reset()
	r.rememberNext(got.ID, time.Now().Add(-time.Second), time.Now())
	fresh, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.runPeriodic([]*App{fresh}, nil)
	if strings.Contains(buf.String(), "Enqueued app") || strings.Contains(buf.String(), "Requeued app") || strings.Contains(buf.String(), "Periodic reconcile queued") {
		t.Fatalf("duplicate enqueue was logged:\n%s", buf.String())
	}
}
