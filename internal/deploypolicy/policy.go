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
	RequirePipelineSuccess bool        `json:"require_pipeline_success" yaml:"require_pipeline_success"`
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
}

// Normalize trims fields and defaults.
func (p *Policy) Normalize() {
	if p == nil {
		return
	}
	p.HealthTimeout = strings.TrimSpace(p.HealthTimeout)
	p.Build = NormalizeBuild(p.Build)
	p.Vars = NormalizeVars(p.Vars)
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
