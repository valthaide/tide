package deploypolicy

import (
	"encoding/json"
	"strings"
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
}

// Normalize trims fields and defaults.
func (p *Policy) Normalize() {
	if p == nil {
		return
	}
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
