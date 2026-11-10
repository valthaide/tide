package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/envcompat"
)

// Mode values for TIDE_MODE.
const (
	ModeStandalone = "standalone"
	ModeAgent      = "agent"
	ModeHub        = "hub"
)

// ResolveMode returns TIDE_MODE or standalone.
func ResolveMode() string {
	m := strings.ToLower(envcompat.Getenv("TIDE_MODE"))
	switch m {
	case ModeAgent, ModeHub, ModeStandalone:
		return m
	default:
		return ModeStandalone
	}
}

// AppSummary is a compact app status for hub aggregation.
type AppSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"error_code,omitempty"`
}

// Heartbeat is posted by agents to the hub.
type Heartbeat struct {
	Host      string       `json:"host"`
	Version   string       `json:"version"`
	Timestamp time.Time    `json:"timestamp"`
	Apps      []AppSummary `json:"apps"`
}

// AppLister lists local apps for heartbeat.
type AppLister interface {
	List() []*api.App
}

// Reporter periodically sends heartbeats to a hub.
type Reporter struct {
	HubURL   string
	Token    string
	Host     string
	Version  string
	Interval time.Duration
	Apps     AppLister
	Logger   *slog.Logger
	Client   *http.Client
}

// NewReporterFromEnv builds an agent reporter when TIDE_HUB_URL is set.
func NewReporterFromEnv(apps AppLister, version string, logger *slog.Logger) *Reporter {
	hub := strings.TrimRight(envcompat.Getenv("TIDE_HUB_URL"), "/")
	if hub == "" {
		return nil
	}
	host := envcompat.Getenv("TIDE_AGENT_HOST")
	if host == "" {
		host, _ = os.Hostname()
	}
	interval := 30 * time.Second
	if v := envcompat.Getenv("TIDE_AGENT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	token := envcompat.FirstEnv("TIDE_HUB_TOKEN", "TIDE_API_TOKEN")
	return &Reporter{
		HubURL:   hub,
		Token:    token,
		Host:     host,
		Version:  version,
		Interval: interval,
		Apps:     apps,
		Logger:   logger,
		Client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Run sends heartbeats until ctx is done.
func (r *Reporter) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.send(ctx)
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.send(ctx)
		}
	}
}

func (r *Reporter) send(ctx context.Context) {
	apps := r.Apps.List()
	summaries := make([]AppSummary, 0, len(apps))
	for _, a := range apps {
		if a == nil {
			continue
		}
		summaries = append(summaries, AppSummary{
			ID: a.ID, Name: a.Name, Status: a.Status,
			Error: a.LastSyncError, Code: a.LastSyncErrorCode,
		})
	}
	hb := Heartbeat{Host: r.Host, Version: r.Version, Timestamp: time.Now().UTC(), Apps: summaries}
	payload, _ := json.Marshal(hb)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.HubURL+"/api/v1/agents/heartbeat", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("Agent heartbeat failed", "error", err)
		}
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 && r.Logger != nil {
		r.Logger.Warn("Agent heartbeat rejected", "status", resp.StatusCode)
	}
}

// HubStore keeps last heartbeats from agents (in-memory).
type HubStore struct {
	mu     sync.Mutex
	byHost map[string]Heartbeat
}

// NewHubStore creates an empty hub store.
func NewHubStore() *HubStore {
	return &HubStore{byHost: map[string]Heartbeat{}}
}

// Put stores a heartbeat.
func (s *HubStore) Put(hb Heartbeat) {
	if strings.TrimSpace(hb.Host) == "" {
		hb.Host = "unknown"
	}
	s.mu.Lock()
	s.byHost[hb.Host] = hb
	s.mu.Unlock()
}

// List returns all agent heartbeats.
func (s *HubStore) List() []Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Heartbeat, 0, len(s.byHost))
	for _, hb := range s.byHost {
		out = append(out, hb)
	}
	return out
}

// Handler serves hub API endpoints.
type Handler struct {
	Store  *HubStore
	Logger *slog.Logger
}

// Heartbeat handles POST /api/v1/agents/heartbeat
func (h *Handler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	var hb Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if hb.Timestamp.IsZero() {
		hb.Timestamp = time.Now().UTC()
	}
	h.Store.Put(hb)
	w.WriteHeader(http.StatusNoContent)
}

// ListAgents handles GET /api/v1/agents
func (h *Handler) ListAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.APIResponse{Data: h.Store.List()})
}
