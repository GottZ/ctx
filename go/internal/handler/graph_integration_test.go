//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	hgShared  = "019e0007-1111-7000-9000-000000000001" // shared block
	hgShared2 = "019e0007-1111-7000-9000-000000000002" // shared neighbor
	hgPrivate = "019e0007-1111-7000-9000-000000000011" // private block
	hgMissing = "019e0007-dead-7000-9000-00000000eeee" // never inserted
	hgKeyA    = "019e0007-2222-7000-9000-0000000000aa" // tenant A api key id
	hgKeyB    = "019e0007-2222-7000-9000-0000000000bb" // tenant B api key id
)

func hgSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, ins := range []struct{ id, scope string }{
		{hgShared, "shared"},
		{hgShared2, "shared"},
		{hgPrivate, "private"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope)
			 VALUES ($1::uuid, 'graphtest', 'blk-'||right($1::text, 4), 'content', $2)`,
			ins.id, ins.scope,
		); err != nil {
			t.Fatalf("insert block %s: %v", ins.id, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'shared', 5)`,
		hgShared, hgShared2,
	); err != nil {
		t.Fatalf("insert link: %v", err)
	}

	for _, k := range []struct{ id, home string }{
		{hgKeyA, "private"},
		{hgKeyB, "work"},
	} {
		if _, err := pool.Exec(ctx,
			`WITH p AS (
			     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
			 )
			 INSERT INTO context_api_keys (id, key_hash, label, home_scope, allowed_scopes, principal_id)
			 SELECT $1::uuid, 'graph-test-hash-'||$2, $2, $2, '{shared}', p.id FROM p`,
			k.id, k.home,
		); err != nil {
			t.Fatalf("insert api key %s: %v", k.id, err)
		}
	}
	return pool
}

func hgAuth(keyID, home string) *auth.AuthResult {
	return &auth.AuthResult{
		ApiKeyID:      keyID,
		HomeScope:     home,
		AllowedScopes: []string{"shared"},
		ReadScopes:    []string{home, "shared"},
		IsValid:       true,
	}
}

// hgDo runs HandleEgo with an injected AuthResult and returns the recorder.
func hgDo(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult, query string) *httptest.ResponseRecorder {
	t.Helper()
	// Zero-valued config: RateLimitRead 0 = disabled, like the old literal 0.
	// Builtin registry set (T6): the fixture blocks are default-typed
	// 'knowledge', so the compiled-in visible allowlist matches the pre-T6
	// literal semantics for this fixture.
	h := NewGraphHandler(pool, config.NewStore(&config.Config{}), blocktype.NewRegistry())
	req := httptest.NewRequest("GET", "/api/graph/ego?"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleEgo(rec, req)
	return rec
}

func hgAccessLogCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_access_log`).Scan(&n); err != nil {
		t.Fatalf("count access_log: %v", err)
	}
	return n
}

// NEGATIVE PROBE §5.2.4 (existence oracle): an invisible focus and a
// nonexistent focus answer with byte-identical 404 bodies — no tenant can
// verify the existence of foreign blocks by UUID probing.
func TestHandleEgo_404Oracle(t *testing.T) {
	pool := hgSetup(t)
	arB := hgAuth(hgKeyB, "work")

	recInvisible := hgDo(t, pool, arB, "block="+hgPrivate)
	recMissing := hgDo(t, pool, arB, "block="+hgMissing)

	if recInvisible.Code != 404 || recMissing.Code != 404 {
		t.Fatalf("status: invisible=%d missing=%d, want 404/404", recInvisible.Code, recMissing.Code)
	}
	if recInvisible.Body.String() != recMissing.Body.String() {
		t.Errorf("404 bodies differ — existence oracle:\n invisible: %s\n missing:   %s",
			recInvisible.Body.String(), recMissing.Body.String())
	}
}

// NEGATIVE PROBE §5.2.8 (access_log discipline): the 404 path writes NO
// access_log row (no telemetric bump for invisible blocks via probing); a
// successful call writes exactly one row with action='graph', block_id IS
// NULL and the focus in metadata.
func TestHandleEgo_AccessLogDiscipline(t *testing.T) {
	pool := hgSetup(t)
	ctx := context.Background()

	// 404 path (invisible focus as B) → zero rows.
	rec := hgDo(t, pool, hgAuth(hgKeyB, "work"), "block="+hgPrivate)
	if rec.Code != 404 {
		t.Fatalf("setup: want 404, got %d", rec.Code)
	}
	if n := hgAccessLogCount(t, pool); n != 0 {
		t.Errorf("404 path wrote %d access_log rows, want 0", n)
	}

	// 400 path (param error) → still zero rows.
	rec = hgDo(t, pool, hgAuth(hgKeyB, "work"), "block="+hgShared+"&hops=9")
	if rec.Code != 400 {
		t.Fatalf("setup: want 400, got %d", rec.Code)
	}
	if n := hgAccessLogCount(t, pool); n != 0 {
		t.Errorf("400 path wrote %d access_log rows, want 0", n)
	}

	// Success path as A → exactly one row, action='graph', block_id NULL,
	// metadata carries the focus.
	rec = hgDo(t, pool, hgAuth(hgKeyA, "private"), "block="+hgShared+"&hops=1")
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var (
		action  string
		blockID *string
		keyID   *string
		focus   string
		nodeCnt int
	)
	err := pool.QueryRow(ctx, `
		SELECT action, block_id::text, api_key_id::text,
		       metadata->>'focus', (metadata->>'node_count')::int
		FROM context_access_log`).
		Scan(&action, &blockID, &keyID, &focus, &nodeCnt)
	if err != nil {
		t.Fatalf("read access_log row: %v", err)
	}
	if n := hgAccessLogCount(t, pool); n != 1 {
		t.Errorf("success path wrote %d rows, want exactly 1", n)
	}
	if action != "graph" {
		t.Errorf("action = %q, want graph", action)
	}
	if blockID != nil {
		t.Errorf("block_id = %v, want NULL (decoupled from access-count ranking)", *blockID)
	}
	if keyID == nil || *keyID != hgKeyA {
		t.Errorf("api_key_id = %v, want %s", keyID, hgKeyA)
	}
	if focus != hgShared {
		t.Errorf("metadata.focus = %q, want %s", focus, hgShared)
	}
	if nodeCnt != 2 { // focus + one shared neighbor
		t.Errorf("metadata.node_count = %d, want 2", nodeCnt)
	}
}

// Wire contract over the real stack: 200 envelope has success/focus/params/
// rels/nodes/edges/stats, no content field anywhere, stats.truncated present.
func TestHandleEgo_WireContract(t *testing.T) {
	pool := hgSetup(t)

	rec := hgDo(t, pool, hgAuth(hgKeyA, "private"), "block="+hgShared)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, k := range []string{"success", "focus", "params", "rels", "nodes", "edges", "stats"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("envelope missing %q", k)
		}
	}
	stats, ok := resp["stats"].(map[string]any)
	if !ok {
		t.Fatal("stats is not an object")
	}
	if _, ok := stats["truncated"]; !ok {
		t.Error("stats.truncated missing")
	}
	nodes, ok := resp["nodes"].([]any)
	if !ok || len(nodes) == 0 {
		t.Fatal("nodes missing or empty")
	}
	for i, n := range nodes {
		nm, ok := n.(map[string]any)
		if !ok {
			t.Fatalf("node %d not an object", i)
		}
		if _, has := nm["content"]; has {
			t.Errorf("node %d carries a content field — privacy contract broken", i)
		}
	}
}
