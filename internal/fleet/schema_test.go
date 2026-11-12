package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The committed schema is generated, never hand-written: this test fails
// when the fleet structs and schemas/host.schema.json drift apart.
// Regenerate with: tide schema host --version v2.4.0 > schemas/host.schema.json
func TestCommittedHostSchemaMatchesGenerated(t *testing.T) {
	generated, err := HostSchemaJSON("v2.4.0")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join("..", "..", "schemas", "host.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(generated) {
		t.Fatal("schemas/host.schema.json is stale; regenerate with `tide schema host --version v2.4.0 > schemas/host.schema.json`")
	}
}

func TestHostSchemaMetadata(t *testing.T) {
	raw, err := HostSchemaJSON("2.4.0")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["$id"] != "https://github.com/valthaide/tide/releases/download/v2.4.0/host.schema.json" {
		t.Fatalf("$id: %v", doc["$id"])
	}
	if doc["title"] == "" {
		t.Fatal("schema has no title")
	}
	// Every property must carry a description from the code.
	props := doc["properties"].(map[string]any)
	for name, prop := range props {
		if _, ok := prop.(map[string]any)["description"]; !ok {
			t.Fatalf("property %s has no description", name)
		}
	}
	if LatestSchemaURL() != "https://github.com/valthaide/tide/releases/latest/download/host.schema.json" {
		t.Fatalf("latest: %s", LatestSchemaURL())
	}
}

// Every yaml field the parser accepts must exist in the generated schema,
// and vice versa. This is the parser-drift guard.
func TestHostSchemaCoversParserStructs(t *testing.T) {
	raw, err := HostSchemaJSON("v2.4.0")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	assertPropsMatchStruct(t, doc, reflect.TypeOf(HostDocument{}))
	defs := doc["$defs"].(map[string]any)
	for _, typ := range []reflect.Type{reflect.TypeOf(AppSpec{}), reflect.TypeOf(TenantSpec{}), reflect.TypeOf(TrustedWebSpec{}), reflect.TypeOf(PolicyException{}), reflect.TypeOf(WatchFileSpec{})} {
		def, ok := defs[typ.Name()].(map[string]any)
		if !ok {
			t.Fatalf("schema has no $defs/%s", typ.Name())
		}
		assertPropsMatchStruct(t, def, typ)
	}
}

func assertPropsMatchStruct(t *testing.T, schema map[string]any, typ reflect.Type) {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s: schema has no properties", typ.Name())
	}
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if name == "-" || name == "" {
			continue
		}
		fields[name] = true
		if _, ok := props[name]; !ok {
			t.Fatalf("%s.%s missing from generated schema", typ.Name(), name)
		}
	}
	for name := range props {
		if !fields[name] {
			t.Fatalf("schema property %s.%s has no parser field", typ.Name(), name)
		}
	}
}

// A document the parser accepts must pass schema validation, and documents
// the schema rejects must be rejected with field paths.
func TestSchemaValidationAgreesWithParser(t *testing.T) {
	valid := []byte("$schema: https://example.test/host.schema.json\nschema: 1\nhost: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    template: web\ntenants:\n  acme:\n    required_jobs: [test]\n")
	issues, err := schemaIssues(valid)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("valid document has schema issues: %+v", issues)
	}
	if _, err := ParseHostDocument(valid); err != nil {
		t.Fatalf("parser rejects $schema document: %v", err)
	}

	cases := map[string]string{
		"apps[0].repo_url": "host: h\napps:\n  - name: a\n    repo_url: 42\n",
		"apps[0].bogus":    "host: h\napps:\n  - name: a\n    bogus: true\n",
		"schema":           "schema: 7\nhost: h\napps: []\n",
		"apps":             "host: h\napps: notalist\n",
	}
	for wantPath, raw := range cases {
		issues, err := schemaIssues([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, issue := range issues {
			if issue.Path == wantPath {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no schema issue at %q: %+v", wantPath, wantPath, issues)
		}
	}
}

func TestUnsupportedSchemaVersionRejectedEverywhere(t *testing.T) {
	raw := []byte("schema: 99\nhost: h\napps: []\n")
	if _, err := ParseHostDocument(raw); err == nil || !strings.Contains(err.Error(), "unsupported_fleet_schema") {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Validate(raw, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "unsupported_fleet_schema") {
		t.Fatalf("validate: %v", err)
	}
	if _, err := Validate([]byte("schema: -1\nhost: h\napps: []\n"), ValidateOptions{}); err == nil {
		t.Fatal("negative schema version accepted")
	}
	// Unspecified and current versions pass.
	for _, ok := range []string{"host: h\napps: []\n", "schema: 1\nhost: h\napps: []\n"} {
		report, err := Validate([]byte(ok), ValidateOptions{})
		if err != nil || !report.OK {
			t.Fatalf("%q: %v %+v", ok, err, report)
		}
	}
}

func TestValidateReportsPathsDuplicatesAndTemplateRefs(t *testing.T) {
	fleetDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fleetDir, "templates", "web"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fleetDir, "templates", "web", "compose.yaml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	raw := []byte("host: h\napps:\n  - name: dup\n    repo_url: https://git.example/a.git\n    template: web\n    compose_path: compose.yaml\n  - name: dup\n    repo_url: https://git.example/b.git\n    template: missing\n")
	report, err := Validate(raw, ValidateOptions{FleetDir: fleetDir})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK {
		t.Fatal("expected issues")
	}
	byRule := map[string]string{}
	for _, issue := range report.Issues {
		byRule[issue.Rule] = issue.Path
	}
	if byRule["template_compose_path_conflict"] != "apps[0].template" {
		t.Fatalf("conflict path: %+v", report.Issues)
	}
	if byRule["duplicate_app_name"] != "apps[1].name" {
		t.Fatalf("duplicate path: %+v", report.Issues)
	}
	if byRule["template_not_found"] != "apps[1].template" {
		t.Fatalf("template path: %+v", report.Issues)
	}
	// A template that exists in the checkout validates cleanly.
	okRaw := []byte("host: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    template: web\n")
	report, err = Validate(okRaw, ValidateOptions{FleetDir: fleetDir})
	if err != nil || !report.OK {
		t.Fatalf("existing template rejected: %v %+v", err, report)
	}
}

func TestValidateAppDirRules(t *testing.T) {
	// app_dir with a template is accepted and present in the schema.
	ok := []byte("host: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    template: web\n    app_dir: tools/pgadmin\n")
	report, err := Validate(ok, ValidateOptions{})
	if err != nil || !report.OK {
		t.Fatalf("valid app_dir rejected: %v %+v", err, report)
	}
	raw, err := HostSchemaJSON("v2.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"app_dir"`) {
		t.Fatal("generated schema has no app_dir")
	}

	cases := map[string]string{
		"app_dir_requires_template": "host: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    app_dir: backend\n",
		"invalid_app_dir":           "host: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    template: web\n    app_dir: ../escape\n",
	}
	for rule, doc := range cases {
		report, err := Validate([]byte(doc), ValidateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, issue := range report.Issues {
			if issue.Rule == rule && issue.Path == "apps[0].app_dir" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s not reported with path: %+v", rule, report.Issues)
		}
	}
}

func TestValidateFilesReportsPerFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte("host: h\napps: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("schema: 42\nhost: h\napps: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	reports := ValidateFiles([]string{good, bad, filepath.Join(dir, "missing.yaml")}, ValidateOptions{})
	if len(reports) != 3 || !reports[0].OK || reports[1].OK || reports[2].OK {
		t.Fatalf("%+v", reports)
	}
	if reports[1].File != bad || reports[2].Issues[0].Rule != "read_error" {
		t.Fatalf("%+v", reports)
	}
	if ReportsOK(reports) {
		t.Fatal("ReportsOK with failures")
	}
}
