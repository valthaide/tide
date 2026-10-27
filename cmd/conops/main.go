package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/valthaide/conops-private/internal/agent"
	"github.com/valthaide/conops-private/internal/apiauth"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/controller"
	"github.com/valthaide/conops-private/internal/credentials"
	"github.com/valthaide/conops-private/internal/fleet"
	"github.com/valthaide/conops-private/internal/fsperm"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/redact"
	"github.com/valthaide/conops-private/internal/secrets"
	"github.com/valthaide/conops-private/internal/store"
	"github.com/valthaide/conops-private/internal/tlsutil"
)

// Version is set by build ldflags when releasing the controller image.
var Version = "dev"

func main() {
	redact.Default.ReloadFromEnv()
	logger := slog.New(redact.WrapSlog(slog.NewJSONHandler(os.Stdout, nil), redact.Default))
	mode := agent.ResolveMode()
	logger.Info("Starting ConOps", "version", Version, "mode", mode)

	dataDir := "/data"
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		dataDir = "."
	}
	_ = fsperm.EnsurePrivateDir(dataDir)

	secretsCfg := secrets.LoadConfigFromEnv()
	for _, w := range secretsCfg.Warnings() {
		logger.Warn(w)
	}

	var dbStore store.Store
	var err error

	dbType := os.Getenv("DB_TYPE")
	if dbType == "postgres" {
		connString := os.Getenv("DB_CONNECTION_STRING")
		if connString == "" {
			logger.Error("DB_CONNECTION_STRING is required for postgres")
			os.Exit(1)
		}
		dbStore, err = store.NewPostgresStore(context.Background(), connString)
		if err != nil {
			logger.Error("Failed to initialize postgres store", "error", err)
			os.Exit(1)
		}
		logger.Info("Using PostgreSQL store")
	} else {
		dbPath := filepath.Join(dataDir, "conops.db")
		dbStore, err = store.NewSQLiteStore(dbPath)
		if err != nil {
			logger.Error("Failed to initialize sqlite store", "error", err)
			os.Exit(1)
		}
		_ = fsperm.EnsurePrivateFile(dbPath)
		logger.Info("Using SQLite store")
	}
	defer dbStore.Close()

	keyPath := filepath.Join(dataDir, "conops-encryption.key")
	credentialService, err := credentials.NewServiceFromEnv(keyPath)
	if err != nil {
		logger.Error("Failed to initialize credential encryption", "error", err)
		os.Exit(1)
	}
	_ = fsperm.EnsurePrivateFile(keyPath)
	logger.Info("Credential encryption is enabled", "source", credentialService.KeySource())

	auth, err := apiauth.Setup(dataDir)
	if err != nil {
		logger.Error("Failed to initialize API auth", "error", err)
		os.Exit(1)
	}
	logger.Info("API bearer auth enabled")

	certFile, keyFile, err := tlsutil.EnsureServerCert(dataDir)
	if err != nil {
		logger.Error("Failed to ensure TLS certificate", "error", err)
		os.Exit(1)
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
	if runtimeDir := strings.TrimSpace(os.Getenv("CONOPS_RUNTIME_DIR")); runtimeDir != "" {
		executor.WorkDir = runtimeDir
	}
	if toolsDir := strings.TrimSpace(os.Getenv("CONOPS_TOOLS_DIR")); toolsDir != "" {
		executor.ToolsDir = toolsDir
	} else if dataDir != "." {
		executor.ToolsDir = filepath.Join(dataDir, "conops-tools")
	}
	_ = fsperm.EnsureSharedDir(executor.WorkDir)
	if err := fsperm.MigrateRuntimePermissions(executor.WorkDir); err != nil {
		logger.Warn("Failed to migrate runtime permissions", "dir", executor.WorkDir, "error", err)
	}
	logger.Info("Runtime workspace configured", "dir", executor.WorkDir, "tools_dir", executor.ToolsDir)

	var reconciler *controller.Reconciler
	var reconcilerCfg controller.ReconcilerConfig
	if mode != agent.ModeHub {
		watcher := controller.NewGitWatcher(registry, logger)
		go watcher.Start(ctx)

		var loadErr error
		reconcilerCfg, loadErr = controller.LoadReconcilerConfigFromEnv()
		if loadErr != nil {
			logger.Error("Failed to load reconciler config", "error", loadErr)
			os.Exit(1)
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
			os.Exit(1)
		}
		if fleetCfg.Enabled() {
			logger.Info("Fleet reconciler enabled", "repo", fleetCfg.RepoURL, "host", fleetCfg.Host, "interval", fleetCfg.Interval.String())
			fleetReconciler := fleet.NewReconciler(fleetCfg, registry, executor, logger)
			fleetReconciler.OnHost = func(doc *fleet.HostDocument) {
				if doc != nil && doc.ReconcileConcurrency > 0 {
					reconciler.SetLimit(doc.ReconcileConcurrency)
				}
			}
			go fleetReconciler.Run(ctx)
		} else {
			logger.Info("Fleet reconciler disabled (set CONOPS_FLEET_REPO_URL to enable)")
		}
	} else {
		logger.Info("Hub mode: local git/fleet reconcile disabled")
	}

	if mode == agent.ModeAgent {
		if rep := agent.NewReporterFromEnv(registry, Version, logger); rep != nil {
			logger.Info("Agent reporter enabled", "hub", rep.HubURL, "host", rep.Host)
			go rep.Run(ctx)
		} else {
			logger.Warn("CONOPS_MODE=agent but CONOPS_HUB_URL is empty")
		}
	}

	promRegistry := prometheus.NewRegistry()
	if err := metrics.Register(promRegistry, registry); err != nil {
		logger.Error("Failed to register metrics", "error", err)
		os.Exit(1)
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
	if fleetRepo := strings.TrimSpace(os.Getenv("CONOPS_FLEET_REPO_URL")); fleetRepo != "" {
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
			_, _ = w.Write([]byte("ConOps controller\nAPI: /api/v1/apps\nHealth: /healthz\nUse CLI: cps\n"))
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
					r.Get("/{id}/containers", appHandler.ListAppContainers)
					r.Patch("/{id}", appHandler.UpdateApp)
					r.Post("/{id}/sync", appHandler.ForceSyncApp)
					r.Post("/{id}/reconcile", appHandler.ReconcileApp)
					r.Post("/{id}/suspend", appHandler.SuspendApp)
					r.Post("/{id}/resume", appHandler.ResumeApp)
					r.Delete("/{id}", appHandler.DeleteApp)
				})
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

	apiAddr := strings.TrimSpace(os.Getenv("CONOPS_API_ADDR"))
	if apiAddr == "" {
		apiAddr = ":8443"
	}
	metricsAddr := strings.TrimSpace(os.Getenv("CONOPS_METRICS_ADDR"))
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
		os.Exit(1)
	}
}
