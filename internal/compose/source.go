package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/fleetcheck"
	"github.com/valthaide/tide/internal/syncerr"
)

// OverrideFileName re-exported for callers that build app paths.
const OverrideFileName = fleetcheck.OverrideFileName

// Invocation is one resolved `docker compose` invocation: the files to load
// (in merge order) and the directory relative paths, `.env` and `env_file`
// resolve from.
//
// Merge order is always: base compose (fleet template or the app's own
// compose_path file), then the app's optional compose.override.yaml, then
// Tide's own generated files (passed separately as tideArgs).
type Invocation struct {
	// ProjectDir is passed as --project-directory and used as the process
	// working directory. Relative paths, `.env` and `env_file` resolve here.
	ProjectDir string
	// Files are the -f files before Tide's own files, in merge order.
	Files []string
	// Primary is the base compose file (template or compose_path file).
	Primary string
	// Template is the fleet template name, empty in compose_path mode.
	Template string
	// TemplateFile is the resolved fleet template file, template mode only.
	TemplateFile string
	// AppDir is the app folder relative to the repository root in template
	// mode ("" = root); ProjectDir is its absolute form.
	AppDir string
	// HasOverride reports whether <app dir>/compose.override.yaml exists.
	HasOverride bool
	// OverrideFile is the app override file when HasOverride is true.
	OverrideFile string
}

// Args builds `docker compose` arguments: --project-directory first, then
// -p, then the resolved -f files, then Tide's own -f files, then the verb.
func (inv Invocation) Args(project string, tideArgs []string, verb ...string) []string {
	args := []string{"compose"}
	if strings.TrimSpace(inv.ProjectDir) != "" {
		args = append(args, "--project-directory", inv.ProjectDir)
	}
	args = append(args, "-p", project)
	for _, f := range inv.Files {
		args = append(args, "-f", f)
	}
	args = append(args, tideArgs...)
	args = append(args, verb...)
	return args
}

// Describe is a short, secret-free source description for logs and status.
func (inv Invocation) Describe() string {
	if inv.Template != "" {
		override := "none"
		if inv.HasOverride {
			override = inv.OverrideFile
		}
		return fmt.Sprintf("template=%s file=%s override=%s project_directory=%s", inv.Template, inv.TemplateFile, override, inv.ProjectDir)
	}
	return fmt.Sprintf("compose_path=%s project_directory=%s", inv.Primary, inv.ProjectDir)
}

// fleetCheckoutDir resolves the fleet checkout this executor reads templates
// from: an explicit ApplyOptions value, then the executor field (wired from
// the fleet reconciler), then TIDE_FLEET_CHECKOUT_DIR.
func (e *ComposeExecutor) fleetCheckoutDir(opts ApplyOptions) string {
	if d := strings.TrimSpace(opts.FleetCheckoutDir); d != "" {
		return d
	}
	if e != nil && strings.TrimSpace(e.FleetCheckoutDir) != "" {
		return e.FleetCheckoutDir
	}
	return strings.TrimSpace(envcompat.Getenv("TIDE_FLEET_CHECKOUT_DIR"))
}

// TemplateFileFor returns templates/<template>/compose.yaml inside a fleet
// checkout, after validating the template name.
func TemplateFileFor(fleetCheckoutDir, template string) (string, error) {
	template = strings.TrimSpace(template)
	if issue := fleetcheck.CheckTemplateName(template); issue != nil {
		return "", syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issue.Message)
	}
	if template == "" {
		return "", syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, "invalid_template: template is empty")
	}
	root := strings.TrimSpace(fleetCheckoutDir)
	if root == "" {
		return "", syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
			fmt.Sprintf("template_unavailable: template %q requires a fleet checkout (fleet is not configured on this controller)", template))
	}
	return filepath.Join(root, "templates", template, fleetcheck.TemplateFileName), nil
}

// ResolveInvocation resolves the compose source for one app checkout.
//
// Template mode: the base file is the fleet template, --project-directory is
// the app folder in its own repository (the checkout root, or the app_dir
// subfolder when set), and an optional compose.override.yaml in that folder
// is merged after the template. A missing override is silently skipped; a
// missing/invalid template or app_dir is a validation error so a deploy
// never starts from a half-resolved source.
//
// compose_path mode keeps the historical layout: the file from the app repo,
// with its own directory as the project directory.
func (e *ComposeExecutor) ResolveInvocation(repoDir, composePath string, opts ApplyOptions) (Invocation, error) {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return Invocation{}, fmt.Errorf("repository directory is empty")
	}
	if abs, err := filepath.Abs(repoDir); err == nil {
		repoDir = abs
	}

	template := strings.TrimSpace(opts.Template)
	if template == "" {
		composeRel := strings.TrimSpace(composePath)
		if composeRel == "" {
			composeRel = "docker-compose.yml"
		}
		full := filepath.Join(repoDir, composeRel)
		if info, err := os.Stat(full); err != nil || info.IsDir() {
			if err == nil {
				err = fmt.Errorf("is a directory")
			}
			return Invocation{}, fmt.Errorf("compose file not found: %s: %w", full, err)
		}
		return Invocation{
			ProjectDir: filepath.Dir(full),
			Files:      []string{full},
			Primary:    full,
		}, nil
	}

	templateFile, err := TemplateFileFor(e.fleetCheckoutDir(opts), template)
	if err != nil {
		return Invocation{}, err
	}
	if info, statErr := os.Stat(templateFile); statErr != nil || info.IsDir() {
		return Invocation{}, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
			fmt.Sprintf("template_unavailable: template %q not found in fleet checkout: %s", template, templateFile))
	}

	appFolder, appErr := appFolderDir(repoDir, opts.AppDir)
	if appErr != nil {
		return Invocation{}, appErr
	}

	inv := Invocation{
		ProjectDir:   appFolder,
		Primary:      templateFile,
		Template:     template,
		TemplateFile: templateFile,
		AppDir:       fleetcheck.NormalizeAppDir(opts.AppDir),
		Files:        []string{templateFile},
	}
	override := filepath.Join(appFolder, fleetcheck.OverrideFileName)
	if info, statErr := os.Stat(override); statErr == nil {
		if info.IsDir() {
			return Invocation{}, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
				fmt.Sprintf("template_override_invalid: %s is a directory, expected a compose file", override))
		}
		inv.HasOverride = true
		inv.OverrideFile = override
		inv.Files = append(inv.Files, override)
	} else if !os.IsNotExist(statErr) {
		return Invocation{}, fmt.Errorf("stat app compose override %s: %w", override, statErr)
	}
	return inv, nil
}

// appFolderDir resolves the compose project directory for template mode:
// the repository root, or its app_dir subfolder. The folder must exist and
// stay inside the checkout; app_dir itself was validated at registration,
// so this is a defense-in-depth check against escapes and typos.
func appFolderDir(repoDir, appDir string) (string, error) {
	// Validate the raw value first so backslashes and ".." are rejected
	// before NormalizeAppDir collapses them into a lookalike relative path.
	if issue := fleetcheck.CheckAppDir(appDir); issue != nil {
		return "", syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issue.Message)
	}
	rel := fleetcheck.NormalizeAppDir(appDir)
	folder := repoDir
	if rel != "" {
		folder = filepath.Join(repoDir, filepath.FromSlash(rel))
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		if rel == "" {
			return "", fmt.Errorf("app directory %s: %w", folder, err)
		}
		return "", syncerr.New(syncerr.CodeValidation, syncerr.StageValidate,
			fmt.Sprintf("app_dir_not_found: app_dir %q does not exist in the app repository: %s", rel, folder))
	}
	return folder, nil
}

// rememberSource keeps the last resolved invocation for an app so Destroy /
// Restart / Exec can address a template-sourced stack after Apply.
func (e *ComposeExecutor) rememberSource(appID string, inv Invocation) {
	if e == nil || strings.TrimSpace(appID) == "" || len(inv.Files) == 0 {
		return
	}
	e.sourcesMu.Lock()
	defer e.sourcesMu.Unlock()
	if e.sources == nil {
		e.sources = map[string]Invocation{}
	}
	e.sources[appID] = inv
}

func (e *ComposeExecutor) rememberedSource(appID string) (Invocation, bool) {
	if e == nil {
		return Invocation{}, false
	}
	e.sourcesMu.Lock()
	defer e.sourcesMu.Unlock()
	inv, ok := e.sources[appID]
	return inv, ok
}

// invocationForRuntime resolves the invocation for runtime operations
// (down/restart/exec): the remembered source when present, otherwise the
// legacy compose_path layout inside the app checkout.
func (e *ComposeExecutor) invocationForRuntime(appID, composePath string) (Invocation, error) {
	if inv, ok := e.rememberedSource(appID); ok {
		return inv, nil
	}
	appDirAbs, err := filepath.Abs(filepath.Join(e.WorkDir, appID))
	if err != nil {
		return Invocation{}, err
	}
	repoDir := filepath.Join(appDirAbs, "repo")
	if strings.TrimSpace(composePath) == "" {
		composePath = "compose.yaml"
	}
	full := filepath.Join(repoDir, composePath)
	return Invocation{ProjectDir: filepath.Dir(full), Files: []string{full}, Primary: full}, nil
}

// HasOverrideForApp reports whether the app checkout currently contains
// compose.override.yaml in its app folder (repo root or app_dir). It never
// errors: a missing checkout means no override.
func (e *ComposeExecutor) HasOverrideForApp(appID, appDir string) bool {
	if e == nil || strings.TrimSpace(appID) == "" {
		return false
	}
	if inv, ok := e.rememberedSource(appID); ok && inv.Template != "" {
		return inv.HasOverride
	}
	folder := filepath.Join(e.WorkDir, appID, "repo")
	if rel := fleetcheck.NormalizeAppDir(appDir); rel != "" {
		folder = filepath.Join(folder, filepath.FromSlash(rel))
	}
	info, err := os.Stat(filepath.Join(folder, fleetcheck.OverrideFileName))
	return err == nil && !info.IsDir()
}

// TemplateHashForApp hashes the effective template source of an app: the
// fleet template file plus the app override (present/absent + content).
// It returns "" for compose_path apps and when the source cannot be read, so
// drift detection stays silent instead of forcing redeploys.
func (e *ComposeExecutor) TemplateHashForApp(appID, template, appDir string) (string, error) {
	template = strings.TrimSpace(template)
	if e == nil || template == "" {
		return "", nil
	}
	templateFile, err := TemplateFileFor(e.fleetCheckoutDir(ApplyOptions{}), template)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	data, err := os.ReadFile(templateFile)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "template:%s\n", hex.EncodeToString(sha256Sum(data)))
	fmt.Fprintf(h, "app_dir:%s\n", fleetcheck.NormalizeAppDir(appDir))
	folder := filepath.Join(e.WorkDir, appID, "repo")
	if rel := fleetcheck.NormalizeAppDir(appDir); rel != "" {
		folder = filepath.Join(folder, filepath.FromSlash(rel))
	}
	override := filepath.Join(folder, fleetcheck.OverrideFileName)
	if overrideData, readErr := os.ReadFile(override); readErr == nil {
		fmt.Fprintf(h, "override:%s\n", hex.EncodeToString(sha256Sum(overrideData)))
	} else {
		h.Write([]byte("override:none\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// CheckTemplate renders and validates a fleet template merged with an app
// override, without a controller, Docker daemon state, pull, or apply. It is
// the engine behind `tidectl check-template` for CI: it runs
// `docker compose config` exactly the way a deploy would (same file order,
// same --project-directory) and fails before anything is started.
//
// repoDir is the app repository checkout (or a CI working copy) and
// appDirRel its app folder inside it ("" = root); .env, env_file, and the
// override are read from that folder, exactly as a deploy resolves them.
func (e *ComposeExecutor) CheckTemplate(ctx context.Context, fleetCheckoutDir, template, repoDir, appDirRel, project string, vars map[string]string) (Invocation, []byte, error) {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return Invocation{}, nil, fmt.Errorf("app repository directory is required")
	}
	if abs, err := filepath.Abs(repoDir); err == nil {
		repoDir = abs
	}
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return Invocation{}, nil, fmt.Errorf("app repository directory %s: %w", repoDir, err)
	}
	opts := ApplyOptions{Template: strings.TrimSpace(template), AppDir: appDirRel, FleetCheckoutDir: fleetCheckoutDir, Vars: vars}
	inv, err := e.ResolveInvocation(repoDir, "", opts)
	if err != nil {
		return Invocation{}, nil, err
	}
	if inv.Template == "" {
		return Invocation{}, nil, fmt.Errorf("template is required")
	}
	if strings.TrimSpace(project) == "" {
		project = composeProjectName(filepath.Base(inv.ProjectDir))
	}
	configJSON, err := renderComposeConfigDirect(ctx, inv, project, vars)
	if err != nil {
		return inv, nil, err
	}
	return inv, configJSON, nil
}

// renderComposeConfigDirect runs `docker compose config` without the
// controller's Docker toolchain resolution (which probes the daemon). CI has
// the compose CLI but often no daemon, and `config` needs neither a daemon
// nor a controller — only the exact invocation a deploy would use.
func renderComposeConfigDirect(ctx context.Context, inv Invocation, project string, vars map[string]string) ([]byte, error) {
	dockerPath := strings.TrimSpace(envcompat.Getenv("TIDE_DOCKER_CLI_PATH"))
	if dockerPath == "" {
		dockerPath = "docker"
	}
	args := inv.Args(project, nil, "config", "--format", "json")
	cmd := exec.CommandContext(ctx, dockerPath, args...)
	cmd.Dir = inv.ProjectDir
	if len(vars) > 0 {
		env := append([]string{}, os.Environ()...)
		for k, v := range copyVars(vars) {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if names := unsetVarsInOutput(string(out)); len(names) > 0 {
		return nil, unsetVarError(attributeUnsetVars(inv, names, interpolationEnv(inv.ProjectDir, vars)))
	}
	if err != nil {
		return nil, composeConfigFailure(inv, string(out), err)
	}
	configJSON := extractJSON(string(out))
	if len(configJSON) == 0 {
		return nil, fmt.Errorf("compose config produced no JSON output")
	}
	return configJSON, nil
}

// composeConfigFailure builds the clear preflight error for a failed
// `docker compose config`, carrying the compose message (never rendered
// config) and tagging template-sourced failures.
func composeConfigFailure(inv Invocation, out string, err error) error {
	detail := strings.TrimSpace(out)
	if len(detail) > 600 {
		detail = detail[:600]
	}
	if inv.Template != "" {
		msg := fmt.Sprintf("template_compose_invalid: template %q with override=%v failed `docker compose config`", inv.Template, inv.HasOverride)
		if detail != "" {
			msg += ": " + detail
		}
		return syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, msg)
	}
	if detail != "" {
		return fmt.Errorf("compose config failed: %s: %w", detail, err)
	}
	return fmt.Errorf("compose config failed: %w", err)
}
