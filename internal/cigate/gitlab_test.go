package cigate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIBaseAndProjectPath(t *testing.T) {
	base, err := APIBaseFromRepoURL("https://git.example/group/proj.git")
	if err != nil || base != "https://git.example/api/v4" {
		t.Fatalf("base=%q err=%v", base, err)
	}
	path, err := ProjectPathFromRepoURL("https://git.example/group/proj.git")
	if err != nil || path != "group%2Fproj" {
		t.Fatalf("path=%q err=%v", path, err)
	}
}

func TestCheckCommitSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") == "" {
			t.Fatal("missing token header")
		}
		_, _ = w.Write([]byte(`[{"status":"success","web_url":"https://git/x"}]`))
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc123")
	if !res.Allowed || res.Reason != "ok" {
		t.Fatalf("%+v", res)
	}
}

func TestCheckCommitBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"status":"failed"}]`))
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Allowed || res.Reason != "blocked_by_ci" {
		t.Fatalf("%+v", res)
	}
}

func TestCheckCommitFailClosedOnError(t *testing.T) {
	c := &Checker{APIBase: "http://127.0.0.1:1", Token: "tok", Client: &http.Client{}}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Allowed {
		t.Fatal("expected fail closed")
	}
}
