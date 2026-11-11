package compose

import (
	"context"
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
