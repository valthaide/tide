package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/valthaide/tide/internal/fleetcheck"
)

// Plan actions reported per service.
const (
	PlanActionCreate   = "create"
	PlanActionRecreate = "recreate"
	PlanActionRestart  = "restart"
	PlanActionRemove   = "remove"
	PlanActionNoop     = "no_change"
)

// ServicePlan is the planned action for one compose service, with
// human-readable, secret-free reasons (environment values never appear).
type ServicePlan struct {
	Service string   `json:"service"`
	Action  string   `json:"action"`
	Reasons []string `json:"reasons,omitempty"`
}

// WatchPlanEntry is one watch_files entry whose content drifted from the last
// applied baseline, with the action a reconcile would run for it.
type WatchPlanEntry struct {
	Path    string `json:"path"`
	Service string `json:"service,omitempty"`
	Action  string `json:"action"` // reload | restart
}

// VolumePlanEntry is one named volume an apply would create.
type VolumePlanEntry struct {
	Name     string `json:"name"`
	External bool   `json:"external,omitempty"`
}

// AppPlan describes what an apply would do, without doing it.
type AppPlan struct {
	Project          string            `json:"project"`
	Services         []ServicePlan     `json:"services"`
	VolumesToCreate  []VolumePlanEntry `json:"volumes_to_create,omitempty"`
	WatchActions     []WatchPlanEntry  `json:"watch_actions,omitempty"`
	ForceRecreateAll bool              `json:"force_recreate_all,omitempty"`
}

// PlanInput carries the controller-known drift into plan building.
type PlanInput struct {
	// EnvChangedPaths are env_file paths whose content hash changed since the
	// last apply; they scope a forced recreate to the services mounting them.
	EnvChangedPaths []string
	// WatchChanged lists watch_files entries whose hash drifted.
	WatchChanged []WatchPlanEntry
	// ExistingVolumes names Docker volumes already present; nil means unknown
	// and the volumes section is skipped.
	ExistingVolumes map[string]bool
}

// BuildAppPlan combines the rendered-vs-running diff with controller drift
// hints into per-service actions. It mirrors what ApplyImages does:
// `up -d` recreates services whose rendered config differs, env_file drift
// force-recreates only the services mounting the changed files (or the whole
// project when a changed file maps to no service), and watch_files drift
// restarts or reloads its targets in place.
func BuildAppPlan(configJSON []byte, desired, running CanonicalProjectSpec, desiredImages map[string]fullImageInspect, in PlanInput) *AppPlan {
	plan := &AppPlan{Project: desired.Project}
	if plan.Project == "" {
		plan.Project = running.Project
	}

	names := map[string]struct{}{}
	for name := range desired.Services {
		names[name] = struct{}{}
	}
	for name := range running.Services {
		names[name] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	byName := map[string]*ServicePlan{}
	zeroScale := scaledToZeroServices(configJSON)
	for _, name := range sorted {
		sp := &ServicePlan{Service: name, Action: PlanActionNoop}
		byName[name] = sp
		des, inDesired := desired.Services[name]
		run, inRunning := running.Services[name]
		switch {
		case inDesired && !inRunning && zeroScale[name]:
			// replicas: 0 — compose up starts nothing, the service stays absent.
		case inDesired && !inRunning:
			sp.Action = PlanActionCreate
			sp.Reasons = []string{"service added"}
		case !inDesired && inRunning:
			sp.Action = PlanActionRemove
			sp.Reasons = []string{"service removed from configuration"}
		case zeroScale[name]:
			// Containers exist but the service is scaled to 0: up removes them.
			sp.Action = PlanActionRemove
			sp.Reasons = []string{"service scaled to 0 replicas"}
		default:
			effective := mergeServiceWithImageDefaults(des, desiredImages)
			_, details := diffSingleService(name, stripMountContentHashes(effective), stripMountContentHashes(run))
			for _, d := range details {
				sp.Reasons = append(sp.Reasons, strings.TrimPrefix(d, "service "+name+": "))
			}
			if !healthcheckEqual(effective.Healthcheck, run.Healthcheck) {
				sp.Reasons = append(sp.Reasons, "healthcheck changed")
			}
			if len(sp.Reasons) > 0 {
				sp.Action = PlanActionRecreate
			}
		}
	}

	if len(in.EnvChangedPaths) > 0 {
		matched, unmapped := ServicesForEnvPaths(configJSON, in.EnvChangedPaths)
		if len(unmapped) > 0 {
			// Same fallback as ApplyImages: a changed env_file that no service
			// mounts force-recreates the whole project.
			plan.ForceRecreateAll = true
			reason := "env_file changed outside service env_file mounts (" + strings.Join(unmapped, ", ") + "); force recreate"
			for _, name := range sorted {
				sp := byName[name]
				if _, ok := running.Services[name]; !ok {
					continue
				}
				if sp.Action == PlanActionNoop {
					sp.Action = PlanActionRecreate
				}
				if sp.Action == PlanActionRecreate {
					sp.Reasons = append(sp.Reasons, reason)
				}
			}
		} else {
			perService := envPathsPerService(configJSON)
			for _, svc := range matched {
				sp, ok := byName[svc]
				if !ok {
					continue
				}
				if sp.Action == PlanActionNoop {
					sp.Action = PlanActionRecreate
				}
				paths := intersectSorted(perService[svc], in.EnvChangedPaths)
				reason := "env_file changed"
				if len(paths) > 0 {
					reason += " (" + strings.Join(paths, ", ") + ")"
				}
				sp.Reasons = append(sp.Reasons, reason)
			}
		}
	}

	applies := plan.ForceRecreateAll
	for _, sp := range byName {
		if sp.Action != PlanActionNoop {
			applies = true
		}
	}

	plan.WatchActions = append(plan.WatchActions, in.WatchChanged...)
	for _, w := range in.WatchChanged {
		if w.Action != PlanActionRestart || strings.TrimSpace(w.Service) == "" {
			continue
		}
		sp, ok := byName[w.Service]
		if !ok || sp.Action != PlanActionNoop {
			// A recreate already covers the in-place restart.
			continue
		}
		sp.Action = PlanActionRestart
		sp.Reasons = []string{"watch_files changed (" + w.Path + ")"}
	}

	// A service sharing another service's network namespace loses it when the
	// target container is recreated, so compose recreates it too.
	for name, des := range desired.Services {
		target, ok := strings.CutPrefix(des.NetworkMode, "service:")
		if !ok {
			continue
		}
		tsp, tok := byName[target]
		sp, sok := byName[name]
		if !tok || !sok || sp.Action != PlanActionNoop {
			continue
		}
		if tsp.Action == PlanActionRecreate {
			sp.Action = PlanActionRecreate
			sp.Reasons = []string{"network namespace target " + target + " is recreated"}
		}
	}

	// One-shot services with the rerun label are force-recreated on every
	// apply; they restart only when an apply actually happens.
	if applies {
		for _, name := range rerunServiceNames(configJSON) {
			sp, ok := byName[name]
			if !ok || sp.Action != PlanActionNoop {
				continue
			}
			sp.Action = PlanActionRestart
			sp.Reasons = []string{"one-shot re-run on every apply (label com.tide.oneshot=rerun)"}
		}
	}

	if in.ExistingVolumes != nil {
		plan.VolumesToCreate = planVolumesToCreate(configJSON, plan.Project, desired, in.ExistingVolumes)
	}

	plan.Services = make([]ServicePlan, 0, len(sorted))
	for _, name := range sorted {
		plan.Services = append(plan.Services, *byName[name])
	}
	return plan
}

// stripMountContentHashes drops bind content hashes before the plan diff.
// Apply (compose up) never recreates on bind content alone — the container
// reads the new bytes through the mount — and content changes of watched
// files already surface as WatchActions, so a content hash mismatch must not
// report a recreate. The volumes slice is copied; the input is not mutated.
func stripMountContentHashes(spec CanonicalServiceSpec) CanonicalServiceSpec {
	if len(spec.Volumes) == 0 {
		return spec
	}
	vols := make([]CanonicalMount, len(spec.Volumes))
	for i, m := range spec.Volumes {
		m.ContentHash = ""
		vols[i] = m
	}
	spec.Volumes = vols
	return spec
}

// scaledToZeroServices reports services whose rendered config sets
// deploy.replicas (or legacy scale) to 0: apply neither creates nor keeps
// containers for them.
func scaledToZeroServices(configJSON []byte) map[string]bool {
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Replicas *int64 `json:"replicas"`
			} `json:"deploy"`
			Scale *int64 `json:"scale"`
		} `json:"services"`
	}
	if err := json.Unmarshal(unescapeComposeDollars(configJSON), &doc); err != nil {
		return nil
	}
	var out map[string]bool
	for name, svc := range doc.Services {
		if svc.Deploy.Replicas != nil && *svc.Deploy.Replicas == 0 ||
			svc.Scale != nil && *svc.Scale == 0 {
			if out == nil {
				out = map[string]bool{}
			}
			out[name] = true
		}
	}
	return out
}

// envPathsPerService maps each service to the env_file paths its rendered
// config declares (both object and string forms).
func envPathsPerService(configJSON []byte) map[string][]string {
	var doc struct {
		Services map[string]struct {
			EnvFile json.RawMessage `json:"env_file"`
		} `json:"services"`
	}
	if err := json.Unmarshal(configJSON, &doc); err != nil {
		return nil
	}
	out := map[string][]string{}
	for name, svc := range doc.Services {
		if paths := envFilePathsFromConfig(svc.EnvFile); len(paths) > 0 {
			out[name] = paths
		}
	}
	return out
}

func intersectSorted(a, b []string) []string {
	set := map[string]struct{}{}
	for _, x := range b {
		set[filepath.Clean(strings.TrimSpace(x))] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := set[filepath.Clean(strings.TrimSpace(x))]; ok {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func rerunServiceNames(configJSON []byte) []string {
	sum, err := fleetcheck.SummarizeCompose(configJSON)
	if err != nil {
		return nil
	}
	return sum.Rerun
}

// planVolumesToCreate lists named volumes referenced by services that do not
// exist yet. Compose creates project volumes on up; Tide creates external
// ones per host policy — both are reported, flagged by External.
func planVolumesToCreate(configJSON []byte, project string, desired CanonicalProjectSpec, existing map[string]bool) []VolumePlanEntry {
	var doc renderedComposeDoc
	if err := json.Unmarshal(unescapeComposeDollars(configJSON), &doc); err != nil {
		return nil
	}
	referenced := map[string]bool{}
	zeroScale := scaledToZeroServices(configJSON)
	for svcName, svc := range desired.Services {
		if zeroScale[svcName] {
			// A scaled-to-0 service gets no containers, so compose creates no
			// volumes for it either.
			continue
		}
		for _, m := range svc.Volumes {
			if m.Type == "volume" && m.Source != "" {
				referenced[m.Source] = true
			}
		}
	}
	var out []VolumePlanEntry
	for key, vol := range doc.Volumes {
		external := isTruthyExternal(vol.External)
		name := strings.TrimSpace(vol.Name)
		if name == "" {
			if external {
				name = key
			} else {
				name = project + "_" + key
			}
		}
		if !referenced[name] || existing[name] {
			continue
		}
		out = append(out, VolumePlanEntry{Name: name, External: external})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PlanApp renders the desired compose state of an app and diffs it against
// the running project, returning what an apply would do. It never pulls,
// starts, stops, or writes anything besides the same temporary env override
// file an apply renders with.
func (e *ComposeExecutor) PlanApp(ctx context.Context, appID, composePath string, envVars map[string]string, opts ApplyOptions, in PlanInput) (*AppPlan, error) {
	repoDir := filepath.Join(e.WorkDir, appID, "repo")
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("app %s has no repository checkout yet", appID)
	}
	inv, err := e.ResolveInvocation(repoDir, composePath, opts)
	if err != nil {
		return nil, err
	}
	projectName := strings.TrimSpace(opts.Project)
	if projectName == "" {
		projectName = e.projectFor(appID)
	}

	var overrideArgs []string
	if len(envVars) > 0 {
		var cleanup func()
		overrideArgs, cleanup, err = e.prepareEnvOverrides(inv.ProjectDir, envVars)
		if err != nil {
			return nil, fmt.Errorf("prepare env override: %w", err)
		}
		defer cleanup()
	}

	configJSON, err := renderComposeConfigDirect(ctx, inv, projectName, opts.Vars, overrideArgs)
	if err != nil {
		return nil, err
	}

	desiredImages := e.inspectDesiredImages(ctx, imagesFromConfig(configJSON), nil)
	desired, err := ParseRenderedComposeSpec(configJSON, projectName, repoDir, "", func(imageRef string) string {
		return localImageDigest(desiredImages, imageRef)
	})
	if err != nil {
		return nil, fmt.Errorf("parse rendered config: %w", err)
	}

	running, _, err := e.InspectRunningProjectSpec(ctx, projectName, repoDir)
	if err != nil {
		return nil, fmt.Errorf("inspect running project: %w", err)
	}

	if volumes := e.allVolumeNames(ctx); volumes != nil {
		in.ExistingVolumes = volumes
	}
	return BuildAppPlan(configJSON, desired, running, desiredImages, in), nil
}

// allVolumeNames lists every Docker volume name, or nil when the daemon
// cannot be queried (the plan then skips the volumes section).
func (e *ComposeExecutor) allVolumeNames(ctx context.Context) map[string]bool {
	out, err := e.runCommand(ctx, "docker", []string{"volume", "ls", "-q"}, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names[n] = true
		}
	}
	return names
}

// DefaultFleetCheckoutDir exposes the fleet checkout root the executor reads
// templates from, for callers that need the raw fleet repository.
func (e *ComposeExecutor) DefaultFleetCheckoutDir() string {
	return e.fleetCheckoutDir(ApplyOptions{})
}

// MaterializeTemplateAtRef extracts templates/<template>/ from a fleet git ref
// (full commit hash, branch, or tag already present in the checkout) into a
// fresh temp directory laid out like a fleet checkout, so a plan can render
// the template at a not-yet-applied fleet revision. The live checkout is not
// modified. The returned cleanup removes the temp directory.
func MaterializeTemplateAtRef(fleetDir, ref, template string) (string, func(), error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil, fmt.Errorf("fleet ref is required")
	}
	if issue := fleetcheck.CheckTemplateName(template); issue != nil {
		return "", nil, fmt.Errorf("%s", issue.Message)
	}
	repo, err := git.PlainOpen(fleetDir)
	if err != nil {
		return "", nil, fmt.Errorf("open fleet checkout: %w", err)
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil || hash == nil {
		return "", nil, fmt.Errorf("fleet ref %q not found in the fleet checkout (push it and let the fleet sync fetch it first)", ref)
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return "", nil, fmt.Errorf("read fleet ref %q: %w", ref, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", nil, fmt.Errorf("read fleet ref %q tree: %w", ref, err)
	}
	sub, err := tree.Tree("templates/" + template)
	if err != nil {
		return "", nil, fmt.Errorf("template %q not found at fleet ref %q", template, ref)
	}
	tmp, err := os.MkdirTemp("", "tide-plan-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	base := filepath.Join(tmp, "templates", template)
	files := sub.Files()
	defer files.Close()
	for {
		f, ferr := files.Next()
		if ferr == io.EOF {
			break
		}
		if ferr != nil {
			cleanup()
			return "", nil, ferr
		}
		target := filepath.Join(base, filepath.FromSlash(f.Name))
		if !pathWithin(target, base) {
			cleanup()
			return "", nil, fmt.Errorf("unsafe path %q in fleet template tree", f.Name)
		}
		reader, rerr := f.Reader()
		if rerr != nil {
			cleanup()
			return "", nil, rerr
		}
		data, rerr := io.ReadAll(reader)
		_ = reader.Close()
		if rerr != nil {
			cleanup()
			return "", nil, rerr
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			cleanup()
			return "", nil, mkErr
		}
		if f.Mode == filemode.Symlink {
			_ = os.Remove(target)
			if slErr := os.Symlink(string(data), target); slErr != nil {
				cleanup()
				return "", nil, slErr
			}
			continue
		}
		perm := os.FileMode(0o644)
		if f.Mode == filemode.Executable {
			perm = 0o755
		}
		if wErr := os.WriteFile(target, data, perm); wErr != nil {
			cleanup()
			return "", nil, wErr
		}
	}
	return tmp, cleanup, nil
}

// canonicalHealthcheck converts the Docker inspect form, normalizing a
// missing or disabled healthcheck to nil.
func canonicalHealthcheck(h *dockerHealthcheck) *CanonicalHealthcheck {
	if h == nil {
		return nil
	}
	return normalizeHealthcheck(&CanonicalHealthcheck{
		Test:        sliceOrNil(h.Test),
		Interval:    h.Interval,
		Timeout:     h.Timeout,
		StartPeriod: h.StartPeriod,
		Retries:     h.Retries,
	})
}

func normalizeHealthcheck(h *CanonicalHealthcheck) *CanonicalHealthcheck {
	if h == nil {
		return nil
	}
	if len(h.Test) == 0 {
		return nil
	}
	if len(h.Test) == 1 && strings.EqualFold(strings.TrimSpace(h.Test[0]), "NONE") {
		return nil
	}
	return h
}

func healthcheckEqual(a, b *CanonicalHealthcheck) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return slices.Equal(a.Test, b.Test) &&
		a.Interval == b.Interval &&
		a.Timeout == b.Timeout &&
		a.StartPeriod == b.StartPeriod &&
		a.Retries == b.Retries
}

// parseRenderedHealthcheck converts the compose config JSON healthcheck into
// the canonical form. Compose renders durations as strings ("30s") but plain
// nanosecond numbers are accepted too.
func parseRenderedHealthcheck(raw json.RawMessage) *CanonicalHealthcheck {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var doc struct {
		Test        any  `json:"test"`
		Interval    any  `json:"interval"`
		Timeout     any  `json:"timeout"`
		StartPeriod any  `json:"start_period"`
		Retries     any  `json:"retries"`
		Disable     bool `json:"disable"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	hc := &CanonicalHealthcheck{
		Test:        parseHealthcheckTest(doc.Test),
		Interval:    composeDurationNs(doc.Interval),
		Timeout:     composeDurationNs(doc.Timeout),
		StartPeriod: composeDurationNs(doc.StartPeriod),
		Retries:     anyToInt(doc.Retries),
	}
	if doc.Disable {
		hc.Test = []string{"NONE"}
	}
	return normalizeHealthcheck(hc)
}

func parseHealthcheckTest(raw any) []string {
	switch v := raw.(type) {
	case nil:
		return nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil
		}
		return []string{"CMD-SHELL", s}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []string:
		return sliceOrNil(v)
	default:
		return nil
	}
}

func composeDurationNs(raw any) int64 {
	switch v := raw.(type) {
	case nil:
		return 0
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		if d, err := time.ParseDuration(s); err == nil {
			return int64(d)
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}
