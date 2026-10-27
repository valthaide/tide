package fleet

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/fleetcheck"
)

// Report is the result of cps validate.
type Report struct {
	OK      bool               `json:"ok"`
	Issues  []fleetcheck.Issue `json:"issues"`
	Unknown []string           `json:"unknown_fields,omitempty"`
}

// ValidateOptions selects static and compose checks.
type ValidateOptions struct {
	AppsDir     string
	WithCompose bool
	Strict      string
}

// Validate checks a host file without a controller or a docker daemon.
// WithCompose runs `docker compose config`, which needs the binary, not the daemon.
func Validate(data []byte, opts ValidateOptions) (Report, error) {
	unknown, err := UnknownFields(data)
	if err != nil {
		return Report{}, err
	}
	doc, err := ParseHostDocument(data)
	if err != nil {
		return Report{}, err
	}
	var issues []fleetcheck.Issue
	var names []fleetcheck.AppName
	if _, _, issue := fleetcheck.ParseReconcileInterval(doc.ReconcileInterval); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := fleetcheck.CheckRetryRejected(doc.RetryRejected); issue != nil {
		issues = append(issues, *issue)
	}
	if issue := fleetcheck.CheckPriority(doc.Priority); issue != nil {
		issues = append(issues, *issue)
	}
	if doc.ReconcileConcurrency < 0 {
		issues = append(issues, fleetcheck.Issue{Rule: "invalid_concurrency", Message: "invalid_concurrency: reconcile_concurrency"})
	}
	for _, app := range doc.Apps {
		project := strings.TrimSpace(app.Project)
		if project == "" {
			project = compose.ProjectNameForApp(app.Name)
		}
		names = append(names, fleetcheck.AppName{Name: app.Name, Project: project})
		if issue := fleetcheck.CheckProject(app.Project); issue != nil {
			issue.App = app.Name
			issues = append(issues, *issue)
		}
		for _, raw := range []string{app.SyncTimeout, app.PullTimeout, app.OneshotTimeout, app.HealthTimeout, app.PollInterval} {
			if _, issue := fleetcheck.ParseDuration(raw); issue != nil {
				issue.App = app.Name
				issues = append(issues, *issue)
			}
		}
		if _, _, issue := fleetcheck.ParseReconcileInterval(app.ReconcileInterval); issue != nil {
			issue.App = app.Name
			issues = append(issues, *issue)
		}
		if issue := fleetcheck.CheckRetryRejected(app.RetryRejected); issue != nil {
			issue.App = app.Name
			issues = append(issues, *issue)
		}
		if issue := fleetcheck.CheckPriority(app.Priority); issue != nil {
			issue.App = app.Name
			issues = append(issues, *issue)
		}
	}
	for _, issue := range fleetcheck.ProjectConflicts(names) {
		issues = append(issues, issue)
	}
	if opts.WithCompose {
		more, composeErr := composeIssues(doc, opts.AppsDir)
		if composeErr != nil {
			return Report{}, composeErr
		}
		issues = append(issues, more...)
	}
	if StrictMode(opts.Strict) == "enforce" {
		for _, field := range unknown {
			issues = append(issues, fleetcheck.Issue{Rule: "unknown_field", Message: "unknown_field: " + field})
		}
	}
	issues = dedupeIssues(issues)
	return Report{OK: len(issues) == 0, Issues: issues, Unknown: unknown}, nil
}

func composeIssues(doc *HostDocument, appsDir string) ([]fleetcheck.Issue, error) {
	ports := map[int]string{}
	var issues []fleetcheck.Issue
	for _, app := range doc.Apps {
		if !appChecked(app) {
			continue
		}
		path, err := composeFile(appsDir, app)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("docker", "compose", "-f", path, "config", "--format", "json")
		cmd.Dir = filepath.Dir(path)
		cmd.Env = composeConfigEnv(os.Environ(), app.Vars)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("compose config %s: %w: %s", app.Name, err, strings.TrimSpace(stderr.String()))
		}
		sum, err := fleetcheck.SummarizeCompose(stdout.Bytes())
		if err != nil {
			return nil, fmt.Errorf("compose config %s: %w", app.Name, err)
		}
		issues = append(issues, configIssues(app, doc.Build, sum, ports)...)
	}
	return issues, nil
}

func configIssues(app AppSpec, hostBuild string, sum fleetcheck.Summary, ports map[int]string) []fleetcheck.Issue {
	var issues []fleetcheck.Issue
	if app.Build == "forbidden" || (app.Build == "" && hostBuild == "forbidden") {
		if len(sum.Build) > 0 {
			issues = append(issues, fleetcheck.Issue{App: app.Name, Rule: "build_forbidden", Message: "build_forbidden: " + strings.Join(sum.Build, ",")})
		}
	}
	if app.RequireDigest != nil && *app.RequireDigest {
		for _, issue := range fleetcheck.DigestRequired(sum.Images) {
			issue.App = app.Name
			issues = append(issues, issue)
		}
	}
	for _, port := range sum.Published {
		if owner, ok := ports[port]; ok {
			issues = append(issues, fleetcheck.PortConflict(app.Name, port, owner))
			continue
		}
		ports[port] = app.Name
	}
	return issues
}

func appChecked(app AppSpec) bool {
	return !app.Suspended
}

func composeConfigEnv(base []string, vars map[string]string) []string {
	if len(vars) == 0 {
		return base
	}
	skip := map[string]struct{}{}
	keys := make([]string, 0, len(vars))
	for key := range vars {
		skip[key] = struct{}{}
		keys = append(keys, key)
	}
	sortStrings(keys)
	var out []string
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			if _, drop := skip[key]; drop {
				continue
			}
		}
		out = append(out, item)
	}
	for _, key := range keys {
		out = append(out, key+"="+vars[key])
	}
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func dedupeIssues(in []fleetcheck.Issue) []fleetcheck.Issue {
	seen := map[string]struct{}{}
	var out []fleetcheck.Issue
	for _, issue := range in {
		if _, ok := seen[issue.Message]; ok {
			continue
		}
		seen[issue.Message] = struct{}{}
		out = append(out, issue)
	}
	return out
}

func composeFile(appsDir string, app AppSpec) (string, error) {
	name := app.ComposePath
	if strings.TrimSpace(name) == "" {
		name = "docker-compose.yml"
	}
	if appsDir == "" {
		return "", fmt.Errorf("apps dir is required with --with-compose")
	}
	candidates := []string{
		filepath.Join(appsDir, app.Name, name),
		filepath.Join(appsDir, name),
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("compose file not found for %s", app.Name)
}
