package fleet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseHostDocument(t *testing.T) {
	doc, err := ParseHostDocument([]byte(`
host: mon.example
apps:
  - name: monitoring
    repo_url: https://git.example/org/monitoring.git
    repo_auth_method: token
    poll_interval: 60s
`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Host != "mon.example" || len(doc.Apps) != 1 {
		t.Fatalf("unexpected doc: %+v", doc)
	}
	if doc.Apps[0].ComposePath != "docker-compose.yml" {
		t.Fatalf("default compose_path: %q", doc.Apps[0].ComposePath)
	}
}

func TestFindHostFile(t *testing.T) {
	root := t.TempDir()
	hosts := filepath.Join(root, "hosts")
	if err := os.MkdirAll(hosts, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hosts, "mon.yaml")
	if err := os.WriteFile(path, []byte("host: mon\napps: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := FindHostFile(root, "mon")
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("got %s want %s", got, path)
	}
}
