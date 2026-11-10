package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/redact"
)

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

func fleetEnvHash(paths []string) (string, string) {
	if len(paths) == 0 {
		return "", ""
	}
	sum := sha256.New()
	used := false
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", path
		}
		used = true
		sum.Write([]byte(path))
		sum.Write([]byte{0})
		sum.Write(data)
	}
	if !used {
		return "", ""
	}
	return hex.EncodeToString(sum.Sum(nil)), ""
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

func migrationWarning(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "oneshot") || strings.Contains(lower, "migrate")
}
