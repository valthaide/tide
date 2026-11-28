package controller

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/valthaide/tide/internal/cigate"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/fleetcheck"
)

// repoRelPathMapper maps an absolute host path inside the app checkout to
// its repo-relative path; implemented by *compose.ComposeExecutor.
type repoRelPathMapper interface {
	RepoRelPathForApp(appID, abs string) (string, bool)
}

// deployFilesForApp lists the repo-relative paths Tide reads to deploy this
// app: the compose file (compose_path mode), the app folder's .env and
// compose.override.yaml, and every env_file / watch_files entry that lives
// inside the app repository. The list comes from the app's own
// configuration, so a change to any of them must run a real pipeline while a
// commit touching nothing else may inherit a green ancestor pipeline.
func (r *Reconciler) deployFilesForApp(app *App) []string {
	if app == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		p = cigate.NormalizeRepoRelPath(p)
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	joinAppDir := func(name string) string {
		if dir := fleetcheck.NormalizeAppDir(app.AppDir); dir != "" {
			return dir + "/" + name
		}
		return name
	}

	// Compose files Tide renders for this app. In template mode the base
	// compose file lives in the fleet repo, not the app repo.
	if strings.TrimSpace(app.Template) == "" {
		composePath := strings.TrimSpace(app.ComposePath)
		if composePath == "" {
			composePath = "docker-compose.yml"
		}
		add(composePath)
	}
	add(joinAppDir(".env"))
	add(joinAppDir(fleetcheck.OverrideFileName))

	var mapper repoRelPathMapper
	if r != nil && r.Executor != nil {
		mapper, _ = r.Executor.(repoRelPathMapper)
	}
	toRepoRel := func(abs string) (string, bool) {
		if mapper == nil {
			return "", false
		}
		return mapper.RepoRelPathForApp(app.ID, abs)
	}

	// env_file entries resolved from the rendered compose (absolute paths);
	// only those inside the app checkout can appear in its repo diff.
	if paths, err := r.composeEnvFilePaths(app); err == nil {
		for _, abs := range paths {
			if rel, ok := toRepoRel(abs); ok {
				add(rel)
			}
		}
	}
	// watch_files entries: templates/ live in the fleet repo; relative paths
	// resolve from the app repo root; absolute paths count only when they
	// point inside the checkout.
	for _, w := range app.Policy().WatchFiles {
		p := strings.TrimSpace(w.Path)
		if p == "" || deploypolicy.IsTemplateWatchPath(p) {
			continue
		}
		if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
			if rel, ok := toRepoRel(p); ok {
				add(rel)
			}
			continue
		}
		add(p)
	}
	sort.Strings(out)
	return out
}
