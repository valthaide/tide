package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/valthaide/tide/internal/envcompat"
	"github.com/valthaide/tide/internal/gitremote"
	"github.com/valthaide/tide/internal/gitrev"
	"github.com/valthaide/tide/internal/metrics"
	"github.com/valthaide/tide/internal/pollinterval"
	"github.com/valthaide/tide/internal/repoauth"
)

// GitWatcher monitors registered apps for changes.
type GitWatcher struct {
	Registry *Registry
	Logger   *slog.Logger
	CacheDir string
}

// NewGitWatcher creates a new Git watcher.
func NewGitWatcher(registry *Registry, logger *slog.Logger) *GitWatcher {
	return &GitWatcher{
		Registry: registry,
		Logger:   logger,
		CacheDir: envcompat.ResolveGitCacheDir(),
	}
}

// Start begins the polling loop.
func (w *GitWatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second) // Check registry for new apps every 10s
	defer ticker.Stop()

	// Track running pollers. A revision, branch, or interval change restarts the poller
	// so the next check happens immediately instead of waiting out the old timer.
	type runningPoller struct {
		cancel context.CancelFunc
		spec   string
	}
	pollers := make(map[string]runningPoller)
	syncPollers := func() {
		apps := w.Registry.List()
		w.Logger.Debug("Registry poll tick", "app_count", len(apps))
		activeIDs := make(map[string]bool)

		for _, app := range apps {
			if app.Suspended {
				if running, ok := pollers[app.ID]; ok {
					running.cancel()
					delete(pollers, app.ID)
					w.Logger.Info("Stopped app poller (suspended)", "id", app.ID)
				}
				continue
			}
			activeIDs[app.ID] = true
			spec := appPollSpec(app)
			if running, ok := pollers[app.ID]; ok && running.spec == spec {
				continue
			}
			if running, ok := pollers[app.ID]; ok {
				running.cancel()
			}
			pollCtx, cancel := context.WithCancel(ctx)
			pollers[app.ID] = runningPoller{cancel: cancel, spec: spec}
			w.Logger.Info("Starting app poller", "id", app.ID, "interval", app.PollInterval, "revision", app.Revision, "branch", app.Branch)
			go w.pollApp(pollCtx, app)
		}

		for id, running := range pollers {
			if !activeIDs[id] {
				running.cancel()
				delete(pollers, id)
				w.Logger.Info("Stopped app poller", "id", id)
			}
		}
	}

	// Prime pollers immediately instead of waiting for the first ticker interval.
	syncPollers()

	for {
		select {
		case <-ctx.Done():
			w.Logger.Info("Git watcher stopped")
			return
		case <-ticker.C:
			syncPollers()
		}
	}
}

func appPollSpec(app *App) string {
	if app == nil {
		return ""
	}
	return strings.Join([]string{app.RepoURL, app.Branch, app.Revision, app.ComposePath, app.PollInterval}, "\x00")
}

func (w *GitWatcher) pollApp(ctx context.Context, app *App) {
	interval, err := time.ParseDuration(app.PollInterval)
	if err != nil || interval <= 0 {
		w.Logger.Warn("Invalid poll interval, using default", "id", app.ID, "interval", app.PollInterval)
		interval = pollinterval.AppDefault()
	}

	w.Logger.Info("Started polling app", "id", app.ID, "repo", app.RepoURL, "revision", app.Revision)

	for {
		if err := w.checkRepo(app); err != nil {
			w.Logger.Error("Failed to check repo", "id", app.ID, "error", err)
		}
		timer := time.NewTimer(pollinterval.WithJitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// checkRepo learns the desired commit cheaply. A full fetch happens later, on apply,
// and only when this commit differs from the one already seen.
// A git error leaves the last successful revision in place.
func (w *GitWatcher) checkRepo(app *App) error {
	if fresh, err := w.Registry.Get(app.ID); err == nil && fresh != nil {
		*app = *fresh
	}
	if gitrev.IsFullSHA(app.Revision) {
		sha := strings.ToLower(strings.TrimSpace(app.Revision))
		if strings.EqualFold(app.LastSeenCommit, sha) {
			metrics.SetLastSyncTimestamp(app.Name)
			return nil
		}
		w.Logger.Info("Pinned revision", "id", app.ID, "sha", sha)
		if err := w.Registry.UpdateCommitWithMessage(app.ID, sha, ""); err != nil {
			return err
		}
		app.LastSeenCommit = sha
		metrics.SetLastSyncTimestamp(app.Name)
		return nil
	}

	auth, err := w.authForApp(app)
	if err != nil {
		metrics.ObserveGitFetch(app.RepoURL, err)
		return err
	}
	refs, err := gitremote.List(app.RepoURL, auth)
	if err != nil {
		metrics.ObserveGitFetch(app.RepoURL, err)
		return fmt.Errorf("ls-remote: %w", err)
	}
	target, err := gitrev.Resolve(app.Revision, app.Branch, refs)
	if err != nil {
		metrics.ObserveGitFetch(app.RepoURL, err)
		return err
	}
	metrics.ObserveGitFetch(app.RepoURL, nil)
	metrics.SetLastSyncTimestamp(app.Name)
	if strings.EqualFold(target.SHA, app.LastSeenCommit) {
		return nil
	}
	if target.Kind == gitrev.KindTag && app.LastSeenCommit != "" {
		w.Logger.Info("tag moved", "app", app.Name, "tag", target.Name, "from", app.LastSeenCommit, "to", target.SHA)
	} else {
		w.Logger.Info("New commit detected", "id", app.ID, "kind", target.Kind, "ref", target.Name, "commit", target.SHA)
	}
	if err := w.Registry.UpdateCommitWithMessage(app.ID, target.SHA, ""); err != nil {
		return err
	}
	app.LastSeenCommit = target.SHA
	return nil
}

func (w *GitWatcher) authForApp(app *App) (transport.AuthMethod, error) {
	method := repoauth.NormalizeMethod(app.RepoAuthMethod)
	switch method {
	case repoauth.MethodPublic, "":
		return nil, nil
	case repoauth.MethodToken:
		secret, err := w.Registry.GetRepoSecret(app.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to load git token: %w", err)
		}
		token := repoauth.ResolveGitToken(string(secret))
		zeroBytes(secret)
		if token == "" {
			return nil, fmt.Errorf("missing git token for app")
		}
		return repoauth.HTTPBasicAuth(token), nil
	case repoauth.MethodDeployKey:
		deployKey, err := w.Registry.GetRepoSecret(app.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to load deploy key: %w", err)
		}
		if len(deployKey) == 0 {
			return nil, fmt.Errorf("missing deploy key for app")
		}
		defer zeroBytes(deployKey)

		knownHostsPath, err := repoauth.ResolveKnownHostsForRepo(app.RepoURL)
		if err != nil {
			return nil, err
		}
		callback, err := repoauth.NewHostKeyCallback(knownHostsPath)
		if err != nil {
			return nil, err
		}

		auth, err := gitssh.NewPublicKeys("git", deployKey, "")
		if err != nil {
			return nil, fmt.Errorf("invalid deploy key: %w", err)
		}
		auth.HostKeyCallbackHelper = gitssh.HostKeyCallbackHelper{
			HostKeyCallback: callback,
		}
		return auth, nil
	default:
		return nil, fmt.Errorf("unsupported repo auth method %q", app.RepoAuthMethod)
	}
}
