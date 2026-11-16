package controller

import (
	"strings"
	"time"

	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/metrics"
)

type envDebounceState struct {
	firstSeen  time.Time
	lastChange time.Time
	lastHash   string
}

func (r *Reconciler) envDebounceWindow(app *App) (debounce, maxWait time.Duration) {
	debounce = 30 * time.Second
	maxWait = 5 * time.Minute
	if raw := strings.TrimSpace(envcompat.Getenv("TIDE_ENV_DEBOUNCE")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			debounce = d
		}
	}
	if raw := strings.TrimSpace(envcompat.Getenv("TIDE_ENV_DEBOUNCE_MAX")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			maxWait = d
		}
	}
	if app != nil {
		pol := app.Policy()
		if raw := strings.TrimSpace(pol.EnvDebounce); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				debounce = d
			}
		}
		if raw := strings.TrimSpace(pol.EnvDebounceMax); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				maxWait = d
			}
		}
	}
	// Max wait is a hard ceiling from first observation; never raise it to
	// the stability window (operators may set a short max for tests/rollouts).
	if debounce > maxWait {
		debounce = maxWait
	}
	return debounce, maxWait
}

// envDebounceReady returns true when the observed env hash has been stable for
// the debounce window, or the max wait from first observation has elapsed.
func (r *Reconciler) envDebounceReady(app *App, hash string, now time.Time) bool {
	if r == nil || app == nil || hash == "" {
		return true
	}
	debounce, maxWait := r.envDebounceWindow(app)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.envPending == nil {
		r.envPending = map[string]*envDebounceState{}
	}
	st := r.envPending[app.ID]
	if st == nil || st.lastHash != hash {
		first := now
		if st != nil {
			first = st.firstSeen
		}
		r.envPending[app.ID] = &envDebounceState{
			firstSeen:  first,
			lastChange: now,
			lastHash:   hash,
		}
		metrics.SetEnvDebouncePending(app.Name, true)
		if now.Sub(first) >= maxWait {
			delete(r.envPending, app.ID)
			metrics.SetEnvDebouncePending(app.Name, false)
			return true
		}
		return false
	}
	if now.Sub(st.lastChange) >= debounce || now.Sub(st.firstSeen) >= maxWait {
		delete(r.envPending, app.ID)
		metrics.SetEnvDebouncePending(app.Name, false)
		return true
	}
	metrics.SetEnvDebouncePending(app.Name, true)
	return false
}

func (r *Reconciler) clearEnvDebounce(appID, appName string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.envPending != nil {
		delete(r.envPending, appID)
	}
	if appName != "" {
		metrics.SetEnvDebouncePending(appName, false)
	}
}
