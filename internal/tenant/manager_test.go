package tenant

import (
	"path/filepath"
	"testing"
	"time"
)

func TestManualHoldPersists(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.ManualHold("acme", "experiment")
	m2 := NewManager(dir)
	st, ok := m2.Hold("acme")
	if !ok || !st.Manual || st.Reason != ReasonManual {
		t.Fatalf("hold: %+v ok=%v", st, ok)
	}
	m2.Release("acme")
	if _, err := filepath.Glob(filepath.Join(dir, "*")); err != nil {
		t.Fatal(err)
	}
	m3 := NewManager(dir)
	if _, ok := m3.Hold("acme"); ok {
		t.Fatal("expected release")
	}
}

func TestPolicyHoldFreezesTenantAndClearsWhenSynced(t *testing.T) {
	m := NewManager(t.TempDir())
	m.UpdateSpecs(map[string]Spec{"dev": {Name: "dev", FailurePolicy: PolicyHold}})
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "failed", LastSyncError: "boom"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	st, ok := m.Hold("dev")
	if !ok || st.Reason != ReasonAuto {
		t.Fatalf("%+v ok=%v", st, ok)
	}
	if len(st.HoldingApps) != 1 || st.HoldingApps[0] != "api" {
		t.Fatalf("holding_apps: %v", st.HoldingApps)
	}
	blocked, reason := m.UpdatesBlocked(AppView{Name: "web", Tenant: "dev", Status: "synced"})
	if !blocked || reason != ReasonAuto {
		t.Fatalf("blocked=%v reason=%s", blocked, reason)
	}
	// Failed/pending apps keep reconciling despite hold.
	if blocked, _ := m.UpdatesBlocked(AppView{Name: "api", Tenant: "dev", Status: "failed"}); blocked {
		t.Fatal("failed app must not be blocked by hold")
	}
	if blocked, _ := m.UpdatesBlocked(AppView{Name: "front", Tenant: "dev", Status: "pending", LastSyncError: "image_not_found"}); blocked {
		t.Fatal("pending app must not be blocked by hold")
	}

	_, cleared := m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "synced"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	if _, ok := m.Hold("dev"); ok {
		t.Fatal("hold should clear when all apps healthy")
	}
	if len(cleared) != 1 || cleared[0] != "dev" {
		t.Fatalf("cleared=%v", cleared)
	}
}

func TestManualReleaseOfAutoHoldSuppressesUntilHealthy(t *testing.T) {
	m := NewManager(t.TempDir())
	m.UpdateSpecs(map[string]Spec{"dev": {Name: "dev", FailurePolicy: PolicyHold}})
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "failed", LastSyncError: "boom"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	if _, ok := m.Hold("dev"); !ok {
		t.Fatal("expected auto hold")
	}
	m.Release("dev")
	if _, ok := m.Hold("dev"); ok {
		t.Fatal("release must clear hold")
	}
	// Same ongoing failures must not re-arm hold on the next Evaluate.
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "failed", LastSyncError: "boom"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	if _, ok := m.Hold("dev"); ok {
		t.Fatal("auto hold must stay suppressed until next failure after healthy")
	}
	// Recovery clears the suppress latch.
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "synced"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	// A new failure re-arms hold.
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "failed", LastSyncError: "again"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	st, ok := m.Hold("dev")
	if !ok || st.Reason != ReasonAuto {
		t.Fatalf("expected re-armed auto hold, got %+v ok=%v", st, ok)
	}
}

func TestManualHoldDoesNotAutoClear(t *testing.T) {
	m := NewManager(t.TempDir())
	m.UpdateSpecs(map[string]Spec{"dev": {Name: "dev", FailurePolicy: PolicyHold}})
	m.ManualHold("dev", "experiment")
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "synced"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	st, ok := m.Hold("dev")
	if !ok || !st.Manual || st.Reason != ReasonManual {
		t.Fatalf("manual hold must survive healthy evaluate: %+v ok=%v", st, ok)
	}
}

func TestPolicyStopAfterGrace(t *testing.T) {
	m := NewManager(t.TempDir())
	m.UpdateSpecs(map[string]Spec{"prod": {Name: "prod", FailurePolicy: PolicyStop, CriticalGrace: time.Millisecond}})
	m.Evaluate([]AppView{
		{Name: "backend", Tenant: "prod", Status: "failed", Critical: true, LastSyncError: "down"},
		{Name: "frontend", Tenant: "prod", Status: "synced"},
	})
	time.Sleep(2 * time.Millisecond)
	toStop, _ := m.Evaluate([]AppView{
		{Name: "backend", Tenant: "prod", Status: "failed", Critical: true, LastSyncError: "down"},
		{Name: "frontend", Tenant: "prod", Status: "synced"},
	})
	if len(toStop) != 1 || toStop[0] != "frontend" {
		t.Fatalf("toStop=%v", toStop)
	}
}

func TestDependsReady(t *testing.T) {
	byName := map[string]AppView{
		"db":  {Name: "db", Status: "synced"},
		"api": {Name: "api", Status: "pending"},
	}
	if !DependsReady(AppView{Name: "api", DependsOn: []string{"db"}}, byName) {
		t.Fatal("db is synced")
	}
	if DependsReady(AppView{Name: "web", DependsOn: []string{"api"}}, byName) {
		t.Fatal("api is not synced")
	}
}
