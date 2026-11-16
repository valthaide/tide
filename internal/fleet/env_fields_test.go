package fleet

import (
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/deploypolicy"
)

func TestRequiredEnvAndEnvFilesInheritance(t *testing.T) {
	doc := &HostDocument{
		EnvFiles:    []string{"/etc/tenko/shared.env"},
		RequiredEnv: []string{"SHARED_KEY"},
		EnvDebounce: "15s",
		Tenants: map[string]TenantSpec{
			"test-ru": {
				EnvFiles:    []string{"/etc/tenko/apps/backend/secrets.env"},
				RequiredEnv: []string{"HASURA_ADMIN_SECRET"},
				EnvDebounce: "20s",
			},
		},
		Apps: []AppSpec{{
			Name:        "backend",
			Tenant:      "test-ru",
			RepoURL:     "https://example/app.git",
			ComposePath: "compose.yaml",
			EnvFiles:    []string{"/etc/tenko/apps/backend/config.env"},
			RequiredEnv: []string{"DB_PASSWORD"},
			EnvDebounce: "30s",
		}},
	}
	raw := policyJSONFromSpec(doc.Apps[0], doc)
	pol := deploypolicy.Decode(raw)
	if len(pol.EnvFiles) != 3 {
		t.Fatalf("env_files=%v", pol.EnvFiles)
	}
	if len(pol.RequiredEnv) != 3 {
		t.Fatalf("required_env=%v", pol.RequiredEnv)
	}
	if pol.EnvDebounce != "30s" {
		t.Fatalf("debounce=%q", pol.EnvDebounce)
	}
}

func TestValidateEnvFilesAndRequiredEnv(t *testing.T) {
	yaml := `
schema: 1
host: example
apps:
  - name: app
    repo_url: https://example/app.git
    compose_path: compose.yaml
    env_files: [relative.env]
    required_env: [bad-name, OK_KEY]
    vars:
      OK_KEY: from-git
`
	report, err := Validate([]byte(yaml), ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, issue := range report.Issues {
		joined += issue.Rule + ":" + issue.Message + "\n"
	}
	if !strings.Contains(joined, "invalid_env_file") {
		t.Fatalf("expected invalid_env_file, issues=%s", joined)
	}
	if !strings.Contains(joined, "invalid_required_env") {
		t.Fatalf("expected invalid_required_env, issues=%s", joined)
	}
	if !strings.Contains(joined, "required_env_conflicts_vars") {
		t.Fatalf("expected required_env_conflicts_vars, issues=%s", joined)
	}
}
