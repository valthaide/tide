package compose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// MigrationMode classifies whether a source migration restarts containers.
const (
	MigrationModeNoRestart = "no_restart"
	MigrationModeRestarted = "restarted"
	MigrationModeFailed    = "failed"

	MigrationStrategyAdoptNoRestart = "adopt_no_restart"
	MigrationStrategyComposeUp      = "compose_up"
	MigrationStrategyDownUp         = "down_up"
)

// CanonicalProjectSpec holds normalized desired or running state for a Compose project.
type CanonicalProjectSpec struct {
	Project  string                          `json:"project"`
	Services map[string]CanonicalServiceSpec `json:"services"`
}

// CanonicalServiceSpec holds the normalized configuration of one Compose service.
type CanonicalServiceSpec struct {
	Image       string            `json:"image"`
	Digest      string            `json:"digest"`
	HasBuild    bool              `json:"has_build,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Entrypoint  []string          `json:"entrypoint,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Ports       []CanonicalPort   `json:"ports,omitempty"`
	Volumes     []CanonicalMount  `json:"volumes,omitempty"`
	Networks    []string          `json:"networks,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Oneshot     bool              `json:"oneshot,omitempty"`
}

// CanonicalPort represents one published container port binding.
type CanonicalPort struct {
	HostIP    string `json:"host_ip,omitempty"`
	Published int    `json:"published"`
	Target    int    `json:"target"`
	Protocol  string `json:"protocol"`
}

// CanonicalMount represents one container volume, bind, or tmpfs mount.
type CanonicalMount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	ReadOnly    bool   `json:"read_only,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// MigrationDiffResult describes whether rendered Compose matches the currently running project.
type MigrationDiffResult struct {
	Equivalent        bool
	Mode              string
	Strategy          string
	Reason            string
	ChangedServices   []string
	RemovedServices   []string
	UnchangedServices []string
	DiffSummary       []string
}

type renderedComposeDoc struct {
	Name     string                           `json:"name"`
	Services map[string]renderedServiceConfig `json:"services"`
	Volumes  map[string]renderedNamedResource `json:"volumes"`
	Networks map[string]renderedNamedResource `json:"networks"`
}

type renderedNamedResource struct {
	Name     string `json:"name"`
	External any    `json:"external"`
}

type renderedServiceConfig struct {
	Image       string           `json:"image"`
	Build       json.RawMessage  `json:"build"`
	Command     any              `json:"command"`
	Entrypoint  any              `json:"entrypoint"`
	Environment any              `json:"environment"`
	Ports       []renderedPort   `json:"ports"`
	Volumes     []renderedVolume `json:"volumes"`
	Networks    any              `json:"networks"`
	NetworkMode string           `json:"network_mode"`
	Labels      any              `json:"labels"`
	DependsOn   json.RawMessage  `json:"depends_on"`
}

type renderedPort struct {
	HostIP    string `json:"host_ip"`
	Published any    `json:"published"`
	Target    any    `json:"target"`
	Protocol  string `json:"protocol"`
}

type renderedVolume struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type fullContainerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image      string            `json:"Image"`
		Cmd        []string          `json:"Cmd"`
		Entrypoint []string          `json:"Entrypoint"`
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode  string                         `json:"NetworkMode"`
		PortBindings map[string][]dockerPortBinding `json:"PortBindings"`
		Mounts       []dockerHostMount              `json:"Mounts"`
	} `json:"HostConfig"`
	Mounts          []dockerRuntimeMount `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			NetworkID string   `json:"NetworkID"`
			Aliases   []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerHostMount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type dockerRuntimeMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type fullImageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	RepoTags    []string `json:"RepoTags"`
	Config      struct {
		Cmd        []string          `json:"Cmd"`
		Entrypoint []string          `json:"Entrypoint"`
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
}

type dockerNetworkInspect struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// ParseRenderedComposeSpec normalizes `docker compose config --format json` output for equivalence comparison.
//
// Compose's `$$` escaping is undone first, so every value is the one a container
// created from this config would hold. Nothing is interpolated again.
//
// resolveLocalDigest is asked for the digest of an image that is not pinned with
// @sha256. It may return bare 64 hex, `sha256:<hex>` or `repo@sha256:<hex>`,
// or "" when the image is not available locally.
func ParseRenderedComposeSpec(
	configJSON []byte,
	projectName string,
	stagingRepoDir string,
	targetAppRepoDir string,
	resolveLocalDigest func(imageRef string) string,
) (CanonicalProjectSpec, error) {
	var doc renderedComposeDoc
	if err := json.Unmarshal(unescapeComposeDollars(configJSON), &doc); err != nil {
		return CanonicalProjectSpec{}, fmt.Errorf("parse compose config json: %w", err)
	}
	if strings.TrimSpace(projectName) == "" {
		projectName = strings.TrimSpace(doc.Name)
	}
	oneshots := map[string]bool{}
	for _, name := range OneshotServiceNames(configJSON) {
		oneshots[name] = true
	}

	volActualName := map[string]string{}
	for key, vol := range doc.Volumes {
		actual := strings.TrimSpace(vol.Name)
		if actual == "" {
			if isTruthyExternal(vol.External) {
				actual = key
			} else {
				actual = projectName + "_" + key
			}
		}
		volActualName[key] = actual
	}

	netActualName := map[string]string{}
	for key, netCfg := range doc.Networks {
		actual := strings.TrimSpace(netCfg.Name)
		if actual == "" {
			if isTruthyExternal(netCfg.External) {
				actual = key
			} else {
				actual = projectName + "_" + key
			}
		}
		netActualName[key] = actual
	}

	out := CanonicalProjectSpec{
		Project:  projectName,
		Services: make(map[string]CanonicalServiceSpec, len(doc.Services)),
	}

	stagingAbs, _ := filepath.Abs(stagingRepoDir)
	targetAbs, _ := filepath.Abs(targetAppRepoDir)

	for svcName, svc := range doc.Services {
		img := strings.TrimSpace(svc.Image)
		digest := extractDigestHex(img)
		if digest == "" && img != "" && resolveLocalDigest != nil {
			digest = normalizeDigestHex(resolveLocalDigest(img))
		}

		rawBuild := strings.TrimSpace(string(svc.Build))
		hasBuild := rawBuild != "" && rawBuild != "null"

		spec := CanonicalServiceSpec{
			Image:       normalizeImageRepoRef(img),
			Digest:      digest,
			HasBuild:    hasBuild,
			Command:     parseStringOrSlice(svc.Command),
			Entrypoint:  parseStringOrSlice(svc.Entrypoint),
			Environment: parseKeyValueMap(svc.Environment),
			Ports:       canonicalizeRenderedPorts(svc.Ports),
			Volumes:     canonicalizeRenderedMounts(svc.Volumes, volActualName, stagingAbs, targetAbs),
			Networks:    canonicalizeRenderedNetworks(svc.Networks, netActualName, projectName, svc.NetworkMode),
			NetworkMode: normalizeNetworkMode(svc.NetworkMode),
			Labels:      filterUserLabels(parseKeyValueMap(svc.Labels)),
			Oneshot:     oneshots[svcName],
		}
		out.Services[svcName] = spec
	}

	return out, nil
}

// InspectRunningProjectSpec inspects live Docker containers, images, and networks of a Compose project.
func (e *ComposeExecutor) InspectRunningProjectSpec(ctx context.Context, projectName, appRepoDir string) (CanonicalProjectSpec, bool, error) {
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return CanonicalProjectSpec{}, false, nil
	}

	containers, images, netLabels, err := e.fetchLiveProjectObjects(ctx, projectName)
	if err != nil {
		return CanonicalProjectSpec{}, false, err
	}
	return BuildRunningProjectSpec(projectName, appRepoDir, containers, images, netLabels)
}

// BuildRunningProjectSpec converts inspected Docker containers and images into a CanonicalProjectSpec.
// allHealthy reports whether every non-oneshot container is running and healthy (and oneshots exited 0).
func BuildRunningProjectSpec(
	projectName string,
	appRepoDir string,
	containers []fullContainerInspect,
	images map[string]fullImageInspect,
	networkProjectNames map[string]string,
) (CanonicalProjectSpec, bool, error) {
	out := CanonicalProjectSpec{
		Project:  projectName,
		Services: map[string]CanonicalServiceSpec{},
	}
	if len(containers) == 0 {
		return out, false, nil
	}

	oneshots := map[string]bool{}
	for _, c := range containers {
		if c.Config.Labels != nil {
			for _, dep := range servicesCompletedSuccessfully(c.Config.Labels["com.docker.compose.depends_on"]) {
				oneshots[dep] = true
			}
		}
	}

	allHealthy := true
	serviceCount := map[string]int{}
	appRepoAbs, _ := filepath.Abs(appRepoDir)

	for _, c := range containers {
		labels := c.Config.Labels
		if strings.EqualFold(strings.TrimSpace(labels["com.docker.compose.oneoff"]), "true") ||
			strings.TrimSpace(labels["com.docker.compose.oneoff"]) == "1" {
			continue
		}
		svcName := strings.TrimSpace(labels["com.docker.compose.service"])
		if svcName == "" {
			continue
		}
		serviceCount[svcName]++
		if serviceCount[svcName] > 1 {
			// Scaled replicas: still inspectable, but ensure consistent check; if replicas > 1, compose config lacks deploy.replicas here so mark unhealthy for no-restart unless single container.
			allHealthy = false
		}

		isOneshot := oneshots[svcName]
		if isOneshot {
			if c.State.Running || c.State.ExitCode != 0 {
				allHealthy = false
			}
		} else {
			if !c.State.Running || strings.ToLower(strings.TrimSpace(c.State.Status)) != "running" {
				allHealthy = false
			}
			if c.State.Health != nil {
				hs := strings.ToLower(strings.TrimSpace(c.State.Health.Status))
				if hs != "" && hs != "healthy" {
					allHealthy = false
				}
			}
		}

		imgInfo, hasImg := lookupImageInspect(c, images)
		if !hasImg {
			allHealthy = false
		}

		cfgImage := strings.TrimSpace(c.Config.Image)
		digest := extractDigestHex(cfgImage)
		if digest == "" && hasImg {
			digest = imageInspectDigest(imgInfo)
		}

		spec := CanonicalServiceSpec{
			Image:       normalizeImageRepoRef(cfgImage),
			Digest:      digest,
			Command:     sliceOrNil(c.Config.Cmd),
			Entrypoint:  sliceOrNil(c.Config.Entrypoint),
			Environment: envSliceToMap(c.Config.Env),
			Ports:       canonicalizeInspectedPorts(c.HostConfig.PortBindings),
			Volumes:     canonicalizeInspectedMounts(c.Mounts, c.HostConfig.Mounts, appRepoAbs),
			Networks:    canonicalizeInspectedNetworks(c.NetworkSettings.Networks, c.HostConfig.NetworkMode, projectName, networkProjectNames),
			NetworkMode: normalizeContainerNetworkMode(c.HostConfig.NetworkMode, projectName, networkProjectNames),
			Labels:      filterUserLabels(labels),
			Oneshot:     isOneshot,
		}
		out.Services[svcName] = spec
	}

	if len(out.Services) == 0 {
		return out, false, nil
	}
	return out, allHealthy, nil
}

// CompareMigrationSpecs compares desired rendered Compose spec against the live running project spec.
// Image defaults from `desiredImages` (keyed by image ref or digest) are merged into desired service specs
// so omitted command/entrypoint/env/labels match what Docker actually materializes on the container.
func CompareMigrationSpecs(
	desired CanonicalProjectSpec,
	running CanonicalProjectSpec,
	runningHealthy bool,
	desiredImages map[string]fullImageInspect,
) MigrationDiffResult {
	if len(running.Services) == 0 {
		var svcs []string
		for name := range desired.Services {
			svcs = append(svcs, name)
		}
		sort.Strings(svcs)
		return MigrationDiffResult{
			Equivalent:      false,
			Mode:            MigrationModeRestarted,
			Strategy:        MigrationStrategyComposeUp,
			Reason:          "runtime_not_running",
			ChangedServices: svcs,
			DiffSummary:     []string{"no running containers found for project"},
		}
	}
	if !runningHealthy {
		var svcs []string
		for name := range desired.Services {
			svcs = append(svcs, name)
		}
		sort.Strings(svcs)
		return MigrationDiffResult{
			Equivalent:      false,
			Mode:            MigrationModeRestarted,
			Strategy:        MigrationStrategyComposeUp,
			Reason:          "runtime_unhealthy",
			ChangedServices: svcs,
			DiffSummary:     []string{"existing containers are not all running and healthy"},
		}
	}

	var changed []string
	var removed []string
	var unchanged []string
	var details []string
	reasonSet := map[string]bool{}

	allNames := map[string]struct{}{}
	for name := range desired.Services {
		allNames[name] = struct{}{}
	}
	for name := range running.Services {
		allNames[name] = struct{}{}
	}
	sortedNames := make([]string, 0, len(allNames))
	for name := range allNames {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)

	for _, name := range sortedNames {
		des, inDesired := desired.Services[name]
		run, inRunning := running.Services[name]
		if !inDesired {
			removed = append(removed, name)
			reasonSet["service_removed"] = true
			details = append(details, fmt.Sprintf("service %s: removed", name))
			continue
		}
		if !inRunning {
			changed = append(changed, name)
			reasonSet["service_added"] = true
			details = append(details, fmt.Sprintf("service %s: added", name))
			continue
		}

		effectiveDes := mergeServiceWithImageDefaults(des, desiredImages)
		svcReasons, svcDetails := diffSingleService(name, effectiveDes, run)
		if len(svcReasons) == 0 {
			unchanged = append(unchanged, name)
			continue
		}
		changed = append(changed, name)
		for _, r := range svcReasons {
			reasonSet[r] = true
		}
		details = append(details, svcDetails...)
	}

	if len(changed) == 0 && len(removed) == 0 {
		return MigrationDiffResult{
			Equivalent:        true,
			Mode:              MigrationModeNoRestart,
			Strategy:          MigrationStrategyAdoptNoRestart,
			Reason:            "config_identical",
			UnchangedServices: unchanged,
		}
	}

	primaryReason := pickPrimaryDiffReason(reasonSet)
	return MigrationDiffResult{
		Equivalent:        false,
		Mode:              MigrationModeRestarted,
		Strategy:          MigrationStrategyComposeUp,
		Reason:            primaryReason,
		ChangedServices:   changed,
		RemovedServices:   removed,
		UnchangedServices: unchanged,
		DiffSummary:       details,
	}
}

func mergeServiceWithImageDefaults(des CanonicalServiceSpec, images map[string]fullImageInspect) CanonicalServiceSpec {
	out := des
	img, ok := findImageDefaults(des, images)
	if !ok {
		return out
	}

	// Entrypoint & Command inheritance rules in Docker:
	// If compose sets entrypoint, image Cmd is cleared unless compose also sets command.
	if len(out.Entrypoint) == 0 {
		out.Entrypoint = sliceOrNil(img.Config.Entrypoint)
		if len(out.Command) == 0 {
			out.Command = sliceOrNil(img.Config.Cmd)
		}
	}

	mergedEnv := envSliceToMap(img.Config.Env)
	if mergedEnv == nil && len(out.Environment) > 0 {
		mergedEnv = map[string]string{}
	}
	for k, v := range out.Environment {
		mergedEnv[k] = v
	}
	out.Environment = mergedEnv

	mergedLabels := filterUserLabels(img.Config.Labels)
	if mergedLabels == nil && len(out.Labels) > 0 {
		mergedLabels = map[string]string{}
	}
	for k, v := range out.Labels {
		mergedLabels[k] = v
	}
	out.Labels = mergedLabels

	return out
}

func findImageDefaults(des CanonicalServiceSpec, images map[string]fullImageInspect) (fullImageInspect, bool) {
	if len(images) == 0 {
		return fullImageInspect{}, false
	}
	if des.Digest != "" {
		if info, ok := images[des.Digest]; ok {
			return info, true
		}
	}
	if des.Image != "" {
		if info, ok := images[des.Image]; ok {
			return info, true
		}
	}
	for _, info := range images {
		if des.Digest != "" {
			if strings.TrimPrefix(info.ID, "sha256:") == des.Digest {
				return info, true
			}
			for _, rd := range info.RepoDigests {
				if extractDigestHex(rd) == des.Digest {
					return info, true
				}
			}
		}
		if des.Image != "" {
			for _, rt := range info.RepoTags {
				if normalizeImageRepoRef(rt) == des.Image {
					return info, true
				}
			}
		}
	}
	return fullImageInspect{}, false
}

func diffSingleService(name string, des, run CanonicalServiceSpec) (reasons []string, details []string) {
	add := func(reason, detail string) {
		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
		details = append(details, fmt.Sprintf("service %s: %s", name, detail))
	}

	if des.HasBuild {
		add("build_defined", "service defines build section")
	}

	// 1. Image & digest
	switch {
	case des.Digest != "" && run.Digest != "":
		if des.Digest != run.Digest {
			add("image_changed", fmt.Sprintf("image digest changed (%s -> %s)", shortHex(run.Digest), shortHex(des.Digest)))
		} else if des.Image != "" && run.Image != "" && des.Image != run.Image {
			add("image_changed", fmt.Sprintf("image ref changed (%s -> %s)", run.Image, des.Image))
		}
	case des.Digest != "" && run.Digest == "":
		add("image_changed", "running image digest unknown")
	default:
		// Unpinned tag: require both normalized image ref and resolved local digest to match if available.
		if des.Image != run.Image {
			add("image_changed", fmt.Sprintf("image changed (%s -> %s)", run.Image, des.Image))
		} else if des.Digest == "" {
			add("image_changed", fmt.Sprintf("image %s has no resolved digest", des.Image))
		}
	}

	// 2. Environment (redacted: never put values in details!)
	if !mapsEqual(des.Environment, run.Environment) {
		changedKeys := diffMapKeys(run.Environment, des.Environment)
		add("env_changed", fmt.Sprintf("environment keys changed (%s)", strings.Join(changedKeys, ",")))
	}

	// 3. Ports
	if !slices.Equal(des.Ports, run.Ports) {
		add("ports_changed", "published ports changed")
	}

	// 4. Volumes / mounts (including bind content hash for repo-tracked files)
	if !slices.Equal(des.Volumes, run.Volumes) {
		add("volumes_changed", "volumes or bind mounts changed")
	}

	// 5. Networks & network_mode
	if des.NetworkMode != run.NetworkMode || !slices.Equal(des.Networks, run.Networks) {
		add("networks_changed", "networks changed")
	}

	// 6. Command & Entrypoint
	if !slices.Equal(des.Entrypoint, run.Entrypoint) || !slices.Equal(des.Command, run.Command) {
		add("command_changed", "command or entrypoint changed")
	}

	// 7. Labels (excluding compose internal metadata)
	if !mapsEqual(des.Labels, run.Labels) {
		changedKeys := diffMapKeys(run.Labels, des.Labels)
		add("labels_changed", fmt.Sprintf("labels changed (%s)", strings.Join(changedKeys, ",")))
	}

	return reasons, details
}

func pickPrimaryDiffReason(reasons map[string]bool) string {
	if len(reasons) == 0 {
		return "config_changed"
	}
	if len(reasons) > 1 {
		order := []string{
			"image_changed",
			"env_changed",
			"ports_changed",
			"volumes_changed",
			"networks_changed",
			"command_changed",
			"labels_changed",
			"service_added",
			"service_removed",
			"build_defined",
		}
		var matched []string
		for _, k := range order {
			if reasons[k] {
				matched = append(matched, k)
			}
		}
		if len(matched) == 1 {
			return matched[0]
		}
		return "multiple_changes"
	}
	for k := range reasons {
		return k
	}
	return "config_changed"
}

func (e *ComposeExecutor) fetchLiveProjectObjects(
	ctx context.Context,
	projectName string,
) ([]fullContainerInspect, map[string]fullImageInspect, map[string]string, error) {
	var rawContainers []byte
	client, engineErr := newEngineClient()
	if engineErr == nil {
		if listed, listErr := client.listProjectContainers(ctx, projectName); listErr == nil {
			if len(listed) == 0 {
				return nil, nil, nil, nil
			}
			var arr []json.RawMessage
			for _, item := range listed {
				if item.ID == "" {
					continue
				}
				b, inspErr := client.inspectContainer(ctx, item.ID)
				if inspErr != nil {
					return nil, nil, nil, inspErr
				}
				arr = append(arr, json.RawMessage(b))
			}
			rawContainers, _ = json.Marshal(arr)
		}
	}

	if len(rawContainers) == 0 {
		ids, err := e.listContainerIDsByFilter(ctx, []string{"label=com.docker.compose.project=" + projectName}, e.runtimeWorkDir())
		if err != nil || len(ids) == 0 {
			return nil, nil, nil, err
		}
		out, err := e.runCommand(ctx, "docker", append([]string{"inspect"}, ids...), e.runtimeWorkDir(), nil, nil)
		if err != nil {
			return nil, nil, nil, err
		}
		rawContainers = extractJSON(out)
	}

	var containers []fullContainerInspect
	if err := json.Unmarshal(rawContainers, &containers); err != nil {
		return nil, nil, nil, fmt.Errorf("unmarshal container inspect: %w", err)
	}

	imageMap := map[string]fullImageInspect{}
	seenRefs := map[string]bool{}
	for _, c := range containers {
		for _, ref := range []string{strings.TrimSpace(c.Image), strings.TrimSpace(c.Config.Image)} {
			if ref == "" || seenRefs[ref] {
				continue
			}
			seenRefs[ref] = true
			if img, err := e.inspectSingleImageFull(ctx, client, ref); err == nil {
				registerImageKeys(imageMap, ref, img)
			}
		}
	}

	netLabels := map[string]string{}
	seenNets := map[string]bool{}
	for _, c := range containers {
		for netName := range c.NetworkSettings.Networks {
			if netName == "" || seenNets[netName] {
				continue
			}
			seenNets[netName] = true
			if logical := e.inspectComposeNetworkKey(ctx, client, netName, projectName); logical != "" {
				netLabels[netName] = logical
			}
		}
	}

	return containers, imageMap, netLabels, nil
}

func (e *ComposeExecutor) inspectDesiredImages(ctx context.Context, refs []string, env map[string]string) map[string]fullImageInspect {
	out := map[string]fullImageInspect{}
	client, _ := newEngineClient()
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if img, err := e.inspectSingleImageFullWithEnv(ctx, client, ref, env); err == nil {
			registerImageKeys(out, ref, img)
		}
	}
	return out
}

func (e *ComposeExecutor) inspectSingleImageFull(ctx context.Context, client *engineClient, ref string) (fullImageInspect, error) {
	return e.inspectSingleImageFullWithEnv(ctx, client, ref, nil)
}

func (e *ComposeExecutor) inspectSingleImageFullWithEnv(ctx context.Context, client *engineClient, ref string, env map[string]string) (fullImageInspect, error) {
	if client != nil {
		if body, err := client.inspectImage(ctx, ref); err == nil {
			var doc fullImageInspect
			if json.Unmarshal(body, &doc) == nil && doc.ID != "" {
				return doc, nil
			}
		}
	}
	out, err := e.runCommand(ctx, "docker", []string{"image", "inspect", ref}, e.runtimeWorkDir(), env, nil)
	if err != nil {
		return fullImageInspect{}, err
	}
	var docs []fullImageInspect
	if err := json.Unmarshal(extractJSON(out), &docs); err != nil || len(docs) == 0 {
		return fullImageInspect{}, fmt.Errorf("empty image inspect for %s", ref)
	}
	return docs[0], nil
}

func (e *ComposeExecutor) inspectComposeNetworkKey(ctx context.Context, client *engineClient, netName, projectName string) string {
	if client != nil {
		if body, err := client.inspectNetwork(ctx, netName); err == nil {
			var doc dockerNetworkInspect
			if json.Unmarshal(body, &doc) == nil {
				if strings.TrimSpace(doc.Labels["com.docker.compose.project"]) == projectName {
					if key := strings.TrimSpace(doc.Labels["com.docker.compose.network"]); key != "" {
						return projectName + "_" + key
					}
				}
				if doc.Name != "" {
					return doc.Name
				}
			}
		}
	}
	out, err := e.runCommand(ctx, "docker", []string{"network", "inspect", netName}, e.runtimeWorkDir(), nil, nil)
	if err == nil {
		var docs []dockerNetworkInspect
		if json.Unmarshal(extractJSON(out), &docs) == nil && len(docs) > 0 {
			if strings.TrimSpace(docs[0].Labels["com.docker.compose.project"]) == projectName {
				if key := strings.TrimSpace(docs[0].Labels["com.docker.compose.network"]); key != "" {
					return projectName + "_" + key
				}
			}
			if docs[0].Name != "" {
				return docs[0].Name
			}
		}
	}
	return netName
}

func registerImageKeys(dst map[string]fullImageInspect, ref string, img fullImageInspect) {
	dst[ref] = img
	dst[normalizeImageRepoRef(ref)] = img
	if d := extractDigestHex(ref); d != "" {
		dst[d] = img
	}
	if id := strings.TrimPrefix(strings.TrimSpace(img.ID), "sha256:"); id != "" {
		dst[id] = img
		dst["sha256:"+id] = img
	}
	for _, rd := range img.RepoDigests {
		dst[rd] = img
		if d := extractDigestHex(rd); d != "" {
			dst[d] = img
		}
	}
	for _, rt := range img.RepoTags {
		dst[rt] = img
		dst[normalizeImageRepoRef(rt)] = img
	}
}

func lookupImageInspect(c fullContainerInspect, images map[string]fullImageInspect) (fullImageInspect, bool) {
	for _, key := range []string{
		strings.TrimSpace(c.Image),
		strings.TrimPrefix(strings.TrimSpace(c.Image), "sha256:"),
		strings.TrimSpace(c.Config.Image),
		normalizeImageRepoRef(c.Config.Image),
		extractDigestHex(c.Config.Image),
	} {
		if key == "" {
			continue
		}
		if info, ok := images[key]; ok {
			return info, true
		}
	}
	return fullImageInspect{}, false
}

func extractDigestHex(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "@sha256:"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(ref[i+len("@sha256:"):]))
	}
	if strings.HasPrefix(strings.ToLower(ref), "sha256:") {
		return strings.ToLower(strings.TrimSpace(ref[len("sha256:"):]))
	}
	return ""
}

// normalizeDigestHex returns the digest hex of an already resolved digest: bare
// 64 hex, `sha256:<hex>` or `repo@sha256:<hex>`. Anything else yields "".
func normalizeDigestHex(value string) string {
	if d := extractDigestHex(value); d != "" {
		return d
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

// imageInspectDigest names the content of a locally inspected image: its first
// RepoDigest, or the image ID when the image has none (built or loaded locally).
// The desired and the running side must both use this rule, otherwise an image
// referenced by tag never compares equal to itself.
func imageInspectDigest(img fullImageInspect) string {
	for _, rd := range img.RepoDigests {
		if d := extractDigestHex(rd); d != "" {
			return d
		}
	}
	return strings.TrimPrefix(strings.TrimSpace(img.ID), "sha256:")
}

// localImageDigest returns the digest of imageRef from local image inspect
// results, or "" when the image was not inspected successfully.
func localImageDigest(images map[string]fullImageInspect, imageRef string) string {
	imageRef = strings.TrimSpace(imageRef)
	img, ok := images[imageRef]
	if !ok {
		img, ok = images[normalizeImageRepoRef(imageRef)]
	}
	if !ok {
		return ""
	}
	return imageInspectDigest(img)
}

// unescapeComposeDollars undoes the `$` to `$$` escaping of `docker compose config`.
//
// After interpolation Compose doubles every `$` left in the rendered document
// (environment, command, entrypoint, labels, paths) so the output can be read
// back as a Compose file. A container holds the value with a single `$`, so
// the escaped form never equals Docker Engine inspect data. Collapsing `$$`
// to `$` is the exact inverse and does not interpolate anything: a literal
// `$$` in a container is rendered as `$$$$` and comes back as `$$`.
//
// A run of `$` with odd length cannot come from that escaping, so such a
// document is returned unchanged.
func unescapeComposeDollars(configJSON []byte) []byte {
	if !bytes.Contains(configJSON, []byte("$$")) {
		return configJSON
	}
	run := 0
	for _, b := range configJSON {
		if b == '$' {
			run++
			continue
		}
		if run%2 != 0 {
			return configJSON
		}
		run = 0
	}
	if run%2 != 0 {
		return configJSON
	}
	return bytes.ReplaceAll(configJSON, []byte("$$"), []byte("$"))
}

func normalizeImageRepoRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "index.docker.io/")
	if !strings.Contains(ref, "/") {
		ref = "library/" + ref
	} else {
		first, rest, _ := strings.Cut(ref, "/")
		if !strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost" {
			// e.g. alpine/git:2.54 -> docker.io/alpine/git:2.54 normalized without docker.io prefix
			ref = first + "/" + rest
		} else if (first == "docker.io" || first == "index.docker.io") && !strings.Contains(rest, "/") {
			ref = "library/" + rest
		}
	}
	// Add :latest if neither tag nor digest had a tag
	lastSlash := strings.LastIndex(ref, "/")
	tagPart := ref
	if lastSlash >= 0 {
		tagPart = ref[lastSlash+1:]
	}
	if !strings.Contains(tagPart, ":") {
		// Note: when compose uses repo@sha256:... without tag, Docker Config.Image also has repo@sha256:... (no tag).
		// Keep tagless only if original ref had @sha256:, otherwise default to :latest.
		return ref
	}
	return ref
}

func isTruthyExternal(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case map[string]any:
		return true
	default:
		return false
	}
}

func parseStringOrSlice(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil
		}
		return []string{"/bin/sh", "-c", s}
	case []string:
		if len(v) == 0 {
			return nil
		}
		return append([]string(nil), v...)
	case []any:
		if len(v) == 0 {
			return nil
		}
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return nil
	}
}

func parseKeyValueMap(raw any) map[string]string {
	out := map[string]string{}
	switch v := raw.(type) {
	case nil:
		return nil
	case map[string]string:
		for k, val := range v {
			out[k] = val
		}
	case map[string]any:
		for k, val := range v {
			if val == nil {
				continue
			}
			out[k] = fmt.Sprint(val)
		}
	case []any:
		for _, item := range v {
			s := fmt.Sprint(item)
			if k, val, ok := strings.Cut(s, "="); ok {
				out[k] = val
			}
		}
	case []string:
		for _, s := range v {
			if k, val, ok := strings.Cut(s, "="); ok {
				out[k] = val
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envSliceToMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func filterUserLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func canonicalizeRenderedPorts(ports []renderedPort) []CanonicalPort {
	if len(ports) == 0 {
		return nil
	}
	seen := map[CanonicalPort]bool{}
	var out []CanonicalPort
	for _, p := range ports {
		target := anyToInt(p.Target)
		published := anyToInt(p.Published)
		if target <= 0 && published <= 0 {
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(p.Protocol))
		if proto == "" {
			proto = "tcp"
		}
		hostIP := normalizeHostIP(p.HostIP)
		cp := CanonicalPort{
			HostIP:    hostIP,
			Published: published,
			Target:    target,
			Protocol:  proto,
		}
		if !seen[cp] {
			seen[cp] = true
			out = append(out, cp)
		}
	}
	sortCanonicalPorts(out)
	return out
}

func canonicalizeInspectedPorts(bindings map[string][]dockerPortBinding) []CanonicalPort {
	if len(bindings) == 0 {
		return nil
	}
	seen := map[CanonicalPort]bool{}
	var out []CanonicalPort
	for portProto, list := range bindings {
		targetStr, proto, ok := strings.Cut(portProto, "/")
		if !ok {
			proto = "tcp"
		}
		proto = strings.ToLower(strings.TrimSpace(proto))
		if proto == "" {
			proto = "tcp"
		}
		target, _ := strconv.Atoi(strings.TrimSpace(targetStr))
		for _, b := range list {
			pub, _ := strconv.Atoi(strings.TrimSpace(b.HostPort))
			hostIP := normalizeHostIP(b.HostIP)
			if hostIP == "::" && hasIPv4Wildcard(list, b.HostPort) {
				// Docker Engine mirrors 0.0.0.0 bindings onto :: when IPv6 is enabled unless explicitly bound to ::.
				continue
			}
			cp := CanonicalPort{
				HostIP:    hostIP,
				Published: pub,
				Target:    target,
				Protocol:  proto,
			}
			if !seen[cp] {
				seen[cp] = true
				out = append(out, cp)
			}
		}
	}
	sortCanonicalPorts(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func hasIPv4Wildcard(list []dockerPortBinding, hostPort string) bool {
	for _, b := range list {
		if strings.TrimSpace(b.HostPort) == strings.TrimSpace(hostPort) && normalizeHostIP(b.HostIP) == "0.0.0.0" {
			return true
		}
	}
	return false
}

func normalizeHostIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" || ip == "0.0.0.0" {
		return "0.0.0.0"
	}
	return ip
}

func sortCanonicalPorts(ports []CanonicalPort) {
	slices.SortFunc(ports, func(a, b CanonicalPort) int {
		if a.Published != b.Published {
			return a.Published - b.Published
		}
		if a.Target != b.Target {
			return a.Target - b.Target
		}
		if a.Protocol != b.Protocol {
			return strings.Compare(a.Protocol, b.Protocol)
		}
		return strings.Compare(a.HostIP, b.HostIP)
	})
}

func canonicalizeRenderedMounts(
	vols []renderedVolume,
	volActualName map[string]string,
	stagingRepoAbs string,
	targetAppRepoAbs string,
) []CanonicalMount {
	if len(vols) == 0 {
		return nil
	}
	var out []CanonicalMount
	for _, v := range vols {
		mtype := strings.ToLower(strings.TrimSpace(v.Type))
		if mtype == "" {
			mtype = "volume"
		}
		src := strings.TrimSpace(v.Source)
		var contentHash string
		switch mtype {
		case "volume":
			if mapped, ok := volActualName[src]; ok {
				src = mapped
			}
		case "bind":
			srcAbs, err := filepath.Abs(src)
			if err == nil {
				src = filepath.Clean(srcAbs)
			}
			if stagingRepoAbs != "" && pathWithin(src, stagingRepoAbs) {
				contentHash = hashBindPath(src)
				if targetAppRepoAbs != "" {
					if rel, relErr := filepath.Rel(stagingRepoAbs, src); relErr == nil {
						src = filepath.Clean(filepath.Join(targetAppRepoAbs, rel))
					}
				}
			}
		case "tmpfs":
			src = ""
		}
		out = append(out, CanonicalMount{
			Type:        mtype,
			Source:      filepath.ToSlash(src),
			Target:      filepath.ToSlash(filepath.Clean(strings.TrimSpace(v.Target))),
			ReadOnly:    v.ReadOnly,
			ContentHash: contentHash,
		})
	}
	sortCanonicalMounts(out)
	return out
}

func canonicalizeInspectedMounts(runtimeMounts []dockerRuntimeMount, hostMounts []dockerHostMount, appRepoAbs string) []CanonicalMount {
	var out []CanonicalMount
	seenTargets := map[string]bool{}
	for _, m := range runtimeMounts {
		mtype := strings.ToLower(strings.TrimSpace(m.Type))
		target := filepath.ToSlash(filepath.Clean(strings.TrimSpace(m.Destination)))
		if target == "" || target == "." {
			continue
		}
		src := strings.TrimSpace(m.Source)
		var contentHash string
		switch mtype {
		case "volume":
			if strings.TrimSpace(m.Name) != "" {
				src = strings.TrimSpace(m.Name)
			}
			// Anonymous image volumes (64-hex random name not declared in compose): skip only if not in HostConfig.Mounts
			if len(src) == 64 && !hostMountHasTarget(hostMounts, m.Destination) {
				continue
			}
		case "bind":
			if srcAbs, err := filepath.Abs(src); err == nil {
				src = filepath.Clean(srcAbs)
			}
			if appRepoAbs != "" && pathWithin(src, appRepoAbs) {
				contentHash = hashBindPath(src)
			}
		case "tmpfs":
			src = ""
		}
		seenTargets[target] = true
		out = append(out, CanonicalMount{
			Type:        mtype,
			Source:      filepath.ToSlash(src),
			Target:      target,
			ReadOnly:    !m.RW,
			ContentHash: contentHash,
		})
	}
	for _, hm := range hostMounts {
		mtype := strings.ToLower(strings.TrimSpace(hm.Type))
		target := filepath.ToSlash(filepath.Clean(strings.TrimSpace(hm.Target)))
		if mtype == "tmpfs" && target != "" && !seenTargets[target] {
			out = append(out, CanonicalMount{
				Type:     "tmpfs",
				Source:   "",
				Target:   target,
				ReadOnly: hm.ReadOnly,
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	sortCanonicalMounts(out)
	return out
}

func hostMountHasTarget(mounts []dockerHostMount, dest string) bool {
	cleanDest := filepath.ToSlash(filepath.Clean(strings.TrimSpace(dest)))
	for _, m := range mounts {
		if filepath.ToSlash(filepath.Clean(strings.TrimSpace(m.Target))) == cleanDest {
			return true
		}
	}
	return false
}

func sortCanonicalMounts(mounts []CanonicalMount) {
	slices.SortFunc(mounts, func(a, b CanonicalMount) int {
		if a.Target != b.Target {
			return strings.Compare(a.Target, b.Target)
		}
		if a.Type != b.Type {
			return strings.Compare(a.Type, b.Type)
		}
		return strings.Compare(a.Source, b.Source)
	})
}

func hashBindPath(target string) string {
	info, err := os.Stat(target)
	if err != nil {
		return "missing"
	}
	h := sha256.New()
	if !info.IsDir() {
		data, readErr := os.ReadFile(target)
		if readErr != nil {
			return "unreadable"
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	_ = filepath.Walk(target, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil || fi == nil || fi.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(target, path)
		if relErr != nil || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		fileSum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s:%x\n", filepath.ToSlash(rel), fileSum)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalizeRenderedNetworks(raw any, netActualName map[string]string, projectName, networkMode string) []string {
	if nm := normalizeNetworkMode(networkMode); nm != "" {
		return nil
	}
	var logicalKeys []string
	switch v := raw.(type) {
	case map[string]any:
		for k := range v {
			logicalKeys = append(logicalKeys, k)
		}
	case []any:
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				logicalKeys = append(logicalKeys, s)
			}
		}
	case []string:
		logicalKeys = append(logicalKeys, v...)
	}
	if len(logicalKeys) == 0 {
		logicalKeys = []string{"default"}
	}
	seen := map[string]bool{}
	var out []string
	for _, key := range logicalKeys {
		actual := netActualName[key]
		if actual == "" {
			actual = projectName + "_" + key
		}
		if !seen[actual] {
			seen[actual] = true
			out = append(out, actual)
		}
	}
	sort.Strings(out)
	return out
}

func canonicalizeInspectedNetworks(
	nets map[string]struct {
		NetworkID string   `json:"NetworkID"`
		Aliases   []string `json:"Aliases"`
	},
	networkMode string,
	projectName string,
	networkProjectNames map[string]string,
) []string {
	if nm := normalizeContainerNetworkMode(networkMode, projectName, networkProjectNames); nm != "" {
		return nil
	}
	if len(nets) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for name := range nets {
		actual := name
		if mapped := networkProjectNames[name]; mapped != "" {
			actual = mapped
		}
		if !seen[actual] {
			seen[actual] = true
			out = append(out, actual)
		}
	}
	sort.Strings(out)
	return out
}

func normalizeNetworkMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", "default", "bridge":
		return ""
	default:
		return mode
	}
}

func normalizeContainerNetworkMode(mode, projectName string, networkProjectNames map[string]string) string {
	raw := strings.TrimSpace(mode)
	lower := strings.ToLower(raw)
	switch lower {
	case "", "default", "bridge":
		return ""
	case "host", "none":
		return lower
	}
	if strings.HasPrefix(lower, "container:") || strings.HasPrefix(lower, "service:") {
		return lower
	}
	// User-defined compose bridge network appears in HostConfig.NetworkMode as the network name!
	if strings.HasPrefix(raw, projectName+"_") {
		return ""
	}
	if _, isComposeNet := networkProjectNames[raw]; isComposeNet {
		return ""
	}
	return ""
}

func anyToInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case float64:
		return int(val)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(val))
		return n
	default:
		return 0
	}
}

func sliceOrNil(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

func diffMapKeys(a, b map[string]string) []string {
	seen := map[string]bool{}
	for k, va := range a {
		if vb, ok := b[k]; !ok || va != vb {
			seen[k] = true
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func shortHex(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
