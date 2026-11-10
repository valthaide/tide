package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverCAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(path, []byte("cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIDE_CA_FILE", path)
	t.Setenv("TIDECTL_CA_FILE", "")
	if got := discoverCAFile(); got != path {
		t.Fatalf("got %q", got)
	}
}
