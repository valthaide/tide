package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/tide/internal/tenant"
)

// HoldTenant freezes updates for one tenant (manual experiment hold).
func (h *Handler) HoldTenant(w http.ResponseWriter, r *http.Request) {
	if h.Tenants == nil {
		http.Error(w, "tenant manager not configured", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimSpace(chi.URLParam(r, "tenant"))
	if name == "" {
		http.Error(w, "tenant is required", http.StatusBadRequest)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	st := h.Tenants.ManualHold(name, body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"message": "held", "data": st})
}

// ReleaseTenant clears a manual or policy hold for one tenant.
func (h *Handler) ReleaseTenant(w http.ResponseWriter, r *http.Request) {
	if h.Tenants == nil {
		http.Error(w, "tenant manager not configured", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimSpace(chi.URLParam(r, "tenant"))
	if name == "" {
		http.Error(w, "tenant is required", http.StatusBadRequest)
		return
	}
	h.Tenants.Release(name)
	// Requeue tenant apps so updates can proceed.
	if h.Registry != nil {
		for _, app := range h.Registry.List() {
			if app == nil || app.Policy().Tenant != name {
				continue
			}
			_ = h.Registry.MarkPending(app.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "released", "tenant": name})
}

// ListTenants returns tenant hold state and member apps.
func (h *Handler) ListTenants(w http.ResponseWriter, r *http.Request) {
	type member struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	type row struct {
		Name   string           `json:"name"`
		Hold   *tenant.HoldState `json:"hold,omitempty"`
		Apps   []member         `json:"apps"`
		Policy string           `json:"failure_policy,omitempty"`
	}
	byTenant := map[string]*row{}
	if h.Registry != nil {
		for _, app := range h.Registry.List() {
			if app == nil {
				continue
			}
			name := strings.TrimSpace(app.Policy().Tenant)
			if name == "" {
				continue
			}
			rec, ok := byTenant[name]
			if !ok {
				rec = &row{Name: name, Apps: []member{}}
				if h.Tenants != nil {
					spec := h.Tenants.Spec(name)
					rec.Policy = tenant.NormalizePolicy(spec.FailurePolicy)
				}
				byTenant[name] = rec
			}
			rec.Apps = append(rec.Apps, member{Name: app.Name, Status: app.Status})
		}
	}
	if h.Tenants != nil {
		for _, st := range h.Tenants.ListHolds() {
			rec, ok := byTenant[st.Tenant]
			if !ok {
				rec = &row{Name: st.Tenant, Apps: []member{}}
				byTenant[st.Tenant] = rec
			}
			cp := st
			rec.Hold = &cp
		}
	}
	out := make([]row, 0, len(byTenant))
	for _, rec := range byTenant {
		out = append(out, *rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "generated_at": time.Now().UTC()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
