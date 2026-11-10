package imagegc

import (
	"strings"
	"time"
)

// Image is one local image the GC may consider.
type Image struct {
	ID      string
	Tags    []string
	Digests []string
	Created time.Time
	Size    int64
	Used    bool
}

// Select returns images that are unused, old, not part of a kept revision, and owned by Tide apps.
// Images whose repository is not in ownRepos are left alone.
func Select(images []Image, protected, ownRepos map[string]struct{}, minAge time.Duration, now time.Time) []Image {
	if ownRepos == nil {
		ownRepos = map[string]struct{}{}
	}
	if protected == nil {
		protected = map[string]struct{}{}
	}
	var out []Image
	for _, image := range images {
		if image.Used || !owned(image, ownRepos) {
			continue
		}
		if !image.Created.IsZero() && now.Sub(image.Created) < minAge {
			continue
		}
		if protects(image, protected) {
			continue
		}
		out = append(out, image)
	}
	return out
}

func owned(image Image, ownRepos map[string]struct{}) bool {
	for _, tag := range image.Tags {
		if _, ok := ownRepos[repoOf(tag)]; ok {
			return true
		}
	}
	return false
}

func protects(image Image, protected map[string]struct{}) bool {
	for _, digest := range image.Digests {
		if protectedRef(digest, protected) {
			return true
		}
	}
	for _, tag := range image.Tags {
		if protectedRef(tag, protected) {
			return true
		}
	}
	if _, ok := protected[image.ID]; ok {
		return true
	}
	return false
}

func protectedRef(ref string, protected map[string]struct{}) bool {
	if _, ok := protected[ref]; ok {
		return true
	}
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		if _, ok := protected[ref[i+1:]]; ok {
			return true
		}
	}
	return false
}

func repoOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i:], "/") {
		ref = ref[:i]
	}
	return ref
}
