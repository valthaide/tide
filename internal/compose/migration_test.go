package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestVerifyNoDataInAppDirRejectsPersistentData(t *testing.T) {
	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "compose.yaml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "compose.yaml")
	run("commit", "-m", "init")

	e := &ComposeExecutor{WorkDir: filepath.Dir(appDir)}

	// Clean checkout directory is allowed.
	if err := e.verifyNoDataInAppDir(context.Background(), appDir, "", "compose.yaml"); err != nil {
		t.Fatalf("clean app dir rejected: %v", err)
	}

	// Untracked persistent data inside checkout blocks removal.
	untracked := filepath.Join(repoDir, "local.sqlite")
	if err := os.WriteFile(untracked, []byte("db-data"), 0644); err != nil {
		t.Fatal(err)
	}
	err := e.verifyNoDataInAppDir(context.Background(), appDir, "", "compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "app_dir_contains_data") {
		t.Fatalf("expected untracked data error, got %v", err)
	}
	_ = os.Remove(untracked)

	// Extra data directory inside appDir (outside repo/.ssh) blocks removal.
	dataSubdir := filepath.Join(appDir, "pgdata")
	if err := os.MkdirAll(dataSubdir, 0755); err != nil {
		t.Fatal(err)
	}
	err = e.verifyNoDataInAppDir(context.Background(), appDir, "", "compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "app_dir_contains_data") {
		t.Fatalf("expected data subdir error, got %v", err)
	}
}

func TestCompareMigrationSpecsEquivalenceAndAllDiffAxes(t *testing.T) {
	const (
		apiDigest = "1111111111111111111111111111111111111111111111111111111111111111"
		dbDigest  = "2222222222222222222222222222222222222222222222222222222222222222"
		apiImage  = "registry.example/tenko/api:1.4.2@sha256:" + apiDigest
		dbImage   = "postgres:16-alpine@sha256:" + dbDigest
	)

	stagingRepo := t.TempDir()
	targetRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(stagingRepo, "app.conf"), []byte("listen=8080\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRepo, "app.conf"), []byte("listen=8080\n"), 0644); err != nil {
		t.Fatal(err)
	}

	renderedJSON := []byte(`{
		"name": "tenko-dev1",
		"services": {
			"api": {
				"image": "` + apiImage + `",
				"environment": {
					"APP_ENV": "dev",
					"DB_HOST": "postgres",
					"SECRET_KEY": "top-secret-dont-leak"
				},
				"ports": [
					{"published": "18080", "target": 8080, "protocol": "tcp"}
				],
				"volumes": [
					{"type": "volume", "source": "uploads", "target": "/var/lib/uploads", "read_only": false},
					{"type": "bind", "source": "` + filepath.ToSlash(filepath.Join(stagingRepo, "app.conf")) + `", "target": "/etc/app/app.conf", "read_only": true}
				],
				"networks": {"default": null, "backend": null},
				"labels": {
					"com.example.tenant": "dev1"
				}
			},
			"postgres": {
				"image": "` + dbImage + `",
				"environment": {
					"POSTGRES_DB": "tenko"
				},
				"volumes": [
					{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data", "read_only": false}
				],
				"networks": {"backend": null}
			}
		},
		"volumes": {
			"uploads": {"name": "tenko-dev1_uploads"},
			"pgdata": {"name": "tenko-dev1_pgdata"}
		},
		"networks": {
			"default": {"name": "tenko-dev1_default"},
			"backend": {"name": "tenko-dev1_backend"}
		}
	}`)

	desired, err := ParseRenderedComposeSpec(renderedJSON, "tenko-dev1", stagingRepo, targetRepo, nil)
	if err != nil {
		t.Fatalf("ParseRenderedComposeSpec: %v", err)
	}

	imageMap := map[string]fullImageInspect{}
	apiImgInspect := fullImageInspect{
		ID:          "sha256:aaaa",
		RepoDigests: []string{"registry.example/tenko/api@sha256:" + apiDigest},
		RepoTags:    []string{"registry.example/tenko/api:1.4.2"},
	}
	apiImgInspect.Config.Entrypoint = []string{"/entrypoint.sh"}
	apiImgInspect.Config.Cmd = []string{"serve", "--port=8080"}
	apiImgInspect.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "IMAGE_DEFAULT=1"}
	apiImgInspect.Config.Labels = map[string]string{"org.opencontainers.image.vendor": "tenko"}
	registerImageKeys(imageMap, apiImage, apiImgInspect)

	dbImgInspect := fullImageInspect{
		ID:          "sha256:bbbb",
		RepoDigests: []string{"docker.io/library/postgres@sha256:" + dbDigest},
		RepoTags:    []string{"postgres:16-alpine"},
	}
	dbImgInspect.Config.Entrypoint = []string{"docker-entrypoint.sh"}
	dbImgInspect.Config.Cmd = []string{"postgres"}
	dbImgInspect.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "PG_MAJOR=16"}
	registerImageKeys(imageMap, dbImage, dbImgInspect)

	makeContainers := func() []fullContainerInspect {
		var apiC, dbC fullContainerInspect
		apiC.ID = "c-api-111"
		apiC.Image = "sha256:aaaa"
		apiC.State.Status = "running"
		apiC.State.Running = true
		apiC.State.Health = &struct {
			Status string `json:"Status"`
		}{Status: "healthy"}
		apiC.Config.Image = apiImage
		apiC.Config.Entrypoint = []string{"/entrypoint.sh"}
		apiC.Config.Cmd = []string{"serve", "--port=8080"}
		apiC.Config.Env = []string{
			"APP_ENV=dev",
			"DB_HOST=postgres",
			"SECRET_KEY=top-secret-dont-leak",
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"IMAGE_DEFAULT=1",
		}
		apiC.Config.Labels = map[string]string{
			"com.docker.compose.project":             "tenko-dev1",
			"com.docker.compose.service":             "api",
			"com.docker.compose.project.working_dir": "/old/checkout",
			"com.docker.compose.config-hash":         "oldhash123",
			"com.example.tenant":                     "dev1",
			"org.opencontainers.image.vendor":        "tenko",
		}
		apiC.HostConfig.NetworkMode = "tenko-dev1_default"
		apiC.HostConfig.PortBindings = map[string][]dockerPortBinding{
			"8080/tcp": {
				{HostIP: "0.0.0.0", HostPort: "18080"},
				{HostIP: "::", HostPort: "18080"}, // IPv6 mirror ignored when 0.0.0.0 present
			},
		}
		apiC.Mounts = []dockerRuntimeMount{
			{Type: "volume", Name: "tenko-dev1_uploads", Destination: "/var/lib/uploads", RW: true},
			{Type: "bind", Source: filepath.Join(targetRepo, "app.conf"), Destination: "/etc/app/app.conf", RW: false},
		}
		apiC.NetworkSettings.Networks = map[string]struct {
			NetworkID string   `json:"NetworkID"`
			Aliases   []string `json:"Aliases"`
		}{
			"tenko-dev1_default": {NetworkID: "n1"},
			"tenko-dev1_backend": {NetworkID: "n2"},
		}

		dbC.ID = "c-db-222"
		dbC.Image = "sha256:bbbb"
		dbC.State.Status = "running"
		dbC.State.Running = true
		dbC.Config.Image = dbImage
		dbC.Config.Entrypoint = []string{"docker-entrypoint.sh"}
		dbC.Config.Cmd = []string{"postgres"}
		dbC.Config.Env = []string{
			"POSTGRES_DB=tenko",
			"PATH=/usr/local/bin:/usr/bin",
			"PG_MAJOR=16",
			"PGDATA=/var/lib/postgresql/data", // note: if image had PGDATA, let's keep exact match
		}
		// Keep dbC.Config.Env matching dbImgInspect + compose env
		dbC.Config.Env = []string{
			"POSTGRES_DB=tenko",
			"PATH=/usr/local/bin:/usr/bin",
			"PG_MAJOR=16",
		}
		dbC.Config.Labels = map[string]string{
			"com.docker.compose.project": "tenko-dev1",
			"com.docker.compose.service": "postgres",
		}
		dbC.HostConfig.NetworkMode = "tenko-dev1_backend"
		dbC.Mounts = []dockerRuntimeMount{
			{Type: "volume", Name: "tenko-dev1_pgdata", Destination: "/var/lib/postgresql/data", RW: true},
			// Anonymous image volume (64-char hex, not in HostConfig.Mounts) must not cause false diff
			{Type: "volume", Name: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Destination: "/tmp/anon", RW: true},
		}
		dbC.NetworkSettings.Networks = map[string]struct {
			NetworkID string   `json:"NetworkID"`
			Aliases   []string `json:"Aliases"`
		}{
			"tenko-dev1_backend": {NetworkID: "n2"},
		}
		return []fullContainerInspect{apiC, dbC}
	}

	netMap := map[string]string{
		"tenko-dev1_default": "tenko-dev1_default",
		"tenko-dev1_backend": "tenko-dev1_backend",
	}

	// 1. Identical config -> Equivalent == true, no_restart, config_identical.
	running, healthy, err := BuildRunningProjectSpec("tenko-dev1", targetRepo, makeContainers(), imageMap, netMap)
	if err != nil {
		t.Fatal(err)
	}
	res := CompareMigrationSpecs(desired, running, healthy, imageMap)
	if !res.Equivalent || res.Mode != MigrationModeNoRestart || res.Reason != "config_identical" {
		t.Fatalf("expected Equivalent no_restart, got %+v", res)
	}
	if !slices.Equal(res.UnchangedServices, []string{"api", "postgres"}) {
		t.Fatalf("unexpected unchanged services: %v", res.UnchangedServices)
	}

	// 2. Changed image digest on `api` -> only `api` in ChangedServices, `postgres` in UnchangedServices.
	desImg := cloneSpec(desired)
	apiSvc := desImg.Services["api"]
	apiSvc.Digest = "9999999999999999999999999999999999999999999999999999999999999999"
	desImg.Services["api"] = apiSvc
	res = CompareMigrationSpecs(desImg, running, healthy, imageMap)
	if res.Equivalent || res.Mode != MigrationModeRestarted || res.Reason != "image_changed" {
		t.Fatalf("expected image_changed restart, got %+v", res)
	}
	if !slices.Equal(res.ChangedServices, []string{"api"}) || !slices.Equal(res.UnchangedServices, []string{"postgres"}) {
		t.Fatalf("expected only api changed, got changed=%v unchanged=%v", res.ChangedServices, res.UnchangedServices)
	}

	// 3. Changed secret env value -> detected as env_changed without leaking secret value in DiffSummary!
	desEnv := cloneSpec(desired)
	apiSvc = desEnv.Services["api"]
	apiSvc.Environment = map[string]string{
		"APP_ENV":    "dev",
		"DB_HOST":    "postgres",
		"SECRET_KEY": "rotated-super-secret",
	}
	desEnv.Services["api"] = apiSvc
	res = CompareMigrationSpecs(desEnv, running, healthy, imageMap)
	if res.Equivalent || res.Reason != "env_changed" || !slices.Equal(res.ChangedServices, []string{"api"}) {
		t.Fatalf("expected env_changed on api, got %+v", res)
	}
	for _, line := range res.DiffSummary {
		if strings.Contains(line, "top-secret-dont-leak") || strings.Contains(line, "rotated-super-secret") {
			t.Fatalf("secret value leaked into diff summary: %s", line)
		}
	}

	// 4. Changed ports
	desPorts := cloneSpec(desired)
	apiSvc = desPorts.Services["api"]
	apiSvc.Ports = []CanonicalPort{{HostIP: "0.0.0.0", Published: 18081, Target: 8080, Protocol: "tcp"}}
	desPorts.Services["api"] = apiSvc
	res = CompareMigrationSpecs(desPorts, running, healthy, imageMap)
	if res.Equivalent || res.Reason != "ports_changed" {
		t.Fatalf("expected ports_changed, got %+v", res)
	}

	// 5. Changed bind-mounted file content inside new repo -> detected as volumes_changed!
	if err := os.WriteFile(filepath.Join(stagingRepo, "app.conf"), []byte("listen=9090\n"), 0644); err != nil {
		t.Fatal(err)
	}
	desiredNewBind, err := ParseRenderedComposeSpec(renderedJSON, "tenko-dev1", stagingRepo, targetRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	res = CompareMigrationSpecs(desiredNewBind, running, healthy, imageMap)
	if res.Equivalent || res.Reason != "volumes_changed" {
		t.Fatalf("expected volumes_changed when tracked bind file bytes differ, got %+v", res)
	}

	// 6. Changed networks
	desNet := cloneSpec(desired)
	apiSvc = desNet.Services["api"]
	apiSvc.Networks = []string{"tenko-dev1_default"}
	desNet.Services["api"] = apiSvc
	res = CompareMigrationSpecs(desNet, running, healthy, imageMap)
	if res.Equivalent || res.Reason != "networks_changed" {
		t.Fatalf("expected networks_changed, got %+v", res)
	}

	// 7. Changed command
	desCmd := cloneSpec(desired)
	apiSvc = desCmd.Services["api"]
	apiSvc.Command = []string{"serve", "--port=9000"}
	desCmd.Services["api"] = apiSvc
	res = CompareMigrationSpecs(desCmd, running, healthy, imageMap)
	if res.Equivalent || res.Reason != "command_changed" {
		t.Fatalf("expected command_changed, got %+v", res)
	}

	// 8. Changed labels
	desLbl := cloneSpec(desired)
	apiSvc = desLbl.Services["api"]
	apiSvc.Labels = map[string]string{"com.example.tenant": "dev2"}
	desLbl.Services["api"] = apiSvc
	res = CompareMigrationSpecs(desLbl, running, healthy, imageMap)
	if res.Equivalent || res.Reason != "labels_changed" {
		t.Fatalf("expected labels_changed, got %+v", res)
	}

	// 9. Unhealthy running container -> never skips restart even if config matches!
	res = CompareMigrationSpecs(desired, running, false, imageMap)
	if res.Equivalent || res.Mode != MigrationModeRestarted || res.Reason != "runtime_unhealthy" {
		t.Fatalf("expected unhealthy runtime to force restart, got %+v", res)
	}
}

func TestCommitRepoMigrationPreservesInodesAndSwapsGitWithoutComposeDown(t *testing.T) {
	workDir := t.TempDir()
	appID := "app-inode-test"
	appDir := filepath.Join(workDir, appID)
	oldRepo := filepath.Join(appDir, "repo")
	if err := os.MkdirAll(oldRepo, 0755); err != nil {
		t.Fatal(err)
	}

	initGit := func(dir, remoteURL, confText string) {
		t.Helper()
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
			}
		}
		run("init", "-b", "main")
		run("remote", "add", "origin", remoteURL)
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  api:\n    image: app:1\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app.conf"), []byte(confText), 0644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-m", "init")
	}

	initGit(oldRepo, "https://git.example/old/app.git", "same-config\n")

	confPath := filepath.Join(oldRepo, "app.conf")
	openHandle, err := os.Open(confPath)
	if err != nil {
		t.Fatal(err)
	}
	defer openHandle.Close()
	statBefore, err := os.Stat(confPath)
	if err != nil {
		t.Fatal(err)
	}

	stageDir := t.TempDir()
	stagedRepo := filepath.Join(stageDir, "repo")
	if err := os.MkdirAll(stagedRepo, 0755); err != nil {
		t.Fatal(err)
	}
	initGit(stagedRepo, "https://git.example/new/tenant-dev1.git", "updated-config\n")

	// Install a fake `docker` binary that fails the test if `compose down` is ever invoked!
	binDir := t.TempDir()
	dockerMarker := filepath.Join(binDir, "down-called")
	fakeDocker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"down\" ]; then\n    touch \"" + dockerMarker + "\"\n    exit 99\n  fi\ndone\nexit 0\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	e := &ComposeExecutor{WorkDir: workDir}
	prep := &MigrationPrepared{
		StagingDir:  stageDir,
		ProjectName: "tenko-dev1",
		Equivalent:  true,
		Mode:        MigrationModeNoRestart,
		Reason:      "config_identical",
	}

	if err := e.CommitRepoMigration(context.Background(), appID, "compose.yaml", nil, prep); err != nil {
		t.Fatalf("CommitRepoMigration failed: %v", err)
	}

	if _, err := os.Stat(dockerMarker); err == nil {
		t.Fatal("CommitRepoMigration invoked `docker compose down`, which must NEVER happen!")
	}

	statAfter, err := os.Stat(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(statBefore, statAfter) {
		t.Fatal("expected CommitRepoMigration to preserve regular file inode for live bind mounts")
	}

	out, err := exec.Command("git", "-C", oldRepo, "remote", "get-url", "origin").CombinedOutput()
	if err != nil {
		t.Fatalf("git remote get-url: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "https://git.example/new/tenant-dev1.git" {
		t.Fatalf("expected .git origin to point to new repo, got %q", got)
	}
}

const (
	dollarProject = "tenko-dev2"

	// Real values, as a container holds them.
	dollarCORS       = `^https://([a-z0-9-]+\.)?example\.com$`
	dollarOtherCORS  = `^https://([a-z0-9-]+\.)?example\.org$`
	dollarSecret     = "s3cr3t$with$dollars"
	dollarOtherSec   = "r0tated$secret"
	dollarLiteral    = "a$$b"
	dollarRedisShell = `exec redis-server --requirepass "$REDIS_PASSWORD"`
	dollarLabelKey   = "traefik.http.middlewares.strip.replacepathregex.replacement"
)

// escapeLikeCompose doubles every `$`, as `docker compose config` does with its
// interpolated output.
func escapeLikeCompose(doc []byte) []byte {
	return bytes.ReplaceAll(doc, []byte("$"), []byte("$$"))
}

// dollarFixture is a healthy three-service project whose images are referenced
// by tag only and whose values contain `$`. doc holds the real values and is
// escaped the way Compose prints it before it is parsed.
type dollarFixture struct {
	doc           map[string]any
	images        map[string]fullImageInspect // by service
	desiredImages map[string]fullImageInspect // as inspectDesiredImages builds it
	runningImages map[string]fullImageInspect // as fetchLiveProjectObjects builds it
	containers    map[string]*fullContainerInspect
	stagingRepo   string
	targetRepo    string
}

func newDollarFixture(t *testing.T) *dollarFixture {
	t.Helper()
	f := &dollarFixture{
		images:        map[string]fullImageInspect{},
		desiredImages: map[string]fullImageInspect{},
		runningImages: map[string]fullImageInspect{},
		containers:    map[string]*fullContainerInspect{},
		stagingRepo:   t.TempDir(),
		targetRepo:    t.TempDir(),
	}
	f.doc = map[string]any{
		"name": dollarProject,
		"services": map[string]any{
			"api": map[string]any{
				"image": "registry.example/tenko/api:1.4.2",
				"environment": map[string]any{
					"CORS_ALLOWED_ORIGIN_REGEX": dollarCORS,
					"SESSION_SECRET":            dollarSecret,
					"LITERAL_DOUBLE":            dollarLiteral,
				},
				"labels":   map[string]any{dollarLabelKey: "/$1"},
				"networks": map[string]any{"default": nil},
			},
			"redis": map[string]any{
				"image":    "redis:7.4-alpine",
				"command":  []any{"sh", "-c", dollarRedisShell},
				"networks": map[string]any{"default": nil},
			},
			"postgres": map[string]any{
				"image":       "postgres:16-alpine",
				"environment": map[string]any{"POSTGRES_DB": "tenko"},
				"networks":    map[string]any{"default": nil},
			},
		},
		"networks": map[string]any{"default": map[string]any{"name": dollarProject + "_default"}},
	}

	// api and redis were pulled from a registry. postgres was loaded from an
	// archive, so it has no RepoDigests and is known by its image ID only.
	api := fullImageInspect{
		ID:          "sha256:" + strings.Repeat("a", 64),
		RepoDigests: []string{"registry.example/tenko/api@sha256:" + strings.Repeat("1", 64)},
		RepoTags:    []string{"registry.example/tenko/api:1.4.2"},
	}
	api.Config.Entrypoint = []string{"/entrypoint.sh"}
	api.Config.Cmd = []string{"sh", "-c", "exec app --home $HOME"}
	api.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "IMAGE_DEFAULT=x$$y"}

	redis := fullImageInspect{
		ID:          "sha256:" + strings.Repeat("b", 64),
		RepoDigests: []string{"redis@sha256:" + strings.Repeat("2", 64)},
		RepoTags:    []string{"redis:7.4-alpine"},
	}
	redis.Config.Entrypoint = []string{"docker-entrypoint.sh"}
	redis.Config.Cmd = []string{"redis-server"}
	redis.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin"}

	postgres := fullImageInspect{
		ID:       "sha256:" + strings.Repeat("c", 64),
		RepoTags: []string{"postgres:16-alpine"},
	}
	postgres.Config.Entrypoint = []string{"docker-entrypoint.sh"}
	postgres.Config.Cmd = []string{"postgres"}
	postgres.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "PG_MAJOR=16"}

	f.addService("api", api, []string{"/entrypoint.sh"}, []string{"sh", "-c", "exec app --home $HOME"}, []string{
		"CORS_ALLOWED_ORIGIN_REGEX=" + dollarCORS,
		"SESSION_SECRET=" + dollarSecret,
		"LITERAL_DOUBLE=" + dollarLiteral,
		"PATH=/usr/local/bin:/usr/bin",
		"IMAGE_DEFAULT=x$$y",
	}, map[string]string{dollarLabelKey: "/$1"})
	f.addService("redis", redis, []string{"docker-entrypoint.sh"}, []string{"sh", "-c", dollarRedisShell}, []string{
		"PATH=/usr/local/bin:/usr/bin",
	}, nil)
	f.addService("postgres", postgres, []string{"docker-entrypoint.sh"}, []string{"postgres"}, []string{
		"POSTGRES_DB=tenko",
		"PATH=/usr/local/bin:/usr/bin",
		"PG_MAJOR=16",
	}, nil)
	return f
}

func (f *dollarFixture) service(name string) map[string]any {
	return f.doc["services"].(map[string]any)[name].(map[string]any)
}

func (f *dollarFixture) addService(name string, img fullImageInspect, entrypoint, cmd, env []string, userLabels map[string]string) {
	ref := f.service(name)["image"].(string)
	f.images[name] = img
	registerImageKeys(f.desiredImages, ref, img)

	c := &fullContainerInspect{}
	c.ID = "cid-" + name
	c.Image = img.ID
	c.State.Status = "running"
	c.State.Running = true
	c.Config.Image = ref
	c.Config.Entrypoint = entrypoint
	c.Config.Cmd = cmd
	c.Config.Env = env
	c.Config.Labels = map[string]string{
		"com.docker.compose.project": dollarProject,
		"com.docker.compose.service": name,
	}
	for k, v := range userLabels {
		c.Config.Labels[k] = v
	}
	c.HostConfig.NetworkMode = dollarProject + "_default"
	c.NetworkSettings.Networks = map[string]struct {
		NetworkID string   `json:"NetworkID"`
		Aliases   []string `json:"Aliases"`
	}{
		dollarProject + "_default": {NetworkID: "n1"},
	}
	f.containers[name] = c
	registerImageKeys(f.runningImages, c.Image, img)
	registerImageKeys(f.runningImages, c.Config.Image, img)
}

func (f *dollarFixture) setContainerEnv(t *testing.T, service, key, value string) {
	t.Helper()
	c := f.containers[service]
	for i, entry := range c.Config.Env {
		if strings.HasPrefix(entry, key+"=") {
			c.Config.Env[i] = key + "=" + value
			return
		}
	}
	t.Fatalf("container %s has no env %s", service, key)
}

// compare runs the same steps as PrepareRepoMigration: parse the escaped
// config with digests resolved from local image inspect, build the running
// spec from container inspect, compare.
func (f *dollarFixture) compare(t *testing.T) (CanonicalProjectSpec, MigrationDiffResult) {
	t.Helper()
	raw, err := json.Marshal(f.doc)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := ParseRenderedComposeSpec(escapeLikeCompose(raw), dollarProject, f.stagingRepo, f.targetRepo, func(ref string) string {
		return localImageDigest(f.desiredImages, ref)
	})
	if err != nil {
		t.Fatalf("ParseRenderedComposeSpec: %v", err)
	}
	var containers []fullContainerInspect
	for _, name := range []string{"api", "postgres", "redis"} {
		containers = append(containers, *f.containers[name])
	}
	netMap := map[string]string{dollarProject + "_default": dollarProject + "_default"}
	running, healthy, err := BuildRunningProjectSpec(dollarProject, f.targetRepo, containers, f.runningImages, netMap)
	if err != nil {
		t.Fatal(err)
	}
	if !healthy {
		t.Fatal("fixture containers must be healthy")
	}
	return desired, CompareMigrationSpecs(desired, running, healthy, f.desiredImages)
}

func requireIdentical(t *testing.T, res MigrationDiffResult) {
	t.Helper()
	if !res.Equivalent || res.Mode != MigrationModeNoRestart || res.Reason != "config_identical" {
		t.Fatalf("expected no_restart/config_identical, got %+v", res)
	}
	if len(res.ChangedServices) != 0 || len(res.RemovedServices) != 0 || len(res.DiffSummary) != 0 {
		t.Fatalf("identical config must report no changes, got %+v", res)
	}
	if !slices.Equal(res.UnchangedServices, []string{"api", "postgres", "redis"}) {
		t.Fatalf("unexpected unchanged services: %v", res.UnchangedServices)
	}
}

func requireChanged(t *testing.T, res MigrationDiffResult, reason string, services ...string) {
	t.Helper()
	if res.Equivalent || res.Mode != MigrationModeRestarted || res.Reason != reason {
		t.Fatalf("expected restarted/%s, got %+v", reason, res)
	}
	if !slices.Equal(res.ChangedServices, services) {
		t.Fatalf("expected changed services %v, got %v", services, res.ChangedServices)
	}
}

func requireNoValuesInSummary(t *testing.T, res MigrationDiffResult, values ...string) {
	t.Helper()
	for _, line := range res.DiffSummary {
		for _, value := range values {
			if strings.Contains(line, value) {
				t.Fatalf("value leaked into diff summary: %s", line)
			}
		}
	}
}

// Compose prints `$` as `$$` in `config` output. The same values in the
// containers must compare equal, and a real change must still be found.
func TestMigrationDiffComposeDollarEscaping(t *testing.T) {
	t.Run("same values are identical", func(t *testing.T) {
		f := newDollarFixture(t)
		desired, res := f.compare(t)
		requireIdentical(t, res)

		api := desired.Services["api"]
		if got := api.Environment["CORS_ALLOWED_ORIGIN_REGEX"]; got != dollarCORS {
			t.Fatalf("CORS value was not unescaped: %q", got)
		}
		if got := api.Labels[dollarLabelKey]; got != "/$1" {
			t.Fatalf("label value was not unescaped: %q", got)
		}
		if got := desired.Services["redis"].Command; !slices.Equal(got, []string{"sh", "-c", dollarRedisShell}) {
			t.Fatalf("redis command was not unescaped: %q", got)
		}
	})

	t.Run("literal double dollar stays double", func(t *testing.T) {
		f := newDollarFixture(t)
		desired, res := f.compare(t)
		requireIdentical(t, res)
		if got := desired.Services["api"].Environment["LITERAL_DOUBLE"]; got != dollarLiteral {
			t.Fatalf("literal $$ must survive, got %q", got)
		}

		// Config says a$b, the container holds a$$b.
		f.service("api")["environment"].(map[string]any)["LITERAL_DOUBLE"] = "a$b"
		_, res = f.compare(t)
		requireChanged(t, res, "env_changed", "api")
	})

	t.Run("container values and image defaults are compared as they are", func(t *testing.T) {
		// The image default is x$$y. A container holding x$y differs from it.
		f := newDollarFixture(t)
		f.setContainerEnv(t, "api", "IMAGE_DEFAULT", "x$y")
		_, res := f.compare(t)
		requireChanged(t, res, "env_changed", "api")

		// A container value with a real $$ is not collapsed either.
		f = newDollarFixture(t)
		f.setContainerEnv(t, "api", "SESSION_SECRET", "s3cr3t$$with$$dollars")
		_, res = f.compare(t)
		requireChanged(t, res, "env_changed", "api")
	})

	t.Run("changed env value is found and not printed", func(t *testing.T) {
		f := newDollarFixture(t)
		f.setContainerEnv(t, "api", "CORS_ALLOWED_ORIGIN_REGEX", dollarOtherCORS)
		f.setContainerEnv(t, "api", "SESSION_SECRET", dollarOtherSec)
		_, res := f.compare(t)
		requireChanged(t, res, "env_changed", "api")
		summary := strings.Join(res.DiffSummary, "\n")
		if !strings.Contains(summary, "CORS_ALLOWED_ORIGIN_REGEX") || !strings.Contains(summary, "SESSION_SECRET") {
			t.Fatalf("changed keys missing from diff summary: %s", summary)
		}
		requireNoValuesInSummary(t, res, dollarCORS, dollarOtherCORS, dollarSecret, dollarOtherSec, "a-z0-9", "s3cr3t", "r0tated")
	})

	t.Run("changed command is found", func(t *testing.T) {
		f := newDollarFixture(t)
		f.containers["redis"].Config.Cmd = []string{"sh", "-c", `exec redis-server --requirepass "$OTHER_PASSWORD"`}
		_, res := f.compare(t)
		requireChanged(t, res, "command_changed", "redis")
		requireNoValuesInSummary(t, res, "requirepass", "PASSWORD")
	})

	t.Run("changed entrypoint is found", func(t *testing.T) {
		f := newDollarFixture(t)
		f.service("api")["entrypoint"] = []any{"/other-entrypoint.sh"}
		_, res := f.compare(t)
		requireChanged(t, res, "command_changed", "api")
	})

	t.Run("changed label is found", func(t *testing.T) {
		f := newDollarFixture(t)
		f.containers["api"].Config.Labels[dollarLabelKey] = "/$2"
		_, res := f.compare(t)
		requireChanged(t, res, "labels_changed", "api")
	})
}

// An image referenced by tag has no digest in the config. Its digest comes
// from local image inspect and must reach the comparison.
func TestMigrationDiffTagOnlyImages(t *testing.T) {
	t.Run("same local image is identical", func(t *testing.T) {
		f := newDollarFixture(t)
		desired, res := f.compare(t)
		requireIdentical(t, res)

		want := map[string]string{
			"api":      strings.Repeat("1", 64), // RepoDigest
			"redis":    strings.Repeat("2", 64), // RepoDigest
			"postgres": strings.Repeat("c", 64), // image ID, no RepoDigests
		}
		for name, digest := range want {
			if got := desired.Services[name].Digest; got != digest {
				t.Fatalf("service %s: digest %q, want %q", name, got, digest)
			}
		}
	})

	t.Run("tag moved to another local image", func(t *testing.T) {
		f := newDollarFixture(t)
		moved := f.images["redis"]
		moved.ID = "sha256:" + strings.Repeat("d", 64)
		moved.RepoDigests = []string{"redis@sha256:" + strings.Repeat("3", 64)}
		registerImageKeys(f.desiredImages, "redis:7.4-alpine", moved)
		_, res := f.compare(t)
		requireChanged(t, res, "image_changed", "redis")
	})

	t.Run("tag changed in compose", func(t *testing.T) {
		f := newDollarFixture(t)
		next := f.images["redis"]
		next.ID = "sha256:" + strings.Repeat("e", 64)
		next.RepoDigests = []string{"redis@sha256:" + strings.Repeat("4", 64)}
		next.RepoTags = []string{"redis:8.0-alpine"}
		f.service("redis")["image"] = "redis:8.0-alpine"
		registerImageKeys(f.desiredImages, "redis:8.0-alpine", next)
		_, res := f.compare(t)
		requireChanged(t, res, "image_changed", "redis")
	})

	t.Run("image missing locally is not treated as the same", func(t *testing.T) {
		f := newDollarFixture(t)
		f.desiredImages = map[string]fullImageInspect{}
		registerImageKeys(f.desiredImages, "registry.example/tenko/api:1.4.2", f.images["api"])
		registerImageKeys(f.desiredImages, "redis:7.4-alpine", f.images["redis"])
		desired, res := f.compare(t)
		if got := desired.Services["postgres"].Digest; got != "" {
			t.Fatalf("image without local inspect must have no digest, got %q", got)
		}
		if res.Equivalent || !slices.Equal(res.ChangedServices, []string{"postgres"}) {
			t.Fatalf("expected only postgres changed, got %+v", res)
		}
		if !strings.Contains(strings.Join(res.DiffSummary, "\n"), "has no resolved digest") {
			t.Fatalf("expected unresolved digest in summary, got %v", res.DiffSummary)
		}
	})
}

func TestUnescapeComposeDollars(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no dollars", `{"a":"b"}`, `{"a":"b"}`},
		{"single escaped dollar", `{"a":"x$$y"}`, `{"a":"x$y"}`},
		{"escaped variable syntax", `{"a":"$${HOME}"}`, `{"a":"${HOME}"}`},
		{"literal double dollar", `{"a":"x$$$$y"}`, `{"a":"x$$y"}`},
		{"dollar at end of value", `{"a":"^x$$"}`, `{"a":"^x$"}`},
		{"several values", `{"a":"$$1","b":["sh","-c","echo $$A $$B"]}`, `{"a":"$1","b":["sh","-c","echo $A $B"]}`},
		{"unescaped document is left alone", `{"a":"x$y","b":"p$$q"}`, `{"a":"x$y","b":"p$$q"}`},
		{"odd run is left alone", `{"a":"x$$$y"}`, `{"a":"x$$$y"}`},
		{"odd run at the end is left alone", `x$$ y$`, `x$$ y$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(unescapeComposeDollars([]byte(tc.in))); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNormalizeDigestHex(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	cases := []struct{ name, in, want string }{
		{"bare hex", hex64, hex64},
		{"bare hex upper case", strings.ToUpper(hex64), hex64},
		{"image id", "sha256:" + hex64, hex64},
		{"repo digest", "registry.example/app@sha256:" + hex64, hex64},
		{"empty", "", ""},
		{"tag", "postgres:16-alpine", ""},
		{"short hex", "abcdef", ""},
		{"not hex", strings.Repeat("zz", 32), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeDigestHex(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLocalImageDigest(t *testing.T) {
	withRepoDigest := fullImageInspect{
		ID:          "sha256:" + strings.Repeat("a", 64),
		RepoDigests: []string{"redis@sha256:" + strings.Repeat("2", 64)},
		RepoTags:    []string{"redis:7.4-alpine"},
	}
	idOnly := fullImageInspect{ID: "sha256:" + strings.Repeat("c", 64), RepoTags: []string{"app:dev"}}
	images := map[string]fullImageInspect{}
	registerImageKeys(images, "redis:7.4-alpine", withRepoDigest)
	registerImageKeys(images, "app:dev", idOnly)

	cases := []struct{ name, ref, want string }{
		{"repo digest wins", "redis:7.4-alpine", strings.Repeat("2", 64)},
		{"same image under its full name", "docker.io/library/redis:7.4-alpine", strings.Repeat("2", 64)},
		{"image id when there is no repo digest", "app:dev", strings.Repeat("c", 64)},
		{"unknown image", "nginx:1.27", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := localImageDigest(images, tc.ref)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			// The value must survive the hand-over to ParseRenderedComposeSpec.
			if normalizeDigestHex(got) != tc.want {
				t.Fatalf("digest %q is dropped by normalizeDigestHex", got)
			}
		})
	}
}

func cloneSpec(in CanonicalProjectSpec) CanonicalProjectSpec {
	out := CanonicalProjectSpec{
		Project:  in.Project,
		Services: make(map[string]CanonicalServiceSpec, len(in.Services)),
	}
	for k, v := range in.Services {
		cp := v
		cp.Command = append([]string(nil), v.Command...)
		cp.Entrypoint = append([]string(nil), v.Entrypoint...)
		cp.Ports = append([]CanonicalPort(nil), v.Ports...)
		cp.Volumes = append([]CanonicalMount(nil), v.Volumes...)
		cp.Networks = append([]string(nil), v.Networks...)
		if v.Environment != nil {
			cp.Environment = make(map[string]string, len(v.Environment))
			for ek, ev := range v.Environment {
				cp.Environment[ek] = ev
			}
		}
		if v.Labels != nil {
			cp.Labels = make(map[string]string, len(v.Labels))
			for lk, lv := range v.Labels {
				cp.Labels[lk] = lv
			}
		}
		out.Services[k] = cp
	}
	return out
}
