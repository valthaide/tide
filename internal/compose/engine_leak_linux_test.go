//go:build linux

package compose

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestObserveContainersDoesNotLeakEngineFDs drives 1000 observe cycles
// against a fake engine on a unix socket. With a per-call http.Transport
// each cycle pinned one keep-alive connection forever; the shared client
// must keep the process fd count flat.
func TestObserveContainersDoesNotLeakEngineFDs(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	t.Setenv("DOCKER_HOST", "unix://"+sockPath)

	exec := &ComposeExecutor{}
	ctx := context.Background()
	if _, err := exec.observeContainers(ctx); err != nil {
		t.Fatalf("warmup observe: %v", err)
	}
	before := countProcessFDs(t)
	for i := 0; i < 1000; i++ {
		if _, err := exec.observeContainers(ctx); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}
	after := countProcessFDs(t)
	if after-before > 10 {
		t.Fatalf("engine connections leak: fds before=%d after=%d", before, after)
	}
}

func countProcessFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unavailable: %v", err)
	}
	return len(entries)
}
