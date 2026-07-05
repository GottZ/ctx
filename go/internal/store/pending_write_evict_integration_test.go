//go:build integration

// Integration coverage for the D-W3 staging-store eviction (chunk-drop) and
// the D2-M2 index probe:
//
//  1. retention=0 ⇒ NOTHING evicted (0-is-off, decoupled from confirm_ttl).
//  2. retention>0 ⇒ aged chunks drop — INCLUDING never-confirmed stages
//     (consumed_at IS NULL): chunk-drop is column-blind, which is the
//     structural D2-M3 guarantee a consumed_at-filtered DELETE would not give.
//     (Rot-Probe, dokumentiert im Commit: eine Mutation auf
//     `DELETE ... WHERE consumed_at IS NOT NULL` lässt die nie-bestätigte
//     Stage überleben ⇒ dieser Test wird rot.)
//  3. Fresh stages in the current 1h chunk survive (a chunk drops only when
//     its ENTIRE range is older than the cutoff).
//  4. D2-M2: the consume/lookup selector uses idx_pending_open (EXPLAIN
//     probe over a populated table — no full seq scan).
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestPendingWriteEvict -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestPendingWriteEviction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	key, _, err := store.CreateApiKey(ctx, pool, "pw-evict", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Aged stages land in their own (old) 1h chunks via explicit created_at:
	// one consumed, one NEVER consumed (the D2-M3 case).
	insertAged := func(hash string, consumed bool, age time.Duration) {
		consumedAt := "NULL"
		if consumed {
			consumedAt = "now()"
		}
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO context_pending_writes
			       (api_key_id, scope, op, origin, payload, payload_hash, created_at, consumed_at)
			VALUES ($1, 'private', 'store', 'mcp', '{"op":"store"}', $2, now() - $3::interval, %s)`, consumedAt),
			key.ID, hash, age.String())
		if err != nil {
			t.Fatalf("insert aged stage %s: %v", hash, err)
		}
	}
	insertAged("aged-consumed", true, 48*time.Hour)
	insertAged("aged-never-confirmed", false, 48*time.Hour)

	if _, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp",
		[]byte(`{"op":"store"}`), "fresh", 10*time.Minute); err != nil {
		t.Fatalf("stage fresh: %v", err)
	}

	countRows := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("retention=0 evicts nothing (0-is-off)", func(t *testing.T) {
		dropped, err := store.EvictPendingWrites(ctx, pool, 0)
		if err != nil {
			t.Fatalf("evict: %v", err)
		}
		if dropped != 0 {
			t.Fatalf("retention=0 dropped %d chunks, want 0", dropped)
		}
		if n := countRows(); n != 3 {
			t.Fatalf("retention=0 left %d rows, want 3", n)
		}
	})

	t.Run("aged chunks drop, never-confirmed included, fresh survives", func(t *testing.T) {
		dropped, err := store.EvictPendingWrites(ctx, pool, 24*time.Hour)
		if err != nil {
			t.Fatalf("evict: %v", err)
		}
		if dropped < 1 {
			t.Fatalf("dropped %d chunks, want >= 1", dropped)
		}
		var agedLeft int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_pending_writes WHERE payload_hash LIKE 'aged-%'`).Scan(&agedLeft); err != nil {
			t.Fatalf("count aged: %v", err)
		}
		if agedLeft != 0 {
			t.Fatalf("%d aged stages survived eviction, want 0 (D2-M3: chunk-drop is column-blind)", agedLeft)
		}
		if _, err := store.LookupPendingWrite(ctx, pool, key.ID, "fresh"); err != nil {
			t.Fatalf("fresh stage did not survive eviction: %v", err)
		}
	})

	t.Run("consume/lookup selector uses idx_pending_open (D2-M2)", func(t *testing.T) {
		// Populate enough consumed history that a seq scan would be the wrong
		// plan, then ANALYZE so the planner sees it.
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_pending_writes
			       (api_key_id, scope, op, origin, payload, payload_hash, consumed_at)
			SELECT $1, 'private', 'store', 'mcp', '{"op":"store"}', 'hist-' || g, now()
			  FROM generate_series(1, 2000) g`, key.ID); err != nil {
			t.Fatalf("populate history: %v", err)
		}
		if _, err := pool.Exec(ctx, `ANALYZE context_pending_writes`); err != nil {
			t.Fatalf("analyze: %v", err)
		}
		rows, err := pool.Query(ctx, `
			EXPLAIN
			SELECT id FROM context_pending_writes
			 WHERE api_key_id = $1 AND payload_hash = $2
			   AND consumed_at IS NULL
			   AND (expires_at IS NULL OR expires_at > now())
			 ORDER BY created_at DESC LIMIT 1`, key.ID, "fresh")
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		if !strings.Contains(plan.String(), "idx_pending_open") {
			t.Fatalf("selector plan does not use idx_pending_open (D2-M2):\n%s", plan.String())
		}
	})
}
