package worker

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kontrolplane/feed/internal/store"
	"github.com/mmcdole/gofeed"
)

func TestParseRetention(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty keeps forever", in: "", want: 0},
		{name: "forever", in: "forever", want: 0},
		{name: "forever is case insensitive", in: "Forever", want: 0},
		{name: "forever is trimmed", in: "  forever  ", want: 0},
		{name: "never is an alias", in: "never", want: 0},
		{name: "seven days", in: "7d", want: 7 * 24 * time.Hour},
		{name: "thirty days", in: "30d", want: 30 * 24 * time.Hour},
		{name: "ninety days", in: "90d", want: 90 * 24 * time.Hour},
		{name: "zero days prunes immediately-read items", in: "0d", want: 0},
		{name: "weeks", in: "2w", want: 14 * 24 * time.Hour},
		{name: "go duration passthrough", in: "12h", want: 12 * time.Hour},
		{name: "garbage", in: "soon", wantErr: true},
		{name: "non numeric days", in: "xd", wantErr: true},
		{name: "negative days", in: "-7d", wantErr: true},
		{name: "negative duration", in: "-1h", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRetention(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRetention(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRetention(%q) returned unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseRetention(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// A retention that fails to parse must never be interpreted as "delete
// everything now".
func TestParseRetentionErrorYieldsForever(t *testing.T) {
	got, err := ParseRetention("nonsense")
	if err == nil {
		t.Fatal("want an error for an unparseable retention")
	}
	if got != 0 {
		t.Fatalf("failed parse returned %v, want the zero (forever) duration", got)
	}
}

func TestFilterKnown(t *testing.T) {
	items := []store.Item{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}

	tests := []struct {
		name  string
		known map[string]bool
		want  []string
	}{
		{name: "nothing known keeps everything", known: nil, want: []string{"a", "b", "c", "d"}},
		{name: "all known keeps nothing", known: map[string]bool{"a": true, "b": true, "c": true, "d": true}, want: nil},
		{name: "keeps order of the survivors", known: map[string]bool{"b": true}, want: []string{"a", "c", "d"}},
		{name: "interleaved", known: map[string]bool{"a": true, "c": true}, want: []string{"b", "d"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterKnown(items, func(id string) bool { return tc.known[id] })
			if len(got) != len(tc.want) {
				t.Fatalf("got %d items, want %d (%v)", len(got), len(tc.want), tc.want)
			}
			for i, item := range got {
				if item.ID != tc.want[i] {
					t.Fatalf("item %d = %q, want %q", i, item.ID, tc.want[i])
				}
			}
		})
	}
}

// filterKnown must ask the store exactly once per item, and must do so before
// anything expensive happens - that is the whole point of finding 7.
func TestFilterKnownQueriesEachItemOnce(t *testing.T) {
	items := []store.Item{{ID: "a"}, {ID: "b"}, {ID: "a"}}
	var asked []string
	filterKnown(items, func(id string) bool {
		asked = append(asked, id)
		return false
	})
	if len(asked) != 3 {
		t.Fatalf("lookup called %d times, want 3", len(asked))
	}
}

func TestItemIDIsStableAndScopedToFeed(t *testing.T) {
	a := itemID("feed-1", "guid-1")
	b := itemID("feed-1", "guid-1")
	if a != b {
		t.Fatalf("itemID is not deterministic: %q vs %q", a, b)
	}
	if c := itemID("feed-2", "guid-1"); c == a {
		t.Fatal("the same guid in two different feeds must not collide")
	}
	if d := itemID("feed-1", "guid-2"); d == a {
		t.Fatal("two guids in the same feed must not collide")
	}
	if len(a) != 16 {
		t.Fatalf("itemID = %q (%d chars), want 16 hex chars", a, len(a))
	}
	// The old implementation hex-encoded then sliced the string; the digest is
	// now truncated in binary, so the id must be pure hex.
	if strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("itemID = %q, want lowercase hex only", a)
	}
}

// Finding 13: a byte slice through a multi-byte rune produces invalid UTF-8,
// which Postgres rejects outright and which failed the whole item insert.
func TestBuildItemProducesValidUTF8(t *testing.T) {
	// Cyrillic and CJK are multi-byte, so every truncation boundary that used
	// to be computed in bytes lands mid-rune here.
	long := strings.Repeat("日本語のテキスト", 200)
	category := strings.Repeat("категория", 10)

	entry := &gofeed.Item{
		Title:       "Тест",
		Description: "<p>" + long + "</p>",
		Content:     "<p>" + long + "</p>",
		Link:        "https://example.com/a",
		GUID:        "guid-1",
		Categories:  []string{category},
	}
	feed := store.Feed{ID: "feed-1", Folder: "news"}

	item := buildItem(feed, entry, time.Now())

	for name, value := range map[string]string{
		"abstract": item.Abstract,
		"tag":      item.Tag,
		"body":     item.Body,
		"id":       item.ID,
	} {
		if !utf8.ValidString(value) {
			t.Fatalf("%s is not valid UTF-8: %q", name, value)
		}
	}

	// TruncateRunes counts runes, so the byte length legitimately exceeds the
	// rune budget; the rune count is what must be capped.
	if n := utf8.RuneCountInString(strings.TrimSuffix(item.Abstract, "…")); n > abstractRunes {
		t.Fatalf("abstract is %d runes, want at most %d", n, abstractRunes)
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(item.Tag, "…")); n > tagRunes {
		t.Fatalf("tag is %d runes, want at most %d", n, tagRunes)
	}
	if len(item.Abstract) <= abstractRunes {
		t.Fatal("test input was too short to exercise truncation at all")
	}
}

// Finding 1: publisher-supplied entry.Content reached templ.Raw unfiltered.
func TestBuildItemSanitisesBodyAndAbstract(t *testing.T) {
	hostile := `<p>hello</p><script>alert(1)</script><img src=x onerror="steal()">` +
		`<a href="javascript:alert(2)">click</a>`

	entry := &gofeed.Item{
		Title:       "post",
		Description: hostile,
		Content:     hostile,
		GUID:        "guid-1",
	}
	item := buildItem(store.Feed{ID: "f"}, entry, time.Now())

	for name, value := range map[string]string{"body": item.Body, "abstract": item.Abstract} {
		lower := strings.ToLower(value)
		for _, bad := range []string{"<script", "onerror", "javascript:"} {
			if strings.Contains(lower, bad) {
				t.Fatalf("%s still contains %q: %q", name, bad, value)
			}
		}
	}
	if !strings.Contains(item.Body, "hello") {
		t.Fatalf("sanitising threw away the legitimate content: %q", item.Body)
	}
	// The abstract is plain text, so it must carry no markup at all.
	if strings.Contains(item.Abstract, "<") {
		t.Fatalf("abstract still contains markup: %q", item.Abstract)
	}
}

func TestBuildItemDate(t *testing.T) {
	published := time.Date(2024, 3, 4, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2025, 6, 7, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		entry *gofeed.Item
		want  string
	}{
		{
			name:  "published wins",
			entry: &gofeed.Item{GUID: "g", PublishedParsed: &published, UpdatedParsed: &updated},
			want:  "2024-03-04",
		},
		{
			name:  "falls back to updated",
			entry: &gofeed.Item{GUID: "g", UpdatedParsed: &updated},
			want:  "2025-06-07",
		},
		{
			name:  "falls back to now",
			entry: &gofeed.Item{GUID: "g"},
			want:  "2026-01-02",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildItem(store.Feed{ID: "f"}, tc.entry, now).Date; got != tc.want {
				t.Fatalf("date = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildItemGUIDFallback(t *testing.T) {
	feed := store.Feed{ID: "f"}
	now := time.Now()

	byGUID := buildItem(feed, &gofeed.Item{GUID: "g", Link: "https://example.com/a", Title: "t"}, now)
	byLink := buildItem(feed, &gofeed.Item{Link: "https://example.com/a", Title: "t"}, now)
	byTitle := buildItem(feed, &gofeed.Item{Title: "t"}, now)

	if byGUID.ID == byLink.ID {
		t.Fatal("guid and link fallback must not derive the same id")
	}
	if byLink.ID != itemID("f", "https://example.com/a") {
		t.Fatal("an entry without a guid must fall back to its link")
	}
	if byTitle.ID != itemID("f", "t") {
		t.Fatal("an entry without guid or link must fall back to its title")
	}
}

func TestAuthorNames(t *testing.T) {
	tests := []struct {
		name  string
		entry *gofeed.Item
		want  string
	}{
		{name: "none", entry: &gofeed.Item{}, want: ""},
		{name: "single author field", entry: &gofeed.Item{Author: &gofeed.Person{Name: "Ada"}}, want: "Ada"},
		{
			name:  "authors list wins over author",
			entry: &gofeed.Item{Author: &gofeed.Person{Name: "Ada"}, Authors: []*gofeed.Person{{Name: "Grace"}, {Name: "Alan"}}},
			want:  "Grace · Alan",
		},
		{
			name:  "blank names are skipped",
			entry: &gofeed.Item{Authors: []*gofeed.Person{{Name: ""}, {Name: "Alan"}}},
			want:  "Alan",
		},
		{
			name:  "all blank falls back to author",
			entry: &gofeed.Item{Author: &gofeed.Person{Name: "Ada"}, Authors: []*gofeed.Person{{Name: ""}}},
			want:  "Ada",
		},
		{
			name:  "nil entries in the list are skipped",
			entry: &gofeed.Item{Authors: []*gofeed.Person{nil, {Name: "Alan"}}},
			want:  "Alan",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := authorNames(tc.entry); got != tc.want {
				t.Fatalf("authorNames = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadingMinutes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		abstract string
		want     int
	}{
		{name: "empty falls back to the default", want: defaultMinutes},
		{name: "short body rounds up to the default", body: "<p>a few words here</p>", want: defaultMinutes},
		{
			name: "markup is not counted as words",
			body: "<p>" + strings.Repeat("word ", 400) + "</p>",
			want: 2,
		},
		{
			name:     "empty body falls back to the abstract",
			abstract: strings.Repeat("word ", 600),
			want:     3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readingMinutes(tc.body, tc.abstract); got != tc.want {
				t.Fatalf("readingMinutes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGuessTag(t *testing.T) {
	tests := []struct {
		name  string
		entry *gofeed.Item
		want  string
	}{
		{name: "no category, short content", entry: &gofeed.Item{Content: "hi"}, want: "post"},
		{name: "no category, long content", entry: &gofeed.Item{Content: strings.Repeat("x", 2001)}, want: "article"},
		{name: "category is lowercased", entry: &gofeed.Item{Categories: []string{"Go"}}, want: "go"},
		{name: "blank category falls through", entry: &gofeed.Item{Categories: []string{"  "}, Content: "hi"}, want: "post"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := guessTag(tc.entry); got != tc.want {
				t.Fatalf("guessTag = %q, want %q", got, tc.want)
			}
		})
	}
}

// A long multi-byte category must come back as valid UTF-8 - the old cat[:20]
// sliced straight through a rune.
func TestGuessTagTruncatesByRunes(t *testing.T) {
	entry := &gofeed.Item{Categories: []string{strings.Repeat("é", 50)}}
	got := guessTag(entry)
	if !utf8.ValidString(got) {
		t.Fatalf("tag is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n > tagRunes {
		t.Fatalf("tag is %d runes, want at most %d", n, tagRunes)
	}
}

// Guards against a regression that would reintroduce byte slicing: every
// abstract length between 290 and 320 runes of multi-byte text must stay valid.
func TestAbstractTruncationAcrossRuneBoundaries(t *testing.T) {
	for n := 290; n <= 320; n++ {
		entry := &gofeed.Item{GUID: "g", Description: strings.Repeat("é", n)}
		item := buildItem(store.Feed{ID: "f"}, entry, time.Now())
		if !utf8.ValidString(item.Abstract) {
			t.Fatalf("abstract for %d runes is not valid UTF-8: %q", n, item.Abstract)
		}
		if got := utf8.RuneCountInString(strings.TrimSuffix(item.Abstract, "…")); got > abstractRunes {
			t.Fatalf("abstract for %d runes kept %d runes, want at most %d", n, got, abstractRunes)
		}
	}
}

func ExampleParseRetention() {
	d, err := ParseRetention("30d")
	fmt.Println(d, err)
	// Output: 720h0m0s <nil>
}
