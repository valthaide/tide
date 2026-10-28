package cigate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		if n.Add(1) == 1 {
			_, _ = w.Write([]byte(`[{"id":1,"status":"pending","source":"push"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":2,"status":"success","source":"push","web_url":"https://git/x"}]`))
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

func TestCheckCommitFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":9,"status":"failed","source":"push"}]`))
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Outcome != OutcomeDeny || res.Reason != ReasonCIFailed || res.Source != "push" || res.PipelineID != 9 {
		t.Fatalf("%+v", res)
	}
}

func TestTriggerFailureDoesNotOverridePushSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":30,"status":"failed","source":"trigger"},
			{"id":29,"status":"failed","source":"api"},
			{"id":28,"status":"success","source":"push"}
		]`))
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if !res.Allowed || res.Source != "push" || res.PipelineID != 28 {
		t.Fatalf("%+v", res)
	}
}

func TestOnlyTriggerFailureDeploys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":30,"status":"failed","source":"trigger"},{"id":29,"status":"success","source":"api"}]`))
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if !res.Allowed || res.Outcome != OutcomeAllow || len(res.Ignored) != 2 {
		t.Fatalf("%+v", res)
	}
	if res.Ignored[0].ID != 30 || res.Ignored[0].Source != "trigger" || res.Ignored[1].Source != "api" {
		t.Fatalf("ignored: %+v", res.Ignored)
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
