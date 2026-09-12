package store

// OPML import and export.
//
// Layering note, made on purpose rather than by accident: XML parsing lives in
// the persistence package. That is not where a parser belongs in the abstract,
// but there used to be two copies of this logic — one here and one in
// internal/handler/opml.go — and they had already drifted apart. The handler
// copy filed top-level feeds under the "default" folder without ever creating
// that folder, so with the foreign key on feeds.folder every one of those
// inserts failed, the error was discarded, and the user was told "imported N
// feeds" when nothing had been saved. One implementation that owns both the
// parse and the write is the cheapest way to make that class of divergence
// impossible. The HTTP layer is left with transport concerns only.

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kontrolplane/feed/internal/httpx"
)

const (
	// DefaultFolderID is where feeds that are not inside any folder land. It
	// must exist as a row in `folders` before any feed referencing it is
	// inserted, or the foreign key rejects the insert.
	DefaultFolderID = "default"

	// maxOutlineDepth bounds how far we descend into nested <outline>
	// elements. OPML permits arbitrary nesting, so a hostile (or merely
	// broken) file can otherwise drive unbounded work. The parser is
	// iterative, so this is a work limit rather than a stack limit.
	maxOutlineDepth = 16

	// maxImportEntries bounds how many feeds a single file may describe.
	maxImportEntries = 20000

	// maxRecordedErrors bounds the per-entry error slice. Skipped still counts
	// every failure; only the detail is capped, so a file full of junk cannot
	// be turned into an allocation attack.
	maxRecordedErrors = 200

	maxFolderIDRunes = 64
	maxTitleRunes    = 500
)

// ErrInvalidOPML wraps every failure to parse an OPML document. Callers use it
// to tell "the user handed us garbage" (a 400) apart from "the database is
// unhappy" (a 500).
var ErrInvalidOPML = errors.New("store: invalid OPML")

var (
	errUnsupportedURL = errors.New("feed url is not an http(s) url")
	errTooDeep        = errors.New("outline nested deeper than the import limit")
	errTooManyEntries = errors.New("file describes more feeds than the import limit")
)

// ImportResult reports what an import actually did. The counts are derived
// from the writes that succeeded, not from the number of entries seen, because
// reporting the latter is exactly the bug this type exists to prevent.
type ImportResult struct {
	FeedsAdded     int
	FeedsUpdated   int
	FoldersCreated int
	// Skipped counts every entry that was not stored, whether it was rejected
	// before persistence (unsafe URL, depth limit) or failed on write.
	Skipped int
	// Errors holds the detail for the first maxRecordedErrors skips.
	Errors []ImportError
}

// ImportError is one entry that did not make it into the database.
type ImportError struct {
	Title string
	URL   string
	Err   error
}

func (e ImportError) Error() string {
	switch {
	case e.URL != "" && e.Title != "":
		return fmt.Sprintf("%s (%s): %v", e.Title, e.URL, e.Err)
	case e.URL != "":
		return fmt.Sprintf("%s: %v", e.URL, e.Err)
	default:
		return e.Err.Error()
	}
}

func (e ImportError) Unwrap() error { return e.Err }

// Summary renders a short human-readable line for the UI. The old handler said
// "imported N feeds" regardless of what happened; this says what happened.
func (r ImportResult) Summary() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%d feeds added", r.FeedsAdded))
	if r.FeedsUpdated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", r.FeedsUpdated))
	}
	if r.FoldersCreated > 0 {
		parts = append(parts, fmt.Sprintf("%d folders created", r.FoldersCreated))
	}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", r.Skipped))
	}
	return strings.Join(parts, ", ")
}

// ---------- import ----------

// ImportOPML parses an OPML document from r and persists the folders and feeds
// it describes.
//
// A returned error means nothing was imported: the document could not be
// parsed (wrapping ErrInvalidOPML) or the database could not be read. Entries
// that individually fail — an unsafe URL, a rejected insert — do not abort the
// import; they are counted in ImportResult.Skipped and described in
// ImportResult.Errors so the caller can tell the user the truth.
func ImportOPML(ctx context.Context, s *Store, r io.Reader) (ImportResult, error) {
	plan, err := parseOPML(r)
	if err != nil {
		return ImportResult{}, err
	}
	return applyImportPlan(ctx, s, plan)
}

// ImportOPMLFile imports the OPML file at path. It is the startup path used by
// FEED_FEEDS_FILE.
//
// The signature stays (int, error) because cmd/server logs the count directly;
// the int is now the number of feeds genuinely written (added + updated)
// rather than the number of lines in the file. Per-entry failures are logged
// rather than returned, because the caller exits the process on error and one
// malformed entry in a feeds file should not stop the server from booting.
func ImportOPMLFile(ctx context.Context, s *Store, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open feeds file: %w", err)
	}
	// Close on a read-only file can only report deferred I/O errors we have
	// already observed through Read, so there is nothing actionable here.
	defer func() { _ = f.Close() }()

	res, err := ImportOPML(ctx, s, f)
	if err != nil {
		return 0, fmt.Errorf("import feeds file %q: %w", path, err)
	}

	for _, ie := range res.Errors {
		slog.Default().Error("import opml: entry skipped",
			slog.String("path", path),
			slog.String("url", ie.URL),
			slog.Any("error", ie.Err))
	}
	if res.Skipped > len(res.Errors) {
		slog.Default().Error("import opml: further entries skipped",
			slog.String("path", path),
			slog.Int("skipped", res.Skipped),
			slog.Int("reported", len(res.Errors)))
	}

	return res.FeedsAdded + res.FeedsUpdated, nil
}

// plannedFeed is one feed together with the folder it belongs in. The folder
// is carried alongside the feed rather than created up front so that folders
// are only written when something actually goes in them.
type plannedFeed struct {
	FolderID    string
	FolderLabel string
	Feed        Feed
}

// importPlan is the pure result of parsing: what we would write, and what we
// already know we cannot. It exists so the parser can be tested without a
// database.
type importPlan struct {
	Feeds   []plannedFeed
	Skipped int
	Errors  []ImportError
}

func (p *importPlan) fail(title, url string, err error) {
	p.Skipped++
	if len(p.Errors) < maxRecordedErrors {
		p.Errors = append(p.Errors, ImportError{Title: title, URL: url, Err: err})
	}
}

// applyImportPlan writes a plan to the store.
//
// Folders are created lazily, immediately before the first feed that needs
// them. That is the fix for the "imported N feeds, saved zero" bug: there is
// no path — top-level feed, nested feed, or file import — that can reach an
// INSERT on feeds without the referenced folder row already existing.
func applyImportPlan(ctx context.Context, s *Store, plan importPlan) (ImportResult, error) {
	res := ImportResult{Skipped: plan.Skipped, Errors: plan.Errors}

	existingFolders, err := s.Folders(ctx)
	if err != nil {
		return res, fmt.Errorf("read folders: %w", err)
	}
	folders := make(map[string]bool, len(existingFolders)+4)
	for _, f := range existingFolders {
		folders[f.ID] = true
	}
	pos := len(existingFolders)

	// The existing feed ids decide whether a write counts as an add or an
	// update. Reading them once beats a SELECT per entry, and it means the
	// counts do not depend on how the store reports "no such row".
	existingFeeds, err := s.Feeds(ctx)
	if err != nil {
		return res, fmt.Errorf("read feeds: %w", err)
	}
	feeds := make(map[string]bool, len(existingFeeds)+len(plan.Feeds))
	for _, f := range existingFeeds {
		feeds[f.ID] = true
	}

	for _, pf := range plan.Feeds {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		if !folders[pf.FolderID] {
			if err := s.UpsertFolder(ctx, Folder{ID: pf.FolderID, Label: pf.FolderLabel}, pos); err != nil {
				// Without the folder the feed insert is guaranteed to violate
				// the foreign key, so do not even try: record one honest
				// failure instead of two confusing ones.
				res.Skipped++
				appendErr(&res, ImportError{Title: pf.Feed.Title, URL: pf.Feed.URL,
					Err: fmt.Errorf("create folder %q: %w", pf.FolderID, err)})
				continue
			}
			folders[pf.FolderID] = true
			pos++
			res.FoldersCreated++
		}

		if err := s.AddFeed(ctx, pf.Feed); err != nil {
			res.Skipped++
			appendErr(&res, ImportError{Title: pf.Feed.Title, URL: pf.Feed.URL, Err: err})
			continue
		}

		if feeds[pf.Feed.ID] {
			res.FeedsUpdated++
		} else {
			res.FeedsAdded++
			feeds[pf.Feed.ID] = true
		}
	}

	return res, nil
}

func appendErr(res *ImportResult, ie ImportError) {
	if len(res.Errors) < maxRecordedErrors {
		res.Errors = append(res.Errors, ie)
	}
}

// ---------- parsing ----------

// outlineFrame is one open <outline> element. Each frame carries the folder
// its children belong to, which is its own slug when the outline is a group
// and the inherited folder when it is a feed.
type outlineFrame struct {
	folderID    string
	folderLabel string
}

// parseOPML turns an OPML document into an importPlan.
//
// It walks the token stream rather than unmarshalling into a recursive struct.
// Unmarshalling recurses once per nesting level, so a file with a few hundred
// thousand nested <outline> elements — well within the upload cap — would
// recurse that deep before any limit of ours could apply. An explicit stack
// makes the depth limit enforceable at the point it is exceeded.
//
// Nesting beyond one level is flattened: the innermost named group wins,
// because the data model has flat folders. A feed in <outline text="a"><outline
// text="b"><outline xmlUrl=.../> lands in folder "b".
func parseOPML(r io.Reader) (importPlan, error) {
	var plan importPlan

	dec := xml.NewDecoder(r)
	dec.CharsetReader = charsetReader

	var (
		stack   []outlineFrame
		sawAny  bool
		sawRoot bool
		inBody  bool
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return plan, fmt.Errorf("%w: %v", ErrInvalidOPML, err)
		}
		sawAny = true

		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)

			if !sawRoot {
				if name != "opml" {
					return plan, fmt.Errorf("%w: root element is <%s>, expected <opml>", ErrInvalidOPML, t.Name.Local)
				}
				sawRoot = true
				continue
			}

			switch {
			case name == "body" && !inBody && len(stack) == 0:
				inBody = true

			case name == "outline" && inBody:
				if len(stack) >= maxOutlineDepth {
					plan.fail("", "", errTooDeep)
					// Skip consumes the whole subtree including its end
					// element, so nothing is pushed and nothing must be popped.
					if err := dec.Skip(); err != nil {
						return plan, fmt.Errorf("%w: %v", ErrInvalidOPML, err)
					}
					continue
				}
				frame, stop := handleOutline(&plan, t, currentFolder(stack))
				if stop {
					return plan, nil
				}
				stack = append(stack, frame)
			}

		case xml.EndElement:
			name := strings.ToLower(t.Name.Local)
			switch {
			case name == "outline" && len(stack) > 0:
				stack = stack[:len(stack)-1]
			case name == "body" && len(stack) == 0:
				inBody = false
			}
		}
	}

	if !sawAny {
		return plan, fmt.Errorf("%w: document is empty", ErrInvalidOPML)
	}
	if !sawRoot {
		return plan, fmt.Errorf("%w: no <opml> element found", ErrInvalidOPML)
	}

	return plan, nil
}

// currentFolder returns the folder feeds at the current position belong to.
// An empty stack means top level, which is the default folder — and, unlike
// the old handler, something that will actually be created.
func currentFolder(stack []outlineFrame) outlineFrame {
	if len(stack) == 0 {
		return outlineFrame{folderID: DefaultFolderID, folderLabel: DefaultFolderID}
	}
	return stack[len(stack)-1]
}

// handleOutline classifies one <outline> and returns the folder scope its
// children inherit. stop is true when the entry limit has been reached.
func handleOutline(plan *importPlan, el xml.StartElement, parent outlineFrame) (frame outlineFrame, stop bool) {
	raw := attrValue(el, "xmlUrl")
	title := firstNonEmpty(attrValue(el, "title"), attrValue(el, "text"))

	// No xmlUrl: a grouping node. Its label names a folder for whatever is
	// inside it. Nothing is written yet — an empty group creates no folder.
	if raw == "" {
		label := firstNonEmpty(attrValue(el, "text"), attrValue(el, "title"))
		id := slugify(label)
		if id == "" {
			return parent, false
		}
		return outlineFrame{folderID: id, folderLabel: truncRunes(label, maxFolderIDRunes)}, false
	}

	if len(plan.Feeds) >= maxImportEntries {
		plan.fail(title, raw, errTooManyEntries)
		return parent, true
	}

	// Every URL is validated before it reaches the database. An OPML file is
	// user-supplied and can just as easily contain file:///etc/passwd or a
	// javascript: URL as an http one; the fetcher would later hand whatever it
	// found straight to the reader.
	clean, err := httpx.ValidateFeedURL(raw)
	if err != nil {
		plan.fail(title, raw, fmt.Errorf("%w: %v", errUnsupportedURL, err))
		return parent, false
	}

	if title == "" {
		title = clean
	}

	plan.Feeds = append(plan.Feeds, plannedFeed{
		FolderID:    parent.folderID,
		FolderLabel: parent.folderLabel,
		Feed: Feed{
			ID:      FeedID(clean),
			Title:   truncRunes(title, maxTitleRunes),
			URL:     clean,
			Folder:  parent.folderID,
			SiteURL: strings.TrimSpace(attrValue(el, "htmlUrl")),
		},
	})

	// A feed outline with children is unusual but legal. Its children inherit
	// the enclosing folder rather than being dropped on the floor.
	return parent, false
}

// attrValue looks up an attribute by local name, case-insensitively: real
// files in the wild write xmlUrl, xmlurl and XMLURL.
func attrValue(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if strings.EqualFold(a.Name.Local, name) {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// slugify turns a folder label into an id: lower-cased, with runs of anything
// that is not a letter or digit collapsed to a single dash. Letters outside
// ASCII are kept, so a non-English folder name does not collapse to nothing.
func slugify(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return truncRunes(b.String(), maxFolderIDRunes)
}

// truncRunes cuts s to at most n runes. Byte slicing here would split a
// multi-byte rune and store invalid UTF-8.
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// charsetReader handles the encodings that turn up in OPML exported by older
// readers. Anything else is refused with a clear message rather than the
// decoder's bare "CharsetReader is nil".
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		return &latin1Reader{br: bufio.NewReader(input)}, nil
	default:
		return nil, fmt.Errorf("unsupported character encoding %q", label)
	}
}

// latin1Reader re-encodes ISO-8859-1 input as UTF-8 on the fly.
type latin1Reader struct {
	br      *bufio.Reader
	pending []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(l.pending) > 0 {
			c := copy(p[n:], l.pending)
			n += c
			l.pending = l.pending[c:]
			continue
		}
		b, err := l.br.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b < utf8.RuneSelf {
			p[n] = b
			n++
			continue
		}
		var enc [4]byte
		w := utf8.EncodeRune(enc[:], rune(b))
		c := copy(p[n:], enc[:w])
		n += c
		if c < w {
			l.pending = append(l.pending[:0], enc[c:w]...)
		}
	}
	return n, nil
}

// ---------- export ----------

type opmlDocument struct {
	XMLName xml.Name    `xml:"opml"`
	Version string      `xml:"version,attr"`
	Head    opmlHead    `xml:"head"`
	Body    opmlBodyXML `xml:"body"`
}

type opmlHead struct {
	Title       string `xml:"title"`
	DateCreated string `xml:"dateCreated,omitempty"`
}

type opmlBodyXML struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string        `xml:"htmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline,omitempty"`
}

// ExportOPML renders folders and feeds as an OPML 2.0 document, ready to be
// written to the response. It builds the whole document in memory rather than
// streaming it: a document this small is not worth the risk of failing halfway
// through a response that has already been committed with a 200.
//
// Attribute escaping is left to encoding/xml, which is the only thing that
// gets it right for quotes, angle brackets and control characters in feed
// titles.
//
// Empty folders are omitted. That matches the importer, which creates a folder
// only when a feed goes in it, so emitting them would not round-trip anyway.
func ExportOPML(folders []Folder, feeds []Feed) ([]byte, error) {
	doc := opmlDocument{
		Version: "2.0",
		Head: opmlHead{
			Title:       "kontrolplane/feed",
			DateCreated: time.Now().UTC().Format(time.RFC1123Z),
		},
	}

	grouped := make(map[string]bool, len(feeds))
	for _, folder := range folders {
		group := opmlOutline{Text: folder.Label, Title: folder.Label}
		for _, feed := range feeds {
			if feed.Folder != folder.ID {
				continue
			}
			grouped[feed.ID] = true
			group.Outlines = append(group.Outlines, feedOutline(feed))
		}
		if len(group.Outlines) > 0 {
			doc.Body.Outlines = append(doc.Body.Outlines, group)
		}
	}

	// A feed whose folder does not exist should be impossible now that the
	// foreign key is enforced, but exporting is the last chance to notice it.
	// Emitting it at top level loses the folder name; silently dropping it
	// would lose the feed.
	for _, feed := range feeds {
		if !grouped[feed.ID] {
			doc.Body.Outlines = append(doc.Body.Outlines, feedOutline(feed))
		}
	}

	var buf strings.Builder
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode opml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode opml: %w", err)
	}
	buf.WriteString("\n")

	return []byte(buf.String()), nil
}

func feedOutline(f Feed) opmlOutline {
	title := f.Title
	if title == "" {
		title = f.URL
	}
	return opmlOutline{
		Text:    title,
		Title:   title,
		Type:    "rss",
		XMLURL:  f.URL,
		HTMLURL: f.SiteURL,
	}
}
