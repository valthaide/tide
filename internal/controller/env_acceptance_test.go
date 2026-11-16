package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/syncerr"
)

func TestEnvFilesFleetHashSingleRedeployNotLoop(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(envFile, []byte("TOKEN=abcdef12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	app := &api.App{
		Name:             "test-ru",
		RepoURL:          "https://git.example/tenko/test-ru.git",
		Branch:           "main",
		ComposePath:      "compose.yaml",
		PollInterval:     "30s",
		LastSeenCommit:   sha,
		LastSyncedCommit: sha,
		Status:           "synced",
	}
	addPublicApp(t, reg, app)
	pol := deploypolicy.Policy{
		EnvFiles:       []string{envFile},
		EnvDebounce:    "1ms",
		EnvDebounceMax: "50ms",
		Vars:           map[string]string{"APP_TAG": "1"},
	}
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(pol))
	_ = reg.UpdateRollback(app.ID, sha, "", deploypolicy.EncodeVars(pol.Vars), "")
	_ = reg.UpdateStatus(app.ID, "synced", nil)

	rt := &optRuntime{}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{Interval: time.Hour, MaxConcurrent: 1})

	fresh, _ := reg.Get(app.ID)
	// First observation adopts without apply.
	if reason := rec.envFileDriftReason(fresh); reason != "" {
		t.Fatalf("adopt should not redeploy, got %q", reason)
	}
	fresh, _ = reg.Get(app.ID)
	if fresh.LastAppliedEnvHash == "" {
		t.Fatal("expected adopted hash")
	}
	if len(rt.applyCalls) != 0 {
		t.Fatalf("adopt must not apply, got %d", len(rt.applyCalls))
	}

	// Content change → after debounce, one redeploy; hash recorded; no loop.
	if err := os.WriteFile(envFile, []byte("TOKEN=changed99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, _ = reg.Get(app.ID)
	_ = rec.envFileDriftReason(fresh) // start debounce
	time.Sleep(5 * time.Millisecond)
	fresh, _ = reg.Get(app.ID)
	if reason := rec.envFileDriftReason(fresh); reason != "env_file_changed" {
		t.Fatalf("expected env_file_changed after debounce, got %q", reason)
	}
	rec.requeuePending(fresh, "env_file_changed")
	rec.dispatch()
	rec.wg.Wait()

	if len(rt.applyCalls) != 1 {
		t.Fatalf("expected exactly one apply after env change, got %d (%v)", len(rt.applyCalls), rt.applyCalls)
	}
	if len(rt.opts) != 1 || !rt.opts[0].ForceRecreate {
		t.Fatalf("expected ForceRecreate on env change apply, opts=%v", rt.opts)
	}
	fresh, _ = reg.Get(app.ID)
	want, err := compose.HashEnvFilePaths([]string{envFile})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastAppliedEnvHash != want {
		t.Fatalf("applied hash=%q want %q", fresh.LastAppliedEnvHash, want)
	}

	// Subsequent ticks must not re-apply (regression for defect 0.4).
	for i := 0; i < 3; i++ {
		fresh, _ = reg.Get(app.ID)
		if reason := rec.envFileDriftReason(fresh); reason != "" {
			t.Fatalf("tick %d: unexpected drift reason %q", i, reason)
		}
		rec.reconcileOnce()
		rec.wg.Wait()
	}
	if len(rt.applyCalls) != 1 {
		t.Fatalf("env change must not loop applies, got %d", len(rt.applyCalls))
	}
}

func TestRequiredEnvBlocksComposeUp(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(envFile, []byte("OTHER=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const sha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	app := &api.App{
		Name:             "backend",
		RepoURL:          "https://git.example/app.git",
		Branch:           "main",
		ComposePath:      "compose.yaml",
		PollInterval:     "30s",
		LastSeenCommit:   sha,
		LastSyncedCommit: "",
		Status:           "pending",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{
		EnvFiles:    []string{envFile},
		RequiredEnv: []string{"HASURA_ADMIN_SECRET"},
		// Secret present only in fleet vars must NOT satisfy required_env.
		Vars: map[string]string{"HASURA_ADMIN_SECRET": "from-git"},
	}))

	rt := &optRuntime{}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{Interval: time.Hour, MaxConcurrent: 1})
	fresh, _ := reg.Get(app.ID)
	if err := rec.syncApp(fresh); err != nil {
		t.Fatalf("syncApp should declareOnce without returning hard error, got %v", err)
	}
	if len(rt.applyCalls) != 0 {
		t.Fatalf("compose up must not run when required_env missing, applies=%v", rt.applyCalls)
	}
	fresh, _ = reg.Get(app.ID)
	if fresh.LastSyncErrorCode != syncerr.CodeEnvKeyMissing && !strings.Contains(fresh.LastSyncError, syncerr.CodeEnvKeyMissing) {
		t.Fatalf("expected env_key_missing, status=%s code=%s err=%s", fresh.Status, fresh.LastSyncErrorCode, fresh.LastSyncError)
	}
	if fresh.LastAppliedEnvHash != "" {
		t.Fatal("hash must not change when required_env fails")
	}
}

func TestEnvFilesAdoptOnExistingAppZeroApply(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, "config.env")
	if err := os.WriteFile(envFile, []byte("X=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	app := &api.App{
		Name:             "existing",
		RepoURL:          "https://git.example/app.git",
		Branch:           "main",
		ComposePath:      "compose.yaml",
		PollInterval:     "30s",
		LastSeenCommit:   sha,
		LastSyncedCommit: sha,
		Status:           "synced",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateRollback(app.ID, sha, "", "", "")
	_ = reg.UpdateStatus(app.ID, "synced", nil)
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{
		EnvFiles: []string{envFile},
	}))

	rt := &optRuntime{}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{Interval: time.Hour, MaxConcurrent: 1})
	fresh, _ := reg.Get(app.ID)
	if reason := rec.envFileDriftReason(fresh); reason != "" {
		t.Fatalf("new env_files on existing app must adopt, got %q", reason)
	}
	rec.reconcileOnce()
	rec.wg.Wait()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("adopt must be 0 applies, got %d", len(rt.applyCalls))
	}
}
