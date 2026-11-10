package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/procguard"
	"github.com/valthaide/tide/internal/store"
)

func TestHelpVersionAndUnknownFlagsDoNotTouchState(t *testing.T) {
	emptyDir := filepath.Join(t.TempDir(), "untouched")

	var out, errBuf bytes.Buffer
	res := parseServerArgs([]string{"--help"}, "v2.0.0", &out, &errBuf)
	if !res.Exit || res.Code != 0 || !strings.Contains(out.String(), "Tide") {
		t.Fatalf("--help: %+v out=%q", res, out.String())
	}
	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Fatalf("directory should not be created on --help")
	}

	out.Reset()
	errBuf.Reset()
	res = parseServerArgs([]string{"--version"}, "v2.0.0", &out, &errBuf)
	if !res.Exit || res.Code != 0 || strings.TrimSpace(out.String()) != "tide v2.0.0" {
		t.Fatalf("--version: %+v out=%q", res, out.String())
	}

	out.Reset()
	errBuf.Reset()
	res = parseServerArgs([]string{"--bogus-flag"}, "v2.0.0", &out, &errBuf)
	if !res.Exit || res.Code != 2 || !strings.Contains(errBuf.String(), "unknown flag") {
		t.Fatalf("unknown flag: %+v err=%q", res, errBuf.String())
	}
}

func TestSecondProcessExitsBeforeResettingSyncingApps(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "tide.db")

	db, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	app := &api.App{
		ID: "app-syncing-1", Name: "backend", RepoURL: "https://git.example/t/backend.git",
		RepoAuthMethod: "public", Branch: "main", ComposePath: "compose.yaml",
		PollInterval: "30s", Status: "syncing", LastSyncAt: now,
	}
	if err := db.CreateApp(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// First process holds the environment lock.
	firstLock, err := procguard.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer firstLock.Release()

	// Second startup attempt must fail at lock acquisition before touching the DB.
	var errBuf bytes.Buffer
	code := runController([]string{"--data-dir", dataDir}, &bytes.Buffer{}, &errBuf)
	if code == 0 {
		t.Fatal("expected second controller instance to fail when lock is held")
	}
	if !strings.Contains(errBuf.String(), "already held") {
		t.Fatalf("expected clear lock error, got: %s", errBuf.String())
	}

	_ = firstLock.Release()

	dbAfter, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dbAfter.Close()
	got, err := dbAfter.GetApp(t.Context(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "syncing" {
		t.Fatalf("second process mutated app status from syncing to %q", got.Status)
	}
}
