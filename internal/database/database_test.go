package database

import (
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kontrolplane/feed/internal/configuration"
)

func TestPostgresDSN(t *testing.T) {
	t.Parallel()

	base := func(mutate func(*configuration.FeedServiceConfiguration)) configuration.FeedServiceConfiguration {
		cfg := configuration.FeedServiceConfiguration{
			DatabaseUser:     "feed",
			DatabasePassword: "secret",
			DatabaseHost:     "postgres",
			DatabasePort:     "5432",
			DatabaseName:     "kontrolplane",
			DatabaseSslMode:  "disable",
		}
		if mutate != nil {
			mutate(&cfg)
		}
		return cfg
	}

	tests := []struct {
		name string
		cfg  configuration.FeedServiceConfiguration
		want string
	}{
		{
			name: "plain",
			cfg:  base(nil),
			want: "postgres://feed:secret@postgres:5432/kontrolplane?sslmode=disable",
		},
		{
			// The concatenated DSN this replaces parsed the text after the
			// final '@' as the host, so this password silently pointed the
			// client at a different server.
			name: "password with at sign",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabasePassword = "p@ss@word"
			}),
			want: "postgres://feed:p%40ss%40word@postgres:5432/kontrolplane?sslmode=disable",
		},
		{
			name: "password with slash colon and space",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabasePassword = "a/b:c d"
			}),
			want: "postgres://feed:a%2Fb%3Ac%20d@postgres:5432/kontrolplane?sslmode=disable",
		},
		{
			name: "password with question mark and hash",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabasePassword = "wh?at#now"
			}),
			want: "postgres://feed:wh%3Fat%23now@postgres:5432/kontrolplane?sslmode=disable",
		},
		{
			name: "user with special characters",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabaseUser = "svc/feed@prod"
			}),
			want: "postgres://svc%2Ffeed%40prod:secret@postgres:5432/kontrolplane?sslmode=disable",
		},
		{
			name: "ipv6 host is bracketed",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabaseHost = "::1"
			}),
			want: "postgres://feed:secret@[::1]:5432/kontrolplane?sslmode=disable",
		},
		{
			name: "empty port omits the colon",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabasePort = ""
			}),
			want: "postgres://feed:secret@postgres/kontrolplane?sslmode=disable",
		},
		{
			name: "empty sslmode omits the query",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabaseSslMode = ""
			}),
			want: "postgres://feed:secret@postgres:5432/kontrolplane",
		},
		{
			name: "sslmode is escaped",
			cfg: base(func(c *configuration.FeedServiceConfiguration) {
				c.DatabaseSslMode = "verify-full"
			}),
			want: "postgres://feed:secret@postgres:5432/kontrolplane?sslmode=verify-full",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := postgresDSN(tt.cfg)
			if got != tt.want {
				t.Fatalf("postgresDSN() = %q, want %q", got, tt.want)
			}

			// Whatever we produce must survive a round trip through the same
			// parser the driver uses, and come back with the original values.
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q) failed: %v", got, err)
			}
			if u.User.Username() != tt.cfg.DatabaseUser {
				t.Errorf("user round trip = %q, want %q", u.User.Username(), tt.cfg.DatabaseUser)
			}
			if pw, _ := u.User.Password(); pw != tt.cfg.DatabasePassword {
				t.Errorf("password round trip = %q, want %q", pw, tt.cfg.DatabasePassword)
			}
			if u.Hostname() != strings.Trim(tt.cfg.DatabaseHost, "[]") {
				t.Errorf("host round trip = %q, want %q", u.Hostname(), tt.cfg.DatabaseHost)
			}
			if u.Port() != tt.cfg.DatabasePort {
				t.Errorf("port round trip = %q, want %q", u.Port(), tt.cfg.DatabasePort)
			}
			if got, want := strings.TrimPrefix(u.Path, "/"), tt.cfg.DatabaseName; got != want {
				t.Errorf("database round trip = %q, want %q", got, want)
			}
			if got := u.Query().Get("sslmode"); got != tt.cfg.DatabaseSslMode {
				t.Errorf("sslmode round trip = %q, want %q", got, tt.cfg.DatabaseSslMode)
			}
		})
	}
}

// TestSQLiteDSNUsesModerncPragmaSyntax guards the exact bug this file fixes:
// modernc.org/sqlite only honours `_pragma=name(value)` and silently ignores
// mattn-style `_journal_mode=` / `_busy_timeout=` keys.
func TestSQLiteDSNUsesModerncPragmaSyntax(t *testing.T) {
	t.Parallel()

	dsn := sqliteDSN("/var/lib/feed/feed.db")

	path, query, found := strings.Cut(dsn, "?")
	if !found {
		t.Fatalf("sqliteDSN() = %q, expected a query string", dsn)
	}
	if path != "/var/lib/feed/feed.db" {
		t.Fatalf("sqliteDSN() path = %q, want the database path unchanged", path)
	}

	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("url.ParseQuery(%q) failed: %v", query, err)
	}

	pragmas := values["_pragma"]
	want := []string{
		"journal_mode(WAL)",
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
	}
	for _, w := range want {
		if !contains(pragmas, w) {
			t.Errorf("missing _pragma=%s in %q", w, dsn)
		}
	}

	for _, ignored := range []string{"_journal_mode", "_busy_timeout", "_foreign_keys"} {
		if values.Has(ignored) {
			t.Errorf("DSN uses %s, which modernc.org/sqlite silently ignores: %q", ignored, dsn)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestSQLitePragmasAreApplied opens a real database file and asks SQLite what
// it actually did. This is the test that proves the DSN fix works rather than
// merely looks right: with the old mattn-style DSN, journal_mode comes back
// "delete" and busy_timeout comes back 0.
func TestSQLitePragmasAreApplied(t *testing.T) {
	t.Parallel()

	db := openTestSQLite(t)
	ctx := t.Context()

	var journalMode string
	if err := db.QueryRow(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := db.QueryRow(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := db.QueryRow(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	var synchronous int
	if err := db.QueryRow(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("PRAGMA synchronous: %v", err)
	}
	if synchronous != 1 { // 1 == NORMAL
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}

// TestSQLiteForeignKeysAreEnforced documents the behaviour change that comes
// with foreign_keys(1): a feed can no longer reference a folder that does not
// exist. The OPML importer used to rely on this silently not being checked.
func TestSQLiteForeignKeysAreEnforced(t *testing.T) {
	t.Parallel()

	db := openTestSQLite(t)
	ctx := t.Context()

	err := db.Exec(ctx,
		"INSERT INTO feeds (id, title, url, folder, site_url) VALUES (?, ?, ?, ?, ?)",
		"f1", "orphan", "https://example.com/feed", "no-such-folder", "")
	if err == nil {
		t.Fatal("insert with a dangling folder reference succeeded, foreign keys are not enforced")
	}

	if err := db.Exec(ctx, "INSERT INTO folders (id, label, pos) VALUES (?, ?, ?)", "tech", "tech", 0); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if err := db.Exec(ctx,
		"INSERT INTO feeds (id, title, url, folder, site_url) VALUES (?, ?, ?, ?, ?)",
		"f1", "ok", "https://example.com/feed", "tech", ""); err != nil {
		t.Fatalf("insert feed with an existing folder: %v", err)
	}
}

// TestSQLiteTransactionRollback covers the Tx half of finding 12: a failed
// multi-statement operation must leave nothing behind.
func TestSQLiteTransactionRollback(t *testing.T) {
	t.Parallel()

	db := openTestSQLite(t)
	ctx := t.Context()

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := tx.Exec(ctx, "INSERT INTO folders (id, label, pos) VALUES (?, ?, ?)", "tech", "tech", 0); err != nil {
		t.Fatalf("Exec in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	var n int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM folders").Scan(&n); err != nil {
		t.Fatalf("count folders: %v", err)
	}
	if n != 0 {
		t.Fatalf("after rollback folders = %d, want 0", n)
	}

	// A second Rollback, as produced by `defer tx.Rollback(ctx)`, must not be
	// reported as an error.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}

	tx, err = db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("deferred Rollback after Commit: %v", err)
		}
	}()
	if err := tx.Exec(ctx, "INSERT INTO folders (id, label, pos) VALUES (?, ?, ?)", "news", "news", 1); err != nil {
		t.Fatalf("Exec in tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM folders").Scan(&n); err != nil {
		t.Fatalf("count folders: %v", err)
	}
	if n != 1 {
		t.Fatalf("after commit folders = %d, want 1", n)
	}
}

// TestMigrationsApplyOnSQLite runs every migration against a fresh file, which
// is what proves 002_indexes.sql is syntactically valid and that goose picks
// it up as version 2.
func TestMigrationsApplyOnSQLite(t *testing.T) {
	db := openTestSQLite(t)
	ctx := t.Context()

	wantIndexes := []string{
		"idx_items_folder_date",
		"idx_items_feed_date",
		"idx_items_starred_date",
		"idx_items_unread_feed",
		"idx_feeds_folder",
	}
	for _, name := range wantIndexes {
		var found string
		err := db.QueryRow(ctx, "SELECT name FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&found)
		if err != nil {
			t.Errorf("index %s missing after migration: %v", name, err)
		}
	}
}

// openTestSQLite creates a migrated SQLite database in a temp directory using
// the real pool constructor, so the pragmas under test are the production ones.
func openTestSQLite(t *testing.T) DB {
	t.Helper()

	cfg := configuration.FeedServiceConfiguration{
		DatabaseDriver: "sqlite",
		DatabasePath:   filepath.Join(t.TempDir(), "feed.db"),
	}
	logger := newTestLogger(t)
	ctx := t.Context()

	if err := Migrate(ctx, cfg, logger); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	db, err := CreatePool(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// newTestLogger discards output; the tests assert on behaviour, not on logs.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
