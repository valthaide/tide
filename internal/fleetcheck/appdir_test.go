package fleetcheck

import "testing"

func TestCheckAppDir(t *testing.T) {
	for _, ok := range []string{"", "backend", "tools/pgadmin", "a-b_1"} {
		if issue := CheckAppDir(ok); issue != nil {
			t.Fatalf("%q: %v", ok, issue)
		}
	}
	for _, bad := range []string{"/", "/abs", "C:/win", "..", "../escape", "a/../b", "a\\b", "a:b", "."} {
		if issue := CheckAppDir(bad); issue == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if got := NormalizeAppDir("tools\\pgadmin"); got != "tools/pgadmin" {
		t.Fatalf("normalize: %q", got)
	}
	if got := NormalizeAppDir("."); got != "" {
		t.Fatalf("dot: %q", got)
	}
}
