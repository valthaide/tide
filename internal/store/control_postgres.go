package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) BumpDesiredGeneration(ctx context.Context, id string) (int64, error) {
	var gen int64
	err := s.pool.QueryRow(ctx, `
	INSERT INTO app_control (app_id, desired_generation) VALUES ($1, 1)
	ON CONFLICT (app_id) DO UPDATE SET desired_generation = app_control.desired_generation + 1
	RETURNING desired_generation
	`, id).Scan(&gen)
	return gen, err
}

func (s *PostgresStore) DesiredGeneration(ctx context.Context, id string) (int64, error) {
	var gen int64
	err := s.pool.QueryRow(ctx, `SELECT desired_generation FROM app_control WHERE app_id = $1`, id).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return gen, err
}

func (s *PostgresStore) SetRetryState(ctx context.Context, id string, attempt int, next time.Time) error {
	_, err := s.pool.Exec(ctx, `
	INSERT INTO app_control (app_id, retry_attempt, next_retry_at) VALUES ($1, $2, $3)
	ON CONFLICT (app_id) DO UPDATE SET retry_attempt = EXCLUDED.retry_attempt, next_retry_at = EXCLUDED.next_retry_at
	`, id, attempt, next)
	return err
}

func (s *PostgresStore) ClearRetryState(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE app_control SET retry_attempt = 0, next_retry_at = NULL WHERE app_id = $1`, id)
	return err
}

func (s *PostgresStore) RetryState(ctx context.Context, id string) (int, time.Time, error) {
	var attempt int
	var next *time.Time
	err := s.pool.QueryRow(ctx, `SELECT retry_attempt, next_retry_at FROM app_control WHERE app_id = $1`, id).Scan(&attempt, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil || next == nil {
		return attempt, time.Time{}, err
	}
	return attempt, *next, nil
}

func (s *PostgresStore) SetNextReconcile(ctx context.Context, id string, next, last time.Time) error {
	_, err := s.pool.Exec(ctx, `
	INSERT INTO app_control (app_id, next_reconcile_at, last_reconcile_at) VALUES ($1, $2, $3)
	ON CONFLICT (app_id) DO UPDATE SET next_reconcile_at = EXCLUDED.next_reconcile_at, last_reconcile_at = EXCLUDED.last_reconcile_at
	`, id, nullTime(next), nullTime(last))
	return err
}

func (s *PostgresStore) SetQueued(ctx context.Context, id string, queued time.Time, trigger string, class int) error {
	_, err := s.pool.Exec(ctx, `
	INSERT INTO app_control (app_id, queued_at, trigger_name, queue_class) VALUES ($1, $2, $3, $4)
	ON CONFLICT (app_id) DO UPDATE SET queued_at = EXCLUDED.queued_at, trigger_name = EXCLUDED.trigger_name, queue_class = EXCLUDED.queue_class
	`, id, nullTime(queued), trigger, class)
	return err
}

func (s *PostgresStore) ClearQueued(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE app_control SET queued_at = NULL, trigger_name = '', queue_class = 0 WHERE app_id = $1`, id)
	return err
}

func (s *PostgresStore) LoadSchedule(ctx context.Context, id string) (time.Time, time.Time, time.Time, string, int, error) {
	var next, last, queued *time.Time
	var trigger string
	var class int
	err := s.pool.QueryRow(ctx, `
	SELECT next_reconcile_at, last_reconcile_at, queued_at, trigger_name, queue_class
	FROM app_control WHERE app_id = $1`, id).Scan(&next, &last, &queued, &trigger, &class)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, time.Time{}, time.Time{}, "", 0, nil
	}
	return ptrTime(next), ptrTime(last), ptrTime(queued), trigger, class, err
}

func ptrTime(v *time.Time) time.Time {
	if v == nil {
		return time.Time{}
	}
	return *v
}
