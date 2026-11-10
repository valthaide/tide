package compose

import (
	"errors"
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/syncerr"
)

func TestDockerAPICompatible(t *testing.T) {
	ok, err := dockerAPICompatible("1.44", "1.44")
	if err != nil || !ok {
		t.Fatalf("1.44 vs 1.44: ok=%v err=%v", ok, err)
	}
	ok, err = dockerAPICompatible("1.49", "1.44")
	if err != nil || !ok {
		t.Fatalf("newer client: ok=%v err=%v", ok, err)
	}
	ok, err = dockerAPICompatible("1.41", "1.44")
	if err != nil || ok {
		t.Fatalf("old client: ok=%v err=%v", ok, err)
	}
}

func TestDockerAPIIncompatibleError(t *testing.T) {
	err := dockerAPIIncompatibleError("1.41", "1.44")
	var se *syncerr.Error
	if !errors.As(err, &se) || se.Code != syncerr.CodeDockerAPI {
		t.Fatalf("code: %v", err)
	}
	if !strings.Contains(se.Error(), "docker_api_incompatible") || !strings.Contains(se.Error(), "1.41") || !strings.Contains(se.Error(), "1.44") {
		t.Fatalf("message: %s", se.Error())
	}
	classified := syncerr.Classify(errors.New("Error response from daemon: client version 1.41 is too old. Minimum supported API version is 1.44"))
	if classified.Code != syncerr.CodeDockerAPI {
		t.Fatalf("classify: %s", classified.Code)
	}
}
