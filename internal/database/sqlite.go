package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/kontrolplane/feed/internal/configuration"
	_ "modernc.org/sqlite"
)

// sqlitePragmas are applied to every connection the driver opens.
//
// These MUST be spelled as `_pragma=name(value)`. The DSN previously used
// `_journal_mode=WAL&_busy_timeout=5000`, which is mattn/go-sqlite3 syntax.
// The driver actually linked here is modernc.org/sqlite, whose
// applyQueryParams only looks at `_pragma`, `_time_format`,
// `_time_integer_format`, `_timezone`, `_txlock`, `_inttotime` and
// `_texttotime`, and silently ignores every other key. The result was a
// database running in rollback-journal mode with busy_timeout 0 while the
// refresh worker wrote and HTTP handlers read concurrently — every collision
// became an immediate SQLITE_BUSY.
//
//   - journal_mode(WAL): readers no longer block the writer. This setting is
//     persisted in the database file itself, not per connection.
//   - busy_timeout(5000): wait up to 5s for a lock instead of failing at once.
//     Belt and braces next to SetMaxOpenConns(1); it still matters because
//     migrations open a second, separate connection to the same file.
//   - foreign_keys(1): SQLite defaults foreign key enforcement to OFF, per
//     connection. Without this the REFERENCES clauses in the migrations are
//     decorative and rows can point at parents that do not exist.
//   - synchronous(NORMAL): the recommended durability level under WAL. A
//     power loss can cost the last few committed transactions but cannot
//     corrupt the database. FULL would fsync on every commit, which for a
//     feed reader writing a row per item is pure cost.
var sqlitePragmas = []string{
	"journal_mode(WAL)",
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"synchronous(NORMAL)",
}

// sqliteDSN builds the SQLite DSN for a database file path.
//
// The driver splits the DSN at the first '?' and parses the remainder with
// url.ParseQuery, so the pragma values are encoded rather than concatenated
// raw.
func sqliteDSN(path string) string {
	q := make(url.Values, 1)
	for _, p := range sqlitePragmas {
		q.Add("_pragma", p)
	}
	// url.Values.Encode sorts by key; with a single repeated key the relative
	// order of the values is preserved, and the driver re-sorts pragmas itself
	// so busy_timeout is applied first regardless.
	return path + "?" + q.Encode()
}

// SQLiteDB wraps a *sql.DB to implement the DB interface.
type SQLiteDB struct {
	db *sql.DB
}

func createSQLitePool(ctx context.Context, cfg configuration.FeedServiceConfiguration, logger *slog.Logger) (*SQLiteDB, error) {
	dsn := sqliteDSN(cfg.DatabasePath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		logger.Error("failed to open sqlite database", slog.Any("error", err))
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// SQLite is a single file with a single writer. Letting database/sql open
	// an unbounded number of connections does not buy parallelism — it just
	// moves the contention into the file lock, where losing looks like
	// SQLITE_BUSY rather than like waiting.
	//
	// Pinning the pool to one connection makes database/sql itself the queue:
	// statements serialise in Go, respect their context deadline while they
	// wait, and never see a lock error. The trade-off is real — read queries
	// can no longer overlap, so a slow query blocks the next one even though
	// WAL would have allowed them to run together. For a single-user feed
	// reader whose heaviest read is a few thousand rows, that ceiling is far
	// above the load, and predictable serialisation is worth more than
	// theoretical read concurrency.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Keep the one connection for the life of the process: reconnecting costs
	// a fresh set of PRAGMAs and buys nothing for a local file.
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := validateSQLitePool(ctx, db); err != nil {
		logger.Error("failed to validate sqlite database", slog.Any("error", err))
		if cerr := db.Close(); cerr != nil {
			logger.Error("failed to close sqlite database after failed validation", slog.Any("error", cerr))
		}
		return nil, fmt.Errorf("failed to validate sqlite database: %w", err)
	}

	return &SQLiteDB{db: db}, nil
}

// validateSQLitePool proves the connection works *and* that the pragmas in the
// DSN were actually honoured. The silent-ignore behaviour that caused this bug
// is invisible at open time, so it is checked explicitly here: if a future
// driver change breaks the DSN syntax again, the process refuses to start
// instead of quietly running without WAL.
func validateSQLitePool(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite connection error: %w", err)
	}

	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("sqlite journal_mode query error: %w", err)
	}
	// An in-memory database cannot use WAL and legitimately reports "memory".
	if mode := strings.ToLower(journalMode); mode != "wal" && mode != "memory" {
		return fmt.Errorf("sqlite journal_mode is %q, expected wal: the DSN pragmas were not applied", journalMode)
	}

	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("sqlite foreign_keys query error: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("sqlite foreign_keys is off: the DSN pragmas were not applied")
	}

	return nil
}

func (s *SQLiteDB) Exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *SQLiteDB) QueryRow(ctx context.Context, query string, args ...any) Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *SQLiteDB) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRows{rows}, nil
}

func (s *SQLiteDB) BeginTx(ctx context.Context) (Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlTx{tx: tx}, nil
}

func (s *SQLiteDB) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

// sqlTx wraps *sql.Tx to implement the Tx interface.
//
// database/sql transactions do not take a context per statement-group the way
// pgx does, so the ctx passed to Commit/Rollback is accepted for interface
// symmetry and deliberately unused: the transaction is already bound to the
// context given to BeginTx, and cancelling that rolls it back.
type sqlTx struct {
	tx *sql.Tx
}

func (t *sqlTx) Exec(ctx context.Context, query string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, query, args...)
	return err
}

func (t *sqlTx) QueryRow(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t *sqlTx) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := t.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRows{rows}, nil
}

func (t *sqlTx) Commit(context.Context) error { return t.tx.Commit() }

// Rollback reports sql.ErrTxDone as success so that the standard
// `defer tx.Rollback(ctx)` guard after a successful Commit is not an error.
func (t *sqlTx) Rollback(context.Context) error {
	if err := t.tx.Rollback(); err != nil && err != sql.ErrTxDone {
		return err
	}
	return nil
}

// sqlRows wraps *sql.Rows to implement the Rows interface.
type sqlRows struct {
	rows *sql.Rows
}

func (r *sqlRows) Next() bool             { return r.rows.Next() }
func (r *sqlRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *sqlRows) Close() error           { return r.rows.Close() }
func (r *sqlRows) Err() error             { return r.rows.Err() }
