package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/a-h/templ"
	"github.com/kontrolplane/feed/internal/configuration"
	"github.com/kontrolplane/feed/internal/database"
	"github.com/kontrolplane/feed/internal/httpx"
	"github.com/kontrolplane/feed/internal/store"
	"github.com/kontrolplane/feed/templates"
)

// ItemRefetcher re-pulls and re-extracts a single stored article. It is the
// only thing the handler needs from the background fetcher, so it is declared
// as an interface here rather than importing the worker: the handler stays
// testable, and the wiring direction (main knows about both) does not change.
type ItemRefetcher interface {
	RefetchItem(ctx context.Context, itemID string) error
}

// Handler holds only process-wide dependencies.
//
// It used to also hold the *current view*, the *current sort order* and the
// *current search query* as plain struct fields (finding 3). That was wrong
// twice over. It was a data race — every HTTP handler wrote those fields from
// its own goroutine while the fetcher goroutine wrote lastSync from another,
// with no synchronisation at all, which `go build -race` flags. And it was a
// correctness bug that no amount of locking would fix: the fields describe a
// *viewer*, not the server, so two browser tabs — or two people — silently
// overwrote each other's open folder and sort order.
//
// View, sort and query are now derived per request (see viewCtx). The only
// mutable state left is lastSync, which genuinely does describe the process
// and not a viewer, and it is atomic.
type Handler struct {
	store  *store.Store
	db     database.DB
	logger *slog.Logger
	cfg    configuration.FeedServiceConfiguration

	// lastSync is written by the fetcher goroutine and read by every request
	// goroutine, hence the atomic. A pointer, because time.Time is two words
	// and cannot be stored atomically by value.
	lastSync atomic.Pointer[time.Time]

	// refetch is wired by SetFetcher after construction, because the fetcher
	// needs the handler (for the sync callback) and the handler needs the
	// fetcher. It is written once during start-up, strictly before the
	// serving goroutine exists, so the goroutine-start edge orders it; no
	// lock is needed and none is implied.
	refetch ItemRefetcher
}

// New builds the handler. db is used only by the health check, which pings it
// directly rather than going through the store: a liveness probe must not
// depend on the schema being what the store expects.
func New(s *store.Store, db database.DB, logger *slog.Logger, cfg configuration.FeedServiceConfiguration) *Handler {
	h := &Handler{store: s, db: db, logger: logger, cfg: cfg}
	now := time.Now()
	h.lastSync.Store(&now)
	return h
}

// SetFetcher wires the article refetcher used by POST /items/{id}/refetch.
// It must be called before the server starts accepting requests.
func (h *Handler) SetFetcher(f ItemRefetcher) {
	h.refetch = f
}

// SetLastSync records when the fetcher last completed a cycle. Called from the
// fetcher's goroutine, read from request goroutines.
func (h *Handler) SetLastSync(t time.Time) {
	h.lastSync.Store(&t)
}

func (h *Handler) lastSyncAt() time.Time {
	if t := h.lastSync.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

// Secure wraps a handler in the two security middlewares from middleware.go.
//
// Order is deliberate: SecurityHeaders is the *outer* layer so that the
// responses CSRFGuard itself produces — a 403 on a cross-site POST — carry the
// same CSP, nosniff and Referrer-Policy as everything else. Headers must be set
// before the first WriteHeader, so a rejecting middleware placed outside the
// header-setting one would emit bare, unprotected error pages. Request logging
// belongs outside both, so it observes the final status code.
func Secure(next http.Handler) http.Handler {
	return SecurityHeaders(CSRFGuard(next))
}

func (h *Handler) Register(mux *http.ServeMux) {
	// health — deliberately outside every page-rendering path
	mux.HandleFunc("GET /healthz", h.handleHealthz)

	// full page
	mux.HandleFunc("GET /", h.handleRoot)
	mux.HandleFunc("GET /v/{id}", h.handleViewPage)
	mux.HandleFunc("GET /items/{id}", h.handleItemPage)
	mux.HandleFunc("GET /feeds/{id}", h.handleFeedOrNew)
	mux.HandleFunc("GET /folders/new", h.handleAddFolderModal)
	mux.HandleFunc("POST /folders", h.handleCreateFolder)
	mux.HandleFunc("GET /folders/{id}/confirm-delete", h.handleConfirmDeleteFolder)
	mux.HandleFunc("DELETE /folders/{id}", h.handleDeleteFolder)
	mux.HandleFunc("GET /folders/{id}", h.handleFolderPage)

	// partials for htmx
	mux.HandleFunc("GET /views/{id}", h.handleViewPartial)
	mux.HandleFunc("GET /partials/list", h.handleListPartial)
	mux.HandleFunc("GET /partials/sidebar", h.handleSidebarPartial)
	mux.HandleFunc("GET /partials/status", h.handleStatusPartial)
	mux.HandleFunc("GET /search", h.handleSearch)
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("GET /manage", h.handleManage)

	// item actions
	mux.HandleFunc("POST /items/{id}/star", h.handleStar)
	mux.HandleFunc("POST /items/{id}/toggle-read", h.handleToggleRead)
	mux.HandleFunc("POST /items/{id}/refetch", h.handleRefetchItem)

	// feed management
	mux.HandleFunc("POST /feeds/probe", h.handleProbe)
	mux.HandleFunc("POST /feeds/subscribe", h.handleSubscribe)
	mux.HandleFunc("GET /feeds/{id}/edit", h.handleEditFeed)
	mux.HandleFunc("GET /feeds/{id}/confirm-delete", h.handleConfirmDeleteFeed)
	mux.HandleFunc("PUT /feeds/{id}", h.handleUpdateFeed)
	mux.HandleFunc("DELETE /feeds/{id}", h.handleDeleteFeed)

	// import / export
	mux.HandleFunc("GET /export/opml", h.handleExportOPML)
	mux.HandleFunc("POST /import/opml", h.handleImportOPML)
}

func (h *Handler) isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// ---------- per-request view state ----------

const (
	defaultViewKind = "view"
	defaultViewID   = "unread"
	defaultSort     = "newest"

	// sortCookie carries the list sort order. Sort is a per-reader preference,
	// not server state, and the sort controls are plain `hx-get
	// /partials/list?sort=...` links with no hx-push-url, so the browser url
	// cannot carry it and the templates may not be edited to make it. A cookie
	// is the one channel that round-trips on every subsequent request without
	// touching the markup — and unlike the old server field it is scoped to one
	// browser, so a second reader no longer re-sorts the first one's list.
	sortCookie = "feed_sort"

	// A folder id ends up in a url path; keep it short enough to stay readable.
	maxFolderIDRunes = 48
)

// viewCtx is the view a single request is about: which collection, filtered by
// what search, in what order. It is built from the request and thrown away with
// it — never stored on the Handler.
type viewCtx struct {
	State templates.ViewState
	Query string
	Sort  string
}

func validSort(s string) bool {
	switch s {
	case "newest", "oldest", "unread":
		return true
	}
	return false
}

// routeView builds the view for a handler whose own route names it — the
// sidebar nav targets (/views/{id}, /folders/{id}, /feeds/{id}) and the full
// page routes. The route is authoritative here and must beat HX-Current-URL,
// which on those requests still holds the url the reader is navigating *away*
// from (htmx pushes the new url only after the swap).
func (h *Handler) routeView(r *http.Request, kind, id string) viewCtx {
	return viewCtx{
		State: templates.ViewState{Kind: kind, ID: id},
		Query: strings.TrimSpace(r.URL.Query().Get("q")),
		Sort:  h.resolveSort(r),
	}
}

// resolveView recovers the view for a handler whose route does *not* name one:
// /partials/list (the sync button), /partials/status, /search, the item
// actions. These used to read the answer out of the shared Handler fields.
//
// Precedence: an explicit query parameter, then the browser url htmx sends in
// HX-Current-URL on every request, then this request's own path, then the
// default. The header is what makes this work without touching a template —
// the sync button posts no parameters at all, but htmx still tells us the
// reader is looking at /folders/tech.
func (h *Handler) resolveView(r *http.Request) viewCtx {
	vc := viewCtx{
		State: templates.ViewState{Kind: defaultViewKind, ID: defaultViewID},
		Query: strings.TrimSpace(r.URL.Query().Get("q")),
		Sort:  h.resolveSort(r),
	}

	if st, ok := viewFromParams(r.URL.Query()); ok {
		vc.State = st
		return vc
	}

	if cur := r.Header.Get("HX-Current-URL"); cur != "" {
		if u, err := url.Parse(cur); err == nil {
			if st, ok := viewFromPath(u.Path); ok {
				vc.State = st
				return vc
			}
		}
	}

	if st, ok := viewFromPath(r.URL.Path); ok {
		vc.State = st
	}
	return vc
}

// viewFromParams reads an explicit view override off the query string. Nothing
// in the current markup sends one; it exists so a link, a bookmark or a future
// template can address a view directly instead of depending on a header.
func viewFromParams(q url.Values) (templates.ViewState, bool) {
	if id := strings.TrimSpace(q.Get("feed")); id != "" {
		return templates.ViewState{Kind: "feed", ID: id}, true
	}
	if id := strings.TrimSpace(q.Get("folder")); id != "" {
		return templates.ViewState{Kind: "folder", ID: id}, true
	}
	if id := strings.TrimSpace(q.Get("view")); id != "" {
		return templates.ViewState{Kind: "view", ID: id}, true
	}
	return templates.ViewState{}, false
}

// viewFromPath maps a browser path onto a view. It understands both the pushed
// urls (/v/{id}, /folders/{id}, /feeds/{id}, /manage, /settings) and the htmx
// partial urls that mirror them, because HX-Current-URL carries whichever one
// the sidebar last pushed.
func viewFromPath(p string) (templates.ViewState, bool) {
	seg := strings.Split(strings.Trim(p, "/"), "/")
	if len(seg) == 1 && seg[0] == "" {
		return templates.ViewState{Kind: defaultViewKind, ID: defaultViewID}, true
	}

	switch {
	case len(seg) == 1 && (seg[0] == "manage" || seg[0] == "settings"):
		return templates.ViewState{Kind: "view", ID: seg[0]}, true
	case len(seg) == 2 && (seg[0] == "v" || seg[0] == "views") && seg[1] != "":
		return templates.ViewState{Kind: "view", ID: seg[1]}, true
	case len(seg) == 2 && seg[0] == "folders" && seg[1] != "" && seg[1] != "new":
		return templates.ViewState{Kind: "folder", ID: seg[1]}, true
	case len(seg) == 2 && seg[0] == "feeds" && seg[1] != "" && seg[1] != "new":
		return templates.ViewState{Kind: "feed", ID: seg[1]}, true
	}
	return templates.ViewState{}, false
}

func (h *Handler) resolveSort(r *http.Request) string {
	if s := r.URL.Query().Get("sort"); validSort(s) {
		return s
	}
	if c, err := r.Cookie(sortCookie); err == nil && validSort(c.Value) {
		return c.Value
	}
	return defaultSort
}

// persistSort remembers an explicitly requested sort order for this browser.
// Called only when the request actually carried ?sort=, so a plain refresh
// never rewrites the cookie.
func (h *Handler) persistSort(w http.ResponseWriter, r *http.Request) {
	s := r.URL.Query().Get("sort")
	if !validSort(s) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sortCookie,
		Value:    s,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func requestIsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Behind a terminating proxy the only evidence is the forwarded header.
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ---------- health ----------

// handleHealthz is a liveness/readiness probe. It pings the database and
// returns 204 or 503 — deliberately never rendering a page, because the full
// page render at GET / runs a dozen queries and building the whole UI to prove
// the process is alive is how a probe becomes the load.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		http.Error(w, "no database configured", http.StatusServiceUnavailable)
		return
	}

	// Bound independently of the client: a probe that hangs as long as the
	// database does tells the orchestrator nothing in time to act on.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		h.logger.Error("health check failed", slog.Any("error", err))
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- full page ----------

// handleRoot serves the index.
//
// "GET /" is Go's catch-all: it matches every path no more specific pattern
// claims, so /typo used to return 200 and a full page instead of a 404 —
// invisible to a reader and actively misleading to a crawler. Anything that is
// not exactly "/" is now a 404.
func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r, h.routeView(r, defaultViewKind, defaultViewID))
}

func (h *Handler) handleViewPage(w http.ResponseWriter, r *http.Request) {
	h.serveIndex(w, r, h.routeView(r, "view", r.PathValue("id")))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, vc viewCtx) {
	h.persistSort(w, r)
	if h.isHTMX(r) {
		h.renderList(w, r, vc)
		return
	}
	h.renderFullPage(w, r, vc, nil)
}

// markReadOnOpen decides whether opening an article also marks it read.
//
// Two separate bugs meet here. MARK_READ_ON has always been parsed, shown on
// the settings page and never once consulted (finding 15) — it is honoured now,
// and "scroll" and "manual" both mean "not on open" as far as the server is
// concerned. And GET /items/{id} is not only the htmx request behind a click:
// it is also the plain href on every row, which link prefetchers, crawlers,
// "open in background tab" and any accessibility tool walking the page will
// follow. A GET that mutates burned through the unread queue with nobody
// reading anything (finding 8), so the mark now requires a genuine htmx
// interaction, which only a real click produces.
func (h *Handler) markReadOnOpen(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(h.cfg.MarkReadOn), "open") {
		return false
	}
	return h.isHTMX(r)
}

func (h *Handler) handleItemPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	item, err := h.store.ItemByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if h.markReadOnOpen(r) {
		if err := h.store.MarkRead(ctx, id, true); err != nil {
			// Not fatal: the reader asked for the article, not for a state
			// change, so still serve it — but leave item.Read alone so the UI
			// says "unread", which is the truth. Previously this error was
			// dropped and the UI lied.
			h.logger.Error("mark item read", slog.String("item", id), slog.Any("error", err))
		} else {
			item.Read = true
		}
	}

	vc := h.resolveView(r)

	rd, err := h.readerData(ctx, vc, item)
	if err != nil {
		h.fail(w, "load article", err)
		return
	}

	if h.isHTMX(r) {
		h.renderComponent(w, r, templates.Reader(rd))
		h.renderOOBSidebar(w, r, vc)
		h.renderOOBStatus(w, r, vc)
		return
	}

	h.renderFullPage(w, r, vc, item)
}

func (h *Handler) handleFeedOrNew(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "new" {
		h.handleAddFeedModal(w, r)
		return
	}

	vc := h.routeView(r, "feed", id)
	h.persistSort(w, r)

	if h.isHTMX(r) {
		h.renderList(w, r, vc)
		h.renderOOBSidebar(w, r, vc)
		h.renderOOBStatus(w, r, vc)
		h.renderOOBReaderReset(w, r)
		return
	}
	h.renderFullPage(w, r, vc, nil)
}

func (h *Handler) handleFolderPage(w http.ResponseWriter, r *http.Request) {
	vc := h.routeView(r, "folder", r.PathValue("id"))
	h.persistSort(w, r)

	if h.isHTMX(r) {
		h.renderList(w, r, vc)
		h.renderOOBSidebar(w, r, vc)
		h.renderOOBStatus(w, r, vc)
		h.renderOOBReaderReset(w, r)
		return
	}
	h.renderFullPage(w, r, vc, nil)
}

// ---------- partials ----------

func (h *Handler) handleViewPartial(w http.ResponseWriter, r *http.Request) {
	vc := h.routeView(r, "view", r.PathValue("id"))
	h.persistSort(w, r)

	h.renderList(w, r, vc)
	h.renderOOBSidebar(w, r, vc)
	h.renderOOBStatus(w, r, vc)
	h.renderOOBReaderReset(w, r)
}

func (h *Handler) handleListPartial(w http.ResponseWriter, r *http.Request) {
	h.persistSort(w, r)
	h.renderList(w, r, h.resolveView(r))
}

func (h *Handler) handleSidebarPartial(w http.ResponseWriter, r *http.Request) {
	vc := h.resolveView(r)
	sd, err := h.sidebarData(r.Context(), vc)
	if err != nil {
		h.fail(w, "load sidebar", err)
		return
	}
	h.renderComponent(w, r, templates.Sidebar(sd))
}

func (h *Handler) handleStatusPartial(w http.ResponseWriter, r *http.Request) {
	vc := h.resolveView(r)
	sd, err := h.statusData(r.Context(), vc)
	if err != nil {
		h.fail(w, "load status", err)
		return
	}
	h.renderComponent(w, r, templates.Status(sd))
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	h.renderList(w, r, h.resolveView(r))
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	vc := h.routeView(r, "view", "settings")

	if h.isHTMX(r) {
		h.renderComponent(w, r, templates.Settings(h.settingsData()))
		h.renderOOBSidebar(w, r, vc)
		h.renderOOBStatus(w, r, vc)
		return
	}
	h.renderFullPage(w, r, vc, nil)
}

func (h *Handler) handleManage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vc := h.routeView(r, "view", "manage")

	if !h.isHTMX(r) {
		h.renderFullPage(w, r, vc, nil)
		return
	}

	md, err := h.manageData(ctx)
	if err != nil {
		h.fail(w, "load feed management", err)
		return
	}
	h.renderComponent(w, r, templates.Manage(md))
	h.renderOOBSidebar(w, r, vc)
	h.renderOOBStatus(w, r, vc)
}

// ---------- folder management ----------

func (h *Handler) handleAddFolderModal(w http.ResponseWriter, r *http.Request) {
	h.renderComponent(w, r, templates.AddFolderModal())
}

// errFolderNameRequired is a user error, not a server error: the folder picker
// was left on "+ new folder…" with the name box empty.
var errFolderNameRequired = errors.New("a folder name is required")

// folderIDFromName derives a url-safe id from a folder's display name.
//
// The old derivation lower-cased the name and replaced spaces with dashes,
// and did nothing else. Two silent failures came out of that: a name
// containing "/" produced an id with a slash in it, and neither
// `DELETE /folders/{id}` nor `GET /folders/{id}` can ever match a segment with
// a slash in it, so the folder became permanently undeletable and unopenable;
// and every other separator was left verbatim in a path segment.
//
// Case folding also means "My Feeds" and "my feeds" collapse to one id — that
// collision is resolved by folderIDForName, which suffixes rather than letting
// the upsert silently relabel someone else's folder.
func folderIDFromName(name string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
			continue
		}
		// Runs of separators collapse, and a trailing run is dropped entirely
		// by never writing the dash until a real character follows it.
		pendingDash = true
	}

	id := b.String()
	// Cut by runes, never by bytes: a byte cut splits a multi-byte rune and
	// leaves invalid UTF-8 in a primary key (finding 13).
	if runes := []rune(id); len(runes) > maxFolderIDRunes {
		id = strings.Trim(string(runes[:maxFolderIDRunes]), "-")
	}
	if id == "" {
		// Everything was punctuation or script the filter does not keep.
		id = "folder"
	}
	return id
}

// folderIDForName returns the id of the folder called name, creating it if it
// does not exist. An existing folder with the same label (case-insensitively)
// is reused rather than duplicated, and a genuine id collision between two
// different labels gets a numeric suffix instead of overwriting the incumbent.
func (h *Handler) folderIDForName(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errFolderNameRequired
	}

	folders, err := h.store.Folders(ctx)
	if err != nil {
		return "", fmt.Errorf("list folders: %w", err)
	}

	taken := make(map[string]struct{}, len(folders))
	for _, f := range folders {
		if strings.EqualFold(strings.TrimSpace(f.Label), name) {
			return f.ID, nil
		}
		taken[f.ID] = struct{}{}
	}

	base := folderIDFromName(name)
	id := base
	for n := 2; ; n++ {
		if _, clash := taken[id]; !clash {
			break
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}

	// len(folders) is the position the old code fetched with a second
	// FolderCount query; it is the same number and one fewer round trip.
	if err := h.store.UpsertFolder(ctx, store.Folder{ID: id, Label: name}, len(folders)); err != nil {
		return "", fmt.Errorf("create folder %q: %w", id, err)
	}
	return id, nil
}

func (h *Handler) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if _, err := h.folderIDForName(ctx, name); err != nil {
		if errors.Is(err, errFolderNameRequired) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.fail(w, "create folder", err)
		return
	}

	vc := h.resolveView(r)
	sd, err := h.sidebarData(ctx, vc)
	if err != nil {
		h.fail(w, "load sidebar", err)
		return
	}
	h.renderComponent(w, r, templates.Sidebar(sd))
}

func (h *Handler) handleConfirmDeleteFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	folders, err := h.store.Folders(ctx)
	if err != nil {
		h.fail(w, "list folders", err)
		return
	}

	var folder *store.Folder
	for i := range folders {
		if folders[i].ID == id {
			folder = &folders[i]
			break
		}
	}
	if folder == nil {
		http.NotFound(w, r)
		return
	}

	feeds, err := h.store.Feeds(ctx)
	if err != nil {
		h.fail(w, "list feeds", err)
		return
	}

	var feedCount int
	for _, f := range feeds {
		if f.Folder == id {
			feedCount++
		}
	}

	h.renderComponent(w, r, templates.ManageFolderConfirmDelete(*folder, feedCount))
}

func (h *Handler) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	if err := h.store.DeleteFolder(ctx, id); err != nil {
		h.logger.Error("delete folder", slog.String("id", id), slog.Any("error", err))
		http.Error(w, "failed to delete folder", http.StatusInternalServerError)
		return
	}

	// The deleted folder cannot be the view any more; this response replaces
	// #main with the manage pane, so that is the view. (The old code computed
	// a fallback view here and then overwrote it on the very next line — dead
	// code that only looked like it handled the case.)
	h.renderManage(w, r, h.routeView(r, "view", "manage"))
}

// ---------- item actions ----------

func (h *Handler) handleStar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	if err := h.store.ToggleStar(ctx, id); err != nil {
		h.logger.Error("toggle star", slog.String("item", id), slog.Any("error", err))
		http.Error(w, "could not update this item", http.StatusInternalServerError)
		return
	}

	item, err := h.store.ItemByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	vc := h.resolveView(r)

	if r.Header.Get("HX-Target") == "reader" {
		rd, err := h.readerData(ctx, vc, item)
		if err != nil {
			h.fail(w, "load article", err)
			return
		}
		h.renderComponent(w, r, templates.Reader(rd))
	} else {
		feeds, err := h.store.Feeds(ctx)
		if err != nil {
			h.fail(w, "list feeds", err)
			return
		}
		var f *store.Feed
		for i := range feeds {
			if feeds[i].ID == item.FeedID {
				f = &feeds[i]
				break
			}
		}
		h.renderComponent(w, r, templates.ItemRow(*item, 0, f))
	}

	h.renderOOBSidebar(w, r, vc)
	h.renderOOBStatus(w, r, vc)
}

func (h *Handler) handleToggleRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	if err := h.store.ToggleRead(ctx, id); err != nil {
		h.logger.Error("toggle read", slog.String("item", id), slog.Any("error", err))
		http.Error(w, "could not update this item", http.StatusInternalServerError)
		return
	}

	item, err := h.store.ItemByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	vc := h.resolveView(r)
	rd, err := h.readerData(ctx, vc, item)
	if err != nil {
		h.fail(w, "load article", err)
		return
	}

	h.renderComponent(w, r, templates.Reader(rd))
	h.renderOOBSidebar(w, r, vc)
	h.renderOOBStatus(w, r, vc)
}

// handleRefetchItem re-pulls one article through the SSRF-safe client and swaps
// the freshly extracted body back into the reader. Feeds that publish a summary
// and fill the body in later, and extractions that failed on a transient error,
// previously had no way back other than waiting for the whole refresh cycle.
func (h *Handler) handleRefetchItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	if h.refetch == nil {
		http.Error(w, "refetching is not available", http.StatusServiceUnavailable)
		return
	}

	if _, err := h.store.ItemByID(ctx, id); err != nil {
		http.NotFound(w, r)
		return
	}

	if err := h.refetch.RefetchItem(ctx, id); err != nil {
		h.logger.Error("refetch item", slog.String("item", id), slog.Any("error", err))
		// 502, not 500: the failure is almost always the origin server or a
		// blocked address, not this process. The frontend's responseError
		// listener turns it into a toast.
		http.Error(w, "could not refetch this article", http.StatusBadGateway)
		return
	}

	// Re-read: RefetchItem writes through the store, so the copy loaded above
	// is stale by definition.
	item, err := h.store.ItemByID(ctx, id)
	if err != nil {
		h.fail(w, "reload refetched item", err)
		return
	}

	vc := h.resolveView(r)
	rd, err := h.readerData(ctx, vc, item)
	if err != nil {
		h.fail(w, "load article", err)
		return
	}
	h.renderComponent(w, r, templates.Reader(rd))
}

// ---------- feed management ----------

func (h *Handler) handleAddFeedModal(w http.ResponseWriter, r *http.Request) {
	folders, err := h.store.Folders(r.Context())
	if err != nil {
		h.fail(w, "list folders", err)
		return
	}
	h.renderComponent(w, r, templates.AddFeedModal(folders))
}

func (h *Handler) handleProbe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	// Validate before the url is ever shown back or stored: the probe pane is
	// the last point at which the reader can be told the address is unusable.
	feedURL, err := httpx.ValidateFeedURL(r.FormValue("url"))
	if err != nil {
		h.logger.Warn("probe: rejected url", slog.Any("error", err))
		http.Error(w, "that does not look like an http(s) feed address", http.StatusBadRequest)
		return
	}

	folders, err := h.store.Folders(r.Context())
	if err != nil {
		h.fail(w, "list folders", err)
		return
	}
	h.renderComponent(w, r, templates.ProbeResults(feedURL, folders))
}

// resolveFolder reads the folder picker: either an existing folder id, or
// "__new__" plus the name of a folder to create.
func (h *Handler) resolveFolder(ctx context.Context, r *http.Request) (string, error) {
	folder := strings.TrimSpace(r.FormValue("folder"))
	if folder != "__new__" {
		return folder, nil
	}
	return h.folderIDForName(ctx, r.FormValue("new_folder"))
}

func (h *Handler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	// Every branch below that used to be a silent no-op is now a 4xx. The
	// modal renders a hidden error region and reveals it on a non-2xx, so a
	// subscribe that adds nothing finally says so instead of closing over a
	// freshly rendered sidebar that looks exactly as it did before.
	feedURL, err := httpx.ValidateFeedURL(r.FormValue("feed_url"))
	if err != nil {
		h.logger.Warn("subscribe: rejected url", slog.Any("error", err))
		http.Error(w, "that does not look like an http(s) feed address", http.StatusBadRequest)
		return
	}

	folder, err := h.resolveFolder(ctx, r)
	if err != nil {
		if errors.Is(err, errFolderNameRequired) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.fail(w, "resolve folder", err)
		return
	}
	if folder == "" {
		http.Error(w, "choose a folder for this feed", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = feedURL
	}

	// store.FeedID replaces the local derivation, which stripped the scheme,
	// swapped "/" for "-" and cut the result to 32 bytes: two feeds on one host
	// with long paths collided onto one id and the upsert replaced the first
	// with the second (finding 10).
	id := store.FeedID(feedURL)

	// Subscribing twice is an upsert, so it used to succeed silently and look
	// exactly like a fresh subscription — the sidebar came back unchanged and
	// the modal closed on it. Say so instead.
	if _, err := h.store.FeedByID(ctx, id); err == nil {
		http.Error(w, "you are already subscribed to this feed", http.StatusConflict)
		return
	}

	err = h.store.AddFeed(ctx, store.Feed{
		ID:     id,
		Title:  title,
		URL:    feedURL,
		Folder: folder,
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicateFeedURL) {
			http.Error(w, "you are already subscribed to this feed", http.StatusConflict)
			return
		}
		h.fail(w, "add feed", err)
		return
	}

	vc := h.resolveView(r)
	sd, err := h.sidebarData(ctx, vc)
	if err != nil {
		h.fail(w, "load sidebar", err)
		return
	}
	h.renderComponent(w, r, templates.Sidebar(sd))
}

func (h *Handler) handleEditFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	feed, err := h.store.FeedByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	folders, err := h.store.Folders(ctx)
	if err != nil {
		h.fail(w, "list folders", err)
		return
	}
	h.renderComponent(w, r, templates.ManageFeedEdit(*feed, folders))
}

func (h *Handler) handleConfirmDeleteFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	feed, err := h.store.FeedByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	h.renderComponent(w, r, templates.ManageFeedConfirmDelete(*feed))
}

func (h *Handler) handleUpdateFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	feed, err := h.store.FeedByID(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if title := strings.TrimSpace(r.FormValue("title")); title != "" {
		feed.Title = title
	}
	if raw := strings.TrimSpace(r.FormValue("url")); raw != "" {
		cleaned, err := httpx.ValidateFeedURL(raw)
		if err != nil {
			h.logger.Warn("update feed: rejected url", slog.String("feed", id), slog.Any("error", err))
			http.Error(w, "that does not look like an http(s) feed address", http.StatusBadRequest)
			return
		}
		// The id stays as it is on purpose: it is the foreign key every stored
		// item points at, so re-deriving it from the new url would orphan the
		// whole archive.
		feed.URL = cleaned
	}

	folder, err := h.resolveFolder(ctx, r)
	if err != nil {
		if errors.Is(err, errFolderNameRequired) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.fail(w, "resolve folder", err)
		return
	}

	moved := folder != "" && folder != feed.Folder
	if folder != "" {
		feed.Folder = folder
	}

	if err := h.store.UpsertFeed(ctx, *feed); err != nil {
		if errors.Is(err, store.ErrDuplicateFeedURL) {
			http.Error(w, "another feed already uses that url", http.StatusConflict)
			return
		}
		h.fail(w, "update feed", err)
		return
	}

	if moved {
		// items carries a denormalised folder column that the list filters on.
		// Updating only the feed row left every existing item filed under the
		// old folder, so the feed moved and its articles did not — and
		// deleting the old folder then took them with it (finding 11).
		// MoveFeed is not redundant with UpsertFeed: UpsertFeed only migrates
		// items when the feed had a previous folder, so a feed moving out of
		// "unfiled" would still leave its items behind.
		if err := h.store.MoveFeed(ctx, feed.ID, feed.Folder); err != nil {
			h.fail(w, "move feed", err)
			return
		}
	}

	h.renderManage(w, r, h.routeView(r, "view", "manage"))
}

func (h *Handler) handleDeleteFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	if err := h.store.DeleteFeed(ctx, id); err != nil {
		h.logger.Error("delete feed", slog.String("id", id), slog.Any("error", err))
		http.Error(w, "failed to delete feed", http.StatusInternalServerError)
		return
	}

	h.renderManage(w, r, h.routeView(r, "view", "manage"))
}

// ---------- helpers ----------

// fail logs a server-side failure and returns a 500. The message is what the
// reader sees in the error toast, so it names the operation and nothing else;
// the detail goes to the log.
func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.logger.Error(what, slog.Any("error", err))
	http.Error(w, "failed to "+what, http.StatusInternalServerError)
}

// renderComponent writes a component and — unlike before — notices when that
// fails. Nothing can be done about the response once part of the body is on the
// wire, but a truncated pane should not be invisible in the logs.
func (h *Handler) renderComponent(w http.ResponseWriter, r *http.Request, c templ.Component) {
	if err := c.Render(r.Context(), w); err != nil {
		h.logRenderError(r, err)
	}
}

func (h *Handler) logRenderError(r *http.Request, err error) {
	// A client that navigated away mid-response cancels the context. That is
	// the browser's business, not a server fault, so it is not an error.
	if errors.Is(err, context.Canceled) {
		h.logger.Debug("render cancelled by client", slog.String("path", r.URL.Path))
		return
	}
	h.logger.Error("render failed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err))
}

func (h *Handler) renderManage(w http.ResponseWriter, r *http.Request, vc viewCtx) {
	md, err := h.manageData(r.Context())
	if err != nil {
		h.fail(w, "load feed management", err)
		return
	}
	h.renderComponent(w, r, templates.Manage(md))
	h.renderOOBSidebar(w, r, vc)
	h.renderOOBStatus(w, r, vc)
}

// The OOB helpers run after the main body is already committed, so a failure
// here can only be logged: the swap is skipped and the pane it targets keeps
// its previous contents rather than the response being torn in half.
func (h *Handler) renderOOBSidebar(w http.ResponseWriter, r *http.Request, vc viewCtx) {
	sd, err := h.sidebarData(r.Context(), vc)
	if err != nil {
		h.logger.Error("oob sidebar", slog.Any("error", err))
		return
	}
	if err := templates.OOBSidebar(sd).Render(r.Context(), w); err != nil {
		h.logRenderError(r, err)
	}
}

func (h *Handler) renderOOBStatus(w http.ResponseWriter, r *http.Request, vc viewCtx) {
	sd, err := h.statusData(r.Context(), vc)
	if err != nil {
		h.logger.Error("oob status", slog.Any("error", err))
		return
	}
	if err := templates.OOBStatus(sd).Render(r.Context(), w); err != nil {
		h.logRenderError(r, err)
	}
}

func (h *Handler) renderOOBReaderReset(w http.ResponseWriter, r *http.Request) {
	if err := templates.OOBReaderReset().Render(r.Context(), w); err != nil {
		h.logRenderError(r, err)
	}
}

func (h *Handler) renderList(w http.ResponseWriter, r *http.Request, vc viewCtx) {
	ld, err := h.listData(r.Context(), vc)
	if err != nil {
		h.fail(w, "load items", err)
		return
	}
	h.renderComponent(w, r, templates.ItemList(ld))
}

func (h *Handler) renderFullPage(w http.ResponseWriter, r *http.Request, vc viewCtx, activeItem *store.Item) {
	ctx := r.Context()

	sidebar, err := h.sidebarData(ctx, vc)
	if err != nil {
		h.fail(w, "load sidebar", err)
		return
	}
	list, err := h.listData(ctx, vc)
	if err != nil {
		h.fail(w, "load items", err)
		return
	}
	reader, err := h.readerData(ctx, vc, activeItem)
	if err != nil {
		h.fail(w, "load article", err)
		return
	}
	// The filtered count is exactly the list we just built — the old code ran
	// the same ListItems query a third time to recompute it.
	status, err := h.statusDataFor(ctx, vc, len(list.Items))
	if err != nil {
		h.fail(w, "load status", err)
		return
	}
	manage, err := h.manageData(ctx)
	if err != nil {
		h.fail(w, "load feed management", err)
		return
	}

	pd := templates.PageData{
		Sidebar:  sidebar,
		List:     list,
		Reader:   reader,
		Status:   status,
		Manage:   manage,
		Settings: h.settingsData(),
	}

	h.renderComponent(w, r, templates.Page(pd))
}

func (h *Handler) listFilter(vc viewCtx) store.ListFilter {
	return store.ListFilter{
		ViewKind: vc.State.Kind,
		ViewID:   vc.State.ID,
		Query:    vc.Query,
		Sort:     vc.Sort,
	}
}

func (h *Handler) readerData(ctx context.Context, vc viewCtx, item *store.Item) (templates.ReaderData, error) {
	rd := templates.ReaderData{Item: item}
	if item == nil {
		return rd, nil
	}

	feed, err := h.store.FeedByID(ctx, item.FeedID)
	if err != nil {
		// An item whose feed row has been deleted still renders; the reader
		// simply shows no source label. Worth a line in the log because it
		// means a delete left an item behind.
		h.logger.Warn("reader: feed not found for item",
			slog.String("item", item.ID), slog.String("feed", item.FeedID), slog.Any("error", err))
	} else {
		rd.Feed = feed
	}

	// Prev/next follow the list the reader is actually looking at, which is
	// why readerData needs the request's view rather than a server-wide one.
	items, err := h.store.ListItems(ctx, h.listFilter(vc))
	if err != nil {
		return rd, fmt.Errorf("list items for reader navigation: %w", err)
	}
	for i, it := range items {
		if it.ID != item.ID {
			continue
		}
		if i > 0 {
			rd.PrevID = items[i-1].ID
		}
		if i < len(items)-1 {
			rd.NextID = items[i+1].ID
		}
		break
	}

	return rd, nil
}

func (h *Handler) sidebarData(ctx context.Context, vc viewCtx) (templates.SidebarData, error) {
	folders, err := h.store.Folders(ctx)
	if err != nil {
		return templates.SidebarData{}, fmt.Errorf("list folders: %w", err)
	}
	feeds, err := h.store.Feeds(ctx)
	if err != nil {
		return templates.SidebarData{}, fmt.Errorf("list feeds: %w", err)
	}
	counts, err := h.store.Counts(ctx)
	if err != nil {
		return templates.SidebarData{}, fmt.Errorf("count items: %w", err)
	}
	return templates.SidebarData{
		Folders: folders,
		Feeds:   feeds,
		Counts:  counts,
		View:    vc.State,
	}, nil
}

func (h *Handler) settingsData() templates.SettingsData {
	var dbInfo string
	switch h.cfg.DatabaseDriver {
	case "postgres":
		dbInfo = fmt.Sprintf("%s:%s/%s", h.cfg.DatabaseHost, h.cfg.DatabasePort, h.cfg.DatabaseName)
	default:
		dbInfo = h.cfg.DatabasePath
	}
	return templates.SettingsData{
		DatabaseDriver:  h.cfg.DatabaseDriver,
		DatabaseInfo:    dbInfo,
		RefreshInterval: h.cfg.RefreshInterval.String(),
		MarkReadOn:      h.cfg.MarkReadOn,
		Retention:       h.cfg.Retention,
		Density:         h.cfg.Density,
	}
}

func (h *Handler) manageData(ctx context.Context) (templates.ManageData, error) {
	feeds, err := h.store.Feeds(ctx)
	if err != nil {
		return templates.ManageData{}, fmt.Errorf("list feeds: %w", err)
	}
	folders, err := h.store.Folders(ctx)
	if err != nil {
		return templates.ManageData{}, fmt.Errorf("list folders: %w", err)
	}
	return templates.ManageData{Feeds: feeds, Folders: folders}, nil
}

func (h *Handler) listData(ctx context.Context, vc viewCtx) (templates.ListData, error) {
	feeds, err := h.store.Feeds(ctx)
	if err != nil {
		return templates.ListData{}, fmt.Errorf("list feeds: %w", err)
	}
	items, err := h.store.ListItems(ctx, h.listFilter(vc))
	if err != nil {
		return templates.ListData{}, fmt.Errorf("list items: %w", err)
	}

	return templates.ListData{
		Items: items,
		Feeds: feeds,
		View:  vc.State,
		Sort:  vc.Sort,
		Title: h.viewTitle(ctx, vc),
		Crumb: h.viewCrumb(ctx, vc),
	}, nil
}

func (h *Handler) statusData(ctx context.Context, vc viewCtx) (templates.StatusData, error) {
	items, err := h.store.ListItems(ctx, h.listFilter(vc))
	if err != nil {
		return templates.StatusData{}, fmt.Errorf("list items: %w", err)
	}
	return h.statusDataFor(ctx, vc, len(items))
}

func (h *Handler) statusDataFor(ctx context.Context, vc viewCtx, filtered int) (templates.StatusData, error) {
	counts, err := h.store.Counts(ctx)
	if err != nil {
		return templates.StatusData{}, fmt.Errorf("count items: %w", err)
	}
	feedCount, err := h.store.FeedCount(ctx)
	if err != nil {
		return templates.StatusData{}, fmt.Errorf("count feeds: %w", err)
	}

	return templates.StatusData{
		ViewTitle:   h.viewTitle(ctx, vc),
		FilteredLen: filtered,
		TotalLen:    counts.All,
		UnreadCount: counts.Unread,
		FeedCount:   feedCount,
		LastSync:    formatSyncAge(time.Since(h.lastSyncAt())),
	}, nil
}

func formatSyncAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("last sync %ds ago", int(d.Seconds()))
	}
	return fmt.Sprintf("last sync %dm ago", int(d.Minutes()))
}

// viewTitle and viewCrumb resolve a label for the current view. A lookup that
// finds nothing is a stale bookmark or a just-deleted feed, not a server
// failure, so both fall back to something printable rather than failing the
// whole render — but the miss is logged, because it used to be discarded.
func (h *Handler) viewTitle(ctx context.Context, vc viewCtx) string {
	switch vc.State.Kind {
	case "feed":
		f, err := h.store.FeedByID(ctx, vc.State.ID)
		if err != nil {
			h.logger.Warn("view title: feed not found", slog.String("feed", vc.State.ID), slog.Any("error", err))
			return vc.State.ID
		}
		return f.Title
	case "folder":
		folders, err := h.store.Folders(ctx)
		if err != nil {
			h.logger.Error("view title: list folders", slog.Any("error", err))
			return vc.State.ID
		}
		for _, f := range folders {
			if f.ID == vc.State.ID {
				return f.Label
			}
		}
		return vc.State.ID
	case "view":
		switch vc.State.ID {
		case "all":
			return "all items"
		case "unread":
			return "unread"
		case "read":
			return "read"
		case "starred":
			return "starred"
		case "today":
			return "today"
		case "yesterday":
			return "yesterday"
		case "last-week":
			return "last week"
		case "last-month":
			return "last month"
		case "settings":
			return "settings"
		case "manage":
			return "manage feeds"
		}
	}
	return "index"
}

func (h *Handler) viewCrumb(ctx context.Context, vc viewCtx) string {
	switch vc.State.Kind {
	case "feed":
		f, err := h.store.FeedByID(ctx, vc.State.ID)
		if err != nil {
			h.logger.Warn("view crumb: feed not found", slog.String("feed", vc.State.ID), slog.Any("error", err))
			return "[ feed ]"
		}
		return fmt.Sprintf("[ feed / %s ]", f.Folder)
	case "folder":
		return fmt.Sprintf("[ folder / %s ]", vc.State.ID)
	case "view":
		return fmt.Sprintf("[ index / %s ]", vc.State.ID)
	}
	return ""
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.code = code
	sw.ResponseWriter.WriteHeader(code)
}

// formatDuration returns a human-readable duration string in ms or s.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// LogRequests logs failed requests.
//
// It used to log only status >= 500, which is precisely why every dropped
// write error in this package was invisible: a handler that swallowed a
// database failure and returned 200 produced no log line at all, and the 4xx
// responses that did tell the truth were silent too. Client errors are now
// logged at WARN, server errors at ERROR.
func LogRequests(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(sw, r)

			if sw.code < 400 {
				return
			}
			level := slog.LevelWarn
			if sw.code >= 500 {
				level = slog.LevelError
			}
			logger.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.code),
				slog.String("duration", formatDuration(time.Since(start))),
			)
		})
	}
}
