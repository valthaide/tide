package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/cigate"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/fleetcheck"
	"github.com/valthaide/tide/internal/gitremote"
	"github.com/valthaide/tide/internal/gitrev"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/repoauth"
	"github.com/valthaide/tide/internal/syncerr"
)

// RepoMigrationRuntime is implemented by *compose.ComposeExecutor and can be faked in controller unit tests.
type RepoMigrationRuntime interface {
	PrepareRepoMigration(ctx context.Context, p compose.MigrationPrepParams) (compose.MigrationPrepared, error)
	CommitRepoMigration(ctx context.Context, appID, oldComposePath string, envVars map[string]string, prepared *compose.MigrationPrepared) error
}

// ResolveCommitFunc allows tests to stub remote git ls-remote resolution during repository migration.
type ResolveCommitFunc func(repoURL, method string, secret []byte, revision, branch string) (string, error)

// MigrateRepoFromFleet satisfies fleet.RepoMigrator for production-safe fleet source updates
// (repo_url, branch, compose_path, revision, or repo_auth_method).
func (r *Reconciler) MigrateRepoFromFleet(ctx context.Context, existing *api.App, req api.RepoMigrationRequest, nextPolicyJSON string) error {
	req.FromFleet = true
	_, err := r.MigrateRepo(ctx, existing, req, nextPolicyJSON)
	return err
}

// MigrateRepo validates and executes (or dry-runs) a production-safe application source change
// (`repo_url`, `branch`, `compose_path`, `revision`, or `repo_auth_method`) for an existing app.
//
// Zero-downtime & safety guarantees:
//  1. Target repo readability, commit resolution, CI gate, compose render & policy validation,
//     env_file availability, data-bind absence, and image pins are verified BEFORE touching the live stack.
//  2. Rendered compose configuration (image+digest, env, ports, volumes, networks, command/entrypoint, labels)
//     is compared against the currently running containers.
//  3. When configuration is identical, Tide swaps the Git checkout metadata in-place, records the new source
//     and commit, and enters regular reconciliation WITHOUT stopping or recreating any containers (`no_restart`).
//  4. When configuration differs, Tide swaps the checkout in-place and runs standard `docker compose up -d --remove-orphans`
//     WITHOUT `compose down`, recreating only the changed services (`restarted`).
//  5. App.ID, ComposeProject, volumes, and env_file stay unchanged.
//  6. If the new source is unreachable, CI gate is not green, or compose fails to render/validate,
//     Tide keeps the old source and leaves the running application completely untouched.
func (r *Reconciler) MigrateRepo(
	ctx context.Context,
	existing *api.App,
	req api.RepoMigrationRequest,
	nextPolicyJSON string,
) (api.RepoMigrationPlan, error) {
	var plan api.RepoMigrationPlan
	if r == nil || r.Registry == nil || existing == nil {
		return plan, fmt.Errorf("app is required")
	}

	newRepoURL := strings.TrimSpace(req.RepoURL)
	if newRepoURL == "" {
		return plan, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, "repo_url is required")
	}
	method := repoauth.NormalizeMethod(req.RepoAuthMethod)
	if method == "" {
		method = repoauth.NormalizeMethod(existing.RepoAuthMethod)
	}
	if method == "" {
		method = repoauth.MethodPublic
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = existing.Branch
	}
	if branch == "" {
		branch = "main"
	}
	revision := strings.TrimSpace(req.Revision)
	if !req.FromFleet && req.Revision == "" && req.Branch == "" {
		revision = existing.Revision
	}
	// Compose source: fleet template XOR compose_path. Fleet requests carry
	// the authoritative template (possibly empty = back to compose_path);
	// API requests either name a template, keep the current source, or switch
	// back by naming a compose_path for a template-sourced app.
	template := strings.TrimSpace(existing.Template)
	switch {
	case req.FromFleet:
		template = strings.TrimSpace(req.Template)
	case strings.TrimSpace(req.Template) != "":
		template = strings.TrimSpace(req.Template)
	case strings.TrimSpace(req.ComposePath) != "" && template != "":
		template = ""
	}
	composePath := strings.TrimSpace(req.ComposePath)
	if template != "" {
		composePath = ""
	} else if composePath == "" {
		composePath = existing.ComposePath
	}
	if composePath == "" && template == "" {
		composePath = "docker-compose.yml"
	}
	if template != "" && strings.TrimSpace(req.ComposePath) != "" && !req.FromFleet {
		if strings.TrimSpace(req.Template) != "" {
			return plan, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, "template_compose_path_conflict: set template or compose_path, not both")
		}
		// Naming only a compose_path switches a template app back (handled above).
		composePath = strings.TrimSpace(req.ComposePath)
		template = ""
	}
	if issue := fleetcheck.CheckTemplateName(template); issue != nil {
		return plan, syncerr.New(syncerr.CodeValidation, syncerr.StageValidate, issue.Message)
	}
	pollInterval := strings.TrimSpace(req.PollInterval)
	if pollInterval == "" {
		pollInterval = existing.PollInterval
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "admin"
	}
	auditReason := strings.TrimSpace(req.Reason)
	if auditReason == "" {
		auditReason = "source migration"
	}

	// Preserve existing compose project name so volumes (<project>_<vol>) and container names are retained 1:1.
	existingProject := existing.ComposeProject()
	var policy deploypolicy.Policy
	if strings.TrimSpace(nextPolicyJSON) != "" {
		policy = deploypolicy.Decode(nextPolicyJSON)
	} else {
		policy = existing.Policy()
		if len(req.Vars) > 0 {
			policy.Vars = deploypolicy.NormalizeVars(req.Vars)
		}
	}
	if strings.TrimSpace(policy.Project) == "" {
		policy.Project = existingProject
	}

	plan = api.RepoMigrationPlan{
		AppID:          existing.ID,
		AppName:        existing.Name,
		OldRepoURL:     existing.RepoURL,
		NewRepoURL:     newRepoURL,
		OldBranch:      existing.Branch,
		Branch:         branch,
		Revision:       revision,
		OldComposePath: existing.ComposePath,
		ComposePath:    composePath,
		OldTemplate:    existing.Template,
		Template:       template,
		ComposeProject: existingProject,
		DryRun:         req.DryRun,
	}

	recordOutcome := func(decision, metricMode, metricReason string, res cigate.Result, detailErr error) {
		details := map[string]any{
			"dry_run":            req.DryRun,
			"compose_project":    existingProject,
			"old_branch":         existing.Branch,
			"new_branch":         branch,
			"old_compose_path":   existing.ComposePath,
			"new_compose_path":   composePath,
			"old_template":       existing.Template,
			"new_template":       template,
			"has_override":       plan.HasOverride,
			"mode":               plan.Mode,
			"strategy":           plan.Strategy,
			"migration_reason":   plan.Reason,
			"changed_services":   plan.ChangedServices,
			"unchanged_services": plan.UnchangedServices,
			"diff_summary":       plan.DiffSummary,
			"kept_volumes":       plan.KeptVolumes,
		}
		if detailErr != nil {
			details["error"] = redactAppText(existing, detailErr.Error())
		}
		rawDetails, _ := json.Marshal(details)
		_ = r.Registry.RecordAudit(api.AuditEvent{
			Kind:           "repo_migration",
			Actor:          actor,
			AppID:          existing.ID,
			AppName:        existing.Name,
			RepoURL:        existing.RepoURL,
			TargetRepoURL:  newRepoURL,
			GitSHA:         plan.TargetCommit,
			PipelineID:     res.PipelineID,
			PipelineSource: res.Source,
			PipelineStatus: res.Status,
			PipelineURL:    res.WebURL,
			Reason:         auditReason,
			Decision:       decision,
			DetailsJSON:    string(rawDetails),
		})
		if !req.DryRun && metricMode != "" {
			metrics.ObserveRepoMigration(existing.Name, metricMode, metricReason)
		}
	}

	logFailed := func(failReason string, err error) {
		if r.Logger != nil {
			r.Logger.Warn("Application source migration aborted; keeping existing source and running containers",
				"app", existing.Name,
				"app_id", existing.ID,
				"from_repo", existing.RepoURL,
				"to_repo", newRepoURL,
				"from_branch", existing.Branch,
				"to_branch", branch,
				"from_compose_path", existing.ComposePath,
				"to_compose_path", composePath,
				"mode", compose.MigrationModeFailed,
				"reason", failReason,
				"error", redactAppText(existing, err.Error()),
			)
		}
	}

	// Resolve credentials for the new repository without overwriting stored credentials yet.
	var newSecret []byte
	switch method {
	case repoauth.MethodDeployKey, repoauth.MethodToken:
		if strings.TrimSpace(req.DeployKey) != "" {
			s := strings.TrimSpace(req.DeployKey)
			if method == repoauth.MethodDeployKey {
				s = repoauth.NormalizeDeployKey(s)
			}
			newSecret = []byte(s)
		} else if method == repoauth.NormalizeMethod(existing.RepoAuthMethod) {
			sec, sErr := r.Registry.GetRepoSecret(existing.ID)
			if sErr == nil {
				newSecret = sec
			}
		}
	}
	defer zeroBytes(newSecret)

	// 1. Resolve target commit from the new repository BEFORE touching anything.
	resolveFn := r.ResolveMigrationCommit
	if resolveFn == nil {
		resolveFn = resolveRemoteCommit
	}
	targetSHA, revErr := resolveFn(newRepoURL, method, newSecret, revision, branch)
	if revErr != nil {
		se := syncerr.Wrap(syncerr.CodeGitFetch, syncerr.StageFetch, fmt.Errorf("new repository check failed: %w", revErr))
		plan.Mode = compose.MigrationModeFailed
		plan.Reason = "source_unreachable"
		recordOutcome("preflight_failed_git", compose.MigrationModeFailed, "source_unreachable", cigate.Result{}, se)
		logFailed("source_unreachable", se)
		return plan, se
	}
	plan.TargetCommit = targetSHA

	// 2. Verify CI gate on the new source and target commit BEFORE touching anything.
	var ciRes cigate.Result
	candidateApp := *existing
	candidateApp.RepoURL = newRepoURL
	candidateApp.RepoAuthMethod = method
	candidateApp.Branch = branch
	candidateApp.Revision = revision
	candidateApp.ComposePath = composePath
	candidateApp.Template = template
	candidateApp.DeployPolicyJSON = deploypolicy.Encode(policy)
	candidateApp.ApplyPolicy()

	if policy.RequirePipelineSuccess || r.Gate != nil {
		ciRes = r.evaluateCIGate(&candidateApp, targetSHA, newSecret)
		plan.CIStatus = ciRes.Outcome
		plan.CIPipelineID = ciRes.PipelineID
		plan.CIPipelineURL = ciRes.WebURL
		if !ciRes.Allowed {
			detail := ciRes.Detail
			if detail == "" {
				detail = ciRes.Reason
			}
			ciMetricReason := "ci_failed"
			if ciRes.Outcome == cigate.OutcomeWait {
				ciMetricReason = "ci_pending"
			} else if ciRes.Reason == cigate.ReasonCIError {
				ciMetricReason = "ci_error"
			}
			se := syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate,
				fmt.Sprintf("new repository CI is not green (%s: %s)", ciRes.Status, detail))
			plan.Mode = compose.MigrationModeFailed
			plan.Reason = ciMetricReason
			recordOutcome("preflight_blocked_ci", compose.MigrationModeFailed, ciMetricReason, ciRes, se)
			logFailed(ciMetricReason, se)
			return plan, se
		}
	} else {
		plan.CIStatus = "not_required"
	}

	// 3. Stage new repo, render & validate compose, verify env_files, check data binds, verify/pull images, and diff vs running containers.
	envVars, envErr := r.Registry.GetAppEnvs(existing.ID)
	if envErr != nil {
		se := syncerr.Wrap(syncerr.CodeCredentials, syncerr.StageCredentials, envErr)
		plan.Mode = compose.MigrationModeFailed
		plan.Reason = "credentials_error"
		recordOutcome("preflight_failed_credentials", compose.MigrationModeFailed, "credentials_error", ciRes, se)
		logFailed("credentials_error", se)
		return plan, se
	}
	fileVars, missing := readEnvFiles(policy.EnvFiles)
	if missing != "" {
		se := syncerr.New(syncerr.CodeEnvFileMissing, syncerr.StageValidate, "env_file_missing: "+missing)
		plan.Mode = compose.MigrationModeFailed
		plan.Reason = "env_file_missing"
		recordOutcome("preflight_failed_env_file", compose.MigrationModeFailed, "env_file_missing", ciRes, se)
		logFailed("env_file_missing", se)
		return plan, se
	}

	syncTimeout := r.Config.SyncTimeout
	if syncTimeout <= 0 {
		syncTimeout = 5 * time.Minute
	}
	if policy.SyncTimeout != "" {
		if parsed, issue := fleetcheck.ParseDuration(policy.SyncTimeout); issue == nil {
			syncTimeout = parsed
		}
	}
	pullIdle := syncTimeout
	if policy.PullTimeout != "" {
		if parsed, issue := fleetcheck.ParseDuration(policy.PullTimeout); issue == nil {
			pullIdle = parsed
		}
	}
	oneshotTimeout := 10 * time.Minute
	if policy.OneshotTimeout != "" {
		if parsed, issue := fleetcheck.ParseDuration(policy.OneshotTimeout); issue == nil {
			oneshotTimeout = parsed
		}
	}

	applyOpts := compose.ApplyOptions{
		Vars:              mergeInterpolation(fileVars, policy.Vars, existing.Name, existingProject),
		Template:          template,
		BuildMode:         policy.EffectiveBuild(),
		Project:           existingProject,
		RequireDigest:     policy.RequireDigest,
		PullIdle:          pullIdle,
		OneshotTimeout:    oneshotTimeout,
		SyncTimeout:       syncTimeout,
		BindPrefixes:      bindPrefixes(),
		AllowedRegistries: policy.AllowedRegistries,
		PolicyExceptions:  policy.PolicyExceptions,
		CreateVolumes:     policy.CreateVolumes,
		VolumeNamePattern: policy.VolumeNamePattern,
	}

	migRuntime, hasMigRuntime := r.Executor.(RepoMigrationRuntime)
	var prepared compose.MigrationPrepared
	if hasMigRuntime {
		var prepErr error
		prepared, prepErr = migRuntime.PrepareRepoMigration(ctx, compose.MigrationPrepParams{
			AppID:          existing.ID,
			AppName:        existing.Name,
			OldComposePath: existing.ComposePath,
			NewRepoURL:     newRepoURL,
			NewAuthMethod:  method,
			NewRepoSecret:  newSecret,
			NewBranch:      branch,
			TargetCommit:   targetSHA,
			NewComposePath: composePath,
			NewTemplate:    template,
			EnvVars:        envVars,
			Opts:           applyOpts,
			DryRun:         req.DryRun,
		})
		if prepErr != nil {
			se := syncerr.Classify(prepErr)
			failReason := classifyMigrationPrepReason(se)
			plan.Mode = compose.MigrationModeFailed
			plan.Reason = failReason
			recordOutcome("preflight_failed_runtime", compose.MigrationModeFailed, failReason, ciRes, se)
			logFailed(failReason, se)
			return plan, se
		}
		defer prepared.Release()
		plan.HasOverride = prepared.HasOverride
		plan.Mode = prepared.Mode
		plan.Strategy = prepared.Strategy
		plan.Reason = prepared.Reason
		plan.ChangedServices = prepared.ChangedServices
		plan.UnchangedServices = prepared.UnchangedServices
		plan.DiffSummary = prepared.DiffSummary
		plan.ToStop = prepared.ToStop
		plan.ToRemove = prepared.ToRemove
		plan.KeptVolumes = prepared.KeptVolumes
		plan.ToCreate = prepared.ToCreate
		plan.ImagesVerified = prepared.ImagesVerified
	}
	if plan.Mode == "" {
		if prepared.Equivalent {
			plan.Mode = compose.MigrationModeNoRestart
			plan.Strategy = compose.MigrationStrategyAdoptNoRestart
			plan.Reason = "config_identical"
		} else {
			plan.Mode = compose.MigrationModeRestarted
			plan.Strategy = compose.MigrationStrategyComposeUp
			plan.Reason = "config_changed"
		}
	}

	if req.DryRun {
		recordOutcome("dry_run_ok", "", "", ciRes, nil)
		return plan, nil
	}

	// Acquire a sync slot for the cutover so periodic reconcile cannot race with migration.
	slotCtx, cancelSlot := context.WithTimeout(ctx, syncTimeout)
	defer cancelSlot()
	if err := r.AcquireSync(slotCtx); err != nil {
		se := syncerr.New(syncerr.CodeTimeout, syncerr.StageUnknown, "sync slot busy during repo migration")
		plan.Mode = compose.MigrationModeFailed
		plan.Reason = "sync_slot_timeout"
		recordOutcome("cutover_timeout", compose.MigrationModeFailed, "sync_slot_timeout", ciRes, se)
		logFailed("sync_slot_timeout", se)
		return plan, se
	}
	r.mu.Lock()
	r.inFlight[existing.ID] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inFlight, existing.ID)
		r.mu.Unlock()
		r.ReleaseSync()
	}()

	// 4. Swap staged checkout in-place (preserving file inodes, volumes, and running containers; never running compose down).
	if hasMigRuntime {
		if err := migRuntime.CommitRepoMigration(ctx, existing.ID, existing.ComposePath, envVars, &prepared); err != nil {
			se := syncerr.Classify(err)
			plan.Mode = compose.MigrationModeFailed
			plan.Reason = "checkout_swap_failed"
			recordOutcome("cutover_failed_swap", compose.MigrationModeFailed, "checkout_swap_failed", ciRes, se)
			logFailed("checkout_swap_failed", se)
			return plan, se
		}
	}

	// 5. Update repository metadata in-place (preserving App.ID, ComposeProject, volumes, and env_file).
	encodedPolicy := deploypolicy.Encode(policy)
	if pollInterval != "" && pollInterval != existing.PollInterval {
		_ = r.Registry.UpdateApp(existing.ID, existing.Name, branch, composePath, pollInterval, nil)
	}
	if err := r.Registry.UpdateRepository(existing.ID, newRepoURL, method, branch, revision, composePath, template); err != nil {
		recordOutcome("cutover_failed_db", compose.MigrationModeFailed, "db_error", ciRes, err)
		return plan, err
	}
	if err := r.Registry.UpdateDeployPolicy(existing.ID, encodedPolicy); err != nil {
		recordOutcome("cutover_failed_policy", compose.MigrationModeFailed, "db_error", ciRes, err)
		return plan, err
	}
	switch method {
	case repoauth.MethodPublic:
		_ = r.Registry.ClearRepoSecret(existing.ID)
	case repoauth.MethodToken:
		if strings.TrimSpace(req.DeployKey) == "" {
			if req.FromFleet {
				_ = r.Registry.ClearRepoSecret(existing.ID)
			}
		} else {
			_ = r.Registry.UpdateRepoSecret(existing.ID, string(newSecret))
		}
	case repoauth.MethodDeployKey:
		if len(newSecret) > 0 {
			if err := r.Registry.UpdateRepoSecret(existing.ID, string(newSecret)); err != nil {
				recordOutcome("cutover_failed_secret", compose.MigrationModeFailed, "credentials_error", ciRes, err)
				return plan, err
			}
		}
	}
	_ = r.Registry.UpdateCommitWithMessage(existing.ID, targetSHA, "")
	metrics.EnsureGitFetchErrors(newRepoURL)

	updatedApp, err := r.Registry.Get(existing.ID)
	if err != nil {
		recordOutcome("cutover_failed_reload", compose.MigrationModeFailed, "db_error", ciRes, err)
		return plan, err
	}
	if namer, ok := r.Executor.(interface{ RememberProject(appID, project string) }); ok {
		namer.RememberProject(updatedApp.ID, existingProject)
	}

	// 6A. Zero-restart path: rendered configuration matches running containers 1:1.
	if prepared.Equivalent {
		now := time.Now()
		sourceDesc := "compose_path=" + composePath
		if template != "" {
			sourceDesc = fmt.Sprintf("template=%s override=%v", template, prepared.HasOverride)
		}
		adoptOutput := fmt.Sprintf("source adopted without container restart: repo=%s branch=%s %s commit=%s reason=%s",
			newRepoURL, branch, sourceDesc, targetSHA, plan.Reason)
		_ = r.Registry.UpdateSyncResult(updatedApp.ID, "synced", now, targetSHA, "", adoptOutput, "")
		_ = r.Registry.UpdateRollback(updatedApp.ID, targetSHA, "", deploypolicy.EncodeVars(policy.Vars), "")
		_ = r.Registry.ClearRetry(updatedApp.ID)
		r.clearTransient(updatedApp.ID)
		r.dequeue(updatedApp.ID)
		r.recordAppliedEnvHash(updatedApp)
		r.recordAppliedTemplateHash(updatedApp)
		r.recordAppliedTemplateHash(updatedApp)
		r.recordAppliedWatchHash(updatedApp)
		if plan.CIStatus == cigate.OutcomeAllow {
			_ = r.Registry.UpdateCIStatus(updatedApp.ID, "allowed", "ok", ciRes.WebURL, ciRes.Source, ciRes.PipelineID, now)
		}
		_ = r.Registry.RecordRevision(updatedApp.ID, targetSHA, deploypolicy.EncodeVars(policy.Vars), metrics.FleetSHA(), revisionImages(r, updatedApp))
		r.noteDeploy(updatedApp, "migrated_no_restart", targetSHA, deploypolicy.EncodeVars(policy.Vars), revisionImages(r, updatedApp), "migration")
		metrics.SetLastSyncTimestamp(updatedApp.Name)

		plan.Applied = true
		recordOutcome("migrated_no_restart", compose.MigrationModeNoRestart, plan.Reason, ciRes, nil)
		if r.Logger != nil {
			r.Logger.Info("Application source migrated without restarting containers",
				"app", updatedApp.Name,
				"app_id", updatedApp.ID,
				"from_repo", existing.RepoURL,
				"to_repo", newRepoURL,
				"from_branch", existing.Branch,
				"to_branch", branch,
				"from_compose_path", existing.ComposePath,
				"to_compose_path", composePath,
				"commit", targetSHA,
				"project", existingProject,
				"mode", plan.Mode,
				"strategy", plan.Strategy,
				"reason", plan.Reason,
				"unchanged_services", strings.Join(plan.UnchangedServices, ","),
			)
		}
		return plan, nil
	}

	// 6B. Changed-config path: apply via standard `compose up -d --remove-orphans` (no `compose down`),
	// so Docker Compose recreates ONLY the changed services while leaving unchanged services running.
	output, applyErr := r.applyRuntime(ctx, updatedApp, envVars, newSecret, targetSHA, applyOpts, nil)
	if applyErr != nil {
		se := syncerr.Classify(applyErr)
		_ = r.Registry.UpdateSyncResultErr(updatedApp.ID, "error", time.Now(), existing.LastSyncedCommit, existing.LastSyncedCommitMessage, output, se)
		recordOutcome("cutover_failed_up", compose.MigrationModeFailed, "compose_apply_failed", ciRes, se)
		logFailed("compose_apply_failed", se)
		return plan, se
	}
	if err := r.confirmHealthy(ctx, updatedApp, targetSHA); err != nil {
		se := syncerr.New(syncerr.CodeUnhealthy, syncerr.StageHealth, err.Error())
		_ = r.Registry.UpdateSyncResultErr(updatedApp.ID, "error", time.Now(), existing.LastSyncedCommit, existing.LastSyncedCommitMessage, output, se)
		recordOutcome("cutover_unhealthy", compose.MigrationModeFailed, "unhealthy", ciRes, se)
		logFailed("unhealthy", se)
		return plan, se
	}

	now := time.Now()
	_ = r.Registry.UpdateSyncResult(updatedApp.ID, "synced", now, targetSHA, "", output, "")
	_ = r.Registry.UpdateRollback(updatedApp.ID, targetSHA, "", deploypolicy.EncodeVars(policy.Vars), "")
	_ = r.Registry.ClearRetry(updatedApp.ID)
	r.clearTransient(updatedApp.ID)
	r.dequeue(updatedApp.ID)
	r.recordAppliedEnvHash(updatedApp)
	r.recordAppliedTemplateHash(updatedApp)
	r.recordAppliedWatchHash(updatedApp)
	if plan.CIStatus == cigate.OutcomeAllow {
		_ = r.Registry.UpdateCIStatus(updatedApp.ID, "allowed", "ok", ciRes.WebURL, ciRes.Source, ciRes.PipelineID, now)
	}
	_ = r.Registry.RecordRevision(updatedApp.ID, targetSHA, deploypolicy.EncodeVars(policy.Vars), metrics.FleetSHA(), revisionImages(r, updatedApp))
	r.noteDeploy(updatedApp, "migrated_restarted", targetSHA, deploypolicy.EncodeVars(policy.Vars), revisionImages(r, updatedApp), "migration")
	metrics.SetLastSyncTimestamp(updatedApp.Name)

	plan.Applied = true
	recordOutcome("migrated_restarted", compose.MigrationModeRestarted, plan.Reason, ciRes, nil)
	if r.Logger != nil {
		r.Logger.Info("Application source migrated with targeted service update",
			"app", updatedApp.Name,
			"app_id", updatedApp.ID,
			"from_repo", existing.RepoURL,
			"to_repo", newRepoURL,
			"from_branch", existing.Branch,
			"to_branch", branch,
			"from_compose_path", existing.ComposePath,
			"to_compose_path", composePath,
			"commit", targetSHA,
			"project", existingProject,
			"mode", plan.Mode,
			"strategy", plan.Strategy,
			"reason", plan.Reason,
			"changed_services", strings.Join(plan.ChangedServices, ","),
			"unchanged_services", strings.Join(plan.UnchangedServices, ","),
		)
	}
	return plan, nil
}

func classifyMigrationPrepReason(se *syncerr.Error) string {
	if se == nil {
		return "preflight_failed"
	}
	switch se.GetCode() {
	case syncerr.CodeGitAuth, syncerr.CodeGitFetch:
		return "source_unreachable"
	case syncerr.CodeEnvFileMissing:
		return "env_file_missing"
	case syncerr.CodeImageNotFound:
		return "image_not_found"
	case syncerr.CodePullFailed, syncerr.CodeRegistryAuth, syncerr.CodeRegistryDenied:
		return "image_pull_failed"
	case syncerr.CodeDigestRequired:
		return "digest_required"
	case syncerr.CodeBuildForbidden:
		return "build_forbidden"
	case syncerr.CodePortConflict:
		return "port_conflict"
	case syncerr.CodeUnsetVar, syncerr.CodeValidation:
		return "compose_invalid"
	default:
		if strings.Contains(strings.ToLower(se.Error()), "compose config") {
			return "compose_invalid"
		}
		return "preflight_failed"
	}
}

func resolveRemoteCommit(repoURL, method string, secret []byte, revision, branch string) (string, error) {
	if gitrev.IsFullSHA(revision) {
		return strings.ToLower(strings.TrimSpace(revision)), nil
	}
	var auth transport.AuthMethod
	switch repoauth.NormalizeMethod(method) {
	case repoauth.MethodToken:
		tok := repoauth.ResolveGitToken(string(secret))
		if tok == "" {
			return "", fmt.Errorf("git token is empty")
		}
		auth = repoauth.HTTPBasicAuth(tok)
	case repoauth.MethodDeployKey:
		if len(secret) == 0 {
			return "", fmt.Errorf("deploy key is empty")
		}
		kh, err := repoauth.ResolveKnownHostsForRepo(repoURL)
		if err != nil {
			return "", err
		}
		cb, err := repoauth.NewHostKeyCallback(kh)
		if err != nil {
			return "", err
		}
		pk, err := gitssh.NewPublicKeys("git", secret, "")
		if err != nil {
			return "", err
		}
		pk.HostKeyCallbackHelper = gitssh.HostKeyCallbackHelper{HostKeyCallback: cb}
		auth = pk
	}

	cleanURL := repoauth.CleanHTTPSURL(repoURL)
	refs, err := gitremote.List(cleanURL, auth)
	if err != nil {
		return "", err
	}
	target, err := gitrev.Resolve(revision, branch, refs)
	if err != nil {
		return "", err
	}
	return target.SHA, nil
}
