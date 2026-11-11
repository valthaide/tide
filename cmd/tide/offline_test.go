package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchemaCommandPrintsGeneratedSchema(t *testing.T) {
	var out, errBuf bytes.Buffer
	code, handled := runOfflineCommand([]string{"schema", "host", "--version", "v2.3.0"}, "dev", &out, &errBuf)
	if !handled || code != 0 {
		t.Fatalf("code=%d handled=%v err=%s", code, handled, errBuf.String())
	}
	if !strings.Contains(out.String(), `"host.schema.json"`) && !strings.Contains(out.String(), "v2.3.0/host.schema.json") {
		t.Fatalf("schema output missing versioned $id:\n%s", out.String()[:200])
	}
	code, _ = runOfflineCommand([]string{"schema"}, "dev", &out, &errBuf)
	if code != 2 {
		t.Fatalf("bare schema: code %d", code)
	}
	code, _ = runOfflineCommand([]string{"schema", "bogus"}, "dev", &out, &errBuf)
	if code != 2 {
		t.Fatalf("bogus schema target: code %d", code)
	}
	if _, handled := runOfflineCommand([]string{"--data-dir", "x"}, "dev", &out, &errBuf); handled {
		t.Fatal("server args must not be handled as offline commands")
	}
}

func TestValidateCommandExitCodesAndFormats(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.MkdirAll(filepath.Join(dir, "templates", "web"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hosts, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "web", "compose.yaml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(hosts, "good.yaml")
	if err := os.WriteFile(good, []byte("host: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    template: web\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(hosts, "bad.yaml")
	if err := os.WriteFile(bad, []byte("host: h\napps:\n  - name: a\n    repo_url: https://git.example/a.git\n    template: missing\n    compose_path: x.yaml\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code, handled := runOfflineCommand([]string{"validate", good}, "dev", &out, &errBuf)
	if !handled || code != 0 || !strings.Contains(out.String(), "good.yaml: ok") {
		t.Fatalf("good: code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	out.Reset()
	code, _ = runOfflineCommand([]string{"validate", bad}, "dev", &out, &errBuf)
	if code != 1 {
		t.Fatalf("bad: code=%d out=%q", code, out.String())
	}
	for _, want := range []string{"bad.yaml: apps[0].template:", "template_compose_path_conflict", "template_not_found"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("bad output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	code, _ = runOfflineCommand([]string{"validate", "-o", "json", bad}, "dev", &out, &errBuf)
	if code != 1 || !strings.Contains(out.String(), `"path": "apps[0].template"`) || !strings.Contains(out.String(), `"ok": false`) {
		t.Fatalf("json: code=%d out=%s", code, out.String())
	}

	code, _ = runOfflineCommand([]string{"validate"}, "dev", &out, &errBuf)
	if code != 2 {
		t.Fatalf("no files: code=%d", code)
	}
	code, _ = runOfflineCommand([]string{"validate", filepath.Join(dir, "missing.yaml")}, "dev", &out, &errBuf)
	if code != 1 {
		t.Fatalf("missing file: code=%d", code)
	}
}
