package controller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/syncerr"
)

func TestShouldAdoptEnvHashFirstAndPathOnly(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("A=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("B=2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hashA, err := compose.HashEnvFilePaths([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	hashAB, err := compose.HashEnvFilePaths([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}

	app := &api.App{}
	if !shouldAdoptEnvHash(app, envHashResult{Hash: hashA, Paths: []string{a}}) {
		t.Fatal("empty last hash should adopt")
	}

	app.LastAppliedEnvHash = hashA
	app.LastAppliedEnvPaths = encodeEnvPaths([]string{a})
	if !shouldAdoptEnvHash(app, envHashResult{Hash: hashAB, Paths: []string{a, b}}) {
		t.Fatal("path-only addition should adopt")
	}

	if err := os.WriteFile(a, []byte("A=changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hashAChangedAB, err := compose.HashEnvFilePaths([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if shouldAdoptEnvHash(app, envHashResult{Hash: hashAChangedAB, Paths: []string{a, b}}) {
		t.Fatal("content change must not adopt")
	}
}

func TestCheckRequiredEnv(t *testing.T) {
	err := checkRequiredEnv([]string{"FOO", "BAR"}, map[string]string{"FOO": "1", "BAR": "2"})
	if err != nil {
		t.Fatal(err)
	}
	err = checkRequiredEnv([]string{"FOO", "BAR"}, map[string]string{"FOO": "1"})
	if err == nil || syncerr.Classify(err).Code != syncerr.CodeEnvKeyMissing {
		t.Fatalf("expected env_key_missing, got %v", err)
	}
	err = checkRequiredEnv([]string{"FOO"}, map[string]string{"FOO": "  "})
	if err == nil {
		t.Fatal("empty value")
	}
	err = checkRequiredEnv([]string{"FOO"}, map[string]string{"FOO": "__INVALID__"})
	if err == nil {
		t.Fatal("invalid marker")
	}
	// Fleet vars must not satisfy required_env — only fileVars are passed in.
	err = checkRequiredEnv([]string{"SECRET"}, map[string]string{})
	if err == nil {
		t.Fatal("missing key")
	}
}

func TestEnvDebounceSeriesThenReady(t *testing.T) {
	rec := &Reconciler{envPending: map[string]*envDebounceState{}}
	app := &api.App{ID: "1", Name: "demo"}
	app.DeployPolicyJSON = deploypolicy.Encode(deploypolicy.Policy{
		EnvDebounce:    "100ms",
		EnvDebounceMax: "500ms",
	})
	now := time.Now()
	if rec.envDebounceReady(app, "h1", now) {
		t.Fatal("first observation must wait")
	}
	if rec.envDebounceReady(app, "h2", now.Add(50*time.Millisecond)) {
		t.Fatal("hash change resets stability window")
	}
	if rec.envDebounceReady(app, "h2", now.Add(120*time.Millisecond)) {
		t.Fatal("not yet stable for debounce from last change")
	}
	// lastChange was at now+50ms; debounce 100ms => ready at now+150ms.
	if !rec.envDebounceReady(app, "h2", now.Add(160*time.Millisecond)) {
		t.Fatal("expected ready after stable window")
	}
}

func TestEnvDebounceMaxWait(t *testing.T) {
	rec := &Reconciler{envPending: map[string]*envDebounceState{}}
	app := &api.App{ID: "1", Name: "demo"}
	app.DeployPolicyJSON = deploypolicy.Encode(deploypolicy.Policy{
		EnvDebounce:    "1h",
		EnvDebounceMax: "200ms",
	})
	now := time.Now()
	if rec.envDebounceReady(app, "h1", now) {
		t.Fatal("first must wait")
	}
	if !rec.envDebounceReady(app, "h2", now.Add(250*time.Millisecond)) {
		t.Fatal("max wait must force ready even while hash keeps changing")
	}
}

func TestRecordAppliedEnvHashAfterSuccessOnlyUsesUnifiedHash(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(envFile, []byte("TOKEN=abcdef12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app := &api.App{
		Name:         "app",
		RepoURL:      "https://example/repo.git",
		Branch:       "main",
		ComposePath:  "compose.yaml",
		PollInterval: "30s",
		Status:       "synced",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{EnvFiles: []string{envFile}}))
	fresh, _ := reg.Get(app.ID)
	rec := NewReconciler(reg, nil, nil, ReconcilerConfig{})
	rec.recordAppliedEnvHash(fresh)
	fresh, _ = reg.Get(app.ID)
	want, err := compose.HashEnvFilePaths([]string{envFile})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastAppliedEnvHash != want {
		t.Fatalf("hash=%q want %q", fresh.LastAppliedEnvHash, want)
	}
	if len(decodeEnvPaths(fresh.LastAppliedEnvPaths)) != 1 {
		t.Fatalf("paths=%q", fresh.LastAppliedEnvPaths)
	}
}
