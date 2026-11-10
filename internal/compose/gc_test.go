package compose

import "testing"

func TestParseImageInspectMarksUsed(t *testing.T) {
	raw := `[{"Id":"sha256:abc","RepoTags":["registry.example/app:1"],"RepoDigests":["registry.example/app@sha256:abc"],"Created":"2020-01-02T03:04:05Z","Size":9}]`
	got := parseImageInspect(raw, map[string]struct{}{"registry.example/app:1": {}})
	if len(got) != 1 || !got[0].Used || got[0].Size != 9 {
		t.Fatalf("%+v", got)
	}
}

func TestVolumeInspectKept(t *testing.T) {
	if !volumeInspectKept(`[{"Labels":{"com.tide.keep":"true"}}]`) {
		t.Fatal("keep label")
	}
	if volumeInspectKept(`[{"Labels":{"com.docker.compose.project":"demo"}}]`) {
		t.Fatal("ordinary volume")
	}
}
