package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServicesForEnvPaths(t *testing.T) {
	config := []byte(`{
		"services": {
			"backend": {"env_file": [{"path": "/var/lib/tide/secrets/backend.env", "required": true}]},
			"worker": {"env_file": ["/var/lib/tide/secrets/backend.env"]},
			"nginx": {"image": "nginx:1"}
		}
	}`)
	services, unmapped := ServicesForEnvPaths(config, []string{"/var/lib/tide/secrets/backend.env"})
	if len(services) != 2 || services[0] != "backend" || services[1] != "worker" {
		t.Fatalf("services %+v", services)
	}
	if len(unmapped) != 0 {
		t.Fatalf("unmapped %+v", unmapped)
	}

	services, unmapped = ServicesForEnvPaths(config, []string{"/etc/other.env"})
	if len(services) != 0 {
		t.Fatalf("services %+v", services)
	}
	if len(unmapped) != 1 || unmapped[0] != filepath.Clean("/etc/other.env") {
		t.Fatalf("unmapped %+v", unmapped)
	}

	services, unmapped = ServicesForEnvPaths(config, []string{"/var/lib/tide/secrets/backend.env", "/etc/other.env"})
	if len(services) != 2 {
		t.Fatalf("services %+v", services)
	}
	if len(unmapped) != 1 {
		t.Fatalf("unmapped %+v", unmapped)
	}

	if services, unmapped := ServicesForEnvPaths([]byte(`not json`), []string{"/x.env"}); len(services) != 0 || len(unmapped) != 1 {
		t.Fatalf("invalid json: %+v %+v", services, unmapped)
	}
}

func TestHashEnvFilePathsDetailed(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.env")
	p2 := filepath.Join(dir, "b.env")
	if err := os.WriteFile(p1, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("B=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agg, perPath, err := HashEnvFilePathsDetailed([]string{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if len(perPath) != 2 || perPath[p1] == "" || perPath[p2] == "" {
		t.Fatalf("perPath %+v", perPath)
	}
	plain, err := HashEnvFilePaths([]string{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if agg != plain {
		t.Fatalf("aggregate mismatch %q vs %q", agg, plain)
	}
	if err := os.WriteFile(p1, []byte("A=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agg2, perPath2, err := HashEnvFilePathsDetailed([]string{p1, p2})
	if err != nil {
		t.Fatal(err)
	}
	if agg2 == agg {
		t.Fatal("aggregate must change")
	}
	if perPath2[p1] == perPath[p1] || perPath2[p2] != perPath[p2] {
		t.Fatalf("per-path diff wrong: %+v vs %+v", perPath2, perPath)
	}
}
