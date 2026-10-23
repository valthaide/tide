package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/valthaide/conops-private/internal/deploypolicy"
)

// WatchHashError is a stable, actionable failure while hashing watch_files.
type WatchHashError struct {
	App  string
	Path string
	Err  error
}

func (e *WatchHashError) Error() string {
	if e == nil {
		return ""
	}
	if e.App != "" {
		return fmt.Sprintf("watch_files hash failed app=%s path=%s: %v", e.App, e.Path, e.Err)
	}
	return fmt.Sprintf("watch_files hash failed path=%s: %v", e.Path, e.Err)
}

func (e *WatchHashError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// WatchFilesHash hashes watch_file paths relative to the app repo checkout.
// Paths may be files, directories (recursive), or globs (* ? []).
// Symlinks are hashed by name only when they escape the checkout; in-tree
// symlink targets are followed only if they stay under repoDir.
func WatchFilesHash(repoDir string, files []deploypolicy.WatchFile) (string, error) {
	return WatchFilesHashForAppName("", repoDir, files)
}

// WatchFilesHashForAppName is like WatchFilesHash but annotates errors with app name.
func WatchFilesHashForAppName(appName, repoDir string, files []deploypolicy.WatchFile) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	repoAbs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", &WatchHashError{App: appName, Path: repoDir, Err: err}
	}

	type entry struct {
		path string
		sum  string
	}
	var entries []entry
	seen := map[string]struct{}{}

	addFile := func(absPath string) error {
		rel, relErr := filepath.Rel(repoAbs, absPath)
		if relErr != nil {
			rel = absPath
		}
		key := filepath.ToSlash(rel)
		if _, ok := seen[key]; ok {
			return nil
		}
		sum, herr := hashOnePath(repoAbs, absPath)
		if herr != nil {
			return &WatchHashError{App: appName, Path: key, Err: herr}
		}
		seen[key] = struct{}{}
		entries = append(entries, entry{path: key, sum: sum})
		return nil
	}

	for _, f := range files {
		pattern := strings.TrimSpace(f.Path)
		if pattern == "" {
			continue
		}
		matches, expandErr := expandWatchPath(repoAbs, pattern)
		if expandErr != nil {
			return "", &WatchHashError{App: appName, Path: pattern, Err: expandErr}
		}
		if len(matches) == 0 {
			return "", &WatchHashError{App: appName, Path: pattern, Err: fmt.Errorf("path not found")}
		}
		for _, m := range matches {
			info, lerr := os.Lstat(m)
			if lerr != nil {
				return "", &WatchHashError{App: appName, Path: m, Err: lerr}
			}
			if info.IsDir() {
				walkErr := filepath.WalkDir(m, func(path string, d fs.DirEntry, werr error) error {
					if werr != nil {
						return werr
					}
					if d.IsDir() {
						return nil
					}
					return addFile(path)
				})
				if walkErr != nil {
					return "", &WatchHashError{App: appName, Path: pattern, Err: walkErr}
				}
				continue
			}
			if err := addFile(m); err != nil {
				return "", err
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s=%s\n", e.path, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func expandWatchPath(repoAbs, pattern string) ([]string, error) {
	raw := strings.TrimSpace(pattern)
	absPattern := raw
	repoRelative := !filepath.IsAbs(raw)
	if repoRelative {
		absPattern = filepath.Join(repoAbs, raw)
	}
	absPattern = filepath.Clean(absPattern)

	if strings.ContainsAny(filepath.Base(absPattern), "*?[") || strings.ContainsAny(absPattern, "*?[") {
		matches, err := filepath.Glob(absPattern)
		if err != nil {
			return nil, err
		}
		if repoRelative {
			var filtered []string
			for _, m := range matches {
				if isUnder(repoAbs, m) {
					filtered = append(filtered, m)
				}
			}
			return filtered, nil
		}
		return matches, nil
	}

	if _, err := os.Lstat(absPattern); err != nil {
		return nil, err
	}
	return []string{absPattern}, nil
}

func hashOnePath(repoAbs, absPath string) (string, error) {
	info, err := os.Lstat(absPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(absPath)
		if rerr != nil {
			return "", rerr
		}
		// Do not follow escapes outside checkout: hash link text only.
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(absPath), target)
		}
		resolved = filepath.Clean(resolved)
		if isUnder(repoAbs, absPath) && !isUnder(repoAbs, resolved) {
			sum := sha256.Sum256([]byte("symlink:" + target))
			return hex.EncodeToString(sum[:]), nil
		}
		// In-tree symlink: hash target content if it is a regular file; else link text.
		tinfo, terr := os.Stat(resolved)
		if terr != nil || tinfo.IsDir() || tinfo.Mode()&os.ModeSymlink != 0 {
			sum := sha256.Sum256([]byte("symlink:" + target))
			return hex.EncodeToString(sum[:]), nil
		}
		absPath = resolved
	}
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isUnder(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// WatchEntryHashesForApp hashes each watch_files entry separately.
func (e *ComposeExecutor) WatchEntryHashesForApp(appID string, files []deploypolicy.WatchFile) (map[string]string, error) {
	out := make(map[string]string, len(files))
	for _, f := range files {
		sum, err := e.WatchFilesHashForApp(appID, []deploypolicy.WatchFile{f})
		if err != nil {
			return nil, err
		}
		out[f.Path] = sum
	}
	return out, nil
}

// WatchFilesHashForApp hashes watch files for an app under the runtime workdir.
func (e *ComposeExecutor) WatchFilesHashForApp(appID string, files []deploypolicy.WatchFile) (string, error) {
	if e == nil {
		return "", fmt.Errorf("compose executor is nil")
	}
	repoDir := filepath.Join(e.WorkDir, appID, "repo")
	return WatchFilesHashForAppName(appID, repoDir, files)
}
