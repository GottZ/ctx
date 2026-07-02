//go:build integration

// Integration tests for the RunGuardBatch pipeline against a real PG18
// testcontainer. Exercises the three decision branches (near_duplicate,
// needs_review, clean) end-to-end, locks down the v1.6.6 42P08 fix as a
// regression test, and verifies that every processed block produces a
// context_write_log row.
//
// Build-tagged 'integration' — `go test -short` skips this file. Run with:
//
//	go test -tags=integration ./internal/guard/... -count=1 -v

package guard_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	gNearAID = "019e9aaa-0000-7000-9000-000000000001"
	gNearBID = "019e9aaa-0000-7000-9000-000000000002"
	gReviewAID = "019e9aaa-0000-7000-9000-000000000003"
	gReviewBID = "019e9aaa-0000-7000-9000-000000000004"
	gCleanAID  = "019e9aaa-0000-7000-9000-000000000005"
	gCleanBID  = "019e9aaa-0000-7000-9000-000000000006"
	gP08ID     = "019e9aaa-0000-7000-9000-000000000007"
	gP08PeerID = "019e9aaa-0000-7000-9000-000000000008"
)

// gitSet returns the builtin policy snapshot (byte-equivalent to the M072
// seeds per the T3 drift gate) — the seed-default behaviour these golden
// tests pin (T7: same three-branch outcomes as the pre-policy literals).
func gitSet() *blocktype.Set { return blocktype.NewRegistry().Snapshot() }

// insertGuardBlock writes a context_blocks row with the supplied id, title
// and embedding vector. The created_at is set explicitly so the FOR UPDATE
// SKIP LOCKED ordering by created_at ASC is deterministic across runs.
func insertGuardBlock(t *testing.T, pool *pgxpool.Pool, id, title string, embedding []float32, createdAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec := pgvec.NewVector(embedding)
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, lifecycle_state, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $8)`,
		id, "learnings", title, "guard-it body for "+title, "private", vec, "knowledge", createdAt,
	)
	if err != nil {
		t.Fatalf("insert guard block %s: %v", id, err)
	}
}

// readGuardState fetches is_archived, guard_status, and the metadata.guard_*
// fields written by applyDecision. Returns zero values when a column is NULL.
type guardState struct {
	IsArchived       bool
	GuardStatus      string
	MetaStatus       string
	MetaCheckedAt    string
	MetaSimilarity   float64
	MetaMatchedID    string
	MetaIsCrossScope bool
	MetaCheckedSet   bool
}

func readGuardState(t *testing.T, pool *pgxpool.Pool, id string) guardState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		s             guardState
		gs            *string
		metaStatus    *string
		metaCheckedAt *string
		metaSim       *float64
		metaMatched   *string
		metaCross     *bool
	)
	err := pool.QueryRow(ctx,
		`SELECT
			is_archived,
			guard_status,
			metadata->>'guard_status',
			metadata->>'guard_checked_at',
			(metadata->>'guard_similarity')::float8,
			metadata->>'guard_matched_id',
			(metadata->>'guard_is_cross_scope')::bool
		FROM context_blocks WHERE id = $1::uuid`,
		id,
	).Scan(&s.IsArchived, &gs, &metaStatus, &metaCheckedAt, &metaSim, &metaMatched, &metaCross)
	if err != nil {
		t.Fatalf("read guard state for %s: %v", id, err)
	}
	if gs != nil {
		s.GuardStatus = *gs
	}
	if metaStatus != nil {
		s.MetaStatus = *metaStatus
	}
	if metaCheckedAt != nil {
		s.MetaCheckedAt = *metaCheckedAt
		s.MetaCheckedSet = true
	}
	if metaSim != nil {
		s.MetaSimilarity = *metaSim
	}
	if metaMatched != nil {
		s.MetaMatchedID = *metaMatched
	}
	if metaCross != nil {
		s.MetaIsCrossScope = *metaCross
	}
	return s
}

// countWriteLog returns the number of context_write_log rows that point at
// the given block_id.
func countWriteLog(t *testing.T, pool *pgxpool.Pool, blockID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_write_log WHERE block_id = $1::uuid`,
		blockID,
	).Scan(&n); err != nil {
		t.Fatalf("count write log for %s: %v", blockID, err)
	}
	return n
}

// unitVec1024 builds a 1024-dim vector with a single component set to v at
// the given index. Cosine similarity between two such vectors equals 1.0
// when both indices match, and 0.0 otherwise — useful for forcing the guard
// thresholds without floating-point noise.
func unitVec1024(idx int, v float32) []float32 {
	x := make([]float32, 1024)
	x[idx] = v
	return x
}

// blendedVec1024 builds a 1024-dim vector with two components set, yielding
// a controlled cosine-similarity against a single-component reference. The
// caller picks aIdx/bIdx and the scalar weights; we normalise the magnitude
// to keep cosine math predictable.
func blendedVec1024(aIdx, bIdx int, aW, bW float32) []float32 {
	x := make([]float32, 1024)
	x[aIdx] = aW
	x[bIdx] = bW
	// Normalise to unit length so cosine ≈ dot product when compared against
	// a unit reference. Avoids precision drift in the round() inside
	// ctx_guard_check.
	mag := float32(math.Sqrt(float64(aW*aW + bW*bW)))
	if mag > 0 {
		x[aIdx] /= mag
		x[bIdx] /= mag
	}
	return x
}

// TestRunGuardBatch_NearDuplicatePath asserts that two blocks with identical
// embeddings (cosine = 1.0) trigger the near_duplicate branch. The block
// processed FIRST (older by created_at) sees the still-unchecked clone as
// its nearest neighbour → it gets archived. The clone, processed second,
// sees no unarchived neighbour (the just-archived first block is filtered
// by the `NOT cb.is_archived` clause in ctx_guard_check) → clean.
//
// This "first-loses" semantic is production behaviour: under FOR UPDATE
// SKIP LOCKED ordering by created_at ASC, the older near-duplicate is the
// one auto-archived. It is the inverse of what intuition suggests but
// matches the SQL: the guard does not skew toward keeping the older block;
// it walks the batch in order, archiving whichever block's match exceeds
// the duplicate threshold at the moment it is checked.
func TestRunGuardBatch_NearDuplicatePath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	emb := unitVec1024(0, 1.0) // identical embedding for both blocks
	tEarly := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	tLate := tEarly.Add(1 * time.Hour)

	insertGuardBlock(t, pool, gNearAID, "near-dup-anchor", emb, tEarly)
	insertGuardBlock(t, pool, gNearBID, "near-dup-clone", emb, tLate)

	processed, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != 2 {
		t.Errorf("processed: want 2, got %d", processed)
	}

	// First-processed (older): matches the still-unchecked clone at cosine=1.0
	// → archived as near_duplicate. All metadata.guard_* fields must land.
	a := readGuardState(t, pool, gNearAID)
	if !a.IsArchived {
		t.Errorf("first-processed block not archived: %+v", a)
	}
	if a.GuardStatus != "archived_dup" {
		t.Errorf("first-processed block guard_status: want 'archived_dup', got %q", a.GuardStatus)
	}
	if a.MetaStatus != "near_duplicate" {
		t.Errorf("first-processed metadata.guard_status: want 'near_duplicate', got %q", a.MetaStatus)
	}
	if !a.MetaCheckedSet {
		t.Errorf("first-processed metadata.guard_checked_at not set")
	}
	if a.MetaMatchedID != gNearBID {
		t.Errorf("first-processed metadata.guard_matched_id: want %s, got %q", gNearBID, a.MetaMatchedID)
	}
	if math.Abs(a.MetaSimilarity-1.0) > 0.001 {
		t.Errorf("first-processed metadata.guard_similarity: want ~1.0, got %g", a.MetaSimilarity)
	}
	// Both blocks share scope='private' → not cross-scope.
	if a.MetaIsCrossScope {
		t.Errorf("first-processed metadata.guard_is_cross_scope: want false, got true")
	}

	// Second-processed (newer): its only embedding-match got archived in the
	// same batch and is now filtered out → no neighbour → clean.
	b := readGuardState(t, pool, gNearBID)
	if b.IsArchived {
		t.Errorf("second-processed block wrongly archived (peer was archived first): %+v", b)
	}
	if b.GuardStatus != "clean" {
		t.Errorf("second-processed block guard_status: want 'clean', got %q", b.GuardStatus)
	}
}

// TestRunGuardBatch_NeedsReviewPath asserts that two blocks with similar
// but not identical embeddings (cosine in [0.92, 0.98)) land in the
// needs_review branch: block survives (is_archived=false), guard_status is
// 'needs_review', and metadata.guard_* is populated.
//
// We control similarity via a blended 2-component vector: anchor on axis 0,
// peer with 0.96 axis-0 weight + 0.28 axis-1 weight → cosine = 0.96, which
// sits squarely between the 0.92 and 0.98 thresholds.
func TestRunGuardBatch_NeedsReviewPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	anchor := unitVec1024(0, 1.0)
	// peer: cosine with anchor = 0.96 (between 0.92 review-threshold and 0.98 dup-threshold)
	peer := blendedVec1024(0, 1, 0.96, 0.28)

	tEarly := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	tLate := tEarly.Add(1 * time.Hour)

	insertGuardBlock(t, pool, gReviewAID, "review-anchor", anchor, tEarly)
	insertGuardBlock(t, pool, gReviewBID, "review-peer", peer, tLate)

	processed, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != 2 {
		t.Errorf("processed: want 2, got %d", processed)
	}

	b := readGuardState(t, pool, gReviewBID)
	if b.IsArchived {
		t.Errorf("review-peer wrongly archived: %+v", b)
	}
	if b.GuardStatus != "needs_review" {
		t.Errorf("review-peer guard_status: want 'needs_review', got %q", b.GuardStatus)
	}
	if b.MetaStatus != "needs_review" {
		t.Errorf("review-peer metadata.guard_status: want 'needs_review', got %q", b.MetaStatus)
	}
	if !b.MetaCheckedSet {
		t.Errorf("review-peer metadata.guard_checked_at not set")
	}
	if b.MetaMatchedID != gReviewAID {
		t.Errorf("review-peer metadata.guard_matched_id: want %s, got %q", gReviewAID, b.MetaMatchedID)
	}
	if b.MetaSimilarity < 0.92 || b.MetaSimilarity >= 0.98 {
		t.Errorf("review-peer metadata.guard_similarity: want in [0.92, 0.98), got %g", b.MetaSimilarity)
	}
}

// TestRunGuardBatch_CleanPath asserts that two orthogonal embeddings
// (cosine = 0.0) take the clean branch: guard_status='clean', no archive,
// metadata populated for the audit trail.
func TestRunGuardBatch_CleanPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	a := unitVec1024(0, 1.0)
	b := unitVec1024(500, 1.0) // orthogonal — cosine = 0

	tEarly := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	tLate := tEarly.Add(1 * time.Hour)

	insertGuardBlock(t, pool, gCleanAID, "clean-a", a, tEarly)
	insertGuardBlock(t, pool, gCleanBID, "clean-b", b, tLate)

	processed, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != 2 {
		t.Errorf("processed: want 2, got %d", processed)
	}

	for _, id := range []string{gCleanAID, gCleanBID} {
		s := readGuardState(t, pool, id)
		if s.IsArchived {
			t.Errorf("clean block %s wrongly archived: %+v", id, s)
		}
		if s.GuardStatus != "clean" {
			t.Errorf("clean block %s guard_status: want 'clean', got %q", id, s.GuardStatus)
		}
		if s.MetaStatus != "clean" {
			t.Errorf("clean block %s metadata.guard_status: want 'clean', got %q", id, s.MetaStatus)
		}
		if !s.MetaCheckedSet {
			t.Errorf("clean block %s metadata.guard_checked_at not set", id)
		}
	}
}

// TestRunGuardBatch_42P08_Regression locks down the v1.6.6 fix. Before the
// fix, the jsonb_build_object call in applyDecision triggered SQLSTATE
// 42P08 (the pgx wrapper around PG's 42P18 "could not determine data type
// of parameter") at PREPARE time under pgx's default extended-protocol
// QueryExecModeCacheStatement.
//
// The trigger requires:
//
//   - a fresh testcontainer (no cached prepared statements from a prior run)
//   - pgx extended protocol (the pgxpool default)
//   - a real UPDATE landing in applyDecision (i.e. a guard decision is taken)
//
// Failure surface: the v1.6.5 binary would return a wrapped pgx error
// containing "could not determine data type of parameter $2" (or $3/$4/$5/$6).
// The v1.6.6 cast-laden query parses cleanly.
//
// Two blocks are inserted with distinct embeddings so we exercise the 'else'
// branch (needs_review or clean) — that branch had the doubly-bad mixed
// deduction (column varchar + jsonb VARIADIC any) and was the original
// crash site reported in Issue #3.
func TestRunGuardBatch_42P08_Regression(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	a := unitVec1024(0, 1.0)
	b := unitVec1024(500, 1.0) // orthogonal → clean branch (the bug's hot path)

	tEarly := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	tLate := tEarly.Add(1 * time.Hour)

	insertGuardBlock(t, pool, gP08ID, "p08-anchor", a, tEarly)
	insertGuardBlock(t, pool, gP08PeerID, "p08-peer", b, tLate)

	processed, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		// 42P08 manifests as a wrapped pgx error from tx.Exec. If anything
		// resembling the pre-fix surface appears we want a clear failure
		// message that points at the v1.6.6 cast regression.
		msg := err.Error()
		if strings.Contains(msg, "42P08") ||
			strings.Contains(msg, "42P18") ||
			strings.Contains(msg, "could not determine data type") {
			t.Fatalf("42P08 regression — v1.6.6 jsonb_build_object casts are missing or stripped: %v", err)
		}
		t.Fatalf("RunGuardBatch returned unexpected error: %v", err)
	}
	if processed != 2 {
		t.Fatalf("processed: want 2, got %d (early abort suggests internal error during applyDecision)", processed)
	}

	// Belt-and-braces: the cast fix is also what makes guard_checked_at land
	// in metadata. If the cast was right but the UPDATE silently failed,
	// metadata.guard_checked_at would be empty.
	for _, id := range []string{gP08ID, gP08PeerID} {
		s := readGuardState(t, pool, id)
		if !s.MetaCheckedSet {
			t.Errorf("block %s: metadata.guard_checked_at not set — UPDATE silently failed", id)
		}
		if s.GuardStatus == "active" {
			t.Errorf("block %s: guard_status still 'active' — applyDecision did not write", id)
		}
	}

	// Run a second batch immediately. This re-exercises any cached prepared
	// statement in pgx's per-connection cache against the same pool. With a
	// regression, the second batch would crash even if the first one (somehow)
	// passed. With the v1.6.6 fix, the cache is happy and processed=0
	// (no pending blocks remain).
	processed2, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch (2nd run, cached prepare path): %v", err)
	}
	if processed2 != 0 {
		t.Errorf("2nd run processed: want 0 (all blocks already checked), got %d", processed2)
	}
}

// TestRunGuardBatch_WriteLogPersistence asserts that every processed block
// produces a corresponding context_write_log row. The decision string in
// the log mirrors GuardResult.Decision (clean / needs_review / near_duplicate).
//
// Pre-1.6.6 the audit log step ran after applyDecision; failure modes from
// the broken UPDATE could leave write-log entries without a decision flip
// on the block, or vice versa. We assert both sides to fail loudly on either
// drift direction.
func TestRunGuardBatch_WriteLogPersistence(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	anchor := unitVec1024(0, 1.0)
	clone := unitVec1024(0, 1.0)                  // cosine 1.0 → near_duplicate
	orth := unitVec1024(500, 1.0)                 // cosine 0    → clean
	review := blendedVec1024(0, 1, 0.96, 0.28)    // cosine 0.96 → needs_review

	t0 := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	insertGuardBlock(t, pool, "019e9bbb-0000-7000-9000-000000000001", "wl-anchor", anchor, t0)
	insertGuardBlock(t, pool, "019e9bbb-0000-7000-9000-000000000002", "wl-clone", clone, t0.Add(1*time.Hour))
	insertGuardBlock(t, pool, "019e9bbb-0000-7000-9000-000000000003", "wl-orth", orth, t0.Add(2*time.Hour))
	insertGuardBlock(t, pool, "019e9bbb-0000-7000-9000-000000000004", "wl-review", review, t0.Add(3*time.Hour))

	processed, err := guard.RunGuardBatch(ctx, pool, gitSet(), 10)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != 4 {
		t.Errorf("processed: want 4, got %d", processed)
	}

	// One write-log row per processed block.
	for _, id := range []string{
		"019e9bbb-0000-7000-9000-000000000001",
		"019e9bbb-0000-7000-9000-000000000002",
		"019e9bbb-0000-7000-9000-000000000003",
		"019e9bbb-0000-7000-9000-000000000004",
	} {
		if n := countWriteLog(t, pool, id); n != 1 {
			t.Errorf("block %s: want 1 write-log row, got %d", id, n)
		}
	}

	// Spot-check: the write-log decision strings cover all three branches we
	// constructed. If one is missing, the audit trail is silently dropping
	// rows (which Welle 45's audit caught for dangling-links and is a known
	// failure mode worth pinning here too).
	expected := map[string]bool{
		"clean":          false,
		"needs_review":   false,
		"near_duplicate": false,
	}
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT decision FROM context_write_log
		 WHERE block_id IN (
			'019e9bbb-0000-7000-9000-000000000001'::uuid,
			'019e9bbb-0000-7000-9000-000000000002'::uuid,
			'019e9bbb-0000-7000-9000-000000000003'::uuid,
			'019e9bbb-0000-7000-9000-000000000004'::uuid
		 )`,
	)
	if err != nil {
		t.Fatalf("query write log decisions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan decision: %v", err)
		}
		if _, ok := expected[d]; ok {
			expected[d] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter decisions: %v", err)
	}
	for d, seen := range expected {
		if !seen {
			t.Errorf("write-log: decision %q missing — audit trail dropped a branch", d)
		}
	}
}
