package fleet

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/fleetcheck"
)

// Report is the result of validating one host file.
type Report struct {
	File    string             `json:"file,omitempty"`
	OK      bool               `json:"ok"`
	Issues  []fleetcheck.Issue `json:"issues"`
	Unknown []string           `json:"unknown_fields,omitempty"`
}

// ValidateOptions selects static and compose checks.
type ValidateOptions struct {
	AppsDir     string
	WithCompose bool
	Strict      string
	// FleetDir is the fleet checkout root holding templates/<name>/. It is
	// only needed with WithCompose for template-sourced apps.
	FleetDir string
}

// Validate checks a host file without a controller or a docker daemon.
// It is the single validation entry point: `tide validate`, `tidectl
// validate`, and the controller's fleet loading all call ValidateDocument,
// so a file that passes offline cannot be rejected by the controller for
// format. WithCompose additionally runs `docker compose config`, which
// needs the binary, not the daemon.
func Validate(data []byte, opts ValidateOptions) (Report, error) {
	report, _, err := ValidateDocument(data, opts)
	return report, err
}

// ValidateDocument is Validate plus the parsed document.
func ValidateDocument(data []byte, opts ValidateOptions) (Report, *HostDocument, error) {
	unknown, err := UnknownFields(data)
	if err != nil {
		return Report{}, nil, err
	}
	doc, err := ParseHostDocument(data)
	if err != nil {
		return Report{}, nil, err
	}
	var issues []fleetcheck.Issue
	// Generated-schema validation (types, enum, unknown properties).
	if schemaFound, schemaErr := schemaIssues(data); schemaErr == nil {
		for _, issue := range schemaFound {
			// Unknown properties are governed by TIDE_FLEET_STRICT below and
			// already listed in Report.Unknown; do not duplicate them.
			if issue.Rule == "unknown_field" {
				continue
			}
			if idx, ok := appIndexFromPath(issue.Path); ok && idx < len(doc.Apps) {
				issue.App = doc.Apps[idx].Name
			}
			issues = append(issues, issue)
		}
	}
	var names []fleetcheck.AppName
	if _, _, issue := fleetcheck.ParseReconcileInterval(doc.ReconcileInterval); issue != nil {
		issue.Path = "reconcile_interval"
		issues = append(issues, *issue)
	}
	if issue := fleetcheck.CheckRetryRejected(doc.RetryRejected); issue != nil {
		issue.Path = "retry_rejected"
		issues = append(issues, *issue)
	}
	if issue := fleetcheck.CheckPriority(doc.Priority); issue != nil {
		issue.Path = "priority"
		issues = append(issues, *issue)
	}
	if doc.CreateVolumes && strings.TrimSpace(doc.VolumeNamePattern) != "" {
		if _, err := regexp.Compile(doc.VolumeNamePattern); err != nil {
			issues = append(issues, fleetcheck.Issue{Path: "volume_name_pattern", Rule: "invalid_volume_pattern", Message: "invalid_volume_pattern: " + err.Error()})
		}
	}
	if doc.ReconcileConcurrency < 0 {
		issues = append(issues, fleetcheck.Issue{Path: "reconcile_concurrency", Rule: "invalid_concurrency", Message: "invalid_concurrency: reconcile_concurrency"})
	}
	seenNames := map[string]int{}
	for i, app := range doc.Apps {
		prefix := fmt.Sprintf("apps[%d]", i)
		project := strings.TrimSpace(app.Project)
		if project == "" {
			project = compose.ProjectNameForApp(app.Name)
		}
		names = append(names, fleetcheck.AppName{Name: app.Name, Project: project})
		if prev, dup := seenNames[app.Name]; dup && app.Name != "" {
			issues = append(issues, fleetcheck.Issue{App: app.Name, Path: prefix + ".name", Rule: "duplicate_app_name", Message: fmt.Sprintf("duplicate_app_name: app name %q is used by both apps[%d] and apps[%d] on this host", app.Name, prev, i)})
		} else if app.Name != "" {
			seenNames[app.Name] = i
		}
		if issue := fleetcheck.CheckProject(app.Project); issue != nil {
			issue.App = app.Name
			issue.Path = prefix + ".project"
			issues = append(issues, *issue)
		}
		for field, raw := range map[string]string{"sync_timeout": app.SyncTimeout, "pull_timeout": app.PullTimeout, "oneshot_timeout": app.OneshotTimeout, "health_timeout": app.HealthTimeout, "poll_interval": app.PollInterval} {
			if _, issue := fleetcheck.ParseDuration(raw); issue != nil {
				issue.App = app.Name
				issue.Path = prefix + "." + field
				issues = append(issues, *issue)
			}
		}
		if _, _, issue := fleetcheck.ParseReconcileInterval(app.ReconcileInterval); issue != nil {
			issue.App = app.Name
			issue.Path = prefix + ".reconcile_interval"
			issues = append(issues, *issue)
		}
		if issue := fleetcheck.CheckRetryRejected(app.RetryRejected); issue != nil {
			issue.App = app.Name
			issue.Path = prefix + ".retry_rejected"
			issues = append(issues, *issue)
		}
		if issue := fleetcheck.CheckPriority(app.Priority); issue != nil {
			issue.App = app.Name
			issue.Path = prefix + ".priority"
			issues = append(issues, *issue)
		}
		if strings.TrimSpace(app.Template) != "" && strings.TrimSpace(app.ComposePath) != "" {
			issues = append(issues, fleetcheck.Issue{App: app.Name, Path: prefix + ".template", Rule: "template_compose_path_conflict", Message: "template_compose_path_conflict: app " + app.Name + " sets both template and compose_path; set only one"})
		}
		if issue := fleetcheck.CheckTemplateName(app.Template); issue != nil {
			issue.App = app.Name
			issue.Path = prefix + ".template"
			issues = append(issues, *issue)
		}
		if issue := fleetcheck.CheckAppDir(app.AppDir); issue != nil {
			issue.App = app.Name
			issue.Path = prefix + ".app_dir"
			issues = append(issues, *issue)
		}
		if strings.TrimSpace(app.AppDir) != "" && strings.TrimSpace(app.Template) == "" {
			issues = append(issues, fleetcheck.Issue{App: app.Name, Path: prefix + ".app_dir", Rule: "app_dir_requires_template", Message: "app_dir_requires_template: app " + app.Name + " sets app_dir without template; app_dir only applies to the fleet template compose source"})
		}
		// Template references must resolve inside the fleet checkout. The
		// controller has the checkout; offline validation derives it from
		// the host file location. Without a fleet dir the reference cannot
		// be checked and is skipped.
		if strings.TrimSpace(app.Template) != "" && strings.TrimSpace(opts.FleetDir) != "" {
			if fleetcheck.ValidTemplateName(app.Template) {
				templateFile := filepath.Join(opts.FleetDir, "templates", app.Template, "compose.yaml")
				if _, statErr := os.Stat(templateFile); statErr != nil {
					issues = append(issues, fleetcheck.Issue{App: app.Name, Path: prefix + ".template", Rule: "template_not_found", Message: fmt.Sprintf("template_not_found: app %s references template %q but %s does not exist", app.Name, app.Template, templateFile)})
				}
			}
		}
	}
	for _, issue := range fleetcheck.ProjectConflicts(names) {
		if issue.Path == "" {
			for i, app := range doc.Apps {
				if app.Name == issue.App {
					if issue.Rule == "name_conflict" {
						issue.Path = fmt.Sprintf("apps[%d].name", i)
					} else {
						issue.Path = fmt.Sprintf("apps[%d].project", i)
					}
					break
				}
			}
		}
		issues = append(issues, issue)
	}
	if opts.WithCompose {
		more, composeErr := composeIssues(doc, opts)
		if composeErr != nil {
			return Report{}, nil, composeErr
		}
		issues = append(issues, more...)
	}
	if StrictMode(opts.Strict) == "enforce" {
		for _, field := range unknown {
			path := field
			if idx := strings.LastIndex(field, "."); idx >= 0 {
				path = field // apps[0].field already is a path
			}
			issues = append(issues, fleetcheck.Issue{Path: path, Rule: "unknown_field", Message: "unknown_field: " + field})
		}
	}
	issues = dedupeIssues(issues)
	return Report{OK: len(issues) == 0, Issues: issues, Unknown: unknown}, doc, nil
}

// appIndexFromPath extracts N from a path like apps[N].field.
func appIndexFromPath(path string) (int, bool) {
	if !strings.HasPrefix(path, "apps[") {
		return 0, false
	}
	rest := strings.TrimPrefix(path, "apps[")
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(rest[:end], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// FleetDirForFile derives the fleet checkout root from a host file path:
// hosts/<host>.yaml lives one level below the root, a host file may also
// sit directly at the root.
func FleetDirForFile(path string) string {
	abs, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return ""
	}
	if filepath.Base(abs) == "hosts" {
		return filepath.Dir(abs)
	}
	return abs
}

// ValidateFiles validates host files from disk without a controller, DB,
// or network. Each returned Report carries its file name; a read/parse
// failure becomes a report with one issue and OK=false so CI sees every
// file's result in one run.
func ValidateFiles(paths []string, opts ValidateOptions) []Report {
	reports := make([]Report, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			reports = append(reports, Report{File: path, OK: false, Issues: []fleetcheck.Issue{{Rule: "read_error", Message: "read_error: " + err.Error()}}})
			continue
		}
		fileOpts := opts
		if strings.TrimSpace(fileOpts.FleetDir) == "" {
			fileOpts.FleetDir = FleetDirForFile(path)
		}
		report, err := Validate(data, fileOpts)
		if err != nil {
			reports = append(reports, Report{File: path, OK: false, Issues: []fleetcheck.Issue{{Rule: "parse_error", Message: err.Error()}}})
			continue
		}
		report.File = path
		reports = append(reports, report)
	}
	return reports
}

// ReportsOK reports whether every file validated cleanly.
func ReportsOK(reports []Report) bool {
	for _, r := range reports {
		if !r.OK {
			return false
		}
	}
	return true
}

func composeIssues(doc *HostDocument, opts ValidateOptions) ([]fleetcheck.Issue, error) {
	appsDir := opts.AppsDir
	ports := map[int]string{}
	var issues []fleetcheck.Issue
	for _, app := range doc.Apps {
		if !appChecked(app) {
			continue
		}
		if strings.TrimSpace(app.Template) != "" {
			// Template source: render the fleet template merged with the
			// app's override, exactly as a deploy would, then check the result.
			if strings.TrimSpace(opts.FleetDir) == "" {
				return nil, fmt.Errorf("fleet dir is required with --with-compose for template app %s (pass a fleet file inside its checkout)", app.Name)
			}
			appDir := appsDir
			if appsDir != "" {
				if info, statErr := os.Stat(filepath.Join(appsDir, app.Name)); statErr == nil && info.IsDir() {
					appDir = filepath.Join(appsDir, app.Name)
				}
			}
			if appDir == "" {
				return nil, fmt.Errorf("apps dir is required with --with-compose for template app %s", app.Name)
			}
			project := strings.TrimSpace(app.Project)
			if project == "" {
				project = compose.ProjectNameForApp(app.Name)
			}
			exec := compose.NewComposeExecutor(nil)
			_, configJSON, checkErr := exec.CheckTemplate(context.Background(), opts.FleetDir, app.Template, appDir, app.AppDir, project, app.Vars)
			if checkErr != nil {
				issues = append(issues, fleetcheck.Issue{App: app.Name, Rule: "template_compose_invalid", Message: checkErr.Error()})
				continue
			}
			sum, sumErr := fleetcheck.SummarizeCompose(configJSON)
			if sumErr != nil {
				return nil, fmt.Errorf("compose config %s: %w", app.Name, sumErr)
			}
			issues = append(issues, configIssues(app, doc.Build, sum, ports)...)
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
