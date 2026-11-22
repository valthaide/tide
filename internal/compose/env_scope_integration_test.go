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
// `docker compose` CLI: the resolved render drops env_file (the regression
// that forced whole-project recreates), while the --no-env-resolution render
// keeps it and maps the changed file to exactly the services mounting it.
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
	content := "services:\n" +
		"  app:\n" +
		"    image: alpine:3.20\n" +
		"    env_file:\n" +
		"      - ./secrets.env\n" +
		"  nginx:\n" +
		"    image: nginx:1.27\n"
	if err := os.WriteFile(composeFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := Invocation{ProjectDir: dir, Files: []string{composeFile}, Primary: composeFile}

	resolved, err := renderComposeConfigDirect(ctx, inv, "tideenvmap", nil, nil)
	if err != nil {
		t.Fatalf("resolved render: %v", err)
	}
	matched, unmapped := ServicesForEnvPaths(resolved, []string{envPath})
	if len(matched) != 0 || len(unmapped) != 1 {
		t.Fatalf("resolved config must drop env_file keys, got matched=%v unmapped=%v", matched, unmapped)
	}

	mapping, err := renderComposeConfigNoEnvResolve(ctx, inv, "tideenvmap", nil, nil)
	if err != nil {
		t.Skipf("compose without --no-env-resolution, scoped mapping unavailable: %v", err)
	}
	matched, unmapped = ServicesForEnvPaths(mapping, []string{envPath})
	if len(unmapped) != 0 {
		t.Fatalf("env file unmapped in no-env-resolution render: %v", unmapped)
	}
	if len(matched) != 1 || matched[0] != "app" {
		t.Fatalf("matched=%v, want exactly [app]", matched)
	}
}
