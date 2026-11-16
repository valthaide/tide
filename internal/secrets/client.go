package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/valthaide/tide/internal/metrics"
)

var errPathMissing = errors.New("secrets_path_missing")

type baoClient struct {
	cfg  Config
	http *http.Client

	mu        sync.Mutex
	token     string
	ttl       time.Duration
	renewAt   time.Time
	nextLogin time.Time
	backoff   time.Duration
}

func newClient(cfg Config) (*baoClient, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSServerName != "" {
		tlsCfg.ServerName = cfg.TLSServerName
	}
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("read TIDE_OPENBAO_CACERT: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TIDE_OPENBAO_CACERT %s: no PEM certificates", cfg.CACert)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.ClientCert != "" || cfg.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("load OpenBao client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &baoClient{
		cfg: cfg,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

func (c *baoClient) login(ctx context.Context) error {
	c.mu.Lock()
	if !c.nextLogin.IsZero() && time.Now().Before(c.nextLogin) {
		wait := c.backoff
		c.mu.Unlock()
		return fmt.Errorf("secrets login backoff (%s)", wait)
	}
	c.mu.Unlock()

	var token string
	var ttl time.Duration
	switch c.cfg.Auth {
	case AuthToken:
		if c.cfg.Token == "" {
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login: TIDE_OPENBAO_TOKEN is empty")
		}
		token, ttl = c.cfg.Token, 0
	case AuthCert:
		body := map[string]string{}
		if c.cfg.CertRole != "" {
			body["name"] = c.cfg.CertRole
		}
		t, lease, err := c.authRequest(ctx, "/v1/auth/cert/login", body)
		if err != nil {
			c.noteLoginFailure()
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login cert: %w", err)
		}
		token, ttl = t, lease
	case AuthApprole:
		roleID, err := readSecretFile(c.cfg.RoleIDFile)
		if err != nil {
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login approle: %w", err)
		}
		secretID, err := readSecretFile(c.cfg.SecretIDFile)
		if err != nil {
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login approle: %w", err)
		}
		t, lease, err := c.authRequest(ctx, "/v1/auth/approle/login", map[string]string{"role_id": roleID, "secret_id": secretID})
		if err != nil {
			c.noteLoginFailure()
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login approle: %w", err)
		}
		token, ttl = t, lease
	case AuthJWT:
		jwt, err := readSecretFile(c.cfg.JWTFile)
		if err != nil {
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login jwt: %w", err)
		}
		t, lease, err := c.authRequest(ctx, "/v1/auth/jwt/login", map[string]string{"jwt": jwt, "role": c.cfg.JWTRole})
		if err != nil {
			c.noteLoginFailure()
			metrics.ObserveSecretsLogin("error")
			return fmt.Errorf("secrets login jwt: %w", err)
		}
		token, ttl = t, lease
	default:
		return fmt.Errorf("secrets login: unsupported auth %q", c.cfg.Auth)
	}

	c.mu.Lock()
	c.token = token
	c.ttl = ttl
	if ttl > 0 {
		c.renewAt = time.Now().Add(ttl * 2 / 3)
	} else {
		c.renewAt = time.Time{}
	}
	c.backoff = 0
	c.nextLogin = time.Time{}
	c.mu.Unlock()
	metrics.ObserveSecretsLogin("success")
	metrics.SetSecretsTokenTTL(ttl.Seconds())
	return nil
}

func (c *baoClient) noteLoginFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backoff < time.Second {
		c.backoff = time.Second
	} else {
		c.backoff *= 2
		if c.backoff > 5*time.Minute {
			c.backoff = 5 * time.Minute
		}
	}
	c.nextLogin = time.Now().Add(c.backoff)
}

func (c *baoClient) ensure(ctx context.Context) error {
	c.mu.Lock()
	token := c.token
	ttl := c.ttl
	renewAt := c.renewAt
	c.mu.Unlock()
	if token == "" {
		return c.login(ctx)
	}
	if ttl <= 0 || time.Now().Before(renewAt) {
		return nil
	}
	lease, err := c.renewRequest(ctx)
	if err != nil {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		return c.login(ctx)
	}
	c.mu.Lock()
	c.ttl = lease
	if lease > 0 {
		c.renewAt = time.Now().Add(lease * 2 / 3)
	}
	c.mu.Unlock()
	metrics.SetSecretsTokenTTL(lease.Seconds())
	return nil
}

func (c *baoClient) TokenTTL() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl, c.token != ""
}

func (c *baoClient) authRequest(ctx context.Context, path string, body map[string]string) (string, time.Duration, error) {
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, path, body, &out, false); err != nil {
		return "", 0, err
	}
	if out.Auth.ClientToken == "" {
		return "", 0, fmt.Errorf("empty client token")
	}
	return out.Auth.ClientToken, time.Duration(out.Auth.LeaseDuration) * time.Second, nil
}

func (c *baoClient) renewRequest(ctx context.Context) (time.Duration, error) {
	var out struct {
		Auth struct {
			LeaseDuration int `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/token/renew-self", map[string]any{}, &out, true); err != nil {
		return 0, err
	}
	return time.Duration(out.Auth.LeaseDuration) * time.Second, nil
}

func (c *baoClient) readMetadata(ctx context.Context, path string) (int, error) {
	var out struct {
		Data struct {
			CurrentVersion int `json:"current_version"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/"+c.cfg.Mount+"/metadata/"+strings.TrimPrefix(path, "/"), nil, &out, true); err != nil {
		return 0, err
	}
	return out.Data.CurrentVersion, nil
}

func (c *baoClient) readData(ctx context.Context, path string) (map[string]string, int, error) {
	var out struct {
		Data struct {
			Data     map[string]any `json:"data"`
			Metadata struct {
				Version int `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/"+c.cfg.Mount+"/data/"+strings.TrimPrefix(path, "/"), nil, &out, true); err != nil {
		return nil, 0, err
	}
	values := make(map[string]string, len(out.Data.Data))
	for k, v := range out.Data.Data {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		values[k] = s
	}
	return values, out.Data.Metadata.Version, nil
}

func (c *baoClient) do(ctx context.Context, method, path string, body any, out any, withToken bool) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Addr+path, reader)
	if err != nil {
		return err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken {
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()
		if token == "" {
			return fmt.Errorf("secrets token missing")
		}
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openbao request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return errPathMissing
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("openbao status %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("openbao response: %w", err)
		}
	}
	return nil
}

func readSecretFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("file %s is empty", path)
	}
	return v, nil
}
