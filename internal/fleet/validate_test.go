package fleet

import "testing"

func TestValidateRejectsProjectConflict(t *testing.T) {
	raw := []byte("host: h\napps:\n  - name: a\n    repo_url: https://example.test/a.git\n    project: shared\n  - name: b\n    repo_url: https://example.test/b.git\n    project: shared\n")
	report, err := Validate(raw, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || len(report.Issues) != 2 {
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
