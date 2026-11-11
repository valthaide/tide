package compose

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestEndToEndExecutorMigrationNoRestartThenTargetedServiceRecreate verifies the full
// PrepareRepoMigration + CommitRepoMigration + ApplyImages pipeline against a simulated
// Docker Engine HTTP API and `docker` CLI:
//  1. Moving an app to a new Git repository with the exact same compose & digest pins
//     leaves container IDs and StartedAt timestamps untouched (`no_restart`, `config_identical`),
//     never invokes `compose down` or `compose up`.
//  2. Moving to a source with an updated image digest on `api` while `postgres` stays unchanged
//     runs `compose up -d --remove-orphans` WITHOUT `compose down`, recreating only `api`
//     while `postgres` keeps its original container ID and `StartedAt`.
func TestEndToEndExecutorMigrationNoRestartThenTargetedServiceRecreate(t *testing.T) {
	const (
		projectName = "tenko-dev1"
		apiDigestV1 = "1111111111111111111111111111111111111111111111111111111111111111"
		apiDigestV2 = "9999999999999999999999999999999999999999999999999999999999999999"
		dbDigest    = "2222222222222222222222222222222222222222222222222222222222222222"
		apiImgV1    = "registry.example/tenko/api:1.0.0@sha256:" + apiDigestV1
		apiImgV2    = "registry.example/tenko/api:1.1.0@sha256:" + apiDigestV2
		dbImg       = "postgres:16-alpine@sha256:" + dbDigest
	)

	var mu sync.Mutex
	apiContainerID := "cid-api-initial-001"
	apiStartedAt := "2026-11-11T10:00:00.000000000Z"
	apiRunningImg := apiImgV1
	apiImageID := "sha256:" + apiDigestV1

	dbContainerID := "cid-postgres-initial-001"
	dbStartedAt := "2026-11-11T10:00:00.000000000Z"

	// Simulated Docker Engine HTTP server
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]engineContainer{
				{
					ID:      apiContainerID,
					Names:   []string{"/" + projectName + "-api-1"},
					Image:   apiRunningImg,
					ImageID: apiImageID,
					State:   "running",
					Status:  "Up 2 hours (healthy)",
					Labels: map[string]string{
						"com.docker.compose.project": projectName,
						"com.docker.compose.service": "api",
						"com.docker.compose.oneoff":  "False",
					},
				},
				{
					ID:      dbContainerID,
					Names:   []string{"/" + projectName + "-postgres-1"},
					Image:   dbImg,
					ImageID: "sha256:" + dbDigest,
					State:   "running",
					Status:  "Up 2 hours (healthy)",
					Labels: map[string]string{
						"com.docker.compose.project": projectName,
						"com.docker.compose.service": "postgres",
						"com.docker.compose.oneoff":  "False",
					},
				},
			})

		case strings.Contains(path, "/containers/"+apiContainerID+"/json"):
			doc := map[string]any{
				"Id":    apiContainerID,
				"Name":  "/" + projectName + "-api-1",
				"Image": apiImageID,
				"State": map[string]any{
					"Status":    "running",
					"Running":   true,
					"StartedAt": apiStartedAt,
					"Health":    map[string]any{"Status": "healthy"},
				},
				"Config": map[string]any{
					"Image":      apiRunningImg,
					"Entrypoint": []string{"/app/entrypoint"},
					"Cmd":        []string{"serve"},
					"Env":        []string{"APP_ENV=dev", "PATH=/usr/local/bin:/usr/bin"},
					"Labels": map[string]string{
						"com.docker.compose.project": projectName,
						"com.docker.compose.service": "api",
						"com.tenko.role":             "api",
					},
				},
				"HostConfig": map[string]any{
					"NetworkMode": projectName + "_default",
					"PortBindings": map[string]any{
						"8080/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": "18080"}},
					},
				},
				"Mounts": []map[string]any{},
				"NetworkSettings": map[string]any{
					"Networks": map[string]any{
						projectName + "_default": map[string]any{"NetworkID": "net-1"},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(doc)

		case strings.Contains(path, "/containers/"+dbContainerID+"/json"):
			doc := map[string]any{
				"Id":    dbContainerID,
				"Name":  "/" + projectName + "-postgres-1",
				"Image": "sha256:" + dbDigest,
				"State": map[string]any{
					"Status":    "running",
					"Running":   true,
					"StartedAt": dbStartedAt,
					"Health":    map[string]any{"Status": "healthy"},
				},
				"Config": map[string]any{
					"Image":      dbImg,
					"Entrypoint": []string{"docker-entrypoint.sh"},
					"Cmd":        []string{"postgres"},
					"Env":        []string{"POSTGRES_DB=tenko", "PATH=/usr/local/bin:/usr/bin"},
					"Labels": map[string]string{
						"com.docker.compose.project": projectName,
						"com.docker.compose.service": "postgres",
					},
				},
				"HostConfig": map[string]any{
					"NetworkMode": projectName + "_default",
				},
				"Mounts": []map[string]any{
					{"Type": "volume", "Name": projectName + "_pgdata", "Destination": "/var/lib/postgresql/data", "RW": true},
				},
				"NetworkSettings": map[string]any{
					"Networks": map[string]any{
						projectName + "_default": map[string]any{"NetworkID": "net-1"},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(doc)

		case strings.Contains(path, "/images/"):
			switch {
			case strings.Contains(path, apiDigestV1):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id":          "sha256:" + apiDigestV1,
					"RepoDigests": []string{"registry.example/tenko/api@sha256:" + apiDigestV1},
					"RepoTags":    []string{"registry.example/tenko/api:1.0.0"},
					"Config": map[string]any{
						"Entrypoint": []string{"/app/entrypoint"},
						"Cmd":        []string{"serve"},
						"Env":        []string{"PATH=/usr/local/bin:/usr/bin"},
					},
				})
			case strings.Contains(path, apiDigestV2):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id":          "sha256:" + apiDigestV2,
					"RepoDigests": []string{"registry.example/tenko/api@sha256:" + apiDigestV2},
					"RepoTags":    []string{"registry.example/tenko/api:1.1.0"},
					"Config": map[string]any{
						"Entrypoint": []string{"/app/entrypoint"},
						"Cmd":        []string{"serve"},
						"Env":        []string{"PATH=/usr/local/bin:/usr/bin"},
					},
				})
			case strings.Contains(path, dbDigest):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id":          "sha256:" + dbDigest,
					"RepoDigests": []string{"docker.io/library/postgres@sha256:" + dbDigest},
					"RepoTags":    []string{"postgres:16-alpine"},
					"Config": map[string]any{
						"Entrypoint": []string{"docker-entrypoint.sh"},
						"Cmd":        []string{"postgres"},
						"Env":        []string{"PATH=/usr/local/bin:/usr/bin"},
					},
				})
			default:
				http.NotFound(w, r)
			}

		case strings.Contains(path, "/networks/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Name": projectName + "_default",
				"Labels": map[string]string{
					"com.docker.compose.project": projectName,
					"com.docker.compose.network": "default",
				},
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer engineSrv.Close()
	t.Setenv("DOCKER_HOST", engineSrv.URL)

	// Create bare/local git repos: oldRepo, newRepoSame, newRepoUpdated
	makeComposeRepo := func(apiImage string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		run := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v (%s)", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		run("init", "-b", "main")
		composeYAML := "services:\n" +
			"  api:\n" +
			"    image: " + apiImage + "\n" +
			"    environment:\n" +
			"      APP_ENV: dev\n" +
			"    ports:\n" +
			"      - \"18080:8080\"\n" +
			"    labels:\n" +
			"      com.tenko.role: api\n" +
			"  postgres:\n" +
			"    image: " + dbImg + "\n" +
			"    environment:\n" +
			"      POSTGRES_DB: tenko\n" +
			"    volumes:\n" +
			"      - pgdata:/var/lib/postgresql/data\n" +
			"volumes:\n" +
			"  pgdata: {}\n"
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0644); err != nil {
			t.Fatal(err)
		}
		run("add", "docker-compose.yml")
		run("commit", "-m", "compose")
		sha := run("rev-parse", "HEAD")
		return dir, sha
	}

	oldRepoDir, oldSHA := makeComposeRepo(apiImgV1)
	newRepoSameDir, sameSHA := makeComposeRepo(apiImgV1)
	newRepoV2Dir, v2SHA := makeComposeRepo(apiImgV2)

	// Fake `docker` CLI supporting `version`, `compose version`, `compose config --format json`, `image inspect`, `volume ls`, `ps`, `compose up`.
	binDir := t.TempDir()
	logFile := filepath.Join(binDir, "docker-cmds.log")
	fakeDocker := filepath.Join(binDir, "docker")
	dockerScript := `#!/bin/sh
echo "$@" >> "` + logFile + `"
if [ "$1" = "version" ]; then
  echo '{"Client":{"Version":"27.3.1","ApiVersion":"1.47"},"Server":{"Version":"27.3.1","ApiVersion":"1.47","MinAPIVersion":"1.24"}}'
  exit 0
fi
if [ "$1" = "compose" ] && [ "$2" = "version" ]; then
  echo "Docker Compose version v2.29.0"
  exit 0
fi
for arg in "$@"; do
  if [ "$arg" = "down" ]; then
    echo "FATAL: compose down must never be called!" >&2
    exit 99
  fi
done
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo '[{"Id":"sha256:ok"}]'
  exit 0
fi
if [ "$1" = "volume" ] && [ "$2" = "ls" ]; then
  echo "` + projectName + `_pgdata"
  exit 0
fi
if [ "$1" = "ps" ]; then
  for a in "$@"; do
    if [ "$a" = "-aq" ]; then
      printf "%s\n%s\n" "` + apiContainerID + `" "` + dbContainerID + `"
      exit 0
    fi
    if [ "$a" = "-a" ]; then
      echo "api|` + apiImgV1 + `|Up 2 hours (healthy)|0.0.0.0:18080->8080/tcp|` + projectName + `-api-1"
      echo "postgres|` + dbImg + `|Up 2 hours (healthy)||` + projectName + `-postgres-1"
      exit 0
    fi
  done
  echo "` + projectName + `|` + projectName + `-api-1|0.0.0.0:18080->8080/tcp"
  echo "` + projectName + `|` + projectName + `-postgres-1|"
  exit 0
fi
if [ "$1" = "inspect" ]; then
  echo '[{"Mounts":[]},{"Mounts":[]}]'
  exit 0
fi
if [ "$1" = "compose" ]; then
  # Check if subcommand is config
  for a in "$@"; do
    if [ "$a" = "config" ]; then
      if grep -q "` + apiDigestV2 + `" docker-compose.yml; then
        API_IMG="` + apiImgV2 + `"
      else
        API_IMG="` + apiImgV1 + `"
      fi
      cat <<JSON
{
  "name": "` + projectName + `",
  "services": {
    "api": {
      "image": "$API_IMG",
      "environment": {"APP_ENV": "dev"},
      "ports": [{"published": "18080", "target": 8080, "protocol": "tcp"}],
      "networks": {"default": null},
      "labels": {"com.tenko.role": "api"}
    },
    "postgres": {
      "image": "` + dbImg + `",
      "environment": {"POSTGRES_DB": "tenko"},
      "volumes": [{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data", "read_only": false}],
      "networks": {"default": null}
    }
  },
  "volumes": {"pgdata": {"name": "` + projectName + `_pgdata"}},
  "networks": {"default": {"name": "` + projectName + `_default"}}
}
JSON
      exit 0
    fi
    if [ "$a" = "up" ]; then
      exit 0
    fi
  done
fi
exit 0
`
	if err := os.WriteFile(fakeDocker, []byte(dockerScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workDir := t.TempDir()
	exec := &ComposeExecutor{WorkDir: workDir}
	appID := "app-e2e-1"
	exec.RememberProject(appID, projectName)

	// Seed initial app checkout from oldRepoDir
	appDirAbs := filepath.Join(workDir, appID)
	_ = os.MkdirAll(appDirAbs, 0755)
	if _, err := exec.prepareRepo(context.Background(), appDirAbs, filepath.Join(appDirAbs, "repo"), oldRepoDir, "main", oldSHA, "public", nil); err != nil {
		t.Fatalf("seed old repo: %v", err)
	}

	// STEP 1: Migrate to newRepoSameDir (identical compose & digest pins)
	prep1, err := exec.PrepareRepoMigration(context.Background(), MigrationPrepParams{
		AppID:          appID,
		AppName:        projectName,
		OldComposePath: "docker-compose.yml",
		NewRepoURL:     newRepoSameDir,
		NewAuthMethod:  "public",
		NewBranch:      "main",
		TargetCommit:   sameSHA,
		NewComposePath: "docker-compose.yml",
		Opts:           ApplyOptions{Project: projectName, RequireDigest: true},
	})
	if err != nil {
		t.Fatalf("PrepareRepoMigration (same config) failed: %v", err)
	}
	if !prep1.Equivalent || prep1.Mode != MigrationModeNoRestart || prep1.Reason != "config_identical" {
		t.Fatalf("expected Equivalent no_restart/config_identical, got %+v", prep1)
	}
	if len(prep1.ToStop) != 0 || !slices.Equal(prep1.UnchangedServices, []string{"api", "postgres"}) {
		t.Fatalf("expected 0 ToStop and [api postgres] unchanged, got ToStop=%v Unchanged=%v", prep1.ToStop, prep1.UnchangedServices)
	}
	if err := exec.CommitRepoMigration(context.Background(), appID, "docker-compose.yml", nil, &prep1); err != nil {
		t.Fatalf("CommitRepoMigration (same config) failed: %v", err)
	}

	// Verify neither `compose down` nor `compose up` was called, and container IDs + StartedAt are identical!
	cmdsBytes, _ := os.ReadFile(logFile)
	cmds := string(cmdsBytes)
	if strings.Contains(cmds, " down") || strings.Contains(cmds, " up ") {
		t.Fatalf("no-restart migration must not call compose down or compose up, commands:\n%s", cmds)
	}

	// STEP 2: Migrate to newRepoV2Dir (updated image digest on `api`, `postgres` unchanged)
	prep2, err := exec.PrepareRepoMigration(context.Background(), MigrationPrepParams{
		AppID:          appID,
		AppName:        projectName,
		OldComposePath: "docker-compose.yml",
		NewRepoURL:     newRepoV2Dir,
		NewAuthMethod:  "public",
		NewBranch:      "main",
		TargetCommit:   v2SHA,
		NewComposePath: "docker-compose.yml",
		Opts:           ApplyOptions{Project: projectName, RequireDigest: true},
	})
	if err != nil {
		t.Fatalf("PrepareRepoMigration (updated api image) failed: %v", err)
	}
	if prep2.Equivalent || prep2.Mode != MigrationModeRestarted || prep2.Reason != "image_changed" {
		t.Fatalf("expected restarted/image_changed, got %+v", prep2)
	}
	if !slices.Equal(prep2.ChangedServices, []string{"api"}) || !slices.Equal(prep2.UnchangedServices, []string{"postgres"}) {
		t.Fatalf("expected only api changed and postgres unchanged, got changed=%v unchanged=%v", prep2.ChangedServices, prep2.UnchangedServices)
	}
	if len(prep2.ToStop) != 1 || !strings.HasPrefix(prep2.ToStop[0], "api ") {
		t.Fatalf("expected ToStop to list only api, got %v", prep2.ToStop)
	}
	if err := exec.CommitRepoMigration(context.Background(), appID, "docker-compose.yml", nil, &prep2); err != nil {
		t.Fatalf("CommitRepoMigration (updated api image) failed: %v", err)
	}
	// Run standard ApplyImages (`compose up -d --remove-orphans`, never `compose down`)
	if _, err := exec.ApplyImages(context.Background(), appID, "", nil, newRepoV2Dir, "main", "docker-compose.yml", v2SHA, "public", nil, ApplyOptions{Project: projectName, RequireDigest: true}, nil); err != nil {
		t.Fatalf("ApplyImages after migration failed: %v", err)
	}
	// Simulate Docker Compose recreating ONLY `api` while `postgres` keeps its original container ID and StartedAt
	mu.Lock()
	apiContainerID = "cid-api-recreated-002"
	apiStartedAt = "2026-11-11T12:05:00.000000000Z"
	apiRunningImg = apiImgV2
	apiImageID = "sha256:" + apiDigestV2
	mu.Unlock()

	cmdsBytes2, _ := os.ReadFile(logFile)
	cmds2 := string(cmdsBytes2)
	if strings.Contains(cmds2, " down") {
		t.Fatalf("changed-service migration must NEVER call compose down, commands:\n%s", cmds2)
	}
	if !strings.Contains(cmds2, "compose -p "+projectName+" -f docker-compose.yml up -d --remove-orphans") {
		t.Fatalf("expected standard `compose up -d --remove-orphans` call, commands:\n%s", cmds2)
	}

	// Verify post-apply state matches desired V2 spec with 0 diff (`Equivalent == true`), while `postgres` kept `cid-postgres-initial-001`!
	obs, err := exec.observeContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundDB := false
	foundAPI := false
	for _, c := range obs {
		if c.Service == "postgres" && c.ID == "cid-postgres-initial-001" && c.StartedAt.UTC().Format("2006-01-02T15:04:05Z") == "2026-11-11T10:00:00Z" {
			foundDB = true
		}
		if c.Service == "api" && c.ID == "cid-api-recreated-002" {
			foundAPI = true
		}
	}
	if !foundDB || !foundAPI {
		t.Fatalf("expected postgres untouched and api recreated, obs=%+v", obs)
	}
}
