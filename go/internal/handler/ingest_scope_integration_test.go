//go:build integration

// §9e probes for POST /api/ingest chunk mode. The (source_id, chunk_index)
// unique index carries no scope, so before the fix a key that knew a source id
// overwrote that source's chunks in any scope; the route had no write rate
// limit, and chunk content never met the credentials detector.
//
// RED against the parent commit: TestIngestChunkScopeBinding/foreign_source
// and /foreign_chunk_slot overwrite the foreign chunk (status "inserted",
// content replaced), /detector_raises leaves the chunk 'public', and
// TestIngestRateLimitBucket answers 200 past the limit.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ingestKey mints a default-tenant key with home scope 'private' and returns
// an authed request context plus its auth result.
func ingestKey(t *testing.T, pool *pgxpool.Pool, label string) (context.Context, *auth.AuthResult) {
	t.Helper()
	ctx := context.Background()
	_, plain, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key %s: %v", label, err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate %s: %v", label, err)
	}
	return context.WithValue(ctx, authResultKey, ar), ar
}

func postIngest(t *testing.T, h *IngestHandler, keyCtx context.Context, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(string(raw))).WithContext(keyCtx)
	rec := httptest.NewRecorder()
	h.HandleIngest(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /api/ingest body %s: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

// seedSource inserts a context_sources row in scope and returns its id.
func seedSource(t *testing.T, pool *pgxpool.Pool, path, scope string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ($1, 's9e', $2) RETURNING id::text`,
		path, scope).Scan(&id); err != nil {
		t.Fatalf("seed source %s: %v", path, err)
	}
	return id
}

// seedChunk inserts a chunk row directly (the owner's earlier ingest).
func seedChunk(t *testing.T, pool *pgxpool.Pool, sourceID string, idx int, title, content, scope, sens string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, source_id, chunk_index, lifecycle_state, sensitivity, sensitivity_source)
		 VALUES ('vault', $1, $2, $3, $4::uuid, $5, 'chunk', $6, 'manual') RETURNING id::text`,
		title, content, scope, sourceID, idx, sens).Scan(&id); err != nil {
		t.Fatalf("seed chunk %s: %v", title, err)
	}
	return id
}

type chunkState struct{ content, scope, sensitivity, source string }

func readChunk(t *testing.T, pool *pgxpool.Pool, id string) chunkState {
	t.Helper()
	var s chunkState
	if err := pool.QueryRow(context.Background(),
		`SELECT content, scope, sensitivity, sensitivity_source FROM context_blocks WHERE id = $1::uuid`, id).
		Scan(&s.content, &s.scope, &s.sensitivity, &s.source); err != nil {
		t.Fatalf("read chunk %s: %v", id, err)
	}
	return s
}

func firstResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	results, _ := resp["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v, want exactly one", resp["results"])
	}
	r, _ := results[0].(map[string]any)
	return r
}

func TestIngestChunkScopeBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewIngestHandler(pool, nil)
	keyCtx, _ := ingestKey(t, pool, "s9e-attacker")

	chunk := func(sourceID string, idx int, title, content string) map[string]any {
		return map[string]any{"source": "s9e", "chunks": []map[string]any{{
			"category": "vault", "title": title, "content": content,
			"source_id": sourceID, "chunk_index": idx,
		}}}
	}

	// Key in 'private' overwrites a chunk whose SOURCE lives in 'isotest'.
	t.Run("foreign_source", func(t *testing.T) {
		src := seedSource(t, pool, "/s9e/foreign.md", "isotest")
		victim := seedChunk(t, pool, src, 0, "s9e foreign chunk", "isotest original", "isotest", "personal")

		status, resp := postIngest(t, h, keyCtx, chunk(src, 0, "s9e hijack", "attacker content"))
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 with a per-chunk failure (body %v)", status, resp)
		}
		r := firstResult(t, resp)
		if r["status"] != "failed" || r["error"] != "source not found" || r["id"] != nil {
			t.Fatalf("result = %v, want failed/source not found without an id", r)
		}
		if got := readChunk(t, pool, victim); got.content != "isotest original" || got.scope != "isotest" {
			t.Fatalf("foreign chunk mutated: %+v", got)
		}
	})

	// The source is the caller's, but the (source_id, chunk_index) slot is
	// held by a chunk in another scope — the conflict arm must not update it.
	t.Run("foreign_chunk_slot", func(t *testing.T) {
		src := seedSource(t, pool, "/s9e/mixed.md", "private")
		victim := seedChunk(t, pool, src, 0, "s9e slot chunk", "isotest slot original", "isotest", "personal")

		_, resp := postIngest(t, h, keyCtx, chunk(src, 0, "s9e slot hijack", "attacker slot content"))
		r := firstResult(t, resp)
		if r["status"] != "failed" || r["error"] != "source not found" || r["id"] != nil {
			t.Fatalf("result = %v, want failed/source not found without an id", r)
		}
		if got := readChunk(t, pool, victim); got.content != "isotest slot original" || got.scope != "isotest" {
			t.Fatalf("foreign chunk mutated: %+v", got)
		}
	})

	// An absent source answers exactly like a foreign one (no oracle).
	t.Run("absent_source_same_answer", func(t *testing.T) {
		_, resp := postIngest(t, h, keyCtx, chunk("0198ffff-0000-7000-8000-000000000000", 0, "s9e absent", "x"))
		r := firstResult(t, resp)
		if r["status"] != "failed" || r["error"] != "source not found" {
			t.Fatalf("result = %v, want failed/source not found", r)
		}
	})

	// Regression: the caller's own source and chunk still upsert.
	t.Run("own_chunk_updates", func(t *testing.T) {
		src := seedSource(t, pool, "/s9e/own.md", "private")
		own := seedChunk(t, pool, src, 0, "s9e own chunk", "own original", "private", "internal")

		_, resp := postIngest(t, h, keyCtx, chunk(src, 0, "s9e own chunk", "own updated"))
		r := firstResult(t, resp)
		if r["status"] == "failed" || r["id"] != own {
			t.Fatalf("result = %v, want the own chunk %s updated", r, own)
		}
		if got := readChunk(t, pool, own); got.content != "own updated" || got.sensitivity != "internal" {
			t.Fatalf("own chunk = %+v, want updated content, sensitivity unchanged", got)
		}

		_, resp = postIngest(t, h, keyCtx, chunk(src, 1, "s9e own chunk 2", "a fresh chunk"))
		if r := firstResult(t, resp); r["status"] != "inserted" {
			t.Fatalf("fresh own chunk result = %v, want inserted", r)
		}
	})

	// The credentials detector runs on chunk content: an overwrite raises a
	// lower-classified chunk to credentials/'pattern', a fresh chunk gets the
	// verdict stamped (not just the DDL default).
	t.Run("detector_raises", func(t *testing.T) {
		src := seedSource(t, pool, "/s9e/creds.md", "private")
		own := seedChunk(t, pool, src, 0, "s9e creds chunk", "harmless", "private", "public")
		secret := "deploy key AKIA" + strings.Repeat("Z", 16)

		_, resp := postIngest(t, h, keyCtx, chunk(src, 0, "s9e creds chunk", secret))
		if r := firstResult(t, resp); r["status"] == "failed" {
			t.Fatalf("overwrite result = %v", r)
		}
		if got := readChunk(t, pool, own); got.sensitivity != "credentials" || got.source != "pattern" {
			t.Fatalf("overwritten chunk = %s/%s, want credentials/pattern", got.sensitivity, got.source)
		}

		_, resp = postIngest(t, h, keyCtx, chunk(src, 1, "s9e creds chunk 2", secret+" again"))
		r := firstResult(t, resp)
		id, _ := r["id"].(string)
		if id == "" {
			t.Fatalf("fresh chunk result = %v", r)
		}
		if got := readChunk(t, pool, id); got.sensitivity != "credentials" || got.source != "pattern" {
			t.Fatalf("fresh chunk = %s/%s, want credentials/pattern", got.sensitivity, got.source)
		}
	})
}

// TestIngestRateLimitBucket: /api/ingest books one "ingest" row per accepted
// request and refuses with 429 once the key's bucket reached
// query.rate_limit_write — the same convention as /api/store and digest.
func TestIngestRateLimitBucket(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	keyCtx, ar := ingestKey(t, pool, "s9e-ratelimit")

	body := func(title string) map[string]any {
		return map[string]any{"source": "s9e", "chunks": []map[string]any{{
			"category": "vault", "title": title, "content": "rate limit probe " + title,
		}}}
	}
	bucket := func() int {
		n, err := store.CheckRateLimitByAction(ctx, pool, ar.ApiKeyID, ingestBucket)
		if err != nil {
			t.Fatalf("count bucket: %v", err)
		}
		return n
	}

	h := NewIngestHandler(pool, staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitWrite: 2}}})
	for i, title := range []string{"s9e rl a", "s9e rl b"} {
		if status, resp := postIngest(t, h, keyCtx, body(title)); status != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (body %v)", i, status, resp)
		}
	}
	if got := bucket(); got != 2 {
		t.Fatalf("ingest bucket = %d after two requests, want 2", got)
	}
	status, resp := postIngest(t, h, keyCtx, body("s9e rl c"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("third request: status = %d, want 429 (body %v)", status, resp)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title = 's9e rl c'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused request wrote %d block(s) (err %v)", n, err)
	}
	if got := bucket(); got != 2 {
		t.Fatalf("refused request booked the bucket: %d, want 2", got)
	}

	// 0 = disabled, and the bucket is per action: the "write" rows of the
	// chunks above do not count here, and a disabled limit books nothing.
	off := NewIngestHandler(pool, staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitWrite: 0}}})
	if status, _ := postIngest(t, off, keyCtx, body("s9e rl d")); status != http.StatusOK {
		t.Fatalf("limit 0: status = %d, want 200", status)
	}
	if got := bucket(); got != 2 {
		t.Fatalf("disabled limit booked the bucket: %d, want 2", got)
	}
}
