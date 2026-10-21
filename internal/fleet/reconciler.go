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
	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/repoauth"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// AppManager applies fleet desired state to the local app registry.
type AppManager interface {
	List() []*api.App
	FindByName(name string) *api.App
	AddWithDeployKeyAndEnvs(app *api.App, secret string, serviceEnvs map[string]string) error
	UpdateApp(id, name, branch, composePath, pollInterval string, serviceEnvs map[string]string) error
	UpdateRepoSecret(id, secret string) error
	ClearRepoSecret(id string) error
	GetRepoSecret(id string) ([]byte, error)
	SetSuspended(id string, suspended bool) error
	Delete(id string) error
}

// RuntimeCleaner destroys compose runtime for a deleted app (best-effort).
type RuntimeCleaner interface {
	Destroy(ctx context.Context, appID, composePath string, envVars map[string]string) (string, error)
}

// Reconciler syncs the local registry from a fleet git repository.
type Reconciler struct {
	Config  Config
	Apps    AppManager
	Cleaner RuntimeCleaner
	Logger  *slog.Logger
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

	ticker := time.NewTicker(r.Config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.Logger != nil {
				r.Logger.Info("Fleet reconciler stopped")
			}
			return
		case <-ticker.C:
			r.syncOnce(ctx)
		}
	}
}

func (r *Reconciler) syncOnce(ctx context.Context) {
	doc, err := r.loadDesired(ctx)
	if err != nil {
		se := syncerr.Classify(err)
		metrics.ObserveFleetError(se.GetCode())
		if r.Logger != nil {
			r.Logger.Error("Fleet sync failed", "code", se.GetCode(), "error", err)
		}
		return
	}
	if err := r.applyDesired(ctx, doc); err != nil {
		se := syncerr.Classify(err)
		metrics.ObserveFleetError(se.GetCode())
		if r.Logger != nil {
			r.Logger.Error("Fleet apply failed", "code", se.GetCode(), "error", err)
		}
		return
	}
	metrics.ObserveFleetSuccess()
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
		if err := r.upsertApp(spec); err != nil {
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
			_, _ = r.Cleaner.Destroy(ctx, app.ID, app.ComposePath, nil)
		}
		if err := r.Apps.Delete(app.ID); err != nil {
			return fmt.Errorf("delete app %q: %w", app.Name, err)
		}
	}
	return nil
}

func (r *Reconciler) upsertApp(spec AppSpec) error {
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
			ComposePath:    spec.ComposePath,
			PollInterval:   spec.PollInterval,
			Suspended:      spec.Suspended,
		}
		if err := r.Apps.AddWithDeployKeyAndEnvs(app, secret, nil); err != nil {
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

	// Repo URL / auth method change → recreate
	if !strings.EqualFold(existing.RepoURL, spec.RepoURL) ||
		repoauth.NormalizeMethod(existing.RepoAuthMethod) != method {
		if r.Cleaner != nil {
			_, _ = r.Cleaner.Destroy(context.Background(), existing.ID, existing.ComposePath, nil)
		}
		if err := r.Apps.Delete(existing.ID); err != nil {
			return err
		}
		app := &api.App{
			Name:           spec.Name,
			RepoURL:        spec.RepoURL,
			RepoAuthMethod: method,
			Branch:         spec.Branch,
			ComposePath:    spec.ComposePath,
			PollInterval:   spec.PollInterval,
			Suspended:      spec.Suspended,
		}
		if err := r.Apps.AddWithDeployKeyAndEnvs(app, secret, nil); err != nil {
			return err
		}
		if spec.Suspended {
			_ = r.Apps.SetSuspended(app.ID, true)
		}
		if r.Logger != nil {
			r.Logger.Info("Fleet recreated app", "name", spec.Name, "id", app.ID)
		}
		return nil
	}

	if err := r.Apps.UpdateApp(existing.ID, spec.Name, spec.Branch, spec.ComposePath, spec.PollInterval, nil); err != nil {
		return err
	}
	if err := r.syncFleetSecret(existing.ID, method, secret, spec); err != nil {
		return err
	}
	if existing.Suspended != spec.Suspended {
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
			// Prefer CONOPS_GIT_TOKEN from process env; do not keep a DB copy.
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
			// Empty secret → ResolveGitToken("") reads CONOPS_GIT_TOKEN on each use.
			if t := repoauth.ResolveGitToken(""); t == "" {
				return "", fmt.Errorf("CONOPS_GIT_TOKEN is empty")
			}
			return "", nil
		}
		return "", fmt.Errorf("deploy_key_file is required for deploy_key auth")
	default:
		return "", nil
	}
}
