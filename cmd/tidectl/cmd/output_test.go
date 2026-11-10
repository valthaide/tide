package cmd

import (
	"strings"
	"testing"

	"github.com/valthaide/tide/internal/api"
)

func TestFullImageRefKeepsOneDigest(t *testing.T) {
	img := api.ImageRef{
		Service: "nginx",
		Image:   "git.tenkocrm.online:5050/tenko/app/crm-backend2/nginx:sha-50608e44a69f05ccd85556b70a83975d607edb50@sha256:481ea944a77819881d66a084f3f35c2e513ed591258142c3cf9b0489a1a7cb47",
		Digest:  "sha256:481ea944a77819881d66a084f3f35c2e513ed591258142c3cf9b0489a1a7cb47",
	}
	full := fullImageRef(img)
	if strings.Count(full, "@sha256:") != 1 {
		t.Fatalf("digest duplicated: %s", full)
	}
	if !strings.Contains(full, "nginx:sha-50608e44a69f05ccd85556b70a83975d607edb50@sha256:481ea944a778") {
		t.Fatalf("full ref: %s", full)
	}
}
