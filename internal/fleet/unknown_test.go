package fleet

import "testing"

func TestUnknownFieldsWarnList(t *testing.T) {
	fields, err := UnknownFields([]byte("host: h\napps:\n  - name: a\n    repo_url: https://example\n    require_digset: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields[0] != "apps[0].require_digset" {
		t.Fatalf("%v", fields)
	}
}

func TestNewerSchemaIsRejected(t *testing.T) {
	_, err := UnknownFields([]byte("schema: 99\nhost: h\napps: []\n"))
	if err == nil {
		t.Fatal("expected schema error")
	}
}
