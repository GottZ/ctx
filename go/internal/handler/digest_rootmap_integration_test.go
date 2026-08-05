//go:build integration

// Wave W-D handler gates (Cluster-Topic-Map, design/02 §4.2 + §7 "W-D"):
// POST /api/digest is the deliberate SECOND trigger of the root map, it answers
// with the map's address additively, and it finally carries the rate-limit
// bucket its read-only sibling has had for waves.
//
//	go test -tags=integration ./internal/handler/ -run TestDigestRootMap -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const wdKeyID = "00000000-0000-7000-8000-0000000000d1"

func wdDigestConfig() *config.Config {
	c := &config.Config{}
	c.RootMap.Enabled = true
	c.RootMap.BudgetBytes = 15360
	c.RootMap.FooterReserveBytes = 512
	c.RootMap.SmallClusterMax = 2
	c.RootMap.CountTimeout = 5 * time.Second
	c.GraphOverview.RebuildInterval = 6 * time.Hour
	return c
}

func wdPostDigest(t *testing.T, pool *pgxpool.Pool, cfg *config.Config) (int, map[string]any) {
	t.Helper()
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	h := NewDigestHandler(pool, reg, staticConfigStore{cfg: cfg})
	ar := &auth.AuthResult{IsValid: true, ApiKeyID: wdKeyID,
		HomeScope: "private", ReadScopes: []string{"private"}}

	req := httptest.NewRequest(http.MethodPost, "/api/digest", strings.NewReader(`{"trigger":"manual"}`))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleDigest(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return rec.Code, body
}

// TestDigestRootMapEnvelope: the endpoint writes the map and answers with its
// address, ADDITIVELY — the three legacy fields keep their names and meaning,
// because the CLI prints the envelope verbatim and a field rename is a break
// with no warning.
//
// RED against HEAD: no rootMapTitle/rootMapLength in the envelope and no
// root-map block in the database — POST /api/digest rebuilds the OLD map only.
func TestDigestRootMapEnvelope(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', 'wd-digest-fixture', 'body', 'private')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code, body := wdPostDigest(t, pool, wdDigestConfig())
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, body)
	}
	for _, k := range []string{"title", "blockCount", "categoryCount", "contentLength"} {
		if _, ok := body[k]; !ok {
			t.Errorf("envelope lost the legacy field %q — the CLI prints this verbatim", k)
		}
	}
	if got, _ := body["rootMapTitle"].(string); got != "root-map-private" {
		t.Errorf("rootMapTitle = %v, want root-map-private", body["rootMapTitle"])
	}
	if got, _ := body["rootMapLength"].(float64); got <= 0 {
		t.Errorf("rootMapLength = %v, want the size of a written map", body["rootMapLength"])
	}

	// The block itself: written under the caller's home scope, classified out of
	// retrieval, sensitivity internal (E4-02).
	var typeName, sensitivity string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(type_name, ''), COALESCE(sensitivity, '') FROM context_blocks
		  WHERE category = 'index' AND title = 'root-map-private' AND scope = 'private'`).
		Scan(&typeName, &sensitivity); err != nil {
		t.Fatalf("no root map written by POST /api/digest: %v", err)
	}
	if typeName != "system-meta" || sensitivity != "internal" {
		t.Errorf("map block: type=%q sensitivity=%q, want system-meta/internal", typeName, sensitivity)
	}

	// The map must NOT have started a rebuild — that is the one thing this
	// route may never do (a Louvain lever for every valid API key).
	var clusterRows int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM graph_cluster_node`).Scan(&clusterRows); err != nil {
		t.Fatalf("count cluster rows: %v", err)
	}
	if clusterRows != 0 {
		t.Errorf("POST /api/digest produced %d cluster rows — it triggered a rebuild", clusterRows)
	}
}

// TestDigestRootMapDisabled: with root_map.enabled=false the endpoint behaves
// exactly as before — no block, no map fields with content. The pausability
// invariant holds for the request path too, not only for the scheduler.
func TestDigestRootMapDisabled(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', 'wd-digest-off', 'body', 'private')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg := wdDigestConfig()
	cfg.RootMap.Enabled = false
	code, body := wdPostDigest(t, pool, cfg)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if got, _ := body["rootMapTitle"].(string); got != "" {
		t.Errorf("rootMapTitle = %q with the flag off", got)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_blocks WHERE title = 'root-map-private'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d root-map blocks written while the flag is off", n)
	}
}

// TestDigestRateLimitBucket: the route rebuilds a corpus-wide artefact and was
// the one such endpoint WITHOUT a bucket, while GET /api/graph/overview — which
// only reads — has carried one for waves.
//
// RED against HEAD: no bucket at all, so the response is 200 no matter how many
// calls precede it.
func TestDigestRateLimitBucket(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('wd-key') RETURNING id)
		 INSERT INTO context_api_keys (id, key_hash, label, home_scope, principal_id)
		 SELECT $1::uuid, 'wd-hash', 'wd-key', 'private', p.id FROM p`, wdKeyID); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_access_log (api_key_id, action) VALUES ($1::uuid, 'digest')`, wdKeyID); err != nil {
			t.Fatalf("seed access log: %v", err)
		}
	}

	cfg := wdDigestConfig()
	cfg.Query.RateLimitWrite = 2 // already reached by the two log rows above

	code, body := wdPostDigest(t, pool, cfg)
	if code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %v)", code, body)
	}

	// A different bucket must not be consumed by this one — the limit is per
	// action, and a shared counter would let a digest run starve /api/store.
	cfg.Query.RateLimitWrite = 3
	code, _ = wdPostDigest(t, pool, cfg)
	if code != http.StatusOK {
		t.Fatalf("status = %d under a raised limit, want 200", code)
	}
}
