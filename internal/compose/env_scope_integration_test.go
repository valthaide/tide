package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEnvFileMappingWithRealCompose builds the config with the real
// `docker compose` CLI the way the backend template does: env_file comes
// from an anchor (<<: *app-env) and references ${APP_ENV_FILE:?}. The
// resolved render drops the env_file key (the regression that forced
// whole-project recreates); the --no-interpolate render keeps it in every
// checked compose version starting with v2.33.0, and after Tide expands the
// variables and normalizes the paths the changed file maps to exactly the
// services mounting it.
func TestEnvFileMappingWithRealCompose(t *testing.T) {
	dockerPath := strings.TrimSpace(os.Getenv("TIDE_DOCKER_CLI_PATH"))
	if dockerPath == "" {
		dockerPath = "docker"
	}
	if _, err := exec.LookPath(dockerPath); err != nil {
		t.Skipf("docker CLI not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, dockerPath, "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose CLI unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	dir := t.TempDir()
	envPath := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(envPath, []byte("API_KEY=one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	composeFile := filepath.Join(dir, "compose.yaml")
	content := "x-app-env: &app-env\n" +
		"  env_file:\n" +
		"    - ${APP_ENV_FILE:?}\n" +
		"services:\n" +
		"  app:\n" +
		"    <<: *app-env\n" +
		"    image: alpine:3.20\n" +
		"  worker:\n" +
		"    <<: *app-env\n" +
		"    image: alpine:3.20\n" +
		"  nginx:\n" +
		"    image: nginx:1.27\n"
	if err := os.WriteFile(composeFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := Invocation{ProjectDir: dir, Files: []string{composeFile}, Primary: composeFile}
	vars := map[string]string{"APP_ENV_FILE": envPath}

	resolved, err := renderComposeConfigDirect(ctx, inv, "tideenvmap", vars, nil)
	if err != nil {
		t.Fatalf("resolved render: %v", err)
	}
	matched, unmapped := ServicesForEnvPaths(resolved, []string{envPath})
	if len(matched) != 0 || len(unmapped) != 1 {
		t.Fatalf("resolved config must drop env_file keys, got matched=%v unmapped=%v", matched, unmapped)
	}

	raw, err := renderComposeConfigNoInterpolate(ctx, inv, "tideenvmap", vars, nil)
	if err != nil {
		t.Fatalf("--no-interpolate render must work on compose v2.33.0+: %v", err)
	}
	mapping := normalizeEnvFileMapPaths(raw, inv.ProjectDir, vars)
	matched, unmapped = ServicesForEnvPaths(mapping, []string{envPath})
	if len(unmapped) != 0 {
		t.Fatalf("env file unmapped in --no-interpolate render after normalization: %v", unmapped)
	}
	if len(matched) != 2 || matched[0] != "app" || matched[1] != "worker" {
		t.Fatalf("matched=%v, want exactly [app worker]", matched)
	}
}
