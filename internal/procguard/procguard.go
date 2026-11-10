package procguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrLocked is returned when another controller process holds the environment lock.
var ErrLocked = errors.New("environment lock is already held by another tide process")

// Lock represents an active inter-process lock on the data directory.
type Lock struct {
	path string
	file *os.File
}

// Acquire takes a non-blocking exclusive inter-process lock on <dataDir>/tide.lock.
// It must be called before opening the database or running startup recovery.
func Acquire(dataDir string) (*Lock, error) {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve lock directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0700); err != nil {
		return nil, fmt.Errorf("prepare lock directory %s: %w", absDir, err)
	}

	lockPath := filepath.Join(absDir, "tide.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	if err := tryLockFile(f); err != nil {
		holder := readHolderHint(f)
		_ = f.Close()
		if holder != "" {
			return nil, fmt.Errorf("%w (%s, holder %s)", ErrLocked, lockPath, holder)
		}
		return nil, fmt.Errorf("%w (%s)", ErrLocked, lockPath)
	}

	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	_ = f.Sync()

	return &Lock{path: lockPath, file: f}, nil
}

func readHolderHint(f *os.File) string {
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n == 0 {
		return ""
	}
	return strings.TrimSpace(string(buf[:n]))
}

// Path returns the path to the underlying lock file.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Release unlocks and closes the lock file descriptor.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	unlockErr := unlockFile(f)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
