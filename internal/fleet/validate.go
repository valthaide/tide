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
	return Report{OK: len(issues) == 0, Issues: issues, Unknown: unknown}, nil
}

func composeIssues(doc *HostDocument, appsDir string) ([]fleetcheck.Issue, error) {
	ports := map[int]string{}
	var issues []fleetcheck.Issue
	for _, app := range doc.Apps {
		path, err := composeFile(appsDir, app)
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("docker", "compose", "-f", path, "config", "--format", "json")
		cmd.Dir = filepath.Dir(path)
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
		if app.Build == "forbidden" || (app.Build == "" && doc.Build == "forbidden") {
			if len(sum.Images) == 0 {
				issues = append(issues, fleetcheck.Issue{App: app.Name, Rule: "build_forbidden", Message: "build_forbidden: no image"})
			}
		}
		if app.RequireDigest {
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
	}
	return issues, nil
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
