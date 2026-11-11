package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/fleetcheck"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/redact"
	"github.com/valthaide/tide/internal/syncerr"
)

// ApplyOptions controls image sync for one apply.
type ApplyOptions struct {
	// Vars are fleet variables passed to docker compose for ${NAME}.
	Vars map[string]string
	// BuildMode is allowed or forbidden. Empty inherits TIDE_BUILD.
	BuildMode string
	// NoBuild pulls and starts containers without docker build.
	// Health rollback sets this so a failed tag is restored without a build.
	NoBuild bool
	// Project is the compose project name. Empty derives one from the app id.
	Project string
	// RequireDigest rejects compose images that are not pinned with @sha256.
	RequireDigest bool
	// PullIdle cancels a pull that produces no output for this long.
	// Zero keeps the caller's deadline.
	PullIdle time.Duration
	// OneshotTimeout bounds one-shot services. Zero uses 10 minutes.
	OneshotTimeout time.Duration
	// SyncTimeout is the wall clock for up and one-shot wait. Zero uses the caller context.
	// Pull uses PullIdle instead, so a long pull with progress is not cut off here.
	SyncTimeout time.Duration
	// BindPrefixes, when non-empty, reject absolute bind mounts outside the list.
	BindPrefixes []string
	// AllowedRegistries, when non-empty, rejects images from other registries.
	AllowedRegistries []string
	// PolicyExceptions are honored only while TIDE_POLICY_MODE=warn.
	PolicyExceptions []deploypolicy.PolicyException
	// ForeignPorts maps a published host port to the app or container that owns it.
	ForeignPorts map[int]string
	// CreateVolumes creates a missing external volume when its name matches VolumeNamePattern.
	// Existing volumes are left unchanged. Default false.
	CreateVolumes bool
	// VolumeNamePattern is a regular expression. Empty refuses creation even when CreateVolumes is set.
	VolumeNamePattern string
	// OnVolumeCreated reports a volume this apply created.
	OnVolumeCreated func(name string)
	// Template selects templates/<Template>/compose.yaml from the fleet
	// checkout as the base compose file, merged with the app's optional
	// compose.override.yaml. Empty means the app's compose_path file is used.
	// Template and a compose_path must never be combined by callers.
	Template string
	// FleetCheckoutDir is the fleet checkout root holding templates/.
	// Empty falls back to the executor's configured checkout.
	FleetCheckoutDir string
}

// SlotGate limits how many image pulls run at once.
type SlotGate interface {
	Acquire(ctx context.Context) error
	Release()
}

// RunningImage is one compose service container that is actually running.
type RunningImage struct {
	Project string
	Service string
	Image   string
	Digest  string
}

type imagePrep struct {
	env        map[string]string
	cleanup    func()
	noBuild    bool
	images     []string
	rerun      []string
	oneshots   []string
	configJSON []byte
}

func (p imagePrep) clean() {
	if p.cleanup != nil {
		p.cleanup()
	}
}

var (
	unsetVarWarning = regexp.MustCompile(`The "([^"]+)" variable is not set`)
	manifestFor     = regexp.MustCompile(`manifest for (\S+)`)
	pullDeniedFor   = regexp.MustCompile(`pull access denied for ([^,:\s]+)`)
	resolveRef      = regexp.MustCompile(`failed to resolve reference "([^"]+)"`)
)

func resolveBuild(opts ApplyOptions) (mode string, noBuild bool) {
	mode = strings.ToLower(strings.TrimSpace(opts.BuildMode))
	if mode == "" {
		mode = strings.ToLower(envcompat.Getenv("TIDE_BUILD"))
	}
	if mode != deploypolicy.BuildForbidden {
		mode = deploypolicy.BuildAllowed
	}
	return mode, opts.NoBuild || mode == deploypolicy.BuildForbidden
}

// ServicesWithBuild returns service names whose compose config has a build key.
func ServicesWithBuild(configJSON []byte) ([]string, error) {
	var doc struct {
		Services map[string]struct {
			Build json.RawMessage `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &doc); err != nil {
		return nil, err
	}
	var names []string
	for name, svc := range doc.Services {
		raw := bytes.TrimSpace(svc.Build)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		names = append(names, name)
	}
	sortStrings(names)
	return names, nil
}

// OneshotServiceNames returns services other services wait for with
// service_completed_successfully.
func OneshotServiceNames(configJSON []byte) []string {
	var doc struct {
		Services map[string]struct {
			DependsOn json.RawMessage `json:"depends_on"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &doc); err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, svc := range doc.Services {
		if len(bytes.TrimSpace(svc.DependsOn)) == 0 {
			continue
		}
		var deps map[string]struct {
			Condition string `json:"condition"`
		}
		if err := json.Unmarshal(svc.DependsOn, &deps); err != nil {
			continue
		}
		for name, dep := range deps {
			if strings.TrimSpace(dep.Condition) == "service_completed_successfully" && strings.TrimSpace(name) != "" {
				seen[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func imagesFromConfig(configJSON []byte) []string {
	var doc struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &doc); err != nil {
		return nil
	}
	var images []string
	seen := map[string]struct{}{}
	for _, svc := range doc.Services {
		image := strings.TrimSpace(svc.Image)
		if image == "" {
			continue
		}
		if _, ok := seen[image]; ok {
			continue
		}
		seen[image] = struct{}{}
		images = append(images, image)
	}
	sortStrings(images)
	return images
}

func extractJSON(output string) []byte {
	startObj := strings.Index(output, "{")
	startArr := strings.Index(output, "[")
	start := startObj
	endMark := "}"
	if startArr >= 0 && (startObj < 0 || startArr < startObj) {
		start = startArr
		endMark = "]"
	}
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(output, endMark)
	if end < start {
		return nil
	}
	return []byte(output[start : end+1])
}

func unsetVarsInOutput(output string) []string {
	matches := unsetVarWarning.FindAllStringSubmatch(output, -1)
	seen := map[string]struct{}{}
	var names []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// PullFailedImage picks the image named by a pull error.
func PullFailedImage(output string, known []string) string {
	best := ""
	for _, image := range known {
		image = strings.TrimSpace(image)
		if image == "" || !strings.Contains(output, image) {
			continue
		}
		if len(image) > len(best) {
			best = image
		}
	}
	if best != "" {
		return best
	}
	for _, re := range []*regexp.Regexp{manifestFor, resolveRef, pullDeniedFor} {
		if m := re.FindStringSubmatch(output); len(m) == 2 {
			return strings.Trim(m[1], `"'`)
		}
	}
	return "unknown"
}

func pullFailedError(output string, images []string) error {
	ref := PullFailedImage(output, images)
	lower := strings.ToLower(output)
	kind := ""
	switch {
	case strings.Contains(lower, "digest mismatch") || strings.Contains(lower, "unexpected commit digest") || strings.Contains(lower, "content digest"):
		kind = "digest_mismatch"
	case strings.Contains(lower, "not found") || strings.Contains(lower, "manifest unknown") || strings.Contains(lower, "name unknown") || strings.Contains(lower, "no such manifest"):
		kind = "not_found"
	}
	if kind == "not_found" {
		return syncerr.New(syncerr.CodeImageNotFound, syncerr.StagePull, "image_not_found: "+ref)
	}
	msg := "pull_failed: " + ref
	if kind != "" {
		msg = "pull_failed: " + kind + " " + ref
	}
	return syncerr.New(syncerr.CodePullFailed, syncerr.StagePull, msg)
}

// classifyPullError reports a registry miss as pull_failed even when the idle
// timer has also fired. timeout is only a pull that produced neither a
// not_found nor a digest_mismatch.
func classifyPullError(cmdErr error, output string, idleFired bool, idle time.Duration, images []string) error {
	if cmdErr == nil {
		return nil
	}
	failed := pullFailedError(output+"\n"+cmdErr.Error(), images)
	if strings.Contains(failed.Error(), "not_found") || strings.Contains(failed.Error(), "digest_mismatch") {
		return failed
	}
	if idleFired {
		return syncerr.New(syncerr.CodeTimeout, syncerr.StagePull, "timeout: pull "+strings.Join(images, ",")+" after "+idle.String())
	}
	return failed
}

func unsetVarError(names []string) error {
	if len(names) == 0 {
		names = []string{"unknown"}
	}
	return syncerr.New(syncerr.CodeUnsetVar, syncerr.StageValidate, "unset_var: "+strings.Join(names, ","))
}

func buildForbiddenError(services []string) error {
	if len(services) == 0 {
		services = []string{"unknown"}
	}
	return syncerr.New(syncerr.CodeBuildForbidden, syncerr.StageValidate, "build_forbidden: "+strings.Join(services, ","))
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// prepareImageSync validates interpolation and build policy, then logs into
// the registry. It does not pull or start containers.
//
// This is the preflight for every deploy: the resolved invocation (fleet
// template + app override + Tide files, or the app's compose_path file) must
// render through `docker compose config` here, before any pull or `up`.
func (e *ComposeExecutor) prepareImageSync(
	ctx context.Context,
	syncLog *strings.Builder,
	projectName string,
	inv Invocation,
	overrideArgs []string,
	opts ApplyOptions,
	onProgress func(string),
) (imagePrep, error) {
	mode, noBuild := resolveBuild(opts)
	composeDir := inv.ProjectDir
	var combined strings.Builder
	for _, file := range inv.Files {
		data, err := os.ReadFile(file)
		if err != nil {
			return imagePrep{}, fmt.Errorf("read compose file: %w", err)
		}
		combined.Write(data)
		combined.WriteByte('\n')
	}
	env := interpolationEnv(composeDir, opts.Vars)
	if missing := MissingInterpolations(combined.String(), env); len(missing) > 0 {
		appendLogSection(syncLog, "Validation")
		appendLogLine(syncLog, "unset_var: "+strings.Join(missing, ","))
		return imagePrep{}, unsetVarError(missing)
	}

	appendLogSection(syncLog, "Compose config")
	configArgs := inv.Args(projectName, overrideArgs, "config", "--format", "json")
	configOut, configErr := e.runCommand(ctx, "docker", configArgs, composeDir, copyVars(opts.Vars), onProgress)
	if names := unsetVarsInOutput(configOut); len(names) > 0 {
		appendLogLine(syncLog, "unset_var: "+strings.Join(names, ","))
		return imagePrep{}, unsetVarError(names)
	}
	configJSON := extractJSON(configOut)
	if configErr != nil {
		appendLogLine(syncLog, "compose config failed")
		lower := strings.ToLower(configOut + " " + configErr.Error())
		if strings.Contains(lower, "variable") && (strings.Contains(lower, "not set") || strings.Contains(lower, "missing")) {
			return imagePrep{}, unsetVarError([]string{"unknown"})
		}
		if inv.Template != "" {
			detail := strings.TrimSpace(configOut)
			if len(detail) > 600 {
				detail = detail[:600]
			}
			msg := fmt.Sprintf("template_compose_invalid: template %q with override=%v failed `docker compose config`", inv.Template, inv.HasOverride)
			if detail != "" {
				msg += ": " + detail
			}
			appendLogLine(syncLog, msg)
			return imagePrep{}, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, msg)
		}
		return imagePrep{}, fmt.Errorf("compose config failed: %w", configErr)
	}
	images := imagesFromConfig(configJSON)
	sum, sumErr := fleetcheck.SummarizeCompose(configJSON)
	if sumErr != nil {
		return imagePrep{}, fmt.Errorf("compose config parse failed: %w", sumErr)
	}
	if opts.RequireDigest {
		if issues := fleetcheck.DigestRequired(sum.Images); len(issues) > 0 {
			appendLogLine(syncLog, issues[0].Message)
			return imagePrep{}, syncerr.New(syncerr.CodeDigestRequired, syncerr.StageValidate, issues[0].Message)
		}
	}
	if denied := fleetcheck.DeniedImages(sum.Images, opts.AllowedRegistries); len(denied) > 0 {
		msg := "registry_not_allowed: " + denied[0]
		appendLogLine(syncLog, msg)
		return imagePrep{}, syncerr.New(syncerr.CodeRegistryDenied, syncerr.StageValidate, msg)
	}
	if hit, ok := policyViolation(sum, opts.PolicyExceptions); ok {
		msg := "policy_violation: " + hit.Service + " " + hit.Rule
		appendLogLine(syncLog, msg)
		metrics.SetPolicyViolation(projectName, hit.Rule, 1)
		if policyMode() == "enforce" {
			return imagePrep{}, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, msg)
		}
		if policyMode() == "warn" && e.Logger != nil {
			e.Logger.Warn("compose policy", "project", projectName, "service", hit.Service, "rule", hit.Rule)
		}
	}
	if issues := fleetcheck.BindViolations(sum.BindSources, opts.BindPrefixes); len(issues) > 0 {
		appendLogLine(syncLog, issues[0].Message)
		return imagePrep{}, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issues[0].Message)
	}
	if err := e.claimPorts(projectName, sum.Published, opts.ForeignPorts); err != nil {
		appendLogLine(syncLog, err.Error())
		return imagePrep{}, err
	}
	for _, vol := range sum.ExternalVols {
		if err := e.ensureExternalVolume(ctx, syncLog, vol, opts); err != nil {
			return imagePrep{}, err
		}
	}
	if len(sum.ContainerName) > 0 {
		switch policyMode() {
		case "enforce":
			msg := "policy_violation: container_name " + strings.Join(sum.ContainerName, ",")
			appendLogLine(syncLog, msg)
			metrics.SetPolicyViolation(projectName, "container_name", 1)
			return imagePrep{}, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, msg)
		case "warn":
			metrics.SetPolicyViolation(projectName, "container_name", 1)
			if e.Logger != nil {
				e.Logger.Warn("compose sets container_name", "project", projectName, "services", strings.Join(sum.ContainerName, ","))
			}
		default:
			metrics.SetPolicyViolation(projectName, "container_name", 0)
		}
	}
	e.rememberDigests(projectName, sum.Images)
	if mode == deploypolicy.BuildForbidden && !opts.NoBuild {
		services, parseErr := ServicesWithBuild(configJSON)
		if parseErr != nil {
			return imagePrep{}, fmt.Errorf("compose config parse failed: %w", parseErr)
		}
		if len(services) > 0 {
			reason := "build_forbidden: " + strings.Join(services, ",")
			appendLogLine(syncLog, reason)
			return imagePrep{}, buildForbiddenError(services)
		}
	}
	appendLogLine(syncLog, "ok")

	prep := imagePrep{env: copyVars(opts.Vars), noBuild: noBuild, images: images, rerun: sum.Rerun, oneshots: OneshotServiceNames(configJSON), configJSON: configJSON, cleanup: func() {}}
	creds, configured, credErr := RegistryCredsFromEnv()
	if credErr != nil || (configured && (creds.Host == "" || creds.Username == "" || creds.Password == "")) {
		appendLogSection(syncLog, "Registry login")
		appendLogLine(syncLog, "registry_auth")
		return imagePrep{}, syncerr.New(syncerr.CodeRegistryAuth, syncerr.StageRegistry, "registry_auth")
	}
	if !configured {
		return prep, nil
	}
	dir, err := os.MkdirTemp("", "tide-docker-config-")
	if err != nil {
		return imagePrep{}, fmt.Errorf("registry config dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	prep.cleanup = func() { _ = os.RemoveAll(dir) }
	if prep.env == nil {
		prep.env = map[string]string{}
	}
	prep.env["DOCKER_CONFIG"] = dir

	resolution, resErr := e.resolveDockerCommand(ctx)
	if resErr != nil {
		prep.clean()
		return imagePrep{}, syncerr.New(syncerr.CodeRegistryAuth, syncerr.StageRegistry, "registry_auth")
	}
	appendLogSection(syncLog, "Registry login")
	appendLogLine(syncLog, "host: "+creds.Host)
	if e.Logger != nil {
		e.Logger.Info("Registry login", "host", creds.Host, "user", creds.Username)
	}
	if err := dockerLogin(ctx, resolution.Path, creds, dir); err != nil {
		appendLogLine(syncLog, "registry_auth")
		prep.clean()
		return imagePrep{}, syncerr.New(syncerr.CodeRegistryAuth, syncerr.StageRegistry, "registry_auth")
	}
	appendLogLine(syncLog, "ok")
	return prep, nil
}

func copyVars(vars map[string]string) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dockerLogin authenticates for the duration of compose pull.
// The password is passed on stdin and is not written as plaintext.
func dockerLogin(ctx context.Context, bin string, creds RegistryCreds, configDir string) error {
	if strings.TrimSpace(creds.Password) != "" {
		redact.Default.AddSecret(creds.Password)
	}
	cmd := exec.CommandContext(ctx, bin, "login", creds.Host, "-u", creds.Username, "--password-stdin")
	cmd.Stdin = strings.NewReader(creds.Password)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := redact.String(string(out))
		msg = strings.ReplaceAll(msg, creds.Password, "***")
		return fmt.Errorf("registry login failed: %s", strings.TrimSpace(msg))
	}
	return nil
}

// RunningImages lists image and digest for running compose containers.
// The list comes from docker ps. A digest inspect failure still returns the images.
func (e *ComposeExecutor) RunningImages(ctx context.Context) ([]RunningImage, error) {
	obs, err := e.observeContainers(ctx)
	if err != nil {
		return nil, err
	}
	var images []RunningImage
	for _, c := range obs {
		if c.Project == "" || c.Service == "" || c.OneOff || !c.Running {
			continue
		}
		images = append(images, RunningImage{
			Project: c.Project,
			Service: c.Service,
			Image:   c.Image,
			Digest:  digestLabel(c.RepoDigest, c.ImageID),
		})
	}
	return images, nil
}

func digestLabel(repoDigest, imageID string) string {
	repoDigest = strings.TrimSpace(repoDigest)
	if i := strings.LastIndex(repoDigest, "@"); i >= 0 && i+1 < len(repoDigest) {
		return repoDigest[i+1:]
	}
	imageID = strings.TrimSpace(imageID)
	if imageID == "" {
		return "unknown"
	}
	return imageID
}

func policyViolation(sum fleetcheck.Summary, exceptions []deploypolicy.PolicyException) (fleetcheck.PolicyHit, bool) {
	mode := policyMode()
	if mode == "off" {
		return fleetcheck.PolicyHit{}, false
	}
	var allowed []fleetcheck.PolicyException
	if mode == "warn" {
		for _, item := range exceptions {
			allowed = append(allowed, fleetcheck.PolicyException{Service: item.Service, Rule: item.Rule})
		}
	}
	for _, hit := range fleetcheck.SecurityHits(sum) {
		if mode == "warn" && fleetcheck.Excepted(hit, allowed) {
			continue
		}
		return hit, true
	}
	return fleetcheck.PolicyHit{}, false
}
