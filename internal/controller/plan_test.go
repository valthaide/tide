package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
)

type fakePlanRuntime struct {
	plan        *compose.AppPlan
	gotOpts     compose.ApplyOptions
	gotInput    compose.PlanInput
	gotEnvVars  map[string]string
	watchHashes map[string]string
}

func (f *fakePlanRuntime) Apply(_ context.Context, _, _ string, _ map[string]string, _, _, _, _, _ string, _ []byte, _ func(string)) (string, error) {
	return "", nil
}

func (f *fakePlanRuntime) SnapshotProjects(_ context.Context) (map[string]compose.ProjectRuntimeState, error) {
	return nil, nil
}

func (f *fakePlanRuntime) PlanApp(_ context.Context, _, _ string, envVars map[string]string, opts compose.ApplyOptions, in compose.PlanInput) (*compose.AppPlan, error) {
	f.gotOpts = opts
	f.gotInput = in
	f.gotEnvVars = envVars
	if f.plan != nil {
		return f.plan, nil
	}
	return &compose.AppPlan{Project: opts.Project}, nil
}

func (f *fakePlanRuntime) WatchFilesHashForApp(_ string, files []deploypolicy.WatchFile) (string, error) {
	hashes, err := f.WatchEntryHashesForApp("", files)
	if err != nil {
		return "", err
	}
	return encodeWatchHashes(hashes), nil
}

func (f *fakePlanRuntime) WatchEntryHashesForApp(_ string, files []deploypolicy.WatchFile) (map[string]string, error) {
	out := map[string]string{}
	for _, w := range files {
		out[w.Path] = f.watchHashes[w.Path]
	}
	return out, nil
}

func (f *fakePlanRuntime) RestartService(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

func (f *fakePlanRuntime) HTTPReload(_ context.Context, _ string) error { return nil }

func (f *fakePlanRuntime) ExecService(_ context.Context, _, _, _, _ string) error { return nil }

func TestReconcilerPlanAppPassesDrift(t *testing.T) {
	reg := testRegistry(t)
	envFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(envFile, []byte("TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &api.App{
		Name:             "backend",
		RepoURL:          "https://example/repo.git",
		Branch:           "main",
		ComposePath:      "compose.yaml",
		DeployPolicyJSON: `{"env_files":["` + strings.ReplaceAll(envFile, `\`, `\\`) + `"],"watch_files":[{"path":"templates/backend/nginx/default.conf","action":"restart","service":"nginx"}]}`,
	}
	addPublicApp(t, reg, app)

	// Baselines differ from the current content: both env and watch drifted.
	app.LastAppliedEnvHash = "stale-hash"
	app.LastAppliedEnvPaths = encodeEnvPathHashes(map[string]string{envFile: "old-hash"})
	app.LastAppliedWatchHash = encodeWatchHashes(map[string]string{"templates/backend/nginx/default.conf": "old-watch-hash"})

	exec := &fakePlanRuntime{
		watchHashes: map[string]string{"templates/backend/nginx/default.conf": "new-watch-hash"},
	}
	r := NewReconciler(reg, exec, nil, ReconcilerConfig{})

	plan, err := r.PlanApp(context.Background(), app, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("nil plan")
	}

	if len(exec.gotInput.EnvChangedPaths) != 1 || exec.gotInput.EnvChangedPaths[0] != envFile {
		t.Fatalf("EnvChangedPaths=%v, want [%s]", exec.gotInput.EnvChangedPaths, envFile)
	}
	if len(exec.gotInput.WatchChanged) != 1 {
		t.Fatalf("WatchChanged=%v, want one entry", exec.gotInput.WatchChanged)
	}
	w := exec.gotInput.WatchChanged[0]
	if w.Path != "templates/backend/nginx/default.conf" || w.Action != "restart" || w.Service != "nginx" {
		t.Fatalf("WatchChanged[0]=%+v", w)
	}
	if exec.gotOpts.Project != app.ComposeProject() {
		t.Fatalf("Project=%q, want %q", exec.gotOpts.Project, app.ComposeProject())
	}
}

func TestReconcilerPlanAppWatchReloadMapping(t *testing.T) {
	cases := []struct {
		name string
		w    deploypolicy.WatchFile
		want string
	}{
		{"reload command", deploypolicy.WatchFile{Action: "reload", ReloadCommand: "kill -HUP 1", Service: "nginx"}, "reload"},
		{"reload url", deploypolicy.WatchFile{Action: "reload", ReloadURL: "http://x/-/reload"}, "reload"},
		{"reload with only service restarts", deploypolicy.WatchFile{Action: "reload", Service: "nginx"}, "restart"},
		{"default restarts", deploypolicy.WatchFile{Service: "nginx"}, "restart"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := watchPlanAction(tc.w); got != tc.want {
				t.Fatalf("watchPlanAction=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestReconcilerPlanAppFleetRefRequiresTemplate(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{Name: "plain", RepoURL: "https://example/repo.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)

	r := NewReconciler(reg, &fakePlanRuntime{}, nil, ReconcilerConfig{})
	if _, err := r.PlanApp(context.Background(), app, "abc123"); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("err=%v, want a template requirement error", err)
	}
}

func TestReconcilerPlanAppNoRuntime(t *testing.T) {
	r := &Reconciler{}
	if _, err := r.PlanApp(context.Background(), &App{Name: "x"}, ""); err == nil {
		t.Fatal("want an error without an executor")
	}
}
