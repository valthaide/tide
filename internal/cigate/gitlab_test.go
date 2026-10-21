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
		status          string
		wantOutcome     string
		wantReason      string
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
			_, _ = w.Write([]byte(`[{"status":"pending"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"status":"success","web_url":"https://git/x"}]`))
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
		_, _ = w.Write([]byte(`[{"status":"failed"}]`))
	}))
	defer srv.Close()
	c := &Checker{APIBase: srv.URL, Token: "tok", Client: srv.Client()}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Outcome != OutcomeDeny || res.Reason != ReasonCIFailed {
		t.Fatalf("%+v", res)
	}
}

func TestCheckCommitFailClosedOnError(t *testing.T) {
	c := &Checker{APIBase: "http://127.0.0.1:1", Token: "tok", Client: &http.Client{}}
	res := c.CheckCommit(context.Background(), "https://git.example/g/p.git", "abc")
	if res.Outcome != OutcomeDeny || res.Reason != ReasonCIError {
		t.Fatalf("%+v", res)
	}
}
