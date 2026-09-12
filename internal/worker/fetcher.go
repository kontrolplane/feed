package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/kontrolplane/feed/internal/configuration"
	"github.com/kontrolplane/feed/internal/httpx"
	"github.com/kontrolplane/feed/internal/sanitize"
	"github.com/kontrolplane/feed/internal/store"
	"github.com/mmcdole/gofeed"
	"golang.org/x/sync/errgroup"
)

const (
	// feedFetchTimeout bounds a single feed download. Before this existed the
	// parser used gofeed's default bare &http.Client{}, which has no Timeout at
	// all, so one unresponsive feed server blocked the serial refresh loop
	// forever and silently stopped refreshes for every other feed too.
	feedFetchTimeout = 30 * time.Second

	// articleFetchTimeout bounds a single article download for readability.
	articleFetchTimeout = 20 * time.Second

	// extractConcurrency caps how many articles are pulled at once. Serial
	// extraction made a refresh cycle outlast the refresh interval on any feed
	// with more than a handful of new entries; unbounded concurrency would
	// instead hammer the upstream site. A small pool is the middle ground.
	extractConcurrency = 6

	// abstractRunes / tagRunes are counted in runes, never bytes - see
	// sanitize.TruncateRunes for why.
	abstractRunes = 300
	tagRunes      = 20

	// wordsPerMinute is the reading-speed assumption behind the time estimate.
	wordsPerMinute = 200

	// defaultMinutes is the estimate used when there is too little text to
	// measure. Preserved from the original implementation so the UI does not
	// suddenly start showing "0 min" on short entries.
	defaultMinutes = 3

	userAgent = "kontrolplane-feed/1.0 (+https://github.com/kontrolplane/feed)"
)

// Errors RefetchItem returns so the HTTP handler can tell the failure modes
// apart and show the reader something more useful than "internal error".
var (
	ErrItemNotFound    = errors.New("worker: item not found")
	ErrItemHasNoLink   = errors.New("worker: item has no link to re-fetch")
	ErrEmptyExtraction = errors.New("worker: extraction produced no content")
)

type Fetcher struct {
	store    *store.Store
	logger   *slog.Logger
	interval time.Duration
	onSync   func(time.Time)

	// retention is how long a read item is kept. Zero means "forever" and
	// disables pruning entirely.
	retention time.Duration

	// client is shared by every outbound request this package makes - feeds,
	// article extraction and user-triggered re-fetches alike. It refuses to
	// dial loopback/private/link-local addresses on every redirect hop, which
	// is the whole SSRF defence; nothing here may fall back to a plain
	// http.Client or to a library helper that builds its own.
	client *http.Client
}

// NewFetcher builds the background refresher. It takes the whole configuration
// rather than individual values because it needs both the refresh interval and
// the retention policy, and more knobs are likely to follow.
func NewFetcher(s *store.Store, logger *slog.Logger, cfg configuration.FeedServiceConfiguration, onSync func(time.Time)) *Fetcher {
	retention, err := ParseRetention(cfg.Retention)
	if err != nil {
		// An unparseable retention must not silently become "delete
		// everything"; fall back to keeping data and say so loudly.
		logger.Error("invalid retention configuration, keeping items forever",
			slog.String("retention", cfg.Retention),
			slog.Any("error", err),
		)
		retention = 0
	}

	return &Fetcher{
		store:     s,
		logger:    logger,
		interval:  cfg.RefreshInterval,
		onSync:    onSync,
		retention: retention,
		client:    httpx.SafeClient(feedFetchTimeout),
	}
}

// ParseRetention converts the RETENTION configuration value into a maximum age
// for read items. "forever" (and an empty value) return zero, meaning nothing
// is ever pruned. Day suffixes are accepted because time.ParseDuration has no
// unit larger than an hour.
func ParseRetention(v string) (time.Duration, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "", "forever", "never":
		return 0, nil
	}

	// "7d" / "30d" / "90d" - the values the settings UI offers - plus weeks for
	// good measure, since neither is a unit time.ParseDuration understands.
	if unit, ok := strings.CutSuffix(s, "d"); ok {
		return parseCount(unit, 24*time.Hour)
	}
	if unit, ok := strings.CutSuffix(s, "w"); ok {
		return parseCount(unit, 7*24*time.Hour)
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("retention %q: want a value like 7d, 30d, 90d or forever", v)
	}
	if d < 0 {
		return 0, fmt.Errorf("retention %q: must not be negative", v)
	}
	return d, nil
}

func parseCount(digits string, unit time.Duration) (time.Duration, error) {
	n, err := strconv.Atoi(strings.TrimSpace(digits))
	if err != nil {
		return 0, fmt.Errorf("retention: %q is not a whole number of units", digits)
	}
	if n < 0 {
		return 0, fmt.Errorf("retention: %d must not be negative", n)
	}
	return time.Duration(n) * unit, nil
}

// Start launches the refresh loop and returns a function that blocks until the
// loop has fully stopped. main must call it after cancelling ctx: previously
// Start was fire-and-forget, so the process could exit with an item write still
// in flight.
func (f *Fetcher) Start(ctx context.Context) (wait func()) {
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Initial fetch after a short delay so startup is not competing with
		// the first requests.
		select {
		case <-time.After(5 * time.Second):
			f.cycle(ctx)
		case <-ctx.Done():
			return
		}

		ticker := time.NewTicker(f.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				f.cycle(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	return wg.Wait
}

// cycle is one refresh pass: fetch every feed, then apply the retention policy.
func (f *Fetcher) cycle(ctx context.Context) {
	f.fetchAll(ctx)
	f.prune(ctx)

	if f.onSync != nil {
		f.onSync(time.Now())
	}
}

func (f *Fetcher) fetchAll(ctx context.Context) {
	feeds, err := f.store.Feeds(ctx)
	if err != nil {
		f.logger.Error("error listing feeds", slog.Any("error", err))
		return
	}

	parser := gofeed.NewParser()
	// Belt and braces: this fetcher reads bodies itself (so the size cap
	// actually applies) but gofeed would otherwise lazily construct a bare
	// client with no timeout if any code path ever reached for one.
	parser.Client = f.client

	var fetched, failed, saved int

	for _, feed := range feeds {
		// Without this a shutdown had to wait out every remaining feed - the
		// loop only ever noticed cancellation through whatever call happened to
		// be in flight.
		if err := ctx.Err(); err != nil {
			f.logger.Info("feed refresh interrupted",
				slog.Int("feeds_done", fetched+failed),
				slog.Int("feeds_total", len(feeds)),
			)
			return
		}

		n, err := f.refreshFeed(ctx, parser, feed)
		if err != nil {
			f.logger.Error("error fetching feed",
				slog.String("feed", feed.Title),
				slog.String("url", feed.URL),
				slog.Any("error", err),
			)
			failed++
			continue
		}
		fetched++
		saved += n
	}

	f.logger.Info("feed refresh complete",
		slog.Int("feeds", fetched),
		slog.Int("failed", failed),
		slog.Int("new_items", saved),
	)
}

// refreshFeed downloads one feed and stores the entries it has not seen before.
// It returns the number of items written.
func (f *Fetcher) refreshFeed(ctx context.Context, parser *gofeed.Parser, feed store.Feed) (int, error) {
	// Feed URLs come from OPML imports and the add-feed form, so they are
	// untrusted even though they are already in the database - re-validate on
	// every use rather than trusting whatever an older, laxer version stored.
	feedURL, err := httpx.ValidateFeedURL(feed.URL)
	if err != nil {
		return 0, fmt.Errorf("feed url: %w", err)
	}

	body, err := f.fetchBody(ctx, feedURL, feedFetchTimeout, "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.8")
	if err != nil {
		return 0, err
	}

	// Parse from the already-capped bytes rather than ParseURL, which would
	// re-download the feed through a path where MaxBodyBytes does not apply.
	parsed, err := parser.Parse(bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("parsing feed: %w", err)
	}

	now := time.Now()
	candidates := make([]store.Item, 0, len(parsed.Items))
	for _, entry := range parsed.Items {
		if entry == nil {
			continue
		}
		candidates = append(candidates, buildItem(feed, entry, now))
	}

	// The expensive article download happens only for entries we are actually
	// going to keep. The old order extracted every article on every cycle and
	// then threw the result away for anything already stored, re-downloading
	// every article of every feed every 15 minutes.
	fresh := filterKnown(candidates, func(id string) bool {
		exists, err := f.store.ItemExists(ctx, id)
		if err != nil {
			// Treat an unreadable database as "already have it": re-extracting
			// every article of every feed is the worst possible response to a
			// database outage, and the upsert would fail anyway.
			f.logger.Error("error checking whether item exists",
				slog.String("feed", feed.Title),
				slog.String("item", id),
				slog.Any("error", err),
			)
			return true
		}
		return exists
	})
	if len(fresh) == 0 {
		return 0, nil
	}

	f.extractAll(ctx, fresh)

	// Written serially and in feed order so the insert order matches the feed's
	// own ordering regardless of which extraction finished first.
	var saved int
	for _, item := range fresh {
		if err := ctx.Err(); err != nil {
			return saved, err
		}
		if err := f.store.UpsertItem(ctx, item); err != nil {
			f.logger.Error("error saving item",
				slog.String("feed", feed.Title),
				slog.String("item", item.ID),
				slog.String("link", item.Link),
				slog.Any("error", err),
			)
			continue
		}
		saved++
	}
	return saved, nil
}

// filterKnown drops the items an id-lookup reports as already stored, keeping
// the original order. Split out from refreshFeed so the skip decision is
// testable without a database.
func filterKnown(items []store.Item, known func(id string) bool) []store.Item {
	out := make([]store.Item, 0, len(items))
	for _, item := range items {
		if known(item.ID) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// extractAll fills in the full article body for every item that has a link,
// using a small worker pool. Items are updated in place.
func (f *Fetcher) extractAll(ctx context.Context, items []store.Item) {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(extractConcurrency)

	for i := range items {
		item := &items[i]
		if item.Link == "" {
			continue
		}
		g.Go(func() error {
			html, err := f.extractArticle(gctx, item.Link)
			if err != nil {
				// One unreachable article must not cost us the rest of the
				// feed, so this is logged and swallowed rather than returned:
				// returning would cancel gctx and abort every sibling.
				f.logger.Debug("article extraction failed",
					slog.String("url", item.Link),
					slog.Any("error", err),
				)
				return nil
			}
			// Keep the feed-supplied body when extraction came back empty -
			// something is better than a blank reader pane.
			if html == "" {
				return nil
			}
			item.Body = html
			item.Minutes = readingMinutes(html, item.Abstract)
			return nil
		})
	}

	// No goroutine above returns an error, so the only way Wait fails is the
	// parent context being cancelled mid-flight.
	if err := g.Wait(); err != nil {
		f.logger.Debug("article extraction cancelled", slog.Any("error", err))
	}
}

// extractArticle fetches link through the SSRF-safe client and runs readability
// over the result. readability.FromURL is deliberately not used: it builds its
// own http.Client, which would bypass both the blocked-address dialler and the
// response size cap.
func (f *Fetcher) extractArticle(ctx context.Context, link string) (string, error) {
	pageURL, err := httpx.ValidateFeedURL(link)
	if err != nil {
		return "", fmt.Errorf("article url: %w", err)
	}
	parsedURL, err := url.Parse(pageURL)
	if err != nil {
		return "", fmt.Errorf("article url: %w", err)
	}

	body, err := f.fetchBody(ctx, pageURL, articleFetchTimeout, "text/html, application/xhtml+xml;q=0.9, */*;q=0.8")
	if err != nil {
		return "", err
	}

	article, err := readability.FromReader(bytes.NewReader(body), parsedURL)
	if err != nil {
		return "", fmt.Errorf("readability: %w", err)
	}
	if article.Node == nil {
		return "", nil
	}

	var buf strings.Builder
	if err := article.RenderHTML(&buf); err != nil {
		return "", fmt.Errorf("rendering article: %w", err)
	}

	// Sanitised here, at the single choke point every extracted body passes
	// through, so no caller can forget. The reader template renders bodies with
	// templ.Raw, which escapes nothing.
	return sanitize.ArticleHTML(buf.String()), nil
}

// fetchBody performs one bounded GET: private-address-blocking client, its own
// deadline, and a hard cap on how much of the response is read into memory.
func (f *Fetcher) fetchBody(ctx context.Context, rawURL string, timeout time.Duration, accept string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	// Nothing actionable can be done about a failed body close.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	// Read one byte past the cap so an over-sized response is reported as an
	// error instead of being silently truncated into unparseable garbage.
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(body)) > httpx.MaxBodyBytes {
		return nil, fmt.Errorf("response exceeds %d byte limit", httpx.MaxBodyBytes)
	}
	return body, nil
}

// RefetchItem re-pulls a single item's article on demand - the reader's
// "re-fetch" button, for entries whose extraction came back empty or wrong. It
// is a user-triggered request to an arbitrary URL, so it goes through exactly
// the same safe client, timeout and size cap as the background loop.
//
// Read and starred flags survive because UpsertItem's conflict clause does not
// touch them.
func (f *Fetcher) RefetchItem(ctx context.Context, itemID string) error {
	// ItemByID surfaces the driver's raw "no rows" error, which differs between
	// sqlite and pgx, so an existence check comes first: that keeps "you asked
	// for an item that is not there" cleanly distinguishable from "the database
	// is broken", which is what the handler needs to pick a status code.
	exists, err := f.store.ItemExists(ctx, itemID)
	if err != nil {
		return fmt.Errorf("looking up item %s: %w", itemID, err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrItemNotFound, itemID)
	}

	item, err := f.store.ItemByID(ctx, itemID)
	if err != nil {
		return fmt.Errorf("loading item %s: %w", itemID, err)
	}
	if item == nil {
		return fmt.Errorf("%w: %s", ErrItemNotFound, itemID)
	}
	if strings.TrimSpace(item.Link) == "" {
		return fmt.Errorf("%w: %s", ErrItemHasNoLink, itemID)
	}

	html, err := f.extractArticle(ctx, item.Link)
	if err != nil {
		return fmt.Errorf("re-fetching %s: %w", item.Link, err)
	}
	if html == "" {
		return fmt.Errorf("%w: %s", ErrEmptyExtraction, item.Link)
	}

	item.Body = html
	item.Minutes = readingMinutes(html, item.Abstract)

	if err := f.store.UpsertItem(ctx, *item); err != nil {
		return fmt.Errorf("saving re-fetched item %s: %w", itemID, err)
	}

	f.logger.Info("re-fetched article",
		slog.String("item", itemID),
		slog.String("link", item.Link),
	)
	return nil
}

// prune applies the retention policy. RETENTION was configured and displayed in
// the settings UI but nothing ever deleted anything, so the database grew
// without bound while storing a full article body per item.
func (f *Fetcher) prune(ctx context.Context) {
	if f.retention <= 0 {
		return // "forever"
	}
	if ctx.Err() != nil {
		return
	}

	cutoff := time.Now().Add(-f.retention)
	removed, err := f.store.PruneReadItems(ctx, cutoff)
	if err != nil {
		f.logger.Error("error pruning read items",
			slog.Time("cutoff", cutoff),
			slog.Any("error", err),
		)
		return
	}
	if removed > 0 {
		f.logger.Info("pruned read items",
			slog.Int64("removed", removed),
			slog.String("retention", f.retention.String()),
			slog.Time("cutoff", cutoff),
		)
	}
}

// buildItem maps a feed entry onto a store.Item using only what the feed
// already gave us - no network access, so it stays cheap enough to run for
// every entry before the already-seen filter.
func buildItem(feed store.Feed, entry *gofeed.Item, now time.Time) store.Item {
	guid := entry.GUID
	if guid == "" {
		guid = entry.Link
	}
	if guid == "" {
		guid = entry.Title
	}

	date := now.Format("2006-01-02")
	if entry.PublishedParsed != nil {
		date = entry.PublishedParsed.Format("2006-01-02")
	} else if entry.UpdatedParsed != nil {
		date = entry.UpdatedParsed.Format("2006-01-02")
	}

	abstract := sanitize.Text(entry.Description)
	if abstract == "" {
		abstract = sanitize.Text(entry.Content)
	}
	abstract = sanitize.TruncateRunes(abstract, abstractRunes)

	// entry.Content is raw publisher-controlled HTML and previously went into
	// the database, and from there into templ.Raw, entirely unfiltered.
	body := sanitize.ArticleHTML(entry.Content)

	return store.Item{
		ID:     itemID(feed.ID, guid),
		FeedID: feed.ID,
		Folder: feed.Folder,
		// Title is stored as the feed supplied it; templ escapes it at render
		// time, so it needs no sanitising and stripping it would change what
		// the list view has always shown.
		Title:    entry.Title,
		Authors:  authorNames(entry),
		Date:     date,
		Tag:      guessTag(entry),
		Abstract: abstract,
		Body:     body,
		Link:     entry.Link,
		Minutes:  readingMinutes(body, abstract),
	}
}

// itemID derives the stable per-item primary key. The digest is truncated in
// binary and then hex-encoded rather than hex-encoded and then sliced, so no
// string is ever cut at a byte offset.
func itemID(feedID, guid string) string {
	sum := sha256.Sum256([]byte(feedID + ":" + guid))
	return hex.EncodeToString(sum[:8])
}

func authorNames(entry *gofeed.Item) string {
	if len(entry.Authors) > 0 {
		names := make([]string, 0, len(entry.Authors))
		for _, a := range entry.Authors {
			if a != nil && a.Name != "" {
				names = append(names, a.Name)
			}
		}
		if len(names) > 0 {
			return strings.Join(names, " · ")
		}
	}
	if entry.Author != nil {
		return entry.Author.Name
	}
	return ""
}

// readingMinutes estimates reading time from the article body, falling back to
// the abstract when the body has no text at all.
func readingMinutes(body, abstract string) int {
	words := len(strings.Fields(sanitize.Text(body)))
	if words == 0 {
		words = len(strings.Fields(abstract))
	}
	minutes := words / wordsPerMinute
	if minutes < 1 {
		return defaultMinutes
	}
	return minutes
}

func guessTag(entry *gofeed.Item) string {
	if len(entry.Categories) > 0 {
		cat := strings.ToLower(strings.TrimSpace(entry.Categories[0]))
		if cat != "" {
			// A category like "Aktualitäten" is 13 runes but 15 bytes; the old
			// cat[:20] byte slice could cut a rune in half and produce invalid
			// UTF-8, which Postgres rejects - failing the entire item insert.
			return sanitize.TruncateRunes(cat, tagRunes)
		}
	}
	if len([]rune(entry.Content)) > 2000 {
		return "article"
	}
	return "post"
}
