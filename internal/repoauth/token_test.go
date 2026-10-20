package repoauth

import (
	"strings"
	"testing"
)

func TestInjectTokenIntoHTTPSURL(t *testing.T) {
	got, err := InjectTokenIntoHTTPSURL("https://gitlab.example/group/repo.git", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "https://oauth2:secret@gitlab.example/") {
		t.Fatalf("unexpected url: %s", got)
	}
}

func TestValidateCreateInputToken(t *testing.T) {
	t.Setenv(GitTokenEnv, "env-token")
	if err := ValidateCreateInput("https://gitlab.example/a/b.git", MethodToken, ""); err != nil {
		t.Fatalf("expected env token to satisfy validation: %v", err)
	}
	if err := ValidateCreateInput("git@gitlab.example:a/b.git", MethodToken, "tok"); err == nil {
		t.Fatal("expected HTTPS requirement")
	}
}
