package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/valthaide/tide/internal/syncerr"
)

// ExternalEnvHashForApp hashes absolute env_file paths for an app runtime checkout.
func (e *ComposeExecutor) ExternalEnvHashForApp(appID, composePath string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("compose executor is nil")
	}
	if strings.TrimSpace(composePath) == "" {
		composePath = "compose.yaml"
	}
	repoDir := filepath.Join(e.WorkDir, appID, "repo")
	return ExternalEnvHash(repoDir, composePath)
}

// ExternalEnvHash computes a stable hash of absolute env_file paths referenced
// by the compose file that live outside the repository checkout.
// Missing required env files return CodeEnvFileMissing.
func ExternalEnvHash(repoDir, composePath string) (string, error) {
	composeFull := filepath.Join(repoDir, composePath)
	data, err := os.ReadFile(composeFull)
	if err != nil {
		return "", err
	}
	paths, err := extractAbsoluteEnvFiles(data, filepath.Dir(composeFull))
	if err != nil {
		return "", err
	}
	repoAbs, _ := filepath.Abs(repoDir)
	var external []string
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			continue
		}
		if repoAbs != "" && (abs == repoAbs || strings.HasPrefix(abs, repoAbs+string(os.PathSeparator))) {
			continue
		}
		external = append(external, abs)
	}
	sort.Strings(external)

	h := sha256.New()
	for _, p := range external {
		content, readErr := os.ReadFile(p)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return "", syncerr.New(syncerr.CodeEnvFileMissing, syncerr.StageCompose, fmt.Sprintf("env_file missing: %s", p))
			}
			return "", readErr
		}
		sum := sha256.Sum256(content)
		fmt.Fprintf(h, "%s=%s\n", p, hex.EncodeToString(sum[:]))
	}
	if len(external) == 0 {
		return "", nil
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractAbsoluteEnvFiles(composeYAML []byte, baseDir string) ([]string, error) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, err
	}
	services, _ := doc["services"].(map[string]interface{})
	var out []string
	seen := map[string]struct{}{}
	for _, svc := range services {
		svcMap, ok := svc.(map[string]interface{})
		if !ok {
			continue
		}
		raw, ok := svcMap["env_file"]
		if !ok {
			continue
		}
		for _, entry := range normalizeEnvFileEntries(raw) {
			path := entry
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			path = filepath.Clean(path)
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			out = append(out, path)
		}
	}
	return out, nil
}

func normalizeEnvFileEntries(raw interface{}) []string {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []interface{}:
		var out []string
		for _, item := range v {
			switch e := item.(type) {
			case string:
				if strings.TrimSpace(e) != "" {
					out = append(out, e)
				}
			case map[string]interface{}:
				if p, ok := e["path"].(string); ok && strings.TrimSpace(p) != "" {
					out = append(out, p)
				}
			}
		}
		return out
	default:
		return nil
	}
}
