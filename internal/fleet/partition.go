package fleet

import (
	"strings"

	"github.com/valthaide/tide/internal/fleetcheck"
)

// PartitionIssues splits validation issues into host-level (stop the whole
// host reconcile) and per-registration (disable only that app).
//
// Broken YAML / unsupported schema / host-field errors stay host-level.
// Unknown template, bad app_dir, and other apps[N] problems are per-app.
func PartitionIssues(issues []fleetcheck.Issue) (host []fleetcheck.Issue, byApp map[string][]fleetcheck.Issue) {
	byApp = map[string][]fleetcheck.Issue{}
	for _, issue := range issues {
		if isHostLevelIssue(issue) {
			host = append(host, issue)
			continue
		}
		name := strings.TrimSpace(issue.App)
		if name == "" {
			host = append(host, issue)
			continue
		}
		byApp[name] = append(byApp[name], issue)
	}
	return host, byApp
}

func isHostLevelIssue(issue fleetcheck.Issue) bool {
	switch issue.Rule {
	case "duplicate_app_name", "name_conflict", "project_conflict",
		"invalid_concurrency", "invalid_volume_pattern", "read_error", "parse_error":
		return true
	}
	path := strings.TrimSpace(issue.Path)
	if path == "" || path == "(document)" {
		return true
	}
	if strings.HasPrefix(path, "apps[") {
		return false
	}
	// Host-level keys: reconcile_interval, build, tenants, …
	return true
}

// FirstIssueMessage joins issues into one operator-facing line.
func FirstIssueMessage(issues []fleetcheck.Issue) string {
	if len(issues) == 0 {
		return ""
	}
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		msg := strings.TrimSpace(issue.Message)
		if msg == "" {
			msg = issue.Rule
		}
		parts = append(parts, msg)
	}
	return strings.Join(parts, "; ")
}
