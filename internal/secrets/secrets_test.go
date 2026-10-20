package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveEnvsVault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok" {
			http.Error(w, "nope", 403)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"data": map[string]interface{}{"password": "s3cr3t"},
			},
		})
	}))
	defer srv.Close()

	m := NewManager(Config{
		VaultEnabled: true,
		VaultAddr:    srv.URL,
		VaultToken:   "tok",
		HTTPClient:   srv.Client(),
	})
	out, err := m.ResolveEnvs(context.Background(), map[string]string{
		"DB":   "vault://secret/data/app#password",
		"KEEP": "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["DB"] != "s3cr3t" || out["KEEP"] != "plain" {
		t.Fatalf("unexpected: %#v", out)
	}
}

func TestLoadConfigFromEnvAuto(t *testing.T) {
	t.Setenv("CONOPS_SOPS", "")
	t.Setenv("CONOPS_SOPS_AGE_KEY", "")
	t.Setenv("CONOPS_SOPS_AGE_KEY_FILE", "/tmp/age.txt")
	t.Setenv("CONOPS_VAULT", "")
	t.Setenv("CONOPS_VAULT_ADDR", "https://vault.example")
	t.Setenv("CONOPS_VAULT_TOKEN", "t")
	cfg := LoadConfigFromEnv()
	if !cfg.SOPSEnabled || !cfg.VaultEnabled {
		t.Fatalf("expected auto-enable: %+v", cfg)
	}
}

func TestLoadConfigFromEnvAgeKeyInline(t *testing.T) {
	t.Setenv("CONOPS_SOPS", "")
	t.Setenv("CONOPS_SOPS_AGE_KEY_FILE", "")
	t.Setenv("CONOPS_SOPS_AGE_KEY", "AGE-SECRET-KEY-1TEST")
	t.Setenv("CONOPS_VAULT", "false")
	cfg := LoadConfigFromEnv()
	if !cfg.SOPSEnabled || cfg.SOPSAgeKey != "AGE-SECRET-KEY-1TEST" {
		t.Fatalf("expected inline age key auto-enable: %+v", cfg)
	}
}
