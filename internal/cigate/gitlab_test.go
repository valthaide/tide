package cigate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valthaide/tide/internal/deploypolicy"
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
	sshBase, err := APIBaseFromRepoURL("git@git.example:tenants/dev-1.git")
	if err != nil || sshBase != "https://git.example/api/v4" {
		t.Fatalf("ssh base=%q err=%v", sshBase, err)
	}
	sshPath, err := ProjectPathFromRepoURL("git@git.example:tenants/dev-1.git")
	if err != nil || sshPath != "tenants%2Fdev-1" {
		t.Fatalf("ssh path=%q err=%v", sshPath, err)
	}
}

func TestClassifyPipelineStatus(t *testing.T) {
	cases := []struct {
		status      string
		wantOutcome string
		wantReason  string
	}{
		{"success", OutcomeAllow, ReasonOK},
		{"pending", OutcomeWait, ReasonCIPending},
		{"running", OutcomeWait, ReasonCIPending},
		{"created", OutcomeWait, ReasonCIPending},
		{"manual", OutcomeWait, ReasonCIPending},
		{"failed", OutcomeDeny, ReasonCIFailed},
		{"canceled", OutcomeDeny, ReasonCIFailed},
		{"skipped", OutcomeDeny, ReasonCIFailed},
	}
	for _, tc := range cases {
		o, r := ClassifyPipelineStatus(tc.status)
		if o != tc.wantOutcome || r != tc.wantReason {
			t.Fatalf("%s: got %s/%s want %s/%s", tc.status, o, r, tc.wantOutcome, tc.wantReason)
		}
	}
}

func TestCheckCommitPendingThenSuccess(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pipelines") {
			if n.Add(1) == 1 {
				_, _ = w.Write([]byte(`[{"id":1,"status":"pending","source":"push"}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":2,"status":"success","source":"push","web_url":"https://git/x"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res1 := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc123")
	if res1.Outcome != OutcomeWait || res1.Reason != ReasonCIPending {
		t.Fatalf("first: %+v", res1)
	}
	res2 := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc123")
	if !res2.Allowed || res2.Outcome != OutcomeAllow {
		t.Fatalf("second: %+v", res2)
	}
}

func TestPerRepoIsolationBetweenTenants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RawPath {
		case "/projects/tenants%2Fdev-1/pipelines":
			_, _ = w.Write([]byte(`[{"id":10,"sha":"deadbeef","ref":"main","status":"failed","source":"push"}]`))
		case "/projects/tenants%2Fdev-2/pipelines":
			_, _ = w.Write([]byte(`[{"id":11,"sha":"deadbeef","ref":"main","status":"success","source":"push"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	r1 := c.Check(context.Background(), CheckRequest{RepoURL: "https://git.example/tenants/dev-1.git", CommitSHA: "deadbeef", Ref: "main"})
	r2 := c.Check(context.Background(), CheckRequest{RepoURL: "https://git.example/tenants/dev-2.git", CommitSHA: "deadbeef", Ref: "main"})
	if r1.Outcome != OutcomeDeny {
		t.Fatalf("dev-1 should be denied: %+v", r1)
	}
	if !r2.Allowed || r2.Outcome != OutcomeAllow {
		t.Fatalf("dev-2 must not be blocked by dev-1 failure: %+v", r2)
	}
}

func TestTenantRequiredJobsAndChildBridgeIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/shared/mono/pipelines":
			// Overall pipeline is failed because dev-1 failed, while shared + dev-2 child succeeded.
			_, _ = w.Write([]byte(`[{"id":100,"sha":"aaa111","ref":"main","status":"failed","source":"push","web_url":"https://git/p/100"}]`))
		case "/projects/shared/mono/pipelines/100/jobs":
			_, _ = w.Write([]byte(`[
				{"id":1,"name":"shared-lint","status":"success","web_url":"https://git/j/1"},
				{"id":2,"name":"backend-dev-1","status":"failed","web_url":"https://git/j/2"}
			]`))
		case "/projects/shared/mono/pipelines/100/bridges":
			_, _ = w.Write([]byte(`[
				{"id":10,"name":"tenant-dev-2","status":"success","downstream_pipeline":{"id":200,"sha":"aaa111","ref":"main","status":"success","source":"parent_pipeline"}},
				{"id":11,"name":"forged-child","status":"success","downstream_pipeline":{"id":201,"sha":"different","ref":"main","status":"success","source":"parent_pipeline"}}
			]`))
		case "/projects/shared/mono/pipelines/200/jobs":
			_, _ = w.Write([]byte(`[{"id":30,"name":"backend-dev-2","status":"success","web_url":"https://git/j/30"}]`))
		case "/projects/shared/mono/pipelines/201/jobs":
			_, _ = w.Write([]byte(`[{"id":31,"name":"forged-job","status":"success"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	dev1 := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/shared/mono.git", CommitSHA: "aaa111", Ref: "main",
		RequiredJobs: []string{"shared-lint", "backend-dev-1"},
	})
	if dev1.Outcome != OutcomeDeny || dev1.WebURL != "https://git/j/2" {
		t.Fatalf("dev-1 should be blocked with job URL: %+v", dev1)
	}

	dev2 := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/shared/mono.git", CommitSHA: "aaa111", Ref: "main",
		RequiredJobs: []string{"shared-lint", "backend-dev-2"},
	})
	if !dev2.Allowed || dev2.Outcome != OutcomeAllow {
		t.Fatalf("dev-2 should be allowed when shared + tenant child jobs pass: %+v", dev2)
	}

	// Forged child with mismatched SHA must not satisfy required job.
	forged := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/shared/mono.git", CommitSHA: "aaa111", Ref: "main",
		RequiredJobs: []string{"forged-job"},
	})
	if forged.Allowed || forged.Outcome != OutcomeDeny {
		t.Fatalf("mismatched child SHA must not satisfy required job: %+v", forged)
	}
}

func TestJobRetryTransitionsFailedToWaitingToAllowedAndIgnoresOldAttempt(t *testing.T) {
	var step atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := step.Load()
		switch r.URL.Path {
		case "/projects/g/p/pipelines":
			st := "failed"
			if cur == 1 {
				st = "running"
			} else if cur >= 2 {
				st = "success"
			}
			_, _ = w.Write([]byte(`[{"id":55,"sha":"bbb222","ref":"main","status":"` + st + `","source":"push","web_url":"https://git/p/55"}]`))
		case "/projects/g/p/pipelines/55/jobs":
			switch cur {
			case 0:
				_, _ = w.Write([]byte(`[{"id":10,"name":"unit-test","status":"failed","web_url":"https://git/j/10"}]`))
			case 1:
				// Retried job creates id=15 in running state alongside old failed id=10.
				_, _ = w.Write([]byte(`[
					{"id":15,"name":"unit-test","status":"running","web_url":"https://git/j/15"},
					{"id":10,"name":"unit-test","status":"failed","web_url":"https://git/j/10"}
				]`))
			default:
				// Latest attempt id=15 succeeded; older failed attempt id=10 must not override it.
				_, _ = w.Write([]byte(`[
					{"id":10,"name":"unit-test","status":"failed","web_url":"https://git/j/10"},
					{"id":15,"name":"unit-test","status":"success","web_url":"https://git/j/15"}
				]`))
			}
		case "/projects/g/p/pipelines/55/bridges":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	req := CheckRequest{RepoURL: "https://git.example/g/p.git", CommitSHA: "bbb222", Ref: "main", RequiredJobs: []string{"unit-test"}}

	r0 := c.Check(context.Background(), req)
	if r0.Outcome != OutcomeDeny || r0.PipelineID != 55 {
		t.Fatalf("step 0 want deny: %+v", r0)
	}

	step.Store(1)
	r1 := c.Check(context.Background(), req)
	if r1.Outcome != OutcomeWait || r1.WebURL != "https://git/j/15" {
		t.Fatalf("step 1 want wait on retried job: %+v", r1)
	}

	step.Store(2)
	r2 := c.Check(context.Background(), req)
	if !r2.Allowed || r2.Outcome != OutcomeAllow || r2.PipelineID != 55 {
		t.Fatalf("step 2 want allow after retry succeeds: %+v", r2)
	}
}

func TestTriggerApiAndWebFailuresNeverBlockPushSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pipelines") {
			_, _ = w.Write([]byte(`[
				{"id":31,"status":"failed","source":"web"},
				{"id":30,"status":"failed","source":"trigger"},
				{"id":29,"status":"failed","source":"api"},
				{"id":28,"status":"success","source":"push"}
			]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if !res.Allowed || res.Source != "push" || res.PipelineID != 28 {
		t.Fatalf("%+v", res)
	}
}

func TestStrictModeWaitsWhenOnlyUntrustedPipelinesExist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":30,"status":"failed","source":"trigger"},{"id":29,"status":"success","source":"api"}]`))
	}))
	defer srv.Close()

	// Default non-strict keeps backwards-compatible pin behavior (Criterion 17).
	defChecker := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	defRes := defChecker.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if !defRes.Allowed || defRes.Outcome != OutcomeAllow || defRes.Status != "absent" || len(defRes.Ignored) != 2 {
		t.Fatalf("default mode: %+v", defRes)
	}

	// Strict mode waits instead of allowing deploy without a trusted pipeline (Req 5 / Criterion 7).
	strictRes := defChecker.Check(context.Background(), CheckRequest{
		RepoURL:   "https://git.example/g/p.git",
		CommitSHA: "abc",
		Strict:    true,
	})
	if strictRes.Allowed || strictRes.Outcome != OutcomeWait || strictRes.Reason != ReasonCIPending {
		t.Fatalf("strict mode must wait: %+v", strictRes)
	}
}

func TestTrustedWebPipelinePolicyAndReadOnlyFallback(t *testing.T) {
	var forbidVars atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/g/p/pipelines":
			_, _ = w.Write([]byte(`[
				{"id":93,"sha":"ccc333","ref":"main","status":"success","source":"web","web_url":"https://git/p/93"},
				{"id":92,"sha":"ccc333","ref":"main","status":"success","source":"web","web_url":"https://git/p/92"},
				{"id":91,"sha":"ccc333","ref":"main","status":"success","source":"web","web_url":"https://git/p/91"},
				{"id":90,"sha":"ccc333","ref":"main","status":"failed","source":"push","web_url":"https://git/p/90"}
			]`))
		case "/projects/g/p/pipelines/93":
			// Untrusted user -> must be ignored.
			_, _ = w.Write([]byte(`{"id":93,"sha":"ccc333","ref":"main","user":{"id":2,"username":"intruder"}}`))
		case "/projects/g/p/pipelines/92":
			// Trusted user, but passes SKIP_TESTS=true -> must be rejected.
			_, _ = w.Write([]byte(`{"id":92,"sha":"ccc333","ref":"main","user":{"id":1,"username":"release-bot"}}`))
		case "/projects/g/p/pipelines/92/variables":
			if forbidVars.Load() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`[{"key":"SKIP_TESTS","value":"1"}]`))
		case "/projects/g/p/pipelines/91":
			// Trusted user, allowed variable, full required jobs -> unlocks deploy.
			_, _ = w.Write([]byte(`{"id":91,"sha":"ccc333","ref":"main","user":{"id":1,"username":"release-bot"}}`))
		case "/projects/g/p/pipelines/91/variables":
			if forbidVars.Load() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`[{"key":"DEPLOY_TARGET","value":"prod"}]`))
		case "/projects/g/p/pipelines/91/jobs":
			_, _ = w.Write([]byte(`[
				{"id":1,"name":"lint","status":"success"},
				{"id":2,"name":"test","status":"success"}
			]`))
		case "/projects/g/p/pipelines/91/bridges":
			_, _ = w.Write([]byte(`[]`))
		case "/projects/g/p/pipelines/90":
			_, _ = w.Write([]byte(`{"id":90,"sha":"ccc333","ref":"main","user":{"id":3,"username":"dev"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}

	// Policy disabled by default: web pipelines are ignored, failed push blocks deploy.
	disabled := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/g/p.git", CommitSHA: "ccc333", Ref: "main",
	})
	if disabled.Allowed || disabled.Outcome != OutcomeDeny || disabled.PipelineID != 90 {
		t.Fatalf("web disabled should deny via push 90: %+v", disabled)
	}

	policy := &deploypolicy.TrustedWebPolicy{
		Enabled:          true,
		AllowedUsers:     []string{"release-bot"},
		AllowedVariables: []string{"DEPLOY_TARGET", "SKIP_TESTS"},
		RequiredJobs:     []string{"lint", "test"},
	}

	// Enabled with sufficient API permissions: pipeline 93 (intruder) and 92 (SKIP_TESTS=1) are skipped, 91 allows deploy.
	allowed := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/g/p.git", CommitSHA: "ccc333", Ref: "main",
		TrustedWeb: policy,
	})
	if !allowed.Allowed || allowed.PipelineID != 91 || allowed.Source != "web" || allowed.TriggeredBy != "release-bot" {
		t.Fatalf("trusted web 91 should allow: %+v", allowed)
	}

	// Token without Maintainer access to /variables (403): trusted web automatically stays off (Criterion 18).
	forbidVars.Store(true)
	readOnly := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/g/p.git", CommitSHA: "ccc333", Ref: "main",
		TrustedWeb: policy,
	})
	if readOnly.Allowed || readOnly.Outcome != OutcomeDeny || readOnly.PipelineID != 90 {
		t.Fatalf("403 on variables must disable trusted web and keep deny: %+v", readOnly)
	}
}

func TestIncompleteManualPipelineDoesNotUnblock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/g/p/pipelines":
			_, _ = w.Write([]byte(`[
				{"id":71,"sha":"ddd444","ref":"main","status":"success","source":"web"},
				{"id":70,"sha":"ddd444","ref":"main","status":"failed","source":"push"}
			]`))
		case "/projects/g/p/pipelines/71":
			_, _ = w.Write([]byte(`{"id":71,"sha":"ddd444","ref":"main","user":{"id":1,"username":"release-bot"}}`))
		case "/projects/g/p/pipelines/71/variables":
			_, _ = w.Write([]byte(`[]`))
		case "/projects/g/p/pipelines/71/jobs":
			// Missing "test" job and skipped "security" job -> incomplete!
			_, _ = w.Write([]byte(`[
				{"id":1,"name":"lint","status":"success"},
				{"id":2,"name":"security","status":"skipped"}
			]`))
		case "/projects/g/p/pipelines/71/bridges":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.Check(context.Background(), CheckRequest{
		RepoURL: "https://git.example/g/p.git", CommitSHA: "ddd444", Ref: "main",
		TrustedWeb: &deploypolicy.TrustedWebPolicy{
			Enabled:      true,
			AllowedUsers: []string{"release-bot"},
			RequiredJobs: []string{"lint", "security", "test"},
		},
	})
	if res.Allowed || res.Outcome != OutcomeDeny || res.PipelineID != 70 {
		t.Fatalf("incomplete web pipeline must not unblock failed push: %+v", res)
	}
}

func TestNoPipelinesWaits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Outcome != OutcomeWait || res.Reason != ReasonCIPending || res.Status != "missing" {
		t.Fatalf("%+v", res)
	}
}

func TestCanceledPushIsIgnored(t *testing.T) {
	chosen, ok := SelectPipeline([]Pipeline{
		{ID: 4, Status: "canceled", Source: "push"},
		{ID: 3, Status: "skipped", Source: "push"},
		{ID: 2, Status: "failed", Source: "trigger"},
		{ID: 1, Status: "success", Source: "merge_request_event"},
	}, nil)
	if !ok || chosen.ID != 1 || chosen.Source != "merge_request_event" {
		t.Fatalf("%+v ok=%v", chosen, ok)
	}
}

func TestCheckCommitFailClosedOnError(t *testing.T) {
	c := &Checker{APIBase: "http://127.0.0.1:1", Token: "tok", Client: &http.Client{}}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Outcome != OutcomeDeny || res.Reason != ReasonCIError {
		t.Fatalf("%+v", res)
	}
}
