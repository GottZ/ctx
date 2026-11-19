//go:build integration

// Issue #223 at the handler: the query path hands rerank.timeout from the
// request's config snapshot to the cross-encoder call. The rrf tests pin what
// the deadline does inside the call; this one pins that the handler passes the
// tenant's value and not a constant.
package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
)

// TestQueryRerankTimeoutFromSettings: a cross-encoder that never answers ends
// at the configured 1 s deadline and the query answers on the pre-rerank
// order, well inside the stall's own 20 s bound.
//
// Falsifying implementation: the handler passing 0 (or a constant) instead of
// rerankCfg.Timeout — the call then waits out the 20 s bound of the stalled
// sidecar and the elapsed-time assertion fails.
func TestQueryRerankTimeoutFromSettings(t *testing.T) {
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)

	var rerankHits atomic.Int32
	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		rerankHits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	t.Cleanup(stalled.Close)

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "k1-embed", Host: emb.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
		{ID: "r", Name: "k1-rerank", Host: stalled.URL, Protocol: "rerank",
			Model: "test-rerank", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleRerank}},
	})
	cfg := bw2Config()
	cfg.Rerank = config.RerankConfig{Enabled: true, MaxDocs: 50, BlendWeight: 1.0, Timeout: time.Second}
	h := NewQueryHandler(pool, config.NewStore(cfg), bpool, nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	start := time.Now()
	rec := bw2Do(t, h, bw2UserKeyID, false, `{"query":"`+bw2GoldenQuery+`","synthesize":false}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if n := rerankHits.Load(); n != 1 {
		t.Fatalf("cross-encoder hits = %d, want 1 — the probe never reached the rerank wire", n)
	}
	if elapsed > 10*time.Second {
		t.Errorf("query took %v with rerank.timeout = 1s — the handler did not pass the setting to the call", elapsed)
	}
	sources, _ := bw2DecodeBody(t, rec)["sources"].([]any)
	if len(sources) < 3 {
		t.Fatalf("sources = %d, want the fixture's results on the pre-rerank order", len(sources))
	}
	for i, s := range sources {
		if m, _ := s.(map[string]any); m["rerank_score"] != nil {
			t.Errorf("sources[%d] carries rerank_score %v — the timed-out rerank must leave the order unscored", i, m["rerank_score"])
		}
	}
}
