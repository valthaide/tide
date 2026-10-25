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

	"github.com/valthaide/conops-private/internal/deploypolicy"
	"github.com/valthaide/conops-private/internal/redact"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// ApplyOptions controls image sync for one apply.
type ApplyOptions struct {
	// Vars are fleet variables passed to docker compose for ${NAME}.
	Vars map[string]string
	// BuildMode is allowed or forbidden. Empty inherits CONOPS_BUILD.
	BuildMode string
	// NoBuild pulls and starts containers without docker build.
	// Health rollback sets this so a failed tag is restored without a build.
	NoBuild bool
	// Project is the compose project name. Empty derives one from the app id.
	Project string
}

// RunningImage is one compose service container that is actually running.
type RunningImage struct {
	Project string
	Service string
	Image   string
	Digest  string
}

type imagePrep struct {
	env     map[string]string
	cleanup func()
	noBuild bool
	images  []string
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
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("CONOPS_BUILD")))
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
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
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
	return syncerr.New(syncerr.CodePullFailed, syncerr.StagePull, "pull_failed: "+PullFailedImage(output, images))
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
func (e *ComposeExecutor) prepareImageSync(
	ctx context.Context,
	syncLog *strings.Builder,
	projectName, composeFileName, composeDir, composeFullPath string,
	overrideArgs []string,
	opts ApplyOptions,
	onProgress func(string),
) (imagePrep, error) {
	mode, noBuild := resolveBuild(opts)
	data, err := os.ReadFile(composeFullPath)
	if err != nil {
		return imagePrep{}, fmt.Errorf("read compose file: %w", err)
	}
	env := interpolationEnv(composeDir, opts.Vars)
	if missing := MissingInterpolations(string(data), env); len(missing) > 0 {
		appendLogSection(syncLog, "Validation")
		appendLogLine(syncLog, "unset_var: "+strings.Join(missing, ","))
		return imagePrep{}, unsetVarError(missing)
	}

	appendLogSection(syncLog, "Compose config")
	configArgs := []string{"compose", "-p", projectName, "-f", composeFileName}
	if len(overrideArgs) > 0 {
		configArgs = append(configArgs, overrideArgs...)
	}
	configArgs = append(configArgs, "config", "--format", "json")
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
		return imagePrep{}, fmt.Errorf("compose config failed: %w", configErr)
	}
	images := imagesFromConfig(configJSON)
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

	prep := imagePrep{env: copyVars(opts.Vars), noBuild: noBuild, images: images, cleanup: func() {}}
	creds, configured, credErr := RegistryCredsFromEnv()
	if credErr != nil || (configured && (creds.Host == "" || creds.Username == "" || creds.Password == "")) {
		appendLogSection(syncLog, "Registry login")
		appendLogLine(syncLog, "registry_auth")
		return imagePrep{}, syncerr.New(syncerr.CodeRegistryAuth, syncerr.StageRegistry, "registry_auth")
	}
	if !configured {
		return prep, nil
	}
	dir, err := os.MkdirTemp("", "conops-docker-config-")
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
