package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/syncerr"
)

type mockBao struct {
	t          *testing.T
	data       map[string]map[string]string
	versions   map[string]int
	failData   atomic.Bool
	failLogin  atomic.Bool
	loginCalls atomic.Int32
	renewCalls atomic.Int32
	lastBody   atomic.Value
}

func newMockBao(t *testing.T) (*mockBao, *httptest.Server) {
	m := &mockBao{t: t, data: map[string]map[string]string{}, versions: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/auth/") && strings.HasSuffix(r.URL.Path, "/login"):
			m.loginCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.lastBody.Store(r.URL.Path + " " + fmt.Sprint(body))
			if m.failLogin.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"auth":{"client_token":"tok-1","lease_duration":3600}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/token/renew-self":
			m.renewCalls.Add(1)
			fmt.Fprint(w, `{"auth":{"lease_duration":3600}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kv/metadata/"):
			path := strings.TrimPrefix(r.URL.Path, "/v1/kv/metadata/")
			version, ok := m.versions[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, `{"data":{"current_version":%d}}`, version)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			if m.failData.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			path := strings.TrimPrefix(r.URL.Path, "/v1/kv/data/")
			values, ok := m.data[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			resp := map[string]any{"data": map[string]any{
				"data":     values,
				"metadata": map[string]any{"version": m.versions[path]},
			}}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return m, srv
}

func testConfig(t *testing.T, addr string) Config {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cache.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("ab", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{
		Provider:     deploypolicy.SecretsProviderOpenBao,
		Addr:         addr,
		Auth:         AuthToken,
		Token:        "test-token",
		Mount:        "kv",
		PollInterval: 0,
		Dir:          dir,
		CacheKeyFile: keyFile,
	}
}

func testSpec(t *testing.T, cfg Config) AppSpec {
	t.Helper()
	return AppSpec{
		Name:  "backend",
		Paths: []string{"shared/backend", "tenko/test-ru/backend"},
		File:  filepath.Join(cfg.Dir, "backend.env"),
	}
}

func TestRenderAppMergesPathsLastWins(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["shared/backend"] = map[string]string{"A": "1", "B": "shared"}
	m.versions["shared/backend"] = 3
	m.data["tenko/test-ru/backend"] = map[string]string{"B": "tenant", "C": "3"}
	m.versions["tenko/test-ru/backend"] = 7

	cfg := testConfig(t, srv.URL)
	p, err := NewProvider(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t, cfg)
	if err := p.RenderApp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(spec.File)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	if !strings.Contains(got, "B='tenant'") {
		t.Fatalf("last path must win, file:\n%s", got)
	}
	if !strings.Contains(got, "A='1'") || !strings.Contains(got, "C='3'") {
		t.Fatalf("merged keys missing, file:\n%s", got)
	}
	info, err := os.Stat(spec.File)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o want 600", info.Mode().Perm())
	}
	st := p.Status()
	if len(st.Apps) != 1 || st.Apps[0].Stale {
		t.Fatalf("status=%+v", st.Apps)
	}
	seen := map[string]int{}
	for _, ps := range st.Apps[0].Paths {
		seen[ps.Path] = ps.Version
	}
	if seen["shared/backend"] != 3 || seen["tenko/test-ru/backend"] != 7 {
		t.Fatalf("versions=%v", seen)
	}
}

func TestRenderAppSecretKeysAndFrom(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["p/backend"] = map[string]string{"KEEP": "1", "DROP": "2", "ADMIN": "root-secret"}
	m.versions["p/backend"] = 1
	cfg := testConfig(t, srv.URL)
	p, err := NewProvider(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := AppSpec{
		Name:  "backend",
		Paths: []string{"p/backend"},
		Keys:  []string{"KEEP"},
		From:  map[string]deploypolicy.SecretFrom{"HASURA_ADMIN_SECRET": {Path: "p/backend", Key: "ADMIN"}},
		File:  filepath.Join(cfg.Dir, "backend.env"),
	}
	if err := p.RenderApp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(spec.File)
	got := string(content)
	if !strings.Contains(got, "KEEP='1'") || !strings.Contains(got, "HASURA_ADMIN_SECRET='root-secret'") {
		t.Fatalf("file:\n%s", got)
	}
	if strings.Contains(got, "DROP") {
		t.Fatalf("secret_keys filter failed, file:\n%s", got)
	}
}

func TestRenderAppSecretKeysMissingFails(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["p/backend"] = map[string]string{"OTHER": "1"}
	m.versions["p/backend"] = 1
	cfg := testConfig(t, srv.URL)
	p, _ := NewProvider(cfg, nil)
	spec := AppSpec{Name: "backend", Paths: []string{"p/backend"}, Keys: []string{"NEED"}, File: filepath.Join(cfg.Dir, "backend.env")}
	err := p.RenderApp(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error")
	}
	se := syncerr.Classify(err)
	if se.Code != syncerr.CodeEnvKeyMissing {
		t.Fatalf("code=%s", se.Code)
	}
	if _, statErr := os.Stat(spec.File); !os.IsNotExist(statErr) {
		t.Fatal("file must not be written on failure")
	}
}

func TestRenderAppInvalidValueRejected(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["p/backend"] = map[string]string{"BAD": "has'quote", "NL": "two\nlines"}
	m.versions["p/backend"] = 1
	cfg := testConfig(t, srv.URL)
	p, _ := NewProvider(cfg, nil)
	spec := AppSpec{Name: "backend", Paths: []string{"p/backend"}, File: filepath.Join(cfg.Dir, "backend.env")}
	err := p.RenderApp(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error")
	}
	se := syncerr.Classify(err)
	if se.Code != syncerr.CodeSecretsInvalidValue {
		t.Fatalf("code=%s", se.Code)
	}
	if strings.Contains(se.Message, "has'quote") || strings.Contains(se.Message, "two") {
		t.Fatalf("value leaked into message: %s", se.Message)
	}
	if !strings.Contains(se.Message, "BAD") && !strings.Contains(se.Message, "NL") {
		t.Fatalf("key name missing from message: %s", se.Message)
	}
}

func TestRenderAppPathMissing(t *testing.T) {
	_, srv := newMockBao(t)
	cfg := testConfig(t, srv.URL)
	p, _ := NewProvider(cfg, nil)
	spec := AppSpec{Name: "backend", Paths: []string{"nope"}, File: filepath.Join(cfg.Dir, "backend.env")}
	err := p.RenderApp(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error")
	}
	if se := syncerr.Classify(err); se.Code != syncerr.CodeSecretsPathMissing {
		t.Fatalf("code=%s", se.Code)
	}
}

func TestRenderAppStaleCacheFallback(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["p/backend"] = map[string]string{"A": "cached-value"}
	m.versions["p/backend"] = 2
	cfg := testConfig(t, srv.URL)
	p, _ := NewProvider(cfg, nil)
	spec := AppSpec{Name: "backend", Paths: []string{"p/backend"}, File: filepath.Join(cfg.Dir, "backend.env")}
	if err := p.RenderApp(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	m.failData.Store(true)
	if err := p.RenderApp(context.Background(), spec); err != nil {
		t.Fatalf("cache fallback must succeed: %v", err)
	}
	st := p.Status()
	if len(st.Apps) != 1 || !st.Apps[0].Stale {
		t.Fatalf("expected stale state: %+v", st.Apps)
	}
}

func TestRenderAppUnavailableWithoutCache(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["p/backend"] = map[string]string{"A": "1"}
	m.versions["p/backend"] = 1
	m.failData.Store(true)
	cfg := testConfig(t, srv.URL)
	p, _ := NewProvider(cfg, nil)
	spec := AppSpec{Name: "backend", Paths: []string{"p/backend"}, File: filepath.Join(cfg.Dir, "backend.env")}
	err := p.RenderApp(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error")
	}
	if se := syncerr.Classify(err); se.Code != syncerr.CodeSecretsUnavailable {
		t.Fatalf("code=%s", se.Code)
	}
}

func TestPollOnceReactsOnVersionChange(t *testing.T) {
	m, srv := newMockBao(t)
	m.data["p/backend"] = map[string]string{"A": "v1"}
	m.versions["p/backend"] = 1
	cfg := testConfig(t, srv.URL)
	p, _ := NewProvider(cfg, nil)
	spec := AppSpec{Name: "backend", Paths: []string{"p/backend"}, File: filepath.Join(cfg.Dir, "backend.env")}

	p.PollOnce(context.Background(), []AppSpec{spec})
	content, _ := os.ReadFile(spec.File)
	if !strings.Contains(string(content), "A='v1'") {
		t.Fatalf("file after first poll:\n%s", content)
	}

	m.data["p/backend"] = map[string]string{"A": "v2"}
	m.versions["p/backend"] = 2
	p.PollOnce(context.Background(), []AppSpec{spec})
	content, _ = os.ReadFile(spec.File)
	if !strings.Contains(string(content), "A='v2'") {
		t.Fatalf("file after version change:\n%s", content)
	}

	delete(m.data, "p/backend")
	delete(m.versions, "p/backend")
	p.PollOnce(context.Background(), []AppSpec{spec})
	content, err := os.ReadFile(spec.File)
	if err != nil || !strings.Contains(string(content), "A='v2'") {
		t.Fatalf("deleted path must keep the file: err=%v file=%s", err, content)
	}
}

func TestLoginAuthMethods(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(Config) Config
		want string
	}{
		{"cert", func(c Config) Config { c.Auth = AuthCert; c.CertRole = "tide"; return c }, "/v1/auth/cert/login map[name:tide]"},
		{"approle", func(c Config) Config {
			dir := t.TempDir()
			roleFile := filepath.Join(dir, "role_id")
			secretFile := filepath.Join(dir, "secret_id")
			_ = os.WriteFile(roleFile, []byte("rid"), 0600)
			_ = os.WriteFile(secretFile, []byte("sid"), 0600)
			c.Auth = AuthApprole
			c.RoleIDFile = roleFile
			c.SecretIDFile = secretFile
			return c
		}, "/v1/auth/approle/login map[role_id:rid secret_id:sid]"},
		{"jwt", func(c Config) Config {
			dir := t.TempDir()
			jwtFile := filepath.Join(dir, "token.jwt")
			_ = os.WriteFile(jwtFile, []byte("jwt-value"), 0600)
			c.Auth = AuthJWT
			c.JWTFile = jwtFile
			c.JWTRole = "tide"
			return c
		}, "/v1/auth/jwt/login map[jwt:jwt-value role:tide]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, srv := newMockBao(t)
			cfg := tc.cfg(testConfig(t, srv.URL))
			p, err := NewProvider(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.client.login(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, _ := m.lastBody.Load().(string)
			if got != tc.want {
				t.Fatalf("login request=%q want %q", got, tc.want)
			}
			if ttl, ok := p.client.TokenTTL(); !ok || ttl <= 0 {
				t.Fatalf("ttl=%v ok=%v", ttl, ok)
			}
		})
	}
}

func TestTokenRenew(t *testing.T) {
	m, srv := newMockBao(t)
	cfg := testConfig(t, srv.URL)
	cfg.Auth = AuthCert
	p, _ := NewProvider(cfg, nil)
	if err := p.client.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.client.mu.Lock()
	p.client.renewAt = time.Now().Add(-time.Hour)
	p.client.mu.Unlock()
	if err := p.client.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.renewCalls.Load() != 1 {
		t.Fatalf("renewCalls=%d", m.renewCalls.Load())
	}
}

func TestActiveFor(t *testing.T) {
	m, srv := newMockBao(t)
	_ = m
	cfg := testConfig(t, srv.URL)
	cfg.Provider = deploypolicy.SecretsProviderNone
	p, _ := NewProvider(cfg, nil)
	if !p.ActiveFor("openbao") {
		t.Fatal("app-level openbao must activate the provider")
	}
	if p.ActiveFor("agent") {
		t.Fatal("agent must not activate the provider")
	}
	if p.ActiveFor("") {
		t.Fatal("empty policy with global none must not activate")
	}
	cfg.Provider = deploypolicy.SecretsProviderOpenBao
	p2, _ := NewProvider(cfg, nil)
	if !p2.ActiveFor("") {
		t.Fatal("global openbao must activate")
	}
}
