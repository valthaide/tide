package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Parent-inheritance: when the commit to deploy has no pipeline at all
// (e.g. pushed with [skip ci]), Tide diffs it against its parent and, while
// no deploy file changed, walks up the first-parent chain until a commit
// with a counted pipeline is found. Only a green ancestor pipeline verifies
// the commit; any other status keeps the current waiting behavior.

const (
	// diffCacheTTL bounds how long one commit's diff is reused; diffs are
	// immutable, the TTL only caps memory and token churn.
	diffCacheTTL = 6 * time.Hour
	// diffCacheMax caps cached commits; a full cache is reset wholesale.
	diffCacheMax = 4096
	// maxParentChain bounds the ancestor walk for one evaluation.
	maxParentChain = 25
)

// commitDiffInfo is the cached, immutable record for one commit: its first
// parent and the paths its diff touched.
type commitDiffInfo struct {
	parent  string
	paths   []string
	expires time.Time
}

var diffCache = struct {
	sync.Mutex
	entries map[string]commitDiffInfo
}{entries: map[string]commitDiffInfo{}}

// cachedCommitDiff returns the first parent and changed paths of a commit,
// fetched from GitLab at most once per commit (per cache TTL). ok is false
// when the commit or its diff could not be read — callers then keep the
// current behavior (wait for a pipeline).
func cachedCommitDiff(ctx context.Context, client *http.Client, token, apiBase, project, sha string) (commitDiffInfo, bool) {
	key := apiBase + "|" + project + "|" + sha
	now := time.Now()
	diffCache.Lock()
	if info, hit := diffCache.entries[key]; hit && now.Before(info.expires) {
		diffCache.Unlock()
		return info, true
	}
	diffCache.Unlock()

	info, err := fetchCommitDiff(ctx, client, token, apiBase, project, sha)
	if err != nil {
		return commitDiffInfo{}, false
	}
	info.expires = now.Add(diffCacheTTL)

	diffCache.Lock()
	if len(diffCache.entries) >= diffCacheMax {
		diffCache.entries = map[string]commitDiffInfo{}
	}
	diffCache.entries[key] = info
	diffCache.Unlock()
	return info, true
}

// fetchCommitDiff reads the commit (for its first parent) and its diff.
func fetchCommitDiff(ctx context.Context, client *http.Client, token, apiBase, project, sha string) (commitDiffInfo, error) {
	commitURL := fmt.Sprintf("%s/projects/%s/repository/commits/%s", apiBase, project, url.PathEscape(sha))
	body, code, err := doGitLabGET(ctx, client, token, commitURL)
	if err != nil || code >= 300 {
		return commitDiffInfo{}, fmt.Errorf("failed to query commit (HTTP %d)", code)
	}
	var commit struct {
		ParentIDs []string `json:"parent_ids"`
	}
	if err := json.Unmarshal(body, &commit); err != nil {
		return commitDiffInfo{}, fmt.Errorf("invalid commit JSON")
	}
	info := commitDiffInfo{}
	if len(commit.ParentIDs) > 0 {
		info.parent = strings.TrimSpace(commit.ParentIDs[0])
	}

	seen := map[string]struct{}{}
	page := 1
	for page <= 20 {
		diffURL := fmt.Sprintf("%s/projects/%s/repository/commits/%s/diff?per_page=100&page=%d",
			apiBase, project, url.PathEscape(sha), page)
		dBody, dCode, nextPage, dErr := doGitLabGETWithNext(ctx, client, token, diffURL)
		if dErr != nil || dCode >= 300 {
			return commitDiffInfo{}, fmt.Errorf("failed to query commit diff (HTTP %d)", dCode)
		}
		var entries []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		}
		if err := json.Unmarshal(dBody, &entries); err != nil {
			return commitDiffInfo{}, fmt.Errorf("invalid commit diff JSON")
		}
		for _, e := range entries {
			for _, p := range []string{e.OldPath, e.NewPath} {
				p = NormalizeRepoRelPath(p)
				if p == "" {
					continue
				}
				if _, dup := seen[p]; dup {
					continue
				}
				seen[p] = struct{}{}
				info.paths = append(info.paths, p)
			}
		}
		if nextPage <= page {
			break
		}
		page = nextPage
	}
	return info, nil
}

// NormalizeRepoRelPath canonicalizes a repo-relative path for comparison:
// slash-separated, no leading ./ or /.
func NormalizeRepoRelPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	p = strings.Trim(p, "/")
	if p == "." {
		return ""
	}
	return p
}

// deployPathsTouched reports whether any changed path matches the app's
// deploy files. Every entry also matches as a directory prefix (a changed
// file under it counts); only entries with glob metacharacters are reduced
// to literal match. A false positive only keeps waiting — a missed match
// would deploy unverified, so the match errs on the inclusive side.
func deployPathsTouched(changed []string, deployFiles []string) bool {
	type entry struct {
		path string
		glob bool
	}
	var entries []entry
	for _, f := range deployFiles {
		f = NormalizeRepoRelPath(f)
		if f == "" {
			continue
		}
		entries = append(entries, entry{path: f, glob: strings.ContainsAny(f, "*?[")})
	}
	for _, changedPath := range changed {
		changedPath = NormalizeRepoRelPath(changedPath)
		if changedPath == "" {
			continue
		}
		for _, e := range entries {
			if changedPath == e.path {
				return true
			}
			if !e.glob && strings.HasPrefix(changedPath, e.path+"/") {
				return true
			}
		}
	}
	return false
}

// inheritGreenParent walks the first-parent chain while commits have no
// counted pipeline and touch no deploy files, and returns the inherited
// allow result when an ancestor's pipeline is green. Any other outcome
// (diff unreadable, deploy file changed, red/absent ancestor chain) keeps
// the current waiting behavior.
func (c *Checker) inheritGreenParent(ctx context.Context, client *http.Client, token, apiBase, project, commitSHA string, req CheckRequest) (Result, bool) {
	if len(req.DeployFiles) == 0 {
		return Result{}, false
	}
	sha := commitSHA
	for depth := 0; depth < maxParentChain; depth++ {
		info, ok := cachedCommitDiff(ctx, client, token, apiBase, project, sha)
		if !ok {
			return Result{}, false
		}
		if deployPathsTouched(info.paths, req.DeployFiles) {
			return Result{}, false
		}
		if info.parent == "" {
			return Result{}, false
		}
		res, found := c.checkAncestorPipeline(ctx, client, token, apiBase, project, info.parent, req)
		if !found {
			sha = info.parent
			continue
		}
		if res.Allowed && res.Outcome == OutcomeAllow {
			parent := info.parent
			if len(parent) > 12 {
				parent = parent[:12]
			}
			res.InheritedFrom = info.parent
			res.Detail = fmt.Sprintf("no deploy files changed; inherited pipeline id=%d source=%s status=%s from parent %s",
				res.PipelineID, res.Source, res.Status, parent)
			return res, true
		}
		return Result{}, false
	}
	return Result{}, false
}

// checkAncestorPipeline evaluates the counted pipeline of one ancestor
// commit. found is false when the ancestor has no counted pipeline and the
// walk should continue up the chain.
func (c *Checker) checkAncestorPipeline(ctx context.Context, client *http.Client, token, apiBase, project, sha string, req CheckRequest) (Result, bool) {
	pipelines, err := fetchPipelines(ctx, client, token, apiBase, project, sha)
	if err != nil {
		return Result{}, false
	}
	chosen, hasPrimary := selectPrimaryPipeline(pipelines, req.Sources, req.Ref, req.StrictRef)
	if !hasPrimary {
		return Result{}, false
	}
	res := evaluatePipeline(ctx, client, token, apiBase, project, chosen, sha, req.Ref, req.RequiredJobs)
	return res, true
}
