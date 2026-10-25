package compose

import (
	"testing"
	"time"
)

func TestOneshotExitZeroIsNotAFailure(t *testing.T) {
	now := time.Now()
	obs := []containerObs{
		{
			Project: "crm", Service: "hasura", Name: "crm-hasura-1",
			Running: true, State: "running", PSStatus: "Up 2 hours",
			DependsOn: "migrate:service_completed_successfully:false",
		},
		{
			Project: "crm", Service: "migrate", Name: "crm-migrate-1",
			Running: false, State: "exited", ExitCode: 0, HasInspect: true,
			PSStatus: "Exited (0) 2 hours ago",
		},
	}
	state := foldObservations(obs, now)["crm"]
	if !state.Ready() {
		t.Fatalf("oneshot exit 0 should leave the stack ready: %+v", state)
	}
	if state.ExitedCount != 0 || state.ContainerCount != 1 {
		t.Fatalf("counts: %+v", state)
	}
}

func TestOneshotExitNonZeroIsUnhealthy(t *testing.T) {
	obs := []containerObs{
		{
			Project: "crm", Service: "hasura", Name: "crm-hasura-1",
			Running: true, State: "running", PSStatus: "Up 1 minute",
			DependsOn: "migrate:service_completed_successfully:false",
		},
		{
			Project: "crm", Service: "migrate", Name: "crm-migrate-1",
			Running: false, State: "exited", ExitCode: 1, HasInspect: true,
			PSStatus: "Exited (1) 1 minute ago",
		},
	}
	state := foldObservations(obs, time.Now())["crm"]
	if state.Ready() || state.UnhealthyCount != 1 {
		t.Fatalf("failed oneshot: %+v", state)
	}
}

func TestFrequentRestartsAreNotHealthy(t *testing.T) {
	now := time.Now()
	obs := []containerObs{{
		Project: "crm", Service: "php-consume", Name: "crm-php-consume-1",
		Running: true, State: "running", PSStatus: "Up 3 seconds",
		RestartCount: 8, StartedAt: now.Add(-3 * time.Second), HasInspect: true,
	}}
	state := foldObservations(obs, now)["crm"]
	if state.Ready() || state.FlappingCount != 1 {
		t.Fatalf("flapping: %+v", state)
	}
}

func TestOldRestartCountStaysHealthy(t *testing.T) {
	now := time.Now()
	obs := []containerObs{{
		Project: "crm", Service: "php", Name: "crm-php-1",
		Running: true, State: "running", PSStatus: "Up 5 hours",
		RestartCount: 8, StartedAt: now.Add(-5 * time.Hour), HasInspect: true,
	}}
	state := foldObservations(obs, now)["crm"]
	if !state.Ready() {
		t.Fatalf("stable container: %+v", state)
	}
}
