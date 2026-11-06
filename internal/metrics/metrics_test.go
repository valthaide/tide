package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type staticLister struct {
	apps []AppSnapshot
}

func (s staticLister) ListAppSnapshots() []AppSnapshot {
	return s.apps
}

func TestAppsCollectorExposesGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	lister := staticLister{apps: []AppSnapshot{{
		ID:               "app-1",
		Name:             "demo",
		Status:           "synced",
		LastSyncAt:       time.Unix(1700000000, 0).UTC(),
		LastSyncError:    "",
		LastSeenCommit:   "abc",
		LastSyncedCommit: "abc",
	}, {
		ID:               "app-2",
		Name:             "broken",
		Status:           "error",
		LastSyncAt:       time.Unix(1700000100, 0).UTC(),
		LastSyncError:    "boom",
		LastSeenCommit:   "def",
		LastSyncedCommit: "abc",
	}}}
	if err := Register(reg, lister); err != nil {
		t.Fatal(err)
	}

	ReconcileLoopsTotal.Inc()
	ObserveSync("app-1", "demo", 2*time.Second, nil)
	ObserveSync("app-2", "broken", time.Second, assertErr{})

	expected := `
# HELP conops_app_commit_drift Whether last_seen_commit differs from last_synced_commit (1=yes, 0=no).
# TYPE conops_app_commit_drift gauge
conops_app_commit_drift{app_id="app-1",app_name="demo"} 0
conops_app_commit_drift{app_id="app-2",app_name="broken"} 1
# HELP conops_app_last_sync_error Whether the app currently has a last sync error (1=yes, 0=no).
# TYPE conops_app_last_sync_error gauge
conops_app_last_sync_error{app_id="app-1",app_name="demo",code="none",error="",stage="none"} 0
conops_app_last_sync_error{app_id="app-2",app_name="broken",code="none",error="boom",stage="none"} 1
# HELP conops_app_sync_status Current sync status of an app (1 for the active status).
# TYPE conops_app_sync_status gauge
conops_app_sync_status{app_id="app-1",app_name="demo",status="synced"} 1
conops_app_sync_status{app_id="app-2",app_name="broken",status="error"} 1
# HELP conops_apps Number of registered apps by status.
# TYPE conops_apps gauge
conops_apps{status="error"} 1
conops_apps{status="synced"} 1
# HELP conops_reconcile_loops_total Number of reconciler loop iterations.
# TYPE conops_reconcile_loops_total counter
conops_reconcile_loops_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"conops_apps",
		"conops_app_sync_status",
		"conops_app_last_sync_error",
		"conops_app_commit_drift",
		"conops_reconcile_loops_total",
	); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendedAndRollbackGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newAppsCollector(staticLister{apps: []AppSnapshot{{
		ID: "1", Name: "web", Status: "synced", Suspended: true,
		LastSyncError:      "revision bbbbbbb not healthy within 120ms; rolled back to aaaaaaa",
		LastSyncErrorCode:  "unhealthy",
		LastSyncErrorStage: "health",
		LastSeenCommit:     "bbbbbbb",
		LastSyncedCommit:   "aaaaaaa",
		LastHealthyCommit:  "aaaaaaa",
		RejectedCommit:     "bbbbbbb",
	}}})
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	expected := `
# HELP conops_app_rolled_back 1 while a failed revision was rolled back and the last healthy revision is running.
# TYPE conops_app_rolled_back gauge
conops_app_rolled_back{app="web",failed_revision="bbbbbbb",reason="health_timeout",running_revision="aaaaaaa"} 1
# HELP conops_app_suspended Whether automatic reconciliation is paused for the app (1=yes, 0=no).
# TYPE conops_app_suspended gauge
conops_app_suspended{app="web"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"conops_app_suspended",
		"conops_app_rolled_back",
	); err != nil {
		t.Fatal(err)
	}
}

func TestAppImageInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(appImageInfo); err != nil {
		t.Fatal(err)
	}
	SetAppImages([]ImageSample{{
		App: "php", Service: "web", Image: "registry.example/php:1", Digest: "sha256:abc",
	}})
	expected := `
# HELP conops_app_image_info Running compose image and digest (value always 1).
# TYPE conops_app_image_info gauge
conops_app_image_info{app="php",digest="sha256:abc",image="registry.example/php:1",service="web"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "conops_app_image_info"); err != nil {
		t.Fatal(err)
	}
	SetAppImages(nil)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(""), "conops_app_image_info"); err != nil {
		t.Fatal(err)
	}
}

func TestImageNotFoundGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newAppsCollector(staticLister{apps: []AppSnapshot{
		{ID: "1", Name: "coach", Status: "image_not_found", LastSyncErrorCode: "image_not_found", LastSyncError: "image_not_found: app:missing"},
		{ID: "2", Name: "web", Status: "synced"},
	}})
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	expected := `
# HELP conops_app_image_not_found 1 when the image manifest is missing and the app waits until desired state changes.
# TYPE conops_app_image_not_found gauge
conops_app_image_not_found{app="coach"} 1
conops_app_image_not_found{app="web"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "conops_app_image_not_found"); err != nil {
		t.Fatal(err)
	}
}

func TestQueueAppWaitGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(QueueAppWait); err != nil {
		t.Fatal(err)
	}
	SetQueueAppWaits(map[string]time.Duration{"dev-2-backend": 90 * time.Minute})
	expected := `
# HELP conops_sync_queue_app_wait_seconds Seconds an app has been waiting in the apply queue.
# TYPE conops_sync_queue_app_wait_seconds gauge
conops_sync_queue_app_wait_seconds{app="dev-2-backend"} 5400
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "conops_sync_queue_app_wait_seconds"); err != nil {
		t.Fatal(err)
	}
	SetQueueAppWaits(nil)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(""), "conops_sync_queue_app_wait_seconds"); err != nil {
		t.Fatal(err)
	}
}

func TestGitFetchErrorsStartAtZero(t *testing.T) {
	EnsureGitFetchErrors("https://git.example/org/monitoring.git")
	for _, reason := range []string{"auth", "network", "not_found", "other"} {
		got := testutil.ToFloat64(GitFetchErrorsTotal.WithLabelValues("https://git.example/org/monitoring.git", reason))
		if got != 0 {
			t.Fatalf("%s = %v", reason, got)
		}
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }
