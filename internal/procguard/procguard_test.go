package procguard

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestSecondProcessCannotAcquireLock(t *testing.T) {
	if os.Getenv("TIDE_PROCGUARD_HELPER") == "1" {
		dir := os.Getenv("TIDE_PROCGUARD_DIR")
		lock, err := Acquire(dir)
		if err != nil {
			os.Exit(42)
		}
		_ = lock.Release()
		os.Exit(0)
	}

	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer first.Release()

	// In-process second open must also fail on OSes with per-fd flock/LockFileEx.
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondProcessCannotAcquireLock$")
	cmd.Env = append(os.Environ(), "TIDE_PROCGUARD_HELPER=1", "TIDE_PROCGUARD_DIR="+dir)
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("expected child process to fail with exit 42 (lock held), got err=%v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	after, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	_ = after.Release()
}
