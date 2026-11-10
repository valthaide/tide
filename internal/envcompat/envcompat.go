package envcompat

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Transitional compatibility aliases for one release (ConOps -> Tide, cps -> tidectl).
// Remove this alias table in the next release after all hosts migrate to TIDE_* and tidectl.
const (
	legacyEnvPrefix    = "CONOPS_"
	legacyCLIPrefix    = "CPS_"
	legacyBinaryName   = "cps"
	legacyDBName       = "conops.db"
	legacyKeyFileName  = "conops-encryption.key"
	legacyOptDataDir   = "/opt/conops/data"
	legacyEtcCA        = "/etc/conops/ca.crt"
	legacyOptCA        = "/opt/conops/data/tls/cert.pem"
	legacySecretsEnv   = "/etc/conops/secrets.env"
	legacyHomeDirName  = ".conops"
	legacyXDGDirName   = "cps"
	legacyRuntimeDir   = "./.conops-runtime"
	legacyGitCacheDir  = "./.conops-cache"
	legacyKeepLabel    = "com.conops.keep"
	legacyOneshotLabel = "com.conops.oneshot"
	legacyWebhookHdr   = "X-Conops-Webhook-Token"

	NewDBName      = "tide.db"
	NewKeyFileName = "tide-encryption.key"
)

type pendingWarn struct {
	msg   string
	attrs []any
}

var (
	mu         sync.Mutex
	logger     *slog.Logger
	cliOut     io.Writer
	warnedKeys = map[string]struct{}{}
	earlyWarns []pendingWarn
)

// SetLogger attaches the controller logger and flushes any warnings recorded before logger init.
func SetLogger(l *slog.Logger) {
	mu.Lock()
	logger = l
	queued := earlyWarns
	earlyWarns = nil
	mu.Unlock()

	if l != nil {
		for _, w := range queued {
			l.Warn(w.msg, w.attrs...)
		}
	}
}

// SetCLIWarningWriter sets an optional writer (typically os.Stderr) for CLI deprecation warnings.
func SetCLIWarningWriter(w io.Writer) {
	mu.Lock()
	cliOut = w
	queued := earlyWarns
	if w != nil {
		earlyWarns = nil
	}
	mu.Unlock()

	if w != nil {
		for _, item := range queued {
			_, _ = fmt.Fprintf(w, "warning: %s\n", formatCLIWarn(item.msg, item.attrs))
		}
	}
}

// ResetForTest clears warning deduplication state in unit tests.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	logger = nil
	cliOut = nil
	warnedKeys = map[string]struct{}{}
	earlyWarns = nil
}

func warnOnce(dedupKey, msg string, attrs ...any) {
	mu.Lock()
	if _, seen := warnedKeys[dedupKey]; seen {
		mu.Unlock()
		return
	}
	warnedKeys[dedupKey] = struct{}{}
	l := logger
	w := cliOut
	if l == nil && w == nil {
		earlyWarns = append(earlyWarns, pendingWarn{msg: msg, attrs: attrs})
		mu.Unlock()
		return
	}
	mu.Unlock()

	if l != nil {
		l.Warn(msg, attrs...)
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "warning: %s\n", formatCLIWarn(msg, attrs))
	}
}

func formatCLIWarn(msg string, attrs []any) string {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(attrs); i += 2 {
		b.WriteString(fmt.Sprintf(" (%v=%v)", attrs[i], attrs[i+1]))
	}
	return b.String()
}

// LegacyAlias returns the deprecated environment variable name for a TIDE_* or TIDECTL_* variable.
func LegacyAlias(key string) string {
	switch {
	case strings.HasPrefix(key, "TIDE_"):
		return legacyEnvPrefix + strings.TrimPrefix(key, "TIDE_")
	case strings.HasPrefix(key, "TIDECTL_"):
		return legacyCLIPrefix + strings.TrimPrefix(key, "TIDECTL_")
	default:
		return ""
	}
}

// Getenv reads a TIDE_* (or TIDECTL_*) environment variable, falling back to its legacy CONOPS_* / CPS_* alias.
// Using a legacy alias emits a deprecation warning once per process.
func Getenv(key string) string {
	val, _ := LookupEnv(key)
	return val
}

// LookupEnv reads a TIDE_* (or TIDECTL_*) variable with legacy fallback, returning whether a non-empty value was found.
func LookupEnv(key string) (string, bool) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v, true
	}
	alias := LegacyAlias(key)
	if alias == "" {
		return "", false
	}
	if v := strings.TrimSpace(os.Getenv(alias)); v != "" {
		warnOnce("env:"+alias, "Deprecated environment variable in use; migrate to Tide variable", "deprecated", alias, "replacement", key)
		return v, true
	}
	return "", false
}

// FirstEnv checks all primary keys first, then checks their legacy aliases in order.
func FirstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	for _, key := range keys {
		alias := LegacyAlias(key)
		if alias == "" {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(alias)); v != "" {
			warnOnce("env:"+alias, "Deprecated environment variable in use; migrate to Tide variable", "deprecated", alias, "replacement", key)
			return v
		}
	}
	return ""
}

// CheckBinaryAlias logs a deprecation warning when tidectl is invoked via the legacy "cps" binary name.
func CheckBinaryAlias(arg0 string, stderr io.Writer) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(arg0)))
	base = strings.TrimSuffix(base, ".exe")
	if base != legacyBinaryName {
		return false
	}
	if stderr != nil {
		_, _ = fmt.Fprintf(stderr, "warning: %q is deprecated and will be removed in the next release; use \"tidectl\"\n", legacyBinaryName)
	}
	return true
}

// ResolveDataDir selects the controller data directory, supporting legacy host paths for one transitional release.
func ResolveDataDir(explicitFlag string) string {
	if v := strings.TrimSpace(explicitFlag); v != "" {
		return v
	}
	if v := Getenv("TIDE_DATA_DIR"); v != "" {
		return v
	}
	if info, err := os.Stat("/data"); err == nil && info.IsDir() {
		return "/data"
	}
	if info, err := os.Stat("/opt/tide/data"); err == nil && info.IsDir() {
		return "/opt/tide/data"
	}
	if info, err := os.Stat(legacyOptDataDir); err == nil && info.IsDir() {
		warnOnce("path:"+legacyOptDataDir, "Deprecated data directory in use; migrate to /opt/tide/data", "deprecated", legacyOptDataDir, "replacement", "/opt/tide/data")
		return legacyOptDataDir
	}
	return "."
}

// ResolveRuntimeDir returns the compose runtime directory.
func ResolveRuntimeDir() string {
	if v := Getenv("TIDE_RUNTIME_DIR"); v != "" {
		return v
	}
	if info, err := os.Stat("./.tide-runtime"); err == nil && info.IsDir() {
		return "./.tide-runtime"
	}
	if info, err := os.Stat(legacyRuntimeDir); err == nil && info.IsDir() {
		warnOnce("path:"+legacyRuntimeDir, "Deprecated runtime directory in use; migrate to ./.tide-runtime", "deprecated", legacyRuntimeDir, "replacement", "./.tide-runtime")
		return legacyRuntimeDir
	}
	return "./.tide-runtime"
}

// ResolveGitCacheDir returns the git watcher cache directory.
func ResolveGitCacheDir() string {
	if info, err := os.Stat("./.tide-cache"); err == nil && info.IsDir() {
		return "./.tide-cache"
	}
	if info, err := os.Stat(legacyGitCacheDir); err == nil && info.IsDir() {
		warnOnce("path:"+legacyGitCacheDir, "Deprecated git cache directory in use; migrate to ./.tide-cache", "deprecated", legacyGitCacheDir, "replacement", "./.tide-cache")
		return legacyGitCacheDir
	}
	return "./.tide-cache"
}

// MigrateDataFiles migrates legacy SQLite database and encryption key files into Tide filenames
// without deleting the original files, verifying byte sizes before atomic rename into place.
func MigrateDataFiles(dataDir string) error {
	if strings.TrimSpace(dataDir) == "" {
		return nil
	}
	newKey := filepath.Join(dataDir, NewKeyFileName)
	oldKey := filepath.Join(dataDir, legacyKeyFileName)
	if !fileExists(newKey) && fileExists(oldKey) {
		if err := copyVerifiedPrivateFile(oldKey, newKey); err != nil {
			return fmt.Errorf("migrate encryption key %s -> %s: %w", oldKey, newKey, err)
		}
		warnOnce("file:"+oldKey, "Migrated legacy encryption key to Tide filename", "from", oldKey, "to", newKey)
	}

	newDB := filepath.Join(dataDir, NewDBName)
	oldDB := filepath.Join(dataDir, legacyDBName)
	if !fileExists(newDB) && fileExists(oldDB) {
		if err := copyVerifiedPrivateFile(oldDB, newDB); err != nil {
			return fmt.Errorf("migrate sqlite database %s -> %s: %w", oldDB, newDB, err)
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if fileExists(oldDB + suffix) {
				if err := copyVerifiedPrivateFile(oldDB+suffix, newDB+suffix); err != nil {
					return fmt.Errorf("migrate sqlite journal %s: %w", oldDB+suffix, err)
				}
			}
		}
		warnOnce("file:"+oldDB, "Migrated legacy SQLite database to Tide filename", "from", oldDB, "to", newDB)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func copyVerifiedPrivateFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(tmp)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if n != srcInfo.Size() {
		_ = os.Remove(tmp)
		return fmt.Errorf("copied %d bytes, expected %d", n, srcInfo.Size())
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// ResolveCLIConfigPath resolves the tidectl config file location with legacy fallback.
func ResolveCLIConfigPath(flagPath string) string {
	if v := strings.TrimSpace(flagPath); v != "" {
		return v
	}
	if v := Getenv("TIDECTL_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tidectl-config.yaml"
	}
	preferred := filepath.Join(home, ".tide", "config")
	if fileExists(preferred) {
		return preferred
	}
	legacyHome := filepath.Join(home, legacyHomeDirName, "config")
	if fileExists(legacyHome) {
		warnOnce("cfg:"+legacyHome, "Deprecated CLI config path in use; run `tidectl config init` to write ~/.tide/config", "deprecated", legacyHome, "replacement", preferred)
		return legacyHome
	}
	legacyXDG := filepath.Join(home, ".config", legacyXDGDirName, "config.yaml")
	if fileExists(legacyXDG) {
		warnOnce("cfg:"+legacyXDG, "Deprecated CLI config path in use; run `tidectl config init` to write ~/.tide/config", "deprecated", legacyXDG, "replacement", preferred)
		return legacyXDG
	}
	return preferred
}

// DiscoverCAFile finds the controller CA certificate with legacy fallback.
func DiscoverCAFile() string {
	if p := FirstEnv("TIDE_CA_FILE", "TIDECTL_CA_FILE"); p != "" {
		if fileExists(p) {
			return p
		}
	}
	for _, path := range []string{
		"/etc/tide/ca.crt",
		"/data/tls/cert.pem",
		"/opt/tide/data/tls/cert.pem",
	} {
		if fileExists(path) {
			return path
		}
	}
	for _, legacy := range []string{legacyEtcCA, legacyOptCA} {
		if fileExists(legacy) {
			warnOnce("ca:"+legacy, "Deprecated CA certificate path in use; migrate to /etc/tide/ca.crt", "deprecated", legacy)
			return legacy
		}
	}
	return ""
}

// ResolveSecretsEnvPath returns the secrets env file for `tidectl config init`.
func ResolveSecretsEnvPath(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	if fileExists("/etc/tide/secrets.env") {
		return "/etc/tide/secrets.env"
	}
	if fileExists(legacySecretsEnv) {
		warnOnce("secrets:"+legacySecretsEnv, "Deprecated secrets file in use; migrate to /etc/tide/secrets.env", "deprecated", legacySecretsEnv)
		return legacySecretsEnv
	}
	return "/etc/tide/secrets.env"
}

// ExtractAPITokenFromMap reads TIDE_API_TOKEN or its transitional alias from a parsed env map.
func ExtractAPITokenFromMap(env map[string]string) string {
	if v := strings.TrimSpace(env["TIDE_API_TOKEN"]); v != "" {
		return v
	}
	legacyKey := LegacyAlias("TIDE_API_TOKEN")
	if v := strings.TrimSpace(env[legacyKey]); v != "" {
		warnOnce("map:"+legacyKey, "Deprecated token key in secrets file; migrate to TIDE_API_TOKEN", "deprecated", legacyKey)
		return v
	}
	return ""
}

// HasKeepLabel reports whether volume labels mark the volume as protected from pruning.
func HasKeepLabel(labels map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(labels["com.tide.keep"]), "true") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(labels[legacyKeepLabel]), "true") {
		warnOnce("label:"+legacyKeepLabel, "Deprecated volume label in use; migrate to com.tide.keep", "deprecated", legacyKeepLabel)
		return true
	}
	return false
}

// HasOneshotRerunLabel reports whether service labels request one-shot rerun on every deploy.
func HasOneshotRerunLabel(labels map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(labels["com.tide.oneshot"]), "rerun") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(labels[legacyOneshotLabel]), "rerun") {
		warnOnce("label:"+legacyOneshotLabel, "Deprecated service label in use; migrate to com.tide.oneshot", "deprecated", legacyOneshotLabel)
		return true
	}
	return false
}

// WebhookTokenHeader reads X-Tide-Webhook-Token or its legacy header alias.
func WebhookTokenHeader(getHeader func(string) string) string {
	if getHeader == nil {
		return ""
	}
	if v := strings.TrimSpace(getHeader("X-Tide-Webhook-Token")); v != "" {
		return v
	}
	if v := strings.TrimSpace(getHeader(legacyWebhookHdr)); v != "" {
		warnOnce("hdr:"+legacyWebhookHdr, "Deprecated webhook header in use; migrate to X-Tide-Webhook-Token", "deprecated", legacyWebhookHdr)
		return v
	}
	return ""
}

// PopulateInterpolationAliases adds transitional compose interpolation aliases for one release.
func PopulateInterpolationAliases(vars map[string]string, appName, project string) {
	if vars == nil {
		return
	}
	if appName != "" {
		vars[legacyEnvPrefix+"APP_NAME"] = appName
	}
	if project != "" {
		vars[legacyEnvPrefix+"PROJECT"] = project
	}
}

// SecretEnvKeys returns all primary and transitional secret environment variable names for log redaction.
func SecretEnvKeys() []string {
	primary := []string{
		"TIDE_API_TOKEN",
		"TIDE_OPERATOR_TOKENS",
		"TIDE_FORCE_TOKENS",
		"TIDE_TOKEN",
		"TIDE_GIT_TOKEN",
		"TIDE_GITLAB_API_TOKEN",
		"TIDE_FLEET_TOKEN",
		"TIDE_VAULT_TOKEN",
		"TIDE_REGISTRY_PASSWORD",
		"REGISTRY_PASSWORD",
		"TIDE_SOPS_AGE_KEY",
		"TIDE_ENCRYPTION_KEY",
		"TIDE_WEBHOOK_SECRET",
		"TIDE_HUB_TOKEN",
		"TIDE_METRICS_TOKEN",
	}
	out := make([]string, 0, len(primary)*2)
	for _, key := range primary {
		out = append(out, key)
		if alias := LegacyAlias(key); alias != "" {
			out = append(out, alias)
		}
	}
	return out
}
