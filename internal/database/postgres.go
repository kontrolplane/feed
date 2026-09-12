package database

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kontrolplane/feed/internal/configuration"
)

// Connection pool limits. These were previously left at the pgxpool defaults
// (MaxConns = max(4, NumCPU), everything else zero), which means the limits
// varied with the machine the container happened to land on and connections
// were held open indefinitely — including through a Postgres failover, after
// which every pooled connection is dead but still handed out.
const (
	// Comfortably above the handful of concurrent HTTP handlers plus the
	// single refresh worker, and low enough that several replicas still fit
	// inside a default max_connections of 100.
	postgresMaxConns = 10
	// Keep a couple warm so a request after an idle period does not pay for a
	// TCP handshake, TLS handshake and authentication round trip.
	postgresMinConns = 2
	// Recycle connections regularly so a failover or a rolling restart of the
	// database is noticed within an hour rather than never.
	postgresMaxConnLifetime = time.Hour
	postgresMaxConnIdleTime = 30 * time.Minute
	// Prunes connections a server-side idle timeout or a network drop killed
	// without telling us.
	postgresHealthCheckPeriod = time.Minute
)

// postgresDSN builds the PostgreSQL connection URL from configuration.
//
// This used to be fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", ...)
// in two places. Concatenating the credentials into a URL means a password
// containing '@', '/', ':', '?', '#' or a space produces a URL that either
// fails to parse or, worse, parses into a different host — so a perfectly
// legal generated password made the service unable to connect. url.URL
// percent-encodes the userinfo and the query for us.
func postgresDSN(cfg configuration.FeedServiceConfiguration) string {
	host := cfg.DatabaseHost
	if cfg.DatabasePort != "" {
		host = net.JoinHostPort(cfg.DatabaseHost, cfg.DatabasePort)
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.DatabaseUser, cfg.DatabasePassword),
		Host:   host,
		Path:   "/" + cfg.DatabaseName,
	}

	if cfg.DatabaseSslMode != "" {
		u.RawQuery = url.Values{"sslmode": {cfg.DatabaseSslMode}}.Encode()
	}

	return u.String()
}

// PostgresDB wraps a *pgxpool.Pool to implement the DB interface.
type PostgresDB struct {
	pool *pgxpool.Pool
}

func createPostgresPool(ctx context.Context, cfg configuration.FeedServiceConfiguration, logger *slog.Logger) (*PostgresDB, error) {
	poolConfig, err := pgxpool.ParseConfig(postgresDSN(cfg))
	if err != nil {
		logger.Error("failed to parse database configuration", slog.Any("error", err))
		return nil, fmt.Errorf("failed to parse database configuration: %w", err)
	}

	poolConfig.MaxConns = postgresMaxConns
	poolConfig.MinConns = postgresMinConns
	poolConfig.MaxConnLifetime = postgresMaxConnLifetime
	poolConfig.MaxConnIdleTime = postgresMaxConnIdleTime
	poolConfig.HealthCheckPeriod = postgresHealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		logger.Error("failed to create a database pool", slog.Any("error", err))
		return nil, fmt.Errorf("failed to create database pool: %w", err)
	}

	if err := validatePostgresPool(ctx, pool); err != nil {
		logger.Error("failed to validate database pool", slog.Any("error", err))
		pool.Close()
		return nil, fmt.Errorf("failed to validate database pool: %w", err)
	}

	return &PostgresDB{pool: pool}, nil
}

// validatePostgresPool checks the pool can both connect and execute. Ping only
// proves a connection can be acquired, so a trivial round trip follows it.
func validatePostgresPool(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database connection error: %w", err)
	}

	var ok int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&ok); err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("no rows were returned")
		}
		return fmt.Errorf("database query error: %w", err)
	}

	return nil
}

func (p *PostgresDB) Exec(ctx context.Context, query string, args ...any) error {
	_, err := p.pool.Exec(ctx, query, args...)
	return err
}

func (p *PostgresDB) QueryRow(ctx context.Context, query string, args ...any) Row {
	return p.pool.QueryRow(ctx, query, args...)
}

func (p *PostgresDB) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows}, nil
}

func (p *PostgresDB) BeginTx(ctx context.Context) (Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx}, nil
}

func (p *PostgresDB) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

func (p *PostgresDB) Close() error {
	p.pool.Close()
	return nil
}

// pgxTx wraps pgx.Tx to implement the Tx interface.
type pgxTx struct {
	tx pgx.Tx
}

func (t *pgxTx) Exec(ctx context.Context, query string, args ...any) error {
	_, err := t.tx.Exec(ctx, query, args...)
	return err
}

func (t *pgxTx) QueryRow(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRow(ctx, query, args...)
}

func (t *pgxTx) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := t.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows}, nil
}

func (t *pgxTx) Commit(ctx context.Context) error { return t.tx.Commit(ctx) }

// Rollback reports pgx.ErrTxClosed as success so that the standard
// `defer tx.Rollback(ctx)` guard after a successful Commit is not an error.
func (t *pgxTx) Rollback(ctx context.Context) error {
	if err := t.tx.Rollback(ctx); err != nil && err != pgx.ErrTxClosed {
		return err
	}
	return nil
}

// pgxRows wraps pgx.Rows to implement the Rows interface.
type pgxRows struct {
	rows pgx.Rows
}

func (r *pgxRows) Next() bool             { return r.rows.Next() }
func (r *pgxRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *pgxRows) Close() error           { r.rows.Close(); return nil }
func (r *pgxRows) Err() error             { return r.rows.Err() }
