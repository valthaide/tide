package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/pollinterval"
	"go.yaml.in/yaml/v3"
)

// HostDocument describes apps that should run on one host.
//
// The `desc` tags are the field descriptions rendered into the generated
// JSON Schema (see schema.go); keep them accurate when adding fields.
type HostDocument struct {
	SchemaRef              string                `yaml:"$schema" desc:"URL of the published JSON Schema for this document, e.g. https://github.com/valthaide/tide/releases/download/v2.3.0/host.schema.json. Used by editors; Tide only checks it is a known field."`
	Schema                 int                   `yaml:"schema" desc:"Fleet format version. Supported version: 1. Omit to use the default. Unknown or newer versions are rejected."`
	Host                   string                `yaml:"host" desc:"Hostname (FQDN) this file describes. Defaults to the controller host when empty."`
	Build                  string                `yaml:"build" desc:"Host build policy: allowed or forbidden. Inherited by apps."`
	RequireDigest          *bool                 `yaml:"require_digest" desc:"Require @sha256 image digest pinning for all apps on this host."`
	AllowedRegistries      []string              `yaml:"allowed_registries" desc:"Registry allowlist inherited by apps."`
	ReconcileInterval      string                `yaml:"reconcile_interval" desc:"Host-wide reconcile interval, e.g. 5m."`
	ReconcileConcurrency   int                   `yaml:"reconcile_concurrency" desc:"Max concurrent stack applies on this host."`
	RetryRejected          string                `yaml:"retry_rejected" desc:"Retry policy for rejected commits: never, on_new_commit, or interval like 30m."`
	Priority               *int                  `yaml:"priority" desc:"Default queue priority inherited by apps (higher runs first)."`
	RequirePipelineSources []string              `yaml:"require_pipeline_sources" desc:"Pipeline sources that gate deployments, inherited by apps."`
	StrictPipelineGate     *bool                 `yaml:"strict_pipeline_gate" desc:"Strict CI gate: absence of a trusted pipeline holds deployment."`
	SharedRequiredJobs     []string              `yaml:"shared_required_jobs" desc:"Required CI jobs shared by all apps on this host."`
	TrustedWebPipelines    *TrustedWebSpec       `yaml:"trusted_web_pipelines" desc:"Acceptance rules for manual (web) pipelines, inherited by apps."`
	Tenants                map[string]TenantSpec `yaml:"tenants" desc:"Per-tenant CI gate rules, keyed by tenant name."`
	CreateVolumes          bool                  `yaml:"create_volumes" desc:"Allow Tide to create missing external Docker volumes."`
	VolumeNamePattern      string                `yaml:"volume_name_pattern" desc:"Regex external volume names must match when create_volumes is on."`
	Apps                   []AppSpec             `yaml:"apps" desc:"Applications that should run on this host."`
}

// TenantSpec groups CI gate rules shared across apps belonging to one tenant.
type TenantSpec struct {
	RequiredJobs        []string        `yaml:"required_jobs" desc:"CI jobs that must succeed for apps of this tenant."`
	SharedRequiredJobs  []string        `yaml:"shared_required_jobs" desc:"Required CI jobs shared across the tenant's apps."`
	TrustedWebPipelines *TrustedWebSpec `yaml:"trusted_web_pipelines" desc:"Acceptance rules for manual (web) pipelines for this tenant."`
}

// TrustedWebSpec configures optional acceptance of manual (`web`) GitLab pipelines.
type TrustedWebSpec struct {
	Enabled          bool     `yaml:"enabled" desc:"Accept manual (web) pipelines when they satisfy the rules below."`
	AllowedUsers     []string `yaml:"allowed_users" desc:"GitLab users allowed to start trusted web pipelines."`
	AllowedVariables []string `yaml:"allowed_variables" desc:"Pipeline variables allowed on trusted web pipelines."`
	RequiredJobs     []string `yaml:"required_jobs" desc:"CI jobs that must succeed on a trusted web pipeline."`
}

// AppSpec is a desired application registration from fleet git.
type AppSpec struct {
	Name        string `yaml:"name" desc:"Application name, unique on the host."`
	Tenant      string `yaml:"tenant" desc:"Tenant this app belongs to (see tenants)."`
	RepoURL     string `yaml:"repo_url" desc:"Git repository of the application."`
	Branch      string `yaml:"branch" desc:"Branch to track. Default: main."`
	Revision    string `yaml:"revision" desc:"Pinned tag or commit SHA; overrides branch tracking."`
	ComposePath string `yaml:"compose_path" desc:"Compose file inside the app repository. Mutually exclusive with template."`
	// Template selects templates/<template>/compose.yaml from this fleet
	// checkout, merged with the app's optional compose.override.yaml.
	// Mutually exclusive with ComposePath.
	Template               string            `yaml:"template" desc:"Fleet template name: templates/<template>/compose.yaml from the fleet checkout, merged with the app's compose.override.yaml. Mutually exclusive with compose_path."`
	RepoAuthMethod         string            `yaml:"repo_auth_method" desc:"Repository auth: public, token, or deploy_key."`
	PollInterval           string            `yaml:"poll_interval" desc:"Git poll interval, e.g. 30s."`
	DeployKeyFile          string            `yaml:"deploy_key_file" desc:"Path to the SSH deploy key for repo_auth_method deploy_key."`
	Suspended              bool              `yaml:"suspended" desc:"Fleet-level pause: keep the app registered but do not deploy."`
	PruneVolumes           bool              `yaml:"prune_volumes" desc:"Prune named volumes when the app is removed."`
	HealthTimeout          string            `yaml:"health_timeout" desc:"How long to wait for containers to become healthy, e.g. 45s."`
	RequirePipelineSuccess bool              `yaml:"require_pipeline_success" desc:"Require a green CI pipeline for the deployed commit."`
	RequirePipelineSources []string          `yaml:"require_pipeline_sources" desc:"Pipeline sources that gate this app."`
	StrictPipelineGate     *bool             `yaml:"strict_pipeline_gate" desc:"Strict CI gate for this app."`
	PipelineRef            string            `yaml:"pipeline_ref" desc:"Git ref whose pipeline gates deployment."`
	RequiredJobs           []string          `yaml:"required_jobs" desc:"CI jobs that must succeed for this app."`
	SharedRequiredJobs     []string          `yaml:"shared_required_jobs" desc:"Required CI jobs shared with other apps from the same repository."`
	TrustedWebPipelines    *TrustedWebSpec   `yaml:"trusted_web_pipelines" desc:"Acceptance rules for manual (web) pipelines for this app."`
	WatchFiles             []WatchFileSpec   `yaml:"watch_files" desc:"Files watched for in-place reload instead of recreate."`
	Build                  string            `yaml:"build" desc:"Build policy for this app: allowed or forbidden."`
	Vars                   map[string]string `yaml:"vars" desc:"Compose interpolation variables for this app."`
	Project                string            `yaml:"project" desc:"Explicit Compose project name (adopts a pre-existing stack)."`
	SyncTimeout            string            `yaml:"sync_timeout" desc:"Timeout for one sync of this app."`
	PullTimeout            string            `yaml:"pull_timeout" desc:"Timeout for image pulls of this app."`
	OneshotTimeout         string            `yaml:"oneshot_timeout" desc:"Timeout for one-shot services, e.g. DB migrations."`
	RequireDigest          *bool             `yaml:"require_digest" desc:"Require @sha256 image digest pinning for this app."`
	PublicVars             []string          `yaml:"public_vars" desc:"Vars safe to show in status output."`
	AllowedRegistries      []string          `yaml:"allowed_registries" desc:"Registry allowlist for this app."`
	EnvFiles               []string          `yaml:"env_files" desc:"External env files on the host whose changes trigger reconciliation."`
	PolicyExceptions       []PolicyException `yaml:"policy_exceptions" desc:"Per-service compose policy exceptions."`
	ReconcileInterval      string            `yaml:"reconcile_interval" desc:"Reconcile interval for this app, e.g. 5m."`
	RetryRejected          string            `yaml:"retry_rejected" desc:"Retry policy for rejected commits of this app."`
	Priority               *int              `yaml:"priority" desc:"Queue priority for this app (higher runs first)."`
}

// PolicyException allows one compose rule for one service while policy mode is warn.
type PolicyException struct {
	Service string `yaml:"service" desc:"Compose service the exception applies to."`
	Rule    string `yaml:"rule" desc:"Policy rule being excepted."`
	Reason  string `yaml:"reason" desc:"Why the exception exists (audited)."`
}

// WatchFileSpec is a path watched for content changes after sync.
type WatchFileSpec struct {
	Path          string `yaml:"path" desc:"Path inside the app checkout to watch."`
	Action        string `yaml:"action" desc:"On change: restart or reload."` // restart | reload
	Service       string `yaml:"service" desc:"Service to restart/reload."`
	ReloadURL     string `yaml:"reload_url" desc:"HTTP endpoint called on reload."`
	ReloadCommand string `yaml:"reload_command" desc:"Command executed in the container on reload, e.g. kill -HUP 1."`
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
		RepoURL:       envcompat.Getenv("TIDE_FLEET_REPO_URL"),
		Branch:        envcompat.Getenv("TIDE_FLEET_BRANCH"),
		Host:          envcompat.Getenv("TIDE_FLEET_HOST"),
		AuthMethod:    envcompat.Getenv("TIDE_FLEET_AUTH_METHOD"),
		Token:         envcompat.Getenv("TIDE_FLEET_TOKEN"),
		KeyFile:       envcompat.Getenv("TIDE_FLEET_DEPLOY_KEY_FILE"),
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
	if v := envcompat.Getenv("TIDE_FLEET_MISSING_CYCLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("invalid TIDE_FLEET_MISSING_CYCLES: %s", v)
		}
		cfg.MissingCycles = n
	}
	if v := envcompat.Getenv("TIDE_FLEET_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid TIDE_FLEET_INTERVAL: %w", err)
		}
		if d > 0 {
			cfg.Interval = d
		}
	}
	// Global git token fallback for fleet HTTPS
	if cfg.Token == "" {
		cfg.Token = envcompat.Getenv("TIDE_GIT_TOKEN")
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
	if err := CheckSchemaVersion(doc.Schema); err != nil {
		return nil, err
	}
	doc.Host = strings.TrimSpace(doc.Host)
	doc.Build = deploypolicy.NormalizeBuild(doc.Build)
	for i := range doc.Apps {
		doc.Apps[i].Name = strings.TrimSpace(doc.Apps[i].Name)
		doc.Apps[i].Tenant = strings.TrimSpace(doc.Apps[i].Tenant)
		doc.Apps[i].RepoURL = strings.TrimSpace(doc.Apps[i].RepoURL)
		if doc.Apps[i].Branch == "" {
			doc.Apps[i].Branch = "main"
		}
		doc.Apps[i].Template = strings.TrimSpace(doc.Apps[i].Template)
		doc.Apps[i].ComposePath = strings.TrimSpace(doc.Apps[i].ComposePath)
		// Only default compose_path when no template is selected. An explicit
		// compose_path next to a template is kept as-is here so validation can
		// report the conflict instead of silently dropping one source.
		if doc.Apps[i].ComposePath == "" && doc.Apps[i].Template == "" {
			doc.Apps[i].ComposePath = "docker-compose.yml"
		}
		doc.Apps[i].Revision = strings.TrimSpace(doc.Apps[i].Revision)
		doc.Apps[i].PipelineRef = strings.TrimSpace(doc.Apps[i].PipelineRef)
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
