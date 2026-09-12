package handler

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// ---------------------------------------------------------------------------
// Content-Security-Policy
// ---------------------------------------------------------------------------
//
// The CSP is the second layer behind the HTML sanitiser: the sanitiser decides
// what third-party article markup is allowed into the page, and this decides
// what the browser will actually execute if something slips past it. Neither is
// sufficient alone — a single missed attribute in an allowlist is a stored XSS
// without this header.
//
// Everything the app loads is same-origin: htmx is vendored at
// /static/js/htmx.min.js, the app code at /static/js/app.js, and the fonts are
// self-hosted under /static/fonts/. There are no external requests left, so
// every directive below is 'self' except the two that genuinely need more.
//
// !!! READ THIS BEFORE EDITING templates/layout.templ !!!
//
// script-src pins five SHA-256 hashes of inline script that the page cannot
// work without. They are hashes of the *rendered* bytes, taken from the
// generated templates/*_templ.go, not from the .templ source — templ rewrites
// whitespace, so the two do not match. If any of the five snippets below is
// edited (even by one space) the browser silently refuses to run it: the theme
// bootstrap is the visible casualty — the app paints the light plate and then
// flips to dark on every single load. Nothing logs when this happens.
//
// To regenerate a hash: take the exact bytes the browser sees — for a <script>
// block everything between the tags including the surrounding newline and tabs,
// for a handler the attribute value only — into a file with no trailing newline
// added, then:
//
//	openssl dgst -sha256 -binary snippet.txt | openssl base64
//
// The bytes come from the generated templates/layout_templ.go string literal or
// from `curl -s localhost:8080/`, never from the .templ file. Failing that,
// every browser prints the hash it computed in the console message it logs when
// it blocks the script; that value can be pasted in directly.
//
// The five pinned snippets and where they live:
//
//  1. templates/layout.templ, <head>            — theme bootstrap (must be inline and
//     synchronous or the wrong plate paints first)
//  2. templates/layout.templ, inside .app        — density/pane bootstrap (same reason)
//  3. templates/layout.templ  onclick="toggleTheme()"
//  4. templates/itemlist.templ onclick="showPane('reader')"
//  5. templates/reader.templ   onclick="showPane('list')"
//
// Hashes were chosen over 'unsafe-inline' deliberately. 'unsafe-inline' would
// allow *any* injected inline script or event handler — exactly the payload a
// malicious feed would deliver — which throws away most of the reason to send a
// CSP at all. With hashes, an injected <script> or onerror= is blocked because
// it does not match, while the five known snippets keep working.
//
// 'unsafe-hashes' is required for 3-5: without it a hash only covers whole
// <script> elements, never event handler attributes. It does not weaken the
// policy beyond the listed hashes.
//
// 'unsafe-eval' is required because htmx compiles hx-on::after-request
// attributes with `new Function`. Three of them exist today (addfeed.templ,
// addfolder.templ, settings.templ); without eval the subscribe/create-folder
// modals stop closing and OPML import reports nothing. Moving those three
// handlers into static/js/app.js as delegated listeners is the fix that lets
// 'unsafe-eval' be dropped — worth doing, but it is a frontend change.
const (
	// cspThemeBootstrap is templates/layout.templ's <head> script.
	cspThemeBootstrap = "'sha256-sNTnJSVQDbU2uwLAkp/00c+upiJJH/n2aQ47zR09kwM='"
	// cspPaneBootstrap is templates/layout.templ's density/pane script inside .app.
	cspPaneBootstrap = "'sha256-X/p9cCh2VFIn196cxcldLoFfoejFzKvUFohNUdGCwbo='"
	// cspOnToggleTheme covers onclick="toggleTheme()" in layout.templ.
	cspOnToggleTheme = "'sha256-5J8QIN0uHa3X4+TaUpRKBKn3pljcpBNl9MhDpDECP+U='"
	// cspOnShowPaneReader covers onclick="showPane('reader')" in itemlist.templ.
	cspOnShowPaneReader = "'sha256-ND6JkynhlSuj/wVrWLmVEud5ER5GyDPFCaBGQNpQeo0='"
	// cspOnShowPaneList covers onclick="showPane('list')" in reader.templ.
	cspOnShowPaneList = "'sha256-9BxNfUw3EBxBF51SgrvSrGbGZmhmETE+YNTmKRHGa/k='"
)

// contentSecurityPolicy is the single policy sent on every response.
//
// The two directives that are not 'self':
//
//   - img-src allows data: and https:. Feed articles legitimately embed remote
//     images and refusing them would gut the reader. This is the one place
//     remote content is expected. Note that plain http: images are still
//     blocked — an article served over http will show broken images rather than
//     leak a request on an https deployment.
//   - script-src, for the five pinned hashes explained above.
//
// style-src is 'self' rather than 'unsafe-inline': the sanitiser strips style
// attributes from article HTML, so nothing legitimate needs inline CSS. htmx
// does inject one inline <style> for its indicator classes at startup and that
// injection is blocked — harmless, because base.css already defines the
// .htmx-indicator rules itself (base.css:371).
//
// frame-ancestors 'none' is the clickjacking defence; base-uri 'none' stops an
// injected <base> from re-pointing every relative URL on the page at an
// attacker; form-action 'self' stops an injected form from posting elsewhere.
var contentSecurityPolicy = strings.Join([]string{
	"default-src 'self'",
	"script-src 'self' 'unsafe-eval' 'unsafe-hashes' " + strings.Join([]string{
		cspThemeBootstrap,
		cspPaneBootstrap,
		cspOnToggleTheme,
		cspOnShowPaneReader,
		cspOnShowPaneList,
	}, " "),
	"style-src 'self'",
	"img-src 'self' data: https:",
	"font-src 'self'",
	"connect-src 'self'",
	"object-src 'none'",
	"base-uri 'none'",
	"form-action 'self'",
	"frame-ancestors 'none'",
}, "; ")

// permissionsPolicy denies the hardware and tracking features this app has no
// use for, so that injected content cannot prompt for them either.
const permissionsPolicy = "geolocation=(), camera=(), microphone=(), interest-cohort=()"

// SecurityHeaders sets CSP, X-Content-Type-Options, Referrer-Policy and friends
// on every response, including error responses.
//
// Deliberately absent: Strict-Transport-Security. This app is routinely served
// over plain HTTP on a LAN, and HSTS is sticky per-hostname for its max-age —
// an accidental header on a shared name (a home server reachable as
// "nas.local", say) pins every other service on that name to https too, and the
// only cure is chrome://net-internals on every device that saw it. A
// self-hoster terminating TLS at a reverse proxy should set HSTS there, where
// it is a deliberate choice rather than a default.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// Stops the browser second-guessing Content-Type, which is how a
		// user-uploaded OPML file becomes a rendered HTML document.
		h.Set("X-Content-Type-Options", "nosniff")
		// The URLs here contain item and feed ids; there is nothing a third
		// party needs to learn from them.
		h.Set("Referrer-Policy", "no-referrer")
		// Redundant with frame-ancestors for modern browsers, kept for the ones
		// that only implement this.
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", permissionsPolicy)
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// CSRFGuard rejects cross-site state-changing requests.
//
// The app has no authentication and no cookies, which is exactly why this is
// needed rather than why it is not: with no origin check at all, any page a
// user happens to visit can POST to http://localhost:8080/feeds/subscribe and
// silently add an attacker-controlled feed to their reader. That feed's content
// is then rendered in the reader, so this chains straight into the stored-XSS
// surface the sanitiser and the CSP above exist to contain. A plain HTML form
// POST needs no CORS permission, so the browser will send it.
//
// The check is origin-based rather than token-based because there is no session
// to bind a token to, and because it costs nothing at the template layer.
func CSRFGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStateChangingMethod(r.Method) {
			// GET/HEAD/OPTIONS pass untouched. This is only sound for handlers
			// that do not mutate on GET — see the note on GET /items/{id} below.
			next.ServeHTTP(w, r)
			return
		}

		if reason, ok := crossSiteReason(r); !ok {
			slog.Warn("rejected cross-site state-changing request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("reason", reason),
				slog.String("sec_fetch_site", r.Header.Get("Sec-Fetch-Site")),
				slog.String("origin", r.Header.Get("Origin")),
			)
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isStateChangingMethod reports whether the method is one that may mutate.
//
// GET /items/{id} currently marks the item read, which makes it state-changing
// in fact if not in method, and no origin check can see that. Making that
// mutation a POST (or moving it behind the existing POST /items/{id}/toggle-read)
// belongs with the route handlers.
func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// crossSiteReason decides whether a state-changing request may proceed,
// returning a short reason when it may not.
//
// Three signals are consulted in order of reliability:
//
//  1. Sec-Fetch-Site, set by the browser itself and unforgeable by page script.
//     "same-origin" and "same-site" are ours; "none" is a direct navigation,
//     bookmark or bookmarklet, which is user-initiated and allowed.
//  2. Origin, compared host-for-host against the Host the request arrived on.
//     Sent by every browser on a cross-origin POST, including old ones.
//  3. Referer, same comparison, for the handful of clients that send neither.
//
// If none of the three is present the request is ALLOWED. That is a deliberate
// trade-off: `curl -X POST` and every scripting client sends none of them, and
// this app is legitimately scripted (the feeds-file import, shell one-liners,
// home automation). The attack being blocked is browser-driven, and a browser
// always sends at least one of the three — so allowing the header-less case
// costs no real protection while keeping the app scriptable.
//
// X-Forwarded-Host is deliberately NOT consulted. Behind a reverse proxy that
// rewrites Host, set the proxy to preserve the original Host header instead;
// trusting a forwarded header here would let a client nominate the host it is
// checked against, which is the same as no check at all.
func crossSiteReason(r *http.Request) (string, bool) {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		switch site {
		case "same-origin", "same-site", "none":
			return "", true
		default:
			// "cross-site", and anything unrecognised, is refused.
			return "sec-fetch-site", false
		}
	}

	if origin := r.Header.Get("Origin"); origin != "" {
		// A sandboxed or otherwise opaque origin sends the literal "null",
		// which parses to an empty host and is correctly refused here.
		if host := hostFromURL(origin); host == "" || !strings.EqualFold(host, r.Host) {
			return "origin", false
		}
		return "", true
	}

	if referer := r.Header.Get("Referer"); referer != "" {
		if host := hostFromURL(referer); host == "" || !strings.EqualFold(host, r.Host) {
			return "referer", false
		}
		return "", true
	}

	// No browser signal at all: a scripted client. See the trade-off above.
	return "", true
}

// hostFromURL returns the host:port of an absolute URL, or "" if it is not one.
// Comparing host *and* port matters: http://example.com:9999 is a different
// origin from http://example.com and must not be treated as same-site.
func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// An unparsable Origin/Referer is not evidence of same-origin, so the
		// caller treats the empty host as a mismatch and refuses the request.
		return ""
	}
	return u.Host
}
