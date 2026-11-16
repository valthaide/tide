package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalEnvPathsMissingCheckoutIsNotError(t *testing.T) {
	e := &ComposeExecutor{WorkDir: t.TempDir()}
	if e.CheckoutExists("app1") {
		t.Fatal("checkout must not exist yet")
	}
	paths, err := e.ExternalEnvPathsForSource("app1", "", "web", "backend")
	if err != nil {
		t.Fatalf("template source before first clone must not error: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths=%v want empty", paths)
	}
	paths, err = e.ExternalEnvPathsForApp("app1", "compose.yaml")
	if err != nil {
		t.Fatalf("compose_path source before first clone must not error: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths=%v want empty", paths)
	}
}

func TestExternalEnvPathsExistingCheckoutMissingAppDirStillErrors(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "app1", "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	e := &ComposeExecutor{WorkDir: workDir}
	if !e.CheckoutExists("app1") {
		t.Fatal("checkout must exist")
	}
	if _, err := e.ExternalEnvPathsForSource("app1", "", "web", "backend"); err == nil {
		t.Fatal("missing app_dir inside an existing checkout must error")
	}
}
