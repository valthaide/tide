package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/valthaide/conops-private/internal/syncerr"
)

const (
	dockerResolutionTTL          = 5 * time.Minute
	dockerVersionProbeFormatJSON = "{{json .}}"
)

type dockerCommandResolution struct {
	Path                string
	Env                 map[string]string
	Source              string
	ClientVersion       string
	ClientAPIVersion    string
	DaemonVersion       string
	DaemonAPIVersion    string
	DaemonMinAPIVersion string
	ComposeVersion      string
}

type dockerPreflightReport struct {
	Resolution dockerCommandResolution
}

func (r dockerPreflightReport) LogLines() []string {
	res := r.Resolution
	lines := []string{
		fmt.Sprintf("docker_command: %s", res.Path),
		fmt.Sprintf("docker_source: %s", res.Source),
		fmt.Sprintf("docker_client_version: %s", fallbackValue(res.ClientVersion)),
		fmt.Sprintf("docker_client_api: %s", fallbackValue(res.ClientAPIVersion)),
		fmt.Sprintf("docker_daemon_version: %s", fallbackValue(res.DaemonVersion)),
		fmt.Sprintf("docker_daemon_api: %s", fallbackValue(res.DaemonAPIVersion)),
		fmt.Sprintf("docker_daemon_min_api: %s", fallbackValue(res.DaemonMinAPIVersion)),
		fmt.Sprintf("docker_compose_version: %s", fallbackValue(res.ComposeVersion)),
	}
	if value := strings.TrimSpace(res.Env["DOCKER_CONFIG"]); value != "" {
		lines = append(lines, fmt.Sprintf("docker_config: %s", value))
	}
	return lines
}

type dockerVersionProbe struct {
	ClientVersion       string
	ClientAPIVersion    string
	ServerVersion       string
	ServerAPIVersion    string
	ServerMinAPIVersion string
	RawOutput           string
}

type dockerVersionJSON struct {
	Client dockerVersionSection `json:"Client"`
	Server dockerVersionSection `json:"Server"`
}

type dockerVersionSection struct {
	Version             string `json:"Version"`
	APIVersion          string `json:"ApiVersion"`
	APIVersionLegacy    string `json:"APIVersion"`
	MinAPIVersion       string `json:"MinAPIVersion"`
	MinAPIVersionLegacy string `json:"MinAPIVERSION"`
}

func (e *ComposeExecutor) ensureDockerPreflight(ctx context.Context) (dockerPreflightReport, error) {
	resolution, err := e.resolveDockerCommand(ctx)
	if err != nil {
		return dockerPreflightReport{}, err
	}
	return dockerPreflightReport{Resolution: resolution}, nil
}

func (e *ComposeExecutor) resolveDockerCommand(ctx context.Context) (dockerCommandResolution, error) {
	e.toolchainMu.Lock()
	defer e.toolchainMu.Unlock()

	if e.dockerResolution.Path != "" && time.Since(e.resolutionAt) < dockerResolutionTTL {
		return e.dockerResolution.clone(), nil
	}

	resolution, err := e.buildDockerResolution(ctx)
	if err != nil {
		return dockerCommandResolution{}, err
	}

	e.dockerResolution = resolution.clone()
	e.resolutionAt = time.Now()
	return resolution, nil
}

func (e *ComposeExecutor) buildDockerResolution(ctx context.Context) (dockerCommandResolution, error) {
	dockerPath := "docker"
	source := "system"
	if value := strings.TrimSpace(os.Getenv("CONOPS_DOCKER_CLI_PATH")); value != "" {
		if _, err := os.Stat(value); err != nil {
			return dockerCommandResolution{}, fmt.Errorf("CONOPS_DOCKER_CLI_PATH does not exist: %w", err)
		}
		dockerPath = value
		source = "custom"
	}

	probe, err := probeDockerVersion(ctx, dockerPath, nil)
	if err != nil {
		return dockerCommandResolution{}, fmt.Errorf("docker probe failed: %w", err)
	}
	compatible, compatErr := dockerAPICompatible(probe.ClientAPIVersion, probe.ServerMinAPIVersion)
	if compatErr != nil {
		return dockerCommandResolution{}, fmt.Errorf("docker api compatibility check failed: %w", compatErr)
	}
	if !compatible {
		return dockerCommandResolution{}, dockerAPIIncompatibleError(probe.ClientAPIVersion, probe.ServerMinAPIVersion)
	}
	composeVersion, err := probeComposeVersion(ctx, dockerPath, nil)
	if err != nil {
		return dockerCommandResolution{}, fmt.Errorf("docker compose plugin unavailable: %w", err)
	}
	if daemonErr := probeComposeDaemonCompatibility(ctx, dockerPath, nil); daemonErr != nil {
		if looksLikeDockerAPIMismatch(daemonErr) {
			return dockerCommandResolution{}, dockerAPIIncompatibleError(probe.ClientAPIVersion, probe.ServerMinAPIVersion)
		}
		return dockerCommandResolution{}, fmt.Errorf("docker compose daemon compatibility check failed: %w", daemonErr)
	}
	return dockerCommandResolution{
		Path:                dockerPath,
		Source:              source,
		ClientVersion:       probe.ClientVersion,
		ClientAPIVersion:    probe.ClientAPIVersion,
		DaemonVersion:       probe.ServerVersion,
		DaemonAPIVersion:    probe.ServerAPIVersion,
		DaemonMinAPIVersion: probe.ServerMinAPIVersion,
		ComposeVersion:      composeVersion,
	}, nil
}

func probeDockerVersion(ctx context.Context, dockerPath string, envVars map[string]string) (dockerVersionProbe, error) {
	output, err := runCommandCapture(ctx, dockerPath, []string{"version", "--format", dockerVersionProbeFormatJSON}, envVars)
	if err != nil {
		return dockerVersionProbe{RawOutput: output}, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}

	parsed := dockerVersionJSON{}
	if decodeErr := json.Unmarshal([]byte(strings.TrimSpace(output)), &parsed); decodeErr != nil {
		return dockerVersionProbe{RawOutput: output}, fmt.Errorf("parse docker version output failed: %w", decodeErr)
	}

	clientAPIVersion := strings.TrimSpace(parsed.Client.APIVersion)
	if clientAPIVersion == "" {
		clientAPIVersion = strings.TrimSpace(parsed.Client.APIVersionLegacy)
	}
	serverMinAPIVersion := strings.TrimSpace(parsed.Server.MinAPIVersion)
	if serverMinAPIVersion == "" {
		serverMinAPIVersion = strings.TrimSpace(parsed.Server.MinAPIVersionLegacy)
	}

	return dockerVersionProbe{
		ClientVersion:       strings.TrimSpace(parsed.Client.Version),
		ClientAPIVersion:    clientAPIVersion,
		ServerVersion:       strings.TrimSpace(parsed.Server.Version),
		ServerAPIVersion:    strings.TrimSpace(parsed.Server.APIVersion),
		ServerMinAPIVersion: serverMinAPIVersion,
		RawOutput:           output,
	}, nil
}

func probeComposeVersion(ctx context.Context, dockerPath string, envVars map[string]string) (string, error) {
	output, err := runCommandCapture(ctx, dockerPath, []string{"compose", "version", "--short"}, envVars)
	if err == nil {
		version := strings.TrimSpace(output)
		if version != "" {
			return version, nil
		}
	}

	output, err = runCommandCapture(ctx, dockerPath, []string{"compose", "version"}, envVars)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	line := strings.TrimSpace(output)
	if line == "" {
		return "", fmt.Errorf("docker compose version returned empty output")
	}
	parts := strings.Fields(line)
	if len(parts) > 0 {
		return parts[len(parts)-1], nil
	}
	return line, nil
}

func probeComposeDaemonCompatibility(ctx context.Context, dockerPath string, envVars map[string]string) error {
	output, err := runCommandCapture(ctx, dockerPath, []string{"compose", "ls", "--all"}, envVars)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	// Some compose versions exit 0 but print API mismatch warnings to
	// combined output.
	if outputLooksLikeDockerAPIMismatch(output) {
		return fmt.Errorf("compose plugin reported API mismatch: %s", strings.TrimSpace(output))
	}
	return nil
}

func runCommandCapture(ctx context.Context, cmd string, args []string, envVars map[string]string) (string, error) {
	command := exec.CommandContext(ctx, cmd, args...)
	if len(envVars) > 0 {
		merged := append([]string{}, os.Environ()...)
		for key, value := range envVars {
			merged = append(merged, fmt.Sprintf("%s=%s", key, value))
		}
		command.Env = merged
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func dockerAPIIncompatibleError(clientAPI, minServerAPI string) error {
	clientAPI = fallbackValue(clientAPI)
	minServerAPI = fallbackValue(minServerAPI)
	return syncerr.New(
		syncerr.CodeDockerAPI,
		syncerr.StageCompose,
		fmt.Sprintf("docker_api_incompatible: client %s is below daemon minimum %s", clientAPI, minServerAPI),
	)
}

func dockerAPICompatible(clientAPI, minServerAPI string) (bool, error) {
	clientAPI = strings.TrimSpace(clientAPI)
	minServerAPI = strings.TrimSpace(minServerAPI)
	if minServerAPI == "" {
		return true, nil
	}
	if clientAPI == "" {
		return false, fmt.Errorf("client API version is empty while daemon minimum API is %s", minServerAPI)
	}
	comparison, err := compareNumericVersions(clientAPI, minServerAPI)
	if err != nil {
		return false, err
	}
	return comparison >= 0, nil
}

func compareNumericVersions(a, b string) (int, error) {
	aParts, err := parseNumericVersionParts(a)
	if err != nil {
		return 0, fmt.Errorf("invalid version %q: %w", a, err)
	}
	bParts, err := parseNumericVersionParts(b)
	if err != nil {
		return 0, fmt.Errorf("invalid version %q: %w", b, err)
	}

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}
	for i := 0; i < maxLen; i++ {
		aValue := 0
		if i < len(aParts) {
			aValue = aParts[i]
		}
		bValue := 0
		if i < len(bParts) {
			bValue = bParts[i]
		}
		if aValue < bValue {
			return -1, nil
		}
		if aValue > bValue {
			return 1, nil
		}
	}
	return 0, nil
}

func parseNumericVersionParts(value string) ([]int, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return nil, fmt.Errorf("empty value")
	}

	parts := strings.Split(trimmed, ".")
	parsed := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty version segment")
		}
		if idx := strings.IndexByte(part, '-'); idx >= 0 {
			part = part[:idx]
		}
		if part == "" {
			return nil, fmt.Errorf("invalid version segment")
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, number)
	}
	return parsed, nil
}

func (r dockerCommandResolution) clone() dockerCommandResolution {
	copyEnv := map[string]string(nil)
	if len(r.Env) > 0 {
		copyEnv = make(map[string]string, len(r.Env))
		for key, value := range r.Env {
			copyEnv[key] = value
		}
	}
	r.Env = copyEnv
	return r
}

func fallbackValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func looksLikeDockerAPIMismatch(err error) bool {
	if err == nil {
		return false
	}
	return outputLooksLikeDockerAPIMismatch(err.Error())
}

func outputLooksLikeDockerAPIMismatch(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "client version") &&
		strings.Contains(lower, "minimum supported api version")
}

func mergeCommandEnv(base, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range extra {
		merged[key] = value
	}
	for key, value := range base {
		merged[key] = value
	}
	return merged
}
