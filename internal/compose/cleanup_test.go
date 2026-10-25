package compose

import "testing"

func TestComposeDownArgs(t *testing.T) {
	keep := composeDownArgs("proj", "compose.yaml", false)
	prune := composeDownArgs("proj", "compose.yaml", true)
	for _, args := range [][]string{keep, prune} {
		for _, a := range args {
			if a == "--volumes" || a == "-v" || a == "--rmi" {
				t.Fatalf("down must not drop volumes: %v", args)
			}
		}
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
