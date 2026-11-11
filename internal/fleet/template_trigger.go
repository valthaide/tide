package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/valthaide/tide/internal/fleetcheck"
)

// enqueueTemplateChanges requeues every app whose fleet template file changed
// since the previous fleet sync.
//
// The first observation of a template only records the baseline: apps are
// already reconciled on registration, and a controller restart must not
// redeploy everything (the controller's own applied-template hash covers a
// template that changed while it was down). A missing template file hashes
// as "missing", so deleting or restoring a template is also a change; the
// app's preflight then reports the clear reason.
func (r *Reconciler) enqueueTemplateChanges(doc *HostDocument) {
	if r == nil || doc == nil || strings.TrimSpace(r.fleetRoot) == "" {
		return
	}
	used := map[string][]string{} // template -> app names
	for _, spec := range doc.Apps {
		template := strings.TrimSpace(spec.Template)
		if template == "" {
			continue
		}
		used[template] = append(used[template], spec.Name)
	}
	if len(used) == 0 {
		return
	}
	if r.templateHashes == nil {
		r.templateHashes = map[string]string{}
	}
	for template, names := range used {
		hash := templateFileHash(r.fleetRoot, template)
		prev, seen := r.templateHashes[template]
		r.templateHashes[template] = hash
		if !seen || prev == hash {
			continue
		}
		for _, name := range names {
			app := r.Apps.FindByName(name)
			if app == nil {
				continue
			}
			if err := r.Apps.MarkPending(app.ID); err == nil && r.Logger != nil {
				r.Logger.Info("Fleet template changed; queued apps using it",
					"template", template, "app", name, "fleet_root", r.fleetRoot)
			}
		}
	}
}

// FleetCheckoutDir returns the fleet checkout root (holding templates/) that
// this reconciler keeps up to date, or "" before the first successful sync.
func (r *Reconciler) FleetCheckoutDir() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.fleetRoot) != "" {
		return r.fleetRoot
	}
	return filepath.Join(r.Config.CacheDir, "repo")
}

func templateFileHash(fleetRoot, template string) string {
	data, err := os.ReadFile(filepath.Join(fleetRoot, "templates", template, fleetcheck.TemplateFileName))
	if err != nil {
		return "missing"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
