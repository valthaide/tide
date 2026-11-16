package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/redact"
	"github.com/valthaide/tide/internal/syncerr"
)

type AppSpec struct {
	Name   string
	Tenant string
	Paths  []string
	Keys   []string
	From   map[string]deploypolicy.SecretFrom
	File   string
}

type PathStatus struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
}

type AppStatus struct {
	App         string       `json:"app"`
	Provider    string       `json:"provider"`
	File        string       `json:"file"`
	Paths       []PathStatus `json:"paths,omitempty"`
	Stale       bool         `json:"stale"`
	LastSuccess time.Time    `json:"last_success,omitempty"`
	LastError   string       `json:"last_error,omitempty"`
}

type Status struct {
	Enabled         bool        `json:"enabled"`
	Provider        string      `json:"provider"`
	Auth            string      `json:"auth,omitempty"`
	Addr            string      `json:"addr,omitempty"`
	TokenTTLSeconds float64     `json:"token_ttl_seconds"`
	Apps            []AppStatus `json:"apps"`
}

type appState struct {
	versions    map[string]int
	data        map[string]map[string]string
	file        string
	stale       bool
	lastSuccess time.Time
	lastError   string
}

type Provider struct {
	cfg    Config
	client *baoClient
	logger *slog.Logger

	mu     sync.Mutex
	states map[string]*appState
}

func NewProvider(cfg Config, logger *slog.Logger) (*Provider, error) {
	p := &Provider{cfg: cfg, logger: logger, states: map[string]*appState{}}
	if cfg.OpenBaoConfigured() {
		client, err := newClient(cfg)
		if err != nil {
			return nil, err
		}
		p.client = client
	}
	return p, nil
}

func (p *Provider) Enabled() bool {
	return p != nil && p.client != nil
}

func (p *Provider) Config() Config {
	return p.cfg
}

func (p *Provider) ActiveFor(policyProvider string) bool {
	if p == nil || p.client == nil {
		return false
	}
	v := deploypolicy.NormalizeSecretsProvider(policyProvider)
	if v == "" {
		v = p.cfg.Provider
	}
	return v == deploypolicy.SecretsProviderOpenBao
}

func (p *Provider) RenderApp(ctx context.Context, spec AppSpec) error {
	if p == nil || p.client == nil {
		return syncerr.New(syncerr.CodeSecretsUnavailable, syncerr.StageSecrets, "secrets_unavailable: openbao provider is not configured")
	}
	app := spec.Name
	if err := p.client.ensure(ctx); err != nil {
		return p.renderFromCache(app, spec, err)
	}
	data := make(map[string]map[string]string, len(spec.Paths))
	versions := make(map[string]int, len(spec.Paths))
	var fetchErr error
	for _, path := range allPaths(spec) {
		values, version, err := p.client.readData(ctx, path)
		if err != nil {
			fetchErr = err
			if err == errPathMissing {
				p.noteError(app, spec, "path_missing", syncerr.CodeSecretsPathMissing, path)
				return syncerr.New(syncerr.CodeSecretsPathMissing, syncerr.StageSecrets, "secrets_path_missing: "+path)
			}
			break
		}
		data[path] = values
		versions[path] = version
	}
	if fetchErr != nil {
		return p.renderFromCache(app, spec, fetchErr)
	}
	out, err := mergeSecrets(spec, data)
	if err != nil {
		p.noteError(app, spec, "error", syncerr.Classify(err).Code, "")
		return err
	}
	if err := writeSecretFile(spec.File, out); err != nil {
		metrics.ObserveSecretsFetch(app, "error")
		return syncerr.Wrap(syncerr.CodeSecretsUnavailable, syncerr.StageSecrets, fmt.Errorf("write secret file: %w", err))
	}
	p.writeCache(app, data, versions)
	p.noteSuccess(app, spec, versions)
	return nil
}

func (p *Provider) renderFromCache(app string, spec AppSpec, cause error) error {
	data, versions, ok := p.readCache(app)
	if !ok {
		p.noteError(app, spec, "error", syncerr.CodeSecretsUnavailable, "")
		return syncerr.New(syncerr.CodeSecretsUnavailable, syncerr.StageSecrets,
			fmt.Sprintf("secrets_unavailable: openbao is unreachable and no cache exists for %s", app))
	}
	out, err := mergeSecrets(spec, data)
	if err != nil {
		p.noteError(app, spec, "error", syncerr.Classify(err).Code, "")
		return err
	}
	if err := writeSecretFile(spec.File, out); err != nil {
		metrics.ObserveSecretsFetch(app, "error")
		return syncerr.Wrap(syncerr.CodeSecretsUnavailable, syncerr.StageSecrets, fmt.Errorf("write secret file: %w", err))
	}
	p.mu.Lock()
	st := p.stateLocked(app)
	st.stale = true
	st.lastError = ""
	st.versions = versions
	st.data = data
	p.mu.Unlock()
	metrics.SetSecretsStale(app, true)
	metrics.ObserveSecretsFetch(app, "stale")
	if p.logger != nil {
		p.logger.Warn("secrets_stale: deploying from cached secrets", "app", app, "cause", cause.Error())
	}
	return nil
}

func (p *Provider) PollOnce(ctx context.Context, specs []AppSpec) {
	if p == nil || p.client == nil {
		return
	}
	if err := p.client.ensure(ctx); err != nil {
		metrics.SetSecretsProviderUp("openbao", false)
		if p.logger != nil {
			p.logger.Warn("secrets provider login unavailable", "error", err.Error())
		}
		return
	}
	metrics.SetSecretsProviderUp("openbao", true)
	for _, spec := range specs {
		p.pollApp(ctx, spec)
	}
}

func (p *Provider) pollApp(ctx context.Context, spec AppSpec) {
	app := spec.Name
	changed := false
	complete := true
	for _, path := range allPaths(spec) {
		version, err := p.client.readMetadata(ctx, path)
		if err != nil {
			complete = false
			if err == errPathMissing {
				p.noteError(app, spec, "path_missing", syncerr.CodeSecretsPathMissing, path)
			} else {
				p.noteError(app, spec, "error", "", path)
			}
			continue
		}
		p.mu.Lock()
		st := p.stateLocked(app)
		known := st.versions[path]
		p.mu.Unlock()
		metrics.SetSecretsVersion(app, path, version)
		if known == version {
			continue
		}
		values, dataVersion, err := p.client.readData(ctx, path)
		if err != nil {
			complete = false
			if err == errPathMissing {
				p.noteError(app, spec, "path_missing", syncerr.CodeSecretsPathMissing, path)
			} else {
				p.noteError(app, spec, "error", "", path)
			}
			continue
		}
		p.mu.Lock()
		st = p.stateLocked(app)
		st.data[path] = values
		st.versions[path] = dataVersion
		st.lastError = ""
		p.mu.Unlock()
		changed = true
	}
	if !changed || !complete {
		return
	}
	p.mu.Lock()
	st := p.stateLocked(app)
	data := make(map[string]map[string]string, len(st.data))
	for path, values := range st.data {
		data[path] = values
	}
	versions := make(map[string]int, len(st.versions))
	for path, version := range st.versions {
		versions[path] = version
	}
	p.mu.Unlock()
	out, err := mergeSecrets(spec, data)
	if err != nil {
		p.noteError(app, spec, "error", syncerr.Classify(err).Code, "")
		return
	}
	if err := writeSecretFile(spec.File, out); err != nil {
		metrics.ObserveSecretsFetch(app, "error")
		if p.logger != nil {
			p.logger.Error("Failed to write secret file", "app", app, "error", err)
		}
		return
	}
	p.writeCache(app, data, versions)
	p.noteSuccess(app, spec, versions)
}

func (p *Provider) Run(ctx context.Context, lister func() []AppSpec) {
	if p == nil || p.client == nil || lister == nil {
		return
	}
	if err := p.client.login(ctx); err != nil && p.logger != nil {
		p.logger.Warn("Initial openbao login failed; retrying with backoff", "error", err.Error())
	}
	p.PollOnce(ctx, lister())
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.PollOnce(ctx, lister())
		}
	}
}

func (p *Provider) Status() Status {
	st := Status{Enabled: p.Enabled(), Provider: p.cfg.Provider}
	if p == nil {
		return st
	}
	if p.client != nil {
		st.Auth = p.cfg.Auth
		st.Addr = p.cfg.Addr
		if ttl, ok := p.client.TokenTTL(); ok {
			st.TokenTTLSeconds = ttl.Seconds()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	apps := make([]string, 0, len(p.states))
	for app := range p.states {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		as := p.states[app]
		entry := AppStatus{App: app, Provider: st.Provider, File: as.file, Stale: as.stale, LastSuccess: as.lastSuccess, LastError: as.lastError}
		paths := make([]string, 0, len(as.versions))
		for path := range as.versions {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			entry.Paths = append(entry.Paths, PathStatus{Path: path, Version: as.versions[path]})
		}
		st.Apps = append(st.Apps, entry)
	}
	return st
}

func (p *Provider) noteSuccess(app string, spec AppSpec, versions map[string]int) {
	p.mu.Lock()
	st := p.stateLocked(app)
	st.stale = false
	st.file = spec.File
	st.lastSuccess = time.Now()
	st.lastError = ""
	for path, version := range versions {
		st.versions[path] = version
	}
	p.mu.Unlock()
	for path, version := range versions {
		metrics.SetSecretsVersion(app, path, version)
	}
	metrics.SetSecretsStale(app, false)
	metrics.SetSecretsLastSuccess(app, time.Now())
	metrics.ObserveSecretsFetch(app, "ok")
}

func (p *Provider) noteError(app string, spec AppSpec, result, code, path string) {
	p.mu.Lock()
	st := p.stateLocked(app)
	if st.file == "" {
		st.file = spec.File
	}
	if code != "" {
		st.lastError = code
		if path != "" {
			st.lastError = code + ": " + path
		}
	}
	p.mu.Unlock()
	if result != "" {
		metrics.ObserveSecretsFetch(app, result)
	}
}

func (p *Provider) stateLocked(app string) *appState {
	st, ok := p.states[app]
	if !ok {
		st = &appState{versions: map[string]int{}, data: map[string]map[string]string{}}
		p.states[app] = st
	}
	return st
}

func allPaths(spec AppSpec) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(path string) {
		path = strings.Trim(strings.TrimSpace(path), "/")
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	for _, path := range spec.Paths {
		add(path)
	}
	for _, from := range spec.From {
		add(from.Path)
	}
	return out
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func mergeSecrets(spec AppSpec, data map[string]map[string]string) (map[string]string, error) {
	merged := map[string]string{}
	for _, path := range spec.Paths {
		path = strings.Trim(strings.TrimSpace(path), "/")
		for key, value := range data[path] {
			merged[key] = value
		}
	}
	for outKey, from := range spec.From {
		src, ok := data[strings.Trim(strings.TrimSpace(from.Path), "/")]
		if !ok {
			return nil, syncerr.New(syncerr.CodeSecretsPathMissing, syncerr.StageSecrets, "secrets_path_missing: "+from.Path)
		}
		value, ok := src[from.Key]
		if !ok {
			return nil, syncerr.New(syncerr.CodeEnvKeyMissing, syncerr.StageSecrets, "env_key_missing: "+outKey)
		}
		merged[outKey] = value
	}
	out := map[string]string{}
	if len(spec.Keys) == 0 {
		out = merged
	} else {
		fromKeys := map[string]struct{}{}
		for outKey := range spec.From {
			fromKeys[outKey] = struct{}{}
		}
		for key, value := range merged {
			if _, ok := fromKeys[key]; ok {
				out[key] = value
			}
		}
		for _, key := range spec.Keys {
			if value, ok := merged[key]; ok {
				out[key] = value
			}
		}
	}
	for _, key := range spec.Keys {
		if strings.TrimSpace(out[key]) == "" {
			return nil, syncerr.New(syncerr.CodeEnvKeyMissing, syncerr.StageSecrets, "env_key_missing: "+key)
		}
	}
	keys := make([]string, 0, len(out))
	for key := range out {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !envKeyRe.MatchString(key) {
			return nil, syncerr.New(syncerr.CodeSecretsInvalidValue, syncerr.StageSecrets, "secrets_invalid_value: invalid key name "+key)
		}
		value := out[key]
		if strings.ContainsAny(value, "'\n\r\x00") {
			return nil, syncerr.New(syncerr.CodeSecretsInvalidValue, syncerr.StageSecrets, "secrets_invalid_value: key "+key)
		}
		if redact.Default != nil && len(value) >= 8 {
			redact.Default.AddSecret(value)
		}
	}
	return out, nil
}

func renderEnvFile(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("='")
		b.WriteString(values[key])
		b.WriteString("'\n")
	}
	return b.String()
}

func writeSecretFile(path string, values map[string]string) error {
	content := renderEnvFile(values)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0700)
	tmp, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type cachePayload struct {
	Paths    map[string]map[string]string `json:"paths"`
	Versions map[string]int               `json:"versions"`
}

func (p *Provider) cachePath(app string) string {
	return filepath.Join(p.cfg.Dir, ".cache", app+".json")
}

func (p *Provider) cacheKey() ([]byte, error) {
	raw, err := os.ReadFile(p.cfg.CacheKeyFile)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(raw) >= 32 {
		return raw[:32], nil
	}
	return nil, fmt.Errorf("cache key %s must be 32 bytes (hex or raw)", p.cfg.CacheKeyFile)
}

func (p *Provider) writeCache(app string, data map[string]map[string]string, versions map[string]int) {
	key, err := p.cacheKey()
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("secrets cache disabled", "app", app, "error", err.Error())
		}
		return
	}
	payload, err := json.Marshal(cachePayload{Paths: data, Versions: versions})
	if err != nil {
		return
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	sealed := gcm.Seal(nonce, nonce, payload, nil)
	path := p.cachePath(app)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	_ = os.Chmod(dir, 0700)
	if err := os.WriteFile(path, sealed, 0600); err != nil && p.logger != nil {
		p.logger.Warn("Failed to write secrets cache", "app", app, "error", err)
	}
}

func (p *Provider) readCache(app string) (map[string]map[string]string, map[string]int, bool) {
	key, err := p.cacheKey()
	if err != nil {
		return nil, nil, false
	}
	sealed, err := os.ReadFile(p.cachePath(app))
	if err != nil {
		return nil, nil, false
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, false
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, nil, false
	}
	payload, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return nil, nil, false
	}
	var decoded cachePayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, nil, false
	}
	if len(decoded.Paths) == 0 {
		return nil, nil, false
	}
	return decoded.Paths, decoded.Versions, true
}
