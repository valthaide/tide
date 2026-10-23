package compose

import "strings"

// composeDownArgs builds `docker compose down` for one project.
// Named volumes are removed only when pruneVolumes is set. Images are not
// removed by compose itself: the caller deletes refs that no other container uses.
func composeDownArgs(projectName, composeFileName string, pruneVolumes bool) []string {
	args := []string{"compose", "-p", projectName, "-f", composeFileName, "down", "--remove-orphans"}
	if pruneVolumes {
		args = append(args, "--volumes")
	}
	return args
}

// imagesSafeToRemove returns image refs from candidates that are not in stillReferenced.
// Comparison is exact on the docker image name (tag or id) reported by the daemon.
func imagesSafeToRemove(candidates, stillReferenced []string) []string {
	used := make(map[string]struct{}, len(stillReferenced))
	for _, ref := range stillReferenced {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		used[ref] = struct{}{}
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, ref := range candidates {
		ref = strings.TrimSpace(ref)
		if ref == "" || ref == "<none>:<none>" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		if _, ok := used[ref]; ok {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func uniqueImageRefs(lines string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, line := range strings.Split(lines, "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || ref == "<none>:<none>" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}
