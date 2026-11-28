package controller

import (
	"reflect"
	"testing"

	"github.com/valthaide/tide/internal/deploypolicy"
)

func TestDeployFilesForAppComposePathMode(t *testing.T) {
	r := &Reconciler{}
	app := &App{ID: "a1", ComposePath: "deploy/compose.yaml"}
	got := r.deployFilesForApp(app)
	want := []string{".env", "compose.override.yaml", "deploy/compose.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeployFilesForAppTemplateMode(t *testing.T) {
	r := &Reconciler{}
	app := &App{ID: "a2", Template: "backend", AppDir: "services/backend"}
	app.DeployPolicyJSON = deploypolicy.Encode(deploypolicy.Policy{WatchFiles: []deploypolicy.WatchFile{
		{Path: "config/nginx.conf", Action: "restart"},
		{Path: "./secrets/backend.env", Action: "restart"},
		{Path: "templates/backend/default.conf", Action: "restart"},
		{Path: "/etc/hosts", Action: "reload"},
	}})
	got := r.deployFilesForApp(app)
	// No compose file from the app repo in template mode; templates/ watch
	// entries live in the fleet repo; absolute host paths are outside the
	// checkout without an executor mapping.
	want := []string{"config/nginx.conf", "secrets/backend.env", "services/backend/.env", "services/backend/compose.override.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDeployFilesForAppDefaults(t *testing.T) {
	r := &Reconciler{}
	app := &App{ID: "a3"}
	got := r.deployFilesForApp(app)
	want := []string{".env", "compose.override.yaml", "docker-compose.yml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if r.deployFilesForApp(nil) != nil {
		t.Fatal("nil app must yield no deploy files")
	}
}
