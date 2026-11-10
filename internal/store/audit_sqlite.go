package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/valthaide/tide/internal/api"
)

func (s *SQLiteStore) UpdateAppRepository(ctx context.Context, id, repoURL, repoAuthMethod, branch, revision, composePath string) error {
	query := `
	UPDATE apps
	SET repo_url = ?, repo_auth_method = ?, branch = ?, revision = ?, compose_path = ?
	WHERE id = ?
	`
	result, err := s.execContext(ctx, query, repoURL, repoAuthMethod, branch, revision, composePath, id)
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
	_, _ = s.BumpDesiredGeneration(ctx, id)
	return nil
}

func (s *SQLiteStore) UpdateAppCIStatus(ctx context.Context, id, ciStatus, ciReason, ciURL, ciSource string, ciPipelineID int64, checkedAt time.Time) error {
	var ts interface{}
	if !checkedAt.IsZero() {
		ts = checkedAt
	}
	query := `
	UPDATE apps
	SET ci_status = ?, ci_reason = ?, ci_url = ?, ci_source = ?, ci_pipeline_id = ?, ci_checked_at = ?
	WHERE id = ?
	`
	result, err := s.execContext(ctx, query, ciStatus, ciReason, ciURL, ciSource, ciPipelineID, ts, id)
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

// InsertAuditEvent appends an immutable audit event without truncating prior records.
func (s *SQLiteStore) InsertAuditEvent(ctx context.Context, event api.AuditEvent) error {
	return retryLock(ctx, func() error {
		created := event.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		_, err := s.db.ExecContext(
			ctx,
			`INSERT INTO audit_events (
				created_at, kind, actor, app_id, app_name, repo_url, target_repo_url,
				git_sha, pipeline_id, pipeline_source, pipeline_status, pipeline_url,
				reason, decision, details_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			created,
			event.Kind,
			event.Actor,
			event.AppID,
			event.AppName,
			event.RepoURL,
			event.TargetRepoURL,
			event.GitSHA,
			event.PipelineID,
			event.PipelineSource,
			event.PipelineStatus,
			event.PipelineURL,
			event.Reason,
			event.Decision,
			event.DetailsJSON,
		)
		return err
	})
}

func (s *SQLiteStore) ListAuditEvents(ctx context.Context, appID string, limit int) ([]api.AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	appID = strings.TrimSpace(appID)
	var out []api.AuditEvent
	err := retryLock(ctx, func() error {
		var rows *sql.Rows
		var err error
		if appID != "" {
			rows, err = s.db.QueryContext(
				ctx,
				`SELECT id, created_at, kind, actor, app_id, app_name, repo_url, target_repo_url,
					git_sha, pipeline_id, pipeline_source, pipeline_status, pipeline_url,
					reason, decision, details_json
				FROM audit_events WHERE app_id = ? ORDER BY id DESC LIMIT ?`,
				appID, limit,
			)
		} else {
			rows, err = s.db.QueryContext(
				ctx,
				`SELECT id, created_at, kind, actor, app_id, app_name, repo_url, target_repo_url,
					git_sha, pipeline_id, pipeline_source, pipeline_status, pipeline_url,
					reason, decision, details_json
				FROM audit_events ORDER BY id DESC LIMIT ?`,
				limit,
			)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var ev api.AuditEvent
			if err := rows.Scan(
				&ev.ID, &ev.CreatedAt, &ev.Kind, &ev.Actor, &ev.AppID, &ev.AppName,
				&ev.RepoURL, &ev.TargetRepoURL, &ev.GitSHA, &ev.PipelineID,
				&ev.PipelineSource, &ev.PipelineStatus, &ev.PipelineURL,
				&ev.Reason, &ev.Decision, &ev.DetailsJSON,
			); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	if out == nil {
		out = []api.AuditEvent{}
	}
	return out, err
}
