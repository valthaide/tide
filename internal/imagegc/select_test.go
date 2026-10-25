package imagegc

import (
	"testing"
	"time"
)

func TestSelectKeepsRunningProtectedAndForeign(t *testing.T) {
	now := time.Now()
	old := now.Add(-100 * time.Hour)
	own := map[string]struct{}{"registry.example/app": {}}
	protected := map[string]struct{}{"sha256:keep": {}}
	got := Select([]Image{
		{ID: "1", Tags: []string{"registry.example/app:old"}, Created: old, Size: 10},
		{ID: "2", Tags: []string{"registry.example/app:keep"}, Digests: []string{"registry.example/app@sha256:keep"}, Created: old, Size: 10},
		{ID: "3", Tags: []string{"registry.example/app:new"}, Created: now.Add(-time.Hour), Size: 10},
		{ID: "4", Tags: []string{"registry.example/app:run"}, Created: old, Used: true, Size: 10},
		{ID: "5", Tags: []string{"busybox:latest"}, Created: old, Size: 10},
	}, protected, own, 72*time.Hour, now)
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("%+v", got)
	}
}
