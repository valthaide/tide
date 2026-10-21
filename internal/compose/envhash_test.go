package compose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valthaide/conops-private/internal/syncerr"
)

func TestExternalEnvHash(t *testing.T) {
	repo := t.TempDir()
	envDir := t.TempDir()
	envPath := filepath.Join(envDir, "app.env")
	if err := os.WriteFile(envPath, []byte("FOO=bar\n"), 0600); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: nginx\n    env_file:\n      - " + envPath + "\n"
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	h1, err := ExternalEnvHash(repo, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}
	h2, err := ExternalEnvHash(repo, "compose.yaml")
	if err != nil || h2 != h1 {
		t.Fatalf("hash unstable: %v %q %q", err, h1, h2)
	}
	if err := os.WriteFile(envPath, []byte("FOO=baz\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h3, err := ExternalEnvHash(repo, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("expected hash to change when env file changes")
	}
}

func TestExternalEnvHashMissing(t *testing.T) {
	repo := t.TempDir()
	compose := "services:\n  web:\n    image: nginx\n    env_file:\n      - /nonexistent/conops-env-test.env\n"
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ExternalEnvHash(repo, "compose.yaml")
	if err == nil {
		t.Fatal("expected missing env_file error")
	}
	se, ok := err.(*syncerr.Error)
	if !ok || se.Code != syncerr.CodeEnvFileMissing {
		t.Fatalf("expected env_file_missing, got %#v", err)
	}
}
