package fleet

import (
	"testing"

	"github.com/valthaide/tide/internal/fleetcheck"
)

func TestPartitionIssuesKeepsHostErrorsHostLevel(t *testing.T) {
	host, byApp := PartitionIssues([]fleetcheck.Issue{
		{Path: "reconcile_interval", Rule: "invalid_interval", Message: "bad interval"},
		{App: "web", Path: "apps[0].template", Rule: "template_not_found", Message: "template_not_found: web"},
		{App: "api", Path: "apps[1].app_dir", Rule: "invalid_app_dir", Message: "invalid_app_dir"},
		{App: "a", Path: "apps[0].name", Rule: "duplicate_app_name", Message: "duplicate"},
	})
	if len(host) != 2 {
		t.Fatalf("host issues: %+v", host)
	}
	if len(byApp) != 2 || len(byApp["web"]) != 1 || len(byApp["api"]) != 1 {
		t.Fatalf("byApp: %+v", byApp)
	}
}
