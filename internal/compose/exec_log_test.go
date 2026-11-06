package compose

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerPSHealthTimeoutIsNotLoggedAsError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &ComposeExecutor{Logger: logger}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := e.execProcess(ctx, bin, []string{"ps", "-a"}, dir, nil, nil)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("health timeout did not stop docker ps, ran %s", time.Since(started))
	}
	if err == nil {
		t.Fatal("expected the health timeout to stop docker ps")
	}
	logText := buf.String()
	if strings.Contains(logText, "ERROR") {
		t.Fatalf("health timeout logged ERROR:\n%s", logText)
	}
	if strings.Contains(logText, "Command failed") || strings.Contains(logText, "Command context ended") {
		t.Fatalf("health timeout logged command failure:\n%s", logText)
	}
}

func TestComposeConfigOutputIsNotLogged(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' 'HASURA_GRAPHQL_ADMIN_SECRET: super-secret-value'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &ComposeExecutor{Logger: logger}
	out, err := e.execProcess(context.Background(), bin, []string{"compose", "-p", "dev", "-f", "compose.yml", "config", "--format", "json"}, dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "super-secret-value") {
		t.Fatalf("caller still needs the config text, got %q", out)
	}
	if strings.Contains(buf.String(), "super-secret-value") || strings.Contains(buf.String(), "Command output") {
		t.Fatalf("compose config leaked into the log:\n%s", buf.String())
	}
}

func TestImageInspectMissIsDebug(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\necho 'Error: No such image: app:missing' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e := &ComposeExecutor{Logger: logger}
	_, err := e.execProcess(context.Background(), bin, []string{"image", "inspect", "app:missing"}, dir, nil, nil)
	if err == nil {
		t.Fatal("expected inspect to fail")
	}
	if strings.Contains(buf.String(), "ERROR") || strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("missing image inspect logged ERROR:\n%s", buf.String())
	}
}

func TestDockerPSRealFailureStillLogged(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e := &ComposeExecutor{Logger: logger}

	_, err := e.execProcess(context.Background(), bin, []string{"ps"}, dir, nil, nil)
	if err == nil {
		t.Fatal("expected docker ps to fail")
	}
	if !strings.Contains(buf.String(), "ERROR") || !strings.Contains(buf.String(), "Command failed") {
		t.Fatalf("real failure was not logged:\n%s", buf.String())
	}
}
