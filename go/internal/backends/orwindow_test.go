// E10-W2 discovery probes: the endpoint cache is measured against httptest
// stubs and a controlled clock — never against openrouter.ai. Each case names
// the implementation it falsifies.
package backends

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// endpointsBody is the discovery shape as the live API returns it. The
// model-level context_length is 262144 — the MAXIMUM over the endpoints below
// it — and is present here on purpose: a parser that reads it instead of the
// per-endpoint values passes every "did we get a number" assertion while being
// exactly wrong.
const endpointsBody = `{"data":{"id":"qwen/qwen3.6-27b","context_length":262144,"endpoints":[
  {"provider_name":"Io Net","context_length":32768,"max_completion_tokens":32768},
  {"provider_name":"Chutes","context_length":262144,"max_completion_tokens":65536},
  {"provider_name":"DeepInfra","context_length":262144,"max_completion_tokens":81920}
]}}`

// discoveryStub serves the endpoints route and counts how often it was asked.
type discoveryStub struct {
	srv    *httptest.Server
	hits   atomic.Int32
	status atomic.Int32
	path   atomic.Value
}

func newDiscoveryStub(t *testing.T) *discoveryStub {
	t.Helper()
	s := &discoveryStub{}
	s.status.Store(http.StatusOK)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.path.Store(r.URL.Path)
		if code := int(s.status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte(endpointsBody))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *discoveryStub) lastPath() string {
	v, _ := s.path.Load().(string)
	return v
}

// testCache builds a cache over a clock the test moves by hand.
func testCache(s *discoveryStub, clock *time.Time) *EndpointCache {
	c := NewEndpointCache()
	c.client = s.srv.Client()
	c.now = func() time.Time { return *clock }
	return c
}

func discoveryBackend(host string) Backend {
	return Backend{
		ID: "or", Name: "or", Host: host, Protocol: ProtocolOpenAI,
		ProviderClass: ProviderOpenRouter, Trust: TrustNoCredentials, Enabled: true,
		ModelMap: map[string]ModelSpec{"default": {Model: "qwen/qwen3.6-27b"}},
	}
}

// TestEndpointsParsesPerProviderWindows is the parse half of probe (a): the
// per-endpoint windows survive, and the model-level context_length does not
// leak into any of them.
//
// Falsifying implementation: a parser reading data.context_length (262144) as
// THE window — every endpoint would come back at 262144 and the 32768-provider
// would disappear as a distinct case.
func TestEndpointsParsesPerProviderWindows(t *testing.T) {
	stub := newDiscoveryStub(t)
	clock := time.Unix(1_700_000_000, 0)
	eps, ok := testCache(stub, &clock).Endpoints(context.Background(), discoveryBackend(stub.srv.URL), "qwen/qwen3.6-27b", time.Hour)
	if !ok {
		t.Fatal("discovery returned no data")
	}
	want := []ProviderEndpoint{
		{ProviderName: "Io Net", ContextLength: 32768, MaxCompletionTokens: 32768},
		{ProviderName: "Chutes", ContextLength: 262144, MaxCompletionTokens: 65536},
		{ProviderName: "DeepInfra", ContextLength: 262144, MaxCompletionTokens: 81920},
	}
	if len(eps) != len(want) {
		t.Fatalf("got %d endpoints, want %d: %+v", len(eps), len(want), eps)
	}
	for i := range want {
		if eps[i] != want[i] {
			t.Errorf("endpoint %d = %+v, want %+v", i, eps[i], want[i])
		}
	}
	if got := stub.lastPath(); got != "/v1/models/qwen/qwen3.6-27b/endpoints" {
		t.Errorf("discovery path = %q, want /v1/models/{author}/{slug}/endpoints", got)
	}
}

// TestEndpointsCachesWithinTTL is probe (e), first half: a second resolution
// inside the TTL must not touch the network.
//
// Falsifying implementation: no cache (fetch on every call) — the stub counter
// reads 2. Measured: with the cache lookup removed this case goes red while
// every functional case stays green, which is why the counter is the assertion
// and not the returned data.
func TestEndpointsCachesWithinTTL(t *testing.T) {
	stub := newDiscoveryStub(t)
	clock := time.Unix(1_700_000_000, 0)
	c := testCache(stub, &clock)
	b := discoveryBackend(stub.srv.URL)

	if _, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour); !ok {
		t.Fatal("first resolution failed")
	}
	clock = clock.Add(59 * time.Minute)
	if _, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour); !ok {
		t.Fatal("second resolution failed")
	}
	if got := stub.hits.Load(); got != 1 {
		t.Errorf("discovery endpoint hit %d times inside the TTL, want 1", got)
	}

	// …and the refresh does happen once the TTL is over.
	clock = clock.Add(2 * time.Minute)
	if _, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour); !ok {
		t.Fatal("post-TTL resolution failed")
	}
	if got := stub.hits.Load(); got != 2 {
		t.Errorf("discovery endpoint hit %d times across the TTL boundary, want 2", got)
	}
}

// TestEndpointsStaleWhileError is probe (e), second half: when the refresh
// fails, the last known provider mix keeps serving.
//
// Falsifying implementation: a refresh that propagates its failure (return
// nil,false on any fetch error) — a transient 500 on a metadata route would
// then take down a synthesis chain that has perfectly good data cached, which
// is the opposite of what discovery is for.
func TestEndpointsStaleWhileError(t *testing.T) {
	stub := newDiscoveryStub(t)
	clock := time.Unix(1_700_000_000, 0)
	c := testCache(stub, &clock)
	b := discoveryBackend(stub.srv.URL)

	if _, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour); !ok {
		t.Fatal("first resolution failed")
	}
	stub.status.Store(http.StatusInternalServerError)
	clock = clock.Add(2 * time.Hour)

	eps, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour)
	if !ok {
		t.Fatal("a failed refresh dropped usable cached endpoints")
	}
	if len(eps) != 3 || eps[0].ContextLength != 32768 {
		t.Errorf("stale data = %+v, want the previously fetched mix", eps)
	}

	// Beyond the hard age limit the stale data stops being evidence.
	clock = clock.Add(endpointStaleMax)
	if _, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour); ok {
		t.Errorf("endpoints older than %s still served — stale-while-error has no age limit", endpointStaleMax)
	}
}

// TestEndpointsFailureBackoff pins the cost of a dead discovery API: one
// request per backoff window, not one per caller.
func TestEndpointsFailureBackoff(t *testing.T) {
	stub := newDiscoveryStub(t)
	stub.status.Store(http.StatusInternalServerError)
	clock := time.Unix(1_700_000_000, 0)
	c := testCache(stub, &clock)
	b := discoveryBackend(stub.srv.URL)

	for i := 0; i < 3; i++ {
		if _, ok := c.Endpoints(context.Background(), b, "qwen/qwen3.6-27b", time.Hour); ok {
			t.Fatal("a failing discovery reported data")
		}
		clock = clock.Add(time.Second)
	}
	if got := stub.hits.Load(); got != 1 {
		t.Errorf("dead discovery API contacted %d times inside the backoff, want 1", got)
	}
}

// TestEndpointsOffAtZeroTTL pins the 0-is-off convention: no request, no data,
// and therefore the H12 path downstream.
func TestEndpointsOffAtZeroTTL(t *testing.T) {
	stub := newDiscoveryStub(t)
	clock := time.Unix(1_700_000_000, 0)
	if _, ok := testCache(stub, &clock).Endpoints(context.Background(), discoveryBackend(stub.srv.URL), "qwen/qwen3.6-27b", 0); ok {
		t.Error("discovery ran at ttl=0 — the off switch does not switch off")
	}
	if got := stub.hits.Load(); got != 0 {
		t.Errorf("discovery endpoint hit %d times at ttl=0, want 0", got)
	}
}

// TestEndpointsURLRejectsRelativeSegments: a model id is a path segment pair,
// not a path expression — traversal never leaves the /v1/models prefix.
func TestEndpointsURLRejectsRelativeSegments(t *testing.T) {
	if _, err := endpointsURL("https://openrouter.ai/api", "../../v1/key"); err == nil {
		t.Error("a traversal model id built a URL")
	}
	got, err := endpointsURL("https://openrouter.ai/api/", "qwen/qwen3.6-27b")
	if err != nil {
		t.Fatalf("endpointsURL: %v", err)
	}
	if want := "https://openrouter.ai/api/v1/models/qwen/qwen3.6-27b/endpoints"; got != want {
		t.Errorf("endpointsURL = %q, want %q", got, want)
	}
}

// TestEndpointsDropsUnusableRows: an endpoint without a context length carries
// no evidence it can hold anything; keeping it would let "eligible" include a
// provider whose window is unknown.
func TestEndpointsDropsUnusableRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"endpoints":[
		  {"provider_name":"Ghost"},
		  {"provider_name":"","context_length":9999},
		  {"provider_name":"Real","context_length":8192}
		]}}`))
	}))
	t.Cleanup(srv.Close)
	clock := time.Unix(1_700_000_000, 0)
	c := NewEndpointCache()
	c.client = srv.Client()
	c.now = func() time.Time { return clock }
	eps, ok := c.Endpoints(context.Background(), discoveryBackend(srv.URL), "m/x", time.Hour)
	if !ok || len(eps) != 1 || eps[0].ProviderName != "Real" {
		t.Errorf("endpoints = %+v, want only the row that declares a window", eps)
	}
}

// TestEndpointsRefusesUnparsableBody: a body that is not the discovery
// document is "no data", never a partially-invented window.
func TestEndpointsRefusesUnparsableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("<html>", 10)))
	}))
	t.Cleanup(srv.Close)
	clock := time.Unix(1_700_000_000, 0)
	c := NewEndpointCache()
	c.client = srv.Client()
	c.now = func() time.Time { return clock }
	if _, ok := c.Endpoints(context.Background(), discoveryBackend(srv.URL), "m/x", time.Hour); ok {
		t.Error("an HTML error page parsed as endpoint data")
	}
}
