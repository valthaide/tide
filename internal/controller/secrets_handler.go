package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/valthaide/tide/internal/secrets"
)

// GetSecretsStatus handles GET /api/v1/secrets/status[?app=<name>].
// It reports provider, paths, versions, staleness, and token TTL. Values are
// never included.
func (h *Handler) GetSecretsStatus(w http.ResponseWriter, r *http.Request) {
	status := secrets.Status{Enabled: false, Provider: "none"}
	if h.Secrets != nil {
		status = h.Secrets.Status()
	}
	if filter := strings.TrimSpace(r.URL.Query().Get("app")); filter != "" {
		var apps []secrets.AppStatus
		for _, entry := range status.Apps {
			if entry.App == filter {
				apps = append(apps, entry)
			}
		}
		status.Apps = apps
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": status})
}
