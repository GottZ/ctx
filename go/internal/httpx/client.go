package httpx

import (
	"net/http"
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
func PooledClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// GuardedPooledClient is PooledClient dialing through internal/ssrfguard: every
// resolved address — first hop, every redirect hop, every DNS answer — is
// re-checked at dial time, so loopback, private, link-local and unspecified
// targets are refused before a byte flows (GHSA 2026-10-31 §3). It serves the
// tenant-owned pool rows (backends.Backend.EgressGuarded); the pool values are
// PooledClient's. ForceAttemptHTTP2 keeps HTTP/2 negotiation, which a custom
// DialContext would otherwise switch off.
func GuardedPooledClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         ssrfguard.GuardedDialer().DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
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
