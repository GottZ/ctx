//go:build integration

// Integration tests for the G41 sensitivity audit store layer against a real
// PG18 testcontainer (migration 055 columns).
//
// Covers the masterplan G41 negative probes on the store side:
//   - 'manual' is untouchable BY THE SQL PREDICATE: ApplyAuditVerdict against
//     a manual row applies nothing and changes nothing (red without the
//     source guard — an unguarded UPDATE demonstrably overwrites it)
//   - the pick set is source='default' only, home-scope only, and respects
//     the no-verdict retry cooldown (MarkAuditAttempt)
//   - a verdict write flips source to 'llm-audit' and removes the row from
//     the pick set (idempotence)
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestSensitivityAudit -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertAuditBlock seeds one block and returns its id. source != 'default'
// is stamped via a follow-up UPDATE — the write path has no API for it,
// which is the point (manual/llm-audit are post-write classifications).
func insertAuditBlock(t *testing.T, pool *pgxpool.Pool, scope, title, source string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'content of ' || $1, $2)
		 RETURNING id`, title, scope).Scan(&id)
	if err != nil {
		t.Fatalf("insert block %s: %v", title, err)
	}
	if source != "default" {
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET sensitivity_source = $2 WHERE id = $1`, id, source); err != nil {
			t.Fatalf("stamp source %s: %v", source, err)
		}
	}
	return id
}

// blockContentMD5 reads the CURRENT content digest of a block — the value
// PickAuditBlocks hands the audit and ApplyAuditVerdict re-checks (H10). Tests
// that are not about the content binding pass this so the probe stays about the
// guarantee it names.
func blockContentMD5(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var sum string
	if err := pool.QueryRow(ctx,
		`SELECT md5(content) FROM context_blocks WHERE id = $1`, id).Scan(&sum); err != nil {
		t.Fatalf("read content digest: %v", err)
	}
	return sum
}

func blockSensState(t *testing.T, pool *pgxpool.Pool, id string) (sens, source string, auditedAt *time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source, sensitivity_audited_at
		 FROM context_blocks WHERE id = $1`, id).Scan(&sens, &source, &auditedAt); err != nil {
		t.Fatalf("read block state: %v", err)
	}
	return sens, source, auditedAt
}

func pickIDs(t *testing.T, pool *pgxpool.Pool, scope string, cooldown time.Duration) map[string]bool {
	t.Helper()
	blocks, err := store.PickAuditBlocks(context.Background(), pool, scope, cooldown, 100, false)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	out := make(map[string]bool, len(blocks))
	for _, b := range blocks {
		out[b.ID] = true
	}
	return out
}

func TestSensitivityAudit_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	defaultID := insertAuditBlock(t, pool, "private", "g41-default", "default")
	manualID := insertAuditBlock(t, pool, "private", "g41-manual", "manual")
	foreignID := insertAuditBlock(t, pool, "work", "g41-foreign-scope", "default")

	t.Run("pick set is default-source home-scope only", func(t *testing.T) {
		picked := pickIDs(t, pool, "private", time.Hour)
		if !picked[defaultID] {
			t.Error("default-source home-scope block missing from pick set")
		}
		if picked[manualID] {
			t.Error("manual block in pick set — the audit must never see it")
		}
		if picked[foreignID] {
			t.Error("foreign-scope block in pick set — classification follows block ownership (design 03 §2.3d)")
		}
	})

	t.Run("manual is untouchable by the source predicate", func(t *testing.T) {
		applied, err := store.ApplyAuditVerdict(ctx, pool, manualID, backends.SensInternal, blockContentMD5(t, pool, manualID))
		if err != nil {
			t.Fatalf("apply against manual: %v", err)
		}
		if applied {
			t.Fatal("verdict applied to a manual row — the source guard is gone")
		}
		sens, source, auditedAt := blockSensState(t, pool, manualID)
		if sens != "credentials" || source != "manual" || auditedAt != nil {
			t.Fatalf("manual row changed: sens=%s source=%s audited=%v", sens, source, auditedAt)
		}

		// Negative probe: the same UPDATE WITHOUT the source predicate
		// demonstrably overwrites the manual row — the guard carries the
		// guarantee, not luck. Probed on a sacrificial row.
		probeID := insertAuditBlock(t, pool, "private", "g41-manual-probe", "manual")
		tag, err := pool.Exec(ctx,
			`UPDATE context_blocks
			    SET sensitivity = 'internal', sensitivity_source = 'llm-audit'
			  WHERE id = $1`, probeID)
		if err != nil {
			t.Fatalf("unguarded probe update: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatal("unguarded probe did not hit — the negative probe proves nothing")
		}
	})

	t.Run("no-verdict cooldown removes and re-admits", func(t *testing.T) {
		if err := store.MarkAuditAttempt(ctx, pool, defaultID); err != nil {
			t.Fatalf("mark attempt: %v", err)
		}
		sens, source, auditedAt := blockSensState(t, pool, defaultID)
		if sens != "credentials" || source != "default" || auditedAt == nil {
			t.Fatalf("attempt stamp wrong: sens=%s source=%s audited=%v", sens, source, auditedAt)
		}

		if picked := pickIDs(t, pool, "private", time.Hour); picked[defaultID] {
			t.Error("block inside the retry cooldown re-picked")
		}
		// Zero cooldown re-admits — the next evening run retries it.
		if picked := pickIDs(t, pool, "private", 0); !picked[defaultID] {
			t.Error("block not re-admitted after the cooldown window")
		}
	})

	t.Run("verdict write flips source and leaves the pick set", func(t *testing.T) {
		applied, err := store.ApplyAuditVerdict(ctx, pool, defaultID, backends.SensInternal, blockContentMD5(t, pool, defaultID))
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if !applied {
			t.Fatal("verdict against a default row not applied")
		}
		sens, source, auditedAt := blockSensState(t, pool, defaultID)
		if sens != "internal" || source != "llm-audit" || auditedAt == nil {
			t.Fatalf("verdict state wrong: sens=%s source=%s audited=%v", sens, source, auditedAt)
		}

		// Idempotence: the classified row is out of the pick set even with
		// zero cooldown, and a second verdict is discarded.
		if picked := pickIDs(t, pool, "private", 0); picked[defaultID] {
			t.Error("classified block still in pick set")
		}
		applied, err = store.ApplyAuditVerdict(ctx, pool, defaultID, backends.SensPersonal, blockContentMD5(t, pool, defaultID))
		if err != nil {
			t.Fatalf("second apply: %v", err)
		}
		if applied {
			t.Error("second verdict applied — llm-audit rows must be out of reach")
		}
	})

	t.Run("progress counts by source", func(t *testing.T) {
		pending, bySource, err := store.AuditProgress(ctx, pool, "private")
		if err != nil {
			t.Fatalf("progress: %v", err)
		}
		if pending != bySource["default"] {
			t.Errorf("pending=%d != by_source[default]=%d", pending, bySource["default"])
		}
		// End state of the private scope after the subtests above: defaultID
		// got its verdict, probeID was overwritten BY DESIGN by the unguarded
		// negative probe (manual → llm-audit), manualID alone stays manual.
		// The foreign-scope block must not appear in private counts.
		if bySource["llm-audit"] != 2 || bySource["manual"] != 1 || bySource["default"] != 0 {
			t.Errorf("by_source end state wrong (want llm-audit=2 manual=1 default=0): %v", bySource)
		}
	})
}
