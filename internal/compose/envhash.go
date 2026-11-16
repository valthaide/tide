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
	paths, err := e.ExternalEnvPathsForApp(appID, composePath)
	if err != nil {
		return "", err
	}
	return HashEnvFilePaths(paths)
}

// CheckoutExists reports whether the app repository checkout is materialized.
func (e *ComposeExecutor) CheckoutExists(appID string) bool {
	if e == nil || strings.TrimSpace(appID) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(e.WorkDir, appID, "repo"))
	return err == nil && info.IsDir()
}

// ExternalEnvPathsForApp lists absolute external env_file paths for an app checkout.
func (e *ComposeExecutor) ExternalEnvPathsForApp(appID, composePath string) ([]string, error) {
	if e == nil {
		return nil, fmt.Errorf("compose executor is nil")
	}
	if !e.CheckoutExists(appID) {
		return nil, nil
	}
	if strings.TrimSpace(composePath) == "" {
		composePath = "compose.yaml"
	}
	repoDir := filepath.Join(e.WorkDir, appID, "repo")
	composeFull := filepath.Join(repoDir, composePath)
	return ListExternalEnvFilePaths(filepath.Dir(composeFull), []string{composeFull})
}

// ExternalEnvHashForSource is ExternalEnvHashForApp for either compose
// source: a compose_path file, or a fleet template plus the app override.
// Relative env_file entries resolve from the project directory (the app
// folder in template mode), matching the deploy invocation.
func (e *ComposeExecutor) ExternalEnvHashForSource(appID, composePath, template, appDir string) (string, error) {
	paths, err := e.ExternalEnvPathsForSource(appID, composePath, template, appDir)
	if err != nil {
		return "", err
	}
	return HashEnvFilePaths(paths)
}

// ExternalEnvPathsForSource lists absolute external env_file paths for either
// compose_path or template+override source.
func (e *ComposeExecutor) ExternalEnvPathsForSource(appID, composePath, template, appDir string) ([]string, error) {
	if e == nil {
		return nil, fmt.Errorf("compose executor is nil")
	}
	if !e.CheckoutExists(appID) {
		return nil, nil
	}
	repoDir := filepath.Join(e.WorkDir, appID, "repo")
	if strings.TrimSpace(template) == "" {
		return e.ExternalEnvPathsForApp(appID, composePath)
	}
	inv, err := e.ResolveInvocation(repoDir, "", ApplyOptions{Template: template, AppDir: appDir})
	if err != nil {
		return nil, err
	}
	return ListExternalEnvFilePaths(inv.ProjectDir, inv.Files)
}

// ExternalEnvHashFiles hashes absolute env_file paths referenced by any of
// the given compose files. Relative entries resolve from projectDir.
func ExternalEnvHashFiles(projectDir string, files []string) (string, error) {
	var paths []string
	seen := map[string]struct{}{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		found, err := extractAbsoluteEnvFiles(data, projectDir)
		if err != nil {
			return "", err
		}
		for _, p := range found {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	repoAbs := ""
	if len(files) > 0 {
		// External means outside every compose file's tree is too strict for
		// template mode (the template lives in the fleet checkout); treat the
		// project directory as the app tree instead.
		repoAbs, _ = filepath.Abs(projectDir)
	}
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

	return HashEnvFilePaths(external)
}

// ExternalEnvHash computes a stable hash of absolute env_file paths referenced
// by the compose file that live outside the repository checkout.
// Missing required env files return CodeEnvFileMissing.
// Empty (zero-byte) files return CodeEnvFileEmpty.
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
	return HashEnvFilePaths(external)
}

// HashEnvFilePaths hashes absolute env file paths as path=sha256(content)\n.
// Empty files return CodeEnvFileEmpty; missing files return CodeEnvFileMissing.
func HashEnvFilePaths(paths []string) (string, error) {
	cleaned := make([]string, 0, len(paths))
	seen := map[string]struct{}{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		cleaned = append(cleaned, p)
	}
	sort.Strings(cleaned)
	if len(cleaned) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, p := range cleaned {
		content, readErr := os.ReadFile(p)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return "", syncerr.New(syncerr.CodeEnvFileMissing, syncerr.StageCompose, fmt.Sprintf("env_file missing: %s", p))
			}
			return "", readErr
		}
		if len(content) == 0 {
			return "", syncerr.New(syncerr.CodeEnvFileEmpty, syncerr.StageValidate, fmt.Sprintf("env_file_empty: %s", p))
		}
		sum := sha256.Sum256(content)
		fmt.Fprintf(h, "%s=%s\n", p, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ListExternalEnvFilePaths returns sorted unique absolute env_file paths from
// compose files that live outside projectDir.
func ListExternalEnvFilePaths(projectDir string, files []string) ([]string, error) {
	var paths []string
	seen := map[string]struct{}{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		found, err := extractAbsoluteEnvFiles(data, projectDir)
		if err != nil {
			return nil, err
		}
		for _, p := range found {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	repoAbs, _ := filepath.Abs(projectDir)
	var external []string
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			continue
		}
		if repoAbs != "" && (p == repoAbs || strings.HasPrefix(p, repoAbs+string(os.PathSeparator))) {
			continue
		}
		external = append(external, p)
	}
	sort.Strings(external)
	return external, nil
}

// ServicesUsingEnvFiles returns compose service names that reference any of the
// given absolute env file paths. Relative compose entries are resolved from baseDir.
func ServicesUsingEnvFiles(composeYAML []byte, baseDir string, wantPaths []string) ([]string, error) {
	want := map[string]struct{}{}
	for _, p := range wantPaths {
		p = filepath.Clean(strings.TrimSpace(p))
		if p != "" {
			want[p] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, err
	}
	services, _ := doc["services"].(map[string]interface{})
	var out []string
	for name, svc := range services {
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
			if _, ok := want[path]; ok {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
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
