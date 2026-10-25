package fleet

import (
	"strings"
	"testing"

	"github.com/valthaide/conops-private/internal/fleetcheck"
)

func TestValidateRejectsProjectConflict(t *testing.T) {
	raw := []byte("host: h\napps:\n  - name: a\n    repo_url: https://example.test/a.git\n    project: shared\n  - name: b\n    repo_url: https://example.test/b.git\n    project: shared\n")
	report, err := Validate(raw, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || len(report.Issues) != 1 {
		t.Fatalf("%+v", report)
	}
}

func TestValidateAllowsUnderscoreProject(t *testing.T) {
	raw := []byte("host: h\napps:\n  - name: a\n    repo_url: https://example.test/a.git\n    project: crm_backend\n")
	report, err := Validate(raw, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("%+v", report)
	}
}

func TestValidateProjectConflictOnce(t *testing.T) {
	raw := []byte("host: h\napps:\n  - name: a\n    repo_url: https://example.test/a.git\n    project: shared\n  - name: b\n    repo_url: https://example.test/b.git\n    project: shared\n")
	report, err := Validate(raw, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) != 1 {
		t.Fatalf("duplicate messages: %+v", report.Issues)
	}
}

func TestConfigIssuesBuildAndSuspendedPorts(t *testing.T) {
	sum := fleetcheck.Summary{
		Images:    map[string]string{"web": "app:${APP_TAG}", "job": "local:1@sha256:def"},
		Build:     []string{"job"},
		Published: []int{8080},
	}
	ports := map[int]string{}
	issues := configIssues(AppSpec{Name: "a", Build: "forbidden", RequireDigest: true}, "", sum, ports)
	if len(issues) != 2 {
		t.Fatalf("%+v", issues)
	}
	if appChecked(AppSpec{Suspended: true}) {
		t.Fatal("suspended apps are not part of compose checks")
	}
	env := composeConfigEnv([]string{"APP_TAG=old", "OTHER=1"}, map[string]string{"APP_TAG": "1.2.3@sha256:abc"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "APP_TAG=old") || !strings.Contains(joined, "APP_TAG=1.2.3@sha256:abc") || !strings.Contains(joined, "OTHER=1") {
		t.Fatalf("%v", env)
	}
}
