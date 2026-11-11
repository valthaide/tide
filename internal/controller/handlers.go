package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/apiauth"
	"github.com/valthaide/tide/internal/cigate"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/fleetcheck"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/redact"
	"github.com/valthaide/tide/internal/syncerr"
)

type registerAppRequest struct {
	Name           string            `json:"name"`
	RepoURL        string            `json:"repo_url"`
	RepoAuthMethod string            `json:"repo_auth_method"`
	DeployKey      string            `json:"deploy_key"`
	GitToken       string            `json:"git_token"`
	Branch         string            `json:"branch"`
	ComposePath    string            `json:"compose_path"`
	Template       string            `json:"template,omitempty"`
	PollInterval   string            `json:"poll_interval"`
	ServiceEnvs    map[string]string `json:"service_envs"`
}

type updateAppRequest struct {
	Name         *string            `json:"name,omitempty"`
	Branch       *string            `json:"branch,omitempty"`
	ComposePath  *string            `json:"compose_path,omitempty"`
	Template     *string            `json:"template,omitempty"`
	PollInterval *string            `json:"poll_interval,omitempty"`
	ServiceEnvs  *map[string]string `json:"service_envs,omitempty"`
}

// RuntimeCleaner performs best-effort runtime cleanup for an app.
// pruneVolumes removes named compose volumes; images used only by this app are always removed.
type RuntimeCleaner interface {
	Destroy(ctx context.Context, appID, composePath string, envVars map[string]string, pruneVolumes bool) (string, error)
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
	project := app.ComposeProject()
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
		Template:       strings.TrimSpace(req.Template),
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
	h.decorate(r.Context(), apps)
	json.NewEncoder(w).Encode(api.APIResponse{
		Data: apps,
	})
}

func (h *Handler) attachRunningImages(ctx context.Context, apps []*api.App) {
	if h == nil || h.Inspector == nil {
		return
	}
	type imageReporter interface {
		RunningImages(ctx context.Context) ([]compose.RunningImage, error)
	}
	reporter, ok := h.Inspector.(imageReporter)
	if !ok {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	images, err := reporter.RunningImages(ictx)
	if err != nil {
		return
	}
	byProject := map[string][]api.ImageRef{}
	for _, img := range images {
		byProject[img.Project] = append(byProject[img.Project], api.ImageRef{
			Service: img.Service,
			Image:   img.Image,
			Digest:  img.Digest,
		})
	}
	for _, app := range apps {
		if app == nil {
			continue
		}
		app.Images = byProject[app.ComposeProject()]
		for i := range app.Images {
			app.Images[i].Tag = imageTag(app.Images[i].Image)
		}
	}
}

func (h *Handler) decorate(ctx context.Context, apps []*api.App) {
	h.attachRunningImages(ctx, apps)
	h.attachOneshot(ctx, apps)
	// Status shows whether the app checkout provides compose.override.yaml.
	if inspector, ok := h.Inspector.(interface{ HasOverrideForApp(appID string) bool }); ok && inspector != nil {
		for _, app := range apps {
			if app != nil && app.Template != "" {
				app.HasOverride = inspector.HasOverrideForApp(app.ID)
			}
		}
	}
	if q, ok := h.SyncGate.(interface{ Annotate([]*api.App) }); ok {
		q.Annotate(apps)
	}
	for _, app := range apps {
		annotatePublic(app)
	}
}

func annotatePublic(app *api.App) {
	if app == nil {
		return
	}
	policy := app.Policy()
	listed := policy.PublicVars
	healthy := deploypolicy.DecodeVars(app.LastHealthyVars)
	rejected := deploypolicy.DecodeVars(app.RejectedVars)
	app.LastSyncError = fleetcheck.RedactText(app.LastSyncError, policy.Vars, listed)
	app.LastSyncError = fleetcheck.RedactText(app.LastSyncError, healthy, listed)
	app.LastSyncError = fleetcheck.RedactText(app.LastSyncError, rejected, listed)
	app.LastHealthyVars = deploypolicy.EncodeVars(fleetcheck.PublicVars(healthy, listed))
	app.RejectedVars = deploypolicy.EncodeVars(fleetcheck.PublicVars(rejected, listed))
	public := map[string]string{}
	var tags []string
	for key, value := range policy.Vars {
		if !fleetcheck.PublicVar(key, policy.PublicVars) {
			continue
		}
		public[key] = value
		upper := strings.ToUpper(key)
		if strings.HasSuffix(upper, "_TAG") || strings.HasSuffix(upper, "_IMAGE") {
			tags = append(tags, key+"="+value)
		}
	}
	if len(public) > 0 {
		app.PublicVars = public
	}
	if len(tags) > 0 {
		sortStrings(tags)
		app.Tags = strings.Join(tags, ",")
	}
	app.FleetRevision = metrics.FleetSHA()
	app.Schema = 1
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func imageTag(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > 0 && !strings.Contains(ref[i:], "/") {
		return ref[i+1:]
	}
	return ""
}

func (h *Handler) attachOneshot(ctx context.Context, apps []*api.App) {
	type snapper interface {
		SnapshotProjects(ctx context.Context) (map[string]compose.ProjectRuntimeState, error)
	}
	source, ok := h.Inspector.(snapper)
	if !ok || source == nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	states, err := source.SnapshotProjects(sctx)
	if err != nil {
		return
	}
	for _, app := range apps {
		if app == nil {
			continue
		}
		app.Oneshot = states[app.ComposeProject()].Oneshot
		app.FleetRevision = metrics.FleetSHA()
		app.Schema = 1
	}
}

func (h *Handler) ListHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	revs, err := h.Registry.ListDeploys(id, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	listed := app.Policy().PublicVars
	for i := range revs {
		revs[i].VarsJSON = deploypolicy.EncodeVars(fleetcheck.PublicVars(deploypolicy.DecodeVars(revs[i].VarsJSON), listed))
	}
	json.NewEncoder(w).Encode(api.APIResponse{Data: revs})
}

func (h *Handler) ImageGC(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run") == "true" || r.URL.Query().Get("dry_run") == "1"
	images, err := RunImageGC(h.Registry, h.Inspector, dry)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(api.APIResponse{Data: images})
}

// GetApp handles GET /api/v1/apps/{id}
func (h *Handler) GetApp(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h.decorate(r.Context(), []*api.App{app})
	if lister, ok := h.Inspector.(interface {
		ProjectVolumes(ctx context.Context, project string) ([]compose.VolumeInfo, error)
	}); ok && lister != nil {
		vctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		vols, volErr := lister.ProjectVolumes(vctx, app.ComposeProject())
		cancel()
		if volErr == nil {
			for _, vol := range vols {
				app.Volumes = append(app.Volumes, api.VolumeRef{Name: vol.Name, Containers: vol.Containers})
			}
		}
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

		if namer, ok := h.Cleaner.(interface{ RememberProject(appID, project string) }); ok {
			namer.RememberProject(app.ID, app.ComposeProject())
		}
		if _, err := h.Cleaner.Destroy(cleanupCtx, app.ID, app.ComposePath, nil, app.Policy().PruneVolumes); err != nil {
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

	message := "App deleted successfully. Named volumes were kept. To remove them, set prune_volumes: true, wait until it is applied, then delete. Volumes labeled com.tide.keep are never removed."
	if app.Policy().PruneVolumes {
		message = "App deleted successfully. Named volumes without com.tide.keep were removed. External volumes and com.tide.keep volumes were kept."
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(api.APIResponse{
		Message: message,
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

	template := app.Template
	// Track if sync-affecting fields changed
	branchChanged := false
	composePathChanged := false
	templateChanged := false
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
	if req.Template != nil {
		newTemplate := strings.TrimSpace(*req.Template)
		if newTemplate != app.Template {
			templateChanged = true
		}
		template = newTemplate
		if newTemplate != "" {
			composePath = ""
		}
	}
	if req.PollInterval != nil {
		pollInterval = strings.TrimSpace(*req.PollInterval)
	}
	if req.ServiceEnvs != nil {
		serviceEnvs = *req.ServiceEnvs
		envVarsChanged = true
	}

	migrator, hasMigrator := h.SyncGate.(interface {
		MigrateRepo(ctx context.Context, existing *api.App, req api.RepoMigrationRequest, nextPolicyJSON string) (api.RepoMigrationPlan, error)
	})
	if (branchChanged || composePathChanged || templateChanged) && !envVarsChanged && hasMigrator && migrator != nil {
		if name != app.Name || pollInterval != app.PollInterval {
			if err := h.Registry.UpdateApp(id, name, app.Branch, app.ComposePath, pollInterval, nil); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		actor := apiauth.ActorFromRequest(r)
		migReq := api.RepoMigrationRequest{
			RepoURL:        app.RepoURL,
			RepoAuthMethod: app.RepoAuthMethod,
			Branch:         branch,
			Revision:       app.Revision,
			ComposePath:    composePath,
			Template:       template,
			PollInterval:   pollInterval,
			Reason:         "api patch source updated",
			Actor:          actor,
		}
		plan, migErr := migrator.MigrateRepo(r.Context(), app, migReq, "")
		if migErr != nil {
			se := syncerr.Classify(migErr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(httpStatusForSyncErr(se))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": se.Message,
				"code":    se.Code,
				"stage":   se.Stage,
				"data":    plan,
			})
			return
		}
		updatedApp, err := h.Registry.Get(id)
		if err != nil {
			http.Error(w, "App updated but failed to retrieve updated data", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(api.APIResponse{
			Message: "App updated successfully",
			Data:    updatedApp,
		})
		return
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

	if templateChanged {
		if err := h.Registry.SetTemplate(id, template); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Trigger sync if sync-affecting fields changed
	needsSync := branchChanged || composePathChanged || templateChanged || envVarsChanged
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
	if err := h.Registry.SetOperatorSuspended(id, suspended); err != nil {
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

	if se := h.syncThroughReconciler(app); se != nil {
		writeSyncError(w, httpStatusForSyncErr(se), se)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(api.APIResponse{
		Message: "App synced successfully",
	})
}

// RetryApp handles POST /api/v1/apps/{id}/retry.
// It puts one app back on the work queue without restarting Tide and does not bypass the CI gate.
func (h *Handler) RetryApp(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if app.Suspended {
		http.Error(w, "app is suspended", http.StatusConflict)
		return
	}
	if kicker, ok := h.SyncGate.(interface{ RetryApp(*api.App) error }); ok && kicker != nil {
		if err := kicker.RetryApp(app); err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(strings.ToLower(err.Error()), "in progress") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
	} else if err := h.Registry.MarkPending(app.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(api.APIResponse{Message: "retry queued", Data: app})
}

type reconcileAppRequest struct {
	Force  bool   `json:"force,omitempty"`
	SHA    string `json:"sha,omitempty"`
	Reason string `json:"reason,omitempty"`
	TTL    string `json:"ttl,omitempty"`
	Actor  string `json:"actor,omitempty"`
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

	var reqBody reconcileAppRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			http.Error(w, "invalid reconcile request body", http.StatusBadRequest)
			return
		}
	}
	forceRequested := reqBody.Force || r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
	if forceRequested {
		actor := apiauth.ActorFromRequest(r)
		if actor == "admin" && strings.TrimSpace(reqBody.Actor) != "" {
			actor = strings.TrimSpace(reqBody.Actor)
		}
		sha := strings.TrimSpace(reqBody.SHA)
		if sha == "" {
			sha = strings.TrimSpace(r.URL.Query().Get("sha"))
		}
		reason := strings.TrimSpace(reqBody.Reason)
		if reason == "" {
			reason = strings.TrimSpace(r.URL.Query().Get("reason"))
		}
		if !apiauth.CanForceBypass(r) {
			_ = h.Registry.RecordAudit(api.AuditEvent{
				Kind:     "ci_force_bypass",
				Actor:    actor,
				AppID:    app.ID,
				AppName:  app.Name,
				RepoURL:  app.RepoURL,
				GitSHA:   sha,
				Reason:   reason,
				Decision: "bypass_denied_unauthorized",
			})
			http.Error(w, "forbidden: token is not authorized for --force CI bypass", http.StatusForbidden)
			return
		}
		var ttl time.Duration
		rawTTL := strings.TrimSpace(reqBody.TTL)
		if rawTTL == "" {
			rawTTL = strings.TrimSpace(r.URL.Query().Get("ttl"))
		}
		if rawTTL != "" {
			parsedTTL, parseErr := time.ParseDuration(rawTTL)
			if parseErr != nil || parsedTTL <= 0 {
				http.Error(w, "invalid bypass ttl", http.StatusBadRequest)
				return
			}
			ttl = parsedTTL
		}
		granter, ok := h.SyncGate.(interface{ GrantBypass(BypassRequest) error })
		if !ok || granter == nil {
			http.Error(w, "CI bypass controller unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := granter.GrantBypass(BypassRequest{
			AppID:  app.ID,
			SHA:    sha,
			Reason: reason,
			Actor:  actor,
			TTL:    ttl,
		}); err != nil {
			status := http.StatusBadRequest
			if strings.Contains(strings.ToLower(err.Error()), "does not match") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
	}
	if note, ok := h.SyncGate.(interface{ NoteManual(*api.App) }); ok {
		note.NoteManual(app)
	}
	pos, class := 0, 0
	if q, ok := h.SyncGate.(interface {
		Position(string) (int, int)
	}); ok {
		pos, class = q.Position(app.ID)
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
		"position":     pos,
		"class":        class,
	})
}

// GetQueue handles GET /api/v1/queue.
func (h *Handler) GetQueue(w http.ResponseWriter, r *http.Request) {
	type snap interface {
		Snapshot() []QueueItem
	}
	var items []QueueItem
	if q, ok := h.SyncGate.(snap); ok && q != nil {
		items = q.Snapshot()
	}
	if items == nil {
		items = []QueueItem{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.APIResponse{Data: items})
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

// ListAudit handles GET /api/v1/audit.
func (h *Handler) ListAudit(w http.ResponseWriter, r *http.Request) {
	h.serveAudit(w, r, "")
}

// ListAppAudit handles GET /api/v1/apps/{id}/audit.
func (h *Handler) ListAppAudit(w http.ResponseWriter, r *http.Request) {
	h.serveAudit(w, r, chi.URLParam(r, "id"))
}

func (h *Handler) serveAudit(w http.ResponseWriter, r *http.Request, appID string) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	events, err := h.Registry.ListAudit(appID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.APIResponse{Data: events})
}

// MigrateAppRepo handles POST /api/v1/apps/{id}/migrate-repo.
func (h *Handler) MigrateAppRepo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	app, err := h.Registry.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var req api.RepoMigrationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid migration request JSON", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("dry_run") == "true" || r.URL.Query().Get("dry_run") == "1" {
		req.DryRun = true
	}
	actor := apiauth.ActorFromRequest(r)
	if actor == "admin" && strings.TrimSpace(req.Actor) != "" {
		actor = strings.TrimSpace(req.Actor)
	}
	req.Actor = actor

	migrator, ok := h.SyncGate.(interface {
		MigrateRepo(ctx context.Context, existing *api.App, req api.RepoMigrationRequest, nextPolicyJSON string) (api.RepoMigrationPlan, error)
	})
	if !ok || migrator == nil {
		http.Error(w, "repository migrator is not configured", http.StatusServiceUnavailable)
		return
	}

	plan, migErr := migrator.MigrateRepo(r.Context(), app, req, "")
	if migErr != nil {
		se := syncerr.Classify(migErr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatusForSyncErr(se))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": se.Message,
			"code":    se.Code,
			"stage":   se.Stage,
			"data":    plan,
		})
		return
	}

	msg := "repository migrated successfully"
	if req.DryRun {
		msg = "repository migration dry-run succeeded"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(api.APIResponse{
		Message: msg,
		Data:    plan,
	})
}

func (h *Handler) runReconcileOperation(opID, appID string) {
	h.Ops.MarkRunning(opID)
	app, err := h.Registry.Get(appID)
	if err != nil {
		h.Ops.Finish(opID, OpFailed, syncerr.CodeUnknown, syncerr.StageUnknown, redact.String(err.Error()))
		return
	}
	if se := h.syncThroughReconciler(app); se != nil {
		h.Ops.Finish(opID, OpFailed, se.Code, se.Stage, redact.String(se.Message))
		return
	}
	h.Ops.Finish(opID, OpSucceeded, "", "", "")
}

func (h *Handler) syncThroughReconciler(app *api.App) *syncerr.Error {
	if syncer, ok := h.SyncGate.(interface {
		SyncAppNow(app *api.App) *syncerr.Error
	}); ok && syncer != nil {
		return syncer.SyncAppNow(app)
	}
	return h.runAppSync(app)
}

func (h *Handler) syncTimeout() time.Duration {
	timeout := h.SyncTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
		if value := envcompat.Getenv("TIDE_SYNC_TIMEOUT"); value != "" {
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
			checker := &cigate.Checker{Token: cigate.TokenFromEnv(), Sources: policy.RequirePipelineSources}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			res := checker.CheckCommit(ctx, app.RepoURL, commit)
			cancel()
			if res.Allowed && len(res.Ignored) > 0 && h.Logger != nil {
				h.Logger.Info("Deploy allowed without a counted pipeline", "app", app.Name, "commit", commit, "ignored_pipelines", cigate.FormatPipelines(res.Ignored))
			}
			if res.Outcome == cigate.OutcomeWait {
				now := time.Now()
				_ = h.Registry.UpdateStatus(app.ID, "pending", &now)
				metrics.ObserveDeployBlocked(app.Name, res.Reason, res.Source)
				if h.Logger != nil {
					h.Logger.Info("Deploy waiting for CI", "app", app.Name, "commit", commit, "pipeline_id", res.PipelineID, "pipeline_source", res.Source, "pipeline_status", res.Status)
				}
				return syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate,
					fmt.Sprintf("deploy waiting: pipeline id=%d source=%s status=%s reason=%s", res.PipelineID, res.Source, res.Status, res.Reason))
			}
			if !res.Allowed {
				se := syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate,
					fmt.Sprintf("deploy blocked: pipeline id=%d source=%s status=%s reason=%s", res.PipelineID, res.Source, res.Status, res.Reason))
				_ = h.Registry.UpdateSyncResultErr(app.ID, "error", time.Now(), app.LastSyncedCommit, app.LastSyncedCommitMessage, "", se)
				metrics.ObserveDeployBlocked(app.Name, res.Reason, res.Source)
				if h.Logger != nil {
					h.Logger.Warn("Deploy blocked by CI gate", "app", app.Name, "commit", commit, "reason", res.Reason, "pipeline_id", res.PipelineID, "pipeline_source", res.Source, "pipeline_status", res.Status)
				}
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
	opts := compose.ApplyOptions{
		Vars:              app.Policy().Vars,
		Template:          app.Template,
		BuildMode:         app.Policy().EffectiveBuild(),
		Project:           app.ComposeProject(),
		CreateVolumes:     app.Policy().CreateVolumes,
		VolumeNamePattern: app.Policy().VolumeNamePattern,
	}
	var output string
	if ia, ok := h.Applier.(imageApplier); ok {
		output, err = ia.ApplyImages(syncCtx, app.ID, "", envVars, app.RepoURL, app.Branch, app.ComposePath, app.LastSeenCommit, app.RepoAuthMethod, repoSecret, opts, progress.Update)
	} else {
		output, err = h.Applier.Apply(syncCtx, app.ID, "", envVars, app.RepoURL, app.Branch, app.ComposePath, app.LastSeenCommit, app.RepoAuthMethod, repoSecret, progress.Update)
	}
	progress.Flush()
	if err != nil {
		se := syncerr.Classify(err)
		se.Message = redact.String(se.Message)
		status := "error"
		if declaredFailure(se) {
			status = "failed"
		}
		now := time.Now()
		_ = h.Registry.UpdateSyncResultErr(
			app.ID,
			status,
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
	_ = h.Registry.UpdateRollback(app.ID, app.LastSeenCommit, "", deploypolicy.EncodeVars(opts.Vars), "")
	if hasher, ok := h.Applier.(interface {
		ExternalEnvHashForSource(appID, composePath, template string) (string, error)
	}); ok {
		if hash, hashErr := hasher.ExternalEnvHashForSource(app.ID, app.ComposePath, app.Template); hashErr == nil && hash != "" {
			_ = h.Registry.UpdateEnvHash(app.ID, hash)
		}
	} else if hasher, ok := h.Applier.(EnvFileHasher); ok {
		if hash, hashErr := hasher.ExternalEnvHashForApp(app.ID, app.ComposePath); hashErr == nil && hash != "" {
			_ = h.Registry.UpdateEnvHash(app.ID, hash)
		}
	}
	if hasher, ok := h.Applier.(interface {
		TemplateHashForApp(appID, template string) (string, error)
	}); ok && app.Template != "" {
		if hash, hashErr := hasher.TemplateHashForApp(app.ID, app.Template); hashErr == nil && hash != "" {
			_ = h.Registry.UpdateTemplateHash(app.ID, hash)
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
