//go:build integration

// Migration 121 gates (H-W14): the BESTAND repair for compaction checkpoint
// heads. M107 retyped + un-archived the "Compaction source …" corpus; the
// manifest heads ("Compaction checkpoint head …") only gained a classify rule
// in M120 (H-W13) — the rows already on disk stayed knowledge-typed and, where
// the guard archive lane had hit them, invisible to every NOT is_archived read
// path. M121 repairs them.
//
// The suite seeds every pre-121 case into ONE database, applies 121 exactly
// once and then reads each fixture back: the three guards of statement (b)
// only prove anything in each other's presence, and the single-statement
// UPDATE is precisely where a missing guard turns into a 23505 on the partial
// unique index (category,title,scope) WHERE NOT is_archived.
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 \
//	  go test -tags=integration ./internal/blocktype/ -run TestRepair_ -count=1 -v
package blocktype_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m121File = "121_checkpoint_head_repair.sql"

// repairState is the post-migration shape of one fixture row: everything M121
// may legitimately touch, plus the audit marker that proves WHICH statement
// touched it.
type repairState struct {
	typeName   string
	typeSource string
	archived   bool
	guard      string
	repairMark string // metadata->>'guard_repair'; "" when the key is absent
}

func (s repairState) String() string {
	return s.typeName + "/" + s.typeSource + "/archived=" +
		map[bool]string{true: "true", false: "false"}[s.archived] +
		"/" + s.guard + "/mark=" + s.repairMark
}

// seedBlock plants one pre-121 row. An empty id lets uuidv7() run; the twin
// case passes explicit ids because M121's rn=1 guard orders by id and the
// test must not depend on insert timing resolution.
func seedBlock(t *testing.T, pool *pgxpool.Pool, id, category, title, typeName, typeSource, guard string, archived bool) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks
		   (id, category, title, content, scope, type_name, type_source, is_archived, guard_status)
		 VALUES (COALESCE(NULLIF($1,'')::uuid, uuidv7()), $2, $3, 'evidence body', 'private', $4, $5, $6, $7)
		 RETURNING id::text`,
		id, category, title, typeName, typeSource, archived, guard).Scan(&out); err != nil {
		t.Fatalf("seed %q (%s): %v", title, typeName, err)
	}
	return out
}

func stateOf(t *testing.T, pool *pgxpool.Pool, id string) repairState {
	t.Helper()
	var s repairState
	if err := pool.QueryRow(context.Background(),
		`SELECT type_name, type_source, is_archived,
		        COALESCE(guard_status, ''), COALESCE(metadata->>'guard_repair', '')
		   FROM context_blocks WHERE id = $1::uuid`, id).
		Scan(&s.typeName, &s.typeSource, &s.archived, &s.guard, &s.repairMark); err != nil {
		t.Fatalf("read state of %s: %v", id, err)
	}
	return s
}

func wantState(t *testing.T, pool *pgxpool.Pool, label, id string, want repairState) {
	t.Helper()
	if got := stateOf(t, pool, id); got != want {
		t.Errorf("%s = %s, want %s", label, got, want)
	}
}

// needsReviewCount counts the WHOLE table, not just the checkpoint category:
// gate (e) asserts M121 never feeds the review queue, and a leak into any
// other category would be just as wrong.
func needsReviewCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_blocks WHERE guard_status = 'needs_review'`).Scan(&n); err != nil {
		t.Fatalf("count needs_review: %v", err)
	}
	return n
}

// categorySnapshot is the idempotency probe: every column M121 writes plus
// updated_at, so a second run that touched even one row cannot come back
// equal.
func categorySnapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text,
		        type_name || '|' || type_source || '|' || is_archived::text || '|' ||
		        COALESCE(guard_status, '<null>') || '|' ||
		        COALESCE(metadata->>'guard_repair', '<null>') || '|' ||
		        updated_at::text
		   FROM context_blocks
		  WHERE category = 'compaction-checkpoints'
		  ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()

	snap := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("scan snapshot row: %v", err)
		}
		snap[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return snap
}

// applyM121Again re-executes the EMBEDDED migration file in its own
// transaction. RunMigrationsUpTo cannot serve here — version 121 is recorded
// in _migrations after the first pass and would be skipped, which would test
// the runner's bookkeeping instead of the statement's idempotency.
func applyM121Again(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.FS.ReadFile(m121File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", m121File, err)
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-apply %s: %v", m121File, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit re-apply: %v", err)
	}
}

// TestRepair_M121_Corpus is the main gate. One container, one migration run,
// seven fixtures that each isolate one guard.
func TestRepair_M121_Corpus(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 120)

	const cat = "compaction-checkpoints"

	// (a) lone archived head — no live slot holder, nothing in its way.
	lone := seedBlock(t, pool, "", cat, "Compaction checkpoint head 20260701_lone", "knowledge", "auto", "archived_dup", true)

	// (b) archived head whose (category,title,scope) slot is already taken by
	// a live row. Dropping the NOT EXISTS guard makes this 23505 on the
	// partial unique index — which aborts the whole migration and, in prod,
	// the boot.
	const slotTitle = "Compaction checkpoint head 20260702_slot"
	slotLive := seedBlock(t, pool, "", cat, slotTitle, "knowledge", "auto", "active", false)
	slotArchived := seedBlock(t, pool, "", cat, slotTitle, "knowledge", "auto", "archived_dup", true)

	// (c) two archived title twins, no live holder. Explicit ids pin the
	// rn=1 order: …0c2 is the newer row and must be the one that returns.
	const twinTitle = "Compaction checkpoint head 20260703_twin"
	twinOld := seedBlock(t, pool, "01920000-0000-7000-8000-0000000000c1", cat, twinTitle, "knowledge", "auto", "archived_dup", true)
	twinNew := seedBlock(t, pool, "01920000-0000-7000-8000-0000000000c2", cat, twinTitle, "knowledge", "auto", "archived_dup", true)

	// (f) non-head row in the same category: M107 already typed it, the guard
	// archived a twin of it. The title predicate is all that keeps M121 off
	// it.
	nonHead := seedBlock(t, pool, "", cat, "Compaction source part 1/3", "checkpoint", "auto", "archived_dup", true)

	// (e) baseline for the review-queue invariant — a live needs_review row
	// outside the category, so the count is non-zero and the assertion is not
	// trivially 0 == 0.
	reviewBaseline := seedBlock(t, pool, "", "learnings", "guard review baseline", "knowledge", "auto", "needs_review", false)

	reviewBefore := needsReviewCount(t, pool)
	if reviewBefore == 0 {
		t.Fatalf("fixture broken: needs_review baseline is 0, the invariant would be vacuous")
	}

	if err := store.RunMigrationsUpTo(ctx, pool, 121); err != nil {
		t.Fatalf("apply migration 121: %v", err)
	}

	t.Run("a_lone_head_retyped_and_unarchived", func(t *testing.T) {
		wantState(t, pool, "lone head", lone, repairState{
			typeName: "checkpoint", typeSource: "auto", archived: false, guard: "active", repairMark: "M121",
		})
	})

	t.Run("b_live_slot_holder_blocks_unarchive", func(t *testing.T) {
		// The archived row is retyped by (a) — that part is unconditional —
		// but stays archived, and carries no repair marker.
		wantState(t, pool, "blocked head", slotArchived, repairState{
			typeName: "checkpoint", typeSource: "auto", archived: true, guard: "archived_dup", repairMark: "",
		})
		// The live holder is retyped too and otherwise untouched.
		wantState(t, pool, "live slot holder", slotLive, repairState{
			typeName: "checkpoint", typeSource: "auto", archived: false, guard: "active", repairMark: "",
		})
	})

	t.Run("c_only_newest_twin_returns", func(t *testing.T) {
		wantState(t, pool, "newer twin", twinNew, repairState{
			typeName: "checkpoint", typeSource: "auto", archived: false, guard: "active", repairMark: "M121",
		})
		wantState(t, pool, "older twin", twinOld, repairState{
			typeName: "checkpoint", typeSource: "auto", archived: true, guard: "archived_dup", repairMark: "",
		})
	})

	t.Run("e_review_queue_unchanged", func(t *testing.T) {
		if got := needsReviewCount(t, pool); got != reviewBefore {
			t.Errorf("needs_review count = %d, want %d — M121 must not feed the review queue (M107 did, M121 rules)", got, reviewBefore)
		}
		wantState(t, pool, "review baseline", reviewBaseline, repairState{
			typeName: "knowledge", typeSource: "auto", archived: false, guard: "needs_review", repairMark: "",
		})
	})

	t.Run("f_non_head_row_untouched", func(t *testing.T) {
		wantState(t, pool, "non-head block", nonHead, repairState{
			typeName: "checkpoint", typeSource: "auto", archived: true, guard: "archived_dup", repairMark: "",
		})
	})

	t.Run("g_idempotent", func(t *testing.T) {
		before := categorySnapshot(t, pool)
		applyM121Again(t, pool)
		after := categorySnapshot(t, pool)

		if len(before) != len(after) {
			t.Fatalf("row count changed on re-run: %d -> %d", len(before), len(after))
		}
		for id, want := range before {
			if got := after[id]; got != want {
				t.Errorf("re-run changed %s:\n  before %s\n  after  %s", id, want, got)
			}
		}
	})
}

// TestRepair_ManualTypeStaysArchived is the type_name filter's own gate: a
// head a human typed by hand keeps its type (statement (a) only touches
// type_source='auto') and therefore must NOT be un-archived either — a
// knowledge-typed row is guard.check=true, so un-archiving it would only feed
// it back into the archive lane on the next guard run. Dropping
// `AND type_name = 'checkpoint'` from statement (b) turns this red.
func TestRepair_ManualTypeStaysArchived(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 120)

	manual := seedBlock(t, pool, "", "compaction-checkpoints",
		"Compaction checkpoint head 20260704_manual", "knowledge", "manual", "archived_dup", true)

	if err := store.RunMigrationsUpTo(ctx, pool, 121); err != nil {
		t.Fatalf("apply migration 121: %v", err)
	}

	wantState(t, pool, "manual head", manual, repairState{
		typeName: "knowledge", typeSource: "manual", archived: true, guard: "archived_dup", repairMark: "",
	})
}
