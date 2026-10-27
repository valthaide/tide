package store

import (
	"context"
	"database/sql"
	"time"
)

func (s *SQLiteStore) BumpDesiredGeneration(ctx context.Context, id string) (int64, error) {
	var gen int64
	err := retryLock(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_control (app_id, desired_generation) VALUES (?, 1)
		ON CONFLICT(app_id) DO UPDATE SET desired_generation = desired_generation + 1
		`, id)
		if err != nil {
			return err
		}
		return s.db.QueryRowContext(ctx, `SELECT desired_generation FROM app_control WHERE app_id = ?`, id).Scan(&gen)
	})
	return gen, err
}

func (s *SQLiteStore) DesiredGeneration(ctx context.Context, id string) (int64, error) {
	var gen int64
	err := retryLock(ctx, func() error {
		err := s.db.QueryRowContext(ctx, `SELECT desired_generation FROM app_control WHERE app_id = ?`, id).Scan(&gen)
		if err == sql.ErrNoRows {
			gen = 0
			return nil
		}
		return err
	})
	return gen, err
}

func (s *SQLiteStore) SetRetryState(ctx context.Context, id string, attempt int, next time.Time) error {
	return retryLock(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_control (app_id, retry_attempt, next_retry_at) VALUES (?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET retry_attempt = excluded.retry_attempt, next_retry_at = excluded.next_retry_at
		`, id, attempt, next)
		return err
	})
}

func (s *SQLiteStore) ClearRetryState(ctx context.Context, id string) error {
	return retryLock(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE app_control SET retry_attempt = 0, next_retry_at = NULL WHERE app_id = ?`, id)
		return err
	})
}

func (s *SQLiteStore) RetryState(ctx context.Context, id string) (int, time.Time, error) {
	var attempt int
	var next sql.NullTime
	err := retryLock(ctx, func() error {
		err := s.db.QueryRowContext(ctx, `SELECT retry_attempt, next_retry_at FROM app_control WHERE app_id = ?`, id).Scan(&attempt, &next)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	})
	if err != nil || !next.Valid {
		return attempt, time.Time{}, err
	}
	return attempt, next.Time, nil
}
