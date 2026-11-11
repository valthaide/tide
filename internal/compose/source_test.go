package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSourceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func templateFixture(t *testing.T) (fleetDir, appDir string) {
	t.Helper()
	fleetDir = t.TempDir()
	appDir = t.TempDir()
	writeSourceFile(t, filepath.Join(fleetDir, "templates", "web", "compose.yaml"),
		"services:\n  web:\n    image: nginx:1.27\n")
	return fleetDir, appDir
}

func TestResolveInvocationTemplateWithOverride(t *testing.T) {
	fleetDir, appDir := templateFixture(t)
	writeSourceFile(t, filepath.Join(appDir, "compose.override.yaml"),
		"services:\n  web:\n    environment:\n      - MODE=prod\n")

	exec := &ComposeExecutor{FleetCheckoutDir: fleetDir}
	inv, err := exec.ResolveInvocation(appDir, "", ApplyOptions{Template: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.ProjectDir != appDir {
		t.Fatalf("project directory = %q, want app dir %q", inv.ProjectDir, appDir)
	}
	if !inv.HasOverride || inv.Template != "web" {
		t.Fatalf("invocation: %+v", inv)
	}
	if len(inv.Files) != 2 || inv.Files[0] != inv.TemplateFile || inv.Files[1] != inv.OverrideFile {
		t.Fatalf("file order: %v", inv.Files)
	}

	// Merge order: template, app override, then Tide's own files last.
	args := inv.Args("proj", []string{"-f", "/tide/env-override.yml"}, "up", "-d")
	joined := strings.Join(args, " ")
	wantPrefix := "compose --project-directory " + appDir + " -p proj -f " + inv.TemplateFile +
		" -f " + inv.OverrideFile + " -f /tide/env-override.yml up -d"
	if joined != wantPrefix {
		t.Fatalf("args:\n got %q\nwant %q", joined, wantPrefix)
	}
}

func TestResolveInvocationTemplateWithoutOverrideIsSilent(t *testing.T) {
	fleetDir, appDir := templateFixture(t)
	exec := &ComposeExecutor{FleetCheckoutDir: fleetDir}
	inv, err := exec.ResolveInvocation(appDir, "", ApplyOptions{Template: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.HasOverride || len(inv.Files) != 1 {
		t.Fatalf("missing override must be silently skipped: %+v", inv)
	}
}

func TestResolveInvocationTemplateErrorsAreClear(t *testing.T) {
	fleetDir, appDir := templateFixture(t)
	exec := &ComposeExecutor{FleetCheckoutDir: fleetDir}

	if _, err := exec.ResolveInvocation(appDir, "", ApplyOptions{Template: "missing"}); err == nil ||
		!strings.Contains(err.Error(), "template_unavailable") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing template error: %v", err)
	}
	for _, bad := range []string{"../escape", "a/b", `a\b`, ".", ".."} {
		if _, err := exec.ResolveInvocation(appDir, "", ApplyOptions{Template: bad}); err == nil ||
			!strings.Contains(err.Error(), "invalid_template") {
			t.Fatalf("template %q error: %v", bad, err)
		}
	}
	noFleet := &ComposeExecutor{}
	if _, err := noFleet.ResolveInvocation(appDir, "", ApplyOptions{Template: "web"}); err == nil ||
		!strings.Contains(err.Error(), "template_unavailable") {
		t.Fatalf("no fleet checkout error: %v", err)
	}
	// check-template shares the same resolution and must fail the same way.
	if _, _, err := exec.CheckTemplate(context.Background(), fleetDir, "missing", appDir, "proj", nil); err == nil ||
		!strings.Contains(err.Error(), "template_unavailable") {
		t.Fatalf("check-template missing template error: %v", err)
	}
}

func TestResolveInvocationComposePathUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeSourceFile(t, filepath.Join(dir, "stack", "compose.yaml"), "services: {}\n")
	exec := &ComposeExecutor{}
	inv, err := exec.ResolveInvocation(dir, "stack/compose.yaml", ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Template != "" || inv.ProjectDir != filepath.Join(dir, "stack") || len(inv.Files) != 1 {
		t.Fatalf("compose_path invocation: %+v", inv)
	}
}

func TestTemplateHashChangesWithTemplateAndOverride(t *testing.T) {
	fleetDir, appDir := templateFixture(t)
	workDir := t.TempDir()
	appID := "app1"
	repoDir := filepath.Join(workDir, appID, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exec := &ComposeExecutor{WorkDir: workDir, FleetCheckoutDir: fleetDir}

	h1, err := exec.TemplateHashForApp(appID, "web")
	if err != nil || h1 == "" {
		t.Fatalf("hash: %q err %v", h1, err)
	}
	if again, _ := exec.TemplateHashForApp(appID, "web"); again != h1 {
		t.Fatalf("hash not stable: %q vs %q", h1, again)
	}
	writeSourceFile(t, filepath.Join(repoDir, "compose.override.yaml"), "services: {}\n")
	h2, _ := exec.TemplateHashForApp(appID, "web")
	if h2 == h1 {
		t.Fatal("adding an override must change the hash")
	}
	writeSourceFile(t, filepath.Join(fleetDir, "templates", "web", "compose.yaml"),
		"services:\n  web:\n    image: nginx:1.28\n")
	h3, _ := exec.TemplateHashForApp(appID, "web")
	if h3 == h2 {
		t.Fatal("changing the template must change the hash")
	}
	if hash, _ := exec.TemplateHashForApp(appID, ""); hash != "" {
		t.Fatalf("compose_path app hash = %q, want empty", hash)
	}
	if !exec.HasOverrideForApp(appID) {
		// HasOverrideForApp falls back to the checkout file.
		t.Fatal("expected override to be reported")
	}
	_ = appDir
}
