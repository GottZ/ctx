//go:build integration

// Wave H10 probes (design 04 §2.4-C/§4.5-d, §7 H10): the G41 verdict is bound
// to the CONTENT VERSION it was formed over, not merely to a row id.
//
// The gap this closes. PickAuditBlocks hands the audit a COPY of the content;
// the two classify calls happen outside any transaction, and the ordinary write
// path only re-stamps sensitivity_source when the write itself upgrades
// (manual/detector) — a plain content update leaves source='default'. Before
// H10 the verdict write re-checked id + source and nothing else, so a writer
// could store harmless v1, let the run judge v1, and overwrite with v2 while
// the model was still answering. The verdict about v1 then landed on v2 — and
// so did the H9 structural veto, which scans that same picked copy.
//
//	(a) content swapped between pick and write ⇒ applied=false, row untouched
//	(b) content unchanged                      ⇒ applied=true (no false discards)
//	(c) manual reclassification between them   ⇒ still applied=false (regression)
//
// RED against Ist (the pre-H10 predicate `WHERE id = $1 AND
// sensitivity_source = 'default'`): case (a) applies, the block is stamped
// llm-audit/internal, and a block whose CURRENT text nobody classified becomes
// eligible for a no-credentials backend. The sub-case "unguarded" below runs
// exactly that predicate on a sacrificial row and proves it hits.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestSensitivityAuditContentBinding -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// pickOne returns the single pick-set row for a scope — the AuditBlock the run
// would carry into the classify calls, digest included.
func pickOne(t *testing.T, pool *pgxpool.Pool, scope string) store.AuditBlock {
	t.Helper()
	blocks, err := store.PickAuditBlocks(context.Background(), pool, scope, 0, 10, false)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("pick returned %d rows, want exactly 1", len(blocks))
	}
	return blocks[0]
}

func rewriteContent(t *testing.T, pool *pgxpool.Pool, id, content string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET content = $2 WHERE id = $1`, id, content); err != nil {
		t.Fatalf("rewrite content: %v", err)
	}
}

func TestSensitivityAuditContentBinding_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (a) The race, made deterministic: pick, then rewrite, then write the
	// verdict formed over the picked version.
	t.Run("content swapped between pick and write is discarded", func(t *testing.T) {
		const scope = "h10-swap"
		id := insertAuditBlock(t, pool, scope, "h10-swapped", "default")
		blk := pickOne(t, pool, scope)
		if blk.ID != id || blk.ContentMD5 == "" {
			t.Fatalf("pick did not carry the content digest: %+v", blk)
		}

		rewriteContent(t, pool, id, "v2: AKIAIOSFODNN7EXAMPLE and other things the run never saw")

		applied, err := store.ApplyAuditVerdict(ctx, pool, id, backends.SensInternal, blk.ContentMD5)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if applied {
			t.Fatal("verdict applied to a rewritten row — the verdict is bound to a row id, not to the content it judged")
		}
		sens, source, auditedAt := blockSensState(t, pool, id)
		if sens != "credentials" || source != "default" || auditedAt != nil {
			t.Fatalf("rewritten row changed: sens=%s source=%s audited=%v", sens, source, auditedAt)
		}
		// The discarded block is back in the pick set — nothing is lost, the
		// next run judges the version that now stands.
		if picked := pickIDs(t, pool, scope, 0); !picked[id] {
			t.Error("discarded block did not re-enter the pick set")
		}

		// Negative probe on a sacrificial row: the PRE-H10 predicate — id +
		// source only — demonstrably hits the rewritten row. The conjunct
		// carries the guarantee, not luck.
		probeID := insertAuditBlock(t, pool, scope, "h10-unguarded-probe", "default")
		rewriteContent(t, pool, probeID, "v2 of the sacrificial row")
		tag, err := pool.Exec(ctx,
			`UPDATE context_blocks
			    SET sensitivity = 'internal', sensitivity_source = 'llm-audit'
			  WHERE id = $1 AND sensitivity_source = 'default'`, probeID)
		if err != nil {
			t.Fatalf("unguarded probe update: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatal("unguarded probe did not hit — the negative probe proves nothing")
		}
	})

	// (b) The regression that keeps the binding honest: an untouched block must
	// still get its verdict. A binding that discards everything would be
	// "secure" and useless.
	t.Run("unchanged content applies", func(t *testing.T) {
		const scope = "h10-stable"
		id := insertAuditBlock(t, pool, scope, "h10-unchanged", "default")
		blk := pickOne(t, pool, scope)

		applied, err := store.ApplyAuditVerdict(ctx, pool, id, backends.SensInternal, blk.ContentMD5)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if !applied {
			t.Fatal("verdict against an unchanged row discarded — the binding produces false discards")
		}
		sens, source, auditedAt := blockSensState(t, pool, id)
		if sens != "internal" || source != "llm-audit" || auditedAt == nil {
			t.Fatalf("verdict state wrong: sens=%s source=%s audited=%v", sens, source, auditedAt)
		}
	})

	// (c) The older guarantee is untouched: the source predicate still discards
	// a manual reclassification, and it does so even when the content matches.
	t.Run("manual reclassification still discards", func(t *testing.T) {
		const scope = "h10-manual"
		id := insertAuditBlock(t, pool, scope, "h10-manual-race", "default")
		blk := pickOne(t, pool, scope)

		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET sensitivity = 'personal', sensitivity_source = 'manual' WHERE id = $1`,
			id); err != nil {
			t.Fatalf("manual reclassification: %v", err)
		}

		applied, err := store.ApplyAuditVerdict(ctx, pool, id, backends.SensInternal, blk.ContentMD5)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if applied {
			t.Fatal("verdict applied over a manual reclassification — the source predicate is gone")
		}
		sens, source, _ := blockSensState(t, pool, id)
		if sens != "personal" || source != "manual" {
			t.Fatalf("manual row changed: sens=%s source=%s", sens, source)
		}
	})
}
