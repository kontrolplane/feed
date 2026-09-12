package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kontrolplane/feed/internal/database"
)

// feedIDLength is the fixed width FeedID promises. Callers store it in a TEXT
// primary key, so a change here changes every id in every existing database.
const feedIDLength = 16

func TestFeedIDIsStable(t *testing.T) {
	t.Parallel()

	// The id is a primary key that outlives the process. If it were derived
	// from anything but the url — a counter, a timestamp, a map iteration —
	// every restart would re-subscribe every feed.
	const raw = "https://example.com/feed.xml?feed=rss"

	first := FeedID(raw)
	for i := 0; i < 100; i++ {
		if got := FeedID(raw); got != first {
			t.Fatalf("FeedID(%q) not stable: call %d gave %q, want %q", raw, i, got, first)
		}
	}
}

func TestFeedIDNormalisesEquivalentURLs(t *testing.T) {
	t.Parallel()

	// Each group is one resource spelled several ways. Every member must hash
	// to the same id, or re-importing an OPML file whose urls differ only in
	// case or a trailing slash duplicates every feed.
	groups := map[string][]string{
		"scheme and host case": {
			"http://example.com/feed",
			"HTTP://Example.COM/feed",
			"http://EXAMPLE.com/feed",
		},
		"trailing slash": {
			"https://example.com/feed",
			"https://example.com/feed/",
		},
		"root path": {
			"https://example.com",
			"https://example.com/",
			"HTTPS://EXAMPLE.COM/",
		},
		"fragment": {
			"https://example.com/feed",
			"https://example.com/feed#top",
		},
		"default port": {
			"https://example.com/feed",
			"https://example.com:443/feed",
		},
		"default port http": {
			"http://example.com/feed",
			"http://example.com:80/feed",
		},
		"surrounding whitespace": {
			"https://example.com/feed",
			"  https://example.com/feed\n",
		},
		"combined": {
			"https://example.com/feed",
			" HTTPS://Example.com:443/feed/#recent ",
		},
	}

	for name, urls := range groups {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := FeedID(urls[0])
			for _, u := range urls[1:] {
				if got := FeedID(u); got != want {
					t.Errorf("FeedID(%q) = %q, want %q (same as FeedID(%q))", u, got, want, urls[0])
				}
			}
		})
	}
}

func TestFeedIDDistinguishesDifferentURLs(t *testing.T) {
	t.Parallel()

	// Finding 10 in full: the old scheme was the first 32 bytes of the url with
	// the scheme stripped and '/' replaced by '-', so every url below collapsed
	// to "blog.example.com-very-long-path-t" and UpsertFeed's
	// ON CONFLICT(id) DO UPDATE silently replaced one feed with the next.
	urls := []string{
		"https://blog.example.com/very/long/path/to/the/first/feed.xml",
		"https://blog.example.com/very/long/path/to/the/second/feed.xml",
		"https://blog.example.com/very/long/path/to/the/third/feed.xml",
		// Query strings are load-bearing: plenty of feeds are ?feed=rss, and
		// normalising them away would merge a site's feed with its comments.
		"https://example.com/?feed=rss",
		"https://example.com/?feed=comments-rss",
		"https://example.com/",
		// http and https are different origins, not spellings of one.
		"http://example.com/feed",
		"https://example.com/feed",
		// Sub-paths that share a prefix with a normalised sibling.
		"https://example.com/feed/2",
		"https://example.com/feeds",
	}

	seen := make(map[string]string, len(urls))
	for _, u := range urls {
		id := FeedID(u)
		if other, dup := seen[id]; dup {
			t.Errorf("FeedID collision: %q and %q both produced %q", other, u, id)
			continue
		}
		seen[id] = u
	}
}

func TestFeedIDShape(t *testing.T) {
	t.Parallel()

	// The id goes into a TEXT primary key and into URL paths like /f/{id}, and
	// the old byte-slicing could cut a multi-byte rune in half (finding 13).
	// Hex is fixed-width, valid UTF-8 and needs no escaping anywhere.
	inputs := []string{
		"https://example.com/feed",
		"",
		"not a url at all",
		"https://例え.テスト/フィード",
		"https://example.com/" + strings.Repeat("x", 4096),
		"://malformed",
		"ftp://example.com/feed",
		"  ",
	}

	for _, in := range inputs {
		id := FeedID(in)
		if len(id) != feedIDLength {
			t.Errorf("FeedID(%q) = %q: length %d, want %d", in, id, len(id), feedIDLength)
		}
		if !utf8.ValidString(id) {
			t.Errorf("FeedID(%q) = %q: not valid UTF-8", in, id)
		}
		if strings.Trim(id, "0123456789abcdef") != "" {
			t.Errorf("FeedID(%q) = %q: contains non-hex characters", in, id)
		}
	}
}

func TestFeedIDHandlesUnparseableInput(t *testing.T) {
	t.Parallel()

	// FeedID has no error return — it must always produce an id — so garbage
	// falls back to hashing the trimmed, lower-cased input. That fallback still
	// has to be stable and still has to separate distinct inputs.
	cases := []struct {
		a, b string
		same bool
	}{
		{a: "not a url", b: "NOT A URL", same: true},
		{a: "not a url", b: " not a url ", same: true},
		{a: "not a url", b: "not a url either", same: false},
		{a: "", b: "", same: true},
		{a: "", b: " ", same: true},
	}

	for _, c := range cases {
		got := FeedID(c.a) == FeedID(c.b)
		if got != c.same {
			t.Errorf("FeedID(%q)==FeedID(%q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

func TestPlaceholderRewriting(t *testing.T) {
	t.Parallel()

	// ph is what lets one hand-written query serve both drivers; getting the
	// numbering wrong binds arguments to the wrong columns rather than failing.
	cases := []struct {
		name   string
		driver string
		query  string
		want   string
	}{
		{
			name:   "sqlite passes through",
			driver: "sqlite",
			query:  "SELECT id FROM feeds WHERE url = ? AND folder = ?",
			want:   "SELECT id FROM feeds WHERE url = ? AND folder = ?",
		},
		{
			name:   "postgres numbers sequentially",
			driver: "postgres",
			query:  "SELECT id FROM feeds WHERE url = ? AND folder = ?",
			want:   "SELECT id FROM feeds WHERE url = $1 AND folder = $2",
		},
		{
			name:   "postgres with no placeholders",
			driver: "postgres",
			query:  "SELECT COUNT(*) FROM feeds",
			want:   "SELECT COUNT(*) FROM feeds",
		},
		{
			name:   "postgres past nine placeholders",
			driver: "postgres",
			query:  strings.Repeat("?,", 11),
			want:   "$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := &Store{driver: c.driver}
			if got := s.ph(c.query); got != c.want {
				t.Errorf("ph(%q) on %s = %q, want %q", c.query, c.driver, got, c.want)
			}
		})
	}
}

// ---------- fake database ----------
//
// The store's multi-statement methods are the ones the audit found broken, and
// they cannot be exercised without a database.DB. internal/database exposes no
// constructor that builds one over an in-memory SQLite file, so these tests run
// against a hand-written fake that understands exactly the handful of queries
// this package issues.
//
// The fake applies writes immediately and does not implement rollback, so a
// test of an error path asserts that no statement was issued at all rather than
// that its effects were undone.

type fakeFeed struct {
	id, title, url, folder, siteURL string
}

type execCall struct {
	query string
	args  []any
}

type fakeDB struct {
	feeds      []fakeFeed
	execs      []execCall
	committed  bool
	rolledBack bool
	queryErr   error // forced failure for the lookup queries
}

func scanStrings(vals []string, dest []any) error {
	if len(vals) != len(dest) {
		return fmt.Errorf("fake: scan into %d destinations, have %d values", len(dest), len(vals))
	}
	for i := range dest {
		p, ok := dest[i].(*string)
		if !ok {
			return fmt.Errorf("fake: destination %d is %T, want *string", i, dest[i])
		}
		*p = vals[i]
	}
	return nil
}

type fakeRow struct {
	vals []string
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanStrings(r.vals, dest)
}

type fakeRows struct {
	rows   [][]string
	i      int
	closed bool
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}
func (r *fakeRows) Scan(dest ...any) error { return scanStrings(r.rows[r.i-1], dest) }
func (r *fakeRows) Close() error           { r.closed = true; return nil }
func (r *fakeRows) Err() error             { return nil }

func (d *fakeDB) feedByURL(u string) (fakeFeed, bool) {
	for _, f := range d.feeds {
		if f.url == u {
			return f, true
		}
	}
	return fakeFeed{}, false
}

func (d *fakeDB) feedByID(id string) (fakeFeed, bool) {
	for _, f := range d.feeds {
		if f.id == id {
			return f, true
		}
	}
	return fakeFeed{}, false
}

func (d *fakeDB) QueryRow(_ context.Context, query string, args ...any) database.Row {
	if d.queryErr != nil {
		return fakeRow{err: d.queryErr}
	}
	// UpsertFeed asks two questions: which feed owns a url (MAX(id)), and which
	// folder a given feed id is in (MAX(folder)).
	switch {
	case strings.Contains(query, "MAX(id)") && strings.Contains(query, "WHERE url = ?"):
		if f, ok := d.feedByURL(args[0].(string)); ok {
			return fakeRow{vals: []string{f.id}}
		}
		return fakeRow{vals: []string{""}}
	case strings.Contains(query, "MAX(folder)") && strings.Contains(query, "WHERE id = ?"):
		if f, ok := d.feedByID(args[0].(string)); ok {
			return fakeRow{vals: []string{f.folder}}
		}
		return fakeRow{vals: []string{""}}
	}
	return fakeRow{err: fmt.Errorf("fake: unexpected QueryRow %q", query)}
}

func (d *fakeDB) Query(_ context.Context, query string, args ...any) (database.Rows, error) {
	if strings.Contains(query, "SELECT id FROM feeds WHERE folder=?") {
		var rows [][]string
		for _, f := range d.feeds {
			if f.folder == args[0].(string) {
				rows = append(rows, []string{f.id})
			}
		}
		return &fakeRows{rows: rows}, nil
	}
	return nil, fmt.Errorf("fake: unexpected Query %q", query)
}

func (d *fakeDB) Exec(_ context.Context, query string, args ...any) error {
	d.execs = append(d.execs, execCall{query: query, args: args})

	switch {
	case strings.Contains(query, "INSERT INTO feeds"):
		row := fakeFeed{
			id:      args[0].(string),
			title:   args[1].(string),
			url:     args[2].(string),
			folder:  args[3].(string),
			siteURL: args[4].(string),
		}
		for i, f := range d.feeds {
			if f.id == row.id {
				d.feeds[i] = row
				return nil
			}
		}
		d.feeds = append(d.feeds, row)
	case strings.Contains(query, "UPDATE feeds SET folder = ? WHERE id = ?"):
		for i, f := range d.feeds {
			if f.id == args[1].(string) {
				d.feeds[i].folder = args[0].(string)
			}
		}
	case strings.Contains(query, "DELETE FROM feeds WHERE folder=?"):
		var kept []fakeFeed
		for _, f := range d.feeds {
			if f.folder != args[0].(string) {
				kept = append(kept, f)
			}
		}
		d.feeds = kept
	case strings.Contains(query, "DELETE FROM feeds WHERE id=?"):
		var kept []fakeFeed
		for _, f := range d.feeds {
			if f.id != args[0].(string) {
				kept = append(kept, f)
			}
		}
		d.feeds = kept
	}
	return nil
}

func (d *fakeDB) BeginTx(context.Context) (database.Tx, error) { return &fakeTx{db: d}, nil }
func (d *fakeDB) Ping(context.Context) error                   { return nil }
func (d *fakeDB) Close() error                                 { return nil }

// execsMatching returns the recorded statements whose text contains substr.
func (d *fakeDB) execsMatching(substr string) []execCall {
	var out []execCall
	for _, e := range d.execs {
		if strings.Contains(e.query, substr) {
			out = append(out, e)
		}
	}
	return out
}

type fakeTx struct{ db *fakeDB }

func (t *fakeTx) Exec(ctx context.Context, q string, a ...any) error { return t.db.Exec(ctx, q, a...) }
func (t *fakeTx) QueryRow(ctx context.Context, q string, a ...any) database.Row {
	return t.db.QueryRow(ctx, q, a...)
}
func (t *fakeTx) Query(ctx context.Context, q string, a ...any) (database.Rows, error) {
	return t.db.Query(ctx, q, a...)
}
func (t *fakeTx) Commit(context.Context) error { t.db.committed = true; return nil }
func (t *fakeTx) Rollback(context.Context) error {
	if !t.db.committed {
		t.db.rolledBack = true
	}
	return nil
}

func newTestStore(feeds ...fakeFeed) (*Store, *fakeDB) {
	db := &fakeDB{feeds: feeds}
	return New(db, "sqlite"), db
}

// ---------- UpsertFeed ----------

func TestUpsertFeedAdoptsExistingIDForKnownURL(t *testing.T) {
	t.Parallel()

	// The upgrade path. Every feed in a database written before FeedID existed
	// has an id built from the first 32 bytes of its url, so FeedID(url) is a
	// different string for a feed the user already subscribed to. That must
	// update the stored row in place — not error, and not insert a second row,
	// which would leave every stored item pointing at the abandoned id.
	const feedURL = "https://blog.example.com/very/long/path/to/the/feed.xml"
	legacyID := "blog.example.com-very-long-path-t"

	s, db := newTestStore(fakeFeed{
		id: legacyID, title: "old title", url: feedURL, folder: "default", siteURL: "",
	})

	err := s.UpsertFeed(context.Background(), Feed{
		ID:      FeedID(feedURL),
		Title:   "new title",
		URL:     feedURL,
		Folder:  "default",
		SiteURL: "https://blog.example.com",
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	if errors.Is(err, ErrDuplicateFeedURL) {
		t.Fatal("UpsertFeed returned ErrDuplicateFeedURL for the feed's own url")
	}
	if !db.committed {
		t.Error("transaction was not committed")
	}

	if len(db.feeds) != 1 {
		t.Fatalf("feeds table has %d rows, want 1 (a second row would orphan the items)", len(db.feeds))
	}
	got := db.feeds[0]
	if got.id != legacyID {
		t.Errorf("id = %q, want the stored %q preserved", got.id, legacyID)
	}
	if got.title != "new title" || got.siteURL != "https://blog.example.com" {
		t.Errorf("row not updated in place: %+v", got)
	}

	inserts := db.execsMatching("INSERT INTO feeds")
	if len(inserts) != 1 {
		t.Fatalf("issued %d feed inserts, want 1", len(inserts))
	}
	if id := inserts[0].args[0]; id != legacyID {
		t.Errorf("wrote id %q, want the adopted %q", id, legacyID)
	}
}

func TestUpsertFeedAdoptionMovesItemsWithTheFeed(t *testing.T) {
	t.Parallel()

	// Adoption must carry the items.folder fix with it: the update has to name
	// the *adopted* id, since that is what items.feed_id holds.
	const feedURL = "https://example.com/feed"
	const legacyID = "example.com-feed"

	s, db := newTestStore(fakeFeed{id: legacyID, title: "t", url: feedURL, folder: "old"})

	if err := s.UpsertFeed(context.Background(), Feed{
		ID: FeedID(feedURL), Title: "t", URL: feedURL, Folder: "new",
	}); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	moves := db.execsMatching("UPDATE items SET folder")
	if len(moves) != 1 {
		t.Fatalf("issued %d item folder updates, want 1", len(moves))
	}
	if moves[0].args[0] != "new" || moves[0].args[1] != legacyID {
		t.Errorf("UPDATE items SET folder args = %v, want [new %s]", moves[0].args, legacyID)
	}
}

func TestUpsertFeedGenuineURLConflict(t *testing.T) {
	t.Parallel()

	// Both rows exist and are different feeds: pointing one at the other's url
	// is a real conflict. Adopting here would merge two subscriptions into one
	// and silently drop the edited feed.
	s, db := newTestStore(
		fakeFeed{id: "feed-a", title: "A", url: "https://a.example/feed", folder: "default"},
		fakeFeed{id: "feed-b", title: "B", url: "https://b.example/feed", folder: "default"},
	)

	err := s.UpsertFeed(context.Background(), Feed{
		ID: "feed-a", Title: "A", URL: "https://b.example/feed", Folder: "default",
	})
	if !errors.Is(err, ErrDuplicateFeedURL) {
		t.Fatalf("UpsertFeed error = %v, want ErrDuplicateFeedURL", err)
	}
	if len(db.execs) != 0 {
		t.Errorf("issued %d statements on the conflict path, want 0", len(db.execs))
	}
	if db.committed {
		t.Error("transaction was committed despite the conflict")
	}
	if !db.rolledBack {
		t.Error("transaction was not rolled back")
	}
}

func TestUpsertFeedInsertsNewFeedUnderItsOwnID(t *testing.T) {
	t.Parallel()

	const feedURL = "https://example.com/feed"
	id := FeedID(feedURL)

	s, db := newTestStore()
	if err := s.UpsertFeed(context.Background(), Feed{
		ID: id, Title: "Example", URL: feedURL, Folder: "default",
	}); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	if len(db.feeds) != 1 || db.feeds[0].id != id {
		t.Fatalf("feeds = %+v, want one row with id %q", db.feeds, id)
	}
	// Nothing existed, so there is no stale items.folder to repair.
	if moves := db.execsMatching("UPDATE items SET folder"); len(moves) != 0 {
		t.Errorf("issued %d item folder updates for a brand new feed, want 0", len(moves))
	}
	if !db.committed {
		t.Error("transaction was not committed")
	}
}

func TestUpsertFeedSameIDLeavesItemsAloneWhenFolderUnchanged(t *testing.T) {
	t.Parallel()

	const feedURL = "https://example.com/feed"
	id := FeedID(feedURL)

	s, db := newTestStore(fakeFeed{id: id, title: "old", url: feedURL, folder: "default"})
	if err := s.UpsertFeed(context.Background(), Feed{
		ID: id, Title: "new", URL: feedURL, Folder: "default",
	}); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	if db.feeds[0].title != "new" {
		t.Errorf("title = %q, want %q", db.feeds[0].title, "new")
	}
	if moves := db.execsMatching("UPDATE items SET folder"); len(moves) != 0 {
		t.Errorf("rewrote items.folder for an unchanged folder (%d statements)", len(moves))
	}
}

func TestUpsertFeedPropagatesLookupError(t *testing.T) {
	t.Parallel()

	// A failed lookup must not fall through into the insert: treating a query
	// error as "no row owns this url" is how the original bug behaved.
	s, db := newTestStore()
	db.queryErr = errors.New("connection reset")

	err := s.UpsertFeed(context.Background(), Feed{ID: "x", URL: "https://example.com/feed"})
	if err == nil {
		t.Fatal("UpsertFeed returned nil after a failed lookup")
	}
	if len(db.execs) != 0 {
		t.Errorf("issued %d statements after a failed lookup, want 0", len(db.execs))
	}
}

// ---------- MoveFeed / DeleteFolder ----------

func TestMoveFeedMovesFeedAndItemsTogether(t *testing.T) {
	t.Parallel()

	s, db := newTestStore(fakeFeed{id: "feed-a", url: "https://a.example/feed", folder: "old"})
	if err := s.MoveFeed(context.Background(), "feed-a", "new"); err != nil {
		t.Fatalf("MoveFeed: %v", err)
	}

	if got := db.feeds[0].folder; got != "new" {
		t.Errorf("feeds.folder = %q, want %q", got, "new")
	}
	moves := db.execsMatching("UPDATE items SET folder")
	if len(moves) != 1 {
		t.Fatalf("issued %d item folder updates, want 1", len(moves))
	}
	if moves[0].args[0] != "new" || moves[0].args[1] != "feed-a" {
		t.Errorf("UPDATE items args = %v, want [new feed-a]", moves[0].args)
	}
	if !db.committed {
		t.Error("transaction was not committed")
	}
}

func TestDeleteFolderDeletesItemsByFeedID(t *testing.T) {
	t.Parallel()

	// Deleting items by folder was wrong in both directions. "feed-moved-out"
	// lives in another folder now; its items must survive even if they still
	// carry the old folder string. The two feeds still in "tech" must have
	// their items deleted by feed_id, which catches items whose folder column
	// was never updated.
	s, db := newTestStore(
		fakeFeed{id: "feed-1", url: "https://one.example/feed", folder: "tech"},
		fakeFeed{id: "feed-2", url: "https://two.example/feed", folder: "tech"},
		fakeFeed{id: "feed-moved-out", url: "https://three.example/feed", folder: "news"},
	)

	if err := s.DeleteFolder(context.Background(), "tech"); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}

	if deletes := db.execsMatching("DELETE FROM items WHERE folder"); len(deletes) != 0 {
		t.Errorf("deleted items by folder (%d statements); items must be deleted by feed_id", len(deletes))
	}

	var deletedFeeds []string
	for _, e := range db.execsMatching("DELETE FROM items WHERE feed_id") {
		deletedFeeds = append(deletedFeeds, e.args[0].(string))
	}
	want := []string{"feed-1", "feed-2"}
	if len(deletedFeeds) != len(want) {
		t.Fatalf("deleted items for %v, want %v", deletedFeeds, want)
	}
	for i := range want {
		if deletedFeeds[i] != want[i] {
			t.Errorf("deleted items for %v, want %v", deletedFeeds, want)
			break
		}
	}

	if len(db.feeds) != 1 || db.feeds[0].id != "feed-moved-out" {
		t.Errorf("feeds after delete = %+v, want only feed-moved-out", db.feeds)
	}
	if !db.committed {
		t.Error("transaction was not committed")
	}
}
