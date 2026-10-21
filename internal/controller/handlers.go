package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/cigate"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/redact"
	"github.com/valthaide/conops-private/internal/syncerr"
)

type registerAppRequest struct {
	Name           string            `json:"name"`
	RepoURL        string            `json:"repo_url"`
	RepoAuthMethod string            `json:"repo_auth_method"`
	DeployKey      string            `json:"deploy_key"`
	GitToken       string            `json:"git_token"`
	Branch         string            `json:"branch"`
	ComposePath    string            `json:"compose_path"`
	PollInterval   string            `json:"poll_interval"`
	ServiceEnvs    map[string]string `json:"service_envs"`
}

type updateAppRequest struct {
	Name         *string            `json:"name,omitempty"`
	Branch       *string            `json:"branch,omitempty"`
	ComposePath  *string            `json:"compose_path,omitempty"`
	PollInterval *string            `json:"poll_interval,omitempty"`
	ServiceEnvs  *map[string]string `json:"service_envs,omitempty"`
}

// RuntimeCleaner performs best-effort runtime cleanup for an app.
type RuntimeCleaner interface {
	Destroy(ctx context.Context, appID, composePath string, envVars map[string]string) (string, error)
}

// RuntimeApplier applies desired app state to the runtime.
type RuntimeApplier interface {
	Apply(
		ctx context.Context,
		appID, content string,
		envVars map[string]string,
		repoURL, branch, composePath, commitHash string,
		repoAuthMethod string,
		repoSecret []byte,
		onProgress func(string),
	) (string, error)
}

// RuntimeInspector inspects compose project containers.
type RuntimeInspector interface {
	InspectProjectContainers(ctx context.Context, projectName string) ([]compose.ServiceContainer, error)
}

// SyncGate limits concurrent syncs (optional; usually the Reconciler).
type SyncGate interface {
	AcquireSync(ctx context.Context) error
	ReleaseSync()
}

// Handler handles HTTP requests for the controller.
type Handler struct {
	Registry    *Registry
	Cleaner     RuntimeCleaner
	Applier     RuntimeApplier
	Inspector   RuntimeInspector
	SyncGate    SyncGate
	Ops         *OperationStore
	Logger      *slog.Logger
	Version     string
	SyncTimeout time.Duration
}

// NewHandler creates a new controller handler.
func NewHandler(registry *Registry, cleaner RuntimeCleaner, applier RuntimeApplier, inspector RuntimeInspector, logger *slog.Logger, version string) *Handler {
	return &Handler{
		Registry:  registry,
		Cleaner:   cleaner,
		Applier:   applier,
		Inspector: inspector,
		Ops:       NewOperationStore(),
		Logger:    logger,
		Version:   version,
	}
}

// Healthz handles GET /healthz.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	status := api.HealthStatus{
		Status:  "ok",
		Version: h.Version,
		DB:      "ok",
	}
	if err := h.Registry.PingStore(r.Context()); err != nil {
		status.Status = "error"
		status.DB = err.Error()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// ListAppContainers handles GET /api/v1/apps/{id}/containers.
func (h *Handler) ListAppContainers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if h.Inspector == nil {
		http.Error(w, "runtime inspector is not configured", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	project := compose.ProjectNameForApp(app.ID)
	runtimeContainers, err := h.Inspector.InspectProjectContainers(ctx, project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]api.Container, 0, len(runtimeContainers))
	for _, c := range runtimeContainers {
		out = append(out, api.Container{
			Service: c.Service,
			Name:    c.Name,
			Image:   c.Image,
			Status:  c.Status,
			Health:  c.Health,
			Ports:   c.Ports,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.APIResponse{Data: out})
}

// RegisterApp handles POST /api/v1/apps
func (h *Handler) RegisterApp(w http.ResponseWriter, r *http.Request) {
	var req registerAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	app := App{
		Name:           strings.TrimSpace(req.Name),
		RepoURL:        strings.TrimSpace(req.RepoURL),
		RepoAuthMethod: strings.TrimSpace(req.RepoAuthMethod),
		Branch:         strings.TrimSpace(req.Branch),
		ComposePath:    strings.TrimSpace(req.ComposePath),
		PollInterval:   strings.TrimSpace(req.PollInterval),
	}

	if app.Name == "" || app.RepoURL == "" {
		http.Error(w, "App name and repo URL are required", http.StatusBadRequest)
		return
	}

	secret := strings.TrimSpace(req.DeployKey)
	if t := strings.TrimSpace(req.GitToken); t != "" {
		secret = t
	}
	if err := h.Registry.AddWithDeployKeyAndEnvs(&app, secret, req.ServiceEnvs); err != nil {
		status := http.StatusConflict
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "required") || strings.Contains(errText, "invalid") || strings.Contains(errText, "unsupported") {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(api.APIResponse{
		Message: "App registered successfully",
		Data:    app,
	})
}

// ListApps handles GET /api/v1/apps
func (h *Handler) ListApps(w http.ResponseWriter, r *http.Request) {
	apps := h.Registry.List()
	json.NewEncoder(w).Encode(api.APIResponse{
		Data: apps,
	})
}

// GetApp handles GET /api/v1/apps/{id}
func (h *Handler) GetApp(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(api.APIResponse{
		Data: app,
	})
}

// DeleteApp handles DELETE /api/v1/apps/{id}
func (h *Handler) DeleteApp(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if h.Cleaner != nil {
		cleanupCtx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()

		if _, err := h.Cleaner.Destroy(cleanupCtx, app.ID, app.ComposePath, nil); err != nil {
			if h.Logger != nil {
				h.Logger.Error("Failed to cleanup app runtime", "id", app.ID, "error", err)
			}
			http.Error(w, "failed to cleanup running containers before deletion", http.StatusInternalServerError)
			return
		}
	}

	if err := h.Registry.Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(api.APIResponse{
		Message: "App deleted successfully",
	})
}

// UpdateApp handles PATCH /api/v1/apps/{id}
func (h *Handler) UpdateApp(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var req updateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Collect current values and update with new ones if provided
	name := app.Name
	branch := app.Branch
	composePath := app.ComposePath
	pollInterval := app.PollInterval
	var serviceEnvs map[string]string

	// Track if sync-affecting fields changed
	branchChanged := false
	composePathChanged := false
	envVarsChanged := false

	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Branch != nil {
		newBranch := strings.TrimSpace(*req.Branch)
		if newBranch != app.Branch {
			branchChanged = true
		}
		branch = newBranch
	}
	if req.ComposePath != nil {
		newPath := strings.TrimSpace(*req.ComposePath)
		if newPath != app.ComposePath {
			composePathChanged = true
		}
		composePath = newPath
	}
	if req.PollInterval != nil {
		pollInterval = strings.TrimSpace(*req.PollInterval)
	}
	if req.ServiceEnvs != nil {
		serviceEnvs = *req.ServiceEnvs
		envVarsChanged = true
	}

	// Update the app
	if err := h.Registry.UpdateApp(id, name, branch, composePath, pollInterval, serviceEnvs); err != nil {
		status := http.StatusInternalServerError
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "required") || strings.Contains(errText, "invalid") {
			status = http.StatusBadRequest
		} else if strings.Contains(errText, "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	// Trigger sync if sync-affecting fields changed
	needsSync := branchChanged || composePathChanged || envVarsChanged
	if needsSync {
		if err := h.Registry.UpdateStatus(id, "pending", nil); err != nil && h.Logger != nil {
			h.Logger.Warn("Failed to mark app pending after update", "id", id, "error", err)
		}
	}

	// Get updated app to return in response
	updatedApp, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, "App updated but failed to retrieve updated data", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(api.APIResponse{
		Message: "App updated successfully",
		Data:    updatedApp,
	})
}

// SuspendApp handles POST /api/v1/apps/{id}/suspend.
func (h *Handler) SuspendApp(w http.ResponseWriter, r *http.Request) {
	h.setSuspended(w, r, true)
}

// ResumeApp handles POST /api/v1/apps/{id}/resume.
func (h *Handler) ResumeApp(w http.ResponseWriter, r *http.Request) {
	h.setSuspended(w, r, false)
}

func (h *Handler) setSuspended(w http.ResponseWriter, r *http.Request, suspended bool) {
	id := chi.URLParam(r, "id")
	if err := h.Registry.SetSuspended(id, suspended); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	msg := "App resumed"
	if suspended {
		msg = "App suspended"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.APIResponse{Message: msg, Data: app})
}

// ForceSyncApp handles POST /api/v1/apps/{id}/sync (synchronous).
func (h *Handler) ForceSyncApp(w http.ResponseWriter, r *http.Request) {
	if h.Applier == nil {
		http.Error(w, "runtime applier is not configured", http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if app.Status == "syncing" {
		if op, ok := h.Ops.ActiveForApp(app.ID); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message":      "sync already in progress",
				"code":         "sync_in_progress",
				"stage":        syncerr.StageUnknown,
				"operation_id": op.ID,
			})
			return
		}
		writeSyncError(w, http.StatusConflict, syncerr.New("sync_in_progress", syncerr.StageUnknown, "sync already in progress"))
		return
	}

	if se := h.runAppSync(app); se != nil {
		writeSyncError(w, httpStatusForSyncErr(se), se)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(api.APIResponse{
		Message: "App synced successfully",
	})
}

// ReconcileApp handles POST /api/v1/apps/{id}/reconcile (asynchronous).
// Returns 202 with operation_id; concurrent POSTs while running return the same id.
func (h *Handler) ReconcileApp(w http.ResponseWriter, r *http.Request) {
	if h.Applier == nil {
		http.Error(w, "runtime applier is not configured", http.StatusServiceUnavailable)
		return
	}
	if h.Ops == nil {
		h.Ops = NewOperationStore()
	}

	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	op, existing := h.Ops.StartReconcile(app.ID, app.Name)
	if existing {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message":      "reconcile already in progress",
			"operation_id": op.ID,
			"data":         op,
		})
		return
	}

	go h.runReconcileOperation(op.ID, app.ID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message":      "reconcile accepted",
		"operation_id": op.ID,
		"data":         op,
	})
}

// GetOperation handles GET /api/v1/operations/{id}.
func (h *Handler) GetOperation(w http.ResponseWriter, r *http.Request) {
	if h.Ops == nil {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	op, ok := h.Ops.Get(id)
	if !ok {
		http.Error(w, "operation not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.APIResponse{Data: op})
}

func (h *Handler) runReconcileOperation(opID, appID string) {
	h.Ops.MarkRunning(opID)
	app, err := h.Registry.Get(appID)
	if err != nil {
		h.Ops.Finish(opID, OpFailed, syncerr.CodeUnknown, syncerr.StageUnknown, redact.String(err.Error()))
		return
	}
	if se := h.runAppSync(app); se != nil {
		h.Ops.Finish(opID, OpFailed, se.Code, se.Stage, redact.String(se.Message))
		return
	}
	h.Ops.Finish(opID, OpSucceeded, "", "", "")
}

func (h *Handler) syncTimeout() time.Duration {
	timeout := h.SyncTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
		if value := strings.TrimSpace(os.Getenv("CONOPS_SYNC_TIMEOUT")); value != "" {
			if parsed, parseErr := time.ParseDuration(value); parseErr == nil && parsed > 0 {
				timeout = parsed
			}
		}
	}
	return timeout
}

// runAppSync performs a synchronous apply for the app. Returns nil on success.
func (h *Handler) runAppSync(app *api.App) *syncerr.Error {
	if app == nil {
		return syncerr.New(syncerr.CodeUnknown, syncerr.StageUnknown, "app is nil")
	}

	syncStartedAt := time.Now()
	if err := h.Registry.UpdateStatus(app.ID, "syncing", &syncStartedAt); err != nil && h.Logger != nil {
		h.Logger.Warn("Failed to mark app syncing", "id", app.ID, "error", err)
	}

	if policy := app.Policy(); policy.RequirePipelineSuccess {
		commit := app.LastSeenCommit
		if commit != "" && commit != app.LastSyncedCommit {
			checker := &cigate.Checker{Token: cigate.TokenFromEnv()}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			res := checker.CheckCommit(ctx, app.RepoURL, commit)
			cancel()
			if !res.Allowed {
				se := syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate,
					"deploy blocked: pipeline status="+res.Status+" reason="+res.Reason)
				_ = h.Registry.UpdateSyncResultErr(app.ID, "error", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
				metrics.ObserveDeployBlocked(app.Name, res.Reason)
				return se
			}
		}
	}

	repoSecret, err := h.Registry.GetRepoSecret(app.ID)
	if err != nil {
		_ = h.Registry.UpdateStatus(app.ID, "error", nil)
		return syncerr.Wrap(syncerr.CodeCredentials, syncerr.StageCredentials, err)
	}
	defer zeroBytes(repoSecret)
	if len(repoSecret) > 0 {
		redact.Default.AddSecret(string(repoSecret))
	}

	envVars, err := h.Registry.GetAppEnvs(app.ID)
	if err != nil {
		_ = h.Registry.UpdateStatus(app.ID, "error", nil)
		return syncerr.Wrap(syncerr.CodeCredentials, syncerr.StageCredentials, err)
	}
	for _, v := range envVars {
		redact.Default.AddSecret(v)
	}

	syncCtx, cancel := context.WithTimeout(context.Background(), h.syncTimeout())
	defer cancel()

	if h.SyncGate != nil {
		if err := h.SyncGate.AcquireSync(syncCtx); err != nil {
			_ = h.Registry.UpdateStatus(app.ID, "pending", nil)
			return syncerr.New(syncerr.CodeTimeout, syncerr.StageUnknown, "sync queue timeout")
		}
		defer h.SyncGate.ReleaseSync()
	}

	progress := newSyncProgressReporter(h.Registry, h.Logger, app.ID, syncProgressFlushInterval)
	output, err := h.Applier.Apply(
		syncCtx,
		app.ID,
		"",
		envVars,
		app.RepoURL,
		app.Branch,
		app.ComposePath,
		"",
		app.RepoAuthMethod,
		repoSecret,
		progress.Update,
	)
	progress.Flush()
	if err != nil {
		se := syncerr.Classify(err)
		se.Message = redact.String(se.Message)
		now := time.Now()
		_ = h.Registry.UpdateSyncResultErr(
			app.ID,
			"error",
			now,
			app.LastSyncedCommit,
			app.LastSyncedCommitMessage,
			redact.String(output),
			se,
		)
		if h.Logger != nil {
			h.Logger.Error("Force sync failed", "id", app.ID, "code", se.Code, "stage", se.Stage, "error", redact.String(err.Error()))
		}
		return se
	}

	now := time.Now()
	if err := h.Registry.UpdateSyncResult(
		app.ID,
		"synced",
		now,
		app.LastSeenCommit,
		app.LastSeenCommitMessage,
		redact.String(output),
		"",
	); err != nil && h.Logger != nil {
		h.Logger.Warn("Failed to update sync status after force sync", "id", app.ID, "error", err)
	}
	if hasher, ok := h.Applier.(EnvFileHasher); ok {
		if hash, hashErr := hasher.ExternalEnvHashForApp(app.ID, app.ComposePath); hashErr == nil && hash != "" {
			_ = h.Registry.UpdateEnvHash(app.ID, hash)
		}
	}
	return nil
}

func httpStatusForSyncErr(se *syncerr.Error) int {
	if se == nil {
		return http.StatusInternalServerError
	}
	switch se.Code {
	case syncerr.CodeGitAuth, syncerr.CodeGitFetch:
		return http.StatusBadGateway
	case syncerr.CodeComposeApply, syncerr.CodeEnvFileMissing, syncerr.CodeBlockedByCI:
		return http.StatusUnprocessableEntity
	case syncerr.CodeTimeout:
		return http.StatusGatewayTimeout
	case "sync_in_progress":
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeSyncError(w http.ResponseWriter, status int, se *syncerr.Error) {
	if se == nil {
		http.Error(w, "force sync failed", status)
		return
	}
	se.Message = redact.String(se.Message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": redact.String(se.Message),
		"code":    se.Code,
		"stage":   se.Stage,
	})
}
