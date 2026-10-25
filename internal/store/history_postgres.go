package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/valthaide/conops-private/internal/api"
)

func (s *PostgresStore) InsertAppRevision(ctx context.Context, rev api.Revision) error {
	var sha, vars string
	err := s.pool.QueryRow(ctx, `SELECT git_sha, vars_json FROM app_revisions WHERE app_id = $1 ORDER BY id DESC LIMIT 1`, rev.AppID).Scan(&sha, &vars)
	if err == nil && sha == rev.GitSHA && vars == rev.VarsJSON {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	created := rev.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO app_revisions (app_id, git_sha, vars_json, fleet_sha, images_json, created_at) VALUES ($1, $2, $3, $4, $5, $6)`, rev.AppID, rev.GitSHA, rev.VarsJSON, rev.FleetSHA, rev.ImagesJSON, created); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM app_revisions WHERE app_id = $1 AND id NOT IN (SELECT id FROM app_revisions WHERE app_id = $1 ORDER BY id DESC LIMIT 5)`, rev.AppID)
	return err
}

func (s *PostgresStore) ListAppRevisions(ctx context.Context, appID string, limit int) ([]api.Revision, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT git_sha, vars_json, fleet_sha, images_json, created_at FROM app_revisions WHERE app_id = $1 ORDER BY id DESC LIMIT $2`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Revision{}
	for rows.Next() {
		var rev api.Revision
		if err := rows.Scan(&rev.GitSHA, &rev.VarsJSON, &rev.FleetSHA, &rev.ImagesJSON, &rev.CreatedAt); err != nil {
			return nil, err
		}
		rev.AppID = appID
		out = append(out, rev)
	}
	return out, rows.Err()
}

func (s *PostgresStore) InsertDeployEvent(ctx context.Context, event api.DeployEvent) error {
	created := event.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO deploy_events (app_id, app_name, project, git_sha, fleet_sha, vars_json, images_json, result, trigger_name, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, event.AppID, event.App, event.Project, event.GitSHA, event.FleetSHA, event.VarsJSON, event.Images, event.Result, event.Trigger, created); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM deploy_events WHERE app_id = $1 AND id NOT IN (SELECT id FROM deploy_events WHERE app_id = $1 ORDER BY id DESC LIMIT 50)`, event.AppID)
	return err
}

func (s *PostgresStore) ListDeployEvents(ctx context.Context, appID string, limit int) ([]api.DeployEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT app_name, project, git_sha, fleet_sha, vars_json, images_json, result, trigger_name, created_at FROM deploy_events WHERE app_id = $1 ORDER BY id DESC LIMIT $2`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.DeployEvent{}
	for rows.Next() {
		var event api.DeployEvent
		if err := rows.Scan(&event.App, &event.Project, &event.GitSHA, &event.FleetSHA, &event.VarsJSON, &event.Images, &event.Result, &event.Trigger, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.AppID = appID
		out = append(out, event)
	}
	return out, rows.Err()
}
