package fleet

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/valthaide/tide/internal/fleetcheck"
	"go.yaml.in/yaml/v3"
)

// schemaIssues validates a raw host document against the generated JSON
// Schema (types, unknown properties, the format-version enum). It
// implements exactly the subset HostSchemaJSON emits ($ref/$defs, allOf,
// type incl. null unions, properties, additionalProperties, items, enum),
// so `tide validate`, `tidectl validate`, and the controller all judge a
// file by the same schema an editor uses. Unknown-property issues carry
// rule "unknown_field" so callers can apply TIDE_FLEET_STRICT to them.
func schemaIssues(data []byte) ([]fleetcheck.Issue, error) {
	schemaJSON, err := HostSchemaJSON("dev")
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return nil, err
	}
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, err // ParseHostDocument reports the same error upstream
	}
	v := &schemaValidator{defs: map[string]map[string]any{}}
	if defs, ok := schema["$defs"].(map[string]any); ok {
		for name, def := range defs {
			if m, ok := def.(map[string]any); ok {
				v.defs[name] = m
			}
		}
	}
	v.check(value, schema, "")
	return v.issues, nil
}

type schemaValidator struct {
	defs   map[string]map[string]any
	issues []fleetcheck.Issue
}

func (v *schemaValidator) fail(path, rule, msg string) {
	v.issues = append(v.issues, fleetcheck.Issue{Path: path, Rule: rule, Message: msg})
}

func (v *schemaValidator) check(value any, schema map[string]any, path string) {
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		if def, ok := v.defs[name]; ok {
			v.check(value, def, path)
		}
		return
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, sub := range allOf {
			if m, ok := sub.(map[string]any); ok {
				v.check(value, m, path)
			}
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		if !enumContains(enum, value) {
			v.fail(path, "unsupported_fleet_schema", fmt.Sprintf("%s: value %v is not one of %v", displayPath(path), value, enumValues(enum)))
		}
		return
	}
	types := schemaTypes(schema["type"])
	if len(types) > 0 && !typeMatches(value, types) {
		v.fail(path, "schema_type", fmt.Sprintf("%s: expected %s, got %s", displayPath(path), strings.Join(types, " or "), yamlTypeName(value)))
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		if addl, ok := schema["additionalProperties"]; ok {
			if flag, isBool := addl.(bool); isBool && !flag {
				for key := range typed {
					if _, known := props[key]; !known {
						p := joinPath(path, key)
						v.fail(p, "unknown_field", "unknown_field: "+p)
					}
				}
			} else if sub, isSchema := addl.(map[string]any); isSchema {
				for key, item := range typed {
					if _, known := props[key]; !known {
						v.check(item, sub, joinPath(path, key))
					}
				}
			}
		}
		for key, sub := range props {
			if item, present := typed[key]; present {
				if m, ok := sub.(map[string]any); ok {
					v.check(item, m, joinPath(path, key))
				}
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range typed {
				v.check(item, items, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
}

func displayPath(path string) string {
	if path == "" {
		return "(document)"
	}
	return path
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func schemaTypes(raw any) []string {
	switch t := raw.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func typeMatches(value any, types []string) bool {
	for _, typ := range types {
		switch typ {
		case "null":
			if value == nil {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		case "integer":
			switch value.(type) {
			case int, int64, uint64:
				return true
			}
		case "number":
			switch value.(type) {
			case int, int64, uint64, float64:
				return true
			}
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		}
	}
	return false
}

func yamlTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, uint64:
		return "integer"
	case float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func enumContains(enum []any, value any) bool {
	for _, candidate := range enum {
		// JSON numbers decode as float64; YAML integers as int.
		if fmt.Sprintf("%v", candidate) == fmt.Sprintf("%v", value) {
			return true
		}
		if f, ok := candidate.(float64); ok {
			switch n := value.(type) {
			case int:
				if f == float64(n) {
					return true
				}
			case int64:
				if f == float64(n) {
					return true
				}
			}
		}
	}
	return false
}

func enumValues(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, fmt.Sprintf("%v", e))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
