package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/ssrfguard"
)

// PooledClient builds the keep-alive pool the three internal backend paths
// (chat, embed, rerank) use for their wire calls. It carries NO client-side
// timeout on purpose: every call site sets its own ctx deadline (llm's
// ChatTimeout family, rerank.Timeout, embed's caller budget), and a second
// deadline on the client would silently cap the shorter one. MaxIdleConns 20 /
// MaxIdleConnsPerHost 10 / IdleConnTimeout 90s are the values all three copies
// carried verbatim before they were merged here.
//
// This is a CONSTRUCTOR, not a singleton: each caller keeps its own pool, so
// idle connections stay separated per backend family exactly as before. One
// shared instance would pool connections across chat/embed/rerank against the
// same hosts — a change in connection behaviour, not a de-duplication. If that
// is ever wanted, it can be introduced here, in one place.
//
// Redirects follow sameOriginRedirect: a Location on another origin is not
// followed, the caller gets the 3xx itself.
func PooledClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: sameOriginRedirect,
	}
}

// GuardedPooledClient is PooledClient dialing through internal/ssrfguard: every
// resolved address — first hop, every redirect hop, every DNS answer — is
// re-checked at dial time, so loopback, private, link-local and unspecified
// targets are refused before a byte flows (GHSA 2026-10-31 §3). It serves the
// tenant-owned pool rows (backends.Backend.EgressGuarded); the pool values are
// PooledClient's. ForceAttemptHTTP2 keeps HTTP/2 negotiation, which a custom
// DialContext would otherwise switch off. The guard judges addresses, not
// origins — a redirect to a foreign PUBLIC host passes it — so the client
// carries the same sameOriginRedirect as PooledClient.
func GuardedPooledClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         redactGuardRefusal(ssrfguard.GuardedDialer().DialContext),
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: sameOriginRedirect,
	}
}

// maxRedirects is net/http's own hop limit (defaultCheckRedirect). Setting
// CheckRedirect replaces that default, so the limit and its error text are
// restated here: same-origin chains end exactly where they ended before.
const maxRedirects = 10

// sameOriginRedirect is the CheckRedirect of both pool clients (design/04 I17,
// S5). net/http re-sends a POST body on 307/308 — every wire builds its body
// from a bytes.Reader, so GetBody is set — and copies every header except
// Authorization/Cookie (x-api-key, a row's extra_headers) to any redirect
// target. Following a Location to another host would hand the prompt and the
// row's credential to a host that never passed the Trust × Sensitivity check,
// while the egress audit names the configured row.
//
// A hop is followed only on the origin of the FIRST request — via[0], the
// configured base_url that was vetted — or as an http→https upgrade of it on
// the same host (sameOrigin). Every other hop returns the 3xx itself
// (http.ErrUseLastResponse): the wires turn a non-200 into a StatusError,
// which backends.Classify maps to ClassBadRequest. The refusal is logged once,
// with both origins and the status — never a path or query, which can carry
// tokens.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	from, to := originOf(via[0].URL), originOf(req.URL)
	if sameOrigin(from, to) {
		return nil
	}
	status := 0
	if req.Response != nil {
		status = req.Response.StatusCode
	}
	slog.Warn("redirect refused", "from_origin", from.String(), "to_origin", to.String(), "status", status)
	return http.ErrUseLastResponse
}

// origin is the (scheme, host, port) triple of a URL: scheme and host
// lowercased in the ASCII range only, the scheme's default port filled in.
// Userinfo, path and query are not part of it.
type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	host := u.Hostname()
	zone := ""
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host, zone = host[:i], host[i:] // an IPv6 zone names an interface: case-sensitive
	}
	o := origin{scheme: asciiLower(u.Scheme), host: asciiLower(host) + zone, port: u.Port()}
	if o.port == "" {
		switch o.scheme {
		case "http":
			o.port = "80"
		case "https":
			o.port = "443"
		}
	}
	return o
}

func (o origin) String() string { return o.scheme + "://" + net.JoinHostPort(o.host, o.port) }

// asciiLower folds A-Z only. strings.ToLower folds Unicode as well — U+0130
// 'İ' becomes ASCII 'i' — while net/http dials the IDNA form of a non-ASCII
// host (xn--…), so a Unicode fold would call two different hosts one origin.
// Non-ASCII bytes therefore compare exactly; a refusal is the safe side.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// sameOrigin reports whether a redirect from → to stays on the vetted origin:
// identical triple, or the http→https upgrade on the same host with port
// 80→443 or the same explicit port. Host names compare as strings (ASCII
// case aside) — a subdomain, a trailing dot, a non-ASCII spelling or an IP
// literal for the same name is another origin (the refusal is the safe side
// of every such ambiguity).
func sameOrigin(from, to origin) bool {
	if from.host != to.host {
		return false
	}
	if from.scheme == to.scheme {
		return from.port == to.port
	}
	return from.scheme == "http" && to.scheme == "https" &&
		(from.port == to.port || (from.port == "80" && to.port == "443"))
}

// errGuardRefused replaces ssrfguard's refusal on the backend path. The
// guard's own message names the RESOLVED address; on a tenant row the error
// reaches the tenant-admin (the first 256 characters of a chain error show in
// its own /api/llmlog), so echoing it would turn the guard into a resolver
// for the server's internal DNS names. The "SSRF guard" marker stays, so
// logs and tests still tell a refusal from a connection-refused.
var errGuardRefused = errors.New("refusing a denied address range (SSRF guard)")

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// redactGuardRefusal keeps the refusal a dial *net.OpError (so
// IsBackendUnavailable / backends.Classify still see a transport failure)
// but drops the resolved address from it. addr is the host:port the row
// named — the tenant's own input — and is not repeated either.
func redactGuardRefusal(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil && strings.Contains(err.Error(), "SSRF guard") {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errGuardRefused}
		}
		return conn, err
	}
}

// Clients is one backend family's pair of keep-alive pools: the plain one for
// operator rows and the SSRF-guarded one for tenant rows. Keeping the two
// apart means a guarded call never reuses an idle connection the plain pool
// opened to an internal host (and vice versa).
type Clients struct {
	open    *http.Client
	guarded *http.Client
}

// NewClients builds the pair (a constructor, like PooledClient: each family
// keeps its own pools).
func NewClients() *Clients {
	return &Clients{open: PooledClient(), guarded: GuardedPooledClient()}
}

// For returns the client for one call. Callers pass
// backends.Backend.EgressGuarded() — that predicate is the single decision
// which rows are guarded; this is the single mapping onto a client.
func (c *Clients) For(guarded bool) *http.Client {
	if guarded {
		return c.guarded
	}
	return c.open
}
