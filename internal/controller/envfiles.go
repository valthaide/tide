package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/redact"
	"github.com/valthaide/tide/internal/syncerr"
)

const invalidEnvMarker = "__INVALID__"

func readEnvFiles(paths []string) (map[string]string, string) {
	out := map[string]string{}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, path
		}
		if len(data) == 0 {
			return nil, path
		}
		for key, value := range parseEnv(string(data)) {
			out[key] = value
			if redact.Default != nil && len(value) >= 8 {
				redact.Default.AddSecret(value)
			}
		}
	}
	return out, ""
}

func parseEnv(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		out[key] = value
	}
	return out
}

// envHashResult is the unified env_files content fingerprint for an app.
type envHashResult struct {
	Hash  string
	Paths []string
}

type sourceEnvPathLister interface {
	ExternalEnvPathsForSource(appID, composePath, template, appDir string) ([]string, error)
}

type envPathLister interface {
	ExternalEnvPathsForApp(appID, composePath string) ([]string, error)
}

// envHash combines absolute compose env_file paths with policy.EnvFiles.
// Format: sha256 of sorted unique "path=sha256(content)\n" lines.
func (r *Reconciler) envHash(app *App) (envHashResult, error) {
	out := envHashResult{}
	if app == nil {
		return out, nil
	}
	paths, err := r.envFilePaths(app)
	if err != nil {
		return out, err
	}
	out.Paths = paths
	if len(paths) == 0 {
		return out, nil
	}
	hash, err := compose.HashEnvFilePaths(paths)
	if err != nil {
		return out, err
	}
	out.Hash = hash
	return out, nil
}

func (r *Reconciler) envFilePaths(app *App) ([]string, error) {
	if app == nil {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	if r != nil && r.Executor != nil {
		composePaths, err := r.composeEnvFilePaths(app)
		if err != nil {
			return nil, err
		}
		for _, p := range composePaths {
			add(p)
		}
	}
	for _, p := range app.Policy().EnvFiles {
		add(p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (r *Reconciler) composeEnvFilePaths(app *App) ([]string, error) {
	if app == nil || r == nil || r.Executor == nil {
		return nil, nil
	}
	if lister, ok := r.Executor.(sourceEnvPathLister); ok {
		return lister.ExternalEnvPathsForSource(app.ID, app.ComposePath, app.Template, app.AppDir)
	}
	if lister, ok := r.Executor.(envPathLister); ok {
		return lister.ExternalEnvPathsForApp(app.ID, app.ComposePath)
	}
	return nil, nil
}

func encodeEnvPaths(paths []string) string {
	if len(paths) == 0 {
		return "[]"
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeEnvPaths(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var paths []string
	if err := json.Unmarshal([]byte(raw), &paths); err != nil {
		return nil
	}
	out := make([]string, 0, len(paths))
	seen := map[string]struct{}{}
	for _, p := range paths {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// envHashNeedsApply reports content drift that still requires a redeploy
// (ignores debounce; used by desiredSettled so queued env changes are not dropped).
func (r *Reconciler) envHashNeedsApply(app *App) bool {
	if app == nil {
		return false
	}
	cur, err := r.envHash(app)
	if err != nil || cur.Hash == "" {
		return false
	}
	if shouldAdoptEnvHash(app, cur) {
		return false
	}
	return cur.Hash != strings.TrimSpace(app.LastAppliedEnvHash)
}

// shouldAdoptEnvHash is true when the hash should be recorded without redeploy:
// first observation, or only new paths were added while previous path contents are unchanged.
func shouldAdoptEnvHash(app *App, cur envHashResult) bool {
	if app == nil || cur.Hash == "" {
		return false
	}
	if strings.TrimSpace(app.LastAppliedEnvHash) == "" {
		return true
	}
	prev := decodeEnvPaths(app.LastAppliedEnvPaths)
	if len(prev) == 0 {
		// Pre-v2.5 row: paths unknown; adopt only when hashes already match.
		return cur.Hash == app.LastAppliedEnvHash
	}
	curSet := make(map[string]struct{}, len(cur.Paths))
	for _, p := range cur.Paths {
		curSet[p] = struct{}{}
	}
	for _, p := range prev {
		if _, ok := curSet[p]; !ok {
			return false
		}
	}
	if len(cur.Paths) <= len(prev) {
		return false
	}
	subset, err := compose.HashEnvFilePaths(prev)
	if err != nil {
		return false
	}
	return subset == app.LastAppliedEnvHash
}

// checkRequiredEnv ensures every required key is present and non-empty in env files.
// Fleet vars are intentionally ignored so secrets cannot be overridden from git.
func checkRequiredEnv(required []string, fileVars map[string]string) error {
	var missing []string
	seen := map[string]struct{}{}
	for _, key := range required {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		val, ok := fileVars[key]
		if !ok || strings.TrimSpace(val) == "" || strings.TrimSpace(val) == invalidEnvMarker {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return syncerr.New(syncerr.CodeEnvKeyMissing, syncerr.StageValidate,
		fmt.Sprintf("env_key_missing: %s", strings.Join(missing, ", ")))
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func envFileInsecure(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	mode := info.Mode().Perm()
	return mode&0077 != 0
}

func mergeInterpolation(files, fleet map[string]string, appName, project string) map[string]string {
	out := map[string]string{}
	for key, value := range files {
		out[key] = value
	}
	if appName != "" {
		out["SLOT"] = appName
		out["TIDE_APP_NAME"] = appName
	}
	if project != "" {
		out["PROJECT"] = project
		out["TIDE_PROJECT"] = project
	}
	envcompat.PopulateInterpolationAliases(out, appName, project)
	for key, value := range fleet {
		out[key] = value
	}
	return out
}

func fileVarsOf(app *App) map[string]string {
	if app == nil {
		return nil
	}
	vars, _ := readEnvFiles(app.Policy().EnvFiles)
	return vars
}

// recordAppliedEnvHash records the unified env hash after a successful force sync.
func (h *Handler) recordAppliedEnvHash(app *App) {
	if h == nil || app == nil || h.Registry == nil {
		return
	}
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	if lister, ok := h.Applier.(sourceEnvPathLister); ok {
		if p, err := lister.ExternalEnvPathsForSource(app.ID, app.ComposePath, app.Template, app.AppDir); err == nil {
			for _, x := range p {
				add(x)
			}
		}
	} else if lister, ok := h.Applier.(envPathLister); ok {
		if p, err := lister.ExternalEnvPathsForApp(app.ID, app.ComposePath); err == nil {
			for _, x := range p {
				add(x)
			}
		}
	}
	for _, p := range app.Policy().EnvFiles {
		add(p)
	}
	sort.Strings(paths)
	hash, err := compose.HashEnvFilePaths(paths)
	if err != nil || hash == "" {
		return
	}
	_ = h.Registry.UpdateEnvHash(app.ID, hash, encodeEnvPaths(paths))
	metrics.SetEnvDrift(app.Name, false)
	metrics.SetEnvHashApplied(app.ID, app.Name, time.Now())
}

func migrationWarning(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "oneshot") || strings.Contains(lower, "migrate")
}
