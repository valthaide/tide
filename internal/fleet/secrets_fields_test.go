package fleet

import (
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/deploypolicy"
)

func TestSecretsProviderInheritance(t *testing.T) {
	t.Setenv("TIDE_SECRETS_PROVIDER", "")
	t.Setenv("TIDE_SECRETS_DIR", "/var/lib/tide/secrets")
	doc := &HostDocument{
		SecretsProvider: "openbao",
		Tenants: map[string]TenantSpec{
			"test-ru": {},
			"agent-t": {SecretsProvider: "agent"},
		},
		Apps: []AppSpec{
			{
				Name:        "backend",
				Tenant:      "test-ru",
				RepoURL:     "https://example/app.git",
				ComposePath: "compose.yaml",
				SecretPaths: []string{"{prefix}/{tenant}/{app}"},
				SecretKeys:  []string{"DB_PASSWORD"},
				SecretFrom:  map[string]SecretFromSpec{"HASURA_ADMIN_SECRET": {Path: "shared/backend", Key: "admin"}},
			},
			{
				Name:        "legacy",
				Tenant:      "agent-t",
				RepoURL:     "https://example/legacy.git",
				ComposePath: "compose.yaml",
			},
			{
				Name:            "override",
				RepoURL:         "https://example/override.git",
				ComposePath:     "compose.yaml",
				SecretsProvider: "none",
			},
		},
	}
	pol := deploypolicy.Decode(policyJSONFromSpec(doc.Apps[0], doc))
	if pol.SecretsProvider != deploypolicy.SecretsProviderOpenBao {
		t.Fatalf("provider=%q", pol.SecretsProvider)
	}
	wantFile := "/var/lib/tide/secrets/backend.env"
	found := false
	for _, f := range pol.EnvFiles {
		if f == wantFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret file not auto-added to env_files: %v", pol.EnvFiles)
	}
	found = false
	for _, k := range pol.RequiredEnv {
		if k == "DB_PASSWORD" {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret_keys not merged into required_env: %v", pol.RequiredEnv)
	}

	pol = deploypolicy.Decode(policyJSONFromSpec(doc.Apps[1], doc))
	if pol.EffectiveSecretsProvider() != deploypolicy.SecretsProviderAgent {
		t.Fatalf("tenant provider=%q", pol.EffectiveSecretsProvider())
	}
	if len(pol.EnvFiles) != 0 {
		t.Fatalf("agent app must not get a secret file: %v", pol.EnvFiles)
	}

	pol = deploypolicy.Decode(policyJSONFromSpec(doc.Apps[2], doc))
	if pol.EffectiveSecretsProvider() != deploypolicy.SecretsProviderNone {
		t.Fatalf("app override provider=%q", pol.EffectiveSecretsProvider())
	}
}

func TestValidateSecretsFields(t *testing.T) {
	yaml := `
schema: 1
host: example
apps:
  - name: noprovider
    repo_url: https://example/a.git
    compose_path: compose.yaml
    secret_paths: [shared/app]
  - name: badkeys
    repo_url: https://example/b.git
    compose_path: compose.yaml
    secrets_provider: openbao
    secret_paths: [/abs/path, "up/../path"]
    secret_keys: [bad-name]
  - name: conflict
    repo_url: https://example/c.git
    compose_path: compose.yaml
    secrets_provider: agent
    secret_keys: [KEY]
  - name: badprovider
    repo_url: https://example/d.git
    compose_path: compose.yaml
    secrets_provider: sops
`
	report, err := Validate([]byte(yaml), ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, issue := range report.Issues {
		joined += issue.Rule + "\n"
	}
	for _, want := range []string{
		"secrets_provider_required",
		"invalid_secret_path",
		"invalid_secret_key",
		"secrets_agent_provider_conflict",
		"invalid_secrets_provider",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %s in issues:\n%s", want, joined)
		}
	}
}

func TestValidateSecretsOpenBaoOK(t *testing.T) {
	yaml := `
schema: 1
host: example
secrets_provider: openbao
apps:
  - name: backend
    repo_url: https://example/a.git
    compose_path: compose.yaml
    secret_paths: ["{prefix}/{tenant}/{app}", "shared/{app}"]
    secret_keys: [DB_PASSWORD]
    secret_from:
      HASURA_ADMIN_SECRET: {path: shared/backend, key: admin}
    secret_file: /var/lib/tide/secrets/backend.env
`
	report, err := Validate([]byte(yaml), ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range report.Issues {
		t.Fatalf("unexpected issue: %+v", issue)
	}
}
