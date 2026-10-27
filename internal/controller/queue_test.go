package controller

import (
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
