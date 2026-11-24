//go:build integration

// Issue #236, point four: InsertChunk runs its upsert and, on a changed title
// or content, the vector invalidation in one transaction. What that costs a
// chunk, on an ingest of 1 000 chunks of 1.2 kB each in three passes: the
// first ingest (every chunk an insert), a re-ingest of the same text (every
// chunk the conflict branch, nothing changed) and a re-ingest of changed text
// over embedded chunks (the conflict branch and the invalidation). Logged per
// chunk: statements sent, shared-buffer accesses and WAL bytes of the backend
// that ran them (PostgreSQL 18's per-backend statistics) and the median wall
// time of a call. Asserted: the statement count of each pass — begin, the
// upsert, commit, and the invalidation's two statements where the text
// changed.
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestInsertChunkReingestCost -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestInsertChunkReingestCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	const n, scope = 1000, "private"
	src := i236Source(t, pool, scope)
	body := strings.Repeat("chunk text of an ingested file. ", 38) // 1 216 bytes
	var counter ubCounter
	actor := ubActor(t, dsn, "i236-chunk-cost", &counter)

	pass := func(t *testing.T, name, text string, wantStmts int64) []string {
		t.Helper()
		ids := make([]string, n)
		took := make([]time.Duration, n)
		stmts0, hits0, wal0 := counter.n.Load(), ubBufferAccesses(t, actor), ubWALBytes(t, actor)
		stmts0 += 4 // ubBufferAccesses and ubWALBytes, two statements each
		for i := range n {
			start := time.Now()
			b, err := store.InsertChunk(ctx, actor, src, i, "ingest", fmt.Sprintf("cost.md #%04d", i), text+fmt.Sprint(i), nil, nil, scope)
			took[i] = time.Since(start)
			if err != nil {
				t.Fatalf("%s: chunk %d: %v", name, i, err)
			}
			ids[i] = b.ID
		}
		stmts := counter.n.Load() - stmts0
		hits, wal := ubBufferAccesses(t, actor)-hits0, ubWALBytes(t, actor)-wal0
		slices.Sort(took)
		t.Logf("%s: %d chunks, %.2f statements, %.1f buffer accesses, %.0f WAL bytes per chunk, median %v per call",
			name, n, float64(stmts)/n, float64(hits)/n, float64(wal)/n, took[n/2])
		if stmts != wantStmts*n {
			t.Errorf("%s: %d statements, want %d per chunk (%d)", name, stmts, wantStmts, wantStmts*n)
		}
		return ids
	}

	ids := pass(t, "first ingest", body, 3)
	pass(t, "re-ingest, same text", body, 3)
	for _, id := range ids {
		if err := store.StoreEmbedding(ctx, pool, id, "i236-model", i236Vec()); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}
	pass(t, "re-ingest, changed text over embedded chunks", body+"edited ", 5)
	var stale int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE id = ANY($1::uuid[]) AND embedding_stale`, ids).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != n {
		t.Errorf("%d of %d re-ingested chunks marked stale", stale, n)
	}
}
