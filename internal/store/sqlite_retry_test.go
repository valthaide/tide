package store

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valthaide/conops-private/internal/api"
)

func TestSQLiteLockRetryLogsDebug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.db")
	s, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	app := &api.App{ID: "app", Name: "app", RepoURL: "https://example.com/a.git", Branch: "main", ComposePath: "compose.yaml", Status: "pending"}
	if err := s.CreateApp(context.Background(), app); err != nil {
		t.Fatal(err)
	}

	other, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE apps SET name = name`); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	done := make(chan error, 1)
	go func() {
		done <- s.SetAppRevision(context.Background(), app.ID, "abc")
	}()
	time.Sleep(400 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), "database lock cleared") {
		t.Fatalf("log:\n%s", buf.String())
	}
}
