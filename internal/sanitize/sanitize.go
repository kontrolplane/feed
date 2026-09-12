// Package sanitize turns third-party feed and article HTML into something that
// is safe to hand to templ.Raw.
//
// The reader renders article bodies verbatim. Those bodies come either straight
// off the wire (an Atom <content> element, filtered by nothing at all) or out of
// go-readability. Readability drops <script> and <iframe>, but it has no
// attribute allowlist: onerror, onload, onmouseover and javascript: hrefs all
// survive on every element it decides to keep, and upstream is explicit that its
// output still has to be sanitised. Everything in this package exists so that a
// single hostile feed cannot execute JavaScript in the reader's origin.
package sanitize

import (
	"html"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// safeSrcset matches an srcset whose every candidate is an absolute http(s) URL
// with an optional width/density descriptor.
//
// bluemonday only runs its URL policy over a fixed set of attributes (href, src,
// cite, ...); srcset is not one of them, so an allowed srcset would otherwise be
// copied through unvalidated and could smuggle a javascript: or data: candidate
// past the scheme allowlist. Pinning the scheme here is what makes srcset safe
// to keep. URLs containing a literal comma are rejected rather than guessed at;
// in that case the whole attribute is dropped and the browser falls back to src.
var safeSrcset = regexp.MustCompile(
	`^\s*(?i:https?)://[^\s,]+(\s+[0-9]+(\.[0-9]+)?[wx])?(\s*,\s*(?i:https?)://[^\s,]+(\s+[0-9]+(\.[0-9]+)?[wx])?)*\s*$`,
)

var (
	articlePolicy = newArticlePolicy()
	textPolicy    = newTextPolicy()
)

// newArticlePolicy builds the allowlist used for article bodies. A bluemonday
// Policy is immutable once built and safe for concurrent use, so this runs once
// at package init and every call to ArticleHTML shares it.
func newArticlePolicy() *bluemonday.Policy {
	// UGCPolicy is the right starting point: it already refuses script, style,
	// svg, object, embed, iframe, form controls and every on* handler, and it
	// permits the phrase/structure elements an article is made of.
	p := bluemonday.UGCPolicy()

	// Spelled out rather than inherited so that a future bluemonday release
	// narrowing UGCPolicy cannot silently gut article formatting. AllowElements
	// is additive and idempotent.
	p.AllowElements(
		"h1", "h2", "h3", "h4", "h5", "h6",
		"p", "div", "span", "br", "hr",
		"ul", "ol", "li", "dl", "dt", "dd",
		"blockquote", "pre", "code", "kbd", "samp",
		"figure", "figcaption",
		"strong", "em", "b", "i", "u", "s", "sub", "sup", "mark", "small",
		"abbr", "cite", "q", "time", "del", "ins",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption",
	)

	// Restrict every URL-bearing attribute to schemes that cannot execute:
	// this is what kills javascript: and data:text/html. Relative URLs are
	// refused as well - a feed's relative link would resolve against *our*
	// origin, which is both wrong (the article lives elsewhere) and a way to
	// dress up a link to our own mutating endpoints as article content.
	p.AllowURLSchemes("http", "https", "mailto")
	p.AllowRelativeURLs(false)
	p.RequireParseableURLs(true)

	// Outbound links are untrusted: no ranking signal, no window.opener handle
	// back into the reader, no referrer leak of what the user is reading.
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	// Adds target="_blank", and bluemonday pairs it with rel="noopener".
	p.AddTargetBlankToFullyQualifiedLinks(true)

	// Images: UGCPolicy's AllowImages covers src/width/height/align, and src
	// goes through the scheme allowlist above; title is allowed globally by
	// AllowStandardAttributes. alt is re-allowed without UGCPolicy's fairly
	// narrow Paragraph pattern - alt text is attribute-escaped on the way out,
	// so a colon or an ampersand in it is a formatting detail, not a risk.
	// srcset is gated on the regex above. Data-URI images stay off: a large
	// unbounded blob inlined in the page buys an article nothing.
	p.AllowAttrs("alt").OnElements("img")
	p.AllowAttrs("srcset").Matching(safeSrcset).OnElements("img")

	// Syntax-highlight hints such as class="language-go". Scoped to pre/code so
	// that feed content cannot reach for the application's own class names.
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("pre", "code")

	// Deliberately no AllowStyles/AllowStyling for the style attribute: inline
	// CSS lets an article break out of its column or cover the page
	// (position:fixed;inset:0), and url() in a style is an exfiltration
	// channel. Article HTML gets its look from our stylesheet, not its own.

	return p
}

// newTextPolicy strips markup entirely, for values that are rendered as text.
func newTextPolicy() *bluemonday.Policy {
	p := bluemonday.StrictPolicy()
	// Without this, <p>one</p><p>two</p> collapses to "onetwo".
	p.AddSpaceWhenStrippingTag(true)
	return p
}

// ArticleHTML strips scripts, event handlers, javascript: URLs and anything
// else not on the allowlist from third-party article HTML. The result is safe
// to render with templ.Raw; nothing else in the codebase is.
func ArticleHTML(s string) string {
	if s == "" {
		return ""
	}
	return articlePolicy.Sanitize(s)
}

// Text strips all markup and returns plain text with entities decoded and
// runs of whitespace collapsed to single spaces. Used for abstracts, which the
// feed hands us as HTML fragments but which we store and render as text.
//
// The result is plain text and must be rendered as such - it is not escaped and
// must never be passed to templ.Raw.
func Text(s string) string {
	if s == "" {
		return ""
	}

	// Sanitize drops the tags but re-escapes the text it keeps, so a single
	// unescape returns what the HTML parser actually saw.
	stripped := html.UnescapeString(textPolicy.Sanitize(s))

	// strings.Fields splits on any Unicode space, so &nbsp; and stray newlines
	// from pretty-printed feed XML collapse too.
	return strings.Join(strings.Fields(stripped), " ")
}

// TruncateRunes cuts s to at most n runes without splitting a rune, appending
// "…" when it truncated. n counts the runes kept from s; the ellipsis is
// additional. n <= 0 yields the empty string.
//
// This replaces s[:n] byte slicing. A byte slice cuts through the middle of any
// multi-byte rune and produces invalid UTF-8, which Postgres rejects outright
// with `invalid byte sequence for encoding "UTF8"` - failing the whole item
// insert, so a feed with any non-ASCII content simply never imported.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}

	// Fast path: an ASCII-only string can only exceed n runes if it exceeds n
	// bytes, so a byte-length check short-circuits the rune count for the vast
	// majority of inputs without ever being wrong for multi-byte ones.
	if len(s) <= n {
		return s
	}

	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
