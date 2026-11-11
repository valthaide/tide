package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/valthaide/tide/internal/agent"
	"github.com/valthaide/tide/internal/apiauth"
	"github.com/valthaide/tide/internal/compose"
	"github.com/valthaide/tide/internal/controller"
	"github.com/valthaide/tide/internal/credentials"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/fleet"
	"github.com/valthaide/tide/internal/fsperm"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/procguard"
	"github.com/valthaide/tide/internal/redact"
	"github.com/valthaide/tide/internal/secrets"
	"github.com/valthaide/tide/internal/store"
	"github.com/valthaide/tide/internal/tlsutil"
)

// Version is set by build ldflags when releasing the controller image.
var Version = "dev"

func main() {
	os.Exit(runController(os.Args[1:], os.Stdout, os.Stderr))
}

func runController(args []string, stdout, stderr io.Writer) int {
	// Offline subcommands (schema/validate) run before any server setup:
	// no lock, DB, Docker, or network.
	if code, handled := runOfflineCommand(args, Version, stdout, stderr); handled {
		return code
	}
	opts := parseServerArgs(args, Version, stdout, stderr)
	if opts.Exit {
		return opts.Code
	}

	redact.Default.ReloadFromEnv()
	logger := slog.New(redact.WrapSlog(slog.NewJSONHandler(stdout, nil), redact.Default))
	envcompat.SetLogger(logger)

	mode := agent.ResolveMode()
	logger.Info("Starting Tide", "version", Version, "mode", mode)

	dataDir := envcompat.ResolveDataDir(opts.DataDir)
	_ = fsperm.EnsurePrivateDir(dataDir)

	// Acquire inter-process lock BEFORE migrating files, opening the database, or resetting syncing apps.
	envLock, lockErr := procguard.Acquire(dataDir)
	if lockErr != nil {
		logger.Error("Refusing to start: another Tide controller owns this environment", "data_dir", dataDir, "error", lockErr)
		if stderr != nil && stderr != stdout {
			_, _ = io.WriteString(stderr, lockErr.Error()+"\n")
		}
		return 1
	}
	defer envLock.Release()

	if migErr := envcompat.MigrateDataFiles(dataDir); migErr != nil {
		logger.Error("Failed to migrate legacy data files", "data_dir", dataDir, "error", migErr)
		return 1
	}

	secretsCfg := secrets.LoadConfigFromEnv()
	for _, w := range secretsCfg.Warnings() {
		logger.Warn(w)
	}

	var dbStore store.Store
	var err error

	dbType := strings.TrimSpace(os.Getenv("DB_TYPE"))
	if dbType == "postgres" {
		connString := strings.TrimSpace(os.Getenv("DB_CONNECTION_STRING"))
		if connString == "" {
			logger.Error("DB_CONNECTION_STRING is required for postgres")
			return 1
		}
		dbStore, err = store.NewPostgresStore(context.Background(), connString)
		if err != nil {
			logger.Error("Failed to initialize postgres store", "error", err)
			return 1
		}
		logger.Info("Using PostgreSQL store")
	} else {
		dbPath := filepath.Join(dataDir, envcompat.NewDBName)
		dbStore, err = store.NewSQLiteStore(dbPath)
		if err != nil {
			logger.Error("Failed to initialize sqlite store", "error", err)
			return 1
		}
		_ = fsperm.EnsurePrivateFile(dbPath)
		logger.Info("Using SQLite store", "path", dbPath)
	}
	defer dbStore.Close()

	keyPath := filepath.Join(dataDir, envcompat.NewKeyFileName)
	credentialService, err := credentials.NewServiceFromEnv(keyPath)
	if err != nil {
		logger.Error("Failed to initialize credential encryption", "error", err)
		return 1
	}
	_ = fsperm.EnsurePrivateFile(keyPath)
	logger.Info("Credential encryption is enabled", "source", credentialService.KeySource())

	auth, err := apiauth.Setup(dataDir)
	if err != nil {
		logger.Error("Failed to initialize API auth", "error", err)
		return 1
	}
	logger.Info("API bearer auth enabled")

	certFile, keyFile, err := tlsutil.EnsureServerCert(dataDir)
	if err != nil {
		logger.Error("Failed to ensure TLS certificate", "error", err)
		return 1
	}

	registry := controller.NewRegistry(dbStore, credentialService)
	if n := registry.ResetStuckSyncing(); n > 0 {
		logger.Info("Reset stuck syncing apps to pending", "count", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	secretsMgr := secrets.NewManager(secretsCfg)
	if secretsCfg.VaultEnabled {
		logger.Info("Vault secrets enabled", "addr", secretsCfg.VaultAddr)
	}

	executor := compose.NewComposeExecutor(logger)
	executor.Secrets = secretsMgr
	if runtimeDir := envcompat.Getenv("TIDE_RUNTIME_DIR"); runtimeDir != "" {
		executor.WorkDir = runtimeDir
	}
	_ = fsperm.EnsureSharedDir(executor.WorkDir)
	if err := fsperm.MigrateRuntimePermissions(executor.WorkDir); err != nil {
		logger.Warn("Failed to migrate runtime permissions", "dir", executor.WorkDir, "error", err)
	}
	logger.Info("Runtime workspace configured", "dir", executor.WorkDir)

	var reconciler *controller.Reconciler
	var reconcilerCfg controller.ReconcilerConfig
	if mode != agent.ModeHub {
		watcher := controller.NewGitWatcher(registry, logger)
		go watcher.Start(ctx)

		var loadErr error
		reconcilerCfg, loadErr = controller.LoadReconcilerConfigFromEnv()
		if loadErr != nil {
			logger.Error("Failed to load reconciler config", "error", loadErr)
			return 1
		}
		logger.Info("Reconciler config", "max_concurrent", reconcilerCfg.MaxConcurrent, "pull_parallel", reconcilerCfg.PullParallel, "interval", reconcilerCfg.Interval.String(), "sync_timeout", reconcilerCfg.SyncTimeout.String())
		reconciler = controller.NewReconciler(registry, executor, logger, reconcilerCfg)
		if reconcilerCfg.PullParallel > 0 {
			reconciler.EnablePullLimit(reconcilerCfg.PullParallel)
			executor.PullGate = reconciler
		}
		go reconciler.Run(ctx)

		fleetCfg, fleetErr := fleet.LoadConfigFromEnv(dataDir)
		if fleetErr != nil {
			logger.Error("Failed to load fleet config", "error", fleetErr)
			return 1
		}
		if fleetCfg.Enabled() {
			logger.Info("Fleet reconciler enabled", "repo", fleetCfg.RepoURL, "host", fleetCfg.Host, "interval", fleetCfg.Interval.String())
			// Template-sourced apps read templates/<name>/compose.yaml from the
			// fleet checkout the fleet reconciler keeps in fleet-cache/repo.
			executor.FleetCheckoutDir = filepath.Join(fleetCfg.CacheDir, "repo")
			fleetReconciler := fleet.NewReconciler(fleetCfg, registry, executor, logger)
			fleetReconciler.RepoMigrator = reconciler
			fleetReconciler.OnHost = func(doc *fleet.HostDocument) {
				if doc != nil && doc.ReconcileConcurrency > 0 {
					reconciler.SetLimit(doc.ReconcileConcurrency)
				}
			}
			go fleetReconciler.Run(ctx)
		} else {
			logger.Info("Fleet reconciler disabled (set TIDE_FLEET_REPO_URL to enable)")
		}
	} else {
		logger.Info("Hub mode: local git/fleet reconcile disabled")
	}

	if mode == agent.ModeAgent {
		if rep := agent.NewReporterFromEnv(registry, Version, logger); rep != nil {
			logger.Info("Agent reporter enabled", "hub", rep.HubURL, "host", rep.Host)
			go rep.Run(ctx)
		} else {
			logger.Warn("TIDE_MODE=agent but TIDE_HUB_URL is empty")
		}
	}

	promRegistry := prometheus.NewRegistry()
	if err := metrics.Register(promRegistry, registry); err != nil {
		logger.Error("Failed to register metrics", "error", err)
		return 1
	}
	metrics.SetBuildInfo(Version)
	var gitRepos []string
	var appNames []string
	for _, app := range registry.List() {
		if app != nil {
			gitRepos = append(gitRepos, app.RepoURL)
			appNames = append(appNames, app.Name)
		}
	}
	if fleetRepo := envcompat.Getenv("TIDE_FLEET_REPO_URL"); fleetRepo != "" {
		gitRepos = append(gitRepos, fleetRepo)
	}
	metrics.EnsureGitFetchErrors(gitRepos...)
	metrics.EnsureAppReloads(appNames...)

	appHandler := controller.NewHandler(registry, executor, executor, executor, logger, Version)
	appHandler.SyncTimeout = reconcilerCfg.SyncTimeout
	if reconciler != nil {
		appHandler.SyncGate = reconciler
	}

	webhookHandler := controller.NewWebhookHandlerFromEnv(registry, logger)
	var hubHandler *agent.Handler
	if mode == agent.ModeHub {
		hubHandler = &agent.Handler{Store: agent.NewHubStore(), Logger: logger}
	}

	apiRouter := chi.NewRouter()
	apiRouter.Use(middleware.Logger)
	apiRouter.Use(middleware.Recoverer)

	// Webhooks use their own secret (not API bearer).
	if webhookHandler != nil {
		logger.Info("Git webhooks enabled at POST /api/v1/hooks/git")
		apiRouter.Post("/api/v1/hooks/git", webhookHandler.GitPush)
	}

	apiRouter.Group(func(r chi.Router) {
		r.Use(auth.Middleware)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("Tide controller\nAPI: /api/v1/apps\nHealth: /healthz\nUse CLI: tidectl\n"))
		})
		r.Get("/healthz", appHandler.Healthz)
		r.Route("/api/v1", func(r chi.Router) {
			if hubHandler != nil {
				r.Post("/agents/heartbeat", hubHandler.Heartbeat)
				r.Get("/agents", hubHandler.ListAgents)
			}
			if mode != agent.ModeHub {
				r.Route("/apps", func(r chi.Router) {
					r.Post("/", appHandler.RegisterApp)
					r.Get("/", appHandler.ListApps)
					r.Get("/{id}", appHandler.GetApp)
					r.Get("/{id}/history", appHandler.ListHistory)
					r.Get("/{id}/audit", appHandler.ListAppAudit)
					r.Get("/{id}/containers", appHandler.ListAppContainers)
					r.Patch("/{id}", appHandler.UpdateApp)
					r.Post("/{id}/migrate-repo", appHandler.MigrateAppRepo)
					r.Post("/{id}/sync", appHandler.ForceSyncApp)
					r.Post("/{id}/reconcile", appHandler.ReconcileApp)
					r.Post("/{id}/retry", appHandler.RetryApp)
					r.Post("/{id}/suspend", appHandler.SuspendApp)
					r.Post("/{id}/resume", appHandler.ResumeApp)
					r.Delete("/{id}", appHandler.DeleteApp)
				})
				r.Get("/audit", appHandler.ListAudit)
				r.Post("/gc", appHandler.ImageGC)
				r.Get("/queue", appHandler.GetQueue)
				r.Get("/operations/{id}", appHandler.GetOperation)
			}
		})
	})

	metricsRouter := chi.NewRouter()
	metricsRouter.Use(middleware.Recoverer)
	metricsRouter.Handle("/metrics", metrics.Protect(metrics.MetricsToken(), metrics.Handler(promRegistry)))
	metricsRouter.Get("/healthz", appHandler.Healthz)

	apiAddr := envcompat.Getenv("TIDE_API_ADDR")
	if apiAddr == "" {
		apiAddr = ":8443"
	}
	metricsAddr := envcompat.Getenv("TIDE_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = "127.0.0.1:8081"
	}

	errCh := make(chan error, 2)

	go func() {
		logger.Info("Starting metrics/health listener", "addr", metricsAddr)
		srv := &http.Server{Addr: metricsAddr, Handler: metricsRouter, ReadHeaderTimeout: 10 * time.Second}
		errCh <- srv.ListenAndServe()
	}()

	go func() {
		logger.Info("Starting HTTPS API listener", "addr", apiAddr, "cert", certFile)
		srv := &http.Server{Addr: apiAddr, Handler: apiRouter, ReadHeaderTimeout: 10 * time.Second}
		errCh <- srv.ListenAndServeTLS(certFile, keyFile)
	}()

	if err := <-errCh; err != nil && err != http.ErrServerClosed {
		logger.Error("Server failed", "error", err)
		return 1
	}
	return 0
}
