package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissingInterpolations(t *testing.T) {
	text := "image: registry/php:${APP_TAG}\nimage2: registry/php:${OTHER:-latest}\nescape: $${APP_TAG}\n"
	missing := MissingInterpolations(text, map[string]string{})
	if len(missing) != 1 || missing[0] != "APP_TAG" {
		t.Fatalf("missing: %v", missing)
	}
	if got := MissingInterpolations(text, map[string]string{"APP_TAG": "1"}); len(got) != 0 {
		t.Fatalf("set var still missing: %v", got)
	}
	if got := MissingInterpolations("image: x:${NEED:?set}", nil); len(got) != 1 || got[0] != "NEED" {
		t.Fatalf("required: %v", got)
	}
}

func TestServicesWithBuild(t *testing.T) {
	raw := []byte(`{"services":{"php":{"image":"reg/php:1"},"job":{"build":{"context":"."},"image":"job"}}}`)
	got, err := ServicesWithBuild(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "job" {
		t.Fatalf("services: %v", got)
	}
	images := imagesFromConfig(raw)
	if len(images) != 2 {
		t.Fatalf("images: %v", images)
	}
}

func TestPullFailedImage(t *testing.T) {
	out := `Error response from daemon: manifest for registry.example/php:9 not found`
	if got := PullFailedImage(out, []string{"registry.example/php:9"}); got != "registry.example/php:9" {
		t.Fatalf("got %q", got)
	}
	if got := PullFailedImage(`pull access denied for registry.example/php, repository does not exist`, nil); got != "registry.example/php" {
		t.Fatalf("denied: %q", got)
	}
}

func TestDigestLabel(t *testing.T) {
	if got := digestLabel("registry.example/php@sha256:abc", "sha256:id"); got != "sha256:abc" {
		t.Fatalf("got %q", got)
	}
	if got := digestLabel("", "sha256:id"); got != "sha256:id" {
		t.Fatalf("id: %q", got)
	}
}

func TestRegistryCredsPartialIsError(t *testing.T) {
	t.Setenv("REGISTRY_HOST", "registry.example")
	t.Setenv("REGISTRY_USERNAME", "")
	t.Setenv("REGISTRY_PASSWORD", "")
	t.Setenv("CONOPS_REGISTRY_HOST", "")
	t.Setenv("CONOPS_REGISTRY_USERNAME", "")
	t.Setenv("CONOPS_REGISTRY_PASSWORD", "")
	_, _, err := RegistryCredsFromEnv()
	if err == nil {
		t.Fatal("partial registry env must fail")
	}
}

func TestDockerLoginDoesNotLeakPassword(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$DOCKER_CONFIG/args"
cat > "$DOCKER_CONFIG/stdin"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cfg")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	const password = "super-secret-token"
	err := dockerLogin(context.Background(), bin, RegistryCreds{
		Host: "registry.example", Username: "ci", Password: password,
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(cfg, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), password) {
		t.Fatalf("password leaked into argv: %s", args)
	}
	stdin, err := os.ReadFile(filepath.Join(cfg, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != password {
		t.Fatalf("stdin: %q", stdin)
	}
	entries, err := os.ReadDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "args" && e.Name() != "stdin" {
			data, _ := os.ReadFile(filepath.Join(cfg, e.Name()))
			if strings.Contains(string(data), password) {
				t.Fatalf("password written to %s", e.Name())
			}
		}
	}

	fail := filepath.Join(dir, "docker-fail")
	failScript := "#!/bin/sh\ncat >&2\nexit 1\n"
	if err := os.WriteFile(fail, []byte(failScript), 0o755); err != nil {
		t.Fatal(err)
	}
	err = dockerLogin(context.Background(), fail, RegistryCreds{
		Host: "registry.example", Username: "ci", Password: password,
	}, cfg)
	if err == nil {
		t.Fatal("expected login failure")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("password in error: %v", err)
	}
}

func TestClassifyPullErrorPrefersNotFound(t *testing.T) {
	err := classifyPullError(context.DeadlineExceeded, "manifest for app:missing not found", true, time.Minute, []string{"app:missing"})
	if err == nil || !strings.Contains(err.Error(), "pull_failed: not_found") {
		t.Fatal(err)
	}
	err = classifyPullError(context.DeadlineExceeded, "", true, time.Minute, []string{"app:slow"})
	if err == nil || !strings.Contains(err.Error(), "timeout: pull") {
		t.Fatal(err)
	}
}

func TestOneshotServiceNames(t *testing.T) {
	raw := []byte(`{"services":{"php":{"depends_on":{"migrate":{"condition":"service_completed_successfully"}}},"migrate":{"image":"app"}}}`)
	got := OneshotServiceNames(raw)
	if len(got) != 1 || got[0] != "migrate" {
		t.Fatalf("%v", got)
	}
}
