package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/valthaide/conops-private/internal/deploypolicy"
	"github.com/valthaide/conops-private/internal/pollinterval"
	"go.yaml.in/yaml/v3"
)

// HostDocument describes apps that should run on one host.
type HostDocument struct {
	Schema               int       `yaml:"schema"`
	Host                 string    `yaml:"host"`
	Build                string    `yaml:"build"`
	RequireDigest        *bool     `yaml:"require_digest"`
	AllowedRegistries    []string  `yaml:"allowed_registries"`
	ReconcileInterval    string    `yaml:"reconcile_interval"`
	ReconcileConcurrency int       `yaml:"reconcile_concurrency"`
	RetryRejected        string    `yaml:"retry_rejected"`
	Priority             *int      `yaml:"priority"`
	Apps                 []AppSpec `yaml:"apps"`
}

// AppSpec is a desired application registration from fleet git.
type AppSpec struct {
	Name                   string            `yaml:"name"`
	RepoURL                string            `yaml:"repo_url"`
	Branch                 string            `yaml:"branch"`
	Revision               string            `yaml:"revision"`
	ComposePath            string            `yaml:"compose_path"`
	RepoAuthMethod         string            `yaml:"repo_auth_method"`
	PollInterval           string            `yaml:"poll_interval"`
	DeployKeyFile          string            `yaml:"deploy_key_file"`
	Suspended              bool              `yaml:"suspended"`
	PruneVolumes           bool              `yaml:"prune_volumes"`
	HealthTimeout          string            `yaml:"health_timeout"`
	RequirePipelineSuccess bool              `yaml:"require_pipeline_success"`
	WatchFiles             []WatchFileSpec   `yaml:"watch_files"`
	Build                  string            `yaml:"build"`
	Vars                   map[string]string `yaml:"vars"`
	Project                string            `yaml:"project"`
	SyncTimeout            string            `yaml:"sync_timeout"`
	PullTimeout            string            `yaml:"pull_timeout"`
	OneshotTimeout         string            `yaml:"oneshot_timeout"`
	RequireDigest          *bool             `yaml:"require_digest"`
	PublicVars             []string          `yaml:"public_vars"`
	AllowedRegistries      []string          `yaml:"allowed_registries"`
	EnvFiles               []string          `yaml:"env_files"`
	PolicyExceptions       []PolicyException `yaml:"policy_exceptions"`
	ReconcileInterval      string            `yaml:"reconcile_interval"`
	RetryRejected          string            `yaml:"retry_rejected"`
	Priority               *int              `yaml:"priority"`
}

// PolicyException allows one compose rule for one service while policy mode is warn.
type PolicyException struct {
	Service string `yaml:"service"`
	Rule    string `yaml:"rule"`
	Reason  string `yaml:"reason"`
}

// WatchFileSpec is a path watched for content changes after sync.
type WatchFileSpec struct {
	Path          string `yaml:"path"`
	Action        string `yaml:"action"` // restart | reload
	Service       string `yaml:"service"`
	ReloadURL     string `yaml:"reload_url"`
	ReloadCommand string `yaml:"reload_command"`
}

// Config controls fleet reconciliation.
type Config struct {
	RepoURL       string
	Branch        string
	Host          string
	AuthMethod    string
	Token         string
	KeyFile       string
	CacheDir      string
	Interval      time.Duration
	MissingCycles int
}

// LoadConfigFromEnv reads fleet settings. Empty RepoURL means fleet disabled.
func LoadConfigFromEnv(dataDir string) (Config, error) {
	cfg := Config{
		RepoURL:       strings.TrimSpace(os.Getenv("CONOPS_FLEET_REPO_URL")),
		Branch:        strings.TrimSpace(os.Getenv("CONOPS_FLEET_BRANCH")),
		Host:          strings.TrimSpace(os.Getenv("CONOPS_FLEET_HOST")),
		AuthMethod:    strings.TrimSpace(os.Getenv("CONOPS_FLEET_AUTH_METHOD")),
		Token:         strings.TrimSpace(os.Getenv("CONOPS_FLEET_TOKEN")),
		KeyFile:       strings.TrimSpace(os.Getenv("CONOPS_FLEET_DEPLOY_KEY_FILE")),
		CacheDir:      filepath.Join(dataDir, "fleet-cache"),
		Interval:      60 * time.Second,
		MissingCycles: 3,
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.Host == "" {
		h, _ := os.Hostname()
		cfg.Host = h
	}
	if cfg.AuthMethod == "" {
		if cfg.Token != "" {
			cfg.AuthMethod = "token"
		} else if cfg.KeyFile != "" {
			cfg.AuthMethod = "deploy_key"
		} else {
			cfg.AuthMethod = "public"
		}
	}
	if v := strings.TrimSpace(os.Getenv("CONOPS_FLEET_MISSING_CYCLES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("invalid CONOPS_FLEET_MISSING_CYCLES: %s", v)
		}
		cfg.MissingCycles = n
	}
	if v := strings.TrimSpace(os.Getenv("CONOPS_FLEET_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid CONOPS_FLEET_INTERVAL: %w", err)
		}
		if d > 0 {
			cfg.Interval = d
		}
	}
	// Global git token fallback for fleet HTTPS
	if cfg.Token == "" {
		cfg.Token = strings.TrimSpace(os.Getenv("CONOPS_GIT_TOKEN"))
	}
	return cfg, nil
}

// Enabled reports whether fleet sync is configured.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.RepoURL) != ""
}

// ParseHostDocument parses a single host YAML document.
func ParseHostDocument(data []byte) (*HostDocument, error) {
	var doc HostDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	doc.Host = strings.TrimSpace(doc.Host)
	doc.Build = deploypolicy.NormalizeBuild(doc.Build)
	for i := range doc.Apps {
		doc.Apps[i].Name = strings.TrimSpace(doc.Apps[i].Name)
		doc.Apps[i].RepoURL = strings.TrimSpace(doc.Apps[i].RepoURL)
		if doc.Apps[i].Branch == "" {
			doc.Apps[i].Branch = "main"
		}
		if doc.Apps[i].ComposePath == "" {
			doc.Apps[i].ComposePath = "docker-compose.yml"
		}
		doc.Apps[i].Revision = strings.TrimSpace(doc.Apps[i].Revision)
		if doc.Apps[i].PollInterval == "" {
			doc.Apps[i].PollInterval = pollinterval.AppDefault().String()
		}
		if doc.Apps[i].RepoAuthMethod == "" {
			doc.Apps[i].RepoAuthMethod = "public"
		}
		doc.Apps[i].Build = deploypolicy.NormalizeBuild(doc.Apps[i].Build)
		doc.Apps[i].Vars = deploypolicy.NormalizeVars(doc.Apps[i].Vars)
		doc.Apps[i].Project = strings.TrimSpace(doc.Apps[i].Project)
	}
	return &doc, nil
}

// FindHostFile returns path to YAML for hostname inside root (hosts/<host>.yaml or matching host: field).
func FindHostFile(root, hostname string) (string, error) {
	hostname = strings.TrimSpace(hostname)
	candidates := []string{
		filepath.Join(root, "hosts", hostname+".yaml"),
		filepath.Join(root, "hosts", hostname+".yml"),
		filepath.Join(root, hostname+".yaml"),
		filepath.Join(root, hostname+".yml"),
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}

	hostsDir := filepath.Join(root, "hosts")
	entries, err := os.ReadDir(hostsDir)
	if err != nil {
		return "", fmt.Errorf("fleet host file for %q not found", hostname)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(hostsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		doc, err := ParseHostDocument(data)
		if err != nil {
			continue
		}
		if strings.EqualFold(doc.Host, hostname) {
			return path, nil
		}
	}
	return "", fmt.Errorf("fleet host file for %q not found", hostname)
}

// IsHostFileMissing reports the error returned when hosts/<fqdn>.yaml is absent.
// Other fleet failures (clone, parse) are not this condition.
func IsHostFileMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "fleet host file for ") && strings.Contains(msg, " not found")
}
