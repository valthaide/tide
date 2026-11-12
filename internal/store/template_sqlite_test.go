package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/valthaide/tide/internal/api"
)

// TestTemplateColumnsRoundTripAndMigration covers the v2.2 additive schema:
// template + last_applied_template_hash persist, survive UpdateAppRepository
// source switches, and default to "" for compose_path apps.
func TestTemplateColumnsRoundTripAndMigration(t *testing.T) {
	db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	app := &api.App{ID: "a1", Name: "web", RepoURL: "https://git.example/web.git", Branch: "main", Template: "web", Status: "pending"}
	if err := db.CreateApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetApp(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Template != "web" || got.ComposePath != "" || got.LastAppliedTemplateHash != "" {
		t.Fatalf("created app: %+v", got)
	}

	if err := db.UpdateAppTemplateHash(ctx, "a1", "hash1"); err != nil {
		t.Fatal(err)
	}
	// Switch the source back to compose_path (template cleared).
	if err := db.UpdateAppRepository(ctx, "a1", app.RepoURL, "public", "main", "", "docker-compose.yml", "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetApp(ctx, "a1")
	if got.Template != "" || got.ComposePath != "docker-compose.yml" || got.LastAppliedTemplateHash != "hash1" {
		t.Fatalf("after source switch: %+v", got)
	}
	// And back to a template.
	if err := db.UpdateAppRepository(ctx, "a1", app.RepoURL, "public", "main", "", "", "api", "backend"); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListApps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Template != "api" || list[0].AppDir != "backend" {
		t.Fatalf("list: %+v", list)
	}
	// app_dir round-trips on create too.
	app2 := &api.App{ID: "a2", Name: "pg", RepoURL: "https://git.example/pg.git", Branch: "main", Template: "db", AppDir: "tools/pgadmin", Status: "pending"}
	if err := db.CreateApp(ctx, app2); err != nil {
		t.Fatal(err)
	}
	got2, err := db.GetApp(ctx, "a2")
	if err != nil || got2.AppDir != "tools/pgadmin" {
		t.Fatalf("created app_dir app: %+v err %v", got2, err)
	}
}
