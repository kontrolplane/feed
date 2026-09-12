package database

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kontrolplane/feed/internal/configuration"
)

// Row represents a single row result from a query.
type Row interface {
	Scan(dest ...any) error
}

// Rows represents multiple row results from a query.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

// Executor is the query surface shared by a pool and by a transaction. It
// exists so callers can write one helper that takes either, instead of
// duplicating every statement for the transactional and non-transactional
// paths.
//
// Placeholder style is deliberately *not* normalised here: SQLite uses `?`
// and PostgreSQL uses `$N`, and the store rewrites queries before they reach
// this layer. A transaction therefore behaves exactly like the pool it came
// from.
type Executor interface {
	Exec(ctx context.Context, query string, args ...any) error
	QueryRow(ctx context.Context, query string, args ...any) Row
	Query(ctx context.Context, query string, args ...any) (Rows, error)
}

// Tx is an open transaction. Exactly one of Commit or Rollback must be
// called; the idiomatic use is `defer tx.Rollback(ctx)` immediately after
// BeginTx, which is a no-op once Commit has succeeded.
//
// This exists because multi-statement operations (deleting a feed and its
// items, deleting a folder and everything under it, importing an OPML file)
// were previously issued as independent statements. A failure part-way
// through left the database holding orphaned rows that nothing would ever
// clean up.
type Tx interface {
	Executor
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// DB is the common database interface used throughout the application.
// Both SQLite and PostgreSQL implement this interface.
type DB interface {
	Executor
	// BeginTx starts a transaction. See Tx for the commit/rollback contract.
	BeginTx(ctx context.Context) (Tx, error)
	// Ping verifies the connection is still usable. Used by the health check
	// endpoint, so it must be cheap and must respect ctx cancellation.
	Ping(ctx context.Context) error
	Close() error
}

// CreatePool creates a database connection based on the configured driver.
// Returns a SQLite connection by default, or a PostgreSQL pool if configured.
func CreatePool(ctx context.Context, cfg configuration.FeedServiceConfiguration, logger *slog.Logger) (DB, error) {
	switch cfg.DatabaseDriver {
	case "postgres":
		return createPostgresPool(ctx, cfg, logger)
	case "sqlite":
		return createSQLitePool(ctx, cfg, logger)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.DatabaseDriver)
	}
}
