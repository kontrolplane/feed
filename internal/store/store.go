package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kontrolplane/feed/internal/database"
)

// ErrDuplicateFeedURL is returned by UpsertFeed when an *existing* feed is
// being pointed at a url that a *different* existing feed already owns — the
// genuine conflict, where merging the two rows would silently discard one.
//
// feeds.url carries a UNIQUE constraint that `ON CONFLICT(id)` does not cover,
// so this case used to surface as a raw driver error from an INSERT that every
// caller discarded. It does *not* cover the merely-different-id case: a feed
// written under a new id but an already-stored url is the same subscription,
// and UpsertFeed adopts the stored id instead of erroring. See UpsertFeed.
var ErrDuplicateFeedURL = errors.New("store: another feed already uses this url")

type Feed struct {
	ID      string
	Title   string
	URL     string
	Folder  string
	SiteURL string
}

type Folder struct {
	ID    string
	Label string
}

type Item struct {
	ID       string
	FeedID   string
	Folder   string
	Title    string
	Authors  string
	Date     string
	Read     bool
	Starred  bool
	Tag      string
	Abstract string
	Body     string
	Link     string
	Minutes  int
}

type Counts struct {
	All       int
	Unread    int
	Read      int
	Starred   int
	Today     int
	Yesterday int
	LastWeek  int
	LastMonth int
	PerFeed   map[string]int
}

type ListFilter struct {
	ViewKind string // "view", "feed", "folder"
	ViewID   string // e.g. "unread", feed id, folder id
	Query    string
	Sort     string // "newest", "oldest", "unread"
}

type Store struct {
	db     database.DB
	driver string // "sqlite" or "postgres"
}

// New creates a new Store wrapping the given database connection.
func New(db database.DB, driver string) *Store {
	return &Store{db: db, driver: driver}
}

// dateLayout is the layout every items.date value is stored in. The column is
// TEXT, so every comparison in this file is a lexicographic string comparison
// that only sorts correctly because the layout is zero-padded and big-endian.
const dateLayout = "2006-01-02"

// FeedID derives a stable, collision-free id for a feed url.
//
// Three call sites used to build the primary key by stripping the scheme,
// replacing '/' with '-' and cutting the result to the first 32 *bytes*. That
// was wrong three ways, all silent:
//
//   - Two feeds on the same host with long paths produced the same id, and
//     UpsertFeed's ON CONFLICT(id) DO UPDATE then replaced the first feed with
//     the second.
//   - A new id whose url already existed violated the feeds.url UNIQUE
//     constraint, which ON CONFLICT(id) does not cover, so the insert failed
//     and every caller dropped the error.
//   - Cutting a byte slice splits multi-byte runes, producing invalid UTF-8.
//
// A truncated hash of the normalised url fixes all three: fixed length,
// URL-safe, deterministic across restarts (so rows written by an earlier run
// keep resolving), and collision-free in practice — 64 bits of SHA-256 give a
// ~50% collision chance only around 5 billion feeds.
//
// Normalisation folds the spellings that denote the same resource — scheme and
// host case, the default port, a trailing slash, a fragment — and deliberately
// keeps the query string, because plenty of feeds live at `?feed=rss`.
func FeedID(rawURL string) string {
	sum := sha256.Sum256([]byte(normaliseFeedURL(rawURL)))
	return hex.EncodeToString(sum[:8]) // 16 hex characters
}

// normaliseFeedURL canonicalises a feed url for hashing. It is deliberately
// conservative: anything it cannot parse is hashed as-is (lower-cased and
// trimmed) rather than rejected, because FeedID must always return an id.
func normaliseFeedURL(rawURL string) string {
	raw := strings.TrimSpace(rawURL)

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(raw)
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""

	// A default port is the same endpoint as no port at all.
	switch {
	case u.Scheme == "http" && strings.HasSuffix(u.Host, ":80"):
		u.Host = strings.TrimSuffix(u.Host, ":80")
	case u.Scheme == "https" && strings.HasSuffix(u.Host, ":443"):
		u.Host = strings.TrimSuffix(u.Host, ":443")
	}

	// "/feed/" and "/feed" are the same document; "/" and "" likewise.
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""

	return u.String()
}

// ph converts SQLite-style ? placeholders to PostgreSQL $N placeholders
// when the driver is postgres.
func (s *Store) ph(query string) string {
	if s.driver != "postgres" {
		return query
	}
	var out strings.Builder
	n := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			out.WriteString(fmt.Sprintf("$%d", n))
			n++
		} else {
			out.WriteByte(query[i])
		}
	}
	return out.String()
}

func (s *Store) UpsertFolder(ctx context.Context, f Folder, pos int) error {
	return s.db.Exec(ctx, s.ph(
		`INSERT INTO folders (id, label, pos) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET label=excluded.label, pos=excluded.pos`),
		f.ID, f.Label, pos)
}

// UpsertFeed inserts or updates a feed.
//
// It runs in a transaction because it has to reconcile two uniqueness rules,
// not one: the id primary key, which ON CONFLICT(id) covers, and the feeds.url
// UNIQUE constraint, which it does not.
//
// A url that is already stored means the same subscription, whatever id the
// caller computed for it. Feed ids are opaque and items.feed_id points at the
// stored one, so the stored row is updated in place under its existing id
// rather than inserted again under the new one. That matters on upgrade: every
// row in a database created before FeedID existed carries the old truncated-url
// id, so FeedID(url) disagrees with the stored id for every feed the user
// already has. Erroring there would skip every feed of a re-imported OPML and
// break editing a feed on the manage page; inserting instead would orphan the
// feed's items, which foreign key enforcement now rejects outright.
//
// ErrDuplicateFeedURL is reserved for the genuine conflict: an existing feed
// being pointed at a url that a different existing feed already owns. Adopting
// there would merge two distinct subscriptions and lose one.
//
// It also keeps items.folder in step with feeds.folder. Moving a feed used to
// update only the feed row, leaving its items filed under the folder they were
// fetched into; DeleteFolder then removed those items while their feed lived
// on somewhere else. See MoveFeed for the same fix on the direct path.
func (s *Store) UpsertFeed(ctx context.Context, f Feed) error {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// No-op once Commit has succeeded; the safety net for every early return.
	defer func() { _ = tx.Rollback(ctx) }()

	// MAX over a filtered set always returns exactly one row on both engines,
	// which avoids depending on a driver-specific "no rows" sentinel
	// (sql.ErrNoRows vs pgx.ErrNoRows) behind the database.Row interface. Both
	// filters match at most one row anyway: id is the primary key and url is
	// UNIQUE.
	var urlOwnerID string
	if err := tx.QueryRow(ctx, s.ph(
		`SELECT COALESCE(MAX(id), '') FROM feeds WHERE url = ?`), f.URL).Scan(&urlOwnerID); err != nil {
		return fmt.Errorf("look up feed by url %q: %w", f.URL, err)
	}

	// feeds.folder is NOT NULL and references folders(id), so a stored row
	// always has a non-empty folder: an empty result here means no row with this
	// id exists, which is what distinguishes a re-id from a real conflict below.
	var idRowFolder string
	if err := tx.QueryRow(ctx, s.ph(
		`SELECT COALESCE(MAX(folder), '') FROM feeds WHERE id = ?`), f.ID).Scan(&idRowFolder); err != nil {
		return fmt.Errorf("look up feed %q: %w", f.ID, err)
	}

	// targetID is the row actually written. It differs from f.ID only when the
	// url is already stored under another id — the legacy-id upgrade path.
	targetID, prevFolder := f.ID, idRowFolder
	if urlOwnerID != "" && urlOwnerID != f.ID {
		if idRowFolder != "" {
			// Two distinct rows exist: one with this id, another already holding
			// this url. That is a rename onto a taken url, not a re-id.
			return fmt.Errorf("%w: %s belongs to feed %q", ErrDuplicateFeedURL, f.URL, urlOwnerID)
		}
		targetID = urlOwnerID
		if err := tx.QueryRow(ctx, s.ph(
			`SELECT COALESCE(MAX(folder), '') FROM feeds WHERE id = ?`), targetID).Scan(&prevFolder); err != nil {
			return fmt.Errorf("look up feed %q: %w", targetID, err)
		}
	}

	if err := tx.Exec(ctx, s.ph(
		`INSERT INTO feeds (id, title, url, folder, site_url) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET title=excluded.title, url=excluded.url, folder=excluded.folder, site_url=excluded.site_url`),
		targetID, f.Title, f.URL, f.Folder, f.SiteURL); err != nil {
		return fmt.Errorf("upsert feed %q: %w", targetID, err)
	}

	if prevFolder != "" && prevFolder != f.Folder {
		if err := tx.Exec(ctx, s.ph(
			`UPDATE items SET folder = ? WHERE feed_id = ?`), f.Folder, targetID); err != nil {
			return fmt.Errorf("move items of feed %q to folder %q: %w", targetID, f.Folder, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit upsert of feed %q: %w", targetID, err)
	}
	return nil
}

// MoveFeed files a feed under a different folder, moving its items with it.
//
// Both rows must change together: items carry a denormalised folder column
// that the item list filters on, and DeleteFolder removes a folder's feeds
// along with their items. A feed whose items still claimed the old folder left
// those items to be deleted out from under it — a foreign key violation on
// PostgreSQL, orphaned rows on SQLite.
func (s *Store) MoveFeed(ctx context.Context, feedID, newFolderID string) error {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := tx.Exec(ctx, s.ph(
		`UPDATE feeds SET folder = ? WHERE id = ?`), newFolderID, feedID); err != nil {
		return fmt.Errorf("move feed %q to folder %q: %w", feedID, newFolderID, err)
	}
	if err := tx.Exec(ctx, s.ph(
		`UPDATE items SET folder = ? WHERE feed_id = ?`), newFolderID, feedID); err != nil {
		return fmt.Errorf("move items of feed %q to folder %q: %w", feedID, newFolderID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit move of feed %q: %w", feedID, err)
	}
	return nil
}

func (s *Store) UpsertItem(ctx context.Context, it Item) error {
	read := 0
	if it.Read {
		read = 1
	}
	starred := 0
	if it.Starred {
		starred = 1
	}
	// folder is intentionally not in the DO UPDATE list: a refetch must not
	// undo a move. MoveFeed/UpsertFeed own that column for existing rows.
	return s.db.Exec(ctx, s.ph(
		`INSERT INTO items (id, feed_id, folder, title, authors, date, read, starred, tag, abstract, body, link, minutes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title=excluded.title, authors=excluded.authors, date=excluded.date,
		   tag=excluded.tag, abstract=excluded.abstract, body=excluded.body,
		   link=excluded.link, minutes=excluded.minutes`),
		it.ID, it.FeedID, it.Folder, it.Title, it.Authors, it.Date,
		read, starred, it.Tag, it.Abstract, it.Body, it.Link, it.Minutes)
}

func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	rows, err := s.db.Query(ctx, "SELECT id, label FROM folders ORDER BY pos")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // the deferred close error is unactionable; rows.Err() is checked below
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Label); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) Feeds(ctx context.Context) ([]Feed, error) {
	rows, err := s.db.Query(ctx, "SELECT id, title, url, folder, site_url FROM feeds ORDER BY title")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Feed
	for rows.Next() {
		var f Feed
		if err := rows.Scan(&f.ID, &f.Title, &f.URL, &f.Folder, &f.SiteURL); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) FeedByID(ctx context.Context, id string) (*Feed, error) {
	var f Feed
	err := s.db.QueryRow(ctx, s.ph("SELECT id, title, url, folder, site_url FROM feeds WHERE id=?"), id).
		Scan(&f.ID, &f.Title, &f.URL, &f.Folder, &f.SiteURL)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Store) ItemByID(ctx context.Context, id string) (*Item, error) {
	var it Item
	var read, starred int
	err := s.db.QueryRow(ctx, s.ph(
		"SELECT id, feed_id, folder, title, authors, date, read, starred, tag, abstract, body, link, minutes FROM items WHERE id=?"), id).
		Scan(&it.ID, &it.FeedID, &it.Folder, &it.Title, &it.Authors, &it.Date, &read, &starred, &it.Tag, &it.Abstract, &it.Body, &it.Link, &it.Minutes)
	if err != nil {
		return nil, err
	}
	it.Read = read == 1
	it.Starred = starred == 1
	return &it, nil
}

func (s *Store) ListItems(ctx context.Context, f ListFilter) ([]Item, error) {
	var where []string
	var args []interface{}

	// Every fragment appended to `where` below is a fixed literal chosen by a
	// closed switch; user input only ever reaches the query as a bound
	// argument. Keep it that way — never interpolate a value into the string.
	switch f.ViewKind {
	case "feed":
		where = append(where, "i.feed_id = ?")
		args = append(args, f.ViewID)
	case "folder":
		where = append(where, "i.folder = ?")
		args = append(args, f.ViewID)
	case "view":
		switch f.ViewID {
		case "unread":
			where = append(where, "i.read = 0")
		case "read":
			where = append(where, "i.read = 1")
		case "starred":
			where = append(where, "i.starred = 1")
		case "today":
			today := time.Now().Format(dateLayout)
			where = append(where, "i.date = ?")
			args = append(args, today)
		case "yesterday":
			yesterday := time.Now().Add(-24 * time.Hour).Format(dateLayout)
			today := time.Now().Format(dateLayout)
			where = append(where, "i.date >= ? AND i.date < ?")
			args = append(args, yesterday, today)
		case "last-week":
			weekAgo := time.Now().Add(-7 * 24 * time.Hour).Format(dateLayout)
			where = append(where, "i.date >= ?")
			args = append(args, weekAgo)
		case "last-month":
			monthAgo := time.Now().Add(-30 * 24 * time.Hour).Format(dateLayout)
			where = append(where, "i.date >= ?")
			args = append(args, monthAgo)
		}
	}

	if f.Query != "" {
		q := "%" + strings.ToLower(f.Query) + "%"
		where = append(where, "(LOWER(i.title) LIKE ? OR LOWER(i.authors) LIKE ? OR LOWER(i.abstract) LIKE ?)")
		args = append(args, q, q, q)
	}

	query := "SELECT i.id, i.feed_id, i.folder, i.title, i.authors, i.date, i.read, i.starred, i.tag, i.abstract, i.body, i.link, i.minutes FROM items i"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	switch f.Sort {
	case "oldest":
		query += " ORDER BY i.date ASC, i.id ASC"
	case "unread":
		query += " ORDER BY i.read ASC, i.date DESC, i.id DESC"
	default:
		query += " ORDER BY i.date DESC, i.id DESC"
	}

	rows, err := s.db.Query(ctx, s.ph(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Item
	for rows.Next() {
		var it Item
		var read, starred int
		if err := rows.Scan(&it.ID, &it.FeedID, &it.Folder, &it.Title, &it.Authors, &it.Date, &read, &starred, &it.Tag, &it.Abstract, &it.Body, &it.Link, &it.Minutes); err != nil {
			return nil, err
		}
		it.Read = read == 1
		it.Starred = starred == 1
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) MarkRead(ctx context.Context, id string, read bool) error {
	v := 0
	if read {
		v = 1
	}
	return s.db.Exec(ctx, s.ph("UPDATE items SET read=? WHERE id=?"), v, id)
}

func (s *Store) ToggleStar(ctx context.Context, id string) error {
	return s.db.Exec(ctx, s.ph("UPDATE items SET starred = 1 - starred WHERE id=?"), id)
}

func (s *Store) ToggleRead(ctx context.Context, id string) error {
	return s.db.Exec(ctx, s.ph("UPDATE items SET read = 1 - read WHERE id=?"), id)
}

func (s *Store) Counts(ctx context.Context) (Counts, error) {
	c := Counts{PerFeed: make(map[string]int)}

	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM items").Scan(&c.All); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM items WHERE read=0").Scan(&c.Unread); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM items WHERE read=1").Scan(&c.Read); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM items WHERE starred=1").Scan(&c.Starred); err != nil {
		return c, err
	}

	today := time.Now().Format(dateLayout)
	yesterday := time.Now().Add(-24 * time.Hour).Format(dateLayout)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).Format(dateLayout)
	monthAgo := time.Now().Add(-30 * 24 * time.Hour).Format(dateLayout)

	if err := s.db.QueryRow(ctx, s.ph("SELECT COUNT(*) FROM items WHERE date = ?"), today).Scan(&c.Today); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(ctx, s.ph("SELECT COUNT(*) FROM items WHERE date >= ? AND date < ?"), yesterday, today).Scan(&c.Yesterday); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(ctx, s.ph("SELECT COUNT(*) FROM items WHERE date >= ?"), weekAgo).Scan(&c.LastWeek); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(ctx, s.ph("SELECT COUNT(*) FROM items WHERE date >= ?"), monthAgo).Scan(&c.LastMonth); err != nil {
		return c, err
	}

	rows, err := s.db.Query(ctx, "SELECT feed_id, COUNT(*) FROM items WHERE read=0 GROUP BY feed_id")
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var fid string
		var n int
		if err := rows.Scan(&fid, &n); err != nil {
			return c, err
		}
		c.PerFeed[fid] = n
	}

	return c, rows.Err()
}

// ItemExists reports whether an item id is already stored.
//
// It returns the query error rather than folding it into `false`: the refresh
// worker uses this to decide whether to write an item, and a database failure
// silently reported as "not present" turns a transient error into a rewritten
// row.
func (s *Store) ItemExists(ctx context.Context, id string) (bool, error) {
	var n int
	if err := s.db.QueryRow(ctx, s.ph("SELECT COUNT(*) FROM items WHERE id=?"), id).Scan(&n); err != nil {
		return false, fmt.Errorf("check item %q: %w", id, err)
	}
	return n > 0, nil
}

func (s *Store) AddFeed(ctx context.Context, f Feed) error {
	return s.UpsertFeed(ctx, f)
}

// DeleteFeed removes a feed and every item belonging to it, atomically. Run as
// two independent statements, a failure between them left the items behind
// pointing at a feed row that no longer existed.
func (s *Store) DeleteFeed(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := tx.Exec(ctx, s.ph("DELETE FROM items WHERE feed_id=?"), id); err != nil {
		return fmt.Errorf("delete items of feed %q: %w", id, err)
	}
	if err := tx.Exec(ctx, s.ph("DELETE FROM feeds WHERE id=?"), id); err != nil {
		return fmt.Errorf("delete feed %q: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete of feed %q: %w", id, err)
	}
	return nil
}

// DeleteFolder removes a folder, the feeds in it and those feeds' items, in
// one transaction.
//
// Items are deleted by feed_id, not by folder. Deleting by folder was wrong in
// both directions: it missed the items of a feed that had been moved into this
// folder but whose items still carried the old folder (leaving rows pointing at
// a deleted feed), and it deleted the items of a feed that had been moved *out*
// of this folder and was therefore about to survive.
func (s *Store) DeleteFolder(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, s.ph("SELECT id FROM feeds WHERE folder=?"), id)
	if err != nil {
		return fmt.Errorf("list feeds in folder %q: %w", id, err)
	}
	var feedIDs []string
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan feed in folder %q: %w", id, err)
		}
		feedIDs = append(feedIDs, fid)
	}
	rowsErr := rows.Err()
	// The rows must be drained and closed before the transaction issues
	// another statement: pgx allows only one active result set per connection.
	if cerr := rows.Close(); cerr != nil && rowsErr == nil {
		rowsErr = cerr
	}
	if rowsErr != nil {
		return fmt.Errorf("list feeds in folder %q: %w", id, rowsErr)
	}

	for _, fid := range feedIDs {
		if err := tx.Exec(ctx, s.ph("DELETE FROM items WHERE feed_id=?"), fid); err != nil {
			return fmt.Errorf("delete items of feed %q: %w", fid, err)
		}
	}
	if err := tx.Exec(ctx, s.ph("DELETE FROM feeds WHERE folder=?"), id); err != nil {
		return fmt.Errorf("delete feeds in folder %q: %w", id, err)
	}
	if err := tx.Exec(ctx, s.ph("DELETE FROM folders WHERE id=?"), id); err != nil {
		return fmt.Errorf("delete folder %q: %w", id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete of folder %q: %w", id, err)
	}
	return nil
}

// PruneReadItems deletes items that were read before olderThan and returns how
// many rows went, so the caller can log the result.
//
// RETENTION has always been configured and displayed but never enforced, and
// every item stores a full extracted article body, so the database grew without
// bound. Starred items are exempt whatever their age: starring is the user
// saying "keep this", and quietly deleting a saved article would be a worse bug
// than the growth this fixes.
//
// items.date is a TEXT column in "2006-01-02" form, so the cutoff is
// day-granular: an item is pruned once its date is strictly before the day
// olderThan falls on.
func (s *Store) PruneReadItems(ctx context.Context, olderThan time.Time) (int64, error) {
	cutoff := olderThan.Format(dateLayout)

	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Counted inside the transaction rather than read from a RowsAffected the
	// database.Executor interface does not expose; the count and the delete
	// therefore see the same snapshot.
	const predicate = `FROM items WHERE read = 1 AND starred = 0 AND date < ?`
	var n int64
	if err := tx.QueryRow(ctx, s.ph(`SELECT COUNT(*) `+predicate), cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("count prunable items before %s: %w", cutoff, err)
	}
	if n == 0 {
		// Nothing to do; committing an empty transaction is still cheaper than
		// issuing a DELETE that matches no rows on every tick.
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit prune: %w", err)
		}
		return 0, nil
	}

	if err := tx.Exec(ctx, s.ph(`DELETE `+predicate), cutoff); err != nil {
		return 0, fmt.Errorf("prune read items before %s: %w", cutoff, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit prune: %w", err)
	}
	return n, nil
}

func (s *Store) FeedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM feeds").Scan(&n)
	return n, err
}

func (s *Store) FolderCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM folders").Scan(&n)
	return n, err
}
