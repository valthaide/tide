package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/valthaide/conops-private/internal/deploypolicy"
)

// WatchFilesHash hashes watch_file paths relative to the app repo checkout
// (or absolute paths as given).
func WatchFilesHash(repoDir string, files []deploypolicy.WatchFile) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	type entry struct {
		path string
		sum  string
	}
	var entries []entry
	for _, f := range files {
		path := strings.TrimSpace(f.Path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoDir, path)
		}
		path = filepath.Clean(path)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("watch file %s: %w", path, err)
		}
		sum := sha256.Sum256(data)
		entries = append(entries, entry{path: path, sum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s=%s\n", e.path, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WatchFilesHashForApp hashes watch files for an app under the runtime workdir.
func (e *ComposeExecutor) WatchFilesHashForApp(appID string, files []deploypolicy.WatchFile) (string, error) {
	if e == nil {
		return "", fmt.Errorf("compose executor is nil")
	}
	repoDir := filepath.Join(e.WorkDir, appID, "repo")
	return WatchFilesHash(repoDir, files)
}
