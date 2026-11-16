package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestFleetRevisionInfoLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(fleetRevisionInfo); err != nil {
		t.Fatal(err)
	}
	const sha = "11223344556677889900aabbccddeeff00112233"
	SetFleetRevision(sha)
	defer SetFleetRevision("")
	expected := `
# HELP tide_fleet_revision_info Fleet checkout revision observed by this host (value always 1).
# TYPE tide_fleet_revision_info gauge
tide_fleet_revision_info{host="` + Host() + `",sha="112233445566"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "tide_fleet_revision_info"); err != nil {
		t.Fatal(err)
	}

	const next = "aabbccddeeff0011223344556677889900112233"
	SetFleetRevision(next)
	expected = `
# HELP tide_fleet_revision_info Fleet checkout revision observed by this host (value always 1).
# TYPE tide_fleet_revision_info gauge
tide_fleet_revision_info{host="` + Host() + `",sha="aabbccddeeff"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "tide_fleet_revision_info"); err != nil {
		t.Fatal(err)
	}
}

func TestHostLabelGathererAddsHost(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "tide", Name: "test_host_label_gauge"})
	if err := reg.Register(g); err != nil {
		t.Fatal(err)
	}
	g.Set(1)
	families, err := (hostLabelGatherer{gatherer: reg}).Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || len(families[0].GetMetric()) != 1 {
		t.Fatalf("families=%d", len(families))
	}
	m := families[0].GetMetric()[0]
	if !hasHostLabel(m) {
		t.Fatal("host label missing")
	}
	for _, lp := range m.GetLabel() {
		if lp.GetName() == "host" && lp.GetValue() != Host() {
			t.Fatalf("host=%q want %q", lp.GetValue(), Host())
		}
	}
}

func TestSyncBackoffMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(syncBackoff); err != nil {
		t.Fatal(err)
	}
	SetSyncBackoff("web", 5)
	expected := `
# HELP tide_sync_backoff_seconds Current pause between sync retries for an app repeating the same error.
# TYPE tide_sync_backoff_seconds gauge
tide_sync_backoff_seconds{app="web"} 5
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "tide_sync_backoff_seconds"); err != nil {
		t.Fatal(err)
	}
}
