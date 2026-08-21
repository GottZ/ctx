//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-1 (Re-Embed-
// Migration "Provenienz-Reparatur"). RED premise this file used to prove
// (captured in the welle's build log, no longer expressible post-fix since
// the OLD 4-arg StoreEmbedding signature no longer compiles): pre-109, the
// embed_model column carried the DDL default 'qwen3-embedding:8b'
// (001_initial.sql:34) for EVERY block regardless of which model actually
// produced the stored vector — StoreEmbedding never wrote embed_model at
// all, so the DDL default silently masqueraded as provenance.
//
// Post-fix (migration 109 + StoreEmbedding(model) + ClearEmbedding(embed_model
// NULL) + the scheduler/query call-site updates) this file proves:
//   - writer-attributed provenance on StoreEmbedding, NULL-out on ClearEmbedding;
//   - the fail-closed empty-model guard (no DB round-trip);
//   - migration 109's backfill convergence, both for a fresh-DB write path and
//     for a simulated pre-existing row re-running the idempotent backfill;
//   - the pending-peek query uses idx_embedding_pending;
//   - embed_status/idx_embed_pending are gone, idx_embedding_pending exists.
//
// Run: go test -tags=integration ./internal/store/ -run TestEmbedProvenance -count=1 -v
package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

func TestEmbedProvenance_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	vec := func(fill float32) []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = fill
		}
		return v
	}

	// (1) Provenienz: StoreEmbedding writes the model that produced the
	// vector; ClearEmbedding nulls both together; model="" is fail-closed
	// with NO db write (a vector without provenance is worse than none).
	t.Run("provenance_clear_and_failclosed", func(t *testing.T) {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', 'w04-1-provenance', 'body', 'w04-1') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatalf("seed block: %v", err)
		}

		if err := store.StoreEmbedding(ctx, pool, id, "served-model-x", vec(0.5)); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		var gotModel string
		if err := pool.QueryRow(ctx,
			`SELECT embed_model FROM context_blocks WHERE id = $1::uuid`, id).Scan(&gotModel); err != nil {
			t.Fatalf("read embed_model: %v", err)
		}
		if gotModel != "served-model-x" {
			t.Fatalf("embed_model = %q, want %q (writer-attributed provenance)", gotModel, "served-model-x")
		}

		if err := store.ClearEmbedding(ctx, pool, id); err != nil {
			t.Fatalf("ClearEmbedding: %v", err)
		}
		var embNull, modelNull bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embed_model IS NULL FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&embNull, &modelNull); err != nil {
			t.Fatalf("read post-clear: %v", err)
		}
		if !embNull || !modelNull {
			t.Fatalf("post-clear: embedding_null=%v embed_model_null=%v, want both true", embNull, modelNull)
		}

		// Fail-closed: model=="" is rejected before any DB round-trip. Prove
		// "no round-trip" by asserting the row is untouched (still both NULL)
		// rather than merely checking the error string.
		if err := store.StoreEmbedding(ctx, pool, id, "", vec(0.9)); err == nil {
			t.Fatalf("StoreEmbedding with model=\"\" succeeded, want fail-closed error")
		}
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embed_model IS NULL FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&embNull, &modelNull); err != nil {
			t.Fatalf("read post-failclosed: %v", err)
		}
		if !embNull || !modelNull {
			t.Fatalf("post-failclosed: row was written despite model=\"\" (embedding_null=%v embed_model_null=%v)", embNull, modelNull)
		}
	})

	// (2) Konvergenz: a fresh-DB write via the production path (UpsertBlock +
	// StoreEmbedding) attributes the writer's model; a vector-less row stays
	// NULL. Separately, a row simulating pre-109 stock data (alt-default
	// embed_model with NO vector) converges to NULL when migration 109's
	// backfill UPDATEs are re-applied — proving the repair is idempotent, not
	// a one-shot fix tied to the original deploy moment.
	t.Run("convergence_freshdb_and_backfill_resim", func(t *testing.T) {
		withVec, err := store.UpsertBlock(ctx, pool, "learnings", "w04-1-conv-vec", "body", nil, nil, "w04-1-conv", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("upsert withVec: %v", err)
		}
		withoutVec, err := store.UpsertBlock(ctx, pool, "learnings", "w04-1-conv-novec", "body", nil, nil, "w04-1-conv", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("upsert withoutVec: %v", err)
		}

		if err := store.StoreEmbedding(ctx, pool, withVec.ID, "fresh-writer-model", vec(0.75)); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}

		var vecModel sql.NullString
		if err := pool.QueryRow(ctx, `SELECT embed_model FROM context_blocks WHERE id = $1::uuid`, withVec.ID).Scan(&vecModel); err != nil {
			t.Fatalf("read vec row model: %v", err)
		}
		if !vecModel.Valid || vecModel.String != "fresh-writer-model" {
			t.Errorf("vector row embed_model = %+v, want 'fresh-writer-model'", vecModel)
		}

		var novecModel sql.NullString
		if err := pool.QueryRow(ctx, `SELECT embed_model FROM context_blocks WHERE id = $1::uuid`, withoutVec.ID).Scan(&novecModel); err != nil {
			t.Fatalf("read vector-less row model: %v", err)
		}
		if novecModel.Valid {
			t.Errorf("vector-less row embed_model = %q, want NULL", novecModel.String)
		}

		// Bestands-Sim: force the vector-less row onto the pre-fix alt-default
		// — a row that would have survived from before 109's backfill ran.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET embed_model = 'qwen3-embedding:8b' WHERE id = $1`, withoutVec.ID); err != nil {
			t.Fatalf("simulate stale alt-default: %v", err)
		}
		var stale sql.NullString
		if err := pool.QueryRow(ctx, `SELECT embed_model FROM context_blocks WHERE id = $1::uuid`, withoutVec.ID).Scan(&stale); err != nil {
			t.Fatalf("read simulated stale: %v", err)
		}
		if !stale.Valid || stale.String != "qwen3-embedding:8b" {
			t.Fatalf("precondition: stale sim not in place, got %+v", stale)
		}

		// Re-apply the REAL embedded 109 file (no test-local SQL copy — the
		// M072/075 golden-test line) end-to-end; its steps are all
		// idempotent (IF EXISTS/IF NOT EXISTS, and a repeat DROP DEFAULT is
		// a no-op), so re-running it must converge the simulated stale row.
		sqlBytes, err := migrations.Section("109_embed_provenance.sql")
		if err != nil {
			t.Fatalf("read embedded 109: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("re-apply embedded 109 (idempotency): %v", err)
		}

		var reconverged sql.NullString
		if err := pool.QueryRow(ctx, `SELECT embed_model FROM context_blocks WHERE id = $1::uuid`, withoutVec.ID).Scan(&reconverged); err != nil {
			t.Fatalf("read reconverged: %v", err)
		}
		if reconverged.Valid {
			t.Errorf("after re-applying 109's backfill, embed_model = %q, want NULL (convergence)", reconverged.String)
		}
		// NOTE: the migration's UPDATE (2) is a deploy-TIME-POINT backfill —
		// it blanket-stamps EVERY row that currently has an embedding, by
		// design (see 109's comment: "Deploy-Zeitpunkt-Annahme"). It is not
		// meant to be idempotent against writer-attributed provenance
		// recorded AFTER the original deploy; re-running it here (as this
		// subtest deliberately does, to prove the NULL-embedding convergence
		// above) legitimately overwrites withVec's "fresh-writer-model" back
		// to the migration's backfill label. That is expected, not asserted
		// against here — this subtest's claim is scoped to the stale-row
		// convergence path only.
	})

	// (3) EXPLAIN-Gate: the pending-peek query (the shape both scheduler
	// backfill and query-path backfill use to pick unembedded blocks) is
	// USABLE via idx_embedding_pending. K3 note: at this test's mini-table
	// scale the planner legitimately prefers a Seq Scan on cost grounds —
	// enable_seqscan=off (session-local, documented) forces the plan to
	// prove the index is present and matches the predicate, not that the
	// planner picks it unprompted at trivial scale (that scale-dependent
	// claim is the W6 EXPLAIN-gate line, out of scope here).
	t.Run("pending_index_explain_gate", func(t *testing.T) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "SET enable_seqscan=off"); err != nil {
			t.Fatalf("set enable_seqscan=off: %v", err)
		}
		rows, err := conn.Query(ctx,
			`EXPLAIN (FORMAT TEXT) SELECT id FROM context_blocks
			 WHERE embedding IS NULL AND NOT is_archived
			 ORDER BY created_at LIMIT 10`)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan line: %v", err)
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("plan rows: %v", err)
		}
		t.Logf("pending-peek plan (enable_seqscan=off, K3 mini-table note):\n%s", plan.String())
		if !strings.Contains(plan.String(), "idx_embedding_pending") {
			t.Errorf("plan does not use idx_embedding_pending — index missing or predicate mismatch:\n%s", plan.String())
		}
	})

	// (4) Tote Objekte: embed_status + idx_embed_pending are gone,
	// idx_embedding_pending exists.
	t.Run("dead_objects_removed", func(t *testing.T) {
		var colCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'context_blocks' AND column_name = 'embed_status'`,
		).Scan(&colCount); err != nil {
			t.Fatalf("column probe: %v", err)
		}
		if colCount != 0 {
			t.Errorf("embed_status column still exists (want dropped)")
		}

		var deadIdx int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE tablename = 'context_blocks' AND indexname = 'idx_embed_pending'`,
		).Scan(&deadIdx); err != nil {
			t.Fatalf("dead index probe: %v", err)
		}
		if deadIdx != 0 {
			t.Errorf("idx_embed_pending still exists (want dropped)")
		}

		var liveIdx int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE tablename = 'context_blocks' AND indexname = 'idx_embedding_pending'`,
		).Scan(&liveIdx); err != nil {
			t.Fatalf("live index probe: %v", err)
		}
		if liveIdx != 1 {
			t.Errorf("idx_embedding_pending missing (want exactly 1)")
		}
	})
}
