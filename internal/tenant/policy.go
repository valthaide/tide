package tenant

import (
	"strings"
	"time"
)

// Failure policies for a tenant (or per-app override).
const (
	PolicyIsolate = "isolate" // default: one app failure stays local
	PolicyHold    = "hold"    // freeze updates for the whole tenant
	PolicyStop    = "stop"    // stop siblings when a critical app is down long enough
)

// NormalizePolicy returns isolate/hold/stop; empty becomes isolate.
func NormalizePolicy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case PolicyHold:
		return PolicyHold
	case PolicyStop:
		return PolicyStop
	default:
		return PolicyIsolate
	}
}

// Spec is the tenant block from the host document (runtime view).
type Spec struct {
	Name            string
	FailurePolicy   string
	CriticalGrace   time.Duration
	CPULimit        string
	MemoryLimit     string
	ManualHold      bool
	ManualHoldReason string
}

// AppView is the subset of app state the tenant manager needs.
type AppView struct {
	Name           string
	Tenant         string
	Status         string
	Critical       bool
	FailurePolicy  string // optional per-app override
	DependsOn      []string
	CPULimit       string
	MemoryLimit    string
	LastSyncError  string
	LastSyncAt     time.Time
	LastHealthyAt  time.Time
}

// EffectivePolicy resolves app override then tenant default then isolate.
func EffectivePolicy(app AppView, spec Spec) string {
	if p := NormalizePolicy(app.FailurePolicy); app.FailurePolicy != "" {
		return p
	}
	return NormalizePolicy(spec.FailurePolicy)
}
