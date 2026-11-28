package cigate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeGitLab serves pipelines, commit info, and commit diffs for the
// parent-inheritance tests. pipelines maps commit SHA to its pipelines JSON;
// commits maps SHA to its parent ("" = root); diffs maps SHA to changed
// paths. diffHits counts diff endpoint calls per SHA to verify caching.
type fakeGitLab struct {
	pipelines map[string]string
	commits   map[string]string
	diffs     map[string][]string
	diffHits  map[string]*atomic.Int32
}

func (f *fakeGitLab) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		const commitsPrefix = "/projects/g/p/repository/commits/"
		if strings.HasPrefix(path, commitsPrefix) {
			rest := strings.TrimPrefix(path, commitsPrefix)
			if strings.HasSuffix(rest, "/diff") {
				sha := strings.TrimSuffix(rest, "/diff")
				if hit, ok := f.diffHits[sha]; ok {
					hit.Add(1)
				}
				paths, ok := f.diffs[sha]
				if !ok {
					http.NotFound(w, r)
					return
				}
				var b strings.Builder
				b.WriteString("[")
				for i, p := range paths {
					if i > 0 {
						b.WriteString(",")
					}
					fmt.Fprintf(&b, `{"old_path":%q,"new_path":%q}`, p, p)
				}
				b.WriteString("]")
				_, _ = w.Write([]byte(b.String()))
				return
			}
			parent, ok := f.commits[rest]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if parent == "" {
				_, _ = w.Write([]byte(`{"id":"` + rest + `","parent_ids":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"` + rest + `","parent_ids":["` + parent + `"]}`))
			return
		}
		if path == "/projects/g/p/pipelines" {
			sha := r.URL.Query().Get("sha")
			if body, ok := f.pipelines[sha]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	})
}

func deployFilesFixture() []string {
	return []string{"compose.yaml", ".env", "compose.override.yaml"}
}

func TestNoPipelineInheritsGreenParentWhenOnlyCIFileChanged(t *testing.T) {
	fake := &fakeGitLab{
		pipelines: map[string]string{
			"p0": `[{"id":7,"sha":"p0","ref":"main","status":"success","source":"push","web_url":"https://git/p/7"}]`,
		},
		commits:  map[string]string{"c1": "p0"},
		diffs:    map[string][]string{"c1": {".gitlab-ci.yml"}},
		diffHits: map[string]*atomic.Int32{"c1": {}},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	req := CheckRequest{
		RepoURL:     "https://git.example/g/p.git",
		CommitSHA:   "c1",
		Ref:         "main",
		DeployFiles: deployFilesFixture(),
	}

	res := c.Check(context.Background(), req)
	if !res.Allowed || res.Outcome != OutcomeAllow {
		t.Fatalf("must inherit green parent: %+v", res)
	}
	if res.InheritedFrom != "p0" {
		t.Fatalf("InheritedFrom=%q, want p0", res.InheritedFrom)
	}
	if !strings.Contains(res.Detail, "no deploy files changed") || !strings.Contains(res.Detail, "p0") {
		t.Fatalf("Detail=%q, want parent sha and reason", res.Detail)
	}

	// Second evaluation reuses the cached diff: at most one diff request per commit.
	res2 := c.Check(context.Background(), req)
	if !res2.Allowed {
		t.Fatalf("second check: %+v", res2)
	}
	if got := fake.diffHits["c1"].Load(); got != 1 {
		t.Fatalf("diff fetched %d times, want 1", got)
	}
}

func TestNoPipelineWithEnvChangeWaits(t *testing.T) {
	fake := &fakeGitLab{
		pipelines: map[string]string{
			"p0": `[{"id":7,"sha":"p0","ref":"main","status":"success","source":"push"}]`,
		},
		commits: map[string]string{"c2": "p0"},
		diffs:   map[string][]string{"c2": {".env"}},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}

	res := c.Check(context.Background(), CheckRequest{
		RepoURL:     "https://git.example/g/p.git",
		CommitSHA:   "c2",
		Ref:         "main",
		DeployFiles: deployFilesFixture(),
	})
	if res.Allowed || res.Outcome != OutcomeWait || res.Status != "missing" {
		t.Fatalf("deploy file changed: must wait for own pipeline: %+v", res)
	}
	if res.InheritedFrom != "" {
		t.Fatalf("InheritedFrom must be empty: %+v", res)
	}
}

func TestNoPipelineOverRedParentDoesNotDeploy(t *testing.T) {
	fake := &fakeGitLab{
		pipelines: map[string]string{
			"p1": `[{"id":8,"sha":"p1","ref":"main","status":"failed","source":"push"}]`,
		},
		commits: map[string]string{"c3": "p1"},
		diffs:   map[string][]string{"c3": {"README.md"}},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}

	res := c.Check(context.Background(), CheckRequest{
		RepoURL:     "https://git.example/g/p.git",
		CommitSHA:   "c3",
		Ref:         "main",
		DeployFiles: deployFilesFixture(),
	})
	if res.Allowed {
		t.Fatalf("red parent must not verify the commit: %+v", res)
	}
	if res.Outcome != OutcomeWait || res.Status != "missing" {
		t.Fatalf("behavior must stay as before (waiting): %+v", res)
	}
}

func TestNoPipelineChainWalksToGreenAncestor(t *testing.T) {
	fake := &fakeGitLab{
		pipelines: map[string]string{
			"p2": `[{"id":9,"sha":"p2","ref":"main","status":"success","source":"push"}]`,
		},
		commits: map[string]string{"c4": "c5", "c5": "p2"},
		diffs:   map[string][]string{"c4": {"README.md"}, "c5": {"docs/guide.md"}},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}

	res := c.Check(context.Background(), CheckRequest{
		RepoURL:     "https://git.example/g/p.git",
		CommitSHA:   "c4",
		Ref:         "main",
		DeployFiles: deployFilesFixture(),
	})
	if !res.Allowed || res.InheritedFrom != "p2" {
		t.Fatalf("must walk chain to green ancestor p2: %+v", res)
	}
}

func TestNoPipelineDiffUnavailableKeepsWaiting(t *testing.T) {
	fake := &fakeGitLab{
		pipelines: map[string]string{},
		commits:   map[string]string{}, // c6 unknown -> 404
		diffs:     map[string][]string{},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}

	res := c.Check(context.Background(), CheckRequest{
		RepoURL:     "https://git.example/g/p.git",
		CommitSHA:   "c6",
		Ref:         "main",
		DeployFiles: deployFilesFixture(),
	})
	if res.Allowed || res.Outcome != OutcomeWait {
		t.Fatalf("unreadable diff must keep current waiting behavior: %+v", res)
	}
}

func TestDeployPathsTouched(t *testing.T) {
	files := []string{"compose.yaml", ".env", "config", "assets/*.tpl"}
	cases := []struct {
		changed string
		want    bool
	}{
		{"compose.yaml", true},
		{".env", true},
		{"./.env", true},
		{"config/nginx.conf", true}, // directory entry matches subpaths
		{"config.yaml", false},
		{"assets/site.tpl", false}, // glob entries match literally only
		{"assets/*.tpl", true},
		{"README.md", false},
		{".gitlab-ci.yml", false},
	}
	for _, tc := range cases {
		if got := deployPathsTouched([]string{tc.changed}, files); got != tc.want {
			t.Errorf("deployPathsTouched(%q) = %v, want %v", tc.changed, got, tc.want)
		}
	}
}
