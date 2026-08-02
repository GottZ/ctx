// OpenRouter endpoint-window discovery (E10-W2). A provider_class=openrouter
// row routes ONE model over many providers whose context windows differ by an
// order of magnitude — live for qwen/qwen3.6-27b (2026-08-02): Io Net
// 32768/32768, Morph 131072/131072, Chutes 262144/65536, SiliconFlow
// 262144/262144, Phala 262144/262140, DeepInfra 262144/81920. The MODEL-level
// context_length the /models list reports (262144) is the MAXIMUM over those
// providers, not a property any single one of them guarantees; that is exactly
// why H12 refused to size a prompt against it.
//
// This file fetches the per-provider truth instead of guessing it:
// GET {base_url}/v1/models/{author}/{slug}/endpoints, cached per (host, model)
// with an operator TTL. It is the input to the AUTO window resolution and to
// the per-request provider constraint — the ONE place that knows a provider's
// window, so no consumer can invent one.
package backends

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ProviderEndpoint is one serving provider of a model as OpenRouter reports
// it. MaxCompletionTokens <= 0 means the endpoint declares no completion bound
// — UNKNOWN, not unlimited: the eligibility filter therefore does not
// disqualify on it and leans on the wire-level max_tokens (which OpenRouter's
// own routing enforces server-side) as the second layer.
type ProviderEndpoint struct {
	ProviderName        string
	ContextLength       int
	MaxCompletionTokens int
}

// endpointsResponse is the parsed shape of the discovery route. The
// data.context_length field of the enclosing model object is deliberately NOT
// parsed: it is the maximum over the endpoints below it, and reading it would
// re-introduce the exact over-estimate this discovery exists to replace.
type endpointsResponse struct {
	Data struct {
		Endpoints []struct {
			ProviderName        string `json:"provider_name"`
			ContextLength       int    `json:"context_length"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
		} `json:"endpoints"`
	} `json:"data"`
}

// Discovery timing constants.
const (
	// endpointFetchTimeout bounds ONE discovery GET. It sits inside a user
	// request (the synthesis path resolves windows before building the
	// prompt), so a hanging metadata call must not eat the caller's deadline:
	// the request degrades to the H12 behaviour (declared num_ctx / fallback /
	// refusal) rather than waiting.
	endpointFetchTimeout = 5 * time.Second
	// endpointStaleMax is the hard age limit for stale-while-error. Beyond it
	// the cached windows stop being evidence: a provider mix a day old may no
	// longer contain the endpoint the router picks. Serving stale data is a
	// deliberate availability choice for a transient API failure, not a
	// licence to run blind indefinitely.
	endpointStaleMax = 24 * time.Hour
	// endpointFailBackoff keeps a dead discovery API from costing every single
	// request a full endpointFetchTimeout. Within the backoff a cache miss
	// answers "no data" immediately (the caller then takes the H12 path).
	endpointFailBackoff = 60 * time.Second
	// endpointBodyCap bounds the response read. The endpoints list of one
	// model is a few kB; anything near this cap is not the document we asked
	// for.
	endpointBodyCap = 1 << 20
)

// endpointEntry is one cached discovery result. A zero fetched with a
// non-zero failedAt is the "never succeeded, currently failing" state.
type endpointEntry struct {
	eps      []ProviderEndpoint
	fetched  time.Time
	failedAt time.Time
}

// EndpointCache is the process-wide discovery cache. It is process STATE, not
// configuration (the TTL travels per call, from the settings snapshot), which
// is why it lives as a package-level singleton next to the wire clients rather
// than being threaded through every call site.
//
// The HTTP client is deliberately NOT the ssrfguard-hardened one: the guarded
// dialer exists for targets an outside party can influence (camo image URLs,
// forge api_base, SSO issuer discovery). This request goes to the very host
// the chat path already POSTs prompts to, unguarded, one function away — a
// deny-list on the metadata GET while the prompt POST travels free would be
// decoration, and it would break every LAN-hosted OpenAI-compatible gateway an
// operator may point an openrouter-class row at.
type EndpointCache struct {
	mu      sync.Mutex
	entries map[string]*endpointEntry
	client  *http.Client
	now     func() time.Time
}

// NewEndpointCache builds an empty cache over the default HTTP client.
func NewEndpointCache() *EndpointCache {
	return &EndpointCache{
		entries: map[string]*endpointEntry{},
		client:  &http.Client{},
		now:     time.Now,
	}
}

// defaultEndpointCache is the singleton the production call sites share.
var defaultEndpointCache = NewEndpointCache()

// DefaultEndpointCache returns the process-wide discovery cache.
func DefaultEndpointCache() *EndpointCache { return defaultEndpointCache }

// Endpoints resolves the per-provider endpoint list of model on backend b,
// serving the cache when it is fresh. ok=false means "no data" — the caller
// must then behave exactly as it did before this wave existed (declared
// num_ctx, operator fallback, or refusal); it never means "unconstrained".
//
// Freshness ladder:
//   - ttl <= 0                         → discovery OFF (the 0-is-off
//     convention of the pool settings), no request, no data
//   - age <= ttl                       → cache, no request
//   - ttl < age <= endpointStaleMax    → refresh; on failure the STALE data
//     stays valid (a transient 500 must not take a working chain down)
//   - age > endpointStaleMax, or no entry at all → refresh; on failure ok=false
func (c *EndpointCache) Endpoints(ctx context.Context, b Backend, model string, ttl time.Duration) ([]ProviderEndpoint, bool) {
	if b.Host == "" || model == "" || ttl <= 0 {
		return nil, false
	}
	key := b.Host + "|" + model
	now := c.now()

	// The whole cache read happens under the lock and yields VALUES — holding
	// a *endpointEntry across the unlock would race a concurrent refresh.
	c.mu.Lock()
	eps, fetched, failedAt := c.snapshot(key)
	c.mu.Unlock()

	if !fetched.IsZero() && now.Sub(fetched) <= ttl {
		return eps, len(eps) > 0
	}
	// Backoff: a dead API costs one timeout per backoff window, not one per
	// request. Stale-but-usable data keeps serving throughout.
	if !failedAt.IsZero() && now.Sub(failedAt) < endpointFailBackoff {
		return staleOrNothing(eps, fetched, now)
	}

	fresh, err := c.fetch(ctx, b, model)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || len(fresh) == 0 {
		entry := c.entries[key]
		if entry == nil {
			entry = &endpointEntry{}
			c.entries[key] = entry
		}
		entry.failedAt = now
		return staleOrNothing(entry.eps, entry.fetched, now)
	}
	c.entries[key] = &endpointEntry{eps: fresh, fetched: now}
	return fresh, true
}

// snapshot copies one cache entry's values. Caller holds the lock.
func (c *EndpointCache) snapshot(key string) ([]ProviderEndpoint, time.Time, time.Time) {
	entry := c.entries[key]
	if entry == nil {
		return nil, time.Time{}, time.Time{}
	}
	return entry.eps, entry.fetched, entry.failedAt
}

// staleOrNothing is the stale-while-error verdict: cached endpoints inside the
// hard age limit still count as evidence, older ones do not.
func staleOrNothing(eps []ProviderEndpoint, fetched, now time.Time) ([]ProviderEndpoint, bool) {
	if fetched.IsZero() || len(eps) == 0 || now.Sub(fetched) > endpointStaleMax {
		return nil, false
	}
	return eps, true
}

// fetch performs one discovery GET. Every failure mode — bad URL, transport,
// non-200, unparsable body — returns an error; the caller turns that into
// "no data", never into a guessed window.
func (c *EndpointCache) fetch(ctx context.Context, b Backend, model string) ([]ProviderEndpoint, error) {
	target, err := endpointsURL(b.Host, model)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, endpointFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	// The route is public; the key is sent when the row has one so a gateway
	// that requires auth on every path still answers.
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	for k, v := range b.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req) //nolint:gosec // G704: the target is the admin-configured backend base_url — the same host the chat path POSTs prompts to.
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backends: endpoint discovery status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, endpointBodyCap))
	if err != nil {
		return nil, err
	}
	var parsed endpointsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]ProviderEndpoint, 0, len(parsed.Data.Endpoints))
	for _, e := range parsed.Data.Endpoints {
		// An endpoint without a usable context length carries no evidence that
		// it can hold anything — dropping it keeps "eligible" honest.
		if e.ProviderName == "" || e.ContextLength <= 0 {
			continue
		}
		out = append(out, ProviderEndpoint{
			ProviderName:        e.ProviderName,
			ContextLength:       e.ContextLength,
			MaxCompletionTokens: e.MaxCompletionTokens,
		})
	}
	return out, nil
}

// endpointsURL builds {base}/v1/models/{author}/{slug}/endpoints. The model id
// carries its own path separator (author/slug) — that is the API's shape, so
// the segments are escaped individually instead of the whole string. Relative
// segments are rejected outright: a model id is not a path expression.
func endpointsURL(base, model string) (string, error) {
	segs := strings.Split(model, "/")
	esc := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("backends: model id %q is not a valid endpoint path", model)
		}
		esc = append(esc, url.PathEscape(s))
	}
	return strings.TrimSuffix(base, "/") + "/v1/models/" + strings.Join(esc, "/") + "/endpoints", nil
}
