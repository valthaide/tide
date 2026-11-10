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
# HELP tide_app_commit_drift Whether last_seen_commit differs from last_synced_commit (1=yes, 0=no).
# TYPE tide_app_commit_drift gauge
tide_app_commit_drift{app_id="app-1",app_name="demo"} 0
tide_app_commit_drift{app_id="app-2",app_name="broken"} 1
# HELP tide_app_last_sync_error Whether the app currently has a last sync error (1=yes, 0=no).
# TYPE tide_app_last_sync_error gauge
tide_app_last_sync_error{app_id="app-1",app_name="demo",code="none",error="",stage="none"} 0
tide_app_last_sync_error{app_id="app-2",app_name="broken",code="none",error="boom",stage="none"} 1
# HELP tide_app_sync_status Current sync status of an app (1 for the active status).
# TYPE tide_app_sync_status gauge
tide_app_sync_status{app_id="app-1",app_name="demo",status="synced"} 1
tide_app_sync_status{app_id="app-2",app_name="broken",status="error"} 1
# HELP tide_apps Number of registered apps by status.
# TYPE tide_apps gauge
tide_apps{status="error"} 1
tide_apps{status="synced"} 1
# HELP tide_reconcile_loops_total Number of reconciler loop iterations.
# TYPE tide_reconcile_loops_total counter
tide_reconcile_loops_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"tide_apps",
		"tide_app_sync_status",
		"tide_app_last_sync_error",
		"tide_app_commit_drift",
		"tide_reconcile_loops_total",
	); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendedAndRollbackGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newAppsCollector(staticLister{apps: []AppSnapshot{{
		ID: "1", Name: "web", Status: "synced", Suspended: true,
		LastSyncError:      "revision b1b2b3b4 not healthy within 120ms; rolled back to a1a2a3a4",
		LastSyncErrorCode:  "unhealthy",
		LastSyncErrorStage: "health",
		LastSeenCommit:     "b1b2b3b4",
		LastSyncedCommit:   "a1a2a3a4",
		LastHealthyCommit:  "a1a2a3a4",
		RejectedCommit:     "b1b2b3b4",
	}}})
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	expected := `
# HELP tide_app_rolled_back 1 while a failed revision was rolled back and the last healthy revision is running.
# TYPE tide_app_rolled_back gauge
tide_app_rolled_back{app="web",reason="health_timeout"} 1
# HELP tide_app_suspended Whether automatic reconciliation is paused for the app (1=yes, 0=no).
# TYPE tide_app_suspended gauge
tide_app_suspended{app="web"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"tide_app_suspended",
		"tide_app_rolled_back",
	); err != nil {
		t.Fatal(err)
	}
}

func TestMetricLabelsExcludeSHAsPipelineIDsAndUsers(t *testing.T) {
	reg := prometheus.NewRegistry()
	const (
		commitSHA   = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
		fleetCommit = "11223344556677889900aabbccddeeff00112233"
	)
	lister := staticLister{apps: []AppSnapshot{{
		ID:                 "app-1",
		Name:               "dev-1-backend",
		Status:             "error",
		Revision:           commitSHA,
		Branch:             "main",
		LastSeenCommit:     commitSHA,
		LastSyncedCommit:   "0123456789abcdef0123456789abcdef01234567",
		LastSyncError:      "deploy blocked: pipeline id=481516 source=push status=failed user=alice commit=" + commitSHA,
		LastSyncErrorCode:  "blocked_by_ci",
		LastSyncErrorStage: "ci_gate",
	}}}
	if err := Register(reg, lister); err != nil {
		t.Fatal(err)
	}
	SetFleetRevision(fleetCommit)
	ObserveDeployBlocked("dev-1-backend", "ci_failed", "push")

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range families {
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				val := lp.GetValue()
				if strings.Contains(val, commitSHA) || strings.Contains(val, fleetCommit) || strings.Contains(val, "481516") || strings.Contains(val, "alice") {
					t.Fatalf("forbidden identifier leaked into metric %s label %s=%q", fam.GetName(), lp.GetName(), val)
				}
			}
		}
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
# HELP tide_app_image_info Running compose image and digest (value always 1).
# TYPE tide_app_image_info gauge
tide_app_image_info{app="php",digest="sha256:abc",image="registry.example/php:1",service="web"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "tide_app_image_info"); err != nil {
		t.Fatal(err)
	}
	SetAppImages(nil)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(""), "tide_app_image_info"); err != nil {
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
# HELP tide_app_image_not_found 1 when the image manifest is missing and the app waits until desired state changes.
# TYPE tide_app_image_not_found gauge
tide_app_image_not_found{app="coach"} 1
tide_app_image_not_found{app="web"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "tide_app_image_not_found"); err != nil {
		t.Fatal(err)
	}
}

func TestQueueAppWaitGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(QueueAppWait); err != nil {
		t.Fatal(err)
	}
	SetQueueAppWaits(map[string]time.Duration{"dev-2-backend": 90 * time.Minute}, "dev-1-backend", "dev-2-backend")
	expected := `
# HELP tide_sync_queue_app_wait_seconds Seconds an app has been waiting in the apply queue.
# TYPE tide_sync_queue_app_wait_seconds gauge
tide_sync_queue_app_wait_seconds{app="dev-1-backend"} 0
tide_sync_queue_app_wait_seconds{app="dev-2-backend"} 5400
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "tide_sync_queue_app_wait_seconds"); err != nil {
		t.Fatal(err)
	}
	SetQueueAppWaits(nil)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(""), "tide_sync_queue_app_wait_seconds"); err != nil {
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
