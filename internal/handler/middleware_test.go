package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler records whether the middleware let the request through.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestCSRFGuard(t *testing.T) {
	const host = "feed.local:8080"

	tests := []struct {
		name    string
		method  string
		host    string
		site    string // Sec-Fetch-Site
		origin  string
		referer string
		allow   bool
	}{
		// Safe methods are never touched, whatever the origin says.
		{name: "get from a cross-site page", method: http.MethodGet, site: "cross-site", origin: "https://evil.example", allow: true},
		{name: "head from a cross-site page", method: http.MethodHead, site: "cross-site", allow: true},
		{name: "options preflight from a cross-site page", method: http.MethodOptions, site: "cross-site", allow: true},

		// Sec-Fetch-Site is preferred and decides on its own.
		{name: "post same-origin", method: http.MethodPost, site: "same-origin", allow: true},
		{name: "post same-site", method: http.MethodPost, site: "same-site", allow: true},
		{name: "post direct navigation", method: http.MethodPost, site: "none", allow: true},
		{name: "post cross-site", method: http.MethodPost, site: "cross-site", allow: false},
		{name: "put cross-site", method: http.MethodPut, site: "cross-site", allow: false},
		{name: "patch cross-site", method: http.MethodPatch, site: "cross-site", allow: false},
		{name: "delete cross-site", method: http.MethodDelete, site: "cross-site", allow: false},
		{name: "unrecognised sec-fetch-site value", method: http.MethodPost, site: "wat", allow: false},
		// A matching Origin must not rescue an explicit cross-site signal:
		// Sec-Fetch-Site comes from the browser, Origin from the request.
		{name: "cross-site beats a matching origin", method: http.MethodPost, site: "cross-site", origin: "http://feed.local:8080", allow: false},

		// Origin fallback, for clients that send no Sec-Fetch-Site.
		{name: "origin matches host", method: http.MethodPost, origin: "http://feed.local:8080", allow: true},
		{name: "origin matches host over https", method: http.MethodPost, origin: "https://feed.local:8080", allow: true},
		{name: "origin differs in host", method: http.MethodPost, origin: "http://evil.example", allow: false},
		{name: "origin differs in port only", method: http.MethodPost, origin: "http://feed.local:9999", allow: false},
		{name: "origin is null", method: http.MethodPost, origin: "null", allow: false},
		{name: "origin unparsable", method: http.MethodPost, origin: "http://%zz", allow: false},
		{name: "origin case-insensitive", method: http.MethodPost, host: "Feed.Local:8080", origin: "http://feed.local:8080", allow: true},
		// Origin wins over Referer when both are present.
		{name: "bad origin with good referer", method: http.MethodPost, origin: "http://evil.example", referer: "http://feed.local:8080/", allow: false},
		{name: "good origin with bad referer", method: http.MethodPost, origin: "http://feed.local:8080", referer: "http://evil.example/", allow: true},

		// Referer fallback, last resort.
		{name: "referer matches host", method: http.MethodPost, referer: "http://feed.local:8080/settings", allow: true},
		{name: "referer differs in host", method: http.MethodPost, referer: "http://evil.example/page", allow: false},
		{name: "referer without a host", method: http.MethodPost, referer: "/settings", allow: false},

		// The documented trade-off: a scripted client sends none of the three
		// and must keep working.
		{name: "no browser headers at all", method: http.MethodPost, allow: true},
		{name: "delete with no browser headers", method: http.MethodDelete, allow: true},
		{name: "empty headers are treated as absent", method: http.MethodPost, site: "", origin: "", referer: "", allow: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			req := httptest.NewRequest(tt.method, "/feeds/subscribe", nil)
			req.Host = host
			if tt.host != "" {
				req.Host = tt.host
			}
			if tt.site != "" {
				req.Header.Set("Sec-Fetch-Site", tt.site)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}

			rec := httptest.NewRecorder()
			CSRFGuard(okHandler(&reached)).ServeHTTP(rec, req)

			if reached != tt.allow {
				t.Fatalf("handler reached = %v, want %v (status %d)", reached, tt.allow, rec.Code)
			}
			if tt.allow {
				if rec.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			if body := strings.TrimSpace(rec.Body.String()); body == "" {
				t.Fatal("rejection body is empty, want a plain-text explanation")
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("rejection Content-Type = %q, want text/plain", ct)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	reached := false
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	SecurityHeaders(okHandler(&reached)).ServeHTTP(rec, req)

	if !reached {
		t.Fatal("SecurityHeaders did not call the next handler")
	}

	headers := []struct {
		name string
		want string
	}{
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", "no-referrer"},
		{"X-Frame-Options", "DENY"},
	}
	for _, h := range headers {
		if got := rec.Header().Get(h.name); got != h.want {
			t.Errorf("%s = %q, want %q", h.name, got, h.want)
		}
	}

	// HSTS must never be set: this app is routinely served over plain HTTP on a
	// LAN and a stray HSTS header on a shared hostname is very hard to undo.
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want it absent", got)
	}

	pp := rec.Header().Get("Permissions-Policy")
	for _, feature := range []string{"geolocation=()", "camera=()", "microphone=()", "interest-cohort=()"} {
		if !strings.Contains(pp, feature) {
			t.Errorf("Permissions-Policy %q is missing %q", pp, feature)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy is not set")
	}
	for _, directive := range []string{
		"default-src 'self'",
		"style-src 'self'",
		"img-src 'self' data: https:",
		"font-src 'self'",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q is missing %q", csp, directive)
		}
	}

	// script-src pins the inline bootstraps by hash. 'unsafe-inline' would
	// allow any injected script and must never appear; the hashes are what
	// makes that possible, so their absence is a regression too.
	if strings.Contains(csp, "'unsafe-inline'") {
		t.Errorf("CSP contains 'unsafe-inline', which defeats the script policy: %q", csp)
	}
	for _, hash := range []string{
		cspThemeBootstrap,
		cspPaneBootstrap,
		cspOnToggleTheme,
		cspOnShowPaneReader,
		cspOnShowPaneList,
	} {
		if !strings.Contains(csp, hash) {
			t.Errorf("CSP %q is missing the pinned hash %s", csp, hash)
		}
	}
	// Inline event handlers only run when 'unsafe-hashes' accompanies the hash.
	if !strings.Contains(csp, "'unsafe-hashes'") {
		t.Errorf("CSP is missing 'unsafe-hashes'; the onclick handlers would stop working: %q", csp)
	}
}

// TestSecurityHeadersOnErrorResponse pins the ordering requirement: the headers
// must be on the response even when an inner middleware refuses the request,
// which is only true if SecurityHeaders wraps CSRFGuard rather than the reverse.
func TestSecurityHeadersOnErrorResponse(t *testing.T) {
	reached := false
	req := httptest.NewRequest(http.MethodPost, "/feeds/subscribe", nil)
	req.Host = "feed.local:8080"
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	rec := httptest.NewRecorder()
	SecurityHeaders(CSRFGuard(okHandler(&reached))).ServeHTTP(rec, req)

	if reached {
		t.Fatal("cross-site POST reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("Content-Security-Policy missing from the 403 response")
	}
}
