package cmd

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/envcompat"
)

type APIClient struct {
	BaseURL string
	Token   string
	Actor   string
	Client  *http.Client
}

// discoverCAFile finds the controller CA when tidectl is not given one.
func discoverCAFile() string {
	return envcompat.DiscoverCAFile()
}

func resolveLocalActor() string {
	if v := envcompat.Getenv("TIDE_ACTOR"); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		name := strings.TrimSpace(u.Username)
		if idx := strings.LastIndex(name, `\`); idx >= 0 {
			name = name[idx+1:]
		}
		return name
	}
	return ""
}

func NewClient() *APIClient {
	url := strings.TrimRight(strings.TrimSpace(viper.GetString("url")), "/")
	token := strings.TrimSpace(viper.GetString("token"))
	insecure := viper.GetBool("insecure")
	caPath := strings.TrimSpace(viper.GetString("certificate-authority"))

	cfgPath := envcompat.ResolveCLIConfigPath(viper.GetString("config"))
	if cfg, err := loadConfig(cfgPath); err == nil {
		_, ctx, ctxErr := resolveActiveContext(cfg, viper.GetString("context"))
		if ctxErr == nil {
			if url == "" && ctx.URL != "" {
				url = strings.TrimRight(ctx.URL, "/")
			}
			if token == "" {
				token = strings.TrimSpace(ctx.Token)
			}
			if !insecure && ctx.InsecureSkipTLSVerify {
				insecure = true
			}
			if caPath == "" {
				caPath = strings.TrimSpace(ctx.CertificateAuthority)
			}
		}
	}
	if caPath == "" && !insecure {
		caPath = discoverCAFile()
	}

	if url == "" {
		url = "https://127.0.0.1:8443"
	}
	if token == "" {
		token = envcompat.FirstEnv("TIDE_TOKEN", "TIDE_API_TOKEN")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				tlsCfg.RootCAs = pool
			}
		}
	}
	transport.TLSClientConfig = tlsCfg

	timeout := 30 * time.Second
	if value := envcompat.Getenv("TIDE_SYNC_TIMEOUT"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > timeout {
			timeout = parsed + time.Minute
		}
	}

	return &APIClient{
		BaseURL: url,
		Token:   token,
		Actor:   resolveLocalActor(),
		Client:  &http.Client{Timeout: timeout, Transport: transport},
	}
}

func (c *APIClient) do(req *http.Request) (*http.Response, error) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Actor != "" && req.Header.Get("X-Tide-Actor") == "" {
		req.Header.Set("X-Tide-Actor", c.Actor)
	}
	return c.Client.Do(req)
}

func (c *APIClient) Get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *APIClient) Post(path string, body interface{}) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *APIClient) Patch(path string, body interface{}) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPatch, c.BaseURL+path, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *APIClient) Delete(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var apiResp api.APIResponse
	if err := json.Unmarshal(body, &apiResp); err == nil && apiResp.Message != "" {
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, apiResp.Message)
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("API error (%d): %s", resp.StatusCode, msg)
}

func listApps(c *APIClient) ([]api.App, error) {
	resp, err := c.Get("/api/v1/apps/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	var apiResp struct {
		Data []api.App `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode apps: %w", err)
	}
	if apiResp.Data == nil {
		return []api.App{}, nil
	}
	return apiResp.Data, nil
}

func resolveApp(c *APIClient, ref string) (*api.App, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("app name or id is required")
	}

	// Prefer direct get by id.
	resp, err := c.Get("/api/v1/apps/" + ref)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var apiResp struct {
				Data api.App `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&apiResp); err == nil && apiResp.Data.ID != "" {
				return &apiResp.Data, nil
			}
		}
	}

	apps, err := listApps(c)
	if err != nil {
		return nil, err
	}
	var matches []api.App
	for _, app := range apps {
		if app.ID == ref || strings.EqualFold(app.Name, ref) {
			matches = append(matches, app)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("app %q not found", ref)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("ambiguous app name %q (%d matches); use app id", ref, len(matches))
	}
}
