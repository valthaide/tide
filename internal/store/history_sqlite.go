package store

import (
	"context"
	"time"

	"github.com/valthaide/conops-private/internal/api"
)

func (s *SQLiteStore) InsertAppRevision(ctx context.Context, rev api.Revision) error {
	return retryLock(ctx, func() error {
		var sha, vars string
		err := s.db.QueryRowContext(ctx, `SELECT git_sha, vars_json FROM app_revisions WHERE app_id = ? ORDER BY id DESC LIMIT 1`, rev.AppID).Scan(&sha, &vars)
		if err == nil && sha == rev.GitSHA && vars == rev.VarsJSON {
			return nil
		}
		created := rev.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO app_revisions (app_id, git_sha, vars_json, fleet_sha, images_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`, rev.AppID, rev.GitSHA, rev.VarsJSON, rev.FleetSHA, rev.ImagesJSON, created); err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `DELETE FROM app_revisions WHERE app_id = ? AND id NOT IN (SELECT id FROM app_revisions WHERE app_id = ? ORDER BY id DESC LIMIT 5)`, rev.AppID, rev.AppID)
		return err
	})
}

func (s *SQLiteStore) ListAppRevisions(ctx context.Context, appID string, limit int) ([]api.Revision, error) {
	if limit <= 0 {
		limit = 20
	}
	var out []api.Revision
	err := retryLock(ctx, func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT git_sha, vars_json, fleet_sha, images_json, created_at FROM app_revisions WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var rev api.Revision
			if err := rows.Scan(&rev.GitSHA, &rev.VarsJSON, &rev.FleetSHA, &rev.ImagesJSON, &rev.CreatedAt); err != nil {
				return err
			}
			rev.AppID = appID
			out = append(out, rev)
		}
		return rows.Err()
	})
	if out == nil {
		out = []api.Revision{}
	}
	return out, err
}

func (s *SQLiteStore) InsertDeployEvent(ctx context.Context, event api.DeployEvent) error {
	return retryLock(ctx, func() error {
		created := event.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO deploy_events (app_id, app_name, project, git_sha, fleet_sha, vars_json, images_json, result, trigger_name, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.AppID, event.App, event.Project, event.GitSHA, event.FleetSHA, event.VarsJSON, event.Images, event.Result, event.Trigger, created); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `DELETE FROM deploy_events WHERE app_id = ? AND id NOT IN (SELECT id FROM deploy_events WHERE app_id = ? ORDER BY id DESC LIMIT 50)`, event.AppID, event.AppID)
		return err
	})
}

func (s *SQLiteStore) ListDeployEvents(ctx context.Context, appID string, limit int) ([]api.DeployEvent, error) {
	if limit <= 0 {
		limit = 20
	}
	var out []api.DeployEvent
	err := retryLock(ctx, func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT app_name, project, git_sha, fleet_sha, vars_json, images_json, result, trigger_name, created_at FROM deploy_events WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var event api.DeployEvent
			if err := rows.Scan(&event.App, &event.Project, &event.GitSHA, &event.FleetSHA, &event.VarsJSON, &event.Images, &event.Result, &event.Trigger, &event.CreatedAt); err != nil {
				return err
			}
			event.AppID = appID
			out = append(out, event)
		}
		return rows.Err()
	})
	if out == nil {
		out = []api.DeployEvent{}
	}
	return out, err
}
