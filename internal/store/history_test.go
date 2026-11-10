package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/valthaide/tide/internal/api"
)

func TestRevisionHistoryKeepsFive(t *testing.T) {
	db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for i, sha := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := db.InsertAppRevision(ctx, api.Revision{AppID: "app", GitSHA: sha, VarsJSON: string(rune('0' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertAppRevision(ctx, api.Revision{AppID: "app", GitSHA: "f", VarsJSON: "5"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListAppRevisions(ctx, "app", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0].GitSHA != "f" || got[4].GitSHA != "b" {
		t.Fatalf("%+v", got)
	}
}
