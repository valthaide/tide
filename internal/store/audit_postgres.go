package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/valthaide/tide/internal/api"
)

func (s *PostgresStore) UpdateAppRepository(ctx context.Context, id, repoURL, repoAuthMethod, branch, revision, composePath string) error {
	ct, err := s.pool.Exec(
		ctx,
		`UPDATE apps SET repo_url = $1, repo_auth_method = $2, branch = $3, revision = $4, compose_path = $5 WHERE id = $6`,
		repoURL, repoAuthMethod, branch, revision, composePath, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("app not found")
	}
	_, _ = s.BumpDesiredGeneration(ctx, id)
	return nil
}

func (s *PostgresStore) UpdateAppCIStatus(ctx context.Context, id, ciStatus, ciReason, ciURL, ciSource string, ciPipelineID int64, checkedAt time.Time) error {
	var ts interface{}
	if !checkedAt.IsZero() {
		ts = checkedAt
	}
	ct, err := s.pool.Exec(
		ctx,
		`UPDATE apps SET ci_status = $1, ci_reason = $2, ci_url = $3, ci_source = $4, ci_pipeline_id = $5, ci_checked_at = $6 WHERE id = $7`,
		ciStatus, ciReason, ciURL, ciSource, ciPipelineID, ts, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("app not found")
	}
	return nil
}

func (s *PostgresStore) InsertAuditEvent(ctx context.Context, event api.AuditEvent) error {
	created := event.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := s.pool.Exec(
		ctx,
		`INSERT INTO audit_events (
			created_at, kind, actor, app_id, app_name, repo_url, target_repo_url,
			git_sha, pipeline_id, pipeline_source, pipeline_status, pipeline_url,
			reason, decision, details_json
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
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
}

func (s *PostgresStore) ListAuditEvents(ctx context.Context, appID string, limit int) ([]api.AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	appID = strings.TrimSpace(appID)
	var rows pgx.Rows
	var err error
	if appID != "" {
		rows, err = s.pool.Query(
			ctx,
			`SELECT id, created_at, kind, actor, app_id, app_name, repo_url, target_repo_url,
				git_sha, pipeline_id, pipeline_source, pipeline_status, pipeline_url,
				reason, decision, details_json
			FROM audit_events WHERE app_id = $1 ORDER BY id DESC LIMIT $2`,
			appID, limit,
		)
	} else {
		rows, err = s.pool.Query(
			ctx,
			`SELECT id, created_at, kind, actor, app_id, app_name, repo_url, target_repo_url,
				git_sha, pipeline_id, pipeline_source, pipeline_status, pipeline_url,
				reason, decision, details_json
			FROM audit_events ORDER BY id DESC LIMIT $1`,
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.AuditEvent{}
	for rows.Next() {
		var ev api.AuditEvent
		if err := rows.Scan(
			&ev.ID, &ev.CreatedAt, &ev.Kind, &ev.Actor, &ev.AppID, &ev.AppName,
			&ev.RepoURL, &ev.TargetRepoURL, &ev.GitSHA, &ev.PipelineID,
			&ev.PipelineSource, &ev.PipelineStatus, &ev.PipelineURL,
			&ev.Reason, &ev.Decision, &ev.DetailsJSON,
		); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
