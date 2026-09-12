package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kontrolplane/feed/internal/configuration"
	"github.com/kontrolplane/feed/internal/database"
	"github.com/kontrolplane/feed/internal/store"
)

// The route table is a contract with the frontend: every path below is one the
// templates emit, and breaking any of them breaks the UI silently — htmx does
// not swap a non-2xx response, so a broken route looks like a slow one. This
// exercise runs the real mux against a real (temporary, file-backed) database,
// because that is the only way to catch a handler that compiles and 500s.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store, *Handler) {
	t.Helper()

	cfg := configuration.FeedServiceConfiguration{
		DatabaseDriver:  "sqlite",
		DatabasePath:    filepath.Join(t.TempDir(), "test.db"),
		MarkReadOn:      "open",
		RefreshInterval: 15 * time.Minute,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	pool, err := database.CreatePool(ctx, cfg, logger)
	if err != nil {
		t.Skipf("cannot open a sqlite database here: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	if err := database.Migrate(ctx, cfg, logger); err != nil {
		t.Skipf("cannot migrate: %v", err)
	}

	s := store.New(pool, cfg.DatabaseDriver)
	h := New(s, pool, logger, cfg)

	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(LogRequests(logger)(Secure(mux)))
	t.Cleanup(srv.Close)

	return srv, s, h
}

// request issues one request with the headers a real htmx interaction carries.
func request(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", srv.URL+"/manage")
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func requireStatus(t *testing.T, resp *http.Response, want int) *http.Response {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Fatalf("%s %s = %d, want %d (%s)", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, b)
	}
	return resp
}

func TestReadRoutes(t *testing.T) {
	srv, _, _ := newTestServer(t)

	tests := []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusNoContent},
		{"/", http.StatusOK},
		{"/v/starred", http.StatusOK},
		{"/views/unread", http.StatusOK},
		{"/partials/list", http.StatusOK},
		{"/partials/sidebar", http.StatusOK},
		{"/partials/status", http.StatusOK},
		{"/search?q=go", http.StatusOK},
		{"/settings", http.StatusOK},
		{"/manage", http.StatusOK},
		{"/feeds/new", http.StatusOK},
		{"/folders/new", http.StatusOK},
		{"/export/opml", http.StatusOK},

		// GET / is Go's catch-all; an unrouted path must not render a page.
		{"/typo", http.StatusNotFound},
		{"/a/b/c", http.StatusNotFound},

		{"/items/does-not-exist", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			requireStatus(t, request(t, srv, http.MethodGet, tc.path, ""), tc.want)
		})
	}
}

func TestSortIsRememberedPerBrowserNotPerServer(t *testing.T) {
	srv, _, _ := newTestServer(t)

	resp := requireStatus(t, request(t, srv, http.MethodGet, "/partials/list?sort=oldest", ""), http.StatusOK)
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != sortCookie || cookies[0].Value != "oldest" {
		t.Fatalf("sort was not handed back to this browser: %+v", cookies)
	}

	// A second reader, sending no cookie, must still get the default: the old
	// implementation stored sort on the Handler, so one reader re-sorted every
	// other reader's list.
	resp = requireStatus(t, request(t, srv, http.MethodGet, "/partials/list", ""), http.StatusOK)
	if got := resp.Cookies(); len(got) != 0 {
		t.Fatalf("an unrelated request rewrote the sort cookie: %+v", got)
	}
}

func TestWriteRoutesReportFailure(t *testing.T) {
	srv, s, _ := newTestServer(t)
	ctx := context.Background()

	// A folder name with a slash in it used to produce an id containing "/",
	// which no {id} route pattern can ever match — the folder was created and
	// then could never be opened or deleted.
	requireStatus(t, request(t, srv, http.MethodPost, "/folders", "name=News%2FWorld"), http.StatusOK)

	folders, err := s.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].ID != "news-world" || folders[0].Label != "News/World" {
		t.Fatalf("unexpected folders: %+v", folders)
	}
	requireStatus(t, request(t, srv, http.MethodGet, "/folders/news-world", ""), http.StatusOK)

	// A case variant of an existing name must reuse that folder rather than
	// derive the same id and silently relabel it.
	requireStatus(t, request(t, srv, http.MethodPost, "/folders", "name=news%2Fworld"), http.StatusOK)
	folders, _ = s.Folders(ctx)
	if len(folders) != 1 {
		t.Fatalf("a case variant created a duplicate folder: %+v", folders)
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"blank folder name", http.MethodPost, "/folders", "name=%20%20", http.StatusBadRequest},
		{"probe rejects a non-http url", http.MethodPost, "/feeds/probe", "url=javascript:alert(1)", http.StatusBadRequest},
		{"probe accepts an https url", http.MethodPost, "/feeds/probe", "url=https%3A%2F%2Fexample.com%2Ffeed.xml", http.StatusOK},
		{"subscribe without a url", http.MethodPost, "/feeds/subscribe", "feed_url=&folder=news-world", http.StatusBadRequest},
		{"subscribe with a blank new folder", http.MethodPost, "/feeds/subscribe", "feed_url=https%3A%2F%2Fexample.com%2Ffeed.xml&folder=__new__&new_folder=", http.StatusBadRequest},
		{"subscribe", http.MethodPost, "/feeds/subscribe", "feed_url=https%3A%2F%2Fexample.com%2Ffeed.xml&folder=news-world", http.StatusOK},
		{"subscribing twice is not a success", http.MethodPost, "/feeds/subscribe", "feed_url=https%3A%2F%2Fexample.com%2Ffeed.xml&folder=news-world", http.StatusConflict},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireStatus(t, request(t, srv, tc.method, tc.path, tc.body), tc.want)
		})
	}

	feeds, err := s.Feeds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 {
		t.Fatalf("want exactly one feed, got %+v", feeds)
	}
	feedID := feeds[0].ID

	requireStatus(t, request(t, srv, http.MethodGet, "/feeds/"+feedID, ""), http.StatusOK)
	requireStatus(t, request(t, srv, http.MethodGet, "/feeds/"+feedID+"/edit", ""), http.StatusOK)
	requireStatus(t, request(t, srv, http.MethodGet, "/feeds/"+feedID+"/confirm-delete", ""), http.StatusOK)
	requireStatus(t, request(t, srv, http.MethodGet, "/folders/news-world/confirm-delete", ""), http.StatusOK)

	// Moving a feed to a new folder must take its items' folder column with it.
	requireStatus(t, request(t, srv, http.MethodPut, "/feeds/"+feedID,
		"title=Example&folder=__new__&new_folder=Tech%20Stuff"), http.StatusOK)
	feeds, _ = s.Feeds(ctx)
	if feeds[0].Folder != "tech-stuff" || feeds[0].Title != "Example" {
		t.Fatalf("feed was not updated: %+v", feeds[0])
	}

	requireStatus(t, request(t, srv, http.MethodDelete, "/feeds/"+feedID, ""), http.StatusOK)
	requireStatus(t, request(t, srv, http.MethodDelete, "/folders/tech-stuff", ""), http.StatusOK)
}

func TestRefetchWithoutAFetcher(t *testing.T) {
	srv, _, _ := newTestServer(t)
	requireStatus(t, request(t, srv, http.MethodPost, "/items/any/refetch", ""), http.StatusServiceUnavailable)
}
