package api

import (
	"strings"
	"time"

	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/gitrev"
)

// App represents a Git repository configuration to track.
// Moved from controller/registry.go to avoid circular dependency with store package.
type App struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	RepoURL        string `json:"repo_url"`
	RepoAuthMethod string `json:"repo_auth_method"`
	Branch         string `json:"branch"`
	// Revision is an optional fleet ref: branch name, tag, or full SHA. Empty tracks Branch.
	Revision    string `json:"revision,omitempty"`
	ComposePath string `json:"compose_path"`
	// Template selects templates/<Template>/compose.yaml from the fleet
	// checkout as the compose source, merged with the app's optional
	// compose.override.yaml. Mutually exclusive with ComposePath: a template
	// app stores an empty ComposePath. Empty means compose_path mode.
	Template string `json:"template,omitempty"`
	// AppDir is the folder of the application inside its own repository in
	// template mode (e.g. "backend", "tools/pgadmin"). Empty means the
	// repository root. .env, env_file, and compose.override.yaml resolve
	// from this folder, and it is the compose --project-directory.
	AppDir string `json:"app_dir,omitempty"`
	// LastAppliedTemplateHash is the hash of the template + override source
	// at the last successful apply (template mode only).
	LastAppliedTemplateHash string `json:"-"`
	// HasOverride reports whether the app checkout currently provides
	// compose.override.yaml. Filled on read, not stored.
	HasOverride             bool                     `json:"has_override,omitempty"`
	PollInterval            string                   `json:"poll_interval"` // Duration string e.g. "30s"
	LastSeenCommit          string                   `json:"last_seen_commit"`
	LastSeenCommitMessage   string                   `json:"last_seen_commit_message"`
	LastSyncedCommit        string                   `json:"last_synced_commit"`
	LastSyncedCommitMessage string                   `json:"last_synced_commit_message"`
	LastSyncOutput          string                   `json:"last_sync_output"`
	LastSyncError           string                   `json:"last_sync_error"`
	LastSyncErrorCode       string                   `json:"last_sync_error_code,omitempty"`
	LastSyncErrorStage      string                   `json:"last_sync_error_stage,omitempty"`
	LastSyncAt              time.Time                `json:"last_sync_at"`
	LastAppliedEnvHash      string                   `json:"last_applied_env_hash,omitempty"`
	LastAppliedWatchHash    string                   `json:"last_applied_watch_hash,omitempty"`
	DeployPolicyJSON        string                   `json:"-"`
	RequirePipelineSuccess  bool                     `json:"require_pipeline_success"`
	WatchFiles              []deploypolicy.WatchFile `json:"watch_files,omitempty"`
	Status                  string                   `json:"status"` // e.g., "synced", "pending", "error"
	// CI gate observability fields (persisted for status display).
	CIStatus     string    `json:"ci_status,omitempty"`
	CIReason     string    `json:"ci_reason,omitempty"`
	CIURL        string    `json:"ci_url,omitempty"`
	CISource     string    `json:"ci_source,omitempty"`
	CIPipelineID int64     `json:"ci_pipeline_id,omitempty"`
	CICheckedAt  time.Time `json:"ci_checked_at,omitempty"`
	// Suspended is true when git or an operator pause blocks automatic reconcile.
	Suspended         bool `json:"suspended"`
	FleetSuspended    bool `json:"fleet_suspended"`
	OperatorSuspended bool `json:"operator_suspended"`
	// PruneVolumes is the last fleet value. Named volumes are removed on delete only when this is true.
	PruneVolumes bool `json:"prune_volumes"`
	// HealthTimeout is the fleet duration string; empty uses DefaultHealthTimeout.
	HealthTimeout string `json:"health_timeout,omitempty"`
	// LastHealthyCommit is the last revision whose compose healthchecks passed.
	LastHealthyCommit string `json:"last_healthy_commit,omitempty"`
	// RejectedCommit is a revision that failed its health timeout and must not be applied again
	// until git moves to a different commit.
	RejectedCommit string `json:"rejected_commit,omitempty"`
	// LastHealthyVars is the fleet vars JSON of the last revision that became healthy.
	LastHealthyVars string `json:"last_healthy_vars,omitempty"`
	// RejectedVars is the fleet vars JSON that failed and must not be applied again
	// until the desired vars change.
	RejectedVars string `json:"rejected_vars,omitempty"`
	// Images are the containers actually running for this app. Filled on read, not stored.
	Images []ImageRef `json:"images,omitempty"`
	// Volumes are named volumes for this app. Filled on read, not stored.
	Volumes []VolumeRef `json:"volumes,omitempty"`
	// PublicVars are fleet variables safe to show. Filled on read, not stored.
	PublicVars map[string]string `json:"public_vars,omitempty"`
	// Tags is the public _TAG and _IMAGE values, for table output.
	Tags string `json:"tags,omitempty"`
	// Oneshot is the latest one-shot status, such as "migrate completed exit 0".
	Oneshot string `json:"oneshot,omitempty"`
	// FleetRevision is the fleet checkout SHA observed by this controller.
	FleetRevision string `json:"fleet_revision,omitempty"`
	// Schema is the additive app document version.
	Schema int `json:"schema,omitempty"`
	// Trigger is why this app is queued. Filled on read.
	Trigger string `json:"trigger,omitempty"`
	// Queue is the queue position, empty when the app is not queued.
	Queue string `json:"queue,omitempty"`
	// LastReconcile is the last cheap or applied reconcile.
	LastReconcile time.Time `json:"last_reconcile_at,omitempty"`
	// NextReconcile is the next periodic reconcile.
	NextReconcile time.Time `json:"next_reconcile_at,omitempty"`
	// Attempt is the current retry attempt.
	Attempt int `json:"attempt,omitempty"`
	// QueueClass is the trigger class while the app is queued.
	QueueClass int `json:"queue_class,omitempty"`
	// Priority is the fleet priority used inside a trigger class.
	Priority int `json:"priority,omitempty"`
}

// EffectivePipelineRef returns the git ref expected on pipelines for this app,
// or empty when tracking a raw commit SHA pin without an explicit pipeline_ref.
func (a *App) EffectivePipelineRef() string {
	if a == nil {
		return ""
	}
	if explicit := strings.TrimSpace(a.Policy().PipelineRef); explicit != "" {
		return explicit
	}
	rev := strings.TrimSpace(a.Revision)
	if rev != "" {
		if gitrev.IsFullSHA(rev) {
			return ""
		}
		return rev
	}
	return strings.TrimSpace(a.Branch)
}

// VolumeRef is one named volume used by an app.
type VolumeRef struct {
	Name       string `json:"name"`
	Containers string `json:"containers,omitempty"`
}

// Revision is one successful sync kept for history. It is not an apps column.
type Revision struct {
	AppID      string    `json:"-"`
	GitSHA     string    `json:"git_sha"`
	VarsJSON   string    `json:"vars_json,omitempty"`
	FleetSHA   string    `json:"fleet_sha,omitempty"`
	ImagesJSON string    `json:"images,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// DeployEvent is one applied, failed, or rolled-back deploy. It is not an apps column.
type DeployEvent struct {
	AppID     string    `json:"-"`
	App       string    `json:"app,omitempty"`
	Project   string    `json:"project,omitempty"`
	GitSHA    string    `json:"git_sha,omitempty"`
	FleetSHA  string    `json:"fleet_sha,omitempty"`
	VarsJSON  string    `json:"vars_json,omitempty"`
	Images    string    `json:"images,omitempty"`
	Result    string    `json:"result"`
	Trigger   string    `json:"trigger,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AuditEvent is an immutable security/operational audit record persisted in the database.
type AuditEvent struct {
	ID             int64     `json:"id"`
	CreatedAt      time.Time `json:"created_at"`
	Kind           string    `json:"kind"` // ci_force_bypass | repo_migration
	Actor          string    `json:"actor"`
	AppID          string    `json:"app_id"`
	AppName        string    `json:"app_name"`
	RepoURL        string    `json:"repo_url"`
	TargetRepoURL  string    `json:"target_repo_url,omitempty"`
	GitSHA         string    `json:"git_sha,omitempty"`
	PipelineID     int64     `json:"pipeline_id,omitempty"`
	PipelineSource string    `json:"pipeline_source,omitempty"`
	PipelineStatus string    `json:"pipeline_status,omitempty"`
	PipelineURL    string    `json:"pipeline_url,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Decision       string    `json:"decision"`
	DetailsJSON    string    `json:"details_json,omitempty"`
}

// RepoMigrationRequest describes a production-safe repository / source migration.
type RepoMigrationRequest struct {
	RepoURL        string `json:"repo_url"`
	RepoAuthMethod string `json:"repo_auth_method,omitempty"`
	DeployKey      string `json:"deploy_key,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Revision       string `json:"revision,omitempty"`
	ComposePath    string `json:"compose_path,omitempty"`
	// Template switches the compose source to a fleet template (alternative
	// to ComposePath). FromFleet requests treat it as authoritative,
	// including empty (switch back to compose_path).
	Template string `json:"template,omitempty"`
	// AppDir is the target app folder inside the repository (template
	// mode). AppDirSet distinguishes "keep current" from "set to root".
	AppDir       string            `json:"app_dir,omitempty"`
	AppDirSet    bool              `json:"app_dir_set,omitempty"`
	PollInterval string            `json:"poll_interval,omitempty"`
	Vars         map[string]string `json:"vars,omitempty"`
	DryRun       bool              `json:"dry_run,omitempty"`
	// AllowRestart permits applying a migration whose plan is `restarted`.
	// Without it a non-dry-run migration applies only a no_restart plan.
	AllowRestart bool   `json:"allow_restart,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Actor        string `json:"actor,omitempty"`
	FromFleet    bool   `json:"-"`
}

// RepoMigrationPlan is returned by dry-run and completed repository migrations.
type RepoMigrationPlan struct {
	AppID          string `json:"app_id"`
	AppName        string `json:"app_name"`
	OldRepoURL     string `json:"old_repo_url"`
	NewRepoURL     string `json:"new_repo_url"`
	OldBranch      string `json:"old_branch,omitempty"`
	Branch         string `json:"branch"`
	Revision       string `json:"revision,omitempty"`
	TargetCommit   string `json:"target_commit"`
	OldComposePath string `json:"old_compose_path,omitempty"`
	ComposePath    string `json:"compose_path"`
	OldTemplate    string `json:"old_template,omitempty"`
	Template       string `json:"template,omitempty"`
	OldAppDir      string `json:"old_app_dir,omitempty"`
	AppDir         string `json:"app_dir,omitempty"`
	// HasOverride reports whether the target app checkout provides
	// compose.override.yaml (template mode).
	HasOverride       bool     `json:"has_override,omitempty"`
	ComposeProject    string   `json:"compose_project"`
	CIStatus          string   `json:"ci_status"`
	CIPipelineID      int64    `json:"ci_pipeline_id,omitempty"`
	CIPipelineURL     string   `json:"ci_pipeline_url,omitempty"`
	Mode              string   `json:"mode,omitempty"`
	Strategy          string   `json:"strategy,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	ChangedServices   []string `json:"changed_services,omitempty"`
	UnchangedServices []string `json:"unchanged_services,omitempty"`
	DiffSummary       []string `json:"diff_summary,omitempty"`
	ToStop            []string `json:"to_stop"`
	ToRemove          []string `json:"to_remove"`
	KeptVolumes       []string `json:"kept_volumes"`
	ToCreate          []string `json:"to_create"`
	ImagesVerified    []string `json:"images_verified"`
	DryRun            bool     `json:"dry_run"`
	Applied           bool     `json:"applied"`
}

// ImageRef is one running compose service image.
type ImageRef struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	Tag     string `json:"tag,omitempty"`
	Digest  string `json:"digest"`
}

// DefaultHealthTimeout is how long a new revision may stay unhealthy before rollback.
const DefaultHealthTimeout = 120 * time.Second

// Policy returns the decoded deploy policy for the app.
func (a *App) Policy() deploypolicy.Policy {
	if a == nil {
		return deploypolicy.Policy{}
	}
	if strings.TrimSpace(a.DeployPolicyJSON) != "" {
		return deploypolicy.Decode(a.DeployPolicyJSON)
	}
	return deploypolicy.Policy{
		RequirePipelineSuccess: a.RequirePipelineSuccess,
		WatchFiles:             a.WatchFiles,
		PruneVolumes:           a.PruneVolumes,
		HealthTimeout:          a.HealthTimeout,
	}
}

// ApplyPolicy fills structured fields from stored JSON.
func (a *App) ApplyPolicy() {
	if a == nil {
		return
	}
	p := deploypolicy.Decode(a.DeployPolicyJSON)
	a.RequirePipelineSuccess = p.RequirePipelineSuccess
	a.WatchFiles = p.WatchFiles
	a.PruneVolumes = p.PruneVolumes
	a.HealthTimeout = p.HealthTimeout
}

// ComposeProject is the docker compose project name for this app.
// A fleet `project` value adopts an existing stack. Otherwise the name is derived from the app id.
func (a *App) ComposeProject() string {
	if a == nil {
		return compose.ProjectNameForApp("")
	}
	if p := strings.TrimSpace(a.Policy().Project); p != "" {
		return compose.ProjectNameForApp(p)
	}
	return compose.ProjectNameForApp(a.ID)
}

// HealthTimeoutDuration parses the fleet health_timeout. Invalid or empty values use DefaultHealthTimeout.
func (a *App) HealthTimeoutDuration() time.Duration {
	if a == nil {
		return DefaultHealthTimeout
	}
	raw := strings.TrimSpace(a.Policy().HealthTimeout)
	if raw == "" {
		return DefaultHealthTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultHealthTimeout
	}
	return d
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
	Kind       string    `json:"kind"`   // reconcile
	Status     string    `json:"status"` // pending|running|succeeded|failed
	Code       string    `json:"code,omitempty"`
	Stage      string    `json:"stage,omitempty"`
	Message    string    `json:"message,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}
