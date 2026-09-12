// Package httpx provides the hardened HTTP client used for every outbound
// request the reader makes on behalf of a user.
//
// The reader fetches two kinds of user-influenced URLs: the feed URL a user
// subscribes to, and every entry.Link inside that feed. Both were previously
// fetched with an unvalidated URL and gofeed's default client, which has no
// timeout at all. That combination is a server-side request forgery primitive:
// a feed whose items link to http://169.254.169.254/latest/meta-data/ or
// http://localhost:5432/ makes the server read internal endpoints and render
// the result straight back to the attacker. A single hanging feed server also
// blocked the serial fetch loop forever, permanently stopping all refreshes.
//
// The defence lives at dial time rather than at parse time: a net.Dialer
// Control hook inspects the address actually being connected to, after DNS
// resolution, for every connection including each redirect hop. Validating the
// hostname alone is not sufficient, because a name can resolve to a private
// address, and can resolve differently between the check and the dial (DNS
// rebinding).
package httpx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// MaxBodyBytes is the cap applied to any fetched feed or article body. Feeds
// are attacker-controlled, so an unbounded io.ReadAll on a response is an
// out-of-memory kill switch for the whole process.
const MaxBodyBytes int64 = 8 << 20

// maxRedirects caps a redirect chain. Each hop is re-validated, but a chain
// still has to terminate: redirect loops otherwise consume a worker slot for
// the full client timeout.
const maxRedirects = 5

// ErrBlockedAddress is returned when a host resolves into a blocked range.
var ErrBlockedAddress = errors.New("httpx: address blocked")

// cgnat is RFC 6598 carrier-grade NAT space. It is not covered by
// netip.Addr.IsPrivate, but is routinely used for internal infrastructure
// (notably by container and cloud networking), so it is blocked too.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// SafeClient returns an *http.Client that refuses to connect to loopback,
// link-local, private, CGNAT, multicast or unspecified addresses, re-checked on
// every redirect hop, with the given per-request timeout.
//
// The timeout on the client covers the whole request including body reads; the
// transport timeouts below bound the individual phases so a server that accepts
// a connection and then stalls cannot hold a worker for the full budget.
func SafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control runs after name resolution with the concrete address that is
		// about to be dialled, which is the only place the check cannot be
		// raced by DNS.
		Control: controlBlockPrivate,
	}

	transport := &http.Transport{
		// Deliberately no Proxy. With a proxy configured the transport dials
		// the proxy, so the Control hook would only ever see the proxy's
		// address and the target would go unchecked — an SSRF filter that a
		// stray HTTP_PROXY in the environment silently disables.
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}

	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: checkRedirect,
	}
}

// checkRedirect bounds the chain and re-checks the scheme on every hop. The
// address check is already handled by the dialer's Control hook, but the scheme
// is not: a redirect to file:// or gopher:// is a different class of attack and
// has to be rejected before the transport is asked to handle it.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("httpx: stopped after %d redirects", maxRedirects)
	}
	if err := checkScheme(req.URL); err != nil {
		return fmt.Errorf("httpx: redirect to %q rejected: %w", req.URL.Redacted(), err)
	}
	return nil
}

// controlBlockPrivate is the net.Dialer Control hook. It is called for every
// connection attempt with the resolved address, so it also covers addresses
// reached through redirects and through multi-A-record hosts.
func controlBlockPrivate(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		// Nothing in this service should be dialling unix sockets or raw
		// datagrams; refuse rather than guess at how to parse the address.
		return fmt.Errorf("%w: network %q not permitted", ErrBlockedAddress, network)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable address %q", ErrBlockedAddress, address)
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Control is documented to receive a resolved address. If it is not a
		// literal IP something is wrong, and failing closed is the safe choice.
		return fmt.Errorf("%w: unresolved address %q", ErrBlockedAddress, host)
	}

	if isBlockedAddr(addr) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
	}
	return nil
}

// isBlockedAddr reports whether addr is in a range the reader must never reach.
// It is a pure function so the whole block list is testable without opening a
// socket.
func isBlockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}

	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) is the classic bypass:
	// the IPv6 predicates below do not consider it loopback, but the kernel
	// connects to 127.0.0.1 all the same. Unmap first so every check runs
	// against the address that will actually be used.
	addr = addr.Unmap()

	// IsPrivate covers RFC1918 for IPv4 and unique-local fc00::/7 for IPv6;
	// IsMulticast covers link-local and interface-local multicast as well.
	switch {
	case addr.IsLoopback(),
		addr.IsUnspecified(),
		addr.IsPrivate(),
		addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast():
		return true
	}

	// RFC 6598 carrier-grade NAT is IPv4-only; Contains is false for any
	// address of a different family, but the guard keeps the intent obvious.
	if addr.Is4() && cgnat.Contains(addr) {
		return true
	}

	return false
}

// ValidateFeedURL normalises and checks a user-supplied feed URL. It rejects
// anything that is not http/https and anything resolving to a blocked range.
// Returns the cleaned absolute URL.
//
// DNS is deliberately not resolved here. Resolving at validation time and
// dialling later leaves a rebinding window, so the address check is the
// dialer's job; this function only enforces the properties of the URL itself.
func ValidateFeedURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("httpx: empty url")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("httpx: invalid url: %w", err)
	}

	if !u.IsAbs() {
		return "", fmt.Errorf("httpx: url %q is not absolute, an http(s) scheme is required", trimmed)
	}

	if err := checkScheme(u); err != nil {
		return "", err
	}

	if u.Host == "" {
		return "", fmt.Errorf("httpx: url %q has no host", trimmed)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("httpx: url %q has no hostname", trimmed)
	}

	// user:pass@host is a well-worn filter bypass: naive host checks read the
	// userinfo as the host, and some clients treat it as authority. There is no
	// legitimate reason for a feed URL to carry credentials in the URL.
	if u.User != nil {
		return "", fmt.Errorf("httpx: url %q embeds userinfo", u.Redacted())
	}

	// Fragments are never sent to the server and only cause the same feed to be
	// stored under several different ids.
	u.Fragment = ""
	u.RawFragment = ""

	// Normalise the scheme and host case so the same feed is not subscribed
	// twice under HTTP://Example.COM and http://example.com.
	u.Scheme = strings.ToLower(u.Scheme)

	return u.String(), nil
}

// checkScheme enforces http/https, case-insensitively. It is shared by
// ValidateFeedURL and the redirect check so both agree on what is fetchable.
func checkScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	case "":
		return errors.New("httpx: missing scheme, http or https required")
	default:
		return fmt.Errorf("httpx: scheme %q is not permitted, http or https required", u.Scheme)
	}
}

// LimitedBody wraps r.Body in a MaxBodyBytes limiter so a multi-gigabyte
// response cannot OOM the process.
func LimitedBody(resp *http.Response) io.Reader {
	if resp == nil || resp.Body == nil {
		return strings.NewReader("")
	}
	return io.LimitReader(resp.Body, MaxBodyBytes)
}
