package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/valthaide/tide/internal/api"
	"github.com/valthaide/tide/internal/apiauth"
	"github.com/valthaide/tide/internal/cigate"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/credentials"
	"github.com/valthaide/tide/internal/deploypolicy"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/store"
	"github.com/valthaide/tide/internal/syncerr"
)

type mapGate struct {
	byRepo map[string]func(cigate.CheckRequest) cigate.Result
}

func (m *mapGate) CheckCommit(_ context.Context, repoURL, commitSHA string) cigate.Result {
	return m.Check(context.Background(), cigate.CheckRequest{RepoURL: repoURL, CommitSHA: commitSHA})
}

func (m *mapGate) Check(_ context.Context, req cigate.CheckRequest) cigate.Result {
	if fn, ok := m.byRepo[req.RepoURL]; ok {
		return fn(req)
	}
	return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success", Reason: cigate.ReasonOK}
}

func TestMultiTenantRepoIsolationAndStatusFields(t *testing.T) {
	reg := testRegistry(t)
	dev1 := &api.App{
		Name: "dev-1-backend", RepoURL: "https://git.example/tenants/dev-1.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "1111111111111111111111111111111111111111", Status: "pending",
	}
	dev2 := &api.App{
		Name: "dev-2-backend", RepoURL: "https://git.example/tenants/dev-2.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "2222222222222222222222222222222222222222", Status: "pending",
	}
	addPublicApp(t, reg, dev1)
	addPublicApp(t, reg, dev2)
	pol := deploypolicy.Encode(deploypolicy.Policy{RequirePipelineSuccess: true})
	_ = reg.UpdateDeployPolicy(dev1.ID, pol)
	_ = reg.UpdateDeployPolicy(dev2.ID, pol)

	rt := &fakeRuntime{}
	r := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 2})
	r.Gate = &mapGate{byRepo: map[string]func(cigate.CheckRequest) cigate.Result{
		dev1.RepoURL: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{
				Allowed: false, Outcome: cigate.OutcomeDeny, Status: "failed",
				Reason: cigate.ReasonCIFailed, Detail: "required job test failed",
				WebURL: "https://git.example/tenants/dev-1/-/jobs/77", PipelineID: 501, Source: "push", TriggeredBy: "dev1-user",
			}
		},
		dev2.RepoURL: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{
				Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success",
				Reason: cigate.ReasonOK, Detail: "ok",
				WebURL: "https://git.example/tenants/dev-2/-/pipelines/502", PipelineID: 502, Source: "push", TriggeredBy: "dev2-user",
			}
		},
	}}

	r.reconcileOnce()

	got1, _ := reg.Get(dev1.ID)
	got2, _ := reg.Get(dev2.ID)
	if got1.Status != "error" || got1.CIStatus != "blocked" || got1.CIURL != "https://git.example/tenants/dev-1/-/jobs/77" || got1.CISource != "push" || got1.CICheckedAt.IsZero() {
		t.Fatalf("dev-1 status/CI metadata wrong: %+v", got1)
	}
	if got2.Status != "synced" || got2.LastSyncedCommit != dev2.LastSeenCommit || got2.CIStatus != "allowed" {
		t.Fatalf("dev-2 should deploy unaffected by dev-1 red CI: %+v", got2)
	}
}

func TestJobRetryUnblocksSameCommitAndRetryCmdDoesNotBypass(t *testing.T) {
	reg := testRegistry(t)
	app := &api.App{
		Name: "backend", RepoURL: "https://git.example/org/backend.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111", Status: "pending",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{RequirePipelineSuccess: true}))

	phase := 0
	rt := &fakeRuntime{}
	r := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 1})
	r.Gate = &mapGate{byRepo: map[string]func(cigate.CheckRequest) cigate.Result{
		app.RepoURL: func(cigate.CheckRequest) cigate.Result {
			switch phase {
			case 0:
				return cigate.Result{Allowed: false, Outcome: cigate.OutcomeDeny, Status: "failed", Reason: cigate.ReasonCIFailed, Detail: "job test failed", PipelineID: 88, Source: "push"}
			case 1:
				return cigate.Result{Allowed: false, Outcome: cigate.OutcomeWait, Status: "running", Reason: cigate.ReasonCIPending, Detail: "waiting for retried job test", PipelineID: 88, Source: "push"}
			default:
				return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success", Reason: cigate.ReasonOK, PipelineID: 88, Source: "push"}
			}
		},
	}}

	// Initial failed push pipeline -> blocked.
	r.reconcileOnce()
	got, _ := reg.Get(app.ID)
	if got.Status != "error" || got.CIStatus != "blocked" || len(rt.applyCalls) != 0 {
		t.Fatalf("phase 0: %+v calls=%v", got, rt.applyCalls)
	}

	// Regular RetryApp triggers an immediate check, NOT a bypass: while CI is still failed, it stays blocked.
	if err := r.RetryApp(got); err != nil {
		t.Fatal(err)
	}
	r.reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("retry must never bypass red CI: %v", rt.applyCalls)
	}

	// Operator retries the failed job in GitLab on the same pipeline 88 -> status transitions failed -> waiting.
	phase = 1
	r.reconcileOnce()
	got, _ = reg.Get(app.ID)
	if got.Status != "pending" || got.CIStatus != "waiting" || len(rt.applyCalls) != 0 {
		t.Fatalf("phase 1 want pending/waiting: status=%s ci=%s", got.Status, got.CIStatus)
	}

	// Retried job finishes green on the same commit -> deploys automatically without new commit or restart.
	phase = 2
	r.reconcileOnce()
	got, _ = reg.Get(app.ID)
	if got.Status != "synced" || got.CIStatus != "allowed" || len(rt.applyCalls) != 1 {
		t.Fatalf("phase 2 want synced/allowed: status=%s ci=%s calls=%v", got.Status, got.CIStatus, rt.applyCalls)
	}
}

func TestHardenedForceBypassSingleUseSHABoundAndAuditedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tide.db")
	keyPath := filepath.Join(dir, "tide-encryption.key")

	db, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.NewServiceFromEnv(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(db, creds)

	const shaA = "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"
	const shaB = "bbbb0000bbbb0000bbbb0000bbbb0000bbbb0000"

	app1 := &api.App{
		Name: "app-1", RepoURL: "https://git.example/t/app1.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s", LastSeenCommit: shaA, Status: "pending",
	}
	app2 := &api.App{
		Name: "app-2", RepoURL: "https://git.example/t/app2.git", Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s", LastSeenCommit: shaA, Status: "pending",
	}
	addPublicApp(t, reg, app1)
	addPublicApp(t, reg, app2)
	pol := deploypolicy.Encode(deploypolicy.Policy{RequirePipelineSuccess: true})
	_ = reg.UpdateDeployPolicy(app1.ID, pol)
	_ = reg.UpdateDeployPolicy(app2.ID, pol)

	rt := &fakeRuntime{}
	rec := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 2})
	rec.Gate = &mapGate{byRepo: map[string]func(cigate.CheckRequest) cigate.Result{
		app1.RepoURL: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{Allowed: false, Outcome: cigate.OutcomeDeny, Status: "failed", Reason: cigate.ReasonCIFailed, PipelineID: 900, Source: "push"}
		},
		app2.RepoURL: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{Allowed: false, Outcome: cigate.OutcomeDeny, Status: "failed", Reason: cigate.ReasonCIFailed, PipelineID: 901, Source: "push"}
		},
	}}

	t.Setenv("TIDE_API_TOKEN", "admin-secret-token")
	t.Setenv("TIDE_OPERATOR_TOKENS", "viewer:viewer-token")
	t.Setenv("TIDE_FORCE_TOKENS", "oncall:oncall-force-token")
	auth, err := apiauth.Setup(dir)
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(reg, nil, rt, nil, nil, "v2.0.0")
	h.SyncGate = rec
	router := chi.NewRouter()
	router.Use(auth.Middleware)
	router.Post("/api/v1/apps/{id}/reconcile", h.ReconcileApp)

	// 1. Unauthorized operator token cannot use --force (403).
	unauthReq := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app1.ID+"/reconcile",
		strings.NewReader(`{"force":true,"sha":"`+shaA+`","reason":"incident 42"}`))
	unauthReq.Header.Set("Authorization", "Bearer viewer-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, unauthReq)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-force token, got %d", rr.Code)
	}

	// 2. Authorized token without reason or SHA is rejected (400).
	noReasonReq := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app1.ID+"/reconcile",
		strings.NewReader(`{"force":true,"sha":"`+shaA+`"}`))
	noReasonReq.Header.Set("Authorization", "Bearer oncall-force-token")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, noReasonReq)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when reason missing, got %d", rr.Code)
	}

	// 3. Grant bypass for app1@shaA, then move commit to shaB before sync -> SHA change revokes bypass!
	if err := rec.GrantBypass(BypassRequest{AppID: app1.ID, SHA: shaA, Reason: "hotfix incident 101", Actor: "oncall"}); err != nil {
		t.Fatal(err)
	}
	_ = reg.UpdateCommitWithMessage(app1.ID, shaB, "next commit")
	rec.reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("bypass must be revoked when SHA changes, got applyCalls=%v", rt.applyCalls)
	}

	// 4. Grant valid bypass for app1@shaB -> deploys app1 ONCE, leaves app2 still blocked!
	if err := rec.GrantBypass(BypassRequest{AppID: app1.ID, SHA: shaB, Reason: "approved breakglass", Actor: "oncall"}); err != nil {
		t.Fatal(err)
	}
	_ = reg.UpdateStatus(app1.ID, "pending", nil)
	rec.reconcileOnce()
	got1, _ := reg.Get(app1.ID)
	got2, _ := reg.Get(app2.ID)
	if got1.Status != "synced" || got1.LastSyncedCommit != shaB {
		t.Fatalf("app1 should deploy via single-use bypass: %+v", got1)
	}
	if got2.Status != "error" {
		t.Fatalf("bypass on app1 must never affect app2: %+v", got2)
	}

	// 5. Reopen SQLite DB (simulate controller restart) -> audit trail persists with actor, sha, pipeline, reason, decision.
	db.Close()
	db2, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	reg2 := NewRegistry(db2, creds)
	events, err := reg2.ListAudit(app1.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundConsumed := false
	foundRevoked := false
	foundDenied := false
	for _, ev := range events {
		if ev.Decision == "bypass_consumed" && ev.Actor == "oncall" && ev.GitSHA == shaB && ev.PipelineID == 900 && ev.Reason == "approved breakglass" {
			foundConsumed = true
		}
		if ev.Decision == "bypass_revoked_sha_changed" {
			foundRevoked = true
		}
		if ev.Decision == "bypass_denied_unauthorized" && ev.Actor == "viewer" {
			foundDenied = true
		}
	}
	if !foundConsumed || !foundRevoked || !foundDenied {
		raw, _ := json.MarshalIndent(events, "", "  ")
		t.Fatalf("missing expected persistent audit records:\n%s", string(raw))
	}
}

type fakeMigRuntime struct {
	fakeRuntime
	prepErr           error
	downErr           error
	downCalls         int
	equivalent        bool
	reason            string
	changedServices   []string
	unchangedServices []string
	keptVols          []string
	stopped           []string
	// noContainers makes ProjectHealth report an empty project (v2.4.1
	// pending-adopt path). Default false keeps the "has containers" path.
	noContainers bool
}

func (f *fakeMigRuntime) ProjectHealth(context.Context, string) (compose.ProjectRuntimeState, error) {
	if f.noContainers {
		return compose.ProjectRuntimeState{}, nil
	}
	return compose.ProjectRuntimeState{ContainerCount: 1, RunningCount: 1}, nil
}

func (f *fakeMigRuntime) PrepareRepoMigration(_ context.Context, p compose.MigrationPrepParams) (compose.MigrationPrepared, error) {
	if f.prepErr != nil {
		return compose.MigrationPrepared{}, f.prepErr
	}
	mode := compose.MigrationModeRestarted
	strategy := compose.MigrationStrategyComposeUp
	reason := f.reason
	if f.equivalent {
		mode = compose.MigrationModeNoRestart
		strategy = compose.MigrationStrategyAdoptNoRestart
		if reason == "" {
			reason = "config_identical"
		}
		return compose.MigrationPrepared{
			StagingDir:        filepath.Join(p.AppID, "staged"),
			ProjectName:       p.Opts.Project,
			Equivalent:        true,
			Mode:              mode,
			Strategy:          strategy,
			Reason:            reason,
			UnchangedServices: append([]string(nil), f.unchangedServices...),
			KeptVolumes:       append([]string(nil), f.keptVols...),
			ImagesVerified:    []string{"registry.example/tenant2/backend@sha256:abcdef"},
		}, nil
	}
	if reason == "" {
		reason = "image_changed"
	}
	return compose.MigrationPrepared{
		StagingDir:        filepath.Join(p.AppID, "staged"), // marker for CommitRepoMigration
		ProjectName:       p.Opts.Project,
		Equivalent:        false,
		Mode:              mode,
		Strategy:          strategy,
		Reason:            reason,
		ChangedServices:   append([]string(nil), f.changedServices...),
		UnchangedServices: append([]string(nil), f.unchangedServices...),
		ToStop:            append([]string(nil), f.stopped...),
		ToRemove:          []string{"/runtime/" + p.AppID + "/repo"},
		KeptVolumes:       append([]string(nil), f.keptVols...),
		ToCreate:          []string{"registry.example/tenant2/backend@sha256:abcdef"},
		ImagesVerified:    []string{"registry.example/tenant2/backend@sha256:abcdef"},
	}, nil
}

func (f *fakeMigRuntime) CommitRepoMigration(_ context.Context, _, _ string, _ map[string]string, _ *compose.MigrationPrepared) error {
	f.downCalls++
	return f.downErr
}

func TestSafeRepoMigrationPreflightDataPreservationDryRunAndRollback(t *testing.T) {
	reg := testRegistry(t)
	const oldRepo = "https://git.example/tenants/old-backend.git"
	const newRepo = "https://git.example/tenants/new-backend.git"
	const oldSHA = "1111222233334444555566667777888899990000"
	const newSHA = "aaaabbbbccccddddeeeeffff0000111122223333"

	app := &api.App{
		Name: "prod-club-1", RepoURL: oldRepo, Branch: "main",
		ComposePath: "compose.yaml", PollInterval: "30s",
		LastSeenCommit: oldSHA, LastSyncedCommit: oldSHA, Status: "synced",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateStatus(app.ID, "synced", nil)
	// Even with PruneVolumes: true, repository migration must NEVER delete volumes!
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(deploypolicy.Policy{
		RequirePipelineSuccess: true,
		PruneVolumes:           true,
	}))
	origApp, _ := reg.Get(app.ID)
	origProject := origApp.ComposeProject()

	rt := &fakeMigRuntime{
		keptVols: []string{origProject + "_pgdata", origProject + "_uploads"},
		stopped:  []string{"postgres (" + origProject + "-postgres-1)", "api (" + origProject + "-api-1)"},
	}
	ciGreen := true
	rec := testReconciler(t, reg, rt, ReconcilerConfig{MaxConcurrent: 2})
	rec.ResolveMigrationCommit = func(repoURL, _ string, _ []byte, _, _ string) (string, error) {
		if repoURL == newRepo {
			return newSHA, nil
		}
		if repoURL == oldRepo {
			return oldSHA, nil
		}
		return "", fmt.Errorf("repo unreachable")
	}
	rec.Gate = &mapGate{byRepo: map[string]func(cigate.CheckRequest) cigate.Result{
		newRepo: func(cigate.CheckRequest) cigate.Result {
			if !ciGreen {
				return cigate.Result{Allowed: false, Outcome: cigate.OutcomeDeny, Status: "failed", Reason: cigate.ReasonCIFailed, PipelineID: 700, Source: "push"}
			}
			return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success", Reason: cigate.ReasonOK, PipelineID: 701, Source: "push"}
		},
		oldRepo: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success", Reason: cigate.ReasonOK, PipelineID: 699, Source: "push"}
		},
	}}

	// 1. Missing image in target repo -> must NOT stop old app (downCalls == 0) and must keep old repo_url (Criterion 11).
	rt.prepErr = syncerr.New(syncerr.CodeImageNotFound, syncerr.StagePull, "image_not_found: registry.example/missing:pin")
	if _, err := rec.MigrateRepo(context.Background(), origApp, api.RepoMigrationRequest{
		RepoURL: newRepo, Actor: "sre", Reason: "move to dedicated repo",
	}, ""); err == nil {
		t.Fatal("expected missing image to abort migration")
	}
	if rt.downCalls != 0 {
		t.Fatalf("old app was stopped despite unavailable image! downCalls=%d", rt.downCalls)
	}
	stillOld, _ := reg.Get(app.ID)
	if stillOld.RepoURL != oldRepo || stillOld.ComposePath != "compose.yaml" {
		t.Fatalf("old source altered after failed preflight: %+v", stillOld)
	}
	// With running containers, missing image marks error and retries next cycle
	// without adopting the new source (v2.4.1).
	if stillOld.Status != "error" || stillOld.LastSyncErrorCode != syncerr.CodeImageNotFound {
		t.Fatalf("expected error hold with old source: status=%s code=%s", stillOld.Status, stillOld.LastSyncErrorCode)
	}

	// 2. Red CI on target repo -> must NOT stop old app.
	rt.prepErr = nil
	ciGreen = false
	if _, err := rec.MigrateRepo(context.Background(), origApp, api.RepoMigrationRequest{
		RepoURL: newRepo, Actor: "sre", Reason: "move to dedicated repo",
	}, ""); err == nil {
		t.Fatal("expected red CI to abort migration")
	}
	if rt.downCalls != 0 {
		t.Fatalf("old app stopped despite red CI! downCalls=%d", rt.downCalls)
	}

	// 3. Dry run -> returns full plan without stopping or mutating app.
	ciGreen = true
	dryPlan, err := rec.MigrateRepo(context.Background(), origApp, api.RepoMigrationRequest{
		RepoURL: newRepo, DryRun: true, Actor: "sre", Reason: "check cutover plan",
	}, "")
	if err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	if dryPlan.Applied || !dryPlan.DryRun || rt.downCalls != 0 || len(dryPlan.ToStop) != 2 || len(dryPlan.KeptVolumes) != 2 {
		t.Fatalf("unexpected dry-run plan: %+v downCalls=%d", dryPlan, rt.downCalls)
	}

	// 4a. Without --allow-restart a restarted plan is refused and nothing is touched.
	if _, err := rec.MigrateRepo(context.Background(), origApp, api.RepoMigrationRequest{
		RepoURL: newRepo, Actor: "sre", Reason: "cutover",
	}, ""); err == nil || !strings.Contains(err.Error(), "migration_requires_allow_restart") {
		t.Fatalf("expected allow_restart gate, got %v", err)
	}
	if rt.downCalls != 0 {
		t.Fatalf("allow_restart gate still stopped containers: downCalls=%d", rt.downCalls)
	}

	// 4. `compose down` error -> halts procedure before changing repo_url or starting new stack.
	// Operator cutovers that would recreate services need --allow-restart (AllowRestart).
	rt.downErr = fmt.Errorf("compose down exit 1")
	if _, err := rec.MigrateRepo(context.Background(), origApp, api.RepoMigrationRequest{
		RepoURL: newRepo, Actor: "sre", Reason: "cutover", AllowRestart: true,
	}, ""); err == nil {
		t.Fatal("expected compose down failure to abort migration")
	}
	afterDownErr, _ := reg.Get(app.ID)
	if afterDownErr.RepoURL != oldRepo || len(rt.applyCalls) != 0 {
		t.Fatalf("migration proceeded after compose down error: repo=%s applyCalls=%v", afterDownErr.RepoURL, rt.applyCalls)
	}

	// 5. Clean migration -> preserves App.ID, ComposeProject, and KeptVolumes (Criterion 12).
	rt.downErr = nil
	appliedPlan, err := rec.MigrateRepo(context.Background(), afterDownErr, api.RepoMigrationRequest{
		RepoURL: newRepo, Actor: "sre", Reason: "cutover to tenant repo", AllowRestart: true,
	}, "")
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	migrated, _ := reg.Get(app.ID)
	if !appliedPlan.Applied || migrated.ID != origApp.ID || migrated.RepoURL != newRepo || migrated.ComposeProject() != origProject || migrated.LastSyncedCommit != newSHA {
		t.Fatalf("unexpected migrated state: %+v (origProject=%s gotProject=%s)", migrated, origProject, migrated.ComposeProject())
	}

	// 6. Rollback by restoring previous repo_url -> preserves same App.ID, ComposeProject, and data volumes, and records audit.
	rolledPlan, err := rec.MigrateRepo(context.Background(), migrated, api.RepoMigrationRequest{
		RepoURL: oldRepo, Actor: "sre", Reason: "rollback to previous repo_url", AllowRestart: true,
	}, "")
	if err != nil || !rolledPlan.Applied {
		t.Fatalf("rollback migration failed: %v plan=%+v", err, rolledPlan)
	}
	rolledBack, _ := reg.Get(app.ID)
	if rolledBack.RepoURL != oldRepo || rolledBack.ComposeProject() != origProject || rolledBack.LastSyncedCommit != oldSHA {
		t.Fatalf("unexpected rollback state: %+v", rolledBack)
	}

	audits, err := reg.ListAudit(app.ID, 20)
	if err != nil || len(audits) < 5 {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(audits)
		t.Fatalf("expected all migration attempts in audit log, got %d: %s", len(audits), buf.String())
	}
}

func TestZeroDowntimeSourceMigrationNoRestartAndTargetedRestartAndMetrics(t *testing.T) {
	reg := testRegistry(t)
	const (
		monoRepo   = "https://git.example/tenko/dev-monorepo.git"
		tenantRepo = "https://git.example/tenko/dev-tenant-1.git"
		oldSHA     = "1010101010101010101010101010101010101010"
		sameCfgSHA = "2020202020202020202020202020202020202020"
		newImgSHA  = "3030303030303030303030303030303030303030"
	)

	envFile := filepath.Join(t.TempDir(), "tenant.env")
	if err := os.WriteFile(envFile, []byte("TENANT_CODE=dev1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	app := &api.App{
		Name:             "tenko-dev-1",
		RepoURL:          monoRepo,
		Branch:           "main",
		ComposePath:      "tenants/dev-1/compose.yaml",
		PollInterval:     "30s",
		LastSeenCommit:   oldSHA,
		LastSyncedCommit: oldSHA,
		Status:           "synced",
	}
	addPublicApp(t, reg, app)
	_ = reg.UpdateStatus(app.ID, "synced", nil)
	pol := deploypolicy.Policy{
		RequirePipelineSuccess: true,
		Project:                "tenko-dev-1",
		EnvFiles:               []string{envFile},
		Vars:                   map[string]string{"API_TAG": "1.0.0"},
		PublicVars:             []string{"API_TAG"},
	}
	_ = reg.UpdateDeployPolicy(app.ID, deploypolicy.Encode(pol))
	_ = reg.UpdateRollback(app.ID, oldSHA, "", deploypolicy.EncodeVars(pol.Vars), "")

	origApp, _ := reg.Get(app.ID)
	origID := origApp.ID
	origProject := origApp.ComposeProject()

	var logBuf bytes.Buffer
	rt := &fakeMigRuntime{
		equivalent:        true,
		reason:            "config_identical",
		unchangedServices: []string{"api", "postgres"},
		keptVols:          []string{"tenko-dev-1_pgdata"},
	}
	rec := NewReconciler(reg, rt, slog.New(slog.NewTextHandler(&logBuf, nil)), ReconcilerConfig{MaxConcurrent: 2})
	currentTargetSHA := sameCfgSHA
	rec.ResolveMigrationCommit = func(repoURL, _ string, _ []byte, _, _ string) (string, error) {
		if repoURL == "https://git.example/tenko/broken.git" {
			return "", fmt.Errorf("repository not found")
		}
		return currentTargetSHA, nil
	}
	rec.Gate = &mapGate{byRepo: map[string]func(cigate.CheckRequest) cigate.Result{
		tenantRepo: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success", Reason: cigate.ReasonOK, PipelineID: 801, Source: "push"}
		},
		monoRepo: func(cigate.CheckRequest) cigate.Result {
			return cigate.Result{Allowed: true, Outcome: cigate.OutcomeAllow, Status: "success", Reason: cigate.ReasonOK, PipelineID: 802, Source: "push"}
		},
	}}

	beforeNoRestart := testutil.ToFloat64(metrics.RepoMigrationsTotal.WithLabelValues(app.Name, "no_restart", "config_identical"))
	beforeFailed := testutil.ToFloat64(metrics.RepoMigrationsTotal.WithLabelValues(app.Name, "failed", "source_unreachable"))

	// 1. Unreachable new source -> leaves old source, branch, compose_path, and running containers untouched!
	if _, err := rec.MigrateRepo(context.Background(), origApp, api.RepoMigrationRequest{
		RepoURL:     "https://git.example/tenko/broken.git",
		Branch:      "release",
		ComposePath: "compose.yaml",
		Actor:       "fleet",
		FromFleet:   true,
	}, deploypolicy.Encode(pol)); err == nil {
		t.Fatal("expected unreachable repo to fail migration")
	}
	unchangedApp, _ := reg.Get(app.ID)
	if unchangedApp.RepoURL != monoRepo || unchangedApp.Branch != "main" || unchangedApp.ComposePath != "tenants/dev-1/compose.yaml" {
		t.Fatalf("old source mutated on failed migration: %+v", unchangedApp)
	}
	if rt.downCalls != 0 || len(rt.applyCalls) != 0 {
		t.Fatalf("containers touched on failed migration: downCalls=%d applyCalls=%v", rt.downCalls, rt.applyCalls)
	}
	if got := testutil.ToFloat64(metrics.RepoMigrationsTotal.WithLabelValues(app.Name, "failed", "source_unreachable")); got != beforeFailed+1 {
		t.Fatalf("expected failed/source_unreachable metric increment, got %v", got)
	}

	// 2. Identical rendered compose across repo_url + branch + compose_path change ->
	//    adopted WITHOUT restarting containers (applyCalls == 0!), preserving ID, project, volumes, env_files.
	plan, err := rec.MigrateRepo(context.Background(), unchangedApp, api.RepoMigrationRequest{
		RepoURL:     tenantRepo,
		Branch:      "release",
		ComposePath: "compose.yaml",
		Actor:       "fleet",
		Reason:      "fleet repo_url updated",
		FromFleet:   true,
	}, deploypolicy.Encode(pol))
	if err != nil {
		t.Fatalf("no-restart migration failed: %v", err)
	}
	if !plan.Applied || plan.Mode != compose.MigrationModeNoRestart || plan.Reason != "config_identical" || len(plan.ToStop) != 0 {
		t.Fatalf("unexpected no-restart plan: %+v", plan)
	}
	if len(rt.applyCalls) != 0 {
		t.Fatalf("expected ZERO compose apply calls on identical config, got %d (%v)", len(rt.applyCalls), rt.applyCalls)
	}
	adopted, _ := reg.Get(app.ID)
	if adopted.ID != origID || adopted.ComposeProject() != origProject ||
		adopted.RepoURL != tenantRepo || adopted.Branch != "release" || adopted.ComposePath != "compose.yaml" ||
		adopted.LastSyncedCommit != sameCfgSHA || adopted.LastSeenCommit != sameCfgSHA || adopted.Status != "synced" {
		t.Fatalf("unexpected adopted app state: %+v", adopted)
	}
	if !slices.Equal(adopted.Policy().EnvFiles, []string{envFile}) {
		t.Fatalf("env_files changed unexpectedly: %v", adopted.Policy().EnvFiles)
	}
	if got := testutil.ToFloat64(metrics.RepoMigrationsTotal.WithLabelValues(app.Name, "no_restart", "config_identical")); got != beforeNoRestart+1 {
		t.Fatalf("expected no_restart/config_identical metric increment, got %v", got)
	}
	if !strings.Contains(logBuf.String(), "mode=no_restart") || !strings.Contains(logBuf.String(), "reason=config_identical") {
		t.Fatalf("expected structured no_restart log with reason, got:\n%s", logBuf.String())
	}

	// Subsequent normal reconciliation must see desired state already settled and NOT trigger a redundant apply!
	rec.reconcileOnce()
	if len(rt.applyCalls) != 0 {
		t.Fatalf("normal reconcile after no-restart migration must not re-apply unchanged commit, got %v", rt.applyCalls)
	}

	// 3. Migrate compose_path/branch with a changed service image -> runs compose up (applyCalls == 1) and records restarted/image_changed.
	beforeRestarted := testutil.ToFloat64(metrics.RepoMigrationsTotal.WithLabelValues(app.Name, "restarted", "image_changed"))
	currentTargetSHA = newImgSHA
	rt.equivalent = false
	rt.reason = "image_changed"
	rt.changedServices = []string{"api"}
	rt.unchangedServices = []string{"postgres"}
	rt.stopped = []string{"api (tenko-dev-1-api-1)"}

	plan2, err := rec.MigrateRepo(context.Background(), adopted, api.RepoMigrationRequest{
		RepoURL:     tenantRepo,
		Branch:      "main",
		ComposePath: "deploy/compose.yaml",
		Actor:       "fleet",
		Reason:      "fleet branch updated",
		FromFleet:   true,
	}, deploypolicy.Encode(pol))
	if err != nil {
		t.Fatalf("targeted restart migration failed: %v", err)
	}
	if !plan2.Applied || plan2.Mode != compose.MigrationModeRestarted || plan2.Reason != "image_changed" || !slices.Equal(plan2.ChangedServices, []string{"api"}) {
		t.Fatalf("unexpected restarted plan: %+v", plan2)
	}
	if len(rt.applyCalls) != 1 {
		t.Fatalf("expected exactly 1 compose up apply call, got %d", len(rt.applyCalls))
	}
	if got := testutil.ToFloat64(metrics.RepoMigrationsTotal.WithLabelValues(app.Name, "restarted", "image_changed")); got != beforeRestarted+1 {
		t.Fatalf("expected restarted/image_changed metric increment, got %v", got)
	}
	if !strings.Contains(logBuf.String(), "mode=restarted") || !strings.Contains(logBuf.String(), "reason=image_changed") {
		t.Fatalf("expected structured restarted log with reason, got:\n%s", logBuf.String())
	}
}
