package controller

import (
	"path/filepath"
	"testing"
)

func TestEnvPathHashesRoundtrip(t *testing.T) {
	a := filepath.Clean("/etc/a.env")
	b := filepath.Clean("/etc/b.env")
	per := map[string]string{b: "bbb", a: "aaa"}
	encoded := encodeEnvPathHashes(per)
	decoded := decodeEnvPathHashes(encoded)
	if len(decoded) != 2 || decoded[a] != "aaa" || decoded[b] != "bbb" {
		t.Fatalf("decoded %+v", decoded)
	}
	paths := decodeEnvPaths(encoded)
	if len(paths) != 2 || paths[0] != a || paths[1] != b {
		t.Fatalf("paths %+v", paths)
	}
}

func TestDecodeEnvPathHashesLegacyList(t *testing.T) {
	a := filepath.Clean("/etc/a.env")
	legacy := encodeEnvPaths([]string{"/etc/a.env", "/etc/b.env"})
	if hashes := decodeEnvPathHashes(legacy); hashes != nil {
		t.Fatalf("legacy list must not yield hashes: %+v", hashes)
	}
	paths := decodeEnvPaths(legacy)
	if len(paths) != 2 || paths[0] != a {
		t.Fatalf("paths %+v", paths)
	}
}

func TestChangedEnvPaths(t *testing.T) {
	prev := map[string]string{"/a.env": "1", "/b.env": "2", "/gone.env": "3"}
	cur := map[string]string{"/a.env": "1", "/b.env": "9", "/new.env": "4"}
	changed := changedEnvPaths(prev, cur)
	if len(changed) != 3 || changed[0] != "/b.env" || changed[1] != "/gone.env" || changed[2] != "/new.env" {
		t.Fatalf("changed %+v", changed)
	}
	if changed := changedEnvPaths(prev, prev); len(changed) != 0 {
		t.Fatalf("unchanged %+v", changed)
	}
	if changed := changedEnvPaths(nil, cur); changed != nil {
		t.Fatalf("unknown prev must be nil: %+v", changed)
	}
}
