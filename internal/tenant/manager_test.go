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

func TestPolicyHoldFreezesTenant(t *testing.T) {
	m := NewManager(t.TempDir())
	m.UpdateSpecs(map[string]Spec{"dev": {Name: "dev", FailurePolicy: PolicyHold}})
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "failed", LastSyncError: "boom"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	st, ok := m.Hold("dev")
	if !ok || st.Reason != ReasonPolicyHold {
		t.Fatalf("%+v ok=%v", st, ok)
	}
	blocked, reason := m.UpdatesBlocked(AppView{Name: "web", Tenant: "dev"})
	if !blocked || reason != ReasonPolicyHold {
		t.Fatalf("blocked=%v reason=%s", blocked, reason)
	}
	m.Evaluate([]AppView{
		{Name: "api", Tenant: "dev", Status: "synced"},
		{Name: "web", Tenant: "dev", Status: "synced"},
	})
	if _, ok := m.Hold("dev"); ok {
		t.Fatal("hold should clear when all apps healthy")
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
	toStop := m.Evaluate([]AppView{
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
