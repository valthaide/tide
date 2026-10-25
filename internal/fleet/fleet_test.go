package fleet

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/deploypolicy"
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

func TestParseRevision(t *testing.T) {
	doc, err := ParseHostDocument([]byte(`
host: club.example
apps:
  - name: hello
    repo_url: https://git.example/org/hello.git
    revision: v1.2.3
`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Apps[0].Revision != "v1.2.3" || doc.Apps[0].Branch != "main" {
		t.Fatalf("%+v", doc.Apps[0])
	}
	if doc.Apps[0].PollInterval != "30s" {
		t.Fatalf("poll %s", doc.Apps[0].PollInterval)
	}
}

func TestParsePruneAndHealthTimeout(t *testing.T) {
	doc, err := ParseHostDocument([]byte(`
host: club.example
apps:
  - name: web
    repo_url: https://git.example/org/web.git
    prune_volumes: true
    health_timeout: 45s
`))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Apps[0].PruneVolumes || doc.Apps[0].HealthTimeout != "45s" {
		t.Fatalf("%+v", doc.Apps[0])
	}
	app := &api.App{DeployPolicyJSON: policyJSONFromSpec(doc.Apps[0], "")}
	if d := app.HealthTimeoutDuration(); d.String() != "45s" {
		t.Fatalf("timeout %s", d)
	}
	if (&api.App{}).HealthTimeoutDuration() != api.DefaultHealthTimeout {
		t.Fatal("default health timeout")
	}
}

func TestParseProjectAdoptsExistingStack(t *testing.T) {
	doc, err := ParseHostDocument([]byte(`
host: dev.example
apps:
  - name: crm
    repo_url: https://git.example/org/crm.git
    project: crm-backend
`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Apps[0].Project != "crm-backend" {
		t.Fatalf("project %q", doc.Apps[0].Project)
	}
	app := &api.App{ID: "11111111-2222-3333-4444-555555555555", DeployPolicyJSON: policyJSONFromSpec(doc.Apps[0], "")}
	if app.ComposeProject() != "crm-backend" {
		t.Fatalf("compose project %s", app.ComposeProject())
	}
	bare := &api.App{ID: "app-1"}
	if bare.ComposeProject() != "app-1" {
		t.Fatalf("default project %s", bare.ComposeProject())
	}
}

func TestParseVarsAndBuild(t *testing.T) {
	doc, err := ParseHostDocument([]byte(`
host: club.example
build: forbidden
apps:
  - name: php
    repo_url: https://git.example/org/php.git
    vars:
      APP_TAG: "1.2.3"
    build: allowed
`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Build != "forbidden" || doc.Apps[0].Build != "allowed" || doc.Apps[0].Vars["APP_TAG"] != "1.2.3" {
		t.Fatalf("%+v", doc)
	}
	raw := policyJSONFromSpec(doc.Apps[0], doc.Build)
	policy := deploypolicy.Decode(raw)
	if policy.Build != "allowed" || policy.Vars["APP_TAG"] != "1.2.3" {
		t.Fatalf("policy %+v", policy)
	}
	inherited := policyJSONFromSpec(AppSpec{Name: "php"}, "forbidden")
	if deploypolicy.Decode(inherited).EffectiveBuild() != "forbidden" {
		t.Fatalf("inherited: %s", inherited)
	}
}

func TestVarsChangeMarksPending(t *testing.T) {
	app := &api.App{
		ID: "web", Name: "web", RepoURL: "https://git.example/web.git",
		Branch: "main", ComposePath: "docker-compose.yml", RepoAuthMethod: "public", Status: "synced",
		DeployPolicyJSON: deploypolicy.Encode(deploypolicy.Policy{Vars: map[string]string{"APP_TAG": "1"}}),
	}
	apps := &fakeApps{list: []*api.App{app}}
	r := &Reconciler{Apps: apps, Cleaner: &fakeCleaner{}}
	spec := AppSpec{
		Name: "web", RepoURL: app.RepoURL, Branch: "main", ComposePath: "docker-compose.yml",
		Vars: map[string]string{"APP_TAG": "2"},
	}
	if err := r.applyDesired(context.Background(), &HostDocument{Apps: []AppSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	if app.Status != "pending" {
		t.Fatalf("status %s, want pending", app.Status)
	}
	app.Status = "synced"
	if err := r.applyDesired(context.Background(), &HostDocument{Apps: []AppSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	if app.Status != "synced" {
		t.Fatalf("unchanged vars set status %s", app.Status)
	}
}

func TestMissingHostLogQuietsAfterThreshold(t *testing.T) {
	for streak := 1; streak <= 5; streak++ {
		level, announce := missingHostLog(streak, 3)
		if streak == 3 {
			if level != slog.LevelWarn || !announce {
				t.Fatalf("cycle 3: level=%v announce=%v", level, announce)
			}
			continue
		}
		if level != slog.LevelDebug || announce {
			t.Fatalf("cycle %d: level=%v announce=%v", streak, level, announce)
		}
	}
}

func TestIsHostFileMissing(t *testing.T) {
	_, err := FindHostFile(t.TempDir(), "club.example")
	if !IsHostFileMissing(err) {
		t.Fatalf("missing file: %v", err)
	}
	if IsHostFileMissing(nil) {
		t.Fatal("nil is not missing")
	}
}

type fakeApps struct {
	list         []*api.App
	suspendCalls []bool
	deleted      []string
}

func (f *fakeApps) List() []*api.App { return f.list }
func (f *fakeApps) FindByName(name string) *api.App {
	for _, app := range f.list {
		if app != nil && app.Name == name {
			return app
		}
	}
	return nil
}
func (f *fakeApps) AddWithDeployKeyAndEnvs(app *api.App, _ string, _ map[string]string) error {
	f.list = append(f.list, app)
	return nil
}
func (f *fakeApps) UpdateApp(string, string, string, string, string, map[string]string) error {
	return nil
}
func (f *fakeApps) UpdateDeployPolicy(id, policyJSON string) error {
	if app := f.byID(id); app != nil {
		app.DeployPolicyJSON = policyJSON
		app.ApplyPolicy()
	}
	return nil
}
func (f *fakeApps) UpdateRepoSecret(string, string) error { return nil }
func (f *fakeApps) ClearRepoSecret(string) error          { return nil }
func (f *fakeApps) GetRepoSecret(string) ([]byte, error)  { return nil, nil }
func (f *fakeApps) SetRevision(id, revision string) error {
	if app := f.byID(id); app != nil {
		app.Revision = revision
	}
	return nil
}
func (f *fakeApps) MarkPending(id string) error {
	if app := f.byID(id); app != nil {
		app.Status = "pending"
	}
	return nil
}
func (f *fakeApps) SetSuspended(id string, suspended bool) error {
	f.suspendCalls = append(f.suspendCalls, suspended)
	if app := f.byID(id); app != nil {
		app.FleetSuspended = suspended
		app.Suspended = app.FleetSuspended || app.OperatorSuspended
	}
	return nil
}
func (f *fakeApps) Delete(id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeApps) byID(id string) *api.App {
	for _, app := range f.list {
		if app != nil && app.ID == id {
			return app
		}
	}
	return nil
}

type fakeCleaner struct {
	ids   []string
	prune []bool
}

func (f *fakeCleaner) Destroy(_ context.Context, appID, _ string, _ map[string]string, pruneVolumes bool) (string, error) {
	f.ids = append(f.ids, appID)
	f.prune = append(f.prune, pruneVolumes)
	return "", nil
}

func TestRemoveAppPrunesImagesNotVolumesUnlessFlag(t *testing.T) {
	gone := &api.App{
		ID: "gone", Name: "gone", RepoURL: "https://git.example/gone.git",
		ComposePath:      "docker-compose.yml",
		DeployPolicyJSON: deploypolicy.Encode(deploypolicy.Policy{PruneVolumes: false}),
	}
	keep := &api.App{
		ID: "keep", Name: "keep", RepoURL: "https://git.example/keep.git",
		Branch: "main", ComposePath: "docker-compose.yml", RepoAuthMethod: "public",
	}
	apps := &fakeApps{list: []*api.App{gone, keep}}
	cleaner := &fakeCleaner{}
	r := &Reconciler{Apps: apps, Cleaner: cleaner}
	doc := &HostDocument{Host: "club", Apps: []AppSpec{{
		Name: "keep", RepoURL: "https://git.example/keep.git", Branch: "main", ComposePath: "docker-compose.yml",
	}}}
	if err := r.applyDesired(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if len(cleaner.ids) != 1 || cleaner.ids[0] != "gone" || cleaner.prune[0] {
		t.Fatalf("destroy ids=%v prune=%v", cleaner.ids, cleaner.prune)
	}
	if len(apps.deleted) != 1 || apps.deleted[0] != "gone" {
		t.Fatalf("deleted %v", apps.deleted)
	}

	gone.DeployPolicyJSON = deploypolicy.Encode(deploypolicy.Policy{PruneVolumes: true})
	apps.list = []*api.App{gone, keep}
	apps.deleted = nil
	cleaner.ids, cleaner.prune = nil, nil
	if err := r.applyDesired(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if len(cleaner.prune) != 1 || !cleaner.prune[0] {
		t.Fatalf("prune volumes: %v", cleaner.prune)
	}
}

func TestFleetReconcileDoesNotClearOperatorSuspend(t *testing.T) {
	app := &api.App{
		ID: "web", Name: "web", RepoURL: "https://git.example/web.git",
		Branch: "main", ComposePath: "docker-compose.yml", RepoAuthMethod: "public",
		OperatorSuspended: true, Suspended: true,
	}
	apps := &fakeApps{list: []*api.App{app}}
	r := &Reconciler{Apps: apps, Cleaner: &fakeCleaner{}}
	doc := &HostDocument{Apps: []AppSpec{{
		Name: "web", RepoURL: "https://git.example/web.git", Branch: "main",
		ComposePath: "docker-compose.yml", Suspended: false,
	}}}
	if err := r.applyDesired(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if len(apps.suspendCalls) != 0 {
		t.Fatalf("fleet wrote suspend flag: %v", apps.suspendCalls)
	}
	if !app.OperatorSuspended {
		t.Fatal("operator pause cleared")
	}
}
