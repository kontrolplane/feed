package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/kontrolplane/feed/internal/configuration"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// goose's top-level API is built on package-level globals — SetLogger,
// SetBaseFS and SetDialect all mutate process-wide state, and
// EnsureDBVersionContext/UpContext then read it. Migrate set all three on
// every call, so two goroutines migrating at once race on the dialect and
// can run migrations against the wrong SQL flavour.
//
// In production this is latent (one call at boot), but it is a real race,
// and the detector flags it the moment two tests each open their own temp
// database. sync.Once is not the fix: the dialect legitimately differs
// between sqlite and postgres, so it cannot be set only once. Serialising
// the whole configure-then-run sequence is what the global API requires —
// and it costs nothing, because migrations run once per process and are
// strictly sequential anyway.
var gooseMu sync.Mutex

// Migrate runs all pending database migrations using goose.
func Migrate(ctx context.Context, cfg configuration.FeedServiceConfiguration, logger *slog.Logger) error {
	var dialect string
	var dsn string

	switch cfg.DatabaseDriver {
	case "sqlite":
		dialect = "sqlite"
		// Same DSN builder as the application pool. Migrations open their own
		// connection to the same file, so they need the same pragmas — in
		// particular busy_timeout, since the worker may already be writing.
		dsn = sqliteDSN(cfg.DatabasePath)
	case "postgres":
		dialect = "pgx"
		dsn = postgresDSN(cfg)
	default:
		return fmt.Errorf("unsupported database driver for migration: %s", cfg.DatabaseDriver)
	}

	db, err := sql.Open(dialect, dsn)
	if err != nil {
		return fmt.Errorf("failed to open database for migration: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("failed to close migration database connection", slog.Any("error", err))
		}
	}()

	// Migrations are strictly sequential, and for SQLite a second writer
	// against the same file is exactly the contention we are trying to remove.
	db.SetMaxOpenConns(1)

	// Held across the reads as well as the writes: the dialect set here must
	// still be in force when UpContext consults it below.
	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations)

	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}

	if _, err := goose.EnsureDBVersionContext(ctx, db); err != nil {
		return fmt.Errorf("failed to ensure goose version table: %w", err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		logger.Error("failed to run migrations", slog.Any("error", err))
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	version, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		// Not fatal: the migrations themselves succeeded. Report it rather
		// than swallowing it, because a failure here means the version table
		// is unreadable and the next boot may behave surprisingly.
		logger.Error("migrations applied but database version could not be read", slog.Any("error", err))
		return nil
	}
	logger.Info("database migrated", slog.Int64("version", version))

	return nil
}
