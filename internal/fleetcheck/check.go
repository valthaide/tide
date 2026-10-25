package fleetcheck

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Issue is one fleet or compose validation problem.
type Issue struct {
	App     string `json:"app,omitempty"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

func (i Issue) Error() string {
	if i.Message != "" {
		return i.Message
	}
	return i.Rule
}

// CheckProject reports a problem with an explicitly configured compose project name.
// Empty means the name is derived from the app id and is not an error.
// Underscores are allowed. Uppercase and length over 63 are not.
func CheckProject(raw string) *Issue {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, r := range raw {
		if unicode.IsUpper(r) {
			return &Issue{Rule: "project_invalid", Message: "project_invalid: uppercase"}
		}
	}
	if utf8.RuneCountInString(raw) > 63 {
		return &Issue{Rule: "project_invalid", Message: "project_invalid: longer than 63"}
	}
	return nil
}

// AppName is the minimum identity needed for host-level checks.
type AppName struct {
	Name    string
	Project string
}

// ProjectConflicts returns one issue per app that shares a compose project or a name.
func ProjectConflicts(apps []AppName) []Issue {
	byProject := map[string][]string{}
	byName := map[string][]string{}
	for _, app := range apps {
		name := strings.TrimSpace(app.Name)
		if name == "" {
			continue
		}
		byName[strings.ToLower(name)] = append(byName[strings.ToLower(name)], name)
		project := strings.TrimSpace(app.Project)
		if project == "" {
			continue
		}
		byProject[project] = append(byProject[project], name)
	}
	var out []Issue
	for _, names := range byProject {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		msg := "validation_failed: project_conflict " + strings.Join(names, " ")
		for _, name := range names {
			out = append(out, Issue{App: name, Rule: "project_conflict", Message: msg})
		}
	}
	for _, names := range byName {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		msg := "validation_failed: name_conflict " + strings.Join(names, " ")
		for _, name := range names {
			out = append(out, Issue{App: name, Rule: "name_conflict", Message: msg})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App == out[j].App {
			return out[i].Rule < out[j].Rule
		}
		return out[i].App < out[j].App
	})
	return out
}

// ParseDuration checks a fleet duration field. Empty is valid.
func ParseDuration(raw string) (time.Duration, *Issue) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, &Issue{Rule: "invalid_duration", Message: "invalid_duration: " + raw}
	}
	return d, nil
}

// Summary is the part of `docker compose config` that validation needs.
type Summary struct {
	Images        map[string]string
	Published     []int
	ContainerName []string
	ExternalVols  []string
	BindSources   []string
	Rerun         []string
	Build         []string
}

// SummarizeCompose reads compose config JSON.
func SummarizeCompose(raw []byte) (Summary, error) {
	var doc struct {
		Services map[string]struct {
			Image         string            `json:"image"`
			Build         json.RawMessage   `json:"build"`
			ContainerName string            `json:"container_name"`
			Ports         []composePort     `json:"ports"`
			Volumes       []composeVolume   `json:"volumes"`
			Labels        map[string]string `json:"labels"`
		} `json:"services"`
		Volumes map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Summary{}, err
	}
	sum := Summary{Images: map[string]string{}}
	for name, svc := range doc.Services {
		if img := strings.TrimSpace(svc.Image); img != "" {
			sum.Images[name] = img
		}
		if hasBuild(svc.Build) {
			sum.Build = append(sum.Build, name)
		}
		if cn := strings.TrimSpace(svc.ContainerName); cn != "" {
			sum.ContainerName = append(sum.ContainerName, name)
		}
		for _, p := range svc.Ports {
			if port := p.HostPort(); port > 0 {
				sum.Published = append(sum.Published, port)
			}
		}
		for _, v := range svc.Volumes {
			if strings.EqualFold(v.Type, "bind") && strings.TrimSpace(v.Source) != "" {
				sum.BindSources = append(sum.BindSources, v.Source)
			}
		}
		if strings.EqualFold(strings.TrimSpace(svc.Labels["com.conops.oneshot"]), "rerun") {
			sum.Rerun = append(sum.Rerun, name)
		}
	}
	for _, vol := range doc.Volumes {
		if vol.External {
			name := strings.TrimSpace(vol.Name)
			if name != "" {
				sum.ExternalVols = append(sum.ExternalVols, name)
			}
		}
	}
	sort.Ints(sum.Published)
	sort.Strings(sum.ContainerName)
	sort.Strings(sum.ExternalVols)
	sort.Strings(sum.BindSources)
	sort.Strings(sum.Rerun)
	sort.Strings(sum.Build)
	return sum, nil
}

func hasBuild(raw json.RawMessage) bool {
	raw = bytesTrim(raw)
	return len(raw) > 0 && string(raw) != "null"
}

func bytesTrim(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

type composePort struct {
	Published any    `json:"published"`
	Target    any    `json:"target"`
	Protocol  string `json:"protocol"`
}

func (p composePort) HostPort() int {
	switch v := p.Published.(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}

type composeVolume struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// DigestRequired lists services whose image has no @sha256: pin.
func DigestRequired(images map[string]string) []Issue {
	var names []string
	for svc, image := range images {
		if strings.TrimSpace(image) == "" {
			continue
		}
		if !strings.Contains(image, "@sha256:") {
			names = append(names, svc)
		}
	}
	sort.Strings(names)
	var out []Issue
	for _, name := range names {
		out = append(out, Issue{Rule: "digest_required", Message: "digest_required: " + name})
	}
	return out
}

// ImageRefOf returns the digest from repo:tag@sha256:hex, or empty.
func ImageDigest(ref string) string {
	i := strings.LastIndex(ref, "@sha256:")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(ref[i+1:])
}

// BindViolations returns absolute bind sources outside the allowed prefixes.
// Empty prefixes allow every path. Relative sources are always allowed.
func BindViolations(sources, prefixes []string) []Issue {
	if len(prefixes) == 0 {
		return nil
	}
	var clean []string
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p != "" {
			clean = append(clean, p)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	var out []Issue
	for _, src := range sources {
		src = strings.TrimSpace(src)
		if src == "" || !strings.HasPrefix(src, "/") {
			continue
		}
		ok := false
		for _, p := range clean {
			if src == p || strings.HasPrefix(src, strings.TrimRight(p, "/")+"/") {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, Issue{Rule: "bind_prefix", Message: "policy_violation: bind " + src})
		}
	}
	return out
}

// PortConflict reports when port is already owned by someone else.
func PortConflict(app string, port int, owner string) Issue {
	return Issue{
		App:     app,
		Rule:    "port_conflict",
		Message: fmt.Sprintf("port_conflict: %s %d used by %s", app, port, owner),
	}
}

// PublicVar reports whether a variable may be shown in status and metrics.
// Keys listed in publicVars, and keys ending in _TAG or _IMAGE, are public.
// Nothing else is. Secret-looking names are never public.
func PublicVar(key string, publicVars []string) bool {
	key = strings.TrimSpace(key)
	if key == "" || secretKey(key) {
		return false
	}
	for _, name := range publicVars {
		if key == strings.TrimSpace(name) {
			return true
		}
	}
	upper := strings.ToUpper(key)
	return strings.HasSuffix(upper, "_TAG") || strings.HasSuffix(upper, "_IMAGE")
}

// PublicVars keeps only values that may be shown. Secrets are dropped.
func PublicVars(vars map[string]string, listed []string) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	out := map[string]string{}
	for key, value := range vars {
		if PublicVar(key, listed) {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func secretKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, part := range []string{"PASSWORD", "SECRET", "TOKEN", "KEY"} {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}
