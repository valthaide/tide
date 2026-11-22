package compose

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// normalizeEnvFileMapPaths rewrites the env_file entries of a
// `config --format json --no-interpolate` render into absolute,
// interpolated paths so they can be matched against the controller's
// changed-file list. The render keeps the env_file key (the resolved render
// drops it) but leaves ${VAR} references unexpanded and may attach the
// project directory in front of a path that turns out absolute after
// expansion. Variables come from the same environment the render itself
// uses (process env, project .env, fleet vars). Entries that cannot be
// resolved stay as rendered; the input is returned unchanged on any
// structural surprise.
func normalizeEnvFileMapPaths(configJSON []byte, projectDir string, vars map[string]string) []byte {
	env := interpolationEnv(projectDir, vars)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(configJSON, &top); err != nil {
		return configJSON
	}
	servicesRaw, ok := top["services"]
	if !ok {
		return configJSON
	}
	var services map[string]json.RawMessage
	if err := json.Unmarshal(servicesRaw, &services); err != nil {
		return configJSON
	}
	changed := false
	for name, svcRaw := range services {
		var svc map[string]json.RawMessage
		if err := json.Unmarshal(svcRaw, &svc); err != nil {
			continue
		}
		envFileRaw, ok := svc["env_file"]
		if !ok || len(bytes.TrimSpace(envFileRaw)) == 0 {
			continue
		}
		rewritten, ok := rewriteEnvFileEntries(envFileRaw, projectDir, env)
		if !ok {
			continue
		}
		svc["env_file"] = rewritten
		if svcJSON, err := json.Marshal(svc); err == nil {
			services[name] = svcJSON
			changed = true
		}
	}
	if !changed {
		return configJSON
	}
	servicesJSON, err := json.Marshal(services)
	if err != nil {
		return configJSON
	}
	top["services"] = servicesJSON
	out, err := json.Marshal(top)
	if err != nil {
		return configJSON
	}
	return out
}

// rewriteEnvFileEntries resolves every path in one service's env_file value,
// keeping the rendered shape (plain strings or {path, ...} objects).
func rewriteEnvFileEntries(raw json.RawMessage, projectDir string, env map[string]string) (json.RawMessage, bool) {
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &objects); err == nil && objects != nil {
		changed := false
		for _, o := range objects {
			var p string
			if err := json.Unmarshal(o["path"], &p); err != nil || p == "" {
				continue
			}
			resolved, ok := resolveEnvMapPath(p, projectDir, env)
			if !ok {
				continue
			}
			pathJSON, err := json.Marshal(resolved)
			if err != nil {
				continue
			}
			o["path"] = pathJSON
			changed = true
		}
		if !changed {
			return nil, false
		}
		out, err := json.Marshal(objects)
		if err != nil {
			return nil, false
		}
		return out, true
	}
	var plain []string
	if err := json.Unmarshal(raw, &plain); err != nil || plain == nil {
		return nil, false
	}
	changed := false
	for i, p := range plain {
		resolved, ok := resolveEnvMapPath(p, projectDir, env)
		if !ok {
			continue
		}
		plain[i] = resolved
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(plain)
	if err != nil {
		return nil, false
	}
	return out, true
}

// resolveEnvMapPath expands variables in one rendered env_file path and
// makes it absolute against the compose project directory. A project-dir
// prefix attached by the renderer in front of a variable that expands to an
// absolute path is dropped. ok is false when a required variable is missing.
func resolveEnvMapPath(raw, projectDir string, env map[string]string) (string, bool) {
	rest := strings.TrimSpace(raw)
	if rest == "" {
		return "", false
	}
	projectDir = strings.TrimSpace(projectDir)
	if projectDir != "" {
		pd := filepath.Clean(projectDir)
		if strings.HasPrefix(rest, pd+string(filepath.Separator)) {
			rest = rest[len(pd)+1:]
		}
	}
	expanded, unresolved := interpolateEnvString(rest, env)
	if len(unresolved) > 0 {
		return "", false
	}
	expanded = strings.TrimSpace(expanded)
	if expanded == "" {
		return "", false
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded), true
	}
	if projectDir == "" {
		return filepath.Clean(expanded), true
	}
	return filepath.Clean(filepath.Join(projectDir, expanded)), true
}
