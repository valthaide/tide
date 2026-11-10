package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/gitremote"
	"github.com/valthaide/tide/internal/gitrev"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/pollinterval"
	"github.com/valthaide/tide/internal/repoauth"
	"github.com/valthaide/tide/internal/syncerr"
)

// AppManager applies fleet desired state to the local app registry.
type AppManager interface {
	List() []*api.App
	FindByName(name string) *api.App
	AddWithDeployKeyAndEnvs(app *api.App, secret string, serviceEnvs map[string]string) error
	UpdateApp(id, name, branch, composePath, pollInterval string, serviceEnvs map[string]string) error
	UpdateDeployPolicy(id, policyJSON string) error
	UpdateRepoSecret(id, secret string) error
	ClearRepoSecret(id string) error
	GetRepoSecret(id string) ([]byte, error)
	SetSuspended(id string, suspended bool) error
	SetRevision(id, revision string) error
	MarkPending(id string) error
	Delete(id string) error
}

// RuntimeCleaner destroys compose runtime for a deleted app.
// pruneVolumes removes named volumes declared by that app only.
type RuntimeCleaner interface {
	Destroy(ctx context.Context, appID, composePath string, envVars map[string]string, pruneVolumes bool) (string, error)
}

// RepoMigrator executes a production-safe repository URL / auth migration without data loss.
type RepoMigrator interface {
	MigrateRepoFromFleet(ctx context.Context, existing *api.App, req api.RepoMigrationRequest, nextPolicyJSON string) error
}

// Reconciler syncs the local registry from a fleet git repository.
type Reconciler struct {
	Config        Config
	Apps          AppManager
	Cleaner       RuntimeCleaner
	RepoMigrator  RepoMigrator
	Logger        *slog.Logger
	OnHost        func(*HostDocument)
	missingStreak int
}

// NewReconciler creates a fleet reconciler.
func NewReconciler(cfg Config, apps AppManager, cleaner RuntimeCleaner, logger *slog.Logger) *Reconciler {
	return &Reconciler{Config: cfg, Apps: apps, Cleaner: cleaner, Logger: logger}
}

// Run polls the fleet repo until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	if !r.Config.Enabled() {
		return
	}
	r.syncOnce(ctx)

	for {
		delay := pollinterval.WithJitter(r.Config.Interval)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if r.Logger != nil {
				r.Logger.Info("Fleet reconciler stopped")
			}
			return
		case <-timer.C:
			r.syncOnce(ctx)
		}
	}
}

func (r *Reconciler) syncOnce(ctx context.Context) {
	doc, err := r.loadDesired(ctx)
	if err != nil {
		if IsHostFileMissing(err) {
			metrics.SetFleetHostFileMissing(true)
			r.logMissingHostFile(err)
			return
		}
		r.missingStreak = 0
		se := syncerr.Classify(err)
		metrics.ObserveFleetError(se.GetCode())
		if r.Logger != nil {
			r.Logger.Error("Fleet sync failed", "code", se.GetCode(), "error", err)
		}
		return
	}
	r.missingStreak = 0
	metrics.SetFleetHostFileMissing(false)
	if err := r.applyDesired(ctx, doc); err != nil {
		se := syncerr.Classify(err)
		metrics.ObserveFleetError(se.GetCode())
		if r.Logger != nil {
			r.Logger.Error("Fleet apply failed", "code", se.GetCode(), "error", err)
		}
		return
	}
	metrics.ObserveFleetSuccess()
	if r.OnHost != nil {
		r.OnHost(doc)
	}
}

// logMissingHostFile counts consecutive misses. Cycle N logs one WARN and
// later misses stay at DEBUG. N is TIDE_FLEET_MISSING_CYCLES (default 3).
func (r *Reconciler) logMissingHostFile(err error) {
	r.missingStreak++
	level, announce := missingHostLog(r.missingStreak, r.Config.MissingCycles)
	if r.Logger == nil {
		return
	}
	if announce {
		r.Logger.Log(context.Background(), level, "fleet host file missing, waiting", "cycles", r.missingStreak, "error", err)
		return
	}
	r.Logger.Log(context.Background(), level, "fleet host file not found", "cycle", r.missingStreak, "error", err)
}

func missingHostLog(streak, threshold int) (slog.Level, bool) {
	if threshold < 1 {
		threshold = 3
	}
	if streak == threshold {
		return slog.LevelWarn, true
	}
	return slog.LevelDebug, false
}

func (r *Reconciler) loadDesired(ctx context.Context) (*HostDocument, error) {
	root, err := r.ensureFleetCheckout(ctx)
	if err != nil {
		return nil, err
	}
	path, err := FindHostFile(root, r.Config.Host)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	unknown, unknownErr := UnknownFields(data)
	if unknownErr != nil {
		return nil, unknownErr
	}
	mode := StrictMode(envcompat.Getenv("TIDE_FLEET_STRICT"))
	if len(unknown) > 0 && mode == "enforce" {
		return nil, fmt.Errorf("unknown_field: %s", strings.Join(unknown, ","))
	}
	if len(unknown) > 0 && r.Logger != nil {
		r.Logger.Warn("Unknown fleet fields ignored", "fields", strings.Join(unknown, ","))
	}
	doc, err := ParseHostDocument(data)
	if err != nil {
		return nil, err
	}
	if doc.Host == "" {
		doc.Host = r.Config.Host
	}
	return doc, nil
}

func (r *Reconciler) ensureFleetCheckout(ctx context.Context) (string, error) {
	dir, err := r.checkoutFleet(ctx)
	metrics.ObserveGitFetch(r.Config.RepoURL, err)
	return dir, err
}

func (r *Reconciler) checkoutFleet(ctx context.Context) (string, error) {
	_ = ctx
	if err := os.MkdirAll(r.Config.CacheDir, 0700); err != nil {
		return "", err
	}
	repoDir := filepath.Join(r.Config.CacheDir, "repo")
	auth, err := r.gitAuth()
	if err != nil {
		return "", err
	}

	cloneURL := repoauth.CleanHTTPSURL(r.Config.RepoURL)

	refs, listErr := gitremote.List(cloneURL, auth)
	if listErr != nil {
		return "", fmt.Errorf("fleet ls-remote: %w", listErr)
	}
	tip, tipErr := gitrev.Resolve("", r.Config.Branch, refs)
	if tipErr != nil {
		return "", tipErr
	}
	metrics.SetFleetRevision(tip.SHA)
	if head, headErr := localGitHEAD(repoDir); headErr == nil && strings.EqualFold(head, tip.SHA) {
		return repoDir, nil
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); os.IsNotExist(err) {
		_, err = git.PlainClone(repoDir, false, &git.CloneOptions{
			URL:           cloneURL,
			ReferenceName: plumbing.NewBranchReferenceName(r.Config.Branch),
			SingleBranch:  true,
			Auth:          auth,
		})
		if err != nil {
			return "", fmt.Errorf("clone fleet repo: %w", err)
		}
		return repoDir, nil
	}

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return "", err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	err = wt.Pull(&git.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.NewBranchReferenceName(r.Config.Branch),
		SingleBranch:  true,
		Auth:          auth,
		Force:         true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		// Recover by re-clone
		_ = os.RemoveAll(repoDir)
		_, err = git.PlainClone(repoDir, false, &git.CloneOptions{
			URL:           cloneURL,
			ReferenceName: plumbing.NewBranchReferenceName(r.Config.Branch),
			SingleBranch:  true,
			Auth:          auth,
		})
		if err != nil {
			return "", fmt.Errorf("re-clone fleet repo: %w", err)
		}
	}
	return repoDir, nil
}

func localGitHEAD(repoDir string) (string, error) {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return "", err
	}
	ref, err := repo.Head()
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

func (r *Reconciler) gitAuth() (transport.AuthMethod, error) {
	method := repoauth.NormalizeMethod(r.Config.AuthMethod)
	switch method {
	case repoauth.MethodPublic, "":
		return nil, nil
	case repoauth.MethodToken:
		token := repoauth.ResolveGitToken(r.Config.Token)
		if token == "" {
			return nil, fmt.Errorf("fleet git token is empty")
		}
		return repoauth.HTTPBasicAuth(token), nil
	case repoauth.MethodDeployKey:
		keyPath := strings.TrimSpace(r.Config.KeyFile)
		if keyPath == "" {
			return nil, fmt.Errorf("fleet deploy key file is empty")
		}
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		key := []byte(repoauth.NormalizeDeployKey(string(data)))
		knownHostsPath, err := repoauth.ResolveKnownHostsForRepo(r.Config.RepoURL)
		if err != nil {
			return nil, err
		}
		callback, err := repoauth.NewHostKeyCallback(knownHostsPath)
		if err != nil {
			return nil, err
		}
		auth, err := gitssh.NewPublicKeys("git", key, "")
		if err != nil {
			return nil, err
		}
		auth.HostKeyCallbackHelper = gitssh.HostKeyCallbackHelper{HostKeyCallback: callback}
		return auth, nil
	default:
		return nil, fmt.Errorf("unsupported fleet auth method %q", r.Config.AuthMethod)
	}
}

func (r *Reconciler) applyDesired(ctx context.Context, doc *HostDocument) error {
	desiredNames := make(map[string]struct{}, len(doc.Apps))
	for _, spec := range doc.Apps {
		if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.RepoURL) == "" {
			continue
		}
		desiredNames[spec.Name] = struct{}{}
		if err := r.upsertApp(ctx, spec, doc); err != nil {
			return fmt.Errorf("upsert app %q: %w", spec.Name, err)
		}
	}

	for _, app := range r.Apps.List() {
		if app == nil {
			continue
		}
		if _, ok := desiredNames[app.Name]; ok {
			continue
		}
		if r.Logger != nil {
			r.Logger.Info("Fleet removing app not in desired state", "name", app.Name, "id", app.ID)
		}
		if r.Cleaner != nil {
			rememberProject(r.Cleaner, app)
			if _, err := r.Cleaner.Destroy(ctx, app.ID, app.ComposePath, nil, app.Policy().PruneVolumes); err != nil {
				return fmt.Errorf("destroy removed app %q: %w", app.Name, err)
			}
		}
		if err := r.Apps.Delete(app.ID); err != nil {
			return fmt.Errorf("delete app %q: %w", app.Name, err)
		}
	}
	return nil
}

func (r *Reconciler) upsertApp(ctx context.Context, spec AppSpec, doc *HostDocument) error {
	method := repoauth.NormalizeMethod(spec.RepoAuthMethod)
	if method == "" {
		method = repoauth.MethodPublic
	}
	secret, err := r.resolveAppSecret(spec, method)
	if err != nil {
		return err
	}

	existing := r.Apps.FindByName(spec.Name)
	if existing == nil {
		app := &api.App{
			Name:           spec.Name,
			RepoURL:        spec.RepoURL,
			RepoAuthMethod: method,
			Branch:         spec.Branch,
			Revision:       spec.Revision,
			ComposePath:    spec.ComposePath,
			PollInterval:   spec.PollInterval,
			Suspended:      spec.Suspended,
		}
		if err := r.Apps.AddWithDeployKeyAndEnvs(app, secret, nil); err != nil {
			return err
		}
		if err := r.Apps.SetRevision(app.ID, spec.Revision); err != nil {
			return err
		}
		if err := r.Apps.UpdateDeployPolicy(app.ID, policyJSONFromSpec(spec, doc)); err != nil {
			return err
		}
		if spec.Suspended {
			_ = r.Apps.SetSuspended(app.ID, true)
		}
		if r.Logger != nil {
			r.Logger.Info("Fleet registered app", "name", spec.Name, "id", app.ID)
		}
		return nil
	}

	// Repo URL / auth method change -> production-safe in-place migration (preserves app ID, compose project, and volumes).
	if !strings.EqualFold(existing.RepoURL, spec.RepoURL) ||
		repoauth.NormalizeMethod(existing.RepoAuthMethod) != method {
		nextPolicy := policyJSONFromSpec(spec, doc)
		if r.RepoMigrator != nil {
			migReq := api.RepoMigrationRequest{
				RepoURL:        spec.RepoURL,
				RepoAuthMethod: method,
				DeployKey:      secret,
				Branch:         spec.Branch,
				Revision:       spec.Revision,
				ComposePath:    spec.ComposePath,
				Vars:           spec.Vars,
				Reason:         "fleet repo_url updated",
				Actor:          "fleet",
			}
			if err := r.RepoMigrator.MigrateRepoFromFleet(ctx, existing, migReq, nextPolicy); err != nil {
				return err
			}
			if existing.FleetSuspended != spec.Suspended {
				_ = r.Apps.SetSuspended(existing.ID, spec.Suspended)
			}
			return nil
		}

		// Unit-test fallback when RepoMigrator is not wired: never prune volumes, fail on cleaner error, preserve app ID if supported.
		if r.Cleaner != nil {
			rememberProject(r.Cleaner, existing)
			if _, err := r.Cleaner.Destroy(ctx, existing.ID, existing.ComposePath, nil, false); err != nil {
				return fmt.Errorf("stop existing app before repo migration: %w", err)
			}
		}
		if updater, ok := r.Apps.(interface {
			UpdateRepository(id, repoURL, repoAuthMethod, branch, revision, composePath string) error
		}); ok {
			if err := updater.UpdateRepository(existing.ID, spec.RepoURL, method, spec.Branch, spec.Revision, spec.ComposePath); err != nil {
				return err
			}
			if err := r.Apps.UpdateDeployPolicy(existing.ID, nextPolicy); err != nil {
				return err
			}
			if err := r.syncFleetSecret(existing.ID, method, secret, spec); err != nil {
				return err
			}
			if existing.FleetSuspended != spec.Suspended {
				_ = r.Apps.SetSuspended(existing.ID, spec.Suspended)
			}
			_ = r.Apps.MarkPending(existing.ID)
			return nil
		}
		existing.RepoURL = spec.RepoURL
		existing.RepoAuthMethod = method
		existing.Branch = spec.Branch
		existing.Revision = spec.Revision
		existing.ComposePath = spec.ComposePath
		_ = r.Apps.UpdateApp(existing.ID, spec.Name, spec.Branch, spec.ComposePath, spec.PollInterval, nil)
		_ = r.Apps.SetRevision(existing.ID, spec.Revision)
		_ = r.Apps.UpdateDeployPolicy(existing.ID, nextPolicy)
		_ = r.syncFleetSecret(existing.ID, method, secret, spec)
		_ = r.Apps.MarkPending(existing.ID)
		return nil
	}

	if err := r.Apps.UpdateApp(existing.ID, spec.Name, spec.Branch, spec.ComposePath, spec.PollInterval, nil); err != nil {
		return err
	}
	varsChanged := !deploypolicy.VarsEqual(existing.Policy().Vars, spec.Vars)
	if err := r.Apps.UpdateDeployPolicy(existing.ID, policyJSONFromSpec(spec, doc)); err != nil {
		return err
	}
	if varsChanged {
		if err := r.Apps.MarkPending(existing.ID); err != nil {
			return err
		}
		if r.Logger != nil {
			r.Logger.Info("Fleet vars changed", "name", spec.Name)
		}
	}
	if err := r.syncFleetSecret(existing.ID, method, secret, spec); err != nil {
		return err
	}
	if existing.Revision != spec.Revision {
		if err := r.Apps.SetRevision(existing.ID, spec.Revision); err != nil {
			return err
		}
		if r.Logger != nil {
			r.Logger.Info("Fleet revision updated", "name", spec.Name, "revision", spec.Revision, "branch", spec.Branch)
		}
	}
	// Fleet owns only the git suspend flag. An operator pause (tidectl suspend) is a
	// separate column and must survive this reconcile.
	if existing.FleetSuspended != spec.Suspended {
		if err := r.Apps.SetSuspended(existing.ID, spec.Suspended); err != nil {
			return err
		}
	}
	return nil
}

// syncFleetSecret keeps deploy_key_file contents up to date and clears stored
// tokens for fleet token apps (env is the source of truth).
func (r *Reconciler) syncFleetSecret(appID, method, secret string, spec AppSpec) error {
	switch method {
	case repoauth.MethodToken:
		if strings.TrimSpace(spec.DeployKeyFile) == "" {
			// Prefer TIDE_GIT_TOKEN from process env; do not keep a DB copy.
			return r.Apps.ClearRepoSecret(appID)
		}
		return r.Apps.UpdateRepoSecret(appID, secret)
	case repoauth.MethodDeployKey:
		current, err := r.Apps.GetRepoSecret(appID)
		if err != nil {
			return err
		}
		defer func() {
			for i := range current {
				current[i] = 0
			}
		}()
		if string(current) == secret {
			return nil
		}
		return r.Apps.UpdateRepoSecret(appID, secret)
	default:
		return nil
	}
}

func rememberProject(cleaner RuntimeCleaner, app *api.App) {
	if cleaner == nil || app == nil {
		return
	}
	namer, ok := cleaner.(interface{ RememberProject(appID, project string) })
	if !ok {
		return
	}
	namer.RememberProject(app.ID, app.ComposeProject())
}

func hostInterval(doc *HostDocument) string {
	if doc == nil {
		return ""
	}
	return doc.ReconcileInterval
}

func hostRetry(doc *HostDocument) string {
	if doc == nil {
		return ""
	}
	return doc.RetryRejected
}

func inherited(app, host string) string {
	if strings.TrimSpace(app) != "" {
		return strings.TrimSpace(app)
	}
	return strings.TrimSpace(host)
}

func hostVolumePattern(doc *HostDocument) string {
	if doc == nil {
		return ""
	}
	return strings.TrimSpace(doc.VolumeNamePattern)
}

func pipelineSources(app []string, doc *HostDocument) []string {
	if len(app) > 0 {
		return app
	}
	if doc == nil {
		return nil
	}
	return doc.RequirePipelineSources
}

func inheritedPriority(app *int, doc *HostDocument) *int {
	if app != nil {
		return app
	}
	if doc == nil {
		return nil
	}
	return doc.Priority
}

func convertTrustedWeb(spec *TrustedWebSpec) *deploypolicy.TrustedWebPolicy {
	if spec == nil {
		return nil
	}
	return &deploypolicy.TrustedWebPolicy{
		Enabled:          spec.Enabled,
		AllowedUsers:     append([]string(nil), spec.AllowedUsers...),
		AllowedVariables: append([]string(nil), spec.AllowedVariables...),
		RequiredJobs:     append([]string(nil), spec.RequiredJobs...),
	}
}

func policyJSONFromSpec(spec AppSpec, doc *HostDocument) string {
	hostBuild := ""
	var hostDigest *bool
	var hostStrictGate *bool
	var hostRegs []string
	var hostSharedJobs []string
	var hostTrustedWeb *TrustedWebSpec
	var tenantSpec TenantSpec
	hasTenant := false

	if doc != nil {
		hostBuild = doc.Build
		hostDigest = doc.RequireDigest
		hostStrictGate = doc.StrictPipelineGate
		hostRegs = doc.AllowedRegistries
		hostSharedJobs = doc.SharedRequiredJobs
		hostTrustedWeb = doc.TrustedWebPipelines
		if tName := strings.TrimSpace(spec.Tenant); tName != "" && doc.Tenants != nil {
			if ts, ok := doc.Tenants[tName]; ok {
				tenantSpec = ts
				hasTenant = true
			}
		}
	}
	build := spec.Build
	if build == "" {
		build = deploypolicy.NormalizeBuild(hostBuild)
	}
	regs := spec.AllowedRegistries
	if len(regs) == 0 {
		regs = hostRegs
	}

	var reqJobs []string
	if hasTenant {
		reqJobs = append(reqJobs, tenantSpec.RequiredJobs...)
	}
	reqJobs = append(reqJobs, spec.RequiredJobs...)

	var sharedJobs []string
	sharedJobs = append(sharedJobs, hostSharedJobs...)
	if hasTenant {
		sharedJobs = append(sharedJobs, tenantSpec.SharedRequiredJobs...)
	}
	sharedJobs = append(sharedJobs, spec.SharedRequiredJobs...)

	trustedWeb := spec.TrustedWebPipelines
	if trustedWeb == nil && hasTenant {
		trustedWeb = tenantSpec.TrustedWebPipelines
	}
	if trustedWeb == nil {
		trustedWeb = hostTrustedWeb
	}

	p := deploypolicy.Policy{
		Tenant:                 spec.Tenant,
		RequirePipelineSuccess: spec.RequirePipelineSuccess,
		RequirePipelineSources: pipelineSources(spec.RequirePipelineSources, doc),
		StrictPipelineGate:     effectiveBool(spec.StrictPipelineGate, hostStrictGate),
		PipelineRef:            spec.PipelineRef,
		RequiredJobs:           reqJobs,
		SharedRequiredJobs:     sharedJobs,
		TrustedWebPipelines:    convertTrustedWeb(trustedWeb),
		PruneVolumes:           spec.PruneVolumes,
		HealthTimeout:          spec.HealthTimeout,
		Build:                  build,
		Vars:                   spec.Vars,
		Project:                spec.Project,
		SyncTimeout:            spec.SyncTimeout,
		PullTimeout:            spec.PullTimeout,
		OneshotTimeout:         spec.OneshotTimeout,
		RequireDigest:          effectiveBool(spec.RequireDigest, hostDigest),
		PublicVars:             spec.PublicVars,
		AllowedRegistries:      regs,
		EnvFiles:               spec.EnvFiles,
		CreateVolumes:          doc != nil && doc.CreateVolumes,
		VolumeNamePattern:      hostVolumePattern(doc),
		ReconcileInterval:      inherited(spec.ReconcileInterval, hostInterval(doc)),
		RetryRejected:          inherited(spec.RetryRejected, hostRetry(doc)),
		Priority:               inheritedPriority(spec.Priority, doc),
	}
	for _, ex := range spec.PolicyExceptions {
		p.PolicyExceptions = append(p.PolicyExceptions, deploypolicy.PolicyException{
			Service: ex.Service,
			Rule:    ex.Rule,
			Reason:  ex.Reason,
		})
	}
	for _, w := range spec.WatchFiles {
		p.WatchFiles = append(p.WatchFiles, deploypolicy.WatchFile{
			Path:          w.Path,
			Action:        w.Action,
			Service:       w.Service,
			ReloadURL:     w.ReloadURL,
			ReloadCommand: w.ReloadCommand,
		})
	}
	return deploypolicy.Encode(p)
}

func effectiveBool(app, host *bool) bool {
	if app != nil {
		return *app
	}
	if host != nil {
		return *host
	}
	return false
}

func (r *Reconciler) resolveAppSecret(spec AppSpec, method string) (string, error) {
	switch method {
	case repoauth.MethodDeployKey, repoauth.MethodToken:
		if path := strings.TrimSpace(spec.DeployKeyFile); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("read secret file: %w", err)
			}
			if method == repoauth.MethodDeployKey {
				return repoauth.NormalizeDeployKey(string(data)), nil
			}
			return strings.TrimSpace(string(data)), nil
		}
		if method == repoauth.MethodToken {
			// Empty secret -> ResolveGitToken("") reads TIDE_GIT_TOKEN on each use.
			if t := repoauth.ResolveGitToken(""); t == "" {
				return "", fmt.Errorf("TIDE_GIT_TOKEN is empty")
			}
			return "", nil
		}
		return "", fmt.Errorf("deploy_key_file is required for deploy_key auth")
	default:
		return "", nil
	}
}
