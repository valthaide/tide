package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/valthaide/conops-private/internal/compose"
	"github.com/valthaide/conops-private/internal/controller"
	"github.com/valthaide/conops-private/internal/credentials"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/store"
)

// Version is set by build ldflags when releasing the controller image.
var Version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dataDir := "/data"
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		dataDir = "."
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
		logger.Info("Using SQLite store")
	}
	defer dbStore.Close()

	credentialService, err := credentials.NewServiceFromEnv(filepath.Join(dataDir, "conops-encryption.key"))
	if err != nil {
		logger.Error("Failed to initialize credential encryption", "error", err)
		os.Exit(1)
	}
	logger.Info("Credential encryption is enabled", "source", credentialService.KeySource())

	registry := controller.NewRegistry(dbStore, credentialService)

	watcher := controller.NewGitWatcher(registry, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Start(ctx)

	reconcilerCfg, err := controller.LoadReconcilerConfigFromEnv()
	if err != nil {
		logger.Error("Failed to load reconciler config", "error", err)
		os.Exit(1)
	}
	executor := compose.NewComposeExecutor(logger)
	if runtimeDir := strings.TrimSpace(os.Getenv("CONOPS_RUNTIME_DIR")); runtimeDir != "" {
		executor.WorkDir = runtimeDir
	}
	if toolsDir := strings.TrimSpace(os.Getenv("CONOPS_TOOLS_DIR")); toolsDir != "" {
		executor.ToolsDir = toolsDir
	} else if dataDir != "." {
		executor.ToolsDir = filepath.Join(dataDir, "conops-tools")
	}
	logger.Info("Runtime workspace configured", "dir", executor.WorkDir, "tools_dir", executor.ToolsDir)
	reconciler := controller.NewReconciler(registry, executor, logger, reconcilerCfg)
	go reconciler.Run(ctx)

	promRegistry := prometheus.NewRegistry()
	if err := metrics.Register(promRegistry, registry); err != nil {
		logger.Error("Failed to register metrics", "error", err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	appHandler := controller.NewHandler(registry, executor, executor, executor, logger, Version)

	r.Handle("/metrics", metrics.Handler(promRegistry))
	r.Get("/healthz", appHandler.Healthz)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ConOps controller\nAPI: /api/v1/apps\nHealth: /healthz\nMetrics: /metrics\nUse CLI: cps\n"))
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/apps", func(r chi.Router) {
			r.Post("/", appHandler.RegisterApp)
			r.Get("/", appHandler.ListApps)
			r.Get("/{id}", appHandler.GetApp)
			r.Get("/{id}/containers", appHandler.ListAppContainers)
			r.Patch("/{id}", appHandler.UpdateApp)
			r.Post("/{id}/sync", appHandler.ForceSyncApp)
			r.Delete("/{id}", appHandler.DeleteApp)
		})
	})

	addr := ":8080"
	logger.Info("Starting controller", "addr", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		logger.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
