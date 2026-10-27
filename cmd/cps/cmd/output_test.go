package cmd

import (
	"strings"
	"testing"

	"github.com/valthaide/conops-private/internal/api"
)

func TestShortImagesLineUsesTagOnly(t *testing.T) {
	app := api.App{
		Images: []api.ImageRef{
			{
				Service: "nginx",
				Image:   "git.tenkocrm.online:5050/tenko/app/crm-backend2/nginx:sha-50608e44a69f05ccd85556b70a83975d607edb50@sha256:481ea944a77819881d66a084f3f35c2e513ed591258142c3cf9b0489a1a7cb47",
				Digest:  "sha256:481ea944a77819881d66a084f3f35c2e513ed591258142c3cf9b0489a1a7cb47",
			},
			{
				Service: "redis",
				Image:   "redis:7-alpine",
				Digest:  "sha256:6ba19c50b567aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
	}
	got := shortImagesLine(app)
	if got != "nginx=sha-50608e44a69f  redis=7-alpine" {
		t.Fatalf("short images: %s", got)
	}
	if strings.Contains(got, "tenkocrm") || strings.Contains(got, "@sha256") {
		t.Fatalf("registry path leaked: %s", got)
	}
	full := fullImageRef(app.Images[0])
	if !strings.Contains(full, "@sha256:481ea944a77819881d66a084f3f35c2e513ed591258142c3cf9b0489a1a7cb47") {
		t.Fatalf("full ref: %s", full)
	}
	if strings.Count(full, "@sha256:") != 1 {
		t.Fatalf("digest duplicated: %s", full)
	}
}

func TestShortRefLeavesPlainTag(t *testing.T) {
	if shortRef("GRAFANA_TAG=11.3.1") != "GRAFANA_TAG=11.3.1" {
		t.Fatal(shortRef("GRAFANA_TAG=11.3.1"))
	}
}
