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
// SOPS support was removed from this fork; CONOPS_SOPS=true is a startup error.
type Config struct {
	SOPSEnabled    bool // always false; retained so callers can detect misconfig
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

// Validate returns an error when the process is configured to use removed features.
func (c Config) Validate() error {
	if c.SOPSEnabled {
		return fmt.Errorf("CONOPS_SOPS is enabled but SOPS support was removed from this ConOps fork; unset CONOPS_SOPS (TENKO uses hiera-eyaml → secrets.env)")
	}
	if age := strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY")); age != "" {
		return fmt.Errorf("CONOPS_SOPS_AGE_KEY is set but SOPS support was removed; unset it")
	}
	if ageFile := strings.TrimSpace(os.Getenv("CONOPS_SOPS_AGE_KEY_FILE")); ageFile != "" {
		return fmt.Errorf("CONOPS_SOPS_AGE_KEY_FILE is set but SOPS support was removed; unset it")
	}
	return nil
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
	if m != nil && m.Config.SOPSEnabled {
		return fmt.Errorf("SOPS is not supported in this ConOps build")
	}
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
