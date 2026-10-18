package repoauth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	MethodPublic    = "public"
	MethodDeployKey = "deploy_key"

	KnownHostsPathEnv = "CONOPS_KNOWN_HOSTS_FILE"

	defaultSSHPort = "22"
)

var managedKnownHosts struct {
	mu   sync.Mutex
	path string
}

// Official github.com host keys (fallback when api.github.com/meta is unreachable).
var githubFallbackSSHKeys = []string{
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
	"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=",
	"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=",
}

// Official gitlab.com host keys (docs.gitlab.com) used when ssh-keyscan fails.
var gitlabFallbackSSHKeys = []string{
	"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf",
	"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCsj2bNKTBSpIrcuMEZJZoXwjKj99b5b4kNmNnyAUdx6vvqWqDduqdNxJxWaYvHPNLZtovMBRJwPcBU6nJCYbpGYzBSXujvAK2X0tpX5SXm2sjl8gCjC8+DVrLLhtgac1MK0dCmQ5sYIQKthV9vfXzEQMjpv+ih8kpk2qcxbtkpO/oBHdWrep/x9DcXpVUbi0ATKmHzrwIl2aHEGaaS0AJdQyCqvGHOmYlly7aG26x/mZiMut3lFRyCSnFDFkL9qn5lUB4nteF5nJj5EFQtQsEnJ0elsoj7fFvA/Ury757ebpFmvza2YNmNI+s0fHxN1SYvhpIbnzXQBJM8L87pU3PZW9PCTv",
	"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFSMqzJeV9hyXmCsOFmrSysQnRmLvtj5vt/0AAc+RvN5cfNyFLSLdLv/JVBut9Utu9c7OUo2PG1NtcU4UfG0sY=",
}

// SSHEndpoint is the SSH host (and optional non-default port) for a git repo URL.
type SSHEndpoint struct {
	Host string
	Port string // empty or "22" means default SSH port
}

func (e SSHEndpoint) knownHostsMarker() string {
	port := e.normalizedPort()
	if port == defaultSSHPort {
		return e.Host
	}
	return fmt.Sprintf("[%s]:%s", e.Host, port)
}

func (e SSHEndpoint) normalizedPort() string {
	port := strings.TrimSpace(e.Port)
	if port == "" {
		return defaultSSHPort
	}
	return port
}

// NormalizeMethod canonicalizes repo auth methods.
func NormalizeMethod(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "public":
		return MethodPublic
	case "deploy_key", "deploy-key", "deploykey":
		return MethodDeployKey
	default:
		return ""
	}
}

// ValidateCreateInput validates repo config before persistence.
func ValidateCreateInput(repoURL, method, deployKey string) error {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return fmt.Errorf("repo URL is required")
	}

	method = NormalizeMethod(method)
	if method == "" {
		return fmt.Errorf("unsupported repo auth method")
	}

	if method != MethodDeployKey {
		return nil
	}

	deployKey = NormalizeDeployKey(deployKey)
	if strings.TrimSpace(deployKey) == "" {
		return fmt.Errorf("deploy key is required for private repositories")
	}

	if _, err := ParseSSHEndpoint(repoURL); err != nil {
		return err
	}
	if !isSSHRepoURL(repoURL) {
		return fmt.Errorf("deploy key mode requires an SSH repo URL (git@host:path or ssh://...)")
	}

	if _, err := ssh.ParseRawPrivateKey([]byte(deployKey)); err != nil {
		var passErr *ssh.PassphraseMissingError
		if errors.As(err, &passErr) {
			return fmt.Errorf("passphrase-protected deploy keys are not supported")
		}
		return fmt.Errorf("invalid deploy key")
	}

	return nil
}

// NormalizeDeployKey normalizes copy-pasted private keys from forms/JSON payloads.
func NormalizeDeployKey(value string) string {
	normalized := strings.TrimSpace(value)
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	// Some API clients send literal "\n" sequences in JSON strings.
	if strings.Contains(normalized, `\n`) && !strings.Contains(normalized, "\n") {
		normalized = strings.ReplaceAll(normalized, `\n`, "\n")
	}
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return ""
	}
	return normalized + "\n"
}

// ResolveKnownHostsForRepo returns a known_hosts file that includes host keys for the
// repo's SSH endpoint. Keys are seeded from CONOPS_KNOWN_HOSTS_FILE / user / system
// files when present, and missing hosts are fetched via GitHub metadata or ssh-keyscan.
func ResolveKnownHostsForRepo(repoURL string) (string, error) {
	endpoint, err := ParseSSHEndpoint(repoURL)
	if err != nil {
		return "", err
	}
	return ensureKnownHostsForEndpoint(endpoint)
}

// ResolveKnownHostsPath returns the managed known_hosts file used for strict host
// verification. Prefer ResolveKnownHostsForRepo when the target host is known.
func ResolveKnownHostsPath() (string, error) {
	return ensureManagedKnownHosts()
}

// NewHostKeyCallback returns a strict host-key callback.
func NewHostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	return knownhosts.New(path)
}

// BuildSSHCommand builds a strict SSH command for git.
func BuildSSHCommand(keyPath, knownHostsPath string) string {
	return fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -F /dev/null",
		shellQuote(keyPath),
		shellQuote(knownHostsPath),
	)
}

// ParseSSHEndpoint extracts host and port from an SSH or HTTPS git URL.
// HTTPS URLs are accepted for host extraction, but deploy_key mode still requires SSH.
func ParseSSHEndpoint(repoURL string) (SSHEndpoint, error) {
	trimmed := strings.TrimSpace(repoURL)
	if trimmed == "" {
		return SSHEndpoint{}, fmt.Errorf("repo URL is required")
	}

	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return SSHEndpoint{}, fmt.Errorf("invalid repo URL: %w", err)
		}
		host := strings.ToLower(parsed.Hostname())
		if host == "" {
			return SSHEndpoint{}, fmt.Errorf("invalid repo URL host")
		}
		port := parsed.Port()
		if port == "" && strings.EqualFold(parsed.Scheme, "ssh") {
			port = defaultSSHPort
		}
		return SSHEndpoint{Host: host, Port: port}, nil
	}

	// SCP-like: git@host:path or user@host:path
	parts := strings.SplitN(trimmed, "@", 2)
	if len(parts) != 2 {
		return SSHEndpoint{}, fmt.Errorf("invalid repo URL")
	}

	hostAndPath := parts[1]
	colonIdx := strings.Index(hostAndPath, ":")
	if colonIdx <= 0 {
		return SSHEndpoint{}, fmt.Errorf("invalid SSH repo URL")
	}

	hostPart := hostAndPath[:colonIdx]
	// Rare form: user@host:port:path is not standard SCP; reject empty host.
	if hostPart == "" {
		return SSHEndpoint{}, fmt.Errorf("invalid SSH repo URL host")
	}
	// host may be IPv6 in brackets without scheme: not supported in SCP form.
	return SSHEndpoint{Host: strings.ToLower(hostPart), Port: defaultSSHPort}, nil
}

func ensureKnownHostsForEndpoint(endpoint SSHEndpoint) (string, error) {
	path, err := ensureManagedKnownHosts()
	if err != nil {
		return "", err
	}

	managedKnownHosts.mu.Lock()
	defer managedKnownHosts.mu.Unlock()

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed reading known_hosts: %w", err)
	}
	if knownHostsContains(string(content), endpoint) {
		return path, nil
	}

	keys, err := fetchSSHHostKeys(endpoint)
	if err != nil {
		return "", fmt.Errorf(
			"host keys for %s not found in known_hosts and could not be fetched: %w (set %s with the host key, or ensure ssh-keyscan can reach the host)",
			endpoint.knownHostsMarker(),
			err,
			KnownHostsPathEnv,
		)
	}

	var b strings.Builder
	b.Write(content)
	if len(content) > 0 && !bytes.HasSuffix(content, []byte("\n")) {
		b.WriteByte('\n')
	}
	for _, key := range keys {
		line := formatKnownHostsEntry(endpoint, key)
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	if err := writeKnownHostsFile(path, b.String()); err != nil {
		return "", err
	}
	return path, nil
}

func ensureManagedKnownHosts() (string, error) {
	managedKnownHosts.mu.Lock()
	defer managedKnownHosts.mu.Unlock()

	if managedKnownHosts.path != "" {
		if info, err := os.Stat(managedKnownHosts.path); err == nil && !info.IsDir() {
			return managedKnownHosts.path, nil
		}
	}

	seed, seedErr := readSeedKnownHosts()
	dir := filepath.Join(os.TempDir(), "conops-known-hosts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed creating known_hosts dir: %w", err)
	}
	path := filepath.Join(dir, "known_hosts")

	content := seed
	if strings.TrimSpace(content) == "" {
		bootstrapped, err := bootstrapGitHubKnownHostsContent()
		if err != nil {
			if seedErr != nil {
				return "", fmt.Errorf("known_hosts file not found; set %s (%v; %w)", KnownHostsPathEnv, seedErr, err)
			}
			return "", fmt.Errorf("known_hosts file not found; set %s (%w)", KnownHostsPathEnv, err)
		}
		content = bootstrapped
	}

	if err := writeKnownHostsFile(path, content); err != nil {
		return "", err
	}
	managedKnownHosts.path = path
	return path, nil
}

func readSeedKnownHosts() (string, error) {
	if path := strings.TrimSpace(os.Getenv(KnownHostsPathEnv)); path != "" {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("%s points to an invalid file", KnownHostsPathEnv)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", KnownHostsPathEnv, err)
		}
		return string(data), nil
	}

	candidates := make([]string, 0, 2)
	if homeDir, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(homeDir, ".ssh", "known_hosts"))
	}
	candidates = append(candidates, "/etc/ssh/ssh_known_hosts")

	var lastErr error
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			lastErr = err
			continue
		}
		if info.IsDir() {
			lastErr = fmt.Errorf("%s is a directory", path)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		return string(data), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no known_hosts seed available")
	}
	return "", lastErr
}

func bootstrapGitHubKnownHostsContent() (string, error) {
	keys, err := fetchGitHubSSHKeys()
	if err != nil || len(keys) == 0 {
		keys = githubFallbackSSHKeys
	}

	var b strings.Builder
	for _, key := range keys {
		if _, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(key)); parseErr != nil {
			return "", fmt.Errorf("invalid ssh key material for known_hosts generation: %w", parseErr)
		}
		b.WriteString("github.com ")
		b.WriteString(strings.TrimSpace(key))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func fetchSSHHostKeys(endpoint SSHEndpoint) ([]string, error) {
	host := endpoint.Host
	switch host {
	case "github.com":
		keys, err := fetchGitHubSSHKeys()
		if err == nil && len(keys) > 0 {
			return keys, nil
		}
		return githubFallbackSSHKeys, nil
	case "gitlab.com":
		if keys, err := fetchHostKeysViaKeyscan(endpoint); err == nil && len(keys) > 0 {
			return keys, nil
		}
		return gitlabFallbackSSHKeys, nil
	default:
		keys, err := fetchHostKeysViaKeyscan(endpoint)
		if err != nil {
			return nil, err
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("ssh-keyscan returned no keys for %s", endpoint.knownHostsMarker())
		}
		return keys, nil
	}
}

func fetchGitHubSSHKeys() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/meta", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "conops")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed requesting github metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github metadata request failed with status %d", resp.StatusCode)
	}

	var payload struct {
		SSHKeys []string `json:"ssh_keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed decoding github metadata: %w", err)
	}

	return payload.SSHKeys, nil
}

func fetchHostKeysViaKeyscan(endpoint SSHEndpoint) ([]string, error) {
	port := endpoint.normalizedPort()
	host := endpoint.Host

	// Prefer ssh-keyscan when available (present in the ConOps container image).
	if path, err := exec.LookPath("ssh-keyscan"); err == nil {
		cmd := exec.Command(path, "-T", "5", "-p", port, "-t", "rsa,ecdsa,ed25519", host)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("ssh-keyscan failed for %s: %w (%s)", endpoint.knownHostsMarker(), err, strings.TrimSpace(stderr.String()))
		}
		keys := parseKeyscanOutput(stdout.String(), endpoint)
		if len(keys) > 0 {
			return keys, nil
		}
		return nil, fmt.Errorf("ssh-keyscan returned no usable keys for %s", endpoint.knownHostsMarker())
	}

	return nil, fmt.Errorf("ssh-keyscan not found in PATH")
}

func parseKeyscanOutput(output string, _ SSHEndpoint) []string {
	keys := make([]string, 0, 3)
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// ssh-keyscan lines: host keytype key [comment]
		keyMaterial := fields[1] + " " + fields[2]
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keyMaterial)); err != nil {
			continue
		}
		keys = append(keys, keyMaterial)
	}
	return keys
}

func formatKnownHostsEntry(endpoint SSHEndpoint, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
		return ""
	}
	return endpoint.knownHostsMarker() + " " + key
}

func knownHostsContains(content string, endpoint SSHEndpoint) bool {
	markers := knownHostsMarkers(endpoint)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		for _, hostField := range strings.Split(fields[0], ",") {
			hostField = strings.TrimSpace(hostField)
			for _, marker := range markers {
				if strings.EqualFold(hostField, marker) {
					return true
				}
			}
		}
	}
	return false
}

func knownHostsMarkers(endpoint SSHEndpoint) []string {
	host := endpoint.Host
	port := endpoint.normalizedPort()
	markers := []string{host, fmt.Sprintf("[%s]:%s", host, port)}
	if port == defaultSSHPort {
		markers = append(markers, fmt.Sprintf("[%s]", host))
	}
	return markers
}

func writeKnownHostsFile(path, content string) error {
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed writing known_hosts file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("failed finalizing known_hosts file: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func isSSHRepoURL(repoURL string) bool {
	trimmed := strings.TrimSpace(repoURL)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "ssh://") {
		return true
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return false
	}
	return strings.Contains(trimmed, "@") && strings.Contains(trimmed, ":")
}
