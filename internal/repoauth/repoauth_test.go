package repoauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestNormalizeMethod(t *testing.T) {
	tests := map[string]string{
		"":           MethodPublic,
		"public":     MethodPublic,
		"PUBLIC":     MethodPublic,
		"deploy_key": MethodDeployKey,
		"deploy-key": MethodDeployKey,
		"DeployKey":  MethodDeployKey,
		"token":      MethodToken,
	}
	for in, want := range tests {
		if got := NormalizeMethod(in); got != want {
			t.Fatalf("NormalizeMethod(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseSSHEndpoint(t *testing.T) {
	tests := []struct {
		url      string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{url: "git@github.com:org/repo.git", wantHost: "github.com", wantPort: "22"},
		{url: "git@gitlab.com:group/project.git", wantHost: "gitlab.com", wantPort: "22"},
		{url: "git@git.example.com:group/infra/puppet.git", wantHost: "git.example.com", wantPort: "22"},
		{url: "ssh://git@git.example.com:2222/group/repo.git", wantHost: "git.example.com", wantPort: "2222"},
		{url: "ssh://git@gitlab.com/group/repo.git", wantHost: "gitlab.com", wantPort: "22"},
		{url: "https://github.com/org/repo.git", wantHost: "github.com", wantPort: ""},
		{url: "not-a-url", wantErr: true},
		{url: "", wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseSSHEndpoint(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseSSHEndpoint(%q) expected error", tc.url)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseSSHEndpoint(%q) unexpected error: %v", tc.url, err)
		}
		wantPort := tc.wantPort
		if wantPort == "" {
			wantPort = defaultSSHPort
		}
		if got.Host != tc.wantHost || got.normalizedPort() != wantPort {
			t.Fatalf("ParseSSHEndpoint(%q)=%+v want host=%s port=%s", tc.url, got, tc.wantHost, tc.wantPort)
		}
	}
}

func TestValidateCreateInputAllowsNonGitHubSSH(t *testing.T) {
	key := generateTestEd25519Key(t)

	cases := []string{
		"git@gitlab.com:group/repo.git",
		"git@git.example.com:group/app.git",
		"ssh://git@git.example.com:2222/group/repo.git",
		"git@github.com:org/repo.git",
	}
	for _, repoURL := range cases {
		if err := ValidateCreateInput(repoURL, MethodDeployKey, key); err != nil {
			t.Fatalf("ValidateCreateInput(%q) unexpected error: %v", repoURL, err)
		}
	}
}

func TestValidateCreateInputRejectsHTTPSDeployKey(t *testing.T) {
	key := generateTestEd25519Key(t)
	err := ValidateCreateInput("https://gitlab.com/group/repo.git", MethodDeployKey, key)
	if err == nil || !strings.Contains(err.Error(), "SSH repo URL") {
		t.Fatalf("expected SSH URL error, got %v", err)
	}
}

func TestKnownHostsContainsAndFormat(t *testing.T) {
	endpoint := SSHEndpoint{Host: "git.example.com", Port: "22"}
	content := "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
	if knownHostsContains(content, endpoint) {
		t.Fatal("expected host missing")
	}

	line := formatKnownHostsEntry(endpoint, githubFallbackSSHKeys[0])
	if !strings.HasPrefix(line, "git.example.com ssh-ed25519 ") {
		t.Fatalf("unexpected known_hosts line: %s", line)
	}
	if !knownHostsContains(content+"\n"+line+"\n", endpoint) {
		t.Fatal("expected host present after append")
	}

	customPort := SSHEndpoint{Host: "git.example.com", Port: "2222"}
	portLine := formatKnownHostsEntry(customPort, githubFallbackSSHKeys[0])
	if !strings.HasPrefix(portLine, "[git.example.com]:2222 ") {
		t.Fatalf("unexpected port marker line: %s", portLine)
	}
	if !knownHostsContains(portLine+"\n", customPort) {
		t.Fatal("expected custom-port host present")
	}
}

func TestResolveKnownHostsForRepoUsesSeedFile(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "known_hosts")
	endpoint := SSHEndpoint{Host: "git.example.com", Port: "22"}
	line := formatKnownHostsEntry(endpoint, githubFallbackSSHKeys[0]) + "\n"
	if err := os.WriteFile(seed, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(KnownHostsPathEnv, seed)
	managedKnownHosts.mu.Lock()
	managedKnownHosts.path = ""
	managedKnownHosts.mu.Unlock()

	path, err := ResolveKnownHostsForRepo("git@git.example.com:group/infra/puppet.git")
	if err != nil {
		t.Fatalf("ResolveKnownHostsForRepo: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !knownHostsContains(string(data), endpoint) {
		t.Fatalf("managed known_hosts missing seeded host:\n%s", data)
	}
}

func generateTestEd25519Key(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}
