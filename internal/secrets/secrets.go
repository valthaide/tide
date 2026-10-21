package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config controls optional Vault integration.
// SOPS support was removed from this fork.
type Config struct {
	SOPSEnabled    bool // true only when leftover env is set (ignored at runtime)
	VaultEnabled   bool
	VaultAddr      string
	VaultToken     string
	VaultNamespace string
	HTTPClient     *http.Client
}

// LoadConfigFromEnv reads CONOPS_SOPS / CONOPS_VAULT_*.
func LoadConfigFromEnv() Config {
	sopsMode := strings.ToLower(strings.TrimSpace(os.Getenv("CONOPS_SOPS")))
	vaultMode := strings.ToLower(strings.TrimSpace(os.Getenv("CONOPS_VAULT")))
	cfg := Config{
		VaultAddr:      strings.TrimRight(strings.TrimSpace(os.Getenv("CONOPS_VAULT_ADDR")), "/"),
		VaultToken:     strings.TrimSpace(os.Getenv("CONOPS_VAULT_TOKEN")),
		VaultNamespace: strings.TrimSpace(os.Getenv("CONOPS_VAULT_NAMESPACE")),
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
	}
	switch sopsMode {
	case "1", "true", "yes", "on":
		cfg.SOPSEnabled = true
	default:
		cfg.SOPSEnabled = false
	}
	if strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY")) != "" || strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY_FILE")) != "" {
		cfg.SOPSEnabled = true
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

// Warnings returns human-readable warnings for leftover/removed config (non-fatal).
func (c Config) Warnings() []string {
	var out []string
	if c.SOPSEnabled || strings.TrimSpace(os.Getenv("CONOPS_SOPS")) != "" {
		out = append(out, "CONOPS_SOPS is set but SOPS support was removed; unset CONOPS_SOPS / AGE key env vars (use hiera-eyaml → secrets.env)")
	}
	if strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY")) != "" {
		out = append(out, "CONOPS_SOPS_AGE_KEY is set but ignored; unset it")
	}
	if strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY_FILE")) != "" {
		out = append(out, "CONOPS_SOPS_AGE_KEY_FILE is set but ignored; unset it")
	}
	return out
}

// Manager applies secret backends during sync.
type Manager struct {
	Config Config
}

func NewManager(cfg Config) *Manager {
	return &Manager{Config: cfg}
}

// PrepareRepo is a no-op (SOPS removed).
func (m *Manager) PrepareRepo(ctx context.Context, repoDir string) error {
	_ = ctx
	_ = repoDir
	return nil
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
