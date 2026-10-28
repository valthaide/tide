package deploypolicy

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

const (
	BuildAllowed   = "allowed"
	BuildForbidden = "forbidden"
)

// WatchFile describes a path to observe and an action when its content changes.
type WatchFile struct {
	Path          string `json:"path" yaml:"path"`
	Action        string `json:"action" yaml:"action"` // restart | reload
	Service       string `json:"service,omitempty" yaml:"service"`
	ReloadURL     string `json:"reload_url,omitempty" yaml:"reload_url"`
	ReloadCommand string `json:"reload_command,omitempty" yaml:"reload_command"`
}

// Policy is per-app deploy behaviour from fleet YAML.
type Policy struct {
	RequirePipelineSuccess bool `json:"require_pipeline_success" yaml:"require_pipeline_success"`
	// RequirePipelineSources limits which GitLab pipeline sources decide the gate.
	// Empty means push and merge_request_event.
	RequirePipelineSources []string    `json:"require_pipeline_sources,omitempty" yaml:"require_pipeline_sources"`
	WatchFiles             []WatchFile `json:"watch_files,omitempty" yaml:"watch_files"`
	// PruneVolumes removes this app's named compose volumes when the app is deleted.
	// Images used only by this app are always removed; volumes stay unless this is set.
	PruneVolumes bool `json:"prune_volumes" yaml:"prune_volumes"`
	// HealthTimeout is how long a new revision may take to become healthy before rollback.
	// Empty means the controller default (120s).
	HealthTimeout string `json:"health_timeout,omitempty" yaml:"health_timeout"`
	// Build is "allowed" or "forbidden". Empty inherits CONOPS_BUILD, which defaults to allowed.
	Build string `json:"build,omitempty" yaml:"build"`
	// Vars are passed to docker compose for ${NAME} interpolation.
	Vars map[string]string `json:"vars,omitempty" yaml:"vars"`
	// Project is the compose project name. Empty keeps the name derived from the app id.
	// Set it to the existing project to adopt its containers and volumes.
	Project string `json:"project,omitempty" yaml:"project"`
	// SyncTimeout overrides the controller sync timeout for this app. Empty uses the controller default.
	SyncTimeout string `json:"sync_timeout,omitempty" yaml:"sync_timeout"`
	// PullTimeout is how long a pull may go without output before it is cancelled.
	PullTimeout string `json:"pull_timeout,omitempty" yaml:"pull_timeout"`
	// OneshotTimeout is how long one-shot services may run before they are stopped.
	OneshotTimeout string `json:"oneshot_timeout,omitempty" yaml:"oneshot_timeout"`
	// RequireDigest rejects images that are not pinned with @sha256. Default false.
	RequireDigest bool `json:"require_digest,omitempty" yaml:"require_digest"`
	// PublicVars names variables that may appear in status and metrics.
	PublicVars []string `json:"public_vars,omitempty" yaml:"public_vars"`
	// AllowedRegistries, when non-empty, rejects images from other registries.
	AllowedRegistries []string `json:"allowed_registries,omitempty" yaml:"allowed_registries"`
	// EnvFiles are host files outside git. Their values interpolate compose and are not logged.
	EnvFiles []string `json:"env_files,omitempty" yaml:"env_files"`
	// PolicyExceptions name service rules that warn mode may allow.
	PolicyExceptions []PolicyException `json:"policy_exceptions,omitempty" yaml:"policy_exceptions"`
	// ReconcileInterval is a Go duration or off. Empty inherits the controller default.
	ReconcileInterval string `json:"reconcile_interval,omitempty" yaml:"reconcile_interval"`
	// RetryRejected is error_only, always, or never. Empty means error_only.
	RetryRejected string `json:"retry_rejected,omitempty" yaml:"retry_rejected"`
	// Priority orders apps inside one trigger class. Higher runs first. Nil means 0.
	Priority *int `json:"priority,omitempty" yaml:"priority"`
	// CreateVolumes creates a missing external volume when its name matches VolumeNamePattern.
	CreateVolumes bool `json:"create_volumes,omitempty" yaml:"create_volumes"`
	// VolumeNamePattern is the host regular expression for volumes this controller may create.
	VolumeNamePattern string `json:"volume_name_pattern,omitempty" yaml:"volume_name_pattern"`
}

// PolicyException is one allowed compose policy rule for one service.
type PolicyException struct {
	Service string `json:"service" yaml:"service"`
	Rule    string `json:"rule" yaml:"rule"`
	Reason  string `json:"reason,omitempty" yaml:"reason"`
}

// Normalize trims fields and defaults.
func (p *Policy) Normalize() {
	if p == nil {
		return
	}
	p.HealthTimeout = strings.TrimSpace(p.HealthTimeout)
	p.Build = NormalizeBuild(p.Build)
	p.Vars = NormalizeVars(p.Vars)
	p.Project = strings.TrimSpace(p.Project)
	out := p.WatchFiles[:0]
	for _, w := range p.WatchFiles {
		w.Path = strings.TrimSpace(w.Path)
		w.Action = strings.ToLower(strings.TrimSpace(w.Action))
		w.Service = strings.TrimSpace(w.Service)
		w.ReloadURL = strings.TrimSpace(w.ReloadURL)
		w.ReloadCommand = strings.TrimSpace(w.ReloadCommand)
		if w.Path == "" {
			continue
		}
		if w.Action == "" {
			w.Action = "restart"
		}
		out = append(out, w)
	}
	p.WatchFiles = out
}

// Encode serializes policy for DB storage.
func Encode(p Policy) string {
	p.Normalize()
	data, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// NormalizeBuild returns allowed, forbidden, or empty when the value is unset.
func NormalizeBuild(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case BuildForbidden:
		return BuildForbidden
	case BuildAllowed:
		return BuildAllowed
	default:
		return ""
	}
}

// EffectiveBuild resolves app policy, then CONOPS_BUILD, then allowed.
func (p Policy) EffectiveBuild() string {
	switch NormalizeBuild(p.Build) {
	case BuildForbidden:
		return BuildForbidden
	case BuildAllowed:
		return BuildAllowed
	default:
		if strings.EqualFold(strings.TrimSpace(os.Getenv("CONOPS_BUILD")), BuildForbidden) {
			return BuildForbidden
		}
		return BuildAllowed
	}
}

// NormalizeVars trims keys and values and drops empty keys.
func NormalizeVars(vars map[string]string) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EncodeVars serializes vars as a stable JSON object. Empty is "".
func EncodeVars(vars map[string]string) string {
	m := NormalizeVars(vars)
	if len(m) == 0 {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// DecodeVars parses EncodeVars output.
func DecodeVars(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return NormalizeVars(m)
}

// VarsEqual reports whether two var maps interpolate the same way.
func VarsEqual(a, b map[string]string) bool {
	return EncodeVars(a) == EncodeVars(b)
}

// VarsLabel renders vars in a stable order for status text.
func VarsLabel(vars map[string]string) string {
	m := NormalizeVars(vars)
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

// Decode parses policy JSON; empty → zero policy.
func Decode(raw string) Policy {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Policy{}
	}
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Policy{}
	}
	p.Normalize()
	return p
}
