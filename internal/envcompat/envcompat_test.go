package envcompat

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetenvPrefersTideAndWarnsOnLegacy(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	t.Setenv("TIDE_API_ADDR", "")
	t.Setenv("CONOPS_API_ADDR", ":9443")

	if got := Getenv("TIDE_API_ADDR"); got != ":9443" {
		t.Fatalf("got %q, want :9443", got)
	}
	if !strings.Contains(buf.String(), "CONOPS_API_ADDR") || !strings.Contains(buf.String(), "TIDE_API_ADDR") {
		t.Fatalf("expected deprecation warning, got %q", buf.String())
	}

	buf.Reset()
	if got := Getenv("TIDE_API_ADDR"); got != ":9443" {
		t.Fatalf("got %q", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected warning once per key, got %q", buf.String())
	}

	ResetForTest()
	SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Setenv("TIDE_API_ADDR", ":8443")
	if got := Getenv("TIDE_API_ADDR"); got != ":8443" {
		t.Fatalf("got %q, want :8443", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("new variable must not warn, got %q", buf.String())
	}
}

func TestEarlyWarningsFlushWhenLoggerSet(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	t.Setenv("TIDE_FLEET_HOST", "")
	t.Setenv("CONOPS_FLEET_HOST", "dev.example")
	if got := Getenv("TIDE_FLEET_HOST"); got != "dev.example" {
		t.Fatalf("got %q", got)
	}

	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	if !strings.Contains(buf.String(), "CONOPS_FLEET_HOST") {
		t.Fatalf("queued warning was not flushed: %q", buf.String())
	}
}

func TestCheckBinaryAlias(t *testing.T) {
	var errBuf bytes.Buffer
	if !CheckBinaryAlias("/usr/local/bin/cps", &errBuf) {
		t.Fatal("expected cps alias to be detected")
	}
	if !strings.Contains(errBuf.String(), "tidectl") {
		t.Fatalf("unexpected stderr: %q", errBuf.String())
	}
	errBuf.Reset()
	if CheckBinaryAlias("/usr/local/bin/tidectl", &errBuf) || errBuf.Len() != 0 {
		t.Fatalf("tidectl should not warn: %q", errBuf.String())
	}
}

func TestMigrateDataFiles(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	dir := t.TempDir()
	oldDB := filepath.Join(dir, legacyDBName)
	oldKey := filepath.Join(dir, legacyKeyFileName)
	if err := os.WriteFile(oldDB, []byte("sqlite-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldKey, []byte("0123456789abcdef0123456789abcdef"), 0600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	if err := MigrateDataFiles(dir); err != nil {
		t.Fatal(err)
	}

	gotDB, err := os.ReadFile(filepath.Join(dir, NewDBName))
	if err != nil || string(gotDB) != "sqlite-bytes" {
		t.Fatalf("migrated db: %q err=%v", gotDB, err)
	}
	gotKey, err := os.ReadFile(filepath.Join(dir, NewKeyFileName))
	if err != nil || string(gotKey) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("migrated key: %q err=%v", gotKey, err)
	}
	if !strings.Contains(buf.String(), "Migrated legacy SQLite database") {
		t.Fatalf("expected migration log, got %q", buf.String())
	}
}
