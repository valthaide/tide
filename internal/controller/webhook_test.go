package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/credentials"
	"github.com/valthaide/tide/internal/store"
)

func TestWebhookGitLabPushMatchesApp(t *testing.T) {
	db, err := store.NewSQLiteStore(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	creds, err := credentials.NewServiceFromEnv(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(db, creds)
	app := &api.App{
		Name: "demo", RepoURL: "https://gitlab.example/group/demo.git",
		Branch: "main", ComposePath: "compose.yaml", PollInterval: "30s",
		RepoAuthMethod: "public", Status: "synced", LastSeenCommit: "abc",
	}
	if err := reg.AddWithDeployKeyAndEnvs(app, "", nil); err != nil {
		t.Fatal(err)
	}
	_ = reg.UpdateStatus(app.ID, "synced", nil)

	h := &WebhookHandler{Registry: reg, Secret: "hook-secret"}
	body := []byte(`{
		"ref":"refs/heads/main",
		"project":{"git_http_url":"https://gitlab.example/group/demo.git"}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/git", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "hook-secret")
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	rr := httptest.NewRecorder()
	h.GitPush(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	got, _ := reg.Get(app.ID)
	if got.Status != "pending" {
		t.Fatalf("want pending, got %s", got.Status)
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	a := normalizeRepoURL("https://gitlab.example/group/demo.git")
	b := normalizeRepoURL("git@gitlab.example:group/demo.git")
	if a != b {
		t.Fatalf("%q vs %q", a, b)
	}
}
