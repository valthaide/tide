package fleetcheck

import (
	"sort"
	"strings"
)

// NormalizeImage returns the image name docker uses before the tag and digest.
// Short names become docker.io/library/<name>.
func NormalizeImage(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	name := ref
	if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i:], "/") {
		name = ref[:i]
	}
	if name == "" {
		return ""
	}
	first, _, _ := strings.Cut(name, "/")
	if !strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost" {
		if !strings.Contains(name, "/") {
			name = "docker.io/library/" + name
		} else {
			name = "docker.io/" + name
		}
	}
	return name
}

// RegistryAllowed reports whether image may be pulled.
// An empty allow list allows every image.
func RegistryAllowed(image string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	name := NormalizeImage(image)
	if name == "" {
		return false
	}
	for _, item := range allowed {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		item = strings.TrimSuffix(NormalizeImage(item), "/")
		if item == "" {
			item = strings.TrimSpace(item)
		}
		if name == item || strings.HasPrefix(name, strings.TrimRight(item, "/")+"/") {
			return true
		}
	}
	return false
}

// DeniedImages lists images outside the allow list.
func DeniedImages(images map[string]string, allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	var out []string
	for _, image := range images {
		if strings.TrimSpace(image) == "" {
			continue
		}
		if !RegistryAllowed(image, allowed) {
			out = append(out, image)
		}
	}
	sort.Strings(out)
	return out
}
