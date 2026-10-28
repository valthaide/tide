package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusRunning  = "running"
	StatusPending  = "pending"
	StatusManual   = "manual"
	StatusSkipped  = "skipped"
	StatusCreated  = "created"
)

const (
	OutcomeAllow = "allow"
	OutcomeWait  = "wait"
	OutcomeDeny  = "deny"
)

const (
	ReasonOK        = "ok"
	ReasonCIPending = "ci_pending"
	ReasonCIFailed  = "ci_failed"
	ReasonCIError   = "ci_error"
)

// Result is the pipeline gate decision for a commit.
type Result struct {
	Allowed    bool
	Outcome    string // allow | wait | deny
	Status     string
	Reason     string // ok | ci_pending | ci_failed | ci_error
	WebURL     string
	PipelineID int64
	Source     string
	// Ignored lists pipelines of other sources when the deploy proceeds
	// because no push or merge request pipeline decided the commit.
	Ignored []Pipeline
}

// Pipeline is one GitLab pipeline on a commit.
type Pipeline struct {
	ID     int64
	Status string
	Source string
	WebURL string
}

// DefaultPipelineSources are the pipeline sources that decide a deploy when
// require_pipeline_sources is unset.
func DefaultPipelineSources() []string {
	return []string{"push", "merge_request_event"}
}

// NormalizeSources returns the sources that count. An empty list means the default.
func NormalizeSources(sources []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, source := range sources {
		source = strings.ToLower(strings.TrimSpace(source))
		if source == "" {
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	if len(out) == 0 {
		return DefaultPipelineSources()
	}
	return out
}

// SelectPipeline returns the newest pipeline whose source is allowed.
// Canceled and skipped pipelines are ignored. Pipelines must be newest first.
func SelectPipeline(pipelines []Pipeline, sources []string) (Pipeline, bool) {
	allowed := map[string]struct{}{}
	for _, source := range NormalizeSources(sources) {
		allowed[source] = struct{}{}
	}
	for _, pipeline := range pipelines {
		source := strings.ToLower(strings.TrimSpace(pipeline.Source))
		if _, ok := allowed[source]; !ok {
			continue
		}
		if !pipelineCounts(pipeline.Status) {
			continue
		}
		pipeline.Source = source
		pipeline.Status = strings.ToLower(strings.TrimSpace(pipeline.Status))
		return pipeline, true
	}
	return Pipeline{}, false
}

func pipelineCounts(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusCanceled, "cancelled", StatusSkipped:
		return false
	default:
		return true
	}
}

// OtherSourcePipelines returns pipelines that are not canceled or skipped and
// whose source is outside the deciding set. Newest first, same order as input.
func OtherSourcePipelines(pipelines []Pipeline, sources []string) []Pipeline {
	allowed := map[string]struct{}{}
	for _, source := range NormalizeSources(sources) {
		allowed[source] = struct{}{}
	}
	var out []Pipeline
	for _, pipeline := range pipelines {
		if !pipelineCounts(pipeline.Status) {
			continue
		}
		source := strings.ToLower(strings.TrimSpace(pipeline.Source))
		if source == "" {
			continue
		}
		if _, ok := allowed[source]; ok {
			continue
		}
		pipeline.Source = source
		pipeline.Status = strings.ToLower(strings.TrimSpace(pipeline.Status))
		out = append(out, pipeline)
	}
	return out
}

// FormatPipelines renders id, source, and status for a log line.
func FormatPipelines(pipelines []Pipeline) string {
	parts := make([]string, 0, len(pipelines))
	for _, pipeline := range pipelines {
		parts = append(parts, fmt.Sprintf("id=%d source=%s status=%s", pipeline.ID, pipeline.Source, pipeline.Status))
	}
	return strings.Join(parts, ", ")
}

// Checker queries GitLab for the newest pipeline of a commit.
// Sources limits which pipeline sources count. Empty means push and merge_request_event.
type Checker struct {
	APIBase string // e.g. https://git.example/api/v4
	Token   string
	Client  *http.Client
	Sources []string
}

// TokenFromEnv returns a GitLab API token (never logged).
func TokenFromEnv() string {
	for _, key := range []string{"CONOPS_GITLAB_API_TOKEN", "CONOPS_GIT_TOKEN", "CONOPS_FLEET_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// APIBaseFromRepoURL derives https://host/api/v4 from an HTTPS git repo URL.
func APIBaseFromRepoURL(repoURL string) (string, error) {
	repoURL = strings.TrimSpace(repoURL)
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("parse repo url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("gitlab API gate requires HTTP(S) repo URL")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("repo URL missing host")
	}
	return scheme + "://" + parsed.Host + "/api/v4", nil
}

// ProjectPathFromRepoURL returns URL-encoded "group/project" path.
func ProjectPathFromRepoURL(repoURL string) (string, error) {
	repoURL = strings.TrimSpace(repoURL)
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", err
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return "", fmt.Errorf("cannot derive project path from %s", repoURL)
	}
	return url.PathEscape(path), nil
}

// ClassifyPipelineStatus maps a GitLab pipeline status to allow/wait/deny.
func ClassifyPipelineStatus(status string) (outcome, reason string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusSuccess:
		return OutcomeAllow, ReasonOK
	case StatusFailed, StatusCanceled, "cancelled", StatusSkipped:
		return OutcomeDeny, ReasonCIFailed
	case StatusPending, StatusRunning, StatusCreated, StatusManual,
		"waiting_for_resource", "preparing", "scheduled", "waiting":
		return OutcomeWait, ReasonCIPending
	case "missing", "":
		return OutcomeWait, ReasonCIPending
	default:
		// Unknown non-terminal → wait and recheck.
		return OutcomeWait, ReasonCIPending
	}
}

// CheckCommit returns whether the commit's latest pipeline allows deploy.
// Fail-closed on transport/API errors (OutcomeDeny, ReasonCIError).
func (c *Checker) CheckCommit(ctx context.Context, repoURL, commitSHA string) Result {
	commitSHA = strings.TrimSpace(commitSHA)
	if commitSHA == "" {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
	}
	if c == nil || strings.TrimSpace(c.Token) == "" {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
	}
	apiBase := strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	if apiBase == "" {
		var err error
		apiBase, err = APIBaseFromRepoURL(repoURL)
		if err != nil {
			return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
		}
	}
	project, err := ProjectPathFromRepoURL(repoURL)
	if err != nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
	}

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	endpoint := fmt.Sprintf("%s/projects/%s/pipelines?sha=%s&per_page=100&order_by=id&sort=desc",
		apiBase, project, url.QueryEscape(commitSHA))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)

	resp, err := client.Do(req)
	if err != nil {
		reason := ReasonCIError
		if ctx.Err() != nil {
			reason = ReasonCIError
		}
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: reason, Status: "unknown"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
	}

	var listed []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Source string `json:"source"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown"}
	}
	pipelines := make([]Pipeline, 0, len(listed))
	for _, item := range listed {
		pipelines = append(pipelines, Pipeline{
			ID:     item.ID,
			Status: item.Status,
			Source: item.Source,
			WebURL: item.WebURL,
		})
	}
	chosen, ok := SelectPipeline(pipelines, c.Sources)
	if !ok {
		others := OtherSourcePipelines(pipelines, c.Sources)
		if len(others) > 0 {
			return Result{
				Allowed: true,
				Outcome: OutcomeAllow,
				Reason:  ReasonOK,
				Status:  "absent",
				Ignored: others,
			}
		}
		outcome, reason := ClassifyPipelineStatus("missing")
		return Result{Allowed: false, Outcome: outcome, Reason: reason, Status: "missing"}
	}
	outcome, reason := ClassifyPipelineStatus(chosen.Status)
	return Result{
		Allowed:    outcome == OutcomeAllow,
		Outcome:    outcome,
		Reason:     reason,
		Status:     chosen.Status,
		WebURL:     chosen.WebURL,
		PipelineID: chosen.ID,
		Source:     chosen.Source,
	}
}
