package compose

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadEngineObservations(t *testing.T) {
	const containerID = "abc123"
	const imageID = "sha256:img1"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project") {
			t.Errorf("filters: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{
			"Id":"abc123",
			"Names":["/crm-php-1"],
			"Image":"registry.example/php:81d4e14a",
			"ImageID":"sha256:img1",
			"Labels":{
				"com.docker.compose.project":"crm",
				"com.docker.compose.service":"php"
			},
			"State":"running",
			"Status":"Up 2 minutes (healthy)"
		}]`))
	})
	mux.HandleFunc("/v1.44/containers/"+containerID+"/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"Id":"sha256:abc123",
			"Image":"sha256:img1",
			"RestartCount":4,
			"State":{"Status":"running","Running":true,"ExitCode":0,"StartedAt":"2026-10-27T10:00:00.0Z","Health":{"Status":"healthy"}},
			"HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}},
			"Config":{"Image":"registry.example/php:81d4e14a","Labels":{"com.docker.compose.depends_on":""}}
		}`))
	})
	mux.HandleFunc("/v1.44/images/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"Id":"sha256:img1",
			"RepoTags":["registry.example/php:81d4e14a"],
			"RepoDigests":["registry.example/php@sha256:deadbeefdeadbeefdeadbeefdeadbeef"]
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &engineClient{base: srv.URL, http: srv.Client()}

	obs, err := (&ComposeExecutor{}).readEngineObservations(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("obs: %+v", obs)
	}
	got := obs[0]
	if got.Project != "crm" || got.Service != "php" || got.Name != "crm-php-1" {
		t.Fatalf("identity: %+v", got)
	}
	if !got.HasInspect || got.RestartCount != 4 || got.Health != "healthy" || got.State != "running" {
		t.Fatalf("inspect: %+v", got)
	}
	if !strings.Contains(got.RepoDigest, "sha256:deadbeef") {
		t.Fatalf("digest: %q image %q", got.RepoDigest, imageID)
	}
}
