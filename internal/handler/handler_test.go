package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kontrolplane/feed/internal/configuration"
	"github.com/kontrolplane/feed/templates"
)

func TestFolderIDFromName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "tech", "tech"},
		{"spaces become dashes", "my feeds", "my-feeds"},
		{"case folds", "My Feeds", "my-feeds"},
		{"slash cannot survive: it made the folder unroutable", "news/world", "news-world"},
		{"runs of separators collapse", "a // b -- c", "a-b-c"},
		{"leading and trailing separators are dropped", "  /tech/  ", "tech"},
		{"punctuation only falls back", "///", "folder"},
		{"empty falls back", "", "folder"},
		{"digits are kept", "web 2.0", "web-2-0"},
		{"unicode letters are kept", "Ångström", "ångström"},
		{"dot segments cannot escape", "..", "folder"},
		{"query and fragment characters go", "a?b#c", "a-b-c"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := folderIDFromName(tc.in); got != tc.want {
				t.Fatalf("folderIDFromName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFolderIDFromNameIsRouteSafe(t *testing.T) {
	// Whatever comes out has to survive being a single path segment, or the
	// folder can never be opened or deleted again.
	inputs := []string{"news/world", "a\\b", "x?y", "p#q", "  ", "%2f", "a b/c d"}
	for _, in := range inputs {
		id := folderIDFromName(in)
		if id == "" {
			t.Fatalf("folderIDFromName(%q) returned an empty id", in)
		}
		for _, r := range id {
			switch {
			case r == '-':
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r > 127: // unicode letters are allowed through
			default:
				t.Fatalf("folderIDFromName(%q) = %q contains unsafe rune %q", in, id, r)
			}
		}
	}
}

func TestFolderIDFromNameTruncatesByRunes(t *testing.T) {
	long := ""
	for i := 0; i < maxFolderIDRunes*2; i++ {
		long += "é"
	}
	id := folderIDFromName(long)
	if got := len([]rune(id)); got != maxFolderIDRunes {
		t.Fatalf("truncated to %d runes, want %d", got, maxFolderIDRunes)
	}
	// A byte cut would have produced invalid UTF-8 here.
	for _, r := range id {
		if r != 'é' {
			t.Fatalf("truncation corrupted the id: %q", id)
		}
	}
}

func TestViewFromPath(t *testing.T) {
	tests := []struct {
		path   string
		want   templates.ViewState
		wantOK bool
	}{
		{"/", templates.ViewState{Kind: "view", ID: "unread"}, true},
		{"", templates.ViewState{Kind: "view", ID: "unread"}, true},
		{"/v/starred", templates.ViewState{Kind: "view", ID: "starred"}, true},
		{"/views/starred", templates.ViewState{Kind: "view", ID: "starred"}, true},
		{"/folders/tech", templates.ViewState{Kind: "folder", ID: "tech"}, true},
		{"/folders/tech/", templates.ViewState{Kind: "folder", ID: "tech"}, true},
		{"/feeds/abc123", templates.ViewState{Kind: "feed", ID: "abc123"}, true},
		{"/manage", templates.ViewState{Kind: "view", ID: "manage"}, true},
		{"/settings", templates.ViewState{Kind: "view", ID: "settings"}, true},
		// modals and sub-resources are not views
		{"/folders/new", templates.ViewState{}, false},
		{"/feeds/new", templates.ViewState{}, false},
		{"/feeds/abc/edit", templates.ViewState{}, false},
		{"/items/xyz", templates.ViewState{}, false},
		{"/nonsense", templates.ViewState{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := viewFromPath(tc.path)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("viewFromPath(%q) = %+v, %v; want %+v, %v", tc.path, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The whole point of finding 3's fix: the view comes out of the request, so
// two concurrent readers cannot see each other's.
func TestResolveViewPrecedence(t *testing.T) {
	h := &Handler{}

	tests := []struct {
		name       string
		target     string
		currentURL string
		want       templates.ViewState
	}{
		{
			name:       "an explicit parameter wins",
			target:     "/partials/list?folder=tech",
			currentURL: "http://localhost:8080/v/starred",
			want:       templates.ViewState{Kind: "folder", ID: "tech"},
		},
		{
			name:       "htmx's current url is used when the route says nothing",
			target:     "/partials/list",
			currentURL: "http://localhost:8080/folders/tech",
			want:       templates.ViewState{Kind: "folder", ID: "tech"},
		},
		{
			name:       "a feed url from the header",
			target:     "/search?q=go",
			currentURL: "http://localhost:8080/feeds/abc123",
			want:       templates.ViewState{Kind: "feed", ID: "abc123"},
		},
		{
			name:       "no header and an uninformative path falls back to the default",
			target:     "/partials/list",
			currentURL: "",
			want:       templates.ViewState{Kind: "view", ID: "unread"},
		},
		{
			name:       "an unparseable current url falls back rather than failing",
			target:     "/partials/list",
			currentURL: "://nonsense",
			want:       templates.ViewState{Kind: "view", ID: "unread"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.currentURL != "" {
				r.Header.Set("HX-Current-URL", tc.currentURL)
			}
			if got := h.resolveView(r).State; got != tc.want {
				t.Fatalf("resolveView = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveViewCarriesQuery(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest(http.MethodGet, "/search?q=%20golang%20", nil)
	if got := h.resolveView(r).Query; got != "golang" {
		t.Fatalf("query = %q, want %q", got, "golang")
	}

	// Navigating away carries no q, so the search clears itself without the
	// server having to remember to reset anything.
	r = httptest.NewRequest(http.MethodGet, "/views/unread", nil)
	if got := h.routeView(r, "view", "unread").Query; got != "" {
		t.Fatalf("query = %q, want empty", got)
	}
}

func TestResolveSort(t *testing.T) {
	h := &Handler{}

	tests := []struct {
		name   string
		target string
		cookie string
		want   string
	}{
		{"default", "/partials/list", "", "newest"},
		{"explicit parameter", "/partials/list?sort=oldest", "", "oldest"},
		{"parameter beats cookie", "/partials/list?sort=oldest", "unread", "oldest"},
		{"cookie is remembered", "/views/unread", "unread", "unread"},
		{"a bogus parameter is ignored", "/partials/list?sort=sideways", "", "newest"},
		{"a bogus cookie is ignored", "/partials/list", "sideways", "newest"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sortCookie, Value: tc.cookie})
			}
			if got := h.resolveSort(r); got != tc.want {
				t.Fatalf("resolveSort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPersistSortOnlyOnExplicitRequest(t *testing.T) {
	h := &Handler{}

	w := httptest.NewRecorder()
	h.persistSort(w, httptest.NewRequest(http.MethodGet, "/partials/list", nil))
	if got := w.Result().Cookies(); len(got) != 0 {
		t.Fatalf("a request without ?sort= set %d cookies", len(got))
	}

	w = httptest.NewRecorder()
	h.persistSort(w, httptest.NewRequest(http.MethodGet, "/partials/list?sort=oldest", nil))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sortCookie || cookies[0].Value != "oldest" {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("sort cookie is not scoped safely: %+v", cookies[0])
	}
}

func TestMarkReadOnOpen(t *testing.T) {
	tests := []struct {
		name       string
		markReadOn string
		htmx       bool
		want       bool
	}{
		{"open via htmx marks read", "open", true, true},
		{"open via a bare GET does not: prefetchers must not burn the queue", "open", false, false},
		{"scroll is the client's job", "scroll", true, false},
		{"manual never marks", "manual", true, false},
		{"an unset value is not 'open'", "", true, false},
		{"case and padding are tolerated", "  OPEN ", true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			h.cfg.MarkReadOn = tc.markReadOn
			r := httptest.NewRequest(http.MethodGet, "/items/abc", nil)
			if tc.htmx {
				r.Header.Set("HX-Request", "true")
			}
			if got := h.markReadOnOpen(r); got != tc.want {
				t.Fatalf("markReadOnOpen = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatSyncAge(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "last sync 0s ago"},
		{30 * time.Second, "last sync 30s ago"},
		{90 * time.Second, "last sync 1m ago"},
		{2 * time.Hour, "last sync 120m ago"},
		{-5 * time.Second, "last sync 0s ago"}, // clock skew must not print a negative
	}
	for _, tc := range tests {
		if got := formatSyncAge(tc.in); got != tc.want {
			t.Fatalf("formatSyncAge(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLastSyncIsSafeBeforeAnyWrite(t *testing.T) {
	h := New(nil, nil, nil, defaultTestConfig())
	if h.lastSyncAt().IsZero() {
		t.Fatal("New must seed lastSync")
	}

	// Concurrent writes from a "fetcher" and reads from "requests": this is the
	// pattern the race detector used to flag on the old plain time.Time field.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.SetLastSync(time.Now())
		}
	}()
	for i := 0; i < 200; i++ {
		_ = h.lastSyncAt()
	}
	<-done
}

func defaultTestConfig() configuration.FeedServiceConfiguration {
	return configuration.FeedServiceConfiguration{
		DatabaseDriver: "sqlite",
		DatabasePath:   "feed.db",
		MarkReadOn:     "open",
	}
}
