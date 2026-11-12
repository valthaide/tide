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

// Metric/status reason labels for tide_tenant_held.
const (
	ReasonManual = "manual"
	ReasonAuto   = "auto"
	// Legacy aliases kept for persisted hold files from v2.4.0.
	ReasonPolicyHold = "policy_hold"
	ReasonPolicyStop = "policy_stop"
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

// NormalizeHoldReason maps persisted/legacy reasons to manual|auto.
func NormalizeHoldReason(raw string, manual bool) string {
	if manual {
		return ReasonManual
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ReasonManual:
		return ReasonManual
	case ReasonAuto, ReasonPolicyHold, ReasonPolicyStop, "":
		return ReasonAuto
	default:
		return ReasonAuto
	}
}

// Spec is the tenant block from the host document (runtime view).
type Spec struct {
	Name             string
	FailurePolicy    string
	CriticalGrace    time.Duration
	CPULimit         string
	MemoryLimit      string
	ManualHold       bool
	ManualHoldReason string
}

// AppView is the subset of app state the tenant manager needs.
type AppView struct {
	Name          string
	Tenant        string
	Status        string
	Critical      bool
	FailurePolicy string // optional per-app override
	DependsOn     []string
	CPULimit      string
	MemoryLimit   string
	LastSyncError string
	LastSyncAt    time.Time
	LastHealthyAt time.Time
}

// EffectivePolicy resolves app override then tenant default then isolate.
func EffectivePolicy(app AppView, spec Spec) string {
	if strings.TrimSpace(app.FailurePolicy) != "" {
		return NormalizePolicy(app.FailurePolicy)
	}
	return NormalizePolicy(spec.FailurePolicy)
}

// NeedsReconcileDespiteHold is true for failed/pending apps that must keep
// syncing while the tenant is frozen (spec change or image appearing).
func NeedsReconcileDespiteHold(app AppView) bool {
	switch strings.TrimSpace(app.Status) {
	case "failed", "error", "image_not_found", "pending", "syncing", "verifying":
		return true
	default:
		return false
	}
}
