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

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (s *SQLiteStore) SetNextReconcile(ctx context.Context, id string, next, last time.Time) error {
	return retryLock(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_control (app_id, next_reconcile_at, last_reconcile_at) VALUES (?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET next_reconcile_at = excluded.next_reconcile_at, last_reconcile_at = excluded.last_reconcile_at
		`, id, nullTime(next), nullTime(last))
		return err
	})
}

func (s *SQLiteStore) SetQueued(ctx context.Context, id string, queued time.Time, trigger string, class int) error {
	return retryLock(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_control (app_id, queued_at, trigger_name, queue_class) VALUES (?, ?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET queued_at = excluded.queued_at, trigger_name = excluded.trigger_name, queue_class = excluded.queue_class
		`, id, nullTime(queued), trigger, class)
		return err
	})
}

func (s *SQLiteStore) ClearQueued(ctx context.Context, id string) error {
	return retryLock(ctx, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE app_control SET queued_at = NULL, trigger_name = '', queue_class = 0 WHERE app_id = ?`, id)
		return err
	})
}

func (s *SQLiteStore) LoadSchedule(ctx context.Context, id string) (time.Time, time.Time, time.Time, string, int, error) {
	var next, last, queued sql.NullTime
	var trigger string
	var class int
	err := retryLock(ctx, func() error {
		err := s.db.QueryRowContext(ctx, `
		SELECT next_reconcile_at, last_reconcile_at, queued_at, trigger_name, queue_class
		FROM app_control WHERE app_id = ?`, id).Scan(&next, &last, &queued, &trigger, &class)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	})
	return timeOf(next), timeOf(last), timeOf(queued), trigger, class, err
}

func timeOf(v sql.NullTime) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time
}
