package compose

import "testing"

func TestComposeDownArgs(t *testing.T) {
	keep := composeDownArgs("proj", "compose.yaml", false)
	if len(keep) == 0 || keep[len(keep)-1] != "--remove-orphans" {
		t.Fatalf("without volumes: %v", keep)
	}
	for _, a := range keep {
		if a == "--volumes" || a == "-v" || a == "--rmi" {
			t.Fatalf("unexpected flag %q in %v", a, keep)
		}
	}
	prune := composeDownArgs("proj", "compose.yaml", true)
	if prune[len(prune)-1] != "--volumes" {
		t.Fatalf("with volumes: %v", prune)
	}
}

func TestImagesSafeToRemoveLeavesShared(t *testing.T) {
	got := imagesSafeToRemove(
		[]string{"app:1", "postgres:16", "app:1", ""},
		[]string{"postgres:16"},
	)
	if len(got) != 1 || got[0] != "app:1" {
		t.Fatalf("got %v", got)
	}
}

func TestProjectReady(t *testing.T) {
	if (ProjectRuntimeState{}).Ready() {
		t.Fatal("empty project is not ready")
	}
	up := ProjectRuntimeState{ContainerCount: 1, RunningCount: 1}
	if !up.Ready() {
		t.Fatal("running container without healthcheck should be ready")
	}
	starting := ProjectRuntimeState{ContainerCount: 1, RunningCount: 1, StartingCount: 1}
	if starting.Ready() {
		t.Fatal("starting healthcheck is not ready")
	}
	unhealthy := ProjectRuntimeState{ContainerCount: 1, RunningCount: 1, UnhealthyCount: 1}
	if unhealthy.Ready() {
		t.Fatal("unhealthy is not ready")
	}
}
