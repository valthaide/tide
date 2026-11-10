package redact

import (
	"strings"
	"testing"
)

func TestRedactURLUserinfo(t *testing.T) {
	r := New()
	in := "fatal: https://oauth2:glpat-SECRETTOKEN123@git.example/repo.git not found"
	out := r.String(in)
	if strings.Contains(out, "SECRETTOKEN") || strings.Contains(out, "oauth2:") {
		t.Fatalf("token leaked: %s", out)
	}
	if !strings.Contains(out, "https://***@git.example") {
		t.Fatalf("expected masked userinfo, got %s", out)
	}
}

func TestRedactGitLabPrefix(t *testing.T) {
	r := New()
	out := r.String("auth failed glpat-TESTMARKER123456 remote")
	if strings.Contains(out, "TESTMARKER") {
		t.Fatalf("marker leaked: %s", out)
	}
	if !strings.Contains(out, "glpat-***") {
		t.Fatalf("expected glpat mask, got %s", out)
	}
}

func TestRedactEnvSecret(t *testing.T) {
	t.Setenv("TIDE_GIT_TOKEN", "supersecrettoken99")
	r := New()
	out := r.String("using supersecrettoken99 for fetch")
	if strings.Contains(out, "supersecrettoken99") {
		t.Fatalf("env secret leaked: %s", out)
	}
}

func TestRedactEmptySecretIgnored(t *testing.T) {
	t.Setenv("TIDE_GIT_TOKEN", "")
	r := New()
	r.AddSecret("ab") // too short
	in := "about above"
	if got := r.String(in); got != in {
		t.Fatalf("short/empty secret should not mask: %q", got)
	}
}

func TestStripUserinfoFromURL(t *testing.T) {
	got := StripUserinfoFromURL("https://oauth2:tok@git.example/a.git")
	if got != "https://git.example/a.git" {
		t.Fatalf("got %s", got)
	}
}
