package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/tenant"
)

func (r *Reconciler) evaluateTenants(apps []*App) {
	if r == nil || r.Tenants == nil {
		metrics.SetTenantHolds(nil)
		return
	}
	views := make([]tenant.AppView, 0, len(apps))
	for _, app := range apps {
		if app == nil {
			continue
		}
		views = append(views, appView(app))
	}
	toStop := r.Tenants.Evaluate(views)
	holds := r.Tenants.ListHolds()
	samples := make([]metrics.TenantHoldSample, 0, len(holds))
	for _, st := range holds {
		samples = append(samples, metrics.TenantHoldSample{Tenant: st.Tenant, Reason: st.Reason})
	}
	metrics.SetTenantHolds(samples)

	// When a policy_stop hold clears, requeue previously stopped siblings.
	if r.prevTenantHolds == nil {
		r.prevTenantHolds = map[string]tenant.HoldState{}
	}
	current := map[string]tenant.HoldState{}
	for _, st := range holds {
		current[st.Tenant] = st
	}
	for name, prev := range r.prevTenantHolds {
		if prev.Reason != tenant.ReasonPolicyStop {
			continue
		}
		if _, still := current[name]; still {
			continue
		}
		for _, appName := range prev.Stopped {
			if existing := r.Registry.FindByName(appName); existing != nil {
				_ = r.Registry.MarkPending(existing.ID)
				if r.Logger != nil {
					r.Logger.Info("Critical app recovered; requeueing stopped sibling", "tenant", name, "app", appName)
				}
			}
		}
	}
	r.prevTenantHolds = current

	type stopper interface {
		Destroy(ctx context.Context, appID, composePath string, envVars map[string]string, pruneVolumes bool) (string, error)
	}
	stop, _ := r.Executor.(stopper)
	for _, appName := range toStop {
		app := r.Registry.FindByName(appName)
		if app == nil || stop == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		_, err := stop.Destroy(ctx, app.ID, app.ComposePath, nil, false)
		cancel()
		if err != nil && r.Logger != nil {
			r.Logger.Error("Tenant stop policy failed to stop app", "app", appName, "error", err)
			continue
		}
		_ = r.Registry.UpdateStatus(app.ID, "failed", nil)
		if r.Logger != nil {
			r.Logger.Warn("Stopped app due to tenant stop policy", "app", appName)
		}
	}
}

func (r *Reconciler) tenantBlocksApply(app *App) (bool, string) {
	if r == nil || r.Tenants == nil || app == nil {
		return false, ""
	}
	return r.Tenants.UpdatesBlocked(appView(app))
}

func (r *Reconciler) dependsReady(app *App) bool {
	if app == nil {
		return true
	}
	deps := app.Policy().DependsOn
	if len(deps) == 0 || r.Registry == nil {
		return true
	}
	byName := map[string]tenant.AppView{}
	for _, other := range r.Registry.List() {
		if other == nil {
			continue
		}
		byName[other.Name] = appView(other)
	}
	return tenant.DependsReady(appView(app), byName)
}

func (r *Reconciler) tenantResourceExceeded(app *App) (bool, string) {
	if r == nil || app == nil || r.Registry == nil {
		return false, ""
	}
	policy := app.Policy()
	tenantName := strings.TrimSpace(policy.Tenant)
	if tenantName == "" {
		return false, ""
	}
	cpuBudget, cpuOK := parseCPU(policy.TenantCPULimit)
	memBudget, memOK := parseMemoryBytes(policy.TenantMemoryLimit)
	if !cpuOK && !memOK {
		return false, ""
	}
	var cpuSum float64
	var memSum int64
	for _, other := range r.Registry.List() {
		if other == nil || strings.TrimSpace(other.Policy().Tenant) != tenantName {
			continue
		}
		// Count already-running/synced apps plus the app about to apply.
		op := other.Policy()
		if other.ID != app.ID {
			switch other.Status {
			case "synced", "rolled_back", "syncing", "verifying", "pending":
			default:
				continue
			}
		}
		if v, ok := parseCPU(op.CPULimit); ok {
			cpuSum += v
		}
		if v, ok := parseMemoryBytes(op.MemoryLimit); ok {
			memSum += v
		}
	}
	if cpuOK && cpuSum > cpuBudget+1e-9 {
		return true, fmt.Sprintf("tenant_cpu_limit_exceeded: tenant %s budget %s, requested sum %.2f", tenantName, policy.TenantCPULimit, cpuSum)
	}
	if memOK && memSum > memBudget {
		return true, fmt.Sprintf("tenant_memory_limit_exceeded: tenant %s budget %s, requested sum %d bytes", tenantName, policy.TenantMemoryLimit, memSum)
	}
	return false, ""
}

func appView(app *App) tenant.AppView {
	if app == nil {
		return tenant.AppView{}
	}
	p := app.Policy()
	return tenant.AppView{
		Name:          app.Name,
		Tenant:        p.Tenant,
		Status:        app.Status,
		Critical:      p.Critical,
		FailurePolicy: p.FailurePolicy,
		DependsOn:     append([]string(nil), p.DependsOn...),
		CPULimit:      p.CPULimit,
		MemoryLimit:   p.MemoryLimit,
		LastSyncError: app.LastSyncError,
		LastSyncAt:    app.LastSyncAt,
	}
}

func parseCPU(raw string) (float64, bool) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(raw), "c"))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func parseMemoryBytes(raw string) (int64, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return 0, false
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(raw, "ki") || strings.HasSuffix(raw, "kib"):
		mult = 1024
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "kib"), "ki")
	case strings.HasSuffix(raw, "mi") || strings.HasSuffix(raw, "mib"):
		mult = 1024 * 1024
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "mib"), "mi")
	case strings.HasSuffix(raw, "gi") || strings.HasSuffix(raw, "gib"):
		mult = 1024 * 1024 * 1024
		raw = strings.TrimSuffix(strings.TrimSuffix(raw, "gib"), "gi")
	case strings.HasSuffix(raw, "k"):
		mult = 1000
		raw = strings.TrimSuffix(raw, "k")
	case strings.HasSuffix(raw, "m"):
		mult = 1000 * 1000
		raw = strings.TrimSuffix(raw, "m")
	case strings.HasSuffix(raw, "g"):
		mult = 1000 * 1000 * 1000
		raw = strings.TrimSuffix(raw, "g")
	case strings.HasSuffix(raw, "b"):
		raw = strings.TrimSuffix(raw, "b")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return int64(v * float64(mult)), true
}
