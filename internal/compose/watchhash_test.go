package compose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valthaide/conops-private/internal/deploypolicy"
)

func TestWatchFilesHashDirectoryChange(t *testing.T) {
	repo := t.TempDir()
	rules := filepath.Join(repo, "prometheus", "rules")
	if err := os.MkdirAll(rules, 0755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(rules, "a.yml")
	if err := os.WriteFile(f, []byte("rule: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	files := []deploypolicy.WatchFile{{Path: "prometheus/rules", Action: "reload"}}
	h1, err := WatchFilesHash(repo, files)
	if err != nil || h1 == "" {
		t.Fatalf("h1=%q err=%v", h1, err)
	}
	h2, err := WatchFilesHash(repo, files)
	if err != nil || h2 != h1 {
		t.Fatalf("unstable hash: %v %q %q", err, h1, h2)
	}
	if err := os.WriteFile(f, []byte("rule: 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	h3, err := WatchFilesHash(repo, files)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("expected hash change when file inside dir changes")
	}
}

func TestWatchFilesHashGlob(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "grafana", "dashboards")
	_ = os.MkdirAll(dir, 0755)
	_ = os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"a":1}`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "b.json"), []byte(`{"b":1}`), 0644)
	_ = os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("x"), 0644)

	h, err := WatchFilesHash(repo, []deploypolicy.WatchFile{{Path: "grafana/dashboards/*.json"}})
	if err != nil || h == "" {
		t.Fatalf("%q %v", h, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("changed"), 0644)
	h2, err := WatchFilesHash(repo, []deploypolicy.WatchFile{{Path: "grafana/dashboards/*.json"}})
	if err != nil || h2 != h {
		t.Fatalf("txt change should not affect json glob: %v %q %q", err, h, h2)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"a":2}`), 0644)
	h3, err := WatchFilesHash(repo, []deploypolicy.WatchFile{{Path: "grafana/dashboards/*.json"}})
	if err != nil || h3 == h {
		t.Fatalf("json change should affect hash: %v", err)
	}
}

func TestWatchFilesHashMissing(t *testing.T) {
	repo := t.TempDir()
	_, err := WatchFilesHashForAppName("monitoring", repo, []deploypolicy.WatchFile{{Path: "no/such/dir"}})
	if err == nil {
		t.Fatal("expected error")
	}
	wh, ok := err.(*WatchHashError)
	if !ok || wh.App != "monitoring" {
		t.Fatalf("expected WatchHashError with app, got %#v", err)
	}
}

func TestWatchFilesHashSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("secret-data"), 0644)
	link := filepath.Join(repo, "escape")
	if err := os.Symlink(filepath.Join(outside, "secret"), link); err != nil {
		t.Fatal(err)
	}
	h1, err := WatchFilesHash(repo, []deploypolicy.WatchFile{{Path: "escape"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("changed"), 0644)
	h2, err := WatchFilesHash(repo, []deploypolicy.WatchFile{{Path: "escape"}})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("escaping symlink must not follow target content")
	}
}
