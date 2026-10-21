package repoauth

import (
	"strings"
	"testing"
)

func TestHTTPSAuthEnv(t *testing.T) {
	url, env, err := HTTPSAuthEnv("https://gitlab.example/group/repo.git", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://gitlab.example/group/repo.git" {
		t.Fatalf("expected clean url, got %s", url)
	}
	if strings.Contains(url, "@") {
		t.Fatalf("url must not contain userinfo: %s", url)
	}
	if env["GIT_CONFIG_COUNT"] != "1" {
		t.Fatalf("missing GIT_CONFIG_COUNT: %#v", env)
	}
	if !strings.Contains(env["GIT_CONFIG_KEY_0"], "gitlab.example") {
		t.Fatalf("unexpected key: %s", env["GIT_CONFIG_KEY_0"])
	}
	if !strings.HasPrefix(env["GIT_CONFIG_VALUE_0"], "Authorization: Basic ") {
		t.Fatalf("unexpected value: %s", env["GIT_CONFIG_VALUE_0"])
	}
	if strings.Contains(env["GIT_CONFIG_VALUE_0"], "secret-token") {
		// base64 of oauth2:secret-token — token itself should not appear plaintext
		t.Fatalf("token must not appear plaintext in env value")
	}
}

func TestCleanHTTPSURL(t *testing.T) {
	got := CleanHTTPSURL("https://oauth2:secret@gitlab.example/a.git")
	if got != "https://gitlab.example/a.git" {
		t.Fatalf("got %s", got)
	}
}

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
