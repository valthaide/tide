package cmd

import (
	"strings"
	"testing"

	"github.com/valthaide/conops-private/internal/api"
)

func TestImagesValueShortensDigest(t *testing.T) {
	app := api.App{
		Images: []api.ImageRef{{
			Service: "grafana",
			Image:   "grafana/grafana:11.3.1",
			Digest:  "sha256:fa801ab6e1ae035135309580891e09f7eb94d1abdbd2106bdc288030b028158c",
		}},
	}
	got := imagesValue(app)
	if strings.Contains(got, "fa801ab6e1ae0351") {
		t.Fatalf("digest was not shortened: %s", got)
	}
	if !strings.Contains(got, "grafana=grafana/grafana:11.3.1@sha256:fa801ab6e1ae") {
		t.Fatalf("short ref: %s", got)
	}
	full := fullImageRef(app.Images[0])
	if !strings.HasSuffix(full, "b028158c") {
		t.Fatalf("full ref: %s", full)
	}
}

func TestShortRefLeavesPlainTag(t *testing.T) {
	if shortRef("GRAFANA_TAG=11.3.1") != "GRAFANA_TAG=11.3.1" {
		t.Fatal(shortRef("GRAFANA_TAG=11.3.1"))
	}
}
