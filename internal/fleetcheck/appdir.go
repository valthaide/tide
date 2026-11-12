package fleetcheck

import (
	"path"
	"strings"
)

// CheckAppDir validates an app_dir registration value: the folder of the
// application inside its own repository, e.g. "backend" or "tools/pgadmin".
// Empty means the repository root and is valid. The path must be relative,
// clean, and stay inside the repository — absolute paths, drive/UNC forms,
// and ".." escapes are rejected so a fleet file can never redirect compose
// outside the app checkout.
func CheckAppDir(raw string) *Issue {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	bad := func() *Issue {
		return &Issue{Rule: "invalid_app_dir", Message: "invalid_app_dir: " + raw + " (want a relative folder inside the app repository, e.g. backend or tools/pgadmin)"}
	}
	if strings.Contains(raw, "\\") || strings.Contains(raw, ":") {
		return bad()
	}
	if path.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return bad()
	}
	// Reject ".." in the raw value before Clean collapses a/../b into b —
	// a fleet file must not hide escapes behind path cleaning.
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return bad()
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return bad()
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return bad()
		}
	}
	return nil
}

// NormalizeAppDir returns the canonical slash-separated app_dir (""
// for the repository root, otherwise a cleaned relative path).
func NormalizeAppDir(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	clean := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
	if clean == "." {
		return ""
	}
	return clean
}
