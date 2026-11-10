package compose

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/imagegc"
)

func parseImageInspect(raw string, used map[string]struct{}) []imagegc.Image {
	var docs []struct {
		ID          string    `json:"Id"`
		RepoTags    []string  `json:"RepoTags"`
		RepoDigests []string  `json:"RepoDigests"`
		Created     time.Time `json:"Created"`
		Size        int64     `json:"Size"`
	}
	if err := json.Unmarshal([]byte(extractJSON(raw)), &docs); err != nil {
		return nil
	}
	var out []imagegc.Image
	for _, doc := range docs {
		image := imagegc.Image{
			ID:      doc.ID,
			Tags:    doc.RepoTags,
			Digests: doc.RepoDigests,
			Created: doc.Created,
			Size:    doc.Size,
		}
		if _, ok := used[doc.ID]; ok {
			image.Used = true
		}
		for _, tag := range doc.RepoTags {
			if _, ok := used[tag]; ok {
				image.Used = true
			}
		}
		out = append(out, image)
	}
	return out
}

func (e *ComposeExecutor) LocalImages(ctx context.Context) ([]imagegc.Image, error) {
	usedRaw, _ := e.runCommand(ctx, "docker", []string{"ps", "-aq", "--no-trunc", "--format", "{{.Image}}"}, e.runtimeWorkDir(), nil, nil)
	used := map[string]struct{}{}
	for _, line := range strings.Split(usedRaw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			used[line] = struct{}{}
		}
	}
	idsRaw, err := e.runCommand(ctx, "docker", []string{"image", "ls", "-q", "--no-trunc"}, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		return nil, err
	}
	var ids []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(idsRaw, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"image", "inspect"}, ids...)
	raw, err := e.runCommand(ctx, "docker", args, e.runtimeWorkDir(), nil, nil)
	if err != nil {
		return nil, err
	}
	return parseImageInspect(raw, used), nil
}
