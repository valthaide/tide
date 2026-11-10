package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/envcompat"
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
	Allowed     bool
	Outcome     string // allow | wait | deny
	Status      string
	Reason      string // ok | ci_pending | ci_failed | ci_error
	Detail      string
	WebURL      string
	PipelineID  int64
	Source      string
	Ref         string
	TriggeredBy string
	// Ignored lists pipelines of other sources when no primary pipeline decided the commit.
	Ignored []Pipeline
}

// Pipeline is one GitLab pipeline on a commit.
type Pipeline struct {
	ID     int64
	SHA    string
	Ref    string
	Status string
	Source string
	WebURL string
}

// CheckRequest configures a single CI gate evaluation for one app and commit.
type CheckRequest struct {
	RepoURL      string
	CommitSHA    string
	Ref          string
	StrictRef    bool
	Sources      []string
	Strict       bool
	RequiredJobs []string
	TrustedWeb   *deploypolicy.TrustedWebPolicy
	Token        string
}

// DefaultPipelineSources are the pipeline sources that decide a deploy when
// require_pipeline_sources is unset.
func DefaultPipelineSources() []string {
	return []string{"push", "merge_request_event"}
}

// NormalizeSources returns the primary sources that count.
// Empty means push and merge_request_event. Manual `web` pipelines are governed
// exclusively by TrustedWebPolicy and are never accepted as an unrestricted source.
func NormalizeSources(sources []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, source := range sources {
		source = strings.ToLower(strings.TrimSpace(source))
		if source == "" || source == "web" {
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
	return selectPrimaryPipeline(pipelines, sources, "", false)
}

func selectPrimaryPipeline(pipelines []Pipeline, sources []string, expectedRef string, strictRef bool) (Pipeline, bool) {
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
		if !pipelineRefMatches(pipeline, source, expectedRef, strictRef) {
			continue
		}
		pipeline.Source = source
		pipeline.Status = strings.ToLower(strings.TrimSpace(pipeline.Status))
		return pipeline, true
	}
	return Pipeline{}, false
}

func pipelineRefMatches(p Pipeline, source, expectedRef string, strictRef bool) bool {
	expectedRef = strings.TrimSpace(expectedRef)
	if expectedRef == "" {
		return true
	}
	gotRef := strings.TrimSpace(p.Ref)
	if gotRef == "" {
		return !strictRef
	}
	if gotRef == expectedRef {
		return true
	}
	if !strictRef && source == "merge_request_event" && strings.HasPrefix(gotRef, "refs/merge-requests/") {
		return true
	}
	return false
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
	Strict  bool
}

// TokenFromEnv returns a GitLab API token (never logged).
func TokenFromEnv() string {
	return envcompat.FirstEnv("TIDE_GITLAB_API_TOKEN", "TIDE_GIT_TOKEN", "TIDE_FLEET_TOKEN")
}

// StrictFromEnv reports whether TIDE_STRICT_CI_GATE enables strict pipeline gating globally.
func StrictFromEnv() bool {
	v := strings.ToLower(envcompat.Getenv("TIDE_STRICT_CI_GATE"))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// APIBaseFromRepoURL derives https://host/api/v4 from an HTTPS git repo URL (or SSH git@host:group/proj.git).
func APIBaseFromRepoURL(repoURL string) (string, error) {
	repoURL = strings.TrimSpace(repoURL)
	if strings.HasPrefix(repoURL, "git@") {
		rest := strings.TrimPrefix(repoURL, "git@")
		host, _, ok := strings.Cut(rest, ":")
		host = strings.TrimSpace(host)
		if !ok || host == "" {
			return "", fmt.Errorf("repo URL missing host")
		}
		return "https://" + host + "/api/v4", nil
	}
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("parse repo url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "ssh" && parsed.Hostname() != "" {
		return "https://" + parsed.Hostname() + "/api/v4", nil
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("gitlab API gate requires HTTP(S) or SSH repo URL")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("repo URL missing host")
	}
	return scheme + "://" + parsed.Host + "/api/v4", nil
}

// ProjectPathFromRepoURL returns URL-encoded "group/project" path.
func ProjectPathFromRepoURL(repoURL string) (string, error) {
	repoURL = strings.TrimSpace(repoURL)
	var rawPath string
	if strings.HasPrefix(repoURL, "git@") {
		rest := strings.TrimPrefix(repoURL, "git@")
		_, after, ok := strings.Cut(rest, ":")
		if !ok {
			return "", fmt.Errorf("cannot derive project path from %s", repoURL)
		}
		rawPath = after
	} else {
		parsed, err := url.Parse(repoURL)
		if err != nil {
			return "", err
		}
		rawPath = parsed.Path
	}
	path := strings.TrimPrefix(rawPath, "/")
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
		// Unknown non-terminal -> wait and recheck.
		return OutcomeWait, ReasonCIPending
	}
}

func isActiveStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusPending, StatusRunning, StatusCreated, "waiting_for_resource", "preparing", "scheduled", "waiting":
		return true
	default:
		return false
	}
}

// CheckCommit evaluates the commit with the checker's default settings.
func (c *Checker) CheckCommit(ctx context.Context, repoURL, commitSHA string) Result {
	if c == nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: "checker not initialized"}
	}
	return c.Check(ctx, CheckRequest{
		RepoURL:   repoURL,
		CommitSHA: commitSHA,
		Sources:   c.Sources,
		Strict:    c.Strict,
	})
}

// Check evaluates the GitLab CI gate for one repository, exact commit SHA, ref, and policy.
func (c *Checker) Check(ctx context.Context, req CheckRequest) Result {
	commitSHA := strings.TrimSpace(req.CommitSHA)
	if commitSHA == "" {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: "commit SHA is empty"}
	}
	if c == nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: "checker is nil"}
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		token = strings.TrimSpace(c.Token)
	}
	if token == "" {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: "GitLab API token is not configured"}
	}

	repoURL := strings.TrimSpace(req.RepoURL)
	apiBase := strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	if apiBase == "" {
		var err error
		apiBase, err = APIBaseFromRepoURL(repoURL)
		if err != nil {
			return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: err.Error()}
		}
	}
	project, err := ProjectPathFromRepoURL(repoURL)
	if err != nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: err.Error()}
	}

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	sources := req.Sources
	if len(sources) == 0 {
		sources = c.Sources
	}
	strict := req.Strict || c.Strict || StrictFromEnv()

	endpoint := fmt.Sprintf("%s/projects/%s/pipelines?sha=%s&per_page=100&order_by=id&sort=desc",
		apiBase, project, url.QueryEscape(commitSHA))
	body, statusCode, err := doGitLabGET(ctx, client, token, endpoint)
	if err != nil || statusCode >= 300 {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: "failed to query GitLab pipelines"}
	}

	var listed []struct {
		ID     int64  `json:"id"`
		SHA    string `json:"sha"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
		Source string `json:"source"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return Result{Allowed: false, Outcome: OutcomeDeny, Reason: ReasonCIError, Status: "unknown", Detail: "invalid GitLab pipelines JSON"}
	}

	pipelines := make([]Pipeline, 0, len(listed))
	for _, item := range listed {
		if item.SHA != "" && !strings.EqualFold(strings.TrimSpace(item.SHA), commitSHA) {
			continue
		}
		pipelines = append(pipelines, Pipeline{
			ID:     item.ID,
			SHA:    strings.TrimSpace(item.SHA),
			Ref:    strings.TrimSpace(item.Ref),
			Status: item.Status,
			Source: item.Source,
			WebURL: item.WebURL,
		})
	}

	var primaryRes *Result
	chosen, hasPrimary := selectPrimaryPipeline(pipelines, sources, req.Ref, req.StrictRef)
	if hasPrimary {
		res := evaluatePipeline(ctx, client, token, apiBase, project, chosen, commitSHA, req.Ref, req.RequiredJobs)
		if res.Allowed {
			res.TriggeredBy = fetchPipelineUser(ctx, client, token, apiBase, project, chosen.ID)
			return res
		}
		primaryRes = &res
	}

	// Optional trusted manual (`web`) pipeline evaluation when primary did not allow deploy.
	if req.TrustedWeb != nil && req.TrustedWeb.Enabled {
		if webRes, ok := evaluateTrustedWebPipelines(ctx, client, token, apiBase, project, pipelines, commitSHA, req.Ref, req.RequiredJobs, req.TrustedWeb); ok {
			if webRes.Allowed || primaryRes == nil {
				return webRes
			}
			if webRes.Outcome == OutcomeWait && primaryRes.Outcome == OutcomeDeny {
				return webRes
			}
		}
	}

	if primaryRes != nil {
		primaryRes.TriggeredBy = fetchPipelineUser(ctx, client, token, apiBase, project, chosen.ID)
		return *primaryRes
	}

	others := OtherSourcePipelines(pipelines, sources)
	if len(others) > 0 && !strict {
		return Result{
			Allowed: true,
			Outcome: OutcomeAllow,
			Reason:  ReasonOK,
			Status:  "absent",
			Detail:  "no push/merge_request_event pipeline; non-strict mode allowed",
			Ignored: others,
		}
	}

	outcome, reason := ClassifyPipelineStatus("missing")
	detail := "waiting for trusted pipeline"
	if strict && len(others) > 0 {
		detail = "strict mode: waiting for trusted pipeline (other sources ignored)"
	}
	return Result{
		Allowed: false,
		Outcome: outcome,
		Reason:  reason,
		Status:  "missing",
		Detail:  detail,
		Ignored: others,
	}
}

type glJob struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

type glBridge struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Status             string `json:"status"`
	WebURL             string `json:"web_url"`
	DownstreamPipeline *struct {
		ID     int64  `json:"id"`
		SHA    string `json:"sha"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
		Source string `json:"source"`
		WebURL string `json:"web_url"`
	} `json:"downstream_pipeline"`
}

func evaluatePipeline(
	ctx context.Context,
	client *http.Client,
	token, apiBase, project string,
	pipeline Pipeline,
	commitSHA, expectedRef string,
	requiredJobs []string,
) Result {
	if len(requiredJobs) == 0 {
		// Even without explicit required_jobs, check if a previously failed pipeline has a retried job currently running.
		outcome, reason := ClassifyPipelineStatus(pipeline.Status)
		detail := fmt.Sprintf("pipeline %d (%s) status=%s", pipeline.ID, pipeline.Source, pipeline.Status)
		return Result{
			Allowed:    outcome == OutcomeAllow,
			Outcome:    outcome,
			Reason:     reason,
			Status:     pipeline.Status,
			Detail:     detail,
			WebURL:     pipeline.WebURL,
			PipelineID: pipeline.ID,
			Source:     pipeline.Source,
			Ref:        pipeline.Ref,
		}
	}

	latestJobs, bridgesPending, err := collectLatestPipelineJobs(ctx, client, token, apiBase, project, pipeline, commitSHA, expectedRef)
	if err != nil {
		return Result{
			Allowed:    false,
			Outcome:    OutcomeDeny,
			Reason:     ReasonCIError,
			Status:     "unknown",
			Detail:     err.Error(),
			WebURL:     pipeline.WebURL,
			PipelineID: pipeline.ID,
			Source:     pipeline.Source,
			Ref:        pipeline.Ref,
		}
	}

	pipelineActive := isActiveStatus(pipeline.Status) || bridgesPending
	var waitDetail, waitURL string
	var failDetail, failURL string

	for _, reqName := range requiredJobs {
		reqName = strings.TrimSpace(reqName)
		if reqName == "" {
			continue
		}
		job, exists := latestJobs[reqName]
		if !exists {
			if pipelineActive {
				if waitDetail == "" {
					waitDetail = fmt.Sprintf("waiting for required job %s", reqName)
					waitURL = pipeline.WebURL
				}
				continue
			}
			if failDetail == "" {
				failDetail = fmt.Sprintf("missing required job %s", reqName)
				failURL = pipeline.WebURL
			}
			continue
		}

		st := strings.ToLower(strings.TrimSpace(job.Status))
		jobURL := job.WebURL
		if jobURL == "" {
			jobURL = pipeline.WebURL
		}
		switch {
		case st == StatusSuccess:
			continue
		case isActiveStatus(st):
			if waitDetail == "" {
				waitDetail = fmt.Sprintf("waiting for required job %s (%s)", reqName, st)
				waitURL = jobURL
			}
		case st == StatusManual:
			// Unfinished manual required job is never treated as success; wait while playable.
			if waitDetail == "" {
				waitDetail = fmt.Sprintf("waiting for manual required job %s", reqName)
				waitURL = jobURL
			}
		default:
			// failed, skipped, canceled, or unknown terminal
			if failDetail == "" {
				failDetail = fmt.Sprintf("required job %s %s", reqName, st)
				failURL = jobURL
			}
		}
	}

	if waitDetail != "" && failDetail == "" {
		return Result{
			Allowed:    false,
			Outcome:    OutcomeWait,
			Reason:     ReasonCIPending,
			Status:     StatusRunning,
			Detail:     waitDetail,
			WebURL:     waitURL,
			PipelineID: pipeline.ID,
			Source:     pipeline.Source,
			Ref:        pipeline.Ref,
		}
	}
	if failDetail != "" {
		// If another required job is actively running due to retry while a sibling hasn't been retried yet,
		// keep reporting deny unless all non-succeeded required jobs are actively running/pending.
		return Result{
			Allowed:    false,
			Outcome:    OutcomeDeny,
			Reason:     ReasonCIFailed,
			Status:     StatusFailed,
			Detail:     failDetail,
			WebURL:     failURL,
			PipelineID: pipeline.ID,
			Source:     pipeline.Source,
			Ref:        pipeline.Ref,
		}
	}

	return Result{
		Allowed:    true,
		Outcome:    OutcomeAllow,
		Reason:     ReasonOK,
		Status:     StatusSuccess,
		Detail:     "all required jobs succeeded",
		WebURL:     pipeline.WebURL,
		PipelineID: pipeline.ID,
		Source:     pipeline.Source,
		Ref:        pipeline.Ref,
	}
}

func collectLatestPipelineJobs(
	ctx context.Context,
	client *http.Client,
	token, apiBase, project string,
	parent Pipeline,
	commitSHA, expectedRef string,
) (map[string]glJob, bool, error) {
	latest := map[string]glJob{}
	record := func(key string, j glJob) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if prev, ok := latest[key]; !ok || j.ID >= prev.ID {
			latest[key] = j
		}
	}

	jobs, err := fetchAllJobs(ctx, client, token, fmt.Sprintf("%s/projects/%s/pipelines/%d/jobs", apiBase, project, parent.ID))
	if err != nil {
		return nil, false, err
	}
	for _, j := range jobs {
		record(j.Name, j)
	}

	bridges, bridgesSupported, err := fetchAllBridges(ctx, client, token, fmt.Sprintf("%s/projects/%s/pipelines/%d/bridges", apiBase, project, parent.ID))
	if err != nil {
		return nil, false, err
	}
	bridgesPending := false
	if bridgesSupported {
		wantRef := strings.TrimSpace(expectedRef)
		if wantRef == "" {
			wantRef = strings.TrimSpace(parent.Ref)
		}
		for _, b := range bridges {
			if isActiveStatus(b.Status) && b.DownstreamPipeline == nil {
				bridgesPending = true
				continue
			}
			if b.DownstreamPipeline == nil {
				continue
			}
			ds := b.DownstreamPipeline
			// Verify child pipeline linkage to this parent (guaranteed by parent /bridges endpoint), exact SHA, and ref.
			if !strings.EqualFold(strings.TrimSpace(ds.SHA), commitSHA) {
				continue
			}
			if wantRef != "" && strings.TrimSpace(ds.Ref) != wantRef {
				continue
			}
			if isActiveStatus(ds.Status) {
				bridgesPending = true
			}
			childJobs, err := fetchAllJobs(ctx, client, token, fmt.Sprintf("%s/projects/%s/pipelines/%d/jobs", apiBase, project, ds.ID))
			if err != nil {
				return nil, false, err
			}
			for _, cj := range childJobs {
				record(cj.Name, cj)
				if b.Name != "" {
					record(b.Name+":"+cj.Name, cj)
					record(b.Name+"/"+cj.Name, cj)
				}
			}
		}
	}

	return latest, bridgesPending, nil
}

func evaluateTrustedWebPipelines(
	ctx context.Context,
	client *http.Client,
	token, apiBase, project string,
	pipelines []Pipeline,
	commitSHA, expectedRef string,
	defaultRequiredJobs []string,
	policy *deploypolicy.TrustedWebPolicy,
) (Result, bool) {
	if policy == nil || !policy.Enabled || len(policy.AllowedUsers) == 0 {
		return Result{}, false
	}
	reqJobs := policy.RequiredJobs
	if len(reqJobs) == 0 {
		reqJobs = defaultRequiredJobs
	}
	if len(reqJobs) == 0 {
		// Trusted manual pipelines must verify a full set of required jobs.
		return Result{}, false
	}
	expectedRef = strings.TrimSpace(expectedRef)
	if expectedRef == "" {
		return Result{}, false
	}

	allowedUsers := map[string]struct{}{}
	for _, u := range policy.AllowedUsers {
		u = strings.ToLower(strings.TrimSpace(u))
		if u != "" {
			allowedUsers[u] = struct{}{}
		}
	}
	allowedVars := map[string]struct{}{}
	for _, k := range policy.AllowedVariables {
		k = strings.TrimSpace(k)
		if k != "" {
			allowedVars[k] = struct{}{}
		}
	}

	var waitingCandidate *Result
	for _, p := range pipelines {
		if !strings.EqualFold(strings.TrimSpace(p.Source), "web") || !pipelineCounts(p.Status) {
			continue
		}
		detail, ok := fetchPipelineDetail(ctx, client, token, apiBase, project, p.ID)
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(detail.SHA), commitSHA) {
			continue
		}
		if strings.TrimSpace(detail.Ref) != expectedRef {
			continue
		}
		username := strings.TrimSpace(detail.User.Username)
		if _, userOK := allowedUsers[strings.ToLower(username)]; !userOK {
			continue
		}
		vars, varsOK := fetchPipelineVariables(ctx, client, token, apiBase, project, p.ID)
		if !varsOK {
			// Token lacks Maintainer access to read pipeline variables: trusted web policy stays off.
			return Result{}, false
		}
		varsValid := true
		for _, v := range vars {
			key := strings.TrimSpace(v.Key)
			if _, ok := allowedVars[key]; !ok {
				varsValid = false
				break
			}
			if isCheckDisablingVariable(key, v.Value) {
				varsValid = false
				break
			}
		}
		if !varsValid {
			continue
		}

		p.Ref = detail.Ref
		p.Source = "web"
		res := evaluatePipeline(ctx, client, token, apiBase, project, p, commitSHA, expectedRef, reqJobs)
		res.TriggeredBy = username
		if res.Allowed {
			return res, true
		}
		if res.Outcome == OutcomeWait && waitingCandidate == nil {
			resCopy := res
			waitingCandidate = &resCopy
		}
	}

	if waitingCandidate != nil {
		return *waitingCandidate, true
	}
	return Result{}, false
}

func isCheckDisablingVariable(key, value string) bool {
	k := strings.ToUpper(strings.TrimSpace(key))
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == "0" || v == "false" || v == "no" || v == "off" {
		return false
	}
	return strings.HasPrefix(k, "SKIP_") ||
		strings.HasSuffix(k, "_SKIP") ||
		strings.HasPrefix(k, "NO_TEST") ||
		strings.HasPrefix(k, "DISABLE_") ||
		strings.Contains(k, "BYPASS")
}

type pipelineDetail struct {
	ID   int64  `json:"id"`
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
	User struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

type pipelineVariable struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func fetchPipelineDetail(ctx context.Context, client *http.Client, token, apiBase, project string, pipelineID int64) (pipelineDetail, bool) {
	if pipelineID <= 0 {
		return pipelineDetail{}, false
	}
	endpoint := fmt.Sprintf("%s/projects/%s/pipelines/%d", apiBase, project, pipelineID)
	body, code, err := doGitLabGET(ctx, client, token, endpoint)
	if err != nil || code >= 300 {
		return pipelineDetail{}, false
	}
	var d pipelineDetail
	if err := json.Unmarshal(body, &d); err != nil {
		return pipelineDetail{}, false
	}
	return d, true
}

func fetchPipelineUser(ctx context.Context, client *http.Client, token, apiBase, project string, pipelineID int64) string {
	d, ok := fetchPipelineDetail(ctx, client, token, apiBase, project, pipelineID)
	if !ok {
		return ""
	}
	return strings.TrimSpace(d.User.Username)
}

func fetchPipelineVariables(ctx context.Context, client *http.Client, token, apiBase, project string, pipelineID int64) ([]pipelineVariable, bool) {
	endpoint := fmt.Sprintf("%s/projects/%s/pipelines/%d/variables", apiBase, project, pipelineID)
	body, code, err := doGitLabGET(ctx, client, token, endpoint)
	if err != nil || code >= 300 {
		return nil, false
	}
	var vars []pipelineVariable
	if err := json.Unmarshal(body, &vars); err != nil {
		return nil, false
	}
	return vars, true
}

func fetchAllJobs(ctx context.Context, client *http.Client, token, baseEndpoint string) ([]glJob, error) {
	var all []glJob
	page := 1
	for page <= 20 {
		endpoint := fmt.Sprintf("%s?per_page=100&page=%d", baseEndpoint, page)
		body, code, nextPage, err := doGitLabGETWithNext(ctx, client, token, endpoint)
		if err != nil || code >= 300 {
			return nil, fmt.Errorf("failed to query pipeline jobs (HTTP %d)", code)
		}
		var batch []glJob
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("invalid pipeline jobs JSON")
		}
		all = append(all, batch...)
		if nextPage <= page {
			break
		}
		page = nextPage
	}
	return all, nil
}

func fetchAllBridges(ctx context.Context, client *http.Client, token, baseEndpoint string) ([]glBridge, bool, error) {
	var all []glBridge
	page := 1
	for page <= 10 {
		endpoint := fmt.Sprintf("%s?per_page=100&page=%d", baseEndpoint, page)
		body, code, nextPage, err := doGitLabGETWithNext(ctx, client, token, endpoint)
		if err != nil {
			return nil, false, fmt.Errorf("failed to query pipeline bridges")
		}
		if code == http.StatusNotFound {
			return nil, false, nil
		}
		if code >= 300 {
			return nil, false, fmt.Errorf("failed to query pipeline bridges (HTTP %d)", code)
		}
		var batch []glBridge
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, false, fmt.Errorf("invalid pipeline bridges JSON")
		}
		all = append(all, batch...)
		if nextPage <= page {
			break
		}
		page = nextPage
	}
	return all, true, nil
}

func doGitLabGET(ctx context.Context, client *http.Client, token, endpoint string) ([]byte, int, error) {
	body, code, _, err := doGitLabGETWithNext(ctx, client, token, endpoint)
	return body, code, err
}

func doGitLabGETWithNext(ctx context.Context, client *http.Client, token, endpoint string) ([]byte, int, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, 0, err
	}
	next := 0
	if rawNext := strings.TrimSpace(resp.Header.Get("X-Next-Page")); rawNext != "" {
		next, _ = strconv.Atoi(rawNext)
	}
	return body, resp.StatusCode, next, nil
}
