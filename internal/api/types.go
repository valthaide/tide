package api

import (
	"time"

	"github.com/valthaide/conops-private/internal/deploypolicy"
)

// App represents a Git repository configuration to track.
// Moved from controller/registry.go to avoid circular dependency with store package.
type App struct {
	ID                      string    `json:"id"`
	Name                    string    `json:"name"`
	RepoURL                 string    `json:"repo_url"`
	RepoAuthMethod          string    `json:"repo_auth_method"`
	Branch                  string    `json:"branch"`
	ComposePath             string    `json:"compose_path"`
	PollInterval            string    `json:"poll_interval"` // Duration string e.g. "30s"
	LastSeenCommit          string    `json:"last_seen_commit"`
	LastSeenCommitMessage   string    `json:"last_seen_commit_message"`
	LastSyncedCommit        string    `json:"last_synced_commit"`
	LastSyncedCommitMessage string    `json:"last_synced_commit_message"`
	LastSyncOutput          string    `json:"last_sync_output"`
	LastSyncError           string    `json:"last_sync_error"`
	LastSyncErrorCode       string    `json:"last_sync_error_code,omitempty"`
	LastSyncErrorStage      string    `json:"last_sync_error_stage,omitempty"`
	LastSyncAt              time.Time `json:"last_sync_at"`
	LastAppliedEnvHash      string    `json:"last_applied_env_hash,omitempty"`
	LastAppliedWatchHash    string    `json:"last_applied_watch_hash,omitempty"`
	DeployPolicyJSON        string    `json:"-"`
	RequirePipelineSuccess  bool      `json:"require_pipeline_success"`
	WatchFiles              []deploypolicy.WatchFile `json:"watch_files,omitempty"`
	Status                  string    `json:"status"` // e.g., "synced", "pending", "error"
	Suspended               bool      `json:"suspended"`
}

// Policy returns the decoded deploy policy for the app.
func (a *App) Policy() deploypolicy.Policy {
	if a == nil {
		return deploypolicy.Policy{}
	}
	if len(a.WatchFiles) > 0 || a.RequirePipelineSuccess {
		return deploypolicy.Policy{
			RequirePipelineSuccess: a.RequirePipelineSuccess,
			WatchFiles:             a.WatchFiles,
		}
	}
	return deploypolicy.Decode(a.DeployPolicyJSON)
}

// ApplyPolicy fills structured fields from stored JSON.
func (a *App) ApplyPolicy() {
	if a == nil {
		return
	}
	p := deploypolicy.Decode(a.DeployPolicyJSON)
	a.RequirePipelineSuccess = p.RequirePipelineSuccess
	a.WatchFiles = p.WatchFiles
}

// APIResponse is a standard wrapper for API responses.
type APIResponse struct {
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Container describes a compose service container runtime state.
type Container struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	Health  string `json:"health,omitempty"`
	Ports   string `json:"ports,omitempty"`
}

// HealthStatus is returned by GET /healthz.
type HealthStatus struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      string `json:"db"`
}

// Operation tracks an asynchronous reconcile.
type Operation struct {
	ID         string    `json:"id"`
	AppID      string    `json:"app_id"`
	AppName    string    `json:"app_name,omitempty"`
	Kind       string    `json:"kind"` // reconcile
	Status     string    `json:"status"` // pending|running|succeeded|failed
	Code       string    `json:"code,omitempty"`
	Stage      string    `json:"stage,omitempty"`
	Message    string    `json:"message,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}
