//go:build integration

// The duplicate guard's keys in a block's metadata (issue #238): every key
// with the guard_ prefix is the guard's record about the block — its verdict,
// the time of it, a person's guard-resolve trace, a repair mark. A client
// write cannot set, overwrite or remove one, and the guard list reads every
// value such a key can hold without failing.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestGuardKeys' -count=1 -v
package store_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gkVec is a 1024-dim vector with up to two components set.
func gkVec(aIdx int, aW float32, bIdx int, bW float32) []float32 {
	v := make([]float32, 1024)
	v[aIdx] = aW
	if bW != 0 {
		v[bIdx] = bW
	}
	return v
}

// gkFlaggedPairs writes n pairs of blocks at cosine 0.96 on disjoint axes
// through the production write path (store.UpsertBlock, store.StoreEmbedding)
// and lets the guard flag them (guard.RunGuardBatch): 2n needs_review blocks.
func gkFlaggedPairs(t *testing.T, pool *pgxpool.Pool, scope, prefix string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, 2*n)
	for i := 0; i < n; i++ {
		for j, vec := range [][]float32{gkVec(2*i, 1.0, 0, 0), gkVec(2*i, 0.96, 2*i+1, 0.28)} {
			title := prefix + "-" + string(rune('a'+i)) + string(rune('0'+j))
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "guard keys body of "+title, nil,
				map[string]any{"topic": "guard keys"}, scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("upsert %s: %v", title, err)
			}
			if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-keys-model", vec); err != nil {
				t.Fatalf("embed %s: %v", title, err)
			}
			ids = append(ids, b.ID)
		}
	}
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 4*n+10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400} /* the guard.* defaults */)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed < len(ids) {
		t.Fatalf("RunGuardBatch processed %d blocks, want at least %d", processed, len(ids))
	}
	for _, id := range ids {
		var status string
		if err := pool.QueryRow(ctx, `SELECT guard_status FROM context_blocks WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != "needs_review" {
			t.Fatalf("block %s: guard_status %q, want needs_review (the fixture did not reach the flagged state)", id, status)
		}
	}
	return ids
}

// TestGuardKeysCallerSimilarityKeepsTheGuardList is the probe of issue #238
// §5: a key that can write ONE flagged block of a scope writes
// guard_similarity:"x" through store.UpdateBlock (the call manage update, the
// MCP update tool and the confirm of a staged update end in), and then every
// reader of the scope asks for the guard list. The list answers, every flagged
// block of the scope is on it, and the written block carries the guard's
// similarity, not the caller's.
func TestGuardKeysCallerSimilarityKeepsTheGuardList(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "gk-probe"
	ids := gkFlaggedPairs(t, pool, scope, "gk-probe", 2)

	var guardSim string
	if err := pool.QueryRow(ctx, `SELECT metadata->>'guard_similarity' FROM context_blocks WHERE id = $1`, ids[0]).Scan(&guardSim); err != nil {
		t.Fatalf("read similarity: %v", err)
	}
	if _, _, err := store.UpdateBlock(ctx, pool, ids[0],
		store.UpdateBlockData{Metadata: map[string]any{"topic": "rewritten", "guard_similarity": "x", "guardrail": "kept", "guard": "kept"}},
		[]string{scope}); err != nil {
		t.Fatalf("UpdateBlock: %v", err)
	}
	// Only the guard_ prefix is the guard's: a caller's key that merely
	// starts with the word lands.
	var topic, guardrail, guardKey string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(metadata->>'topic', ''), COALESCE(metadata->>'guardrail', ''), COALESCE(metadata->>'guard', '') FROM context_blocks WHERE id = $1`,
		ids[0]).Scan(&topic, &guardrail, &guardKey); err != nil {
		t.Fatalf("read caller keys: %v", err)
	}
	if topic != "rewritten" || guardrail != "kept" || guardKey != "kept" {
		t.Errorf("caller keys after the write: topic %q, guardrail %q, guard %q; want rewritten, kept, kept", topic, guardrail, guardKey)
	}

	for _, status := range []string{"", "needs_review"} {
		items, err := store.GuardList(ctx, pool, []string{scope}, "", status, nil, 50)
		if err != nil {
			t.Fatalf("GuardList(status %q) failed for every reader of the scope after one caller wrote guard_similarity:\"x\": %v", status, err)
		}
		if len(items) != len(ids) {
			t.Fatalf("GuardList(status %q): %d items, want %d", status, len(items), len(ids))
		}
		for _, it := range items {
			if it.ID != ids[0] {
				continue
			}
			if it.Similarity == nil || *it.Similarity != guardSim {
				t.Errorf("GuardList(status %q): the written block's similarity = %v, want the guard's %q", status, it.Similarity, guardSim)
			}
		}
	}
}

// TestGuardKeysGuardListReadsAnyStoredValue: a database written before the
// write path kept the guard's keys to the guard holds whatever its callers
// sent in guard_similarity. The guard list reads every such value: a number
// (or a text the cast reads as one, as the list read it before) orders the
// list, highest first, and every other value sorts after it like a missing
// one. The values are written with SQL because no write path produces them
// any more; the blocks themselves are flagged by the guard.
func TestGuardKeysGuardListReadsAnyStoredValue(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "gk-legacy"
	ids := gkFlaggedPairs(t, pool, scope, "gk-legacy", 6)

	// One value per flagged block; the first and the third are JSON numbers,
	// the only values the list sorts by (issue #536: a JSON string such as
	// "0.97" is not the guard's float8 and sorts last, like the rest).
	values := []string{
		`0.99`, `"0.97"`, `1e-3`, `"-5"`,
		`"x"`, `""`, `{"v": 0.95}`, `[0.95]`, `true`, `null`, `"0.9x"`, `"  "`,
	}
	if len(values) != len(ids) {
		t.Fatalf("fixture: %d values for %d blocks", len(values), len(ids))
	}
	for i, id := range ids {
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET metadata = jsonb_set(metadata, '{guard_similarity}', $2::jsonb) WHERE id = $1`,
			id, values[i]); err != nil {
			t.Fatalf("write legacy value %s: %v", values[i], err)
		}
	}

	items, err := store.GuardList(ctx, pool, []string{scope}, "", "", nil, 50)
	if err != nil {
		t.Fatalf("GuardList: %v", err)
	}
	if len(items) != len(ids) {
		t.Fatalf("GuardList: %d items, want %d", len(items), len(ids))
	}
	wantHead := []string{ids[0], ids[2]} // 0.99, 0.001
	for i, want := range wantHead {
		if items[i].ID != want {
			got := make([]string, len(items))
			for j, it := range items {
				got[j] = it.ID
			}
			t.Fatalf("GuardList order: position %d is %s, want %s (the numbers first, highest first)\n got %v\nwant head %v",
				i, items[i].ID, want, got, wantHead)
		}
	}

	// The same values on blocks the guard found clean, through the one filter
	// that lists them.
	var cleanIDs []string
	for _, title := range []string{"gk-clean-a", "gk-clean-b"} {
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, "a lone block "+title, nil, nil,
			scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		cleanIDs = append(cleanIDs, b.ID)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET metadata = metadata || '{"guard_similarity": "x"}'::jsonb WHERE id = ANY($1::uuid[])`,
		cleanIDs); err != nil {
		t.Fatalf("write legacy value on unflagged blocks: %v", err)
	}
	active, err := store.GuardList(ctx, pool, []string{scope}, "", "active", nil, 50)
	if err != nil {
		t.Fatalf("GuardList(status active): %v", err)
	}
	if len(active) != len(cleanIDs) {
		t.Fatalf("GuardList(status active): %d items, want %d", len(active), len(cleanIDs))
	}
}

// TestGuardKeysGuardListReportsAFault: a fault that ends the list's statement
// while its rows are read reaches the caller as an error. Before, the list
// dropped it and answered with no rows (issue #238 §5: a caller's
// guard_similarity:"x" failed the cast and every reader of the scope saw an
// empty queue). The fault is injected with a function of a schema that the
// session's search path puts ahead of pg_catalog: it shadows
// pg_input_is_valid and raises for one value.
func TestGuardKeysGuardListReportsAFault(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "gk-fault"
	ids := gkFlaggedPairs(t, pool, scope, "gk-fault", 1)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET metadata = jsonb_set(metadata, '{guard_similarity}', '"boom"') WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("write value: %v", err)
	}
	for _, st := range []string{
		`CREATE SCHEMA ctx_fault`,
		`CREATE FUNCTION ctx_fault.pg_input_is_valid(v text, t text) RETURNS boolean LANGUAGE plpgsql AS $$
		 BEGIN
		   IF v = 'boom' THEN RAISE EXCEPTION 'injected fault'; END IF;
		   RETURN pg_catalog.pg_input_is_valid(v, t);
		 END $$`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("fault: %v", err)
		}
	}
	faulty := testdb.WithSessionSettings(t, pool, map[string]string{"search_path": "ctx_fault, public, pg_catalog"})
	items, err := store.GuardList(ctx, faulty, []string{scope}, "", "", nil, 50)
	if err == nil {
		t.Fatalf("GuardList answered %d items and no error for a statement that failed", len(items))
	}
	if items != nil {
		t.Errorf("GuardList answered %d items beside the error %v", len(items), err)
	}
	// Without the fault the same list answers.
	if items, err := store.GuardList(ctx, pool, []string{scope}, "", "", nil, 50); err != nil || len(items) != len(ids) {
		t.Fatalf("GuardList without the fault: %d items, error %v; want %d and none", len(items), err, len(ids))
	}
}

// TestGuardKeysCallerCannotMoveTheQueue: the other three things one write to
// one block could do to the readers of its scope before (issue #238 §5),
// through store.UpdateBlock with a replacement document and with a merge:
//   - order: guard_similarity 99 put the block at the head of the guard list;
//   - age: a guard_checked_at between the block's creation and now set the
//     time the queue reports the block as flagged since;
//   - queue: a guard_checked_at on a block the guard has not checked yet took
//     it out of the guard's queue for good (guardPendingWhere).
func TestGuardKeysCallerCannotMoveTheQueue(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "gk-queue"
	ids := gkFlaggedPairs(t, pool, scope, "gk-queue", 2)
	read := func(id string) (sim, checked string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(metadata->>'guard_similarity', ''), COALESCE(metadata->>'guard_checked_at', '') FROM context_blocks WHERE id = $1`,
			id).Scan(&sim, &checked); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return sim, checked
	}
	sim1, checked1 := read(ids[1])
	sim2, checked2 := read(ids[2])
	var created string
	if err := pool.QueryRow(ctx,
		`SELECT to_char(date_trunc('second', created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM context_blocks WHERE id = $1`,
		ids[1]).Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if _, _, err := store.UpdateBlock(ctx, pool, ids[1], store.UpdateBlockData{
		Metadata: map[string]any{"topic": "replaced", "guard_similarity": 99, "guard_checked_at": created},
	}, []string{scope}); err != nil {
		t.Fatalf("UpdateBlock (replace): %v", err)
	}
	if _, _, err := store.UpdateBlock(ctx, pool, ids[2], store.UpdateBlockData{
		MetadataMerge: map[string]any{"topic": "merged", "guard_similarity": 98, "guard_checked_at": created},
	}, []string{scope}); err != nil {
		t.Fatalf("UpdateBlock (merge): %v", err)
	}
	for _, c := range []struct{ id, sim, checked, how string }{
		{ids[1], sim1, checked1, "replace"}, {ids[2], sim2, checked2, "merge"},
	} {
		sim, checked := read(c.id)
		if sim != c.sim {
			t.Errorf("%s: guard_similarity %q after the write, want the guard's %q", c.how, sim, c.sim)
		}
		if checked != c.checked {
			t.Errorf("%s: guard_checked_at %q after the write, want the guard's %q", c.how, checked, c.checked)
		}
	}
	items, err := store.GuardList(ctx, pool, []string{scope}, "", "", nil, 50)
	if err != nil || len(items) != len(ids) {
		t.Fatalf("GuardList: %d items, error %v; want %d", len(items), err, len(ids))
	}
	guardSims := map[string]bool{sim1: true, sim2: true}
	for _, it := range items {
		if it.Similarity == nil || !guardSims[*it.Similarity] {
			got := "<null>"
			if it.Similarity != nil {
				got = *it.Similarity
			}
			t.Errorf("GuardList: block %s carries similarity %s, not one the guard wrote (%q, %q)", it.ID, got, sim1, sim2)
		}
	}

	// A block the guard has not checked yet stays in its queue.
	b, err := store.UpsertBlock(ctx, pool, "learnings", "gk-queue-unchecked", "a block the guard has not seen", nil, nil,
		scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-keys-model", gkVec(100, 1.0, 0, 0)); err != nil {
		t.Fatalf("embed: %v", err)
	}
	checkTypes := blocktype.NewRegistry().Snapshot().GuardCheckTypes()
	pending := func() int {
		t.Helper()
		n, _, err := guard.PendingStats(ctx, pool, []string{scope}, checkTypes)
		if err != nil {
			t.Fatalf("PendingStats: %v", err)
		}
		return n
	}
	if n := pending(); n != 1 {
		t.Fatalf("pending before the write: %d, want 1 (the fixture is not in the guard's queue)", n)
	}
	for _, data := range []store.UpdateBlockData{
		{Metadata: map[string]any{"guard_checked_at": created}},
		{MetadataMerge: map[string]any{"guard_checked_at": created}},
	} {
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, data, []string{scope}); err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		if n := pending(); n != 1 {
			t.Errorf("pending after a caller wrote guard_checked_at (%+v): %d, want 1 — the block left the guard's queue", data, n)
		}
	}
}

// TestGuardKeysChunkRewriteKeepsTheTrace: a chunk the guard never checks
// (lifecycle chunk) can still carry a guard key — a person's guard-resolve
// trace, which the single-id resolve writes on any live block. Rewriting the
// chunk's slot (store.InsertChunk, the ingest chunk path) replaces the
// metadata document and keeps that trace, and the caller's own guard keys do
// not land. The source row is written with SQL (no store writer of it).
func TestGuardKeysChunkRewriteKeepsTheTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "gk-chunk"
	var sourceID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/gk/chunk.md', 'gk', $1) RETURNING id::text`,
		scope).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	b, err := store.InsertChunk(ctx, pool, sourceID, 0, "learnings", "gk-chunk-0", "chunk body one", nil,
		map[string]any{"topic": "first"}, scope)
	if err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}
	if _, err := store.GuardResolve(ctx, pool, b.ID, "keep", []string{scope}); err != nil {
		t.Fatalf("GuardResolve keep: %v", err)
	}
	trace := func() (resolution, resolvedAt, topic string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(metadata->>'guard_resolution', ''), COALESCE(metadata->>'guard_resolved_at', ''), COALESCE(metadata->>'topic', '')
			   FROM context_blocks WHERE id = $1`, b.ID).Scan(&resolution, &resolvedAt, &topic); err != nil {
			t.Fatalf("read: %v", err)
		}
		return
	}
	res0, at0, _ := trace()
	if res0 != "keep" || at0 == "" {
		t.Fatalf("after guard-resolve keep: guard_resolution %q, guard_resolved_at %q (the fixture carries no trace)", res0, at0)
	}
	if _, err := store.InsertChunk(ctx, pool, sourceID, 0, "learnings", "gk-chunk-0", "chunk body two", nil,
		map[string]any{"topic": "second", "guard_resolution": "archive", "guard_resolved_at": "1999-01-01"}, scope); err != nil {
		t.Fatalf("InsertChunk rewrite: %v", err)
	}
	res, at, topic := trace()
	if res != res0 || at != at0 {
		t.Errorf("after the rewrite: guard_resolution %q, guard_resolved_at %q; want the trace %q, %q", res, at, res0, at0)
	}
	if topic != "second" {
		t.Errorf("after the rewrite: topic %q, want the caller's %q", topic, "second")
	}
}

// TestGuardKeysGuardListReadsTheSimilarityOncePerRow: the guard list sorts
// every flagged row of the read scopes by its similarity, so the order key is
// read once per row there, not only for the rows the list returns. Guarding
// the cast (guardListOrderSQL) must not read the key twice: with metadata
// stored out of line, every read is a TOAST fetch per row. The pin compares
// the buffers of the list's own statement with those of the same statement
// ordered by the bare cast the list ran before (one read), on blocks the guard
// flagged through the production path, at both deployment values of
// random_page_cost. A second read costs one TOAST fetch per row (four buffers
// per row here); the slack is one buffer per row.
func TestGuardKeysGuardListReadsTheSimilarityOncePerRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope, pairs = "gk-toast", 20
	for i := 0; i < pairs; i++ {
		for j, vec := range [][]float32{gkVec(2*i, 1.0, 0, 0), gkVec(2*i, 0.96, 2*i+1, 0.28)} {
			raw := make([]byte, 6000)
			if _, err := rand.Read(raw); err != nil {
				t.Fatalf("random: %v", err)
			}
			title := "gk-toast-" + string(rune('a'+i)) + string(rune('0'+j))
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "toasted guard list body "+title, nil,
				map[string]any{"blob": base64.StdEncoding.EncodeToString(raw)}, scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("upsert %s: %v", title, err)
			}
			if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-keys-model", vec); err != nil {
				t.Fatalf("embed %s: %v", title, err)
			}
		}
	}
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 4*pairs+10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400} /* the guard.* defaults */)
	if err != nil || processed != 2*pairs {
		t.Fatalf("RunGuardBatch: processed %d, error %v; want %d", processed, err, 2*pairs)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	query, args := store.GuardListQueryForTest([]string{scope}, "", "", nil, 10)
	if strings.Count(query, store.GuardListOrderSQLForTest) != 1 {
		t.Fatalf("the list's statement does not carry its order key once:\n%s", query)
	}
	bare := strings.Replace(query, store.GuardListOrderSQLForTest, `(b.metadata->>'guard_similarity')::numeric`, 1)
	rowsOutOfLine := int64(2 * pairs)
	for _, rpc := range testdb.RandomPageCosts {
		p := testdb.WithRandomPageCost(t, pool, rpc)
		// One run of each first: the first execution on a connection also
		// reads the catalog for the functions it calls.
		testdb.ExplainCustom(t, p, query, args...)
		testdb.ExplainCustom(t, p, bare, args...)
		bList := testdb.ExplainCustom(t, p, query, args...).Buffers
		bBare := testdb.ExplainCustom(t, p, bare, args...).Buffers
		t.Logf("random_page_cost %s: buffers list %d, bare cast %d (%d rows out of line)", rpc, bList, bBare, rowsOutOfLine)
		if bList > bBare+rowsOutOfLine {
			t.Errorf("random_page_cost %s: the guard list reads %d buffers, the bare cast %d: the similarity is read more than once per row",
				rpc, bList, bBare)
		}
	}
}
