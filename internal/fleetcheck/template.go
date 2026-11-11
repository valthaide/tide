package fleetcheck

import (
	"path/filepath"
	"strings"
)

// TemplateFileName is the compose file every fleet template provides.
const TemplateFileName = "compose.yaml"

// OverrideFileName is the optional per-app compose file living in the app
// repository, next to the app (in its project directory).
const OverrideFileName = "compose.override.yaml"

// CheckTemplateName validates a fleet `template` value.
//
// A template name is a single path segment: it selects
// templates/<template>/compose.yaml inside the fleet checkout, so it must
// never contain separators, traversal, or an absolute path.
func CheckTemplateName(raw string) *Issue {
	name := strings.TrimSpace(raw)
	if name == "" {
		return nil
	}
	bad := func(reason string) *Issue {
		return &Issue{Rule: "invalid_template", Message: "invalid_template: " + name + " (" + reason + ")"}
	}
	switch {
	case name == "." || name == ".." || strings.Contains(name, ".."):
		return bad("path traversal is not allowed")
	case strings.ContainsAny(name, "/\\"):
		return bad("template must be a single directory name, not a path")
	case filepath.IsAbs(name) || strings.Contains(name, ":"):
		return bad("template must be a single directory name, not a path")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.'
		if !ok {
			return bad("allowed characters are letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

// ValidTemplateName reports whether raw is an acceptable template name.
func ValidTemplateName(raw string) bool {
	return CheckTemplateName(raw) == nil && strings.TrimSpace(raw) != ""
}
