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
conops_app_last_sync_error{app_id="app-1",app_name="demo"} 0
conops_app_last_sync_error{app_id="app-2",app_name="broken"} 1
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

type assertErr struct{}

func (assertErr) Error() string { return "boom" }
