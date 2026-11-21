package compose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valthaide/tide/internal/deploypolicy"
)

func TestWatchFilesHashTemplatePaths(t *testing.T) {
	workDir := t.TempDir()
	fleetDir := t.TempDir()
	repoDir := filepath.Join(workDir, "app1", "repo")
	tmplDir := filepath.Join(fleetDir, "templates", "backend", "nginx")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(tmplDir, "default.conf")
	if err := os.WriteFile(conf, []byte("server {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "app.conf"), []byte("a=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &ComposeExecutor{WorkDir: workDir, FleetCheckoutDir: fleetDir}
	files := []deploypolicy.WatchFile{
		{Path: "templates/backend/nginx/default.conf", Action: "reload", Service: "nginx"},
	}
	sum1, err := e.WatchEntryHashesForApp("app1", files)
	if err != nil {
		t.Fatal(err)
	}
	if sum1["templates/backend/nginx/default.conf"] == "" {
		t.Fatalf("no hash: %+v", sum1)
	}

	mixed := []deploypolicy.WatchFile{
		{Path: "app.conf", Action: "restart"},
		{Path: "templates/backend/nginx/default.conf", Action: "reload", Service: "nginx"},
	}
	mixed1, err := e.WatchEntryHashesForApp("app1", mixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("server { listen 81; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mixed2, err := e.WatchEntryHashesForApp("app1", mixed)
	if err != nil {
		t.Fatal(err)
	}
	if mixed2["templates/backend/nginx/default.conf"] == mixed1["templates/backend/nginx/default.conf"] {
		t.Fatal("template hash must change after edit")
	}
	if mixed2["app.conf"] != mixed1["app.conf"] {
		t.Fatal("repo hash must stay")
	}

	bad := []deploypolicy.WatchFile{{Path: "templates/../../etc/passwd", Action: "reload"}}
	if _, err := e.WatchEntryHashesForApp("app1", bad); err == nil {
		t.Fatal("parent escape must fail")
	}
}

func TestWatchFilesHashTemplateWithoutFleetDir(t *testing.T) {
	workDir := t.TempDir()
	e := &ComposeExecutor{WorkDir: workDir}
	files := []deploypolicy.WatchFile{{Path: "templates/backend/nginx/default.conf", Action: "reload"}}
	if _, err := e.WatchEntryHashesForApp("app1", files); err == nil {
		t.Fatal("missing fleet dir must fail")
	}
}

func TestIsTemplateWatchPath(t *testing.T) {
	cases := map[string]bool{
		"templates/backend/nginx/default.conf": true,
		"./templates/backend/app.conf":         true,
		"templates/backend":                    true,
		"templates/../secret":                  false,
		"templates":                            false,
		"/templates/backend/x":                 false,
		"nginx/default.conf":                   false,
		"":                                     false,
	}
	for path, want := range cases {
		if got := deploypolicy.IsTemplateWatchPath(path); got != want {
			t.Fatalf("IsTemplateWatchPath(%q) = %v, want %v", path, got, want)
		}
	}
}
