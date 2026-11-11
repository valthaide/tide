package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/api"
)

func TestParseTemplateKeepsComposePathEmpty(t *testing.T) {
	doc, err := ParseHostDocument([]byte(`
host: mon.example
apps:
  - name: web
    repo_url: https://git.example/org/web.git
    template: web
`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Apps[0].Template != "web" {
		t.Fatalf("template: %+v", doc.Apps[0])
	}
	if doc.Apps[0].ComposePath != "" {
		t.Fatalf("template app must not get a default compose_path, got %q", doc.Apps[0].ComposePath)
	}
}

func TestValidateTemplateComposePathConflict(t *testing.T) {
	report, err := Validate([]byte(`
host: mon.example
apps:
  - name: web
    repo_url: https://git.example/org/web.git
    template: web
    compose_path: docker-compose.yml
  - name: bad
    repo_url: https://git.example/org/bad.git
    template: "../escape"
`), ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var conflict, invalid bool
	for _, issue := range report.Issues {
		if issue.Rule == "template_compose_path_conflict" && issue.App == "web" {
			conflict = true
		}
		if issue.Rule == "invalid_template" && issue.App == "bad" {
			invalid = true
		}
	}
	if !conflict || !invalid || report.OK {
		t.Fatalf("report: %+v", report)
	}
}

func TestFleetUpsertRejectsTemplateAndComposePath(t *testing.T) {
	apps := &fakeApps{list: []*api.App{}}
	r := &Reconciler{Apps: apps, Cleaner: &fakeCleaner{}}
	err := r.upsertApp(context.Background(), AppSpec{
		Name: "web", RepoURL: "https://git.example/org/web.git", Branch: "main",
		Template: "web", ComposePath: "docker-compose.yml",
	}, &HostDocument{})
	if err == nil || !strings.Contains(err.Error(), "template_compose_path_conflict") {
		t.Fatalf("err: %v", err)
	}
	if len(apps.list) != 0 {
		t.Fatalf("conflicting app must not be registered: %+v", apps.list)
	}
}

func TestTemplateChangeRequeuesOnlyAppsUsingIt(t *testing.T) {
	root := t.TempDir()
	write := func(content string) {
		t.Helper()
		path := filepath.Join(root, "templates", "web", "compose.yaml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("services: {}\n")

	webApp := &api.App{ID: "1", Name: "web", Template: "web", Status: "synced"}
	other := &api.App{ID: "2", Name: "other", ComposePath: "docker-compose.yml", Status: "synced"}
	apps := &fakeApps{list: []*api.App{webApp, other}}
	r := &Reconciler{Apps: apps, Cleaner: &fakeCleaner{}, fleetRoot: root}
	doc := &HostDocument{Apps: []AppSpec{
		{Name: "web", Template: "web"},
		{Name: "other", ComposePath: "docker-compose.yml"},
	}}

	// First observation is the baseline only.
	r.enqueueTemplateChanges(doc)
	if webApp.Status != "synced" {
		t.Fatalf("baseline must not requeue, status %s", webApp.Status)
	}
	// Unchanged template: still nothing.
	r.enqueueTemplateChanges(doc)
	if webApp.Status != "synced" {
		t.Fatalf("unchanged template requeued, status %s", webApp.Status)
	}
	// Template change: only the templated app is queued.
	write("services:\n  web:\n    image: nginx:1.28\n")
	r.enqueueTemplateChanges(doc)
	if webApp.Status != "pending" {
		t.Fatalf("template change must queue the app, status %s", webApp.Status)
	}
	if other.Status != "synced" {
		t.Fatalf("compose_path app must not be queued by a template change, status %s", other.Status)
	}
}
