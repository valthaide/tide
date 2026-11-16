package compose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valthaide/tide/internal/syncerr"
)

func TestHashEnvFilePathsOrderAndDedup(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("A=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("B=2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h1, err := HashEnvFilePaths([]string{b, a, a, b})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashEnvFilePaths([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || h1 == "" {
		t.Fatalf("expected stable deduped hash, got %q vs %q", h1, h2)
	}
}

func TestHashEnvFilePathsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := HashEnvFilePaths([]string{p})
	if err == nil {
		t.Fatal("expected empty file error")
	}
	se := syncerr.Classify(err)
	if se.Code != syncerr.CodeEnvFileEmpty {
		t.Fatalf("code=%s want env_file_empty", se.Code)
	}
}

func TestHashEnvFilePathsMissing(t *testing.T) {
	_, err := HashEnvFilePaths([]string{filepath.Join(t.TempDir(), "missing.env")})
	if err == nil {
		t.Fatal("expected missing")
	}
	se := syncerr.Classify(err)
	if se.Code != syncerr.CodeEnvFileMissing {
		t.Fatalf("code=%s", se.Code)
	}
}

func TestExternalEnvHashFilesCombined(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "secrets.env")
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("K=v\n"), 0600); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(appDir, "compose.yaml")
	// Relative env_file outside projectDir is treated as external.
	content := "services:\n  api:\n    image: x\n    env_file:\n      - ../secrets.env\n"
	if err := os.WriteFile(composePath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := ExternalEnvHashFiles(appDir, []string{composePath})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := HashEnvFilePaths([]string{env})
	if err != nil {
		t.Fatal(err)
	}
	if hash != direct {
		t.Fatalf("compose hash %q != path hash %q", hash, direct)
	}
}
