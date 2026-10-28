package fleet

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

const supportedSchema = 1

// StrictMode is CONOPS_FLEET_STRICT. Empty and warn keep unknown fields.
// enforce rejects the host file before any app is changed.
func StrictMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "enforce":
		return "enforce"
	default:
		return "warn"
	}
}

// UnknownFields lists fleet keys this version does not know.
// A schema newer than this controller is returned as an error.
func UnknownFields(data []byte) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	doc := &root
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		doc = root.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil, nil
	}
	var unknown []string
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := doc.Content[i].Value
		val := doc.Content[i+1]
		switch key {
		case "schema":
			if val.Kind == yaml.ScalarNode {
				var n int
				if _, err := fmt.Sscan(val.Value, &n); err == nil && n > supportedSchema {
					return nil, fmt.Errorf("unknown_field: schema %d is newer than %d", n, supportedSchema)
				}
			}
		case "host", "build", "require_digest", "allowed_registries", "reconcile_interval", "reconcile_concurrency", "retry_rejected", "priority", "require_pipeline_sources":
		case "apps":
			unknown = append(unknown, unknownAppFields(val)...)
		default:
			unknown = append(unknown, key)
		}
	}
	return unknown, nil
}

func unknownAppFields(seq *yaml.Node) []string {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	known := map[string]struct{}{
		"name": {}, "repo_url": {}, "branch": {}, "revision": {}, "compose_path": {},
		"repo_auth_method": {}, "poll_interval": {}, "deploy_key_file": {}, "suspended": {},
		"prune_volumes": {}, "health_timeout": {}, "require_pipeline_success": {}, "require_pipeline_sources": {},
		"watch_files": {}, "build": {}, "vars": {}, "project": {},
		"sync_timeout": {}, "pull_timeout": {}, "oneshot_timeout": {},
		"require_digest": {}, "public_vars": {}, "allowed_registries": {},
		"env_files": {}, "policy_exceptions": {},
		"reconcile_interval": {}, "retry_rejected": {}, "priority": {},
	}
	var out []string
	for i, app := range seq.Content {
		if app.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(app.Content); j += 2 {
			key := app.Content[j].Value
			if _, ok := known[key]; !ok {
				out = append(out, fmt.Sprintf("apps[%d].%s", i, key))
			}
		}
	}
	return out
}
