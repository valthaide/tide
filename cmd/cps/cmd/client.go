package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/valthaide/conops-private/internal/api"
)

type APIClient struct {
	BaseURL string
	Client  *http.Client
}

func NewClient() *APIClient {
	return &APIClient{
		BaseURL: strings.TrimRight(viper.GetString("url"), "/"),
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *APIClient) Get(path string) (*http.Response, error) {
	return c.Client.Get(c.BaseURL + path)
}

func (c *APIClient) Post(path string, body interface{}) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.Client.Post(c.BaseURL+path, "application/json", bytes.NewBuffer(payload))
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
	return c.Client.Do(req)
}

func (c *APIClient) Delete(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.Client.Do(req)
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
