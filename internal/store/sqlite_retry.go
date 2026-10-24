package store

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"
)

const sqliteLockWait = 5 * time.Second

func sqliteDSN(path string) string {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.Contains(path, "?") {
		return path
	}
	return "file:" + path + "?_pragma=busy_timeout(200)"
}

func isDatabaseLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

// retryLock runs op until it succeeds, the error is not a lock, or 5s have passed.
// A success after a lock is logged at DEBUG.
func retryLock(ctx context.Context, op func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(sqliteLockWait)
	var last error
	for {
		err := op()
		if err == nil {
			if last != nil {
				slog.Debug("database lock cleared", "error", last.Error())
			}
			return nil
		}
		if !isDatabaseLocked(err) || !time.Now().Before(deadline) {
			return err
		}
		last = err
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

func (s *SQLiteStore) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := retryLock(ctx, func() error {
		var execErr error
		res, execErr = s.db.ExecContext(ctx, query, args...)
		return execErr
	})
	return res, err
}

func (s *SQLiteStore) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	err := retryLock(ctx, func() error {
		var queryErr error
		rows, queryErr = s.db.QueryContext(ctx, query, args...)
		return queryErr
	})
	return rows, err
}

func (s *SQLiteStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	var tx *sql.Tx
	err := retryLock(ctx, func() error {
		var beginErr error
		tx, beginErr = s.db.BeginTx(ctx, nil)
		return beginErr
	})
	return tx, err
}
