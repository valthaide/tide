package controller

import (
	"testing"
	"time"

	"github.com/valthaide/tide/internal/syncerr"
)

func TestRepeatedErrorBackoffSequence(t *testing.T) {
	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		5 * time.Minute,
		5 * time.Minute,
	}
	for i, w := range want {
		if got := repeatedErrorBackoff(i + 1); got != w {
			t.Fatalf("attempt %d: got %s want %s", i+1, got, w)
		}
	}
}

func TestRepeatErrorPausedSameReasonAndCode(t *testing.T) {
	r := NewReconciler(nil, nil, nil, ReconcilerConfig{})
	app := &App{ID: "a1", Name: "web", LastSyncErrorCode: syncerr.CodeValidation}

	if r.repeatErrorPaused(app, "vars_changed") {
		t.Fatal("first repeat must be allowed")
	}
	if !r.repeatErrorPaused(app, "vars_changed") {
		t.Fatal("immediate second repeat with same reason and code must be paused")
	}
	r.retryMu.Lock()
	st := r.repeatBackoff[app.ID]
	r.retryMu.Unlock()
	if st.attempts != 1 || st.wait != 5*time.Second {
		t.Fatalf("state=%+v want attempts=1 wait=5s", st)
	}

	app.LastSyncErrorCode = syncerr.CodeEnvFileMissing
	if r.repeatErrorPaused(app, "vars_changed") {
		t.Fatal("changed error code must reset the pause")
	}
	r.retryMu.Lock()
	st = r.repeatBackoff[app.ID]
	r.retryMu.Unlock()
	if st.attempts != 1 {
		t.Fatalf("attempts=%d want reset to 1", st.attempts)
	}

	app.LastSyncErrorCode = ""
	if r.repeatErrorPaused(app, "vars_changed") {
		t.Fatal("no error code must not pause")
	}
	r.retryMu.Lock()
	_, ok := r.repeatBackoff[app.ID]
	r.retryMu.Unlock()
	if ok {
		t.Fatal("state must be cleared once the error is gone")
	}
}

func TestRepeatErrorPausedElapsedAllowsRetry(t *testing.T) {
	r := NewReconciler(nil, nil, nil, ReconcilerConfig{})
	app := &App{ID: "a2", Name: "web", LastSyncErrorCode: syncerr.CodeValidation}
	if r.repeatErrorPaused(app, "vars_changed") {
		t.Fatal("first repeat must be allowed")
	}
	r.retryMu.Lock()
	st := r.repeatBackoff[app.ID]
	st.next = time.Now().Add(-time.Second)
	r.repeatBackoff[app.ID] = st
	r.retryMu.Unlock()
	if r.repeatErrorPaused(app, "vars_changed") {
		t.Fatal("repeat after the pause must be allowed")
	}
	r.retryMu.Lock()
	st = r.repeatBackoff[app.ID]
	r.retryMu.Unlock()
	if st.attempts != 2 || st.wait != 10*time.Second {
		t.Fatalf("state=%+v want attempts=2 wait=10s", st)
	}
}
