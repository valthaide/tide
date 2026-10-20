package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config controls optional SOPS and Vault integration.
type Config struct {
	SOPSEnabled    bool
	SOPSAgeKey     string // raw AGE-SECRET-KEY-... (CONOPS_SOPS_AGE_KEY)
	SOPSAgeKeyFile string // path (CONOPS_SOPS_AGE_KEY_FILE)
	VaultEnabled   bool
	VaultAddr      string
	VaultToken     string
	VaultNamespace string
	HTTPClient     *http.Client
}

// LoadConfigFromEnv reads CONOPS_SOPS_* and CONOPS_VAULT_*.
func LoadConfigFromEnv() Config {
	sopsMode := strings.ToLower(strings.TrimSpace(os.Getenv("CONOPS_SOPS")))
	vaultMode := strings.ToLower(strings.TrimSpace(os.Getenv("CONOPS_VAULT")))
	cfg := Config{
		SOPSAgeKey:     strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY")),
		SOPSAgeKeyFile: strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY_FILE")),
		VaultAddr:      strings.TrimRight(strings.TrimSpace(os.Getenv("CONOPS_VAULT_ADDR")), "/"),
		VaultToken:     strings.TrimSpace(os.Getenv("CONOPS_VAULT_TOKEN")),
		VaultNamespace: strings.TrimSpace(os.Getenv("CONOPS_VAULT_NAMESPACE")),
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
	}
	switch sopsMode {
	case "1", "true", "yes", "on", "auto":
		cfg.SOPSEnabled = true
	case "0", "false", "no", "off":
		cfg.SOPSEnabled = false
	default:
		// auto-enable when age key material is present
		cfg.SOPSEnabled = cfg.SOPSAgeKey != "" || cfg.SOPSAgeKeyFile != ""
	}
	switch vaultMode {
	case "1", "true", "yes", "on":
		cfg.VaultEnabled = true
	case "0", "false", "no", "off":
		cfg.VaultEnabled = false
	default:
		cfg.VaultEnabled = cfg.VaultAddr != "" && cfg.VaultToken != ""
	}
	return cfg
}

// Manager applies secret backends during sync.
type Manager struct {
	Config Config
}

func NewManager(cfg Config) *Manager {
	return &Manager{Config: cfg}
}

// PrepareRepo decrypts SOPS files in-place under repoDir (runtime checkout only).
func (m *Manager) PrepareRepo(ctx context.Context, repoDir string) error {
	if m == nil || !m.Config.SOPSEnabled {
		return nil
	}
	return decryptSOPSTree(ctx, repoDir, m.Config.SOPSAgeKey, m.Config.SOPSAgeKeyFile)
}

// ResolveEnvs expands vault://path#key references in env map values.
func (m *Manager) ResolveEnvs(ctx context.Context, envs map[string]string) (map[string]string, error) {
	if m == nil || !m.Config.VaultEnabled || len(envs) == 0 {
		return envs, nil
	}
	out := make(map[string]string, len(envs))
	for k, v := range envs {
		resolved, err := m.resolveValue(ctx, v)
		if err != nil {
			return nil, err
		}
		out[k] = resolved
	}
	return out, nil
}

func (m *Manager) resolveValue(ctx context.Context, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "vault:") && !strings.HasPrefix(trimmed, "vault://") {
		return value, nil
	}
	ref := strings.TrimPrefix(trimmed, "vault://")
	ref = strings.TrimPrefix(ref, "vault:")
	path, key, ok := strings.Cut(ref, "#")
	if !ok || path == "" || key == "" {
		return "", fmt.Errorf("invalid vault ref %q (want vault://path#key)", value)
	}
	secret, err := m.readVault(ctx, path)
	if err != nil {
		return "", err
	}
	got, ok := secret[key]
	if !ok {
		return "", fmt.Errorf("vault path %q has no key %q", path, key)
	}
	return fmt.Sprint(got), nil
}

func (m *Manager) readVault(ctx context.Context, path string) (map[string]interface{}, error) {
	if m.Config.VaultAddr == "" || m.Config.VaultToken == "" {
		return nil, fmt.Errorf("vault addr/token not configured")
	}
	path = strings.TrimPrefix(path, "/")
	// KV v2: secret/data/foo → /v1/secret/data/foo
	url := m.Config.VaultAddr + "/v1/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", m.Config.VaultToken)
	if m.Config.VaultNamespace != "" {
		req.Header.Set("X-Vault-Namespace", m.Config.VaultNamespace)
	}
	client := m.Config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vault status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	// KV v2 nests under data.data
	var v2 struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(payload.Data, &v2); err == nil && v2.Data != nil {
		return v2.Data, nil
	}
	var v1 map[string]interface{}
	if err := json.Unmarshal(payload.Data, &v1); err != nil {
		return nil, err
	}
	return v1, nil
}

func decryptSOPSTree(ctx context.Context, root, ageKey, ageKeyFile string) error {
	sopsPath, err := exec.LookPath("sops")
	if err != nil {
		return fmt.Errorf("sops enabled but sops binary not found in PATH")
	}
	if strings.TrimSpace(ageKey) == "" && strings.TrimSpace(ageKeyFile) == "" {
		return fmt.Errorf("sops enabled but neither CONOPS_SOPS_AGE_KEY nor CONOPS_SOPS_AGE_KEY_FILE is set")
	}
	var files []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		lower := strings.ToLower(name)
		if strings.Contains(lower, ".sops.") || strings.HasSuffix(lower, ".sops.yaml") ||
			strings.HasSuffix(lower, ".sops.yml") || strings.HasSuffix(lower, ".sops.json") ||
			strings.HasSuffix(lower, ".enc.yaml") || strings.HasSuffix(lower, ".enc.yml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range files {
		if err := decryptSOPSFile(ctx, sopsPath, path, ageKey, ageKeyFile); err != nil {
			return fmt.Errorf("sops decrypt %s: %w", path, err)
		}
	}
	return nil
}

func decryptSOPSFile(ctx context.Context, sopsPath, file, ageKey, ageKeyFile string) error {
	cmd := exec.CommandContext(ctx, sopsPath, "-d", "-i", file)
	env := os.Environ()
	env = append(env, "SOPS_ABSENT_KEYS=ignore")
	// Inline key takes precedence (maps to sops SOPS_AGE_KEY).
	if ageKey != "" {
		env = append(env, "SOPS_AGE_KEY="+ageKey)
	} else if ageKeyFile != "" {
		env = append(env, "SOPS_AGE_KEY_FILE="+ageKeyFile)
	}
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (%s)", err, truncate(stderr.String(), 300))
	}
	// Rename *.sops.yaml → *.yaml when pattern matches, so compose can include cleartext name.
	dir := filepath.Dir(file)
	base := filepath.Base(file)
	for _, pat := range []string{".sops.yaml", ".sops.yml", ".sops.json"} {
		if strings.HasSuffix(strings.ToLower(base), pat) {
			out := filepath.Join(dir, base[:len(base)-len(pat)]+pat[len(".sops"):])
			if err := os.Rename(file, out); err != nil {
				return err
			}
			return nil
		}
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
