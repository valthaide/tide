package compose

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func planService(p *AppPlan, name string) *ServicePlan {
	for i := range p.Services {
		if p.Services[i].Service == name {
			return &p.Services[i]
		}
	}
	return nil
}

func TestBuildAppPlanActions(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{"web":{"image":"nginx:1"},"new":{"image":"redis:7"}}}`)
	desired := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"web": {Image: "library/nginx:1", Digest: "aaa"},
		"new": {Image: "library/redis:7", Digest: "bbb"},
	}}
	running := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"web": {Image: "library/nginx:1", Digest: "ccc"},
		"old": {Image: "library/busybox:1", Digest: "ddd"},
	}}

	plan := BuildAppPlan(configJSON, desired, running, nil, PlanInput{})

	web := planService(plan, "web")
	if web == nil || web.Action != PlanActionRecreate {
		t.Fatalf("web=%+v, want recreate", web)
	}
	if !strings.Contains(strings.Join(web.Reasons, ";"), "image digest changed") {
		t.Fatalf("web reasons=%v, want image digest change", web.Reasons)
	}
	if sp := planService(plan, "new"); sp == nil || sp.Action != PlanActionCreate {
		t.Fatalf("new=%+v, want create", sp)
	}
	if sp := planService(plan, "old"); sp == nil || sp.Action != PlanActionRemove {
		t.Fatalf("old=%+v, want remove", sp)
	}
}

func TestBuildAppPlanNoChange(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{"web":{"image":"nginx:1"}}}`)
	spec := CanonicalServiceSpec{Image: "library/nginx:1", Digest: "aaa"}
	desired := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{"web": spec}}
	running := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{"web": spec}}

	plan := BuildAppPlan(configJSON, desired, running, nil, PlanInput{})
	sp := planService(plan, "web")
	if sp == nil || sp.Action != PlanActionNoop || len(sp.Reasons) != 0 {
		t.Fatalf("web=%+v, want no_change without reasons", sp)
	}
}

func TestBuildAppPlanEnvFileScopedRecreate(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{
		"app":{"image":"app:1","env_file":[{"path":"/etc/app/secrets.env"}]},
		"side":{"image":"side:1"}
	}}`)
	spec := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"app":  {Image: "app:1", Digest: "aaa"},
		"side": {Image: "side:1", Digest: "bbb"},
	}}

	plan := BuildAppPlan(configJSON, spec, spec, nil, PlanInput{EnvChangedPaths: []string{"/etc/app/secrets.env"}})

	app := planService(plan, "app")
	if app == nil || app.Action != PlanActionRecreate {
		t.Fatalf("app=%+v, want recreate", app)
	}
	joined := strings.Join(app.Reasons, ";")
	if !strings.Contains(joined, "env_file changed") || !strings.Contains(joined, "secrets.env") {
		t.Fatalf("app reasons=%v, want env_file change with path", app.Reasons)
	}
	if sp := planService(plan, "side"); sp == nil || sp.Action != PlanActionNoop {
		t.Fatalf("side=%+v, want no_change", sp)
	}
	if plan.ForceRecreateAll {
		t.Fatal("ForceRecreateAll must stay false when every path maps to a service")
	}
}

func TestBuildAppPlanEnvFileUnmappedForcesAll(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{
		"app":{"image":"app:1","env_file":["/etc/app/a.env"]},
		"side":{"image":"side:1"}
	}}`)
	spec := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"app":  {Image: "app:1", Digest: "aaa"},
		"side": {Image: "side:1", Digest: "bbb"},
	}}

	plan := BuildAppPlan(configJSON, spec, spec, nil, PlanInput{EnvChangedPaths: []string{"/etc/other.env"}})

	if !plan.ForceRecreateAll {
		t.Fatal("ForceRecreateAll must be true when a changed env_file maps to no service")
	}
	for _, name := range []string{"app", "side"} {
		sp := planService(plan, name)
		if sp == nil || sp.Action != PlanActionRecreate {
			t.Fatalf("%s=%+v, want recreate", name, sp)
		}
		if !strings.Contains(strings.Join(sp.Reasons, ";"), "force recreate") {
			t.Fatalf("%s reasons=%v, want force recreate reason", name, sp.Reasons)
		}
	}
}

func TestBuildAppPlanWatchActions(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{
		"nginx":{"image":"nginx:1"},
		"prom":{"image":"prom:1"}
	}}`)
	spec := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"nginx": {Image: "nginx:1", Digest: "aaa"},
		"prom":  {Image: "prom:1", Digest: "bbb"},
	}}
	in := PlanInput{WatchChanged: []WatchPlanEntry{
		{Path: "templates/backend/nginx/default.conf", Service: "nginx", Action: "restart"},
		{Path: "templates/backend/prom/rules.yaml", Service: "prom", Action: "reload"},
	}}

	plan := BuildAppPlan(configJSON, spec, spec, nil, in)

	nginx := planService(plan, "nginx")
	if nginx == nil || nginx.Action != PlanActionRestart {
		t.Fatalf("nginx=%+v, want restart", nginx)
	}
	if !strings.Contains(strings.Join(nginx.Reasons, ";"), "watch_files changed") {
		t.Fatalf("nginx reasons=%v, want watch_files reason", nginx.Reasons)
	}
	// A reload happens in place: the container is not restarted.
	if sp := planService(plan, "prom"); sp == nil || sp.Action != PlanActionNoop {
		t.Fatalf("prom=%+v, want no_change", sp)
	}
	if len(plan.WatchActions) != 2 {
		t.Fatalf("WatchActions=%v, want both entries", plan.WatchActions)
	}
}

func TestBuildAppPlanHealthcheck(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{"web":{"image":"nginx:1"}}}`)
	hc := &CanonicalHealthcheck{Test: []string{"CMD-SHELL", "curl -f http://localhost/"}, Interval: int64(30 * time.Second)}
	desired := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"web": {Image: "nginx:1", Digest: "aaa", Healthcheck: hc},
	}}
	running := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"web": {Image: "nginx:1", Digest: "aaa"},
	}}

	plan := BuildAppPlan(configJSON, desired, running, nil, PlanInput{})
	sp := planService(plan, "web")
	if sp == nil || sp.Action != PlanActionRecreate {
		t.Fatalf("web=%+v, want recreate on healthcheck change", sp)
	}
	found := false
	for _, r := range sp.Reasons {
		if strings.Contains(r, "healthcheck") {
			found = true
		}
	}
	if !found {
		t.Fatalf("web reasons=%v, want a healthcheck reason", sp.Reasons)
	}
}

func TestBuildAppPlanVolumesToCreate(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{"db":{"image":"pg:16"}},"volumes":{"data":{},"ext":{"name":"shared","external":true}}}`)
	desired := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"db": {Image: "pg:16", Digest: "aaa", Volumes: []CanonicalMount{
			{Type: "volume", Source: "proj_data", Target: "/var/lib/postgresql/data"},
			{Type: "volume", Source: "shared", Target: "/shared"},
		}},
	}}
	running := CanonicalProjectSpec{Project: "proj"}

	plan := BuildAppPlan(configJSON, desired, running, nil, PlanInput{ExistingVolumes: map[string]bool{"proj_data": true}})
	if len(plan.VolumesToCreate) != 1 || plan.VolumesToCreate[0].Name != "shared" || !plan.VolumesToCreate[0].External {
		t.Fatalf("VolumesToCreate=%+v, want only external shared", plan.VolumesToCreate)
	}
}

func TestBuildAppPlanOneshotRerunOnlyWhenApplying(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{
		"migrate":{"image":"app:1","labels":{"com.tide.oneshot":"rerun"}},
		"web":{"image":"app:1"}
	}}`)
	spec := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"migrate": {Image: "app:1", Digest: "aaa", Oneshot: true},
		"web":     {Image: "app:1", Digest: "aaa"},
	}}

	quiet := BuildAppPlan(configJSON, spec, spec, nil, PlanInput{})
	if sp := planService(quiet, "migrate"); sp == nil || sp.Action != PlanActionNoop {
		t.Fatalf("quiet migrate=%+v, want no_change without an apply", sp)
	}

	changed := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"migrate": {Image: "app:1", Digest: "aaa", Oneshot: true},
		"web":     {Image: "app:1", Digest: "newdigest"},
	}}
	applying := BuildAppPlan(configJSON, changed, spec, nil, PlanInput{})
	if sp := planService(applying, "web"); sp == nil || sp.Action != PlanActionRecreate {
		t.Fatalf("applying web=%+v, want recreate", sp)
	}
	if sp := planService(applying, "migrate"); sp == nil || sp.Action != PlanActionRestart {
		t.Fatalf("applying migrate=%+v, want restart on apply", sp)
	}
}

func TestBuildAppPlanNetworkNamespaceDependent(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{
		"php":{"image":"php:8"},
		"sidecar":{"image":"side:1","network_mode":"service:php"}
	}}`)
	desired := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"php":     {Image: "php:8", Digest: "new"},
		"sidecar": {Image: "side:1", Digest: "aaa", NetworkMode: "service:php"},
	}}
	running := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"php":     {Image: "php:8", Digest: "old"},
		"sidecar": {Image: "side:1", Digest: "aaa", NetworkMode: "service:php"},
	}}

	plan := BuildAppPlan(configJSON, desired, running, nil, PlanInput{})
	sp := planService(plan, "sidecar")
	if sp == nil || sp.Action != PlanActionRecreate {
		t.Fatalf("sidecar=%+v, want recreate with its namespace target", sp)
	}
	if !strings.Contains(strings.Join(sp.Reasons, ";"), "network namespace") {
		t.Fatalf("sidecar reasons=%v, want network namespace reason", sp.Reasons)
	}
}

func TestBuildAppPlanNeverLeaksEnvValues(t *testing.T) {
	configJSON := []byte(`{"name":"proj","services":{"app":{"image":"app:1"}}}`)
	desired := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"app": {Image: "app:1", Digest: "aaa", Environment: map[string]string{"PASSWORD": "new-super-secret"}},
	}}
	running := CanonicalProjectSpec{Project: "proj", Services: map[string]CanonicalServiceSpec{
		"app": {Image: "app:1", Digest: "aaa", Environment: map[string]string{"PASSWORD": "old-super-secret"}},
	}}

	plan := BuildAppPlan(configJSON, desired, running, nil, PlanInput{})
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if strings.Contains(out, "new-super-secret") || strings.Contains(out, "old-super-secret") {
		t.Fatalf("plan leaks env values: %s", out)
	}
	if !strings.Contains(out, "PASSWORD") {
		t.Fatalf("plan must name the changed key: %s", out)
	}
}

func TestParseRenderedHealthcheck(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		hc := parseRenderedHealthcheck(json.RawMessage(`{"test":["CMD-SHELL","curl -f x"],"interval":"30s","timeout":"5s","start_period":"1m30s","retries":3}`))
		if hc == nil {
			t.Fatal("nil healthcheck")
		}
		if len(hc.Test) != 2 || hc.Test[1] != "curl -f x" {
			t.Fatalf("Test=%v", hc.Test)
		}
		if hc.Interval != int64(30*time.Second) || hc.Timeout != int64(5*time.Second) || hc.StartPeriod != int64(90*time.Second) || hc.Retries != 3 {
			t.Fatalf("hc=%+v", hc)
		}
	})
	t.Run("string test becomes CMD-SHELL", func(t *testing.T) {
		hc := parseRenderedHealthcheck(json.RawMessage(`{"test":"curl -f x"}`))
		if hc == nil || len(hc.Test) != 2 || hc.Test[0] != "CMD-SHELL" {
			t.Fatalf("hc=%+v", hc)
		}
	})
	t.Run("disable normalizes to nil", func(t *testing.T) {
		if hc := parseRenderedHealthcheck(json.RawMessage(`{"disable":true}`)); hc != nil {
			t.Fatalf("hc=%+v, want nil", hc)
		}
	})
	t.Run("NONE normalizes to nil", func(t *testing.T) {
		if hc := parseRenderedHealthcheck(json.RawMessage(`{"test":["NONE"]}`)); hc != nil {
			t.Fatalf("hc=%+v, want nil", hc)
		}
	})
	t.Run("numeric durations", func(t *testing.T) {
		hc := parseRenderedHealthcheck(json.RawMessage(`{"test":["CMD","true"],"interval":30000000000}`))
		if hc == nil || hc.Interval != int64(30*time.Second) {
			t.Fatalf("hc=%+v", hc)
		}
	})
	t.Run("null", func(t *testing.T) {
		if hc := parseRenderedHealthcheck(json.RawMessage(`null`)); hc != nil {
			t.Fatalf("hc=%+v, want nil", hc)
		}
	})
}

func TestCanonicalHealthcheckFromInspect(t *testing.T) {
	if got := canonicalHealthcheck(nil); got != nil {
		t.Fatalf("nil in, got %+v", got)
	}
	if got := canonicalHealthcheck(&dockerHealthcheck{Test: []string{"NONE"}}); got != nil {
		t.Fatalf("disabled in, got %+v", got)
	}
	got := canonicalHealthcheck(&dockerHealthcheck{Test: []string{"CMD-SHELL", "x"}, Interval: int64(10 * time.Second)})
	if got == nil || got.Interval != int64(10*time.Second) {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeServiceWithImageDefaultsInheritsHealthcheck(t *testing.T) {
	imgHC := &dockerHealthcheck{Test: []string{"CMD-SHELL", "pg_isready"}, Interval: int64(10 * time.Second)}
	images := map[string]fullImageInspect{"db:16": {ID: "sha256:ff", RepoTags: []string{"db:16"}, Config: struct {
		Cmd         []string           `json:"Cmd"`
		Entrypoint  []string           `json:"Entrypoint"`
		Env         []string           `json:"Env"`
		Labels      map[string]string  `json:"Labels"`
		Healthcheck *dockerHealthcheck `json:"Healthcheck"`
	}{Healthcheck: imgHC}}}

	merged := mergeServiceWithImageDefaults(CanonicalServiceSpec{Image: "db:16"}, images)
	if merged.Healthcheck == nil || len(merged.Healthcheck.Test) != 2 {
		t.Fatalf("Healthcheck=%+v, want inherited from image", merged.Healthcheck)
	}

	own := &CanonicalHealthcheck{Test: []string{"CMD", "true"}}
	kept := mergeServiceWithImageDefaults(CanonicalServiceSpec{Image: "db:16", Healthcheck: own}, images)
	if kept.Healthcheck != own {
		t.Fatalf("Healthcheck=%+v, want the service's own", kept.Healthcheck)
	}
}

func TestBuildRunningProjectSpecMapsNetworkModeToService(t *testing.T) {
	mkContainer := func(id, service, netmode string) fullContainerInspect {
		var c fullContainerInspect
		c.ID = id
		c.State.Running = true
		c.State.Status = "running"
		c.Config.Labels = map[string]string{"com.docker.compose.service": service}
		c.HostConfig.NetworkMode = netmode
		return c
	}
	containers := []fullContainerInspect{
		mkContainer("abcdef123456", "db", ""),
		mkContainer("ffee00112233", "app", "container:abcdef123456"),
	}
	spec, _, err := BuildRunningProjectSpec("proj", "", containers, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Services["app"].NetworkMode; got != "service:db" {
		t.Fatalf("NetworkMode=%q, want service:db", got)
	}
}
