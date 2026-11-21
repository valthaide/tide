package fleet

import (
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/fleetcheck"
)

func boolPtr(v bool) *bool { return &v }

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

func TestValidateRollout(t *testing.T) {
	cases := []struct {
		name      string
		rollout   string
		wantRules []string
	}{
		{"no rollout section", "", nil},
		{"all strategy", "rollout:\n  strategy: all\n", nil},
		{"canary ok", "rollout:\n  strategy: canary\n  canary_apps: [a]\n", nil},
		{"manual ok", "rollout:\n  strategy: manual\n", nil},
		{"bad strategy", "rollout:\n  strategy: waves\n", []string{"invalid_rollout_strategy"}},
		{"canary without apps", "rollout:\n  strategy: canary\n", []string{"rollout_canary_apps_required"}},
		{"apps without canary", "rollout:\n  canary_apps: [a]\n", []string{"rollout_canary_without_strategy"}},
		{"apps under manual", "rollout:\n  strategy: manual\n  canary_apps: [a]\n", []string{"rollout_canary_without_strategy"}},
		{"unknown canary app", "rollout:\n  strategy: canary\n  canary_apps: [ghost]\n", []string{"rollout_unknown_canary"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("host: h\n" + tc.rollout + "apps:\n  - name: a\n    repo_url: https://example.test/a.git\n")
			report, err := Validate(raw, ValidateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, issue := range report.Issues {
				if strings.HasPrefix(issue.Path, "rollout") {
					got = append(got, issue.Rule)
				}
			}
			if len(got) != len(tc.wantRules) {
				t.Fatalf("rules=%v, want %v (all issues: %+v)", got, tc.wantRules, report.Issues)
			}
			for i, rule := range tc.wantRules {
				if got[i] != rule {
					t.Fatalf("rules=%v, want %v", got, tc.wantRules)
				}
			}
		})
	}
}

func TestConfigIssuesBuildAndSuspendedPorts(t *testing.T) {
	sum := fleetcheck.Summary{
		Images:    map[string]string{"web": "app:${APP_TAG}", "job": "local:1@sha256:def"},
		Build:     []string{"job"},
		Published: []int{8080},
	}
	ports := map[int]string{}
	issues := configIssues(AppSpec{Name: "a", Build: "forbidden", RequireDigest: boolPtr(true)}, "", sum, ports)
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
