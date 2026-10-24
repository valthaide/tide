package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/valthaide/conops-private/internal/api"
	"github.com/valthaide/conops-private/internal/credentials"
	"github.com/valthaide/conops-private/internal/metrics"
	"github.com/valthaide/conops-private/internal/redact"
	"github.com/valthaide/conops-private/internal/repoauth"
	"github.com/valthaide/conops-private/internal/store"
	"github.com/valthaide/conops-private/internal/syncerr"
)

// App is aliased to api.App for compatibility, though we should prefer api.App.
type App = api.App

// Registry manages the lifecycle of tracked applications using a backend store.
type Registry struct {
	store       store.Store
	credentials *credentials.Service
}

// NewRegistry creates a new application registry with the given store backend.
func NewRegistry(s store.Store, credentialService *credentials.Service) *Registry {
	return &Registry{
		store:       s,
		credentials: credentialService,
	}
}

// PingStore checks database connectivity.
func (r *Registry) PingStore(ctx context.Context) error {
	if r.store == nil {
		return fmt.Errorf("store is not configured")
	}
	return r.store.Ping(ctx)
}

// Add registers a new application.
func (r *Registry) Add(app *api.App) error {
	return r.AddWithDeployKey(app, "")
}

// AddWithDeployKeyAndEnvs registers a new application with optional deploy key / git token and environment variables.
func (r *Registry) AddWithDeployKeyAndEnvs(app *api.App, secret string, serviceEnvs map[string]string) error {
	if app == nil {
		return fmt.Errorf("app is required")
	}

	if strings.TrimSpace(app.ID) == "" {
		app.ID = uuid.NewString()
	}

	if normalized := repoauth.NormalizeMethod(app.RepoAuthMethod); normalized != "" {
		app.RepoAuthMethod = normalized
	} else if strings.TrimSpace(secret) != "" {
		app.RepoAuthMethod = repoauth.MethodDeployKey
	} else {
		app.RepoAuthMethod = repoauth.MethodPublic
	}

	if app.RepoAuthMethod == repoauth.MethodDeployKey {
		secret = repoauth.NormalizeDeployKey(secret)
	} else {
		secret = strings.TrimSpace(secret)
	}

	if err := repoauth.ValidateCreateInput(app.RepoURL, app.RepoAuthMethod, secret); err != nil {
		return err
	}

	hasSecret := secret != "" && (app.RepoAuthMethod == repoauth.MethodDeployKey || app.RepoAuthMethod == repoauth.MethodToken)
	hasEnvVars := len(serviceEnvs) > 0

	if (hasSecret || hasEnvVars) && (r.credentials == nil || !r.credentials.Enabled()) {
		return fmt.Errorf("encryption support is unavailable: set %s", credentials.EncryptionKeyEnv)
	}

	// Set defaults if missing
	if app.Branch == "" {
		app.Branch = "main"
	}
	if app.ComposePath == "" {
		app.ComposePath = "compose.yaml"
	}
	if app.PollInterval == "" {
		app.PollInterval = "30s"
	}
	// New apps should enter the reconciliation pipeline immediately.
	app.Status = "pending"
	app.LastSyncAt = time.Time{}

	if err := r.store.CreateApp(context.Background(), app); err != nil {
		return err
	}
	noteNewAppMetrics(app)

	// If no credentials to store, return early
	if !hasSecret && !hasEnvVars {
		return nil
	}

	cred := &store.AppCredential{
		AppID: app.ID,
	}

	if hasSecret {
		plaintext := []byte(secret)
		defer zeroBytes(plaintext)

		ciphertext, nonce, err := r.credentials.Encrypt(plaintext)
		if err != nil {
			_ = r.store.DeleteApp(context.Background(), app.ID)
			return err
		}
		cred.DeployKeyCiphertext = ciphertext
		cred.DeployKeyNonce = nonce
	}

	if hasEnvVars {
		// Serialize map to JSON before encryption
		// We use standard JSON marshalling
		jsonBytes, err := json.Marshal(serviceEnvs)
		if err != nil {
			_ = r.store.DeleteApp(context.Background(), app.ID)
			return fmt.Errorf("failed to serialize env vars: %w", err)
		}
		defer zeroBytes(jsonBytes)

		ciphertext, nonce, err := r.credentials.Encrypt(jsonBytes)
		if err != nil {
			_ = r.store.DeleteApp(context.Background(), app.ID)
			return err
		}
		cred.EnvCiphertext = ciphertext
		cred.EnvNonce = nonce
	}

	if err := r.store.UpsertAppCredential(context.Background(), cred); err != nil {
		_ = r.store.DeleteApp(context.Background(), app.ID)
		return err
	}

	return nil
}

// AddWithDeployKey registers a new application and stores deploy-key credentials when provided.
func (r *Registry) AddWithDeployKey(app *api.App, deployKey string) error {
	return r.AddWithDeployKeyAndEnvs(app, deployKey, nil)
}

// Get retrieves an application by ID.
func (r *Registry) Get(id string) (*api.App, error) {
	app, err := r.store.GetApp(context.Background(), id)
	if err != nil {
		return nil, err
	}
	enrichAppSyncError(app)
	return app, nil
}

// List returns all registered applications.
func (r *Registry) List() []*api.App {
	apps, err := r.store.ListApps(context.Background())
	if err != nil {
		// Log error? For now return empty list to be safe for UI.
		return []*api.App{}
	}
	for _, app := range apps {
		enrichAppSyncError(app)
	}
	return apps
}

// ListAppSnapshots returns app state for Prometheus gauge collectors.
func (r *Registry) ListAppSnapshots() []metrics.AppSnapshot {
	apps := r.List()
	out := make([]metrics.AppSnapshot, 0, len(apps))
	for _, app := range apps {
		if app == nil {
			continue
		}
		out = append(out, metrics.AppSnapshot{
			ID:                 app.ID,
			Name:               app.Name,
			Status:             app.Status,
			Suspended:          app.Suspended,
			LastSyncAt:         app.LastSyncAt,
			LastSyncError:      app.LastSyncError,
			LastSyncErrorCode:  app.LastSyncErrorCode,
			LastSyncErrorStage: app.LastSyncErrorStage,
			LastSeenCommit:     app.LastSeenCommit,
			LastSyncedCommit:   app.LastSyncedCommit,
			LastHealthyCommit:  app.LastHealthyCommit,
			RejectedCommit:     app.RejectedCommit,
			Branch:             app.Branch,
			Revision:           app.Revision,
		})
	}
	return out
}

func enrichAppSyncError(app *api.App) {
	if app == nil {
		return
	}
	code, stage, msg := syncerr.Decode(app.LastSyncError)
	app.LastSyncErrorCode = code
	app.LastSyncErrorStage = stage
	if msg != "" {
		app.LastSyncError = redact.String(msg)
	}
	app.LastSyncOutput = redact.String(app.LastSyncOutput)
}

// Delete removes an application by ID and drops its metric series.
func (r *Registry) Delete(id string) error {
	app, _ := r.Get(id)
	if err := r.store.DeleteAppCredential(context.Background(), id); err != nil {
		return err
	}
	if err := r.store.DeleteApp(context.Background(), id); err != nil {
		return err
	}
	if app != nil {
		still := false
		for _, other := range r.List() {
			if other != nil && other.RepoURL == app.RepoURL {
				still = true
				break
			}
		}
		metrics.ForgetApp(app.Name, app.ID, app.RepoURL, still)
	}
	return nil
}

func noteNewAppMetrics(app *api.App) {
	if app == nil {
		return
	}
	metrics.EnsureAppReloads(app.Name)
	metrics.EnsureGitFetchErrors(app.RepoURL)
}

// UpdateApp updates an application's editable fields and optionally its environment variables.
func (r *Registry) UpdateApp(id, name, branch, composePath, pollInterval string, serviceEnvs map[string]string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("app id is required")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("app name is required")
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("branch is required")
	}
	if strings.TrimSpace(composePath) == "" {
		return fmt.Errorf("compose path is required")
	}
	if strings.TrimSpace(pollInterval) == "" {
		return fmt.Errorf("poll interval is required")
	}

	// Verify app exists
	if _, err := r.store.GetApp(context.Background(), id); err != nil {
		return fmt.Errorf("app not found: %w", err)
	}

	// Update app fields
	if err := r.store.UpdateApp(context.Background(), id, name, branch, composePath, pollInterval); err != nil {
		return fmt.Errorf("failed to update app: %w", err)
	}

	// Update environment variables if provided
	if serviceEnvs != nil {
		hasEnvVars := len(serviceEnvs) > 0

		if hasEnvVars && (r.credentials == nil || !r.credentials.Enabled()) {
			return fmt.Errorf("encryption support is unavailable: set %s", credentials.EncryptionKeyEnv)
		}

		if hasEnvVars {
			// Serialize and encrypt env vars
			jsonBytes, err := json.Marshal(serviceEnvs)
			if err != nil {
				return fmt.Errorf("failed to serialize env vars: %w", err)
			}
			defer zeroBytes(jsonBytes)

			ciphertext, nonce, err := r.credentials.Encrypt(jsonBytes)
			if err != nil {
				return fmt.Errorf("failed to encrypt env vars: %w", err)
			}

			// Check if credentials exist
			_, err = r.store.GetAppCredential(context.Background(), id)
			if err != nil {
				if errors.Is(err, store.ErrCredentialNotFound) {
					// Create new credential entry with only env vars
					cred := &store.AppCredential{
						AppID:         id,
						EnvCiphertext: ciphertext,
						EnvNonce:      nonce,
					}
					if err := r.store.UpsertAppCredential(context.Background(), cred); err != nil {
						return fmt.Errorf("failed to create app credentials: %w", err)
					}
				} else {
					return fmt.Errorf("failed to check credentials: %w", err)
				}
			} else {
				// Update existing credentials
				if err := r.store.UpdateAppCredentials(context.Background(), id, ciphertext, nonce); err != nil {
					return fmt.Errorf("failed to update app credentials: %w", err)
				}
			}
		} else {
			// Clear env vars by updating to empty
			_ = r.store.UpdateAppCredentials(context.Background(), id, nil, nil)
		}
	}

	return nil
}

// GetAppEnvs returns the decrypted environment variables for the app.
func (r *Registry) GetAppEnvs(id string) (map[string]string, error) {
	credential, err := r.store.GetAppCredential(context.Background(), id)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			return nil, nil
		}
		return nil, err
	}

	if len(credential.EnvCiphertext) == 0 {
		return nil, nil
	}

	if r.credentials == nil || !r.credentials.Enabled() {
		return nil, fmt.Errorf("encryption support is unavailable: set %s", credentials.EncryptionKeyEnv)
	}

	jsonBytes, err := r.credentials.Decrypt(credential.EnvCiphertext, credential.EnvNonce)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt env vars: %w", err)
	}
	defer zeroBytes(jsonBytes)

	var envs map[string]string
	if err := json.Unmarshal(jsonBytes, &envs); err != nil {
		return nil, fmt.Errorf("failed to deserialize env vars: %w", err)
	}

	return envs, nil
}

// GetDeployKey returns the decrypted deploy key for the app if configured.
func (r *Registry) GetDeployKey(id string) ([]byte, error) {
	return r.GetRepoSecret(id)
}

// ClearRepoSecret removes stored deploy key / git token ciphertext (keeps env credentials).
// Used for fleet-managed token apps that must read CONOPS_GIT_TOKEN from the process env.
func (r *Registry) ClearRepoSecret(id string) error {
	return r.store.ClearAppRepoSecret(context.Background(), id)
}

// UpdateRepoSecret encrypts and stores a new deploy key / git token for the app.
func (r *Registry) UpdateRepoSecret(id, secret string) error {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return r.ClearRepoSecret(id)
	}
	app, err := r.Get(id)
	if err != nil {
		return err
	}
	if app.RepoAuthMethod == repoauth.MethodDeployKey {
		secret = repoauth.NormalizeDeployKey(secret)
	}
	if r.credentials == nil || !r.credentials.Enabled() {
		return fmt.Errorf("encryption support is unavailable: set %s", credentials.EncryptionKeyEnv)
	}
	plaintext := []byte(secret)
	defer zeroBytes(plaintext)
	ciphertext, nonce, err := r.credentials.Encrypt(plaintext)
	if err != nil {
		return err
	}
	cred := &store.AppCredential{
		AppID:               id,
		DeployKeyCiphertext: ciphertext,
		DeployKeyNonce:      nonce,
	}
	return r.store.UpsertAppCredential(context.Background(), cred)
}

// UpdateEnvHash stores the last successfully applied external env_file content hash.
func (r *Registry) UpdateEnvHash(id, hash string) error {
	return r.store.UpdateAppEnvHash(context.Background(), id, hash)
}

// UpdateWatchHash stores the last applied watch_files content hash.
func (r *Registry) UpdateWatchHash(id, hash string) error {
	return r.store.UpdateAppWatchHash(context.Background(), id, hash)
}

// UpdateDeployPolicy stores fleet deploy policy JSON for an app.
func (r *Registry) UpdateDeployPolicy(id, policyJSON string) error {
	return r.store.UpdateAppDeployPolicy(context.Background(), id, policyJSON)
}

// ResetStuckSyncing marks apps left in "syncing" after a process crash as pending.
func (r *Registry) ResetStuckSyncing() int {
	n := 0
	for _, app := range r.List() {
		if app == nil || app.Status != "syncing" {
			continue
		}
		if err := r.UpdateStatus(app.ID, "pending", nil); err != nil {
			continue
		}
		n++
	}
	return n
}

// HasStoredRepoSecret reports whether encrypted repo credentials exist in the DB.
func (r *Registry) HasStoredRepoSecret(id string) (bool, error) {
	cred, err := r.store.GetAppCredential(context.Background(), id)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			return false, nil
		}
		return false, err
	}
	return len(cred.DeployKeyCiphertext) > 0, nil
}

// GetRepoSecret returns the decrypted deploy key or git token for the app.
func (r *Registry) GetRepoSecret(id string) ([]byte, error) {
	app, err := r.Get(id)
	if err != nil {
		return nil, err
	}

	credential, err := r.store.GetAppCredential(context.Background(), id)
	if err != nil {
		if errors.Is(err, store.ErrCredentialNotFound) {
			if app.RepoAuthMethod == repoauth.MethodToken {
				if t := repoauth.ResolveGitToken(""); t != "" {
					return []byte(t), nil
				}
			}
			return nil, nil
		}
		return nil, err
	}

	if len(credential.DeployKeyCiphertext) == 0 {
		if app.RepoAuthMethod == repoauth.MethodToken {
			if t := repoauth.ResolveGitToken(""); t != "" {
				return []byte(t), nil
			}
		}
		return nil, nil
	}

	if r.credentials == nil || !r.credentials.Enabled() {
		return nil, fmt.Errorf("credential decryption unavailable: set %s", credentials.EncryptionKeyEnv)
	}

	secret, err := r.credentials.Decrypt(credential.DeployKeyCiphertext, credential.DeployKeyNonce)
	if err != nil {
		return nil, err
	}

	if app.RepoAuthMethod == repoauth.MethodDeployKey {
		normalized := []byte(repoauth.NormalizeDeployKey(string(secret)))
		zeroBytes(secret)
		return normalized, nil
	}
	if app.RepoAuthMethod == repoauth.MethodToken {
		token := []byte(strings.TrimSpace(string(secret)))
		zeroBytes(secret)
		if len(token) == 0 {
			if t := repoauth.ResolveGitToken(""); t != "" {
				return []byte(t), nil
			}
		}
		return token, nil
	}
	zeroBytes(secret)
	return nil, nil
}

// FindByName returns the first app with the given name (case-insensitive).
func (r *Registry) FindByName(name string) *api.App {
	name = strings.TrimSpace(name)
	for _, app := range r.List() {
		if app != nil && strings.EqualFold(app.Name, name) {
			return app
		}
	}
	return nil
}

// UpdateCommit updates the latest commit hash for an app.
func (r *Registry) UpdateCommit(id, commitHash string) error {
	return r.UpdateCommitWithMessage(id, commitHash, "")
}

// UpdateCommitWithMessage updates latest desired commit hash and subject.
func (r *Registry) UpdateCommitWithMessage(id, commitHash, commitMessage string) error {
	return r.store.UpdateAppCommit(context.Background(), id, commitHash, commitMessage)
}

// UpdateStatus updates app status and optionally the last sync time.
func (r *Registry) UpdateStatus(id, status string, lastSyncAt *time.Time) error {
	return r.store.UpdateAppStatus(context.Background(), id, status, lastSyncAt)
}

// SetSuspended stores the fleet git suspend flag. It does not clear an operator pause.
func (r *Registry) SetSuspended(id string, suspended bool) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("app id is required")
	}
	if _, err := r.Get(id); err != nil {
		return err
	}
	return r.store.SetAppSuspended(context.Background(), id, suspended)
}

// SetOperatorSuspended pauses or resumes automatic git polling and reconciliation.
// The flag is stored separately from fleet YAML and survives controller restarts.
func (r *Registry) SetOperatorSuspended(id string, suspended bool) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("app id is required")
	}
	if _, err := r.Get(id); err != nil {
		return err
	}
	if err := r.store.SetOperatorSuspended(context.Background(), id, suspended); err != nil {
		return err
	}
	if suspended {
		return nil
	}
	// Resume: apply git on the next reconcile even when the commit did not change.
	return r.store.UpdateAppStatus(context.Background(), id, "pending", nil)
}

// SetRevision stores the fleet revision ref (branch, tag, or full SHA). Empty tracks Branch.
func (r *Registry) SetRevision(id, revision string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("app id is required")
	}
	return r.store.SetAppRevision(context.Background(), id, strings.TrimSpace(revision))
}

// MarkPending queues the app for the next reconcile even when the git commit is unchanged.
func (r *Registry) MarkPending(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("app id is required")
	}
	return r.UpdateStatus(id, "pending", nil)
}

// UpdateRollback stores the last healthy revision and a revision that must not be retried.
func (r *Registry) UpdateRollback(id, lastHealthyCommit, rejectedCommit, lastHealthyVars, rejectedVars string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("app id is required")
	}
	return r.store.UpdateAppRollback(context.Background(), id, lastHealthyCommit, rejectedCommit, lastHealthyVars, rejectedVars)
}

// UpdateSyncResult stores sync execution metadata.
// syncError may be a plain string or syncerr.Encode(...) JSON.
func (r *Registry) UpdateSyncResult(
	id string,
	status string,
	lastSyncAt time.Time,
	syncedCommit string,
	syncedCommitMessage string,
	syncOutput string,
	syncError string,
) error {
	return r.store.UpdateAppSyncResult(
		context.Background(),
		id,
		status,
		lastSyncAt,
		syncedCommit,
		syncedCommitMessage,
		syncOutput,
		syncError,
	)
}

// UpdateSyncResultErr stores sync metadata with a structured error.
func (r *Registry) UpdateSyncResultErr(
	id string,
	status string,
	lastSyncAt time.Time,
	syncedCommit string,
	syncedCommitMessage string,
	syncOutput string,
	err error,
) error {
	return r.UpdateSyncResult(id, status, lastSyncAt, syncedCommit, syncedCommitMessage, syncOutput, syncerr.Encode(err))
}

// UpdateSyncProgress stores in-flight sync logs while status is syncing.
func (r *Registry) UpdateSyncProgress(id string, lastSyncAt time.Time, syncOutput string) error {
	return r.store.UpdateAppSyncProgress(context.Background(), id, lastSyncAt, syncOutput)
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
