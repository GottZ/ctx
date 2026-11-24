//go:build integration

// Issue #211, update items of store.UpsertBlocks: the same writes as single
// UpdateBlock calls (the difference probe), what they cost against those
// calls, the lock of the scope a move goes to, and an interrupted batch.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestUpsertBlocksUpdate' -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func ubuPtr(s string) *string { return &s }

func ubuUpdate(id string, data store.UpdateBlockData, scopes ...string) store.BatchUpsert {
	return store.BatchUpsert{Update: &store.BatchUpdate{ID: id, Data: data, WriteScopes: scopes}}
}

// ubuWorld is one database seeded with the difference probe's blocks, each
// through the production write path; ids by title.
func ubuWorld(t *testing.T) (*pgxpool.Pool, map[string]string) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	ids := map[string]string{}
	for _, title := range []string{"a", "c", "t", "same"} {
		ids[title] = ubSeed(t, pool, "ubd-s1", title)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = '{"source":"seed","guard_status":"needs_review","guard_checked_at":"2026-01-01"}'::jsonb
		WHERE id = $1`, ids["a"]); err != nil {
		t.Fatal(err)
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	b, err := store.UpsertBlockSupersedes(ctx, pool, "batch", "b", "b body", nil, nil, "ubd-s1", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: ids["t"], APIKeyID: "ubd", Types: reg.Snapshot()})
	if err != nil {
		t.Fatalf("declare b supersedes t: %v", err)
	}
	ids["b"] = b.ID
	if err := pgxWriteFn(ctx, pool, func(tx pgx.Tx) error {
		return store.PutStructuralLink(ctx, tx, store.StructuralLink{SourceID: ids["a"], TargetID: ids["c"], LinkClass: "references"}, []string{"ubd-s1"})
	}); err != nil {
		t.Fatalf("structural link: %v", err)
	}
	return pool, ids
}

func pgxWriteFn(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ubuSnapshot is every block (all columns but ids and times, the uuid
// references as titles), every dream and structural link (endpoints as
// titles) and the vector backfill queue, in one JSON document.
func ubuSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var doc string
	if err := pool.QueryRow(context.Background(), `
		WITH dropped AS (
		  SELECT array_agg(column_name::text) AS cols FROM information_schema.columns
		   WHERE table_name = 'context_blocks' AND table_schema = current_schema()
		     AND (data_type IN ('uuid', 'timestamp with time zone', 'timestamp without time zone') OR column_name = 'content_times'))
		SELECT jsonb_build_object(
		  'blocks', (SELECT jsonb_agg((to_jsonb(b) - d.cols) || jsonb_build_object(
		       'superseded_by', (SELECT x.title FROM context_blocks x WHERE x.id = b.superseded_by),
		       'parent', (SELECT x.title FROM context_blocks x WHERE x.id = b.parent_id),
		       'metadata', COALESCE(b.metadata, '{}'::jsonb) - 'sensitivity_audit',
		       'content_times', b.content_times IS NOT NULL)
		     ORDER BY b.title, b.scope) FROM context_blocks b, dropped d),
		  'dream_links', (SELECT jsonb_agg(jsonb_build_object('s', s.title, 't', g.title, 'rel', l.relationship, 'scope', l.scope)
		     ORDER BY s.title, g.title) FROM context_dream_links l
		     JOIN context_blocks s ON s.id = l.source_block_id JOIN context_blocks g ON g.id = l.target_block_id),
		  'structural_links', (SELECT jsonb_agg(jsonb_build_object('s', s.title, 't', g.title, 'class', l.link_class, 'scope', l.scope)
		     ORDER BY s.title, g.title) FROM context_structural_links l
		     JOIN context_blocks s ON s.id = l.source_block_id JOIN context_blocks g ON g.id = l.target_block_id))::text`,
	).Scan(&doc); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var pretty map[string]any
	_ = json.Unmarshal([]byte(doc), &pretty)
	out, _ := json.MarshalIndent(pretty, "", " ")
	return string(out)
}

// TestUpsertBlocksUpdateItemsMatchSingleUpdates is the difference probe: one
// sequence of updates — edits with a metadata merge that names a guard key,
// a scope move that sweeps a supersedes link and reconciles its target, a
// rename with tags and a type, an update that changes nothing — applied once
// as single UpdateBlock calls and once as one batch, each to its own database
// seeded alike. The end state is the same: every column of every block but
// ids and times, the supersedes state, the dream and structural links and the
// vector queue.
func TestUpsertBlocksUpdateItemsMatchSingleUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	scopes := []string{"ubd-s1", "ubd-s2"}
	updates := func(ids map[string]string) []store.BatchUpsert {
		repl := "ALPHA"
		return []store.BatchUpsert{
			ubuUpdate(ids["a"], store.UpdateBlockData{
				Edits:         []store.ContentEdit{{Old: "a body", New: &repl}},
				MetadataMerge: map[string]any{"k": "v", "source": nil, "guard_status": "kept"},
			}, scopes...),
			ubuUpdate(ids["b"], store.UpdateBlockData{Scope: ubuPtr("ubd-s2")}, scopes...),
			ubuUpdate(ids["c"], store.UpdateBlockData{Title: ubuPtr("c renamed"), Tags: []string{"z"}, Type: ubuPtr("reference")}, scopes...),
			ubuUpdate(ids["same"], store.UpdateBlockData{Content: ubuPtr("same body")}, scopes...),
		}
	}

	singles, singleIDs := ubuWorld(t)
	for _, u := range updates(singleIDs) {
		if _, _, err := store.UpdateBlock(ctx, singles, u.Update.ID, u.Update.Data, u.Update.WriteScopes); err != nil {
			t.Fatalf("single update %s: %v", u.Update.ID, err)
		}
	}
	batched, batchIDs := ubuWorld(t)
	results, err := store.UpsertBlocks(ctx, batched, updates(batchIDs), "ubd", nil, nil)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if results[3].Block != nil || !results[3].Match.Unchanged() || results[3].Match.ID != batchIDs["same"] {
		t.Errorf("the update that changes nothing: %+v; want it left as it was", results[3])
	}
	for i := 0; i < 3; i++ {
		if results[i].Block == nil {
			t.Errorf("items[%d] not written", i)
		}
	}
	if got, want := ubuSnapshot(t, batched), ubuSnapshot(t, singles); got != want {
		t.Fatalf("the batch left another state than the single updates:\nbatch:\n%s\nsingles:\n%s", got, want)
	}
	// The probe must be able to fail: the sweep and the reconcile ran.
	snap := ubuSnapshot(t, batched)
	if !strings.Contains(snap, `"scope": "ubd-s2"`) || strings.Contains(snap, `"rel": "supersedes"`) {
		t.Fatalf("the move did not happen as the probe expects:\n%s", snap)
	}
}

// TestUpsertBlocksUpdateItemCost measures a batch of 100 update items against
// the same 100 updates as single UpdateBlock calls: statements sent and
// shared-buffer accesses of the backend that ran them, and wall time. The
// assertion is the issue's bound: the batch sends no more statements per item
// than the single update.
func TestUpsertBlocksUpdateItemCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	const n = 100
	repl := "EDITED"
	for _, shape := range []struct {
		name string
		data func(i int) store.UpdateBlockData
	}{
		{"a new content", func(i int) store.UpdateBlockData { return store.UpdateBlockData{Content: ubuPtr(fmt.Sprintf("new content %d", i))} }},
		{"tags", func(int) store.UpdateBlockData { return store.UpdateBlockData{Tags: []string{"x", "y"}} }},
		{"edits", func(int) store.UpdateBlockData {
			return store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "body", New: &repl}}}
		}},
		{"a metadata merge", func(i int) store.UpdateBlockData { return store.UpdateBlockData{MetadataMerge: map[string]any{"n": i}} }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			prefix := strings.ReplaceAll(shape.name, " ", "-")
			batch := make([]store.BatchUpsert, n)
			singles := make([]string, n)
			for i := 0; i < n; i++ {
				batch[i] = ubuUpdate(ubSeed(t, pool, "ubu-cost", fmt.Sprintf("%s-b-%03d", prefix, i)), shape.data(i), "ubu-cost")
				singles[i] = ubSeed(t, pool, "ubu-cost", fmt.Sprintf("%s-s-%03d", prefix, i))
			}
			var batchCount, singleCount ubCounter
			batchPool := ubActor(t, dsn, "ubu-cost-batch", &batchCount)
			singlePool := ubActor(t, dsn, "ubu-cost-single", &singleCount)

			hits := ubBufferAccesses(t, batchPool)
			start := time.Now()
			results, err := store.UpsertBlocks(ctx, batchPool, batch, "ubu", nil, nil)
			if err != nil {
				t.Fatalf("batch: %v", err)
			}
			batchTime := time.Since(start)
			batchHits := ubBufferAccesses(t, batchPool) - hits
			batchStmts := batchCount.n.Load() - 4
			for i, r := range results {
				if r.Block == nil {
					t.Fatalf("items[%d] not written", i)
				}
			}

			hits = ubBufferAccesses(t, singlePool)
			start = time.Now()
			for i, id := range singles {
				if _, _, err := store.UpdateBlock(ctx, singlePool, id, shape.data(i), []string{"ubu-cost"}); err != nil {
					t.Fatalf("single: %v", err)
				}
			}
			singleTime := time.Since(start)
			singleHits := ubBufferAccesses(t, singlePool) - hits
			singleStmts := singleCount.n.Load() - 4

			t.Logf("batch: %v (%v per item), %d statements (%.2f per item), %d buffer accesses (%.1f per item) | "+
				"singles: %v (%v per item), %d statements (%.2f per item), %d buffer accesses (%.1f per item)",
				batchTime, batchTime/n, batchStmts, float64(batchStmts)/n, batchHits, float64(batchHits)/n,
				singleTime, singleTime/n, singleStmts, float64(singleStmts)/n, singleHits, float64(singleHits)/n)
			if batchStmts > singleStmts {
				t.Errorf("the batch sent %d statements for %d items, the single updates %d; want no more per item", batchStmts, n, singleStmts)
			}
		})
	}
}

// TestUpsertBlocksUpdateMoveLocksTheTargetScope: an update item that moves a
// block takes the supersedes lock of the scope it moves to as well, in sorted
// order with the others, before any row lock. While another transaction holds
// that scope's lock the batch waits — holding no block row: a third
// transaction locks the moved block at once.
func TestUpsertBlocksUpdateMoveLocksTheTargetScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	a := ubSeed(t, pool, "ubm-a", "moved")
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ctx:supersedes:' || 'ubm-b'))`); err != nil {
		t.Fatal(err)
	}
	actor := ubActor(t, dsn, "ubm-batch", nil)
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlocks(ctx, actor, []store.BatchUpsert{
			ubuUpdate(a, store.UpdateBlockData{Scope: ubuPtr("ubm-b")}, "ubm-a", "ubm-b"),
		}, "ubm", nil, nil)
		done <- err
	}()
	ubWaitForLockWait(t, pool, "ubm-batch")
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, a); err != nil {
		t.Errorf("the waiting batch holds the moved block: %v", err)
	}
	_ = probe.Rollback(ctx)
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ubAwait(t, "ubm-batch", done); err != nil {
		t.Fatalf("batch: %v", err)
	}
	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM context_blocks WHERE id = $1`, a).Scan(&scope); err != nil || scope != "ubm-b" {
		t.Fatalf("scope %q (%v), want ubm-b", scope, err)
	}
}

// TestUpsertBlocksUpdateInterrupted is the interruption the issue describes:
// the client goes away in the middle of a batch — after its update items are
// written, while it waits to book them — by cancelling its context, or by the
// connection ending. The store holds nothing of the batch: no item, no
// booking; never a prefix.
func TestUpsertBlocksUpdateInterrupted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "ubi", "ubi", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	for _, how := range []string{"context cancelled", "connection terminated"} {
		t.Run(how, func(t *testing.T) {
			prefix := strings.ReplaceAll(how, " ", "-")
			a := ubSeed(t, pool, "ubi", prefix+"-a")
			b := ubSeed(t, pool, "ubi", prefix+"-b")
			// The booking's foreign key takes the key-share lock of the key
			// row: holding the row stops the batch after its writes.
			gate, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback(ctx) }()
			if _, err := gate.Exec(ctx, `SELECT 1 FROM context_api_keys WHERE id = $1 FOR UPDATE`, key.ID); err != nil {
				t.Fatal(err)
			}
			actor := ubActor(t, dsn, "ubi-"+prefix, nil)
			bctx, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := store.UpsertBlocks(bctx, actor, []store.BatchUpsert{
					ubuUpdate(a, store.UpdateBlockData{Content: ubuPtr("a rewritten")}, "ubi"),
					ubuUpdate(b, store.UpdateBlockData{Content: ubuPtr("b rewritten")}, "ubi"),
					{Category: "batch", Title: prefix + "-new", Content: "new", Scope: "ubi"},
				}, key.ID, nil, &store.WriteBudget{APIKeyID: key.ID, Limit: 0})
				done <- err
			}()
			ubWaitForLockWait(t, pool, "ubi-"+prefix)
			// The batch waits for the booking: its writes are made.
			var pending int
			if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE a.application_name = $1 AND l.locktype = 'transactionid' AND l.granted`, "ubi-"+prefix).Scan(&pending); err != nil || pending == 0 {
				t.Fatalf("the batch holds no transaction id (%d, %v): it has not written yet", pending, err)
			}
			if how == "context cancelled" {
				cancel()
			} else if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`,
				"ubi-"+prefix); err != nil {
				t.Fatal(err)
			}
			if err := ubAwait(t, "ubi-"+prefix, done); err == nil {
				t.Fatalf("the interrupted batch reported success")
			}
			_ = gate.Rollback(ctx)
			var aContent, bContent string
			var written, booked int
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT content FROM context_blocks WHERE id = $1),
				(SELECT content FROM context_blocks WHERE id = $2),
				(SELECT count(*) FROM context_blocks WHERE title = $3)::int,
				(SELECT count(*) FROM context_access_log WHERE api_key_id = $4)::int`,
				a, b, prefix+"-new", key.ID).Scan(&aContent, &bContent, &written, &booked); err != nil {
				t.Fatal(err)
			}
			if aContent != prefix+"-a body" || bContent != prefix+"-b body" || written != 0 || booked != 0 {
				t.Fatalf("after the interruption: a %q, b %q, new %d, booked %d; want nothing of the batch", aContent, bContent, written, booked)
			}
		})
	}
}

// TestUpsertBlocksUpdateFollowsAMovedBlock: an update item's block is moved to
// another scope after the batch read its scope and before the batch's lock
// pass reached it (the batch waits for the first scope's lock meanwhile). The
// lock pass does not find the block in the scope it locked, and the batch runs
// again: it reads the new scope, locks it in order and writes the block there.
func TestUpsertBlocksUpdateFollowsAMovedBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	x := ubSeed(t, pool, "ubf-s1", "moving")
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('ctx:supersedes:' || 'ubf-s1'))`); err != nil {
		t.Fatal(err)
	}
	actor := ubActor(t, dsn, "ubf-batch", nil)
	done := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlocks(ctx, actor, []store.BatchUpsert{
			ubuUpdate(x, store.UpdateBlockData{Content: ubuPtr("written after the move")}, "ubf-s1", "ubf-s2"),
		}, "ubf", nil, nil)
		done <- err
	}()
	ubWaitForLockWait(t, pool, "ubf-batch")
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET scope = 'ubf-s2' WHERE id = $1`, x); err != nil {
		t.Fatal(err)
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ubAwait(t, "ubf-batch", done); err != nil {
		t.Fatalf("batch: %v", err)
	}
	var scope, content string
	if err := pool.QueryRow(ctx, `SELECT scope, content FROM context_blocks WHERE id = $1`, x).Scan(&scope, &content); err != nil {
		t.Fatal(err)
	}
	if scope != "ubf-s2" || content != "written after the move" {
		t.Fatalf("block in %s with %q; want the batch's content in ubf-s2", scope, content)
	}
}
