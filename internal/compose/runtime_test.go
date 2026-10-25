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

func TestContainerInspectWithoutHealthcheck(t *testing.T) {
	raw := []byte(`[
  {"Id":"sha256:abc123","Image":"sha256:img1","RestartCount":4,
   "State":{"Status":"running","Running":true,"ExitCode":0,"StartedAt":"2026-10-25T06:00:00.5Z"},
   "HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}},
   "Config":{"Image":"registry.example/php:81d4e14a","Labels":{"com.docker.compose.depends_on":"migrate:service_completed_successfully:false"}}}
]`)
	obs := []containerObs{{ID: "abc123", Project: "crm", Service: "php-consume", Image: "registry.example/php:81d4e14a", PSStatus: "Up 3 seconds"}}
	if err := applyContainerInspectJSON(obs, raw); err != nil {
		t.Fatal(err)
	}
	if !obs[0].HasInspect || obs[0].RestartCount != 4 || obs[0].State != "running" || obs[0].ExitCode != 0 {
		t.Fatalf("inspect: %+v", obs[0])
	}
	if obs[0].Health != "" {
		t.Fatalf("missing healthcheck must stay empty, got %q", obs[0].Health)
	}
	if obs[0].DependsOn == "" || obs[0].ImageID != "sha256:img1" {
		t.Fatalf("labels/image: %+v", obs[0])
	}
	img := []byte(`[{"Id":"sha256:img1","RepoTags":["registry.example/php:81d4e14a"],"RepoDigests":["registry.example/php@sha256:deadbeef"]}]`)
	if err := applyImageInspectJSON(obs, img); err != nil {
		t.Fatal(err)
	}
	if digestLabel(obs[0].RepoDigest, obs[0].ImageID) != "sha256:deadbeef" {
		t.Fatalf("digest %q", obs[0].RepoDigest)
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
