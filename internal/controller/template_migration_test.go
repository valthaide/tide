package controller

import (
	"context"
	"testing"

	"github.com/valthaide/tide/internal/api"
)

// Switching an app from compose_path to a fleet template whose rendered
// config matches the running containers must be a dry-run-visible
// no_restart plan, and the real switch must adopt the source without an
// apply (no containers recreated), preserving ID and compose project.
func TestTemplateSwitchDryRunNoRestartAndAdoption(t *testing.T) {
	reg := testRegistry(t)
	const repo = "https://git.example/tenants/backend.git"
	const sha = "1111222233334444555566667777888899990000"

	app := &api.App{
		Name: "backend", RepoURL: repo, Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: sha, LastSyncedCommit: sha, Status: "synced",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateStatus(app.ID, "synced", nil)
	orig, _ := reg.Get(app.ID)
	origProject := orig.ComposeProject()

	rt := &fakeMigRuntime{
		equivalent:        true,
		unchangedServices: []string{"api"},
		keptVols:          []string{origProject + "_pgdata"},
	}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 2})
	rec.ResolveMigrationCommit = func(string, string, []byte, string, string) (string, error) {
		return sha, nil
	}

	// Dry-run: template switch with identical rendered config -> no_restart,
	// changed-services empty, nothing applied, stored source untouched.
	plan, err := rec.MigrateRepo(context.Background(), orig, api.RepoMigrationRequest{
		RepoURL: repo, Template: "web", DryRun: true, Actor: "sre", Reason: "switch to fleet template",
	}, "")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if plan.Mode != "no_restart" || plan.Reason != "config_identical" || len(plan.ChangedServices) != 0 {
		t.Fatalf("dry-run plan must show no_restart with no changed services: %+v", plan)
	}
	if plan.Template != "web" || plan.OldTemplate != "" || !plan.DryRun || plan.Applied {
		t.Fatalf("dry-run plan source fields: %+v", plan)
	}
	stillOld, _ := reg.Get(app.ID)
	if stillOld.Template != "" || stillOld.ComposePath != "compose.yaml" {
		t.Fatalf("dry-run mutated the stored source: %+v", stillOld)
	}

	// Real switch: adopts the template source with no apply call (no restart),
	// same ID/project, compose_path cleared.
	applied, err := rec.MigrateRepo(context.Background(), orig, api.RepoMigrationRequest{
		RepoURL: repo, Template: "web", Actor: "sre", Reason: "switch to fleet template",
	}, "")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if !applied.Applied || applied.Mode != "no_restart" {
		t.Fatalf("applied plan: %+v", applied)
	}
	if len(rt.applyCalls) != 0 {
		t.Fatalf("no_restart adoption must not apply (recreate) anything, applyCalls=%v", rt.applyCalls)
	}
	migrated, _ := reg.Get(app.ID)
	if migrated.ID != orig.ID || migrated.ComposeProject() != origProject {
		t.Fatalf("identity/project changed: id=%s project=%s want %s/%s", migrated.ID, migrated.ComposeProject(), orig.ID, origProject)
	}
	if migrated.Template != "web" || migrated.ComposePath != "" {
		t.Fatalf("stored source after switch: %+v", migrated)
	}

	// Both sources at once is a validation error, before anything is touched.
	if _, err := rec.MigrateRepo(context.Background(), migrated, api.RepoMigrationRequest{
		RepoURL: repo, Template: "web", ComposePath: "compose.yaml", Actor: "sre",
	}, ""); err == nil {
		t.Fatal("expected template+compose_path to be rejected")
	}
}
