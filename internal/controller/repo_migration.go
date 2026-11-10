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

// MigrateRepoFromFleet satisfies fleet.RepoMigrator for production-safe fleet repo_url updates.
func (r *Reconciler) MigrateRepoFromFleet(ctx context.Context, existing *api.App, req api.RepoMigrationRequest, nextPolicyJSON string) error {
	_, err := r.MigrateRepo(ctx, existing, req, nextPolicyJSON)
	return err
}

// MigrateRepo validates and executes (or dry-runs) a production-safe repository change for an existing app.
//
// Safety guarantees:
//  1. New repo readability, target commit, CI gate, compose validity, data-bind absence, and image pins
//     are verified BEFORE stopping the currently running stack.
//  2. If any preflight check or `compose down` fails, the old stack continues running untouched.
//  3. Named and external volumes and persistent data are NEVER removed regardless of prune_volumes.
//  4. App ID and ComposeProject stay identical so the new stack attaches to the existing volumes;
//     restoring the previous repo_url cleanly rolls the migration back.
//  5. Every attempt and outcome is recorded in the persistent audit log.
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
	if req.Revision == "" && req.Branch == "" {
		revision = existing.Revision
	}
	composePath := strings.TrimSpace(req.ComposePath)
	if composePath == "" {
		composePath = existing.ComposePath
	}
	if composePath == "" {
		composePath = "docker-compose.yml"
	}
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "admin"
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "repository migration"
	}

	// Preserve existing compose project name so volumes (<project>_<vol>) are retained 1:1.
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
		Branch:         branch,
		Revision:       revision,
		ComposePath:    composePath,
		ComposeProject: existingProject,
		DryRun:         req.DryRun,
	}

	auditOutcome := func(decision string, res cigate.Result, detailErr error) {
		details := map[string]any{
			"dry_run":         req.DryRun,
			"compose_project": existingProject,
			"kept_volumes":    plan.KeptVolumes,
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
			Reason:         reason,
			Decision:       decision,
			DetailsJSON:    string(rawDetails),
		})
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

	// 1. Resolve target commit from the new repository BEFORE stopping anything.
	resolveFn := r.ResolveMigrationCommit
	if resolveFn == nil {
		resolveFn = resolveRemoteCommit
	}
	targetSHA, revErr := resolveFn(newRepoURL, method, newSecret, revision, branch)
	if revErr != nil {
		se := syncerr.Wrap(syncerr.CodeGitFetch, syncerr.StageFetch, fmt.Errorf("new repository check failed: %w", revErr))
		auditOutcome("preflight_failed_git", cigate.Result{}, se)
		return plan, se
	}
	plan.TargetCommit = targetSHA

	// 2. Verify CI gate on the new repository and target commit BEFORE stopping anything.
	var ciRes cigate.Result
	candidateApp := *existing
	candidateApp.RepoURL = newRepoURL
	candidateApp.Branch = branch
	candidateApp.Revision = revision
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
			se := syncerr.New(syncerr.CodeBlockedByCI, syncerr.StageCIGate,
				fmt.Sprintf("new repository CI is not green (%s: %s)", ciRes.Status, detail))
			auditOutcome("preflight_blocked_ci", ciRes, se)
			return plan, se
		}
	} else {
		plan.CIStatus = "not_required"
	}

	// 3. Stage new repo, validate compose & policy, check persistent data binds, and verify/pull all images.
	envVars, envErr := r.Registry.GetAppEnvs(existing.ID)
	if envErr != nil {
		se := syncerr.Wrap(syncerr.CodeCredentials, syncerr.StageCredentials, envErr)
		auditOutcome("preflight_failed_credentials", ciRes, se)
		return plan, se
	}
	fileVars, missing := readEnvFiles(policy.EnvFiles)
	if missing != "" {
		se := syncerr.New(syncerr.CodeEnvFileMissing, syncerr.StageValidate, "env_file_missing: "+missing)
		auditOutcome("preflight_failed_env_file", ciRes, se)
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

	applyOpts := compose.ApplyOptions{
		Vars:              mergeInterpolation(fileVars, policy.Vars, existing.Name, existingProject),
		BuildMode:         policy.EffectiveBuild(),
		Project:           existingProject,
		RequireDigest:     policy.RequireDigest,
		PullIdle:          pullIdle,
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
			EnvVars:        envVars,
			Opts:           applyOpts,
			DryRun:         req.DryRun,
		})
		if prepErr != nil {
			se := syncerr.Classify(prepErr)
			auditOutcome("preflight_failed_runtime", ciRes, se)
			return plan, se
		}
		defer prepared.Release()
		plan.ToStop = prepared.ToStop
		plan.ToRemove = prepared.ToRemove
		plan.KeptVolumes = prepared.KeptVolumes
		plan.ToCreate = prepared.ToCreate
		plan.ImagesVerified = prepared.ImagesVerified
	}

	if req.DryRun {
		auditOutcome("dry_run_ok", ciRes, nil)
		return plan, nil
	}

	// Acquire a sync slot for the cutover so periodic reconcile cannot race with migration.
	slotCtx, cancelSlot := context.WithTimeout(ctx, syncTimeout)
	defer cancelSlot()
	if err := r.AcquireSync(slotCtx); err != nil {
		se := syncerr.New(syncerr.CodeTimeout, syncerr.StageUnknown, "sync slot busy during repo migration")
		auditOutcome("cutover_timeout", ciRes, se)
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

	// 4. Stop old compose stack (strictly failing if compose down fails; never pruning volumes) and swap checkout.
	if hasMigRuntime {
		if err := migRuntime.CommitRepoMigration(ctx, existing.ID, existing.ComposePath, envVars, &prepared); err != nil {
			se := syncerr.Classify(err)
			auditOutcome("cutover_failed_down", ciRes, se)
			return plan, se
		}
	}

	// 5. Update repository metadata in-place (preserving App.ID and ComposeProject).
	encodedPolicy := deploypolicy.Encode(policy)
	if err := r.Registry.UpdateRepository(existing.ID, newRepoURL, method, branch, revision, composePath); err != nil {
		auditOutcome("cutover_failed_db", ciRes, err)
		return plan, err
	}
	if err := r.Registry.UpdateDeployPolicy(existing.ID, encodedPolicy); err != nil {
		auditOutcome("cutover_failed_policy", ciRes, err)
		return plan, err
	}
	switch method {
	case repoauth.MethodPublic:
		_ = r.Registry.ClearRepoSecret(existing.ID)
	case repoauth.MethodToken:
		if strings.TrimSpace(req.DeployKey) == "" {
			_ = r.Registry.ClearRepoSecret(existing.ID)
		} else {
			_ = r.Registry.UpdateRepoSecret(existing.ID, string(newSecret))
		}
	case repoauth.MethodDeployKey:
		if len(newSecret) > 0 {
			if err := r.Registry.UpdateRepoSecret(existing.ID, string(newSecret)); err != nil {
				auditOutcome("cutover_failed_secret", ciRes, err)
				return plan, err
			}
		}
	}
	_ = r.Registry.UpdateCommitWithMessage(existing.ID, targetSHA, "")

	// 6. Start the new stack on the preserved compose project and volumes.
	updatedApp, err := r.Registry.Get(existing.ID)
	if err != nil {
		auditOutcome("cutover_failed_reload", ciRes, err)
		return plan, err
	}
	if namer, ok := r.Executor.(interface{ RememberProject(appID, project string) }); ok {
		namer.RememberProject(updatedApp.ID, existingProject)
	}
	output, applyErr := r.applyRuntime(ctx, updatedApp, envVars, newSecret, targetSHA, applyOpts, nil)
	if applyErr != nil {
		se := syncerr.Classify(applyErr)
		_ = r.Registry.UpdateSyncResultErr(updatedApp.ID, "error", time.Now(), existing.LastSyncedCommit, existing.LastSyncedCommitMessage, output, se)
		auditOutcome("cutover_failed_up", ciRes, se)
		return plan, se
	}
	if err := r.confirmHealthy(ctx, updatedApp, targetSHA); err != nil {
		se := syncerr.New(syncerr.CodeUnhealthy, syncerr.StageHealth, err.Error())
		_ = r.Registry.UpdateSyncResultErr(updatedApp.ID, "error", time.Now(), existing.LastSyncedCommit, existing.LastSyncedCommitMessage, output, se)
		auditOutcome("cutover_unhealthy", ciRes, se)
		return plan, se
	}

	now := time.Now()
	_ = r.Registry.UpdateSyncResult(updatedApp.ID, "synced", now, targetSHA, "", output, "")
	_ = r.Registry.UpdateRollback(updatedApp.ID, targetSHA, "", deploypolicy.EncodeVars(policy.Vars), "")
	if plan.CIStatus == cigate.OutcomeAllow {
		_ = r.Registry.UpdateCIStatus(updatedApp.ID, "allowed", "ok", ciRes.WebURL, ciRes.Source, ciRes.PipelineID, now)
	}
	plan.Applied = true
	auditOutcome("migrated", ciRes, nil)
	if r.Logger != nil {
		r.Logger.Info("Repository migrated safely",
			"app", updatedApp.Name,
			"app_id", updatedApp.ID,
			"from_repo", existing.RepoURL,
			"to_repo", newRepoURL,
			"commit", targetSHA,
			"project", existingProject,
		)
	}
	return plan, nil
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
