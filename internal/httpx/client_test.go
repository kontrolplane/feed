package httpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestValidateFeedURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string // expected cleaned url; empty means an error is expected
	}{
		{name: "http", raw: "http://example.com/feed.xml", want: "http://example.com/feed.xml"},
		{name: "https", raw: "https://example.com/feed.xml", want: "https://example.com/feed.xml"},
		{name: "https with port and query", raw: "https://example.com:8443/feed?format=rss", want: "https://example.com:8443/feed?format=rss"},
		{name: "uppercase scheme is normalised", raw: "HTTPS://example.com/feed", want: "https://example.com/feed"},
		{name: "surrounding whitespace is trimmed", raw: "  https://example.com/feed  ", want: "https://example.com/feed"},
		{name: "fragment is stripped", raw: "https://example.com/feed#latest", want: "https://example.com/feed"},
		{name: "bare host is kept", raw: "https://example.com", want: "https://example.com"},

		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "relative path", raw: "/feed.xml"},
		{name: "host without scheme", raw: "example.com/feed.xml"},
		{name: "scheme relative", raw: "//example.com/feed.xml"},
		{name: "file scheme", raw: "file:///etc/passwd"},
		{name: "gopher scheme", raw: "gopher://example.com:70/1"},
		{name: "javascript scheme", raw: "javascript:alert(1)"},
		{name: "data scheme", raw: "data:text/xml,<rss/>"},
		{name: "ftp scheme", raw: "ftp://example.com/feed.xml"},
		{name: "empty host", raw: "http:///feed.xml"},
		{name: "userinfo with password", raw: "http://user:pass@example.com/feed.xml"},
		{name: "userinfo without password", raw: "http://admin@example.com/feed.xml"},
		{name: "control character in url", raw: "http://exa\x7fmple.com/feed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateFeedURL(tc.raw)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ValidateFeedURL(%q) = %q, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateFeedURL(%q) returned unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateFeedURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestValidateFeedURLDoesNotLeakCredentials guards the error path: the rejected
// url is echoed back to the user, so it must go through URL.Redacted.
func TestValidateFeedURLDoesNotLeakCredentials(t *testing.T) {
	_, err := ValidateFeedURL("http://user:hunter2@example.com/feed.xml")
	if err == nil {
		t.Fatal("expected an error for a url with userinfo")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestIsBlockedAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		blocked bool
	}{
		// loopback
		{name: "ipv4 loopback", addr: "127.0.0.1", blocked: true},
		{name: "ipv4 loopback range", addr: "127.1.2.3", blocked: true},
		{name: "ipv6 loopback", addr: "::1", blocked: true},
		{name: "ipv4 mapped loopback", addr: "::ffff:127.0.0.1", blocked: true},

		// unspecified
		{name: "ipv4 unspecified", addr: "0.0.0.0", blocked: true},
		{name: "ipv6 unspecified", addr: "::", blocked: true},

		// RFC1918
		{name: "10/8", addr: "10.0.0.1", blocked: true},
		{name: "192.168/16", addr: "192.168.1.1", blocked: true},
		{name: "172.16/12", addr: "172.16.0.1", blocked: true},
		{name: "172.31 upper bound", addr: "172.31.255.255", blocked: true},
		{name: "ipv4 mapped rfc1918", addr: "::ffff:10.0.0.1", blocked: true},

		// link-local — the cloud metadata endpoint lives here
		{name: "aws metadata", addr: "169.254.169.254", blocked: true},
		{name: "ipv6 link local", addr: "fe80::1", blocked: true},
		{name: "ipv4 link local multicast", addr: "224.0.0.251", blocked: true},
		{name: "ipv6 link local multicast", addr: "ff02::1", blocked: true},
		{name: "ipv6 interface local multicast", addr: "ff01::1", blocked: true},

		// multicast
		{name: "ipv4 multicast", addr: "239.255.255.250", blocked: true},
		{name: "ipv6 multicast", addr: "ff05::1", blocked: true},

		// CGNAT
		{name: "cgnat low", addr: "100.64.0.1", blocked: true},
		{name: "cgnat high", addr: "100.127.255.255", blocked: true},

		// IPv6 unique local
		{name: "ipv6 unique local", addr: "fd00::1", blocked: true},

		// public
		{name: "example.com", addr: "93.184.216.34", blocked: false},
		{name: "google dns", addr: "8.8.8.8", blocked: false},
		{name: "cloudflare dns", addr: "1.1.1.1", blocked: false},
		{name: "public ipv6", addr: "2606:4700:4700::1111", blocked: false},
		{name: "just below cgnat", addr: "100.63.255.255", blocked: false},
		{name: "just above cgnat", addr: "100.128.0.0", blocked: false},
		{name: "just below rfc1918 172 block", addr: "172.15.255.255", blocked: false},
		{name: "just above rfc1918 172 block", addr: "172.32.0.0", blocked: false},
		{name: "not link local", addr: "169.253.0.1", blocked: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("bad test fixture %q: %v", tc.addr, err)
			}
			if got := isBlockedAddr(addr); got != tc.blocked {
				t.Fatalf("isBlockedAddr(%s) = %v, want %v", tc.addr, got, tc.blocked)
			}
		})
	}
}

// The zero netip.Addr is not a real address; failing closed matters because
// anything that reaches the dialer without a parseable address is a bug.
func TestIsBlockedAddrRejectsInvalid(t *testing.T) {
	if !isBlockedAddr(netip.Addr{}) {
		t.Fatal("isBlockedAddr(invalid) = false, want true")
	}
}

// TestSafeClientRefusesLoopback is the test that proves the Control hook is
// actually wired into the transport. httptest always listens on loopback, so a
// client that can reach it is a client whose block list is not in the dial
// path — which a unit test of isBlockedAddr alone would not catch.
func TestSafeClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("secret internal response"))
	}))
	defer srv.Close()

	client := SafeClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("SafeClient reached loopback server %s, want it blocked", srv.URL)
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("error %v does not wrap ErrBlockedAddress", err)
	}
}

// A public address must still be dialable; this checks the block list does not
// reject everything. The request is aimed at discard/9 where nothing answers,
// so it is expected to fail — but it must fail with a connection or timeout
// error, never with ErrBlockedAddress. Skipped under -short because it does
// attempt a real outbound connection.
func TestSafeClientAllowsPublicAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("makes an outbound connection attempt")
	}
	client := SafeClient(1 * time.Second)
	_, err := client.Get("http://93.184.216.34:9/")
	if err != nil && errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("public address was blocked: %v", err)
	}
}

func TestSafeClientTimeoutAndRedirectPolicy(t *testing.T) {
	client := SafeClient(3 * time.Second)
	if client.Timeout != 3*time.Second {
		t.Fatalf("client.Timeout = %v, want 3s", client.Timeout)
	}
	if client.CheckRedirect == nil {
		t.Fatal("client.CheckRedirect is nil, redirects would be followed unchecked")
	}

	mustReq := func(rawURL string) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("building request for %q: %v", rawURL, err)
		}
		return req
	}

	// A redirect to a non-http scheme must be refused even though the address
	// check would never run for it.
	if err := client.CheckRedirect(mustReq("http://example.com/"), nil); err != nil {
		t.Fatalf("http redirect hop rejected: %v", err)
	}
	if err := client.CheckRedirect(mustReq("file:///etc/passwd"), nil); err == nil {
		t.Fatal("redirect to file:// was allowed")
	}

	// The chain must terminate.
	via := make([]*http.Request, maxRedirects)
	if err := client.CheckRedirect(mustReq("http://example.com/"), via); err == nil {
		t.Fatalf("redirect chain of %d hops was allowed", maxRedirects)
	}
}

func TestLimitedBody(t *testing.T) {
	t.Run("truncates oversized bodies", func(t *testing.T) {
		// A body one byte past the cap: the reader must stop exactly at the cap
		// rather than streaming the whole thing into memory.
		resp := &http.Response{Body: io.NopCloser(&repeatReader{n: MaxBodyBytes + 1})}
		n, err := io.Copy(io.Discard, LimitedBody(resp))
		if err != nil {
			t.Fatalf("reading limited body: %v", err)
		}
		if n != MaxBodyBytes {
			t.Fatalf("read %d bytes, want %d", n, MaxBodyBytes)
		}
	})

	t.Run("passes through small bodies", func(t *testing.T) {
		resp := &http.Response{Body: io.NopCloser(strings.NewReader("<rss/>"))}
		got, err := io.ReadAll(LimitedBody(resp))
		if err != nil {
			t.Fatalf("reading limited body: %v", err)
		}
		if string(got) != "<rss/>" {
			t.Fatalf("got %q, want %q", got, "<rss/>")
		}
	})

	t.Run("nil response and nil body are safe", func(t *testing.T) {
		for _, resp := range []*http.Response{nil, {}} {
			got, err := io.ReadAll(LimitedBody(resp))
			if err != nil {
				t.Fatalf("reading limited body: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("got %q, want empty", got)
			}
		}
	})
}

// repeatReader yields n zero bytes without allocating them, so the oversized
// body test does not itself need to hold 8MB.
type repeatReader struct{ n int64 }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	clear(p)
	r.n -= int64(len(p))
	return len(p), nil
}
