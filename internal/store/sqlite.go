package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/valthaide/conops-private/internal/api"
	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore initializes the SQLite database and creates necessary tables.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := retryLock(context.Background(), func() error { return db.Ping() }); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	if path != "" && path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		_ = os.Chmod(path, 0600)
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	query := `
	CREATE TABLE IF NOT EXISTS apps (
		id TEXT PRIMARY KEY,
		name TEXT,
		repo_url TEXT,
		repo_auth_method TEXT NOT NULL DEFAULT 'public',
		branch TEXT,
		compose_path TEXT,
		poll_interval TEXT,
		last_seen_commit TEXT,
		last_seen_commit_message TEXT,
		last_synced_commit TEXT,
		last_synced_commit_message TEXT,
		last_sync_output TEXT,
		last_sync_error TEXT,
		last_sync_at DATETIME,
		status TEXT
	);
	`
	if _, err := db.Exec(query); err != nil {
		return nil, fmt.Errorf("failed to create apps table: %w", err)
	}

	if err := addSQLiteColumnIfMissing(db, "apps", "repo_auth_method TEXT NOT NULL DEFAULT 'public'"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_seen_commit_message TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_synced_commit TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_synced_commit_message TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_sync_output TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_sync_error TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "suspended INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_applied_env_hash TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_applied_watch_hash TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "deploy_policy_json TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "operator_suspended INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_healthy_commit TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "rejected_commit TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "revision TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "last_healthy_vars TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "apps", "rejected_vars TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS app_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		app_id TEXT NOT NULL,
		git_sha TEXT NOT NULL,
		vars_json TEXT NOT NULL DEFAULT '',
		fleet_sha TEXT NOT NULL DEFAULT '',
		images_json TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);
	`); err != nil {
		return nil, fmt.Errorf("failed to create app_revisions table: %w", err)
	}
	if err := addSQLiteColumnIfMissing(db, "app_revisions", "images_json TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS deploy_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		app_id TEXT NOT NULL,
		app_name TEXT NOT NULL DEFAULT '',
		project TEXT NOT NULL DEFAULT '',
		git_sha TEXT NOT NULL DEFAULT '',
		fleet_sha TEXT NOT NULL DEFAULT '',
		vars_json TEXT NOT NULL DEFAULT '',
		images_json TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT '',
		trigger_name TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);
	`); err != nil {
		return nil, fmt.Errorf("failed to create deploy_events table: %w", err)
	}

	credentialsQuery := `
	CREATE TABLE IF NOT EXISTS app_credentials (
		app_id TEXT PRIMARY KEY,
		deploy_key_ciphertext BLOB,
		deploy_key_nonce BLOB,
		env_ciphertext BLOB,
		env_nonce BLOB,
		FOREIGN KEY(app_id) REFERENCES apps(id) ON DELETE CASCADE
	);
	`
	if _, err := db.Exec(credentialsQuery); err != nil {
		return nil, fmt.Errorf("failed to create app_credentials table: %w", err)
	}

	// Add env_ciphertext and env_nonce columns to app_credentials
	if err := addSQLiteColumnIfMissing(db, "app_credentials", "env_ciphertext BLOB"); err != nil {
		return nil, err
	}
	if err := addSQLiteColumnIfMissing(db, "app_credentials", "env_nonce BLOB"); err != nil {
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) CreateApp(ctx context.Context, app *api.App) error {
	query := `
	INSERT INTO apps (
		id,
		name,
		repo_url,
		repo_auth_method,
		branch,
		compose_path,
		poll_interval,
		last_seen_commit,
		last_seen_commit_message,
		last_synced_commit,
		last_synced_commit_message,
		last_sync_output,
		last_sync_error,
		last_sync_at,
		status,
		suspended
	)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	suspended := 0
	if app.Suspended {
		suspended = 1
	}
	_, err := s.execContext(
		ctx,
		query,
		app.ID,
		app.Name,
		app.RepoURL,
		app.RepoAuthMethod,
		app.Branch,
		app.ComposePath,
		app.PollInterval,
		app.LastSeenCommit,
		app.LastSeenCommitMessage,
		app.LastSyncedCommit,
		app.LastSyncedCommitMessage,
		app.LastSyncOutput,
		app.LastSyncError,
		app.LastSyncAt,
		app.Status,
		suspended,
	)
	return err
}

func (s *SQLiteStore) GetApp(ctx context.Context, id string) (*api.App, error) {
	query := `
	SELECT
		id,
		name,
		repo_url,
		repo_auth_method,
		branch,
		compose_path,
		poll_interval,
		COALESCE(last_seen_commit, ''),
		COALESCE(last_seen_commit_message, ''),
		COALESCE(last_synced_commit, ''),
		COALESCE(last_synced_commit_message, ''),
		COALESCE(last_sync_output, ''),
		COALESCE(last_sync_error, ''),
		last_sync_at,
		status,
		COALESCE(suspended, 0),
		COALESCE(operator_suspended, 0),
		COALESCE(last_applied_env_hash, ''),
		COALESCE(last_applied_watch_hash, ''),
		COALESCE(deploy_policy_json, ''),
		COALESCE(last_healthy_commit, ''),
		COALESCE(rejected_commit, ''),
		COALESCE(revision, ''),
		COALESCE(last_healthy_vars, ''),
		COALESCE(rejected_vars, '')
	FROM apps
	WHERE id = ?
	`
	var app api.App
	var suspended, operatorSuspended int
	err := retryLock(ctx, func() error {
		row := s.db.QueryRowContext(ctx, query, id)
		return row.Scan(
			&app.ID,
			&app.Name,
			&app.RepoURL,
			&app.RepoAuthMethod,
			&app.Branch,
			&app.ComposePath,
			&app.PollInterval,
			&app.LastSeenCommit,
			&app.LastSeenCommitMessage,
			&app.LastSyncedCommit,
			&app.LastSyncedCommitMessage,
			&app.LastSyncOutput,
			&app.LastSyncError,
			&app.LastSyncAt,
			&app.Status,
			&suspended,
			&operatorSuspended,
			&app.LastAppliedEnvHash,
			&app.LastAppliedWatchHash,
			&app.DeployPolicyJSON,
			&app.LastHealthyCommit,
			&app.RejectedCommit,
			&app.Revision,
			&app.LastHealthyVars,
			&app.RejectedVars,
		)
	})
	if err != nil {
		return nil, err
	}
	applyRuntimeFlags(&app, suspended != 0, operatorSuspended != 0)
	return &app, nil
}

func (s *SQLiteStore) ListApps(ctx context.Context) ([]*api.App, error) {
	query := `
	SELECT
		id,
		name,
		repo_url,
		repo_auth_method,
		branch,
		compose_path,
		poll_interval,
		COALESCE(last_seen_commit, ''),
		COALESCE(last_seen_commit_message, ''),
		COALESCE(last_synced_commit, ''),
		COALESCE(last_synced_commit_message, ''),
		COALESCE(last_sync_output, ''),
		COALESCE(last_sync_error, ''),
		last_sync_at,
		status,
		COALESCE(suspended, 0),
		COALESCE(operator_suspended, 0),
		COALESCE(last_applied_env_hash, ''),
		COALESCE(last_applied_watch_hash, ''),
		COALESCE(deploy_policy_json, ''),
		COALESCE(last_healthy_commit, ''),
		COALESCE(rejected_commit, ''),
		COALESCE(revision, ''),
		COALESCE(last_healthy_vars, ''),
		COALESCE(rejected_vars, '')
	FROM apps
	`
	rows, err := s.queryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*api.App
	for rows.Next() {
		var app api.App
		var suspended, operatorSuspended int
		if err := rows.Scan(
			&app.ID,
			&app.Name,
			&app.RepoURL,
			&app.RepoAuthMethod,
			&app.Branch,
			&app.ComposePath,
			&app.PollInterval,
			&app.LastSeenCommit,
			&app.LastSeenCommitMessage,
			&app.LastSyncedCommit,
			&app.LastSyncedCommitMessage,
			&app.LastSyncOutput,
			&app.LastSyncError,
			&app.LastSyncAt,
			&app.Status,
			&suspended,
			&operatorSuspended,
			&app.LastAppliedEnvHash,
			&app.LastAppliedWatchHash,
			&app.DeployPolicyJSON,
			&app.LastHealthyCommit,
			&app.RejectedCommit,
			&app.Revision,
			&app.LastHealthyVars,
			&app.RejectedVars,
		); err != nil {
			continue
		}
		applyRuntimeFlags(&app, suspended != 0, operatorSuspended != 0)
		apps = append(apps, &app)
	}
	return apps, nil
}

func (s *SQLiteStore) DeleteApp(ctx context.Context, id string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM app_credentials WHERE app_id = ?`, id); err != nil {
		return err
	}

	query := `DELETE FROM apps WHERE id = ?`
	result, err := tx.ExecContext(ctx, query, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app not found")
	}

	return tx.Commit()
}

func (s *SQLiteStore) SetAppSuspended(ctx context.Context, id string, suspended bool) error {
	return s.setBoolColumn(ctx, "suspended", id, suspended)
}

func (s *SQLiteStore) SetOperatorSuspended(ctx context.Context, id string, suspended bool) error {
	return s.setBoolColumn(ctx, "operator_suspended", id, suspended)
}

func (s *SQLiteStore) setBoolColumn(ctx context.Context, column, id string, value bool) error {
	n := 0
	if value {
		n = 1
	}
	result, err := s.execContext(ctx, `UPDATE apps SET `+column+` = ? WHERE id = ?`, n, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) SetAppRevision(ctx context.Context, id, revision string) error {
	result, err := s.execContext(ctx, `UPDATE apps SET revision = ? WHERE id = ?`, revision, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppRollback(ctx context.Context, id, lastHealthyCommit, rejectedCommit, lastHealthyVars, rejectedVars string) error {
	result, err := s.execContext(ctx, `UPDATE apps SET last_healthy_commit = ?, rejected_commit = ?, last_healthy_vars = ?, rejected_vars = ? WHERE id = ?`, lastHealthyCommit, rejectedCommit, lastHealthyVars, rejectedVars, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateApp(ctx context.Context, id, name, branch, composePath, pollInterval string) error {
	query := `
	UPDATE apps
	SET
		name = ?,
		branch = ?,
		compose_path = ?,
		poll_interval = ?
	WHERE id = ?
	`
	result, err := s.execContext(ctx, query, name, branch, composePath, pollInterval, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpsertAppCredential(ctx context.Context, credential *AppCredential) error {
	query := `
	INSERT INTO app_credentials (app_id, deploy_key_ciphertext, deploy_key_nonce, env_ciphertext, env_nonce)
	VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(app_id) DO UPDATE SET
		deploy_key_ciphertext = CASE WHEN excluded.deploy_key_ciphertext IS NOT NULL THEN excluded.deploy_key_ciphertext ELSE app_credentials.deploy_key_ciphertext END,
		deploy_key_nonce = CASE WHEN excluded.deploy_key_nonce IS NOT NULL THEN excluded.deploy_key_nonce ELSE app_credentials.deploy_key_nonce END,
		env_ciphertext = CASE WHEN excluded.env_ciphertext IS NOT NULL THEN excluded.env_ciphertext ELSE app_credentials.env_ciphertext END,
		env_nonce = CASE WHEN excluded.env_nonce IS NOT NULL THEN excluded.env_nonce ELSE app_credentials.env_nonce END
	`
	_, err := s.execContext(ctx, query, credential.AppID, credential.DeployKeyCiphertext, credential.DeployKeyNonce, credential.EnvCiphertext, credential.EnvNonce)
	return err
}

func (s *SQLiteStore) GetAppCredential(ctx context.Context, id string) (*AppCredential, error) {
	query := `SELECT app_id, deploy_key_ciphertext, deploy_key_nonce, env_ciphertext, env_nonce FROM app_credentials WHERE app_id = ?`
	credential := &AppCredential{}
	var deployKeyCiphertext, deployKeyNonce, envCiphertext, envNonce []byte

	if err := retryLock(ctx, func() error {
		row := s.db.QueryRowContext(ctx, query, id)
		return row.Scan(&credential.AppID, &deployKeyCiphertext, &deployKeyNonce, &envCiphertext, &envNonce)
	}); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrCredentialNotFound
		}
		return nil, err
	}

	credential.DeployKeyCiphertext = deployKeyCiphertext
	credential.DeployKeyNonce = deployKeyNonce
	credential.EnvCiphertext = envCiphertext
	credential.EnvNonce = envNonce

	return credential, nil
}

func (s *SQLiteStore) DeleteAppCredential(ctx context.Context, id string) error {
	_, err := s.execContext(ctx, `DELETE FROM app_credentials WHERE app_id = ?`, id)
	return err
}

func (s *SQLiteStore) ClearAppRepoSecret(ctx context.Context, appID string) error {
	_, err := s.execContext(ctx, `
		UPDATE app_credentials
		SET deploy_key_ciphertext = NULL, deploy_key_nonce = NULL
		WHERE app_id = ?
	`, appID)
	return err
}

func (s *SQLiteStore) UpdateAppEnvHash(ctx context.Context, id, envHash string) error {
	result, err := s.execContext(ctx, `UPDATE apps SET last_applied_env_hash = ? WHERE id = ?`, envHash, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppWatchHash(ctx context.Context, id, watchHash string) error {
	result, err := s.execContext(ctx, `UPDATE apps SET last_applied_watch_hash = ? WHERE id = ?`, watchHash, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppDeployPolicy(ctx context.Context, id, policyJSON string) error {
	result, err := s.execContext(ctx, `UPDATE apps SET deploy_policy_json = ? WHERE id = ?`, policyJSON, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppCredentials(ctx context.Context, appID string, envCiphertext, envNonce []byte) error {
	query := `
	UPDATE app_credentials
	SET
		env_ciphertext = ?,
		env_nonce = ?
	WHERE app_id = ?
	`
	result, err := s.execContext(ctx, query, envCiphertext, envNonce, appID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app credentials not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppCommit(ctx context.Context, id, commitHash, commitMessage string) error {
	query := `UPDATE apps SET last_seen_commit = ?, last_seen_commit_message = ?, status = ? WHERE id = ?`
	result, err := s.execContext(ctx, query, commitHash, commitMessage, "pending", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppSyncResult(
	ctx context.Context,
	id string,
	status string,
	lastSyncAt time.Time,
	syncedCommit string,
	syncedCommitMessage string,
	syncOutput string,
	syncError string,
) error {
	query := `
	UPDATE apps
	SET
		status = ?,
		last_sync_at = ?,
		last_synced_commit = ?,
		last_synced_commit_message = ?,
		last_sync_output = ?,
		last_sync_error = ?
	WHERE id = ?
	`
	result, err := s.execContext(
		ctx,
		query,
		status,
		lastSyncAt,
		syncedCommit,
		syncedCommitMessage,
		syncOutput,
		syncError,
		id,
	)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppSyncProgress(ctx context.Context, id string, lastSyncAt time.Time, syncOutput string) error {
	query := `
	UPDATE apps
	SET
		last_sync_at = ?,
		last_sync_output = ?,
		last_sync_error = ''
	WHERE id = ?
	`
	result, err := s.execContext(ctx, query, lastSyncAt, syncOutput, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) UpdateAppStatus(ctx context.Context, id, status string, lastSyncAt *time.Time) error {
	if lastSyncAt == nil {
		query := `UPDATE apps SET status = ? WHERE id = ?`
		result, err := s.execContext(ctx, query, status, id)
		if err != nil {
			return err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rowsAffected == 0 {
			return fmt.Errorf("app not found")
		}
		return nil
	}

	query := `UPDATE apps SET status = ?, last_sync_at = ? WHERE id = ?`
	result, err := s.execContext(ctx, query, status, *lastSyncAt, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *SQLiteStore) Ping(ctx context.Context) error {
	return retryLock(ctx, func() error { return s.db.PingContext(ctx) })
}

func (s *SQLiteStore) Close() {
	s.db.Close()
}

func addSQLiteColumnIfMissing(db *sql.DB, tableName, columnDDL string) error {
	query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", tableName, columnDDL)
	err := retryLock(context.Background(), func() error {
		_, execErr := db.Exec(query)
		return execErr
	})
	if err != nil {
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "duplicate column name") {
			return nil
		}
		return fmt.Errorf("failed to alter %s table: %w", tableName, err)
	}
	return nil
}
