package store

import (
	"context"
	"errors"
	"time"

	"github.com/valthaide/conops-private/internal/api"
)

var ErrCredentialNotFound = errors.New("app credential not found")

// Store defines the interface for data persistence.
type Store interface {
	CreateApp(ctx context.Context, app *api.App) error
	GetApp(ctx context.Context, id string) (*api.App, error)
	ListApps(ctx context.Context) ([]*api.App, error)
	DeleteApp(ctx context.Context, id string) error
	UpdateApp(ctx context.Context, id, name, branch, composePath, pollInterval string) error
	SetAppSuspended(ctx context.Context, id string, suspended bool) error
	SetAppRevision(ctx context.Context, id, revision string) error
	SetOperatorSuspended(ctx context.Context, id string, suspended bool) error
	UpdateAppRollback(ctx context.Context, id, lastHealthyCommit, rejectedCommit, lastHealthyVars, rejectedVars string) error
	UpsertAppCredential(ctx context.Context, credential *AppCredential) error
	GetAppCredential(ctx context.Context, id string) (*AppCredential, error)
	DeleteAppCredential(ctx context.Context, id string) error
	ClearAppRepoSecret(ctx context.Context, appID string) error
	UpdateAppCredentials(ctx context.Context, appID string, envCiphertext, envNonce []byte) error
	UpdateAppEnvHash(ctx context.Context, id, envHash string) error
	UpdateAppWatchHash(ctx context.Context, id, watchHash string) error
	UpdateAppDeployPolicy(ctx context.Context, id, policyJSON string) error
	UpdateAppCommit(ctx context.Context, id, commitHash, commitMessage string) error
	UpdateAppStatus(ctx context.Context, id, status string, lastSyncAt *time.Time) error
	UpdateAppSyncResult(
		ctx context.Context,
		id string,
		status string,
		lastSyncAt time.Time,
		syncedCommit string,
		syncedCommitMessage string,
		syncOutput string,
		syncError string,
	) error
	UpdateAppSyncProgress(ctx context.Context, id string, lastSyncAt time.Time, syncOutput string) error
	Ping(ctx context.Context) error
	Close()
	InsertAppRevision(ctx context.Context, rev api.Revision) error
	ListAppRevisions(ctx context.Context, appID string, limit int) ([]api.Revision, error)
	InsertDeployEvent(ctx context.Context, event api.DeployEvent) error
	ListDeployEvents(ctx context.Context, appID string, limit int) ([]api.DeployEvent, error)
}

// AppCredential stores encrypted app-level credentials.
type AppCredential struct {
	AppID               string
	DeployKeyCiphertext []byte
	DeployKeyNonce      []byte
	EnvCiphertext       []byte
	EnvNonce            []byte
}
