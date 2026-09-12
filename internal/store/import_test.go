package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kontrolplane/feed/internal/database"
)

// ---------- parser ----------

func TestParseOPML(t *testing.T) {
	t.Parallel()

	type want struct {
		folderID string
		title    string
		url      string
	}

	tests := []struct {
		name    string
		doc     string
		wantErr bool
		// skipped is the number of entries rejected during parsing.
		skipped int
		feeds   []want
	}{
		{
			name: "flat, no folders",
			doc: `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <head><title>subs</title></head>
  <body>
    <outline text="One" type="rss" xmlUrl="https://one.example/feed.xml" htmlUrl="https://one.example/"/>
    <outline text="Two" type="rss" xmlUrl="https://two.example/feed.xml"/>
  </body>
</opml>`,
			feeds: []want{
				{DefaultFolderID, "One", "https://one.example/feed.xml"},
				{DefaultFolderID, "Two", "https://two.example/feed.xml"},
			},
		},
		{
			name: "one level of folders",
			doc: `<opml version="2.0"><body>
    <outline text="Tech News">
      <outline text="One" xmlUrl="https://one.example/feed.xml"/>
    </outline>
    <outline text="Loose" xmlUrl="https://loose.example/feed.xml"/>
  </body></opml>`,
			feeds: []want{
				{"tech-news", "One", "https://one.example/feed.xml"},
				{DefaultFolderID, "Loose", "https://loose.example/feed.xml"},
			},
		},
		{
			name: "doubly nested folders flatten to the innermost group",
			doc: `<opml version="2.0"><body>
    <outline text="Outer">
      <outline text="Inner">
        <outline text="Deep" xmlUrl="https://deep.example/feed.xml"/>
      </outline>
      <outline text="Shallow" xmlUrl="https://shallow.example/feed.xml"/>
    </outline>
  </body></opml>`,
			feeds: []want{
				{"inner", "Deep", "https://deep.example/feed.xml"},
				{"outer", "Shallow", "https://shallow.example/feed.xml"},
			},
		},
		{
			name: "title attribute wins over text",
			doc: `<opml version="2.0"><body>
    <outline text="from text" title="from title" xmlUrl="https://a.example/feed.xml"/>
  </body></opml>`,
			feeds: []want{{DefaultFolderID, "from title", "https://a.example/feed.xml"}},
		},
		{
			name: "text is used when title is absent",
			doc: `<opml version="2.0"><body>
    <outline text="from text" xmlUrl="https://a.example/feed.xml"/>
  </body></opml>`,
			feeds: []want{{DefaultFolderID, "from text", "https://a.example/feed.xml"}},
		},
		{
			name: "url is used when both title and text are absent",
			doc: `<opml version="2.0"><body>
    <outline xmlUrl="https://a.example/feed.xml"/>
  </body></opml>`,
			feeds: []want{{DefaultFolderID, "https://a.example/feed.xml", "https://a.example/feed.xml"}},
		},
		{
			name: "folder groups prefer text for their label",
			doc: `<opml version="2.0"><body>
    <outline title="Title Label" text="Text Label">
      <outline text="One" xmlUrl="https://one.example/feed.xml"/>
    </outline>
  </body></opml>`,
			feeds: []want{{"text-label", "One", "https://one.example/feed.xml"}},
		},
		{
			name: "file:// entries are skipped, not stored",
			doc: `<opml version="2.0"><body>
    <outline text="evil" xmlUrl="file:///etc/passwd"/>
    <outline text="good" xmlUrl="https://good.example/feed.xml"/>
  </body></opml>`,
			skipped: 1,
			feeds:   []want{{DefaultFolderID, "good", "https://good.example/feed.xml"}},
		},
		{
			name: "javascript: entries are skipped",
			doc: `<opml version="2.0"><body>
    <outline text="evil" xmlUrl="javascript:alert(1)"/>
  </body></opml>`,
			skipped: 1,
		},
		{
			name: "urls with embedded credentials are skipped",
			doc: `<opml version="2.0"><body>
    <outline text="evil" xmlUrl="https://user:pass@evil.example/feed.xml"/>
  </body></opml>`,
			skipped: 1,
		},
		{
			name: "empty body imports nothing",
			doc:  `<opml version="2.0"><head><title>none</title></head><body></body></opml>`,
		},
		{
			name: "outlines outside body are ignored",
			doc: `<opml version="2.0">
    <head><title>x</title></head>
    <body><outline text="One" xmlUrl="https://one.example/feed.xml"/></body>
  </opml>`,
			feeds: []want{{DefaultFolderID, "One", "https://one.example/feed.xml"}},
		},
		{
			name: "attribute names are matched case-insensitively",
			doc: `<opml version="2.0"><body>
    <outline TEXT="One" XMLURL="https://one.example/feed.xml"/>
  </body></opml>`,
			feeds: []want{{DefaultFolderID, "One", "https://one.example/feed.xml"}},
		},
		{
			name:    "malformed xml is an error, not a panic",
			doc:     `<opml version="2.0"><body><outline text="One" xmlUrl="https://a.example/"</body>`,
			wantErr: true,
		},
		{
			name:    "unclosed root is an error",
			doc:     `<opml><body><outline text="One" xmlUrl="https://a.example/feed.xml"/>`,
			wantErr: true,
		},
		{
			name:    "empty file is an error",
			doc:     ``,
			wantErr: true,
		},
		{
			name:    "whitespace-only file is an error",
			doc:     "   \n\t ",
			wantErr: true,
		},
		{
			name:    "a non-opml document is an error",
			doc:     `<html><body><outline xmlUrl="https://a.example/feed.xml"/></body></html>`,
			wantErr: true,
		},
		{
			name:    "an unsupported charset declaration is an error",
			doc:     `<?xml version="1.0" encoding="Shift_JIS"?><opml><body></body></opml>`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan, err := parseOPML(strings.NewReader(tc.doc))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got plan with %d feeds", len(plan.Feeds))
				}
				if !errors.Is(err, ErrInvalidOPML) {
					t.Fatalf("error %v does not wrap ErrInvalidOPML", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if plan.Skipped != tc.skipped {
				t.Errorf("skipped = %d, want %d (errors: %v)", plan.Skipped, tc.skipped, plan.Errors)
			}
			if len(plan.Errors) != tc.skipped {
				t.Errorf("recorded errors = %d, want %d", len(plan.Errors), tc.skipped)
			}
			if len(plan.Feeds) != len(tc.feeds) {
				t.Fatalf("got %d feeds, want %d: %+v", len(plan.Feeds), len(tc.feeds), plan.Feeds)
			}
			for i, w := range tc.feeds {
				got := plan.Feeds[i]
				if got.FolderID != w.folderID {
					t.Errorf("feed %d: folder = %q, want %q", i, got.FolderID, w.folderID)
				}
				if got.Feed.Title != w.title {
					t.Errorf("feed %d: title = %q, want %q", i, got.Feed.Title, w.title)
				}
				if got.Feed.URL != w.url {
					t.Errorf("feed %d: url = %q, want %q", i, got.Feed.URL, w.url)
				}
				if got.Feed.Folder != got.FolderID {
					t.Errorf("feed %d: Feed.Folder %q disagrees with plan folder %q", i, got.Feed.Folder, got.FolderID)
				}
				if got.Feed.ID != FeedID(w.url) {
					t.Errorf("feed %d: id = %q, want FeedID(%q) = %q", i, got.Feed.ID, w.url, FeedID(w.url))
				}
			}
		})
	}
}

// TestParseOPMLDepthGuard makes sure pathological nesting is bounded rather
// than followed to the bottom.
func TestParseOPMLDepthGuard(t *testing.T) {
	t.Parallel()

	const depth = maxOutlineDepth + 5
	var b strings.Builder
	b.WriteString(`<opml version="2.0"><body>`)
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, `<outline text="level%d">`, i)
	}
	b.WriteString(`<outline text="buried" xmlUrl="https://buried.example/feed.xml"/>`)
	for i := 0; i < depth; i++ {
		b.WriteString(`</outline>`)
	}
	b.WriteString(`</body></opml>`)

	plan, err := parseOPML(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Feeds) != 0 {
		t.Errorf("feeds below the depth limit were imported: %+v", plan.Feeds)
	}
	if plan.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", plan.Skipped)
	}
	if len(plan.Errors) != 1 || !errors.Is(plan.Errors[0].Err, errTooDeep) {
		t.Errorf("errors = %v, want one errTooDeep", plan.Errors)
	}
}

// TestParseOPMLLatin1 covers the encoding older readers still export.
func TestParseOPMLLatin1(t *testing.T) {
	t.Parallel()

	// 0xE9 is "é" in ISO-8859-1 and invalid UTF-8 on its own.
	doc := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><opml version=\"2.0\"><body>" +
		"<outline text=\"caf\xe9\" xmlUrl=\"https://cafe.example/feed.xml\"/>" +
		"</body></opml>"

	plan, err := parseOPML(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Feeds) != 1 {
		t.Fatalf("got %d feeds, want 1", len(plan.Feeds))
	}
	if plan.Feeds[0].Feed.Title != "café" {
		t.Errorf("title = %q, want %q", plan.Feeds[0].Feed.Title, "café")
	}
}

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"Tech News", "tech-news"},
		{"  Tech   News  ", "tech-news"},
		{"News/Politics", "news-politics"},
		{"UPPER", "upper"},
		{"...", ""},
		{"", ""},
		{"Nachrichten über alles", "nachrichten-über-alles"},
		{strings.Repeat("a", maxFolderIDRunes+10), strings.Repeat("a", maxFolderIDRunes)},
	}
	for _, tc := range tests {
		if got := slugify(tc.in); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncRunesDoesNotSplitRunes(t *testing.T) {
	t.Parallel()

	if got := truncRunes("ééé", 2); got != "éé" {
		t.Errorf("truncRunes = %q, want %q", got, "éé")
	}
	if got := truncRunes("abc", 10); got != "abc" {
		t.Errorf("truncRunes = %q, want %q", got, "abc")
	}
}

// ---------- persistence ----------

// TestImportOPMLCreatesDefaultFolder is the regression test for the bug where
// an import reported "imported N feeds" while every insert was rejected by the
// foreign key on feeds.folder, because the "default" folder was referenced but
// never created.
func TestImportOPMLCreatesDefaultFolder(t *testing.T) {
	t.Parallel()

	db := newOPMLFakeDB()
	s := New(db, "sqlite")

	doc := `<opml version="2.0"><body>
    <outline text="One" xmlUrl="https://one.example/feed.xml"/>
    <outline text="Two" xmlUrl="https://two.example/feed.xml"/>
  </body></opml>`

	res, err := ImportOPML(context.Background(), s, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !db.hasFolder(DefaultFolderID) {
		t.Fatalf("the default folder was never created; feeds: %d", len(db.feeds))
	}
	if res.FoldersCreated != 1 {
		t.Errorf("FoldersCreated = %d, want 1", res.FoldersCreated)
	}
	if res.FeedsAdded != 2 {
		t.Errorf("FeedsAdded = %d, want 2 (errors: %v)", res.FeedsAdded, res.Errors)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0: %v", res.Skipped, res.Errors)
	}
	if len(db.feeds) != 2 {
		t.Errorf("%d feeds stored, want 2", len(db.feeds))
	}
	// The folder row must be written before the feed that references it.
	if got := db.firstIndexOf("INSERT INTO folders"); got != 0 {
		t.Errorf("the folder insert was statement %d, want it first: %v", got, db.execs)
	}
}

// TestImportOPMLFileCreatesDefaultFolder covers the other historical path: the
// startup file import. Both paths now run the same code, which is the point.
func TestImportOPMLFileCreatesDefaultFolder(t *testing.T) {
	t.Parallel()

	db := newOPMLFakeDB()
	s := New(db, "sqlite")

	path := t.TempDir() + "/feeds.opml"
	doc := `<opml version="2.0"><body>
    <outline text="One" xmlUrl="https://one.example/feed.xml"/>
  </body></opml>`
	if err := writeFileForTest(path, doc); err != nil {
		t.Fatal(err)
	}

	n, err := ImportOPMLFile(context.Background(), s, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("imported = %d, want 1", n)
	}
	if !db.hasFolder(DefaultFolderID) {
		t.Error("the default folder was never created")
	}
}

// TestImportOPMLCreatesNestedFolders checks folders are created lazily: only
// when something actually goes in them.
func TestImportOPMLCreatesNestedFolders(t *testing.T) {
	t.Parallel()

	db := newOPMLFakeDB()
	s := New(db, "sqlite")

	doc := `<opml version="2.0"><body>
    <outline text="Tech News">
      <outline text="One" xmlUrl="https://one.example/feed.xml"/>
      <outline text="Two" xmlUrl="https://two.example/feed.xml"/>
    </outline>
    <outline text="Empty Folder"/>
  </body></opml>`

	res, err := ImportOPML(context.Background(), s, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FoldersCreated != 1 || !db.hasFolder("tech-news") {
		t.Errorf("FoldersCreated = %d, folders = %v", res.FoldersCreated, db.folderIDs())
	}
	if db.hasFolder("empty-folder") {
		t.Error("a folder with no feeds in it was created")
	}
	if res.FeedsAdded != 2 {
		t.Errorf("FeedsAdded = %d, want 2", res.FeedsAdded)
	}
}

// TestImportOPMLCountsUpdates asserts the number reported back distinguishes
// new subscriptions from refreshed ones.
func TestImportOPMLCountsUpdates(t *testing.T) {
	t.Parallel()

	db := newOPMLFakeDB()
	db.folders = append(db.folders, Folder{ID: DefaultFolderID, Label: DefaultFolderID})
	db.feeds = append(db.feeds, Feed{
		ID:     FeedID("https://one.example/feed.xml"),
		Title:  "old title",
		URL:    "https://one.example/feed.xml",
		Folder: DefaultFolderID,
	})
	s := New(db, "sqlite")

	doc := `<opml version="2.0"><body>
    <outline text="One (renamed)" xmlUrl="https://one.example/feed.xml"/>
    <outline text="Two" xmlUrl="https://two.example/feed.xml"/>
  </body></opml>`

	res, err := ImportOPML(context.Background(), s, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FeedsAdded != 1 {
		t.Errorf("FeedsAdded = %d, want 1", res.FeedsAdded)
	}
	if res.FeedsUpdated != 1 {
		t.Errorf("FeedsUpdated = %d, want 1", res.FeedsUpdated)
	}
	if res.FoldersCreated != 0 {
		t.Errorf("FoldersCreated = %d, want 0, the folder already existed", res.FoldersCreated)
	}
}

// TestImportOPMLReportsWriteFailures is the other half of the lie the old code
// told: a failed insert must not be counted as an import.
func TestImportOPMLReportsWriteFailures(t *testing.T) {
	t.Parallel()

	db := newOPMLFakeDB()
	db.failFeed = FeedID("https://two.example/feed.xml")
	s := New(db, "sqlite")

	doc := `<opml version="2.0"><body>
    <outline text="One" xmlUrl="https://one.example/feed.xml"/>
    <outline text="Two" xmlUrl="https://two.example/feed.xml"/>
    <outline text="Bad" xmlUrl="file:///etc/passwd"/>
  </body></opml>`

	res, err := ImportOPML(context.Background(), s, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FeedsAdded != 1 {
		t.Errorf("FeedsAdded = %d, want 1", res.FeedsAdded)
	}
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (one unsafe url, one failed write)", res.Skipped)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("Errors = %v, want 2 entries", res.Errors)
	}
	if !strings.Contains(res.Summary(), "skipped") {
		t.Errorf("summary %q does not mention the skipped entries", res.Summary())
	}
}

// TestImportOPMLSkipsFeedWhenFolderFails makes sure we do not follow a failed
// folder write with a feed insert that the foreign key is certain to reject.
func TestImportOPMLSkipsFeedWhenFolderFails(t *testing.T) {
	t.Parallel()

	db := newOPMLFakeDB()
	db.failFolder = DefaultFolderID
	s := New(db, "sqlite")

	doc := `<opml version="2.0"><body>
    <outline text="One" xmlUrl="https://one.example/feed.xml"/>
  </body></opml>`

	res, err := ImportOPML(context.Background(), s, strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.FeedsAdded != 0 || res.Skipped != 1 {
		t.Errorf("FeedsAdded = %d, Skipped = %d, want 0 and 1", res.FeedsAdded, res.Skipped)
	}
	if len(db.feeds) != 0 {
		t.Errorf("a feed was inserted without its folder: %+v", db.feeds)
	}
}

// ---------- export ----------

func TestExportOPMLRoundTrip(t *testing.T) {
	t.Parallel()

	folders := []Folder{
		{ID: "tech-news", Label: "Tech News"},
		{ID: DefaultFolderID, Label: DefaultFolderID},
	}
	feeds := []Feed{
		{ID: FeedID("https://one.example/feed.xml"), Title: `Quotes "and" <angles>`,
			URL: "https://one.example/feed.xml", Folder: "tech-news", SiteURL: "https://one.example/"},
		{ID: FeedID("https://two.example/feed.xml"), Title: "Two",
			URL: "https://two.example/feed.xml", Folder: DefaultFolderID},
	}

	doc, err := ExportOPML(folders, feeds)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.HasPrefix(string(doc), "<?xml") {
		t.Errorf("export is missing the xml declaration: %q", string(doc[:20]))
	}
	if strings.Contains(string(doc), `text="Quotes "and"`) {
		t.Error("attribute values are not escaped")
	}

	plan, err := parseOPML(strings.NewReader(string(doc)))
	if err != nil {
		t.Fatalf("re-parsing our own export failed: %v", err)
	}
	if plan.Skipped != 0 {
		t.Errorf("round trip skipped %d entries: %v", plan.Skipped, plan.Errors)
	}
	if len(plan.Feeds) != len(feeds) {
		t.Fatalf("round trip produced %d feeds, want %d", len(plan.Feeds), len(feeds))
	}

	got := map[string]plannedFeed{}
	for _, pf := range plan.Feeds {
		got[pf.Feed.URL] = pf
	}
	for _, want := range feeds {
		pf, ok := got[want.URL]
		if !ok {
			t.Errorf("feed %q did not survive the round trip", want.URL)
			continue
		}
		if pf.Feed.ID != want.ID {
			t.Errorf("feed %q: id = %q, want %q", want.URL, pf.Feed.ID, want.ID)
		}
		if pf.Feed.Title != want.Title {
			t.Errorf("feed %q: title = %q, want %q", want.URL, pf.Feed.Title, want.Title)
		}
		if pf.FolderID != want.Folder {
			t.Errorf("feed %q: folder = %q, want %q", want.URL, pf.FolderID, want.Folder)
		}
		if pf.Feed.SiteURL != want.SiteURL {
			t.Errorf("feed %q: site url = %q, want %q", want.URL, pf.Feed.SiteURL, want.SiteURL)
		}
	}
}

// ---------- test doubles ----------

// opmlFakeDB is a minimal in-memory stand-in for database.DB. It enforces the
// foreign key on feeds.folder, which is the whole reason these tests exist:
// without it the old code looked like it worked.
type opmlFakeDB struct {
	folders    []Folder
	feeds      []Feed
	execs      []string
	failFolder string // folder id whose insert should fail
	failFeed   string // feed id whose insert should fail
}

func newOPMLFakeDB() *opmlFakeDB { return &opmlFakeDB{} }

func (f *opmlFakeDB) hasFolder(id string) bool {
	for _, folder := range f.folders {
		if folder.ID == id {
			return true
		}
	}
	return false
}

func (f *opmlFakeDB) folderIDs() []string {
	out := make([]string, 0, len(f.folders))
	for _, folder := range f.folders {
		out = append(out, folder.ID)
	}
	return out
}

func (f *opmlFakeDB) firstIndexOf(fragment string) int {
	for i, q := range f.execs {
		if strings.Contains(q, fragment) {
			return i
		}
	}
	return -1
}

func (f *opmlFakeDB) Exec(_ context.Context, query string, args ...any) error {
	f.execs = append(f.execs, query)

	switch {
	case strings.Contains(query, "INSERT INTO folders"):
		id, label := args[0].(string), args[1].(string)
		if id == f.failFolder {
			return errors.New("fake: folder insert failed")
		}
		for i := range f.folders {
			if f.folders[i].ID == id {
				f.folders[i].Label = label
				return nil
			}
		}
		f.folders = append(f.folders, Folder{ID: id, Label: label})
		return nil

	case strings.Contains(query, "INSERT INTO feeds"):
		feed := Feed{
			ID:      args[0].(string),
			Title:   args[1].(string),
			URL:     args[2].(string),
			Folder:  args[3].(string),
			SiteURL: args[4].(string),
		}
		if feed.ID == f.failFeed {
			return errors.New("fake: feed insert failed")
		}
		if !f.hasFolder(feed.Folder) {
			// This is what Postgres does today, and what SQLite now does with
			// foreign_keys on.
			return fmt.Errorf("fake: FOREIGN KEY constraint failed: feeds.folder = %q", feed.Folder)
		}
		for i := range f.feeds {
			if f.feeds[i].ID == feed.ID {
				f.feeds[i] = feed
				return nil
			}
		}
		f.feeds = append(f.feeds, feed)
		return nil
	}

	return nil
}

func (f *opmlFakeDB) Query(_ context.Context, query string, _ ...any) (database.Rows, error) {
	switch {
	case strings.Contains(query, "FROM folders"):
		rows := make([][]any, 0, len(f.folders))
		for _, folder := range f.folders {
			rows = append(rows, []any{folder.ID, folder.Label})
		}
		return &opmlFakeRows{rows: rows}, nil
	case strings.Contains(query, "FROM feeds"):
		rows := make([][]any, 0, len(f.feeds))
		for _, feed := range f.feeds {
			rows = append(rows, []any{feed.ID, feed.Title, feed.URL, feed.Folder, feed.SiteURL})
		}
		return &opmlFakeRows{rows: rows}, nil
	}
	return &opmlFakeRows{}, nil
}

// QueryRow answers the two lookups UpsertFeed makes inside its transaction:
// which feed owns a url, and which folder a feed is currently in.
func (f *opmlFakeDB) QueryRow(_ context.Context, query string, args ...any) database.Row {
	switch {
	case strings.Contains(query, "MAX(id)") && strings.Contains(query, "FROM feeds"):
		url, _ := args[0].(string)
		for _, feed := range f.feeds {
			if feed.URL == url {
				return opmlFakeRow{vals: []any{feed.ID}}
			}
		}
		return opmlFakeRow{vals: []any{""}}

	case strings.Contains(query, "MAX(folder)") && strings.Contains(query, "FROM feeds"):
		id, _ := args[0].(string)
		for _, feed := range f.feeds {
			if feed.ID == id {
				return opmlFakeRow{vals: []any{feed.Folder}}
			}
		}
		return opmlFakeRow{vals: []any{""}}
	}
	return opmlFakeRow{err: fmt.Errorf("fake: unsupported query %q", query)}
}

// BeginTx hands back a transaction that writes straight through. These tests
// assert on the final state of successful imports and on the errors returned
// by failed ones, neither of which depends on rollback semantics.
func (f *opmlFakeDB) BeginTx(context.Context) (database.Tx, error) {
	return opmlFakeTx{db: f}, nil
}

type opmlFakeTx struct{ db *opmlFakeDB }

func (t opmlFakeTx) Exec(ctx context.Context, query string, args ...any) error {
	return t.db.Exec(ctx, query, args...)
}

func (t opmlFakeTx) QueryRow(ctx context.Context, query string, args ...any) database.Row {
	return t.db.QueryRow(ctx, query, args...)
}

func (t opmlFakeTx) Query(ctx context.Context, query string, args ...any) (database.Rows, error) {
	return t.db.Query(ctx, query, args...)
}

func (t opmlFakeTx) Commit(context.Context) error   { return nil }
func (t opmlFakeTx) Rollback(context.Context) error { return nil }

func (f *opmlFakeDB) Ping(context.Context) error { return nil }
func (f *opmlFakeDB) Close() error               { return nil }

type opmlFakeRows struct {
	rows [][]any
	i    int
}

func (r *opmlFakeRows) Next() bool {
	r.i++
	return r.i <= len(r.rows)
}

func (r *opmlFakeRows) Scan(dest ...any) error {
	return opmlScan(r.rows[r.i-1], dest)
}

func (r *opmlFakeRows) Close() error { return nil }
func (r *opmlFakeRows) Err() error   { return nil }

type opmlFakeRow struct {
	vals []any
	err  error
}

func (r opmlFakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return opmlScan(r.vals, dest)
}

func opmlScan(src []any, dest []any) error {
	if len(src) != len(dest) {
		return fmt.Errorf("fake: scan wants %d columns, row has %d", len(dest), len(src))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = src[i].(string)
		case *int:
			*d = src[i].(int)
		default:
			return fmt.Errorf("fake: unsupported scan target %T", dest[i])
		}
	}
	return nil
}

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
