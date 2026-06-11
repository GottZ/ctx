// F1-W4 snapshot discipline: HandleQuery takes exactly ONE config snapshot
// per request and derives every stage (rate limit, chat tuple, embed tuple,
// heartbeat gate, rerank dispatch, synthesis settings) from it. These tests
// pin the invariant with a counting fake store — a second Snapshot() call
// anywhere in the request path, or a cached boot-copy that survives a config
// replace, fails here.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// countingStore implements ConfigStore and counts Snapshot calls.
type countingStore struct {
	calls atomic.Int32
	cfg   atomic.Pointer[config.Config]
}

func (s *countingStore) Snapshot() *config.Config {
	s.calls.Add(1)
	return s.cfg.Load()
}

// snapshotTestConfig builds a minimal request-path config. Hosts use
// RFC-2606 documentation names except the embed host, which points at the
// test's fake server — the only backend this path actually contacts.
func snapshotTestConfig(embedHost string) *config.Config {
	return &config.Config{
		Chat: config.ChatConfig{
			Host: "http://chat.invalid", Protocol: "ollama", Model: "test-chat",
		},
		Embed: config.EmbedConfig{
			Host: embedHost, Protocol: "ollama", Model: "test-embed",
		},
		// Rerank/Graph stay zero-valued = disabled (no heartbeat, no sidecar).
		Query: config.QueryConfig{Timezone: time.UTC}, // RateLimitRead 0 = disabled
	}
}

// fakeEmbedServer serves the Ollama /api/embed wire shape with a vector that
// passes the embed quality gates (norm ~1 after L2, variance above floor).
func fakeEmbedServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2) // 0,2 alternating
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("unexpected embed path %q", r.URL.Path)
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// brokenPool returns a lazily connecting pool against a closed local port:
// construction succeeds, every acquire fails fast. backfillPending fails
// open, the embed cache falls through to the backend, and rrf.Search errors
// — the deepest stage this DB-less unit test can reach (the stages behind it
// share the same single cfg local by construction).
func brokenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://test:test@127.0.0.1:1/test?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// authedQueryRequest builds a POST /api/query request carrying a valid
// AuthResult, the way the Auth middleware would.
func authedQueryRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ar := &auth.AuthResult{
		ApiKeyID:   "019e0011-aaaa-7000-9000-000000000001",
		HomeScope:  "private",
		ReadScopes: []string{"private"},
		IsValid:    true,
	}
	return req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
}

// An unauthenticated request exits at the auth step — and must still have
// read exactly one snapshot. Counter 1 (not 0) also pins the snapshot as the
// FIRST statement of HandleQuery, ahead of parse and auth.
func TestHandleQuery_OneSnapshotPerRequest_EarlyExit(t *testing.T) {
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig("http://embed.invalid"))
	h := NewQueryHandler(nil, st) // pool is never touched before auth

	rec := httptest.NewRecorder()
	h.HandleQuery(rec, httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"query":"alpha"}`)))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := st.calls.Load(); got != 1 {
		t.Errorf("Snapshot calls = %d, want exactly 1 per request", got)
	}
}

// The deep path: auth -> rate limit -> translate gate -> temporal rules ->
// backfill (fails open) -> query embed (against the snapshot's embed host) ->
// rrf.Search (500 on the broken pool). Still exactly one snapshot — and the
// embed call landing on the fake server proves the tuple came from the
// snapshot, not from any constructor copy.
func TestHandleQuery_OneSnapshotPerRequest_DeepPath(t *testing.T) {
	srv, hits := fakeEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig(srv.URL))
	h := NewQueryHandler(brokenPool(t), st)

	rec := httptest.NewRecorder()
	h.HandleQuery(rec, authedQueryRequest(`{"query":"alpha bravo charlie"}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 at the search stage (body %q)", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("embed backend hits = %d, want 1", got)
	}
	if got := st.calls.Load(); got != 1 {
		t.Errorf("Snapshot calls = %d, want exactly 1 per request", got)
	}
}

// Hot-flip regression: a new generation published between two requests must
// be live for the second one — one snapshot per request, none cached across
// requests. The pre-W4 handler held boot copies as fields; under this test it
// would keep embedding against generation A forever.
func TestHandleQuery_SnapshotPerRequest_HotFlip(t *testing.T) {
	srvA, hitsA := fakeEmbedServer(t)
	srvB, hitsB := fakeEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig(srvA.URL))
	h := NewQueryHandler(brokenPool(t), st)

	h.HandleQuery(httptest.NewRecorder(), authedQueryRequest(`{"query":"alpha bravo"}`))
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("request 1 must embed against generation A: hits A=%d B=%d", hitsA.Load(), hitsB.Load())
	}

	st.cfg.Store(snapshotTestConfig(srvB.URL)) // publish generation B

	h.HandleQuery(httptest.NewRecorder(), authedQueryRequest(`{"query":"alpha bravo"}`))
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("request 2 must embed against generation B: hits A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	if got := st.calls.Load(); got != 2 {
		t.Errorf("Snapshot calls = %d, want 2 (one per request)", got)
	}
}
