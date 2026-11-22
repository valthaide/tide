package compose

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInterpolateEnvString(t *testing.T) {
	env := map[string]string{"SET": "val", "EMPTY": ""}
	cases := []struct {
		in         string
		want       string
		unresolved []string
	}{
		{"$SET and ${SET}", "val and val", nil},
		{"pre${SET}post", "prevalpost", nil},
		{"${TIDE_TEST_UNSET:-d}", "d", nil},
		{"${EMPTY:-d}", "d", nil},
		{"${TIDE_TEST_UNSET-d}", "d", nil},
		{"${EMPTY-d}", "", nil},
		{"${SET:+alt}", "alt", nil},
		{"${TIDE_TEST_UNSET:+alt}", "", nil},
		{"${EMPTY:+alt}", "", nil},
		{"${SET?err}", "val", nil},
		{"${EMPTY?err}", "", nil},
		{"${TIDE_TEST_UNSET?err}", "", []string{"TIDE_TEST_UNSET"}},
		{"${TIDE_TEST_UNSET:?err}", "", []string{"TIDE_TEST_UNSET"}},
		{"${EMPTY:?err}", "", []string{"EMPTY"}},
		{"$$SET", "$SET", nil},
		{"no dollars", "no dollars", nil},
	}
	for _, tc := range cases {
		got, unresolved := interpolateEnvString(tc.in, env)
		if got != tc.want || !reflect.DeepEqual(unresolved, tc.unresolved) {
			t.Errorf("interpolateEnvString(%q) = %q, %v; want %q, %v", tc.in, got, unresolved, tc.want, tc.unresolved)
		}
	}
}

func TestResolveEnvMapPath(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()
	sep := string(filepath.Separator)
	cases := []struct {
		name string
		raw  string
		env  map[string]string
		want string
		ok   bool
	}{
		{"absolute literal", filepath.Join(other, "file.env"), nil, filepath.Join(other, "file.env"), true},
		{"relative joins project dir", "./rel.env", nil, filepath.Join(proj, "rel.env"), true},
		{"prefixed var expanding absolute", proj + sep + "${TIDE_TEST_RMA:?}", map[string]string{"TIDE_TEST_RMA": filepath.Join(other, "x.env")}, filepath.Join(other, "x.env"), true},
		{"prefixed var expanding relative", proj + sep + "${TIDE_TEST_RMR:?}", map[string]string{"TIDE_TEST_RMR": filepath.Join("sub", "x.env")}, filepath.Join(proj, "sub", "x.env"), true},
		{"default when unset", "${TIDE_TEST_RMD:-def.env}", nil, filepath.Join(proj, "def.env"), true},
		{"required missing", "${TIDE_TEST_RMQ:?}", nil, "", false},
	}
	for _, tc := range cases {
		got, ok := resolveEnvMapPath(tc.raw, proj, tc.env)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: resolveEnvMapPath(%q) = %q, %v; want %q, %v", tc.name, tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeEnvFileMapPaths(t *testing.T) {
	proj := t.TempDir()
	other := t.TempDir()
	sharedEnv := filepath.Join(other, "shared.env")
	secretsEnv := filepath.Join(proj, "secrets.env")
	plainEnv := filepath.Join(proj, "plain.env")
	vars := map[string]string{
		"TIDE_TEST_MAP_REL": "secrets.env",
		"TIDE_TEST_MAP_ABS": sharedEnv,
	}
	config, err := json.Marshal(map[string]interface{}{
		"name": "proj",
		"services": map[string]interface{}{
			"app": map[string]interface{}{
				"image":    "app:1",
				"env_file": []map[string]interface{}{{"path": proj + string(filepath.Separator) + "${TIDE_TEST_MAP_REL:?}", "required": true}},
			},
			"worker": map[string]interface{}{
				"image":    "w:1",
				"env_file": []string{"./plain.env", "${TIDE_TEST_MAP_ABS:?}"},
			},
			"nginx": map[string]interface{}{"image": "n:1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	normalized := normalizeEnvFileMapPaths(config, proj, vars)

	matched, unmapped := ServicesForEnvPaths(normalized, []string{secretsEnv, sharedEnv, plainEnv})
	if len(unmapped) != 0 {
		t.Fatalf("unmapped %v", unmapped)
	}
	if len(matched) != 2 || matched[0] != "app" || matched[1] != "worker" {
		t.Fatalf("matched %v, want [app worker]", matched)
	}
}

func TestNormalizeEnvFileMapPathsUnresolvedStays(t *testing.T) {
	proj := t.TempDir()
	config := []byte(`{"name":"proj","services":{
		"app":{"image":"app:1","env_file":[{"path":"${TIDE_TEST_MAP_MISSING:?}"}]}
	}}`)
	normalized := normalizeEnvFileMapPaths(config, proj, nil)
	matched, unmapped := ServicesForEnvPaths(normalized, []string{filepath.Join(proj, "secrets.env")})
	if len(matched) != 0 || len(unmapped) != 1 {
		t.Fatalf("unresolvable entries must stay unmapped, got matched=%v unmapped=%v", matched, unmapped)
	}
}
