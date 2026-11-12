package tenant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// HoldState is one tenant freeze.
type HoldState struct {
	Tenant      string    `json:"tenant"`
	Reason      string    `json:"reason"` // manual | auto
	Detail      string    `json:"detail,omitempty"`
	Since       time.Time `json:"since"`
	Manual      bool      `json:"manual"`
	HoldingApps []string  `json:"holding_apps,omitempty"` // apps that keep the auto hold active
	Stopped     []string  `json:"stopped,omitempty"`      // apps stopped by policy stop
}

// Manager tracks tenant specs and hold/stop state for one host.
type Manager struct {
	mu sync.Mutex
	specs map[string]Spec
	holds map[string]HoldState
	// suppressAuto remembers tenants whose auto hold was released manually
	// until every member is healthy again; then the next failure may re-hold.
	suppressAuto map[string]bool
	critDown     map[string]time.Time // critical app name → first observed unhealthy
	path         string
}

// NewManager loads optional persisted holds from dataDir.
func NewManager(dataDir string) *Manager {
	m := &Manager{
		specs:        map[string]Spec{},
		holds:        map[string]HoldState{},
		suppressAuto: map[string]bool{},
		critDown:     map[string]time.Time{},
		path:         filepath.Join(strings.TrimSpace(dataDir), "tenant_holds.json"),
	}
	_ = m.load()
	return m
}

// UpdateSpecs replaces tenant specs from the latest host document.
func (m *Manager) UpdateSpecs(specs map[string]Spec) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.specs = map[string]Spec{}
	for name, spec := range specs {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		spec.Name = name
		spec.FailurePolicy = NormalizePolicy(spec.FailurePolicy)
		m.specs[name] = spec
	}
}

// Spec returns a copy of the tenant spec (empty isolate defaults when unknown).
func (m *Manager) Spec(name string) Spec {
	name = strings.TrimSpace(name)
	if m == nil || name == "" {
		return Spec{FailurePolicy: PolicyIsolate}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if spec, ok := m.specs[name]; ok {
		return spec
	}
	return Spec{Name: name, FailurePolicy: PolicyIsolate}
}

// ManualHold freezes a tenant until Release.
func (m *Manager) ManualHold(name, detail string) HoldState {
	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.suppressAuto, name)
	st := HoldState{
		Tenant: name,
		Reason: ReasonManual,
		Detail: strings.TrimSpace(detail),
		Since:  time.Now().UTC(),
		Manual: true,
	}
	m.holds[name] = st
	_ = m.saveLocked()
	return st
}

// Release clears a hold. Manual holds stay clear until the next tidectl hold.
// Auto holds stay clear until every member is healthy, then the next failure
// may re-arm hold (not on the very next Evaluate while failures remain).
func (m *Manager) Release(name string) {
	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.holds[name]; ok && !st.Manual {
		m.suppressAuto[name] = true
	}
	delete(m.holds, name)
	_ = m.saveLocked()
}

// Hold returns the current hold, if any.
func (m *Manager) Hold(name string) (HoldState, bool) {
	name = strings.TrimSpace(name)
	if m == nil || name == "" {
		return HoldState{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.holds[name]
	if ok {
		st.Reason = NormalizeHoldReason(st.Reason, st.Manual)
	}
	return st, ok
}

// ListHolds returns all current holds sorted by tenant name.
func (m *Manager) ListHolds() []HoldState {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HoldState, 0, len(m.holds))
	for _, st := range m.holds {
		st.Reason = NormalizeHoldReason(st.Reason, st.Manual)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}

// Evaluate updates automatic hold/stop state from the latest app snapshots.
// Returns apps that should be stopped (compose down) under policy stop, and
// tenants whose auto hold just cleared (so deferred updates can be applied).
func (m *Manager) Evaluate(apps []AppView) (toStop []string, cleared []string) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	byTenant := map[string][]AppView{}
	for _, app := range apps {
		t := strings.TrimSpace(app.Tenant)
		if t == "" {
			continue
		}
		byTenant[t] = append(byTenant[t], app)
	}

	now := time.Now().UTC()
	for tenantName, members := range byTenant {
		spec := m.specs[tenantName]
		if spec.Name == "" {
			spec = Spec{Name: tenantName, FailurePolicy: PolicyIsolate}
		}
		policy := NormalizePolicy(spec.FailurePolicy)
		for _, app := range members {
			if EffectivePolicy(app, spec) == PolicyHold {
				policy = PolicyHold
				break
			}
		}

		existing, held := m.holds[tenantName]
		if held && existing.Manual {
			continue // manual holds stick until release
		}

		holding := holdingApps(members)
		anyFailed := len(holding) > 0
		if !anyFailed {
			delete(m.suppressAuto, tenantName)
		}

		switch policy {
		case PolicyHold:
			if anyFailed {
				if m.suppressAuto[tenantName] {
					if held && !existing.Manual {
						delete(m.holds, tenantName)
						cleared = append(cleared, tenantName)
					}
					continue
				}
				m.holds[tenantName] = HoldState{
					Tenant:      tenantName,
					Reason:      ReasonAuto,
					Detail:      "tenant held: at least one app failed reconcile",
					Since:       holdSince(existing, held, now),
					Manual:      false,
					HoldingApps: holding,
				}
			} else if held && !existing.Manual {
				delete(m.holds, tenantName)
				cleared = append(cleared, tenantName)
			}
		case PolicyStop:
			var criticalDown []AppView
			for _, app := range members {
				if !app.Critical {
					continue
				}
				if appHealthy(app) {
					delete(m.critDown, app.Name)
					continue
				}
				if _, ok := m.critDown[app.Name]; !ok {
					m.critDown[app.Name] = now
				}
				grace := spec.CriticalGrace
				if grace <= 0 {
					grace = 5 * time.Minute
				}
				if now.Sub(m.critDown[app.Name]) >= grace {
					criticalDown = append(criticalDown, app)
				}
			}
			if len(criticalDown) > 0 {
				if m.suppressAuto[tenantName] {
					continue
				}
				var stopped []string
				for _, app := range members {
					if app.Critical {
						continue
					}
					stopped = append(stopped, app.Name)
					toStop = append(toStop, app.Name)
				}
				sort.Strings(stopped)
				m.holds[tenantName] = HoldState{
					Tenant:      tenantName,
					Reason:      ReasonAuto,
					Detail:      "critical app unhealthy past grace; siblings stopped",
					Since:       holdSince(existing, held, now),
					Manual:      false,
					HoldingApps: holdingApps(criticalDown),
					Stopped:     stopped,
				}
			} else if held && !existing.Manual {
				delete(m.holds, tenantName)
				cleared = append(cleared, tenantName)
			}
		default: // isolate
			if held && !existing.Manual {
				delete(m.holds, tenantName)
				cleared = append(cleared, tenantName)
			}
		}
	}
	_ = m.saveLocked()
	return uniqueSorted(toStop), uniqueSorted(cleared)
}

// UpdatesBlocked reports whether a healthy app must not receive a new apply
// while the tenant is held. Failed/pending apps keep reconciling.
func (m *Manager) UpdatesBlocked(app AppView) (blocked bool, reason string) {
	t := strings.TrimSpace(app.Tenant)
	if t == "" || m == nil {
		return false, ""
	}
	if NeedsReconcileDespiteHold(app) {
		return false, ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.holds[t]
	if !ok {
		return false, ""
	}
	return true, NormalizeHoldReason(st.Reason, st.Manual)
}

// DependsReady reports whether all depends_on registrations are synced.
func DependsReady(app AppView, byName map[string]AppView) bool {
	for _, dep := range app.DependsOn {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		other, ok := byName[dep]
		if !ok {
			return false
		}
		if other.Status != "synced" && other.Status != "rolled_back" {
			return false
		}
	}
	return true
}

func holdingApps(members []AppView) []string {
	var out []string
	for _, app := range members {
		if appFailedReconcile(app) {
			out = append(out, app.Name)
		}
	}
	sort.Strings(out)
	return out
}

func appFailedReconcile(app AppView) bool {
	switch app.Status {
	case "failed", "error", "image_not_found":
		return true
	case "pending":
		// pending with a sync error (e.g. image_not_found) still holds the tenant.
		return strings.TrimSpace(app.LastSyncError) != ""
	default:
		return false
	}
}

func appHealthy(app AppView) bool {
	switch app.Status {
	case "synced", "rolled_back":
		return true
	default:
		return false
	}
}

func holdSince(existing HoldState, held bool, now time.Time) time.Time {
	if held && !existing.Since.IsZero() {
		return existing.Since
	}
	return now
}

func uniqueSorted(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

type holdFile struct {
	Holds        []HoldState    `json:"holds"`
	SuppressAuto map[string]bool `json:"suppress_auto,omitempty"`
}

func (m *Manager) load() error {
	if m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	var doc holdFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	for _, st := range doc.Holds {
		name := strings.TrimSpace(st.Tenant)
		if name == "" {
			continue
		}
		st.Reason = NormalizeHoldReason(st.Reason, st.Manual)
		m.holds[name] = st
	}
	for name, v := range doc.SuppressAuto {
		if v {
			m.suppressAuto[strings.TrimSpace(name)] = true
		}
	}
	return nil
}

func (m *Manager) saveLocked() error {
	if m.path == "" {
		return nil
	}
	doc := holdFile{
		Holds:        make([]HoldState, 0, len(m.holds)),
		SuppressAuto: map[string]bool{},
	}
	for _, st := range m.holds {
		st.Reason = NormalizeHoldReason(st.Reason, st.Manual)
		doc.Holds = append(doc.Holds, st)
	}
	sort.Slice(doc.Holds, func(i, j int) bool { return doc.Holds[i].Tenant < doc.Holds[j].Tenant })
	for name, v := range m.suppressAuto {
		if v {
			doc.SuppressAuto[name] = true
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0600)
}
