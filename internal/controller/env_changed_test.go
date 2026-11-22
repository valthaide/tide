package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/compose"
)

func TestEnvPathHashesRoundtrip(t *testing.T) {
	a := filepath.Clean("/etc/a.env")
	b := filepath.Clean("/etc/b.env")
	per := map[string]string{b: "bbb", a: "aaa"}
	encoded := encodeEnvPathHashes(per)
	decoded := decodeEnvPathHashes(encoded)
	if len(decoded) != 2 || decoded[a] != "aaa" || decoded[b] != "bbb" {
		t.Fatalf("decoded %+v", decoded)
	}
	paths := decodeEnvPaths(encoded)
	if len(paths) != 2 || paths[0] != a || paths[1] != b {
		t.Fatalf("paths %+v", paths)
	}
}

func TestDecodeEnvPathHashesLegacyList(t *testing.T) {
	a := filepath.Clean("/etc/a.env")
	legacy := encodeEnvPaths([]string{"/etc/a.env", "/etc/b.env"})
	if hashes := decodeEnvPathHashes(legacy); hashes != nil {
		t.Fatalf("legacy list must not yield hashes: %+v", hashes)
	}
	paths := decodeEnvPaths(legacy)
	if len(paths) != 2 || paths[0] != a {
		t.Fatalf("paths %+v", paths)
	}
}

func TestChangedEnvPaths(t *testing.T) {
	prev := map[string]string{"/a.env": "1", "/b.env": "2", "/gone.env": "3"}
	cur := map[string]string{"/a.env": "1", "/b.env": "9", "/new.env": "4"}
	changed := changedEnvPaths(prev, cur)
	if len(changed) != 3 || changed[0] != "/b.env" || changed[1] != "/gone.env" || changed[2] != "/new.env" {
		t.Fatalf("changed %+v", changed)
	}
	if changed := changedEnvPaths(prev, prev); len(changed) != 0 {
		t.Fatalf("unchanged %+v", changed)
	}
	if changed := changedEnvPaths(nil, cur); changed != nil {
		t.Fatalf("unknown prev must be nil: %+v", changed)
	}
}

func TestMigrateLegacyEnvPathHashes(t *testing.T) {
	dir := t.TempDir()
	envA := filepath.Join(dir, "a.env")
	envB := filepath.Join(dir, "b.env")
	if err := os.WriteFile(envA, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envB, []byte("B=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := testRegistry(t)
	app := &api.App{Name: "legacy", RepoURL: "https://example/legacy.git", Branch: "main", ComposePath: "compose.yaml"}
	addPublicApp(t, reg, app)
	policy := `{"env_files":["` + strings.ReplaceAll(envA, `\`, `\\`) + `","` + strings.ReplaceAll(envB, `\`, `\\`) + `"]}`
	if err := reg.UpdateDeployPolicy(app.ID, policy); err != nil {
		t.Fatal(err)
	}

	// Pre-v2.9.0 baseline: combined hash matches, paths stored as a plain list.
	legacyHash, _, err := compose.HashEnvFilePathsDetailed([]string{envA, envB})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateEnvHash(app.ID, legacyHash, encodeEnvPaths([]string{envA, envB})); err != nil {
		t.Fatal(err)
	}
	fresh, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}

	r := NewReconciler(reg, &fakePlanRuntime{}, nil, ReconcilerConfig{})
	if reason := r.envFileDriftReason(fresh); reason != "" {
		t.Fatalf("drift=%q, want none for unchanged env files", reason)
	}

	migrated, err := reg.Get(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	prev := decodeEnvPathHashes(migrated.LastAppliedEnvPaths)
	if len(prev) != 2 {
		t.Fatalf("paths after migration %q, want per-path hashes", migrated.LastAppliedEnvPaths)
	}

	// The first real change after the upgrade now scopes to the changed file.
	if err := os.WriteFile(envB, []byte("B=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, perPath, err := compose.HashEnvFilePathsDetailed([]string{envA, envB})
	if err != nil {
		t.Fatal(err)
	}
	changed := changedEnvPaths(prev, perPath)
	if len(changed) != 1 || changed[0] != filepath.Clean(envB) {
		t.Fatalf("changed %+v, want only b.env", changed)
	}
}
