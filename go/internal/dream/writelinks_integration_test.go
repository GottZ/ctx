//go:build integration

package dream_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	icSourceID = "019d0000-0000-7000-9000-000000000001"
	icTargetID = "019d0000-0000-7000-9000-000000000002"
	icOtherID  = "019d0000-0000-7000-9000-000000000003"
)

// insertBlock writes a minimal context_blocks row with the supplied id and
// metadata. created_at is set explicitly so V8/V9 temporal checks fire
// deterministically.
func insertBlock(t *testing.T, pool *pgxpool.Pool, id, scope, category, title string, createdAt, updatedAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
		id, category, title, "test content for "+title, scope, createdAt, updatedAt,
	)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

// countLinks returns the number of context_dream_links rows for the given
// source.
func countLinks(t *testing.T, pool *pgxpool.Pool, sourceID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links WHERE source_block_id = $1::uuid`,
		sourceID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// TestWriteLinks_TxAbort_BehaviourMatchesContract verifies that when a per-link
// INSERT fails inside the transaction, the deferred Rollback (or the failing
// Commit) actually leaves the DB clean — i.e. no partial writes survive. The
// pgxmock equivalent (TestWriteLinks_LoopBreaks_ZeroWritten) only checks the
// return value, not the persisted state, because pgxmock cannot model an
// aborted-transaction Commit (W10-gap).
//
// Trigger: a relationship value that passes the per-link Go-side filter chain
// but fails the schema CHECK constraint
// (relationship IN ('topical','factual','causal','supersedes')).
func TestWriteLinks_TxAbort_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, icSourceID, "private", "decisions", "src title", tEarly, tEarly)
	insertBlock(t, pool, icTargetID, "private", "decisions", "tgt title", tLate, tLate)

	// 'invalid_relationship' clears the V5/V6/V8/V9/V10 chain (none of those
	// gates apply to unknown relationships) and reaches the INSERT, where the
	// CHECK constraint rejects it. Real PG marks the TX aborted, so Commit
	// must fail or the deferred Rollback must reclaim cleanly.
	written, err := dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "invalid_relationship", Confidence: 0.9}})

	// Either: Commit returns the aborted-TX error wrapped by WriteLinks, OR
	// the deferred Rollback fires first and Commit never runs (also fine).
	// The contract that matters is: written=0 AND no row persisted.
	if written != 0 {
		t.Errorf("got written=%d, want 0", written)
	}
	if err != nil {
		// Acceptable: error path with aborted-TX wrapping.
		msg := err.Error()
		if !(strings.Contains(msg, "commit") || strings.Contains(msg, "transaction") || strings.Contains(msg, "abort")) {
			t.Errorf("error not recognisable as TX-abort: %v", err)
		}
	}

	if got := countLinks(t, pool, icSourceID); got != 0 {
		t.Errorf("persisted %d links after CHECK-rejected INSERT, want 0 (TX must roll back)", got)
	}
}

// TestWriteLinks_Supersedes_RealSimilarity_BehaviourMatchesContract verifies
// that V8's supersedes structural pre-filter uses pg_trgm `similarity()`
// against the real extension. Two cases:
//
//   - similar titles (sim ≥ 0.25): link inserted.
//   - dissimilar titles (sim < 0.25): link rejected, no row written.
//
// pgxmock can stub the similarity return value directly but cannot exercise
// pg_trgm itself, so a real-PG layer is the only way to catch a regression
// where the SQL changes (column order, function name, threshold const) but
// still parses as a valid query.
func TestWriteLinks_Supersedes_RealSimilarity_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Welle 46 Convention-Switch (2026-05-22): "A supersedes B" → A=source=newer,
	// B=target=older. Setup: source = tLate (newer), target = tEarly (older).
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, icSourceID, "private", "decisions", "Authentication strategy v2", tLate, tLate)
	insertBlock(t, pool, icTargetID, "private", "decisions", "Authentication strategy", tEarly, tEarly)

	written, err := dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "supersedes", Confidence: 0.95}})
	if err != nil {
		t.Fatalf("similar-title supersedes: unexpected error: %v", err)
	}
	if written != 1 {
		t.Errorf("similar-title supersedes: got written=%d, want 1", written)
	}
	if got := countLinks(t, pool, icSourceID); got != 1 {
		t.Errorf("similar-title supersedes: got %d persisted, want 1", got)
	}

	// Reset: drop the link so the next sub-case starts clean. Direct DELETE,
	// not via WriteLinks (we want to test the dissimilar path standalone).
	if _, err := pool.Exec(ctx, `DELETE FROM context_dream_links WHERE source_block_id = $1::uuid`, icSourceID); err != nil {
		t.Fatalf("reset links: %v", err)
	}
	// Welle 46: snapshot side-effect is on the TARGET (the older block); revert it.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET lifecycle_state = 'knowledge', superseded_by = NULL WHERE id = $1::uuid`, icTargetID); err != nil {
		t.Fatalf("reset lifecycle_state: %v", err)
	}

	// Dissimilar-title sibling — same category, source NEWER than target, but
	// no trigram overlap above the 0.25 threshold.
	insertBlock(t, pool, icOtherID, "private", "decisions", "completely unrelated topic xyzzy", tEarly, tEarly)

	written, err = dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icOtherID, Relationship: "supersedes", Confidence: 0.95}})
	if err != nil {
		t.Fatalf("dissimilar-title supersedes: unexpected error: %v", err)
	}
	if written != 0 {
		t.Errorf("dissimilar-title supersedes: got written=%d, want 0 (V8 sim<0.25 must reject)", written)
	}
	if got := countLinks(t, pool, icSourceID); got != 0 {
		t.Errorf("dissimilar-title supersedes: got %d persisted, want 0", got)
	}
}

// TestWriteLinks_OnConflictUpsert_BehaviourMatchesContract verifies that a
// second WriteLinks run for the same (source, target) pair updates the row
// instead of duplicating it (PRIMARY KEY would otherwise fail). Confirms
// the ON CONFLICT DO UPDATE clause refreshes confidence and dream_version.
func TestWriteLinks_OnConflictUpsert_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, icSourceID, "private", "topic", "src", tEarly, tEarly)
	insertBlock(t, pool, icTargetID, "private", "topic", "tgt", tLate, tLate)

	// First run: write with confidence 0.7.
	written, err := dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "topical", Confidence: 0.7}})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if written != 1 {
		t.Fatalf("first write: got written=%d, want 1", written)
	}

	// Second run: same (source, target) pair, higher confidence. Must
	// upsert into the existing row, not insert a duplicate.
	written, err = dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "topical", Confidence: 0.95}})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if written != 1 {
		t.Errorf("second write: got written=%d, want 1", written)
	}

	if got := countLinks(t, pool, icSourceID); got != 1 {
		t.Errorf("after upsert: got %d persisted, want 1 (ON CONFLICT must update, not duplicate)", got)
	}

	var conf, rawConf float64
	if err := pool.QueryRow(ctx,
		`SELECT confidence, raw_confidence FROM context_dream_links
		WHERE source_block_id=$1::uuid AND target_block_id=$2::uuid`,
		icSourceID, icTargetID,
	).Scan(&conf, &rawConf); err != nil {
		t.Fatalf("read upserted row: %v", err)
	}
	// weighted = raw * sourceQuality(1.0) * targetQuality(1.0) = raw
	if math.Abs(conf-0.95) > 0.01 {
		t.Errorf("upsert did not refresh confidence: got %f, want ~0.95", conf)
	}
	if math.Abs(rawConf-0.95) > 0.01 {
		t.Errorf("upsert did not refresh raw_confidence: got %f, want ~0.95", rawConf)
	}
}

// TestWriteLinks_ReplaceSemantics_RealUUIDs_BehaviourMatchesContract verifies
// the stale-DELETE clause `target_block_id != ALL($2::uuid[])` against real
// UUID arrays. Two-run scenario: first run writes two links, second run
// kept-targets contains only one — the other must be deleted in-TX.
func TestWriteLinks_ReplaceSemantics_RealUUIDs_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	insertBlock(t, pool, icSourceID, "private", "topic", "src", tEarly, tEarly)
	insertBlock(t, pool, icTargetID, "private", "topic", "tgt1", tLate, tLate)
	insertBlock(t, pool, icOtherID, "private", "topic", "tgt2", tLate, tLate)

	written, err := dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{
			{TargetID: icTargetID, Relationship: "topical", Confidence: 0.8},
			{TargetID: icOtherID, Relationship: "topical", Confidence: 0.8},
		})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if written != 2 {
		t.Fatalf("first write: got written=%d, want 2", written)
	}
	if got := countLinks(t, pool, icSourceID); got != 2 {
		t.Fatalf("after first write: got %d, want 2", got)
	}

	// Second run with only icTargetID — replace-semantics must delete
	// the orphaned icOtherID via the != ALL clause.
	written, err = dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if written != 1 {
		t.Errorf("second write: got written=%d, want 1", written)
	}

	if got := countLinks(t, pool, icSourceID); got != 1 {
		t.Errorf("after replace: got %d, want 1 (ALL-bounds DELETE must drop the orphan)", got)
	}

	var survivor string
	if err := pool.QueryRow(ctx,
		`SELECT target_block_id::text FROM context_dream_links WHERE source_block_id=$1::uuid`,
		icSourceID,
	).Scan(&survivor); err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	if survivor != icTargetID {
		t.Errorf("wrong survivor after replace: got %s, want %s (kept-target must survive)", survivor, icTargetID)
	}
}

// TestWriteLinks_SnapshotRevert_Idempotent_BehaviourMatchesContract verifies
// the full ApplySupersedes lifecycle against real PG:
//
//  1. supersedes link writes lifecycle_state='snapshot' on TARGET (older block).
//  2. Idempotent re-write of the same supersedes link does not drift.
//  3. Subsequent write that drops the supersedes link reverts lifecycle_state
//     to 'knowledge' and superseded_by to NULL via replaceStaleLinks (M070:
//     the lifecycle state machine is NOT NULL — pre-M070 the revert wrote NULL).
//
// Welle 46 Convention-Switch (2026-05-22): under "A supersedes B" → A=source=
// newer, B=target=outdated, the TARGET is the block that becomes a snapshot.
func TestWriteLinks_SnapshotRevert_Idempotent_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Source = tLate (newer), Target = tEarly (older). Snapshot marker lands on
	// the target.
	insertBlock(t, pool, icSourceID, "private", "decisions", "auth strategy v2", tLate, tLate)
	insertBlock(t, pool, icTargetID, "private", "decisions", "auth strategy", tEarly, tEarly)

	// Step 1: write supersedes link → target becomes snapshot, superseded_by=source.
	written, err := dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "supersedes", Confidence: 0.95}})
	if err != nil {
		t.Fatalf("step 1 write: %v", err)
	}
	if written != 1 {
		t.Fatalf("step 1: got written=%d, want 1", written)
	}

	bt, sb := readSnapshotState(t, pool, icTargetID)
	if bt != "snapshot" || sb != icSourceID {
		t.Errorf("step 1: got lifecycle_state=%q superseded_by=%q on target, want snapshot/%s", bt, sb, icSourceID)
	}
	// Source must NOT be snapshotted.
	if btSrc, sbSrc := readSnapshotState(t, pool, icSourceID); btSrc == "snapshot" || sbSrc != "" {
		t.Errorf("step 1: source incorrectly marked: lifecycle_state=%q superseded_by=%q", btSrc, sbSrc)
	}

	// Step 2: idempotent re-write — same input, no drift on target.
	written, err = dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icTargetID, Relationship: "supersedes", Confidence: 0.95}})
	if err != nil {
		t.Fatalf("step 2 write: %v", err)
	}
	if written != 1 {
		t.Errorf("step 2: got written=%d, want 1", written)
	}

	bt, sb = readSnapshotState(t, pool, icTargetID)
	if bt != "snapshot" || sb != icSourceID {
		t.Errorf("step 2 drift: got lifecycle_state=%q superseded_by=%q on target, want snapshot/%s", bt, sb, icSourceID)
	}

	// Step 3: write a non-supersedes link to a different target → replaceStale
	// drops the old supersedes row and reverts the original target's lifecycle_state.
	insertBlock(t, pool, icOtherID, "private", "decisions", "unrelated decision", tLate, tLate)

	written, err = dream.WriteLinks(ctx, pool, icSourceID, "private", 1.0,
		[]dream.Link{{TargetID: icOtherID, Relationship: "topical", Confidence: 0.8}})
	if err != nil {
		t.Fatalf("step 3 write: %v", err)
	}
	if written != 1 {
		t.Errorf("step 3: got written=%d, want 1", written)
	}

	bt, sb = readSnapshotState(t, pool, icTargetID)
	if bt != "knowledge" || sb != "" {
		t.Errorf("step 3 revert (target): got lifecycle_state=%q superseded_by=%q, want knowledge/NULL", bt, sb)
	}
}

// readSnapshotState fetches lifecycle_state and superseded_by for the given block,
// returning empty strings when the column is NULL.
func readSnapshotState(t *testing.T, pool *pgxpool.Pool, id string) (blockType, supersededBy string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var bt, sb *string
	err := pool.QueryRow(ctx,
		`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id=$1::uuid`,
		id,
	).Scan(&bt, &sb)
	if err != nil {
		t.Fatalf("read snapshot state: %v", err)
	}
	if bt != nil {
		blockType = *bt
	}
	if sb != nil {
		supersededBy = *sb
	}
	return
}
