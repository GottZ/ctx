//go:build integration

// guard_review.oldest_updated_at is the aging signal of the review queue: how
// long has its head been waiting for a decision. This file pins where that
// time comes from, end to end, with the guard that produces it:
//
//   - a guard pass that flags a block leaves the block's updated_at alone, and
//     the section still reports the time of the FLAG, not the age of the
//     content;
//   - a later write that leaves the content alone (tags) moves updated_at and
//     keeps the flag, and the queue's age stays at the flag;
//   - a flagged row that carries no guard_checked_at (rows older than the key,
//     repair migrations) or an unreadable one falls back to updated_at, and one
//     such row never takes the section down for everybody else;
//   - guard_checked_at is metadata a caller can write. A value that cannot be
//     a flag time of the row (infinite, before the row existed, in the future)
//     falls back to updated_at too.
//
// Run:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestGuardReviewAging -count=1 -v

package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// agingVec builds a 1024-dim vector with up to two components set.
func agingVec(aIdx int, aW float32, bIdx int, bW float32) []float32 {
	v := make([]float32, 1024)
	v[aIdx] = aW
	if bW != 0 {
		v[bIdx] = bW
	}
	return v
}

// writeAgingBlock creates a block through the production write path and gives
// it a vector the way the embed worker does.
func writeAgingBlock(t *testing.T, pool *pgxpool.Pool, scope, title string, embedding []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "learnings", title, "guard aging body for "+title, nil, nil,
		scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("upsert %q: %v", title, err)
	}
	if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-aging-model", embedding); err != nil {
		t.Fatalf("StoreEmbedding %q: %v", title, err)
	}
	return b.ID
}

// TestGuardReviewAgingFollowsTheFlag: content written long ago, flagged by the
// guard just now. The block keeps its updated_at, and the queue's age starts
// at the flag.
func TestGuardReviewAgingFollowsTheFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "ga-flag"
	seedGuardScope(t, pool, scope, nil)

	// A pair at cosine 0.96: the guard flags both as needs_review.
	ids := []string{
		writeAgingBlock(t, pool, scope, "ga-anchor", agingVec(0, 1.0, 0, 0)),
		writeAgingBlock(t, pool, scope, "ga-peer", agingVec(0, 0.96, 1, 0.28)),
	}

	// The content is old: move the last write a long way back. Only the
	// timestamp is set by hand; the rows themselves came from the write path.
	written := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET updated_at = $1 WHERE id = ANY($2::uuid[])`, written, ids); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	passStart := time.Now().UTC().Truncate(time.Second)
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10, config.Defaults().GuardSearch())
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}

	var oldestFlag time.Time
	for _, id := range ids {
		var status, checkedAt string
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, COALESCE(metadata->>'guard_checked_at', ''), updated_at
			   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&status, &checkedAt, &updatedAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != "needs_review" {
			t.Fatalf("block %s: guard_status = %q, want needs_review (the fixture did not reach the flagged state)", id, status)
		}
		if !updatedAt.Equal(written) {
			t.Errorf("block %s: the guard pass moved updated_at from %s to %s; flagging a block is not a write to it",
				id, written.Format(time.RFC3339), updatedAt.Format(time.RFC3339Nano))
		}
		flag, err := time.Parse(time.RFC3339, checkedAt)
		if err != nil {
			t.Fatalf("block %s: guard_checked_at %q is not RFC 3339: %v", id, checkedAt, err)
		}
		if oldestFlag.IsZero() || flag.Before(oldestFlag) {
			oldestFlag = flag
		}
	}

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil")
	}
	for name, row := range map[string]*guardReviewStatus{"scope": gen.forScope(scope), "global": gen.globalRow()} {
		if row == nil {
			t.Fatalf("%s row missing", name)
		}
		if row.NeedsReview != 2 {
			t.Errorf("%s row: needs_review = %d, want 2", name, row.NeedsReview)
		}
		if row.OldestUpdatedAt == nil {
			t.Fatalf("%s row: oldest_updated_at is null with two flagged blocks", name)
		}
		got := *row.OldestUpdatedAt
		if got.Before(passStart) {
			t.Errorf("%s row: oldest_updated_at = %s is before the guard pass (%s); it reports the age of the content, not the time in the queue",
				name, got.Format(time.RFC3339Nano), passStart.Format(time.RFC3339))
		}
		if !got.Equal(oldestFlag) {
			t.Errorf("%s row: oldest_updated_at = %s, want the oldest guard_checked_at %s",
				name, got.Format(time.RFC3339Nano), oldestFlag.Format(time.RFC3339))
		}
	}

	// A write that leaves the content alone keeps the flag and moves
	// updated_at past it. The queue's age must not restart with that write.
	for _, id := range ids {
		if _, _, err := store.UpdateBlock(ctx, pool, id,
			store.UpdateBlockData{Tags: []string{"ga-retagged"}}, []string{scope}); err != nil {
			t.Fatalf("retag %s: %v", id, err)
		}
		var checkedAt string
		var updatedAt time.Time
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(metadata->>'guard_checked_at', ''), updated_at
			   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&checkedAt, &updatedAt); err != nil {
			t.Fatalf("read %s after retag: %v", id, err)
		}
		if checkedAt == "" || !updatedAt.After(written) {
			t.Fatalf("block %s after retag: guard_checked_at %q, updated_at %s (the fixture did not reach a flagged row with a later write)",
				id, checkedAt, updatedAt.Format(time.RFC3339Nano))
		}
	}
	gen = buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil after the retag")
	}
	for name, row := range map[string]*guardReviewStatus{"scope": gen.forScope(scope), "global": gen.globalRow()} {
		if row == nil || row.OldestUpdatedAt == nil {
			t.Fatalf("%s row after retag: no oldest_updated_at (row %+v)", name, row)
		}
		if !row.OldestUpdatedAt.Equal(oldestFlag) {
			t.Errorf("%s row after retag: oldest_updated_at = %s, want the flag %s; a tags write restarted the queue's age",
				name, row.OldestUpdatedAt.Format(time.RFC3339Nano), oldestFlag.Format(time.RFC3339))
		}
	}
}

// TestGuardReviewAgingFallsBackToUpdatedAt: flagged rows the guard's stamp
// cannot be read from, the way they exist in a database that predates the key
// or was hand-repaired. They are inserted directly: the guard itself always
// writes RFC 3339, and a row older than the key has no write path any more.
func TestGuardReviewAgingFallsBackToUpdatedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Every time is counted back from now: a stamp is only taken when it lies
	// between the row's creation and now. created is before all of them.
	base := time.Now().UTC().Truncate(time.Second)
	created := base.Add(-365 * 24 * time.Hour)
	insert := func(scope, title, metadata string, updatedAt time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, guard_status, metadata, created_at, updated_at)
			 VALUES ('reference', $1, 'c', $2, 'needs_review', $3::jsonb, $4, $5)`,
			title, scope, metadata, created, updatedAt); err != nil {
			t.Fatalf("insert %s: %v", title, err)
		}
	}

	noKey := base.Add(-90 * 24 * time.Hour)
	unreadable := base.Add(-60 * 24 * time.Hour)
	stamped := base.Add(-30 * 24 * time.Hour)
	stampedMeta := `{"guard_checked_at": "` + stamped.Format(time.RFC3339) + `"}`

	seedGuardScope(t, pool, "ga-nokey", nil)
	insert("ga-nokey", "ga-nokey-block", `{}`, noKey)

	seedGuardScope(t, pool, "ga-unreadable", nil)
	insert("ga-unreadable", "ga-unreadable-block", `{"guard_checked_at": "not a timestamp"}`, unreadable)
	insert("ga-unreadable", "ga-unreadable-empty", `{"guard_checked_at": ""}`, unreadable.Add(time.Hour))

	// A stamped row whose last write is OLDER than its flag: the stamp wins.
	seedGuardScope(t, pool, "ga-stamped", nil)
	insert("ga-stamped", "ga-stamped-block", stampedMeta, noKey.Add(-24*time.Hour))

	// A stamped row whose last write is NEWER than its flag (a tags or
	// metadata write on a flagged block): the stamp still wins.
	seedGuardScope(t, pool, "ga-rewritten", nil)
	insert("ga-rewritten", "ga-rewritten-block", stampedMeta, base.Add(-24*time.Hour))

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil: one unreadable guard_checked_at took the whole section down")
	}
	for scope, want := range map[string]time.Time{
		"ga-nokey":      noKey,
		"ga-unreadable": unreadable,
		"ga-stamped":    stamped,
		"ga-rewritten":  stamped,
	} {
		row := gen.forScope(scope)
		if row == nil || row.OldestUpdatedAt == nil {
			t.Errorf("scope %s: no oldest_updated_at (row %+v)", scope, row)
			continue
		}
		if !row.OldestUpdatedAt.Equal(want) {
			t.Errorf("scope %s: oldest_updated_at = %s, want %s", scope,
				row.OldestUpdatedAt.Format(time.RFC3339Nano), want.Format(time.RFC3339))
		}
	}
	// The grand total takes the minimum over the same per-row time.
	if g := gen.globalRow(); g == nil || g.OldestUpdatedAt == nil || !g.OldestUpdatedAt.Equal(noKey) {
		t.Errorf("global oldest_updated_at = %+v, want %s", g, noKey.Format(time.RFC3339))
	}
}

// TestGuardReviewAgingIgnoresAStampOutsideTheRowsLifetime: a database written
// before the write path kept guard_checked_at to the guard (issue #238) holds
// whatever its callers put there. A value that cannot be the flag time of its
// row must not reach the aggregate: an infinite one does not scan and would
// take the section away from every scope, a finite one far in the past or in
// the future would set the age of the whole queue, grand total included.
//
// Each forged stamp sits in a scope of its own, so each bound is needed by the
// row of its own scope: in a scope with other rows the minimum would hide a
// future stamp (issue #238 §2). The rows are flagged by the guard; the stamps
// are written with SQL, as no write path produces them any more.
func TestGuardReviewAgingIgnoresAStampOutsideTheRowsLifetime(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	forged := map[string]string{
		"ga-f-neginf": "-infinity",
		"ga-f-posinf": "infinity",
		"ga-f-past":   "1900-01-01T00:00:00Z",
		"ga-f-future": "2999-01-01T00:00:00Z",
	}
	want := map[string]time.Time{}
	axis := 0
	for scope, stamp := range forged {
		seedGuardScope(t, pool, scope, nil)
		ids := []string{
			writeAgingBlock(t, pool, scope, scope+"-a", agingVec(axis, 1.0, 0, 0)),
			writeAgingBlock(t, pool, scope, scope+"-b", agingVec(axis, 0.96, axis+1, 0.28)),
		}
		axis += 2
		processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 10, config.Defaults().GuardSearch())
		if err != nil {
			t.Fatalf("RunGuardBatch: %v", err)
		}
		if processed != len(ids) {
			t.Fatalf("scope %s: processed %d, want %d", scope, processed, len(ids))
		}
		// Both rows of the scope carry the forged stamp.
		var oldest time.Time
		for _, id := range ids {
			var status string
			var updatedAt time.Time
			if err := pool.QueryRow(ctx,
				`UPDATE context_blocks SET metadata = jsonb_set(metadata, '{guard_checked_at}', to_jsonb($2::text))
				  WHERE id = $1::uuid RETURNING guard_status, updated_at`, id, stamp).Scan(&status, &updatedAt); err != nil {
				t.Fatalf("forge %s: %v", id, err)
			}
			if status != "needs_review" {
				t.Fatalf("block %s: guard_status %q, want needs_review (the fixture did not reach the flagged state)", id, status)
			}
			if oldest.IsZero() || updatedAt.Before(oldest) {
				oldest = updatedAt
			}
		}
		want[scope] = oldest
	}

	gen := buildGuardReviewGeneration(ctx, pool)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil: a forged guard_checked_at took the section down for every scope")
	}
	var globalWant time.Time
	for scope, w := range want {
		row := gen.forScope(scope)
		if row == nil || row.NeedsReview != 2 || row.OldestUpdatedAt == nil {
			t.Fatalf("scope %s (stamp %s): row %+v, want needs_review 2 and an oldest_updated_at", scope, forged[scope], row)
		}
		if !row.OldestUpdatedAt.Equal(w) {
			t.Errorf("scope %s (stamp %s): oldest_updated_at = %s, want the oldest updated_at %s (the stamp cannot be a flag time of its row)",
				scope, forged[scope], row.OldestUpdatedAt.Format(time.RFC3339Nano), w.Format(time.RFC3339Nano))
		}
		if globalWant.IsZero() || w.Before(globalWant) {
			globalWant = w
		}
	}
	if g := gen.globalRow(); g == nil || g.OldestUpdatedAt == nil || !g.OldestUpdatedAt.Equal(globalWant) {
		t.Errorf("global oldest_updated_at = %+v, want %s", g, globalWant.Format(time.RFC3339Nano))
	}
}

// laggingClockPool returns a pool on the database of pool whose clock runs the
// interval lag behind the application's, in the session time zone
// Asia/Kathmandu (UTC+05:45): now() and statement_timestamp() resolve to
// functions of a schema ahead of pg_catalog in the search path (pg_catalog is
// named last, so its functions no longer win) that subtract lag. Column
// defaults keep the real clock — they were bound when the table was created.
func laggingClockPool(t *testing.T, pool *pgxpool.Pool, lag string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	for _, st := range []string{
		`CREATE SCHEMA IF NOT EXISTS ctx_lagging_clock`,
		`CREATE OR REPLACE FUNCTION ctx_lagging_clock.now() RETURNS timestamptz LANGUAGE sql STABLE
		   AS $$ SELECT pg_catalog.now() - '` + lag + `'::interval $$`,
		`CREATE OR REPLACE FUNCTION ctx_lagging_clock.statement_timestamp() RETURNS timestamptz LANGUAGE sql STABLE
		   AS $$ SELECT pg_catalog.statement_timestamp() - '` + lag + `'::interval $$`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("lagging clock: %v", err)
		}
	}
	// A session time zone far from UTC as well: the stamp is RFC 3339 UTC
	// whatever the session's zone is.
	p := testdb.WithSessionSettings(t, pool, map[string]string{
		"search_path": "ctx_lagging_clock, public, pg_catalog",
		"TimeZone":    "Asia/Kathmandu",
	})
	var lagging, real time.Time
	if err := p.QueryRow(ctx, `SELECT now(), pg_catalog.now()`).Scan(&lagging, &real); err != nil {
		t.Fatalf("lagging clock: %v", err)
	}
	if !lagging.Before(real) {
		t.Fatalf("lagging clock: now() = %s is not behind pg_catalog.now() = %s", lagging, real)
	}
	return p
}

// TestGuardReviewAgingWithTheAppClockAhead: the application's clock runs five
// minutes ahead of the database's (issue #238 §4). A block the guard flags
// just now ages from its flag at once: the stamp is the database's time of the
// verdict, which the readers bound by their now(), and it is written in the
// RFC 3339 UTC form the Go clock wrote before (GuardList hands it to clients
// as checked_at). With the stamp from the application's clock the row fell
// back to its updated_at for the length of the skew.
//
// The database's clock is the one injected (laggingClockPool), as the
// application has no clock of its own in the verdict any more. The blocks are
// written through the production path; their creation and last write are
// moved back by hand, so the stamp of the lagging clock lies after the row's
// creation, as it does when the two clocks agree.
func TestGuardReviewAgingWithTheAppClockAhead(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "ga-skew"
	seedGuardScope(t, pool, scope, nil)
	ids := []string{
		writeAgingBlock(t, pool, scope, "ga-skew-a", agingVec(0, 1.0, 0, 0)),
		writeAgingBlock(t, pool, scope, "ga-skew-b", agingVec(0, 0.96, 1, 0.28)),
	}
	written := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET created_at = $1, updated_at = $1 WHERE id = ANY($2::uuid[])`, written, ids); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	db := laggingClockPool(t, pool, "5 minutes")
	var before, after time.Time
	if err := db.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&before); err != nil {
		t.Fatalf("clock before: %v", err)
	}
	processed, err := guard.RunGuardBatch(ctx, db, blocktype.NewRegistry().Snapshot(), 10, config.Defaults().GuardSearch())
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed: want %d, got %d", len(ids), processed)
	}
	if err := db.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&after); err != nil {
		t.Fatalf("clock after: %v", err)
	}

	var oldestFlag time.Time
	for _, id := range ids {
		var status, stamp string
		if err := pool.QueryRow(ctx,
			`SELECT guard_status, COALESCE(metadata->>'guard_checked_at', '') FROM context_blocks WHERE id = $1::uuid`,
			id).Scan(&status, &stamp); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != "needs_review" {
			t.Fatalf("block %s: guard_status %q, want needs_review (the fixture did not reach the flagged state)", id, status)
		}
		flag, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			t.Fatalf("block %s: guard_checked_at %q is not RFC 3339: %v", id, stamp, err)
		}
		if again := flag.UTC().Format(time.RFC3339); again != stamp {
			t.Errorf("block %s: guard_checked_at %q is not in the form the Go clock wrote (%q)", id, stamp, again)
		}
		if flag.Before(before.Truncate(time.Second)) || flag.After(after) {
			t.Errorf("block %s: guard_checked_at %s is not the database's time of the verdict (between %s and %s)",
				id, stamp, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
		}
		if oldestFlag.IsZero() || flag.Before(oldestFlag) {
			oldestFlag = flag
		}
	}

	gen := buildGuardReviewGeneration(ctx, db)
	if gen == nil {
		t.Fatal("buildGuardReviewGeneration returned nil")
	}
	for name, row := range map[string]*guardReviewStatus{"scope": gen.forScope(scope), "global": gen.globalRow()} {
		if row == nil || row.OldestUpdatedAt == nil {
			t.Fatalf("%s row: no oldest_updated_at (row %+v)", name, row)
		}
		if !row.OldestUpdatedAt.Equal(oldestFlag) {
			t.Errorf("%s row: oldest_updated_at = %s, want the flag %s; a fresh flag fell back to updated_at (%s) because its stamp is ahead of the database's now()",
				name, row.OldestUpdatedAt.Format(time.RFC3339Nano), oldestFlag.Format(time.RFC3339), written.Format(time.RFC3339Nano))
		}
	}
}

// toastedFlaggedPairs writes n pairs of blocks at cosine 0.96 on disjoint axes
// through the production path, each with ~8 KB of incompressible caller
// metadata, which PostgreSQL stores out of line (TOAST), and lets the guard
// flag them: 2n needs_review rows whose metadata document lives in TOAST.
func toastedFlaggedPairs(t *testing.T, pool *pgxpool.Pool, scope string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		for j, vec := range [][]float32{agingVec(2*i, 1.0, 0, 0), agingVec(2*i, 0.96, 2*i+1, 0.28)} {
			raw := make([]byte, 6000)
			if _, err := rand.Read(raw); err != nil {
				t.Fatalf("random: %v", err)
			}
			title := fmt.Sprintf("%s-%02d-%d", scope, i, j)
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "toasted guard body "+title, nil,
				map[string]any{"blob": base64.StdEncoding.EncodeToString(raw)},
				scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("upsert %s: %v", title, err)
			}
			if err := store.StoreEmbedding(ctx, pool, b.ID, "guard-aging-model", vec); err != nil {
				t.Fatalf("StoreEmbedding %s: %v", title, err)
			}
		}
	}
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 4*n+10, config.Defaults().GuardSearch())
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != 2*n {
		t.Fatalf("processed %d, want %d", processed, 2*n)
	}
}

// TestGuardReviewRollupReadsTheStampOncePerRow pins the cost of the flag time
// on the /api/status rollup (issue #238 §1) in buffers, the exact measure: a
// flagged row whose metadata lives out of line costs the rollup what ONE read
// of the key costs, not one read per reference to it (five on the base).
//
// Three statements over the same flagged set and the same scan: the rollup
// itself (guardGenRollupSQL), the rollup as it was before the flag time read
// metadata at all (min(updated_at): no metadata read), and that one with
// exactly one read of the key per row added. The buffers the rollup spends on
// the key (rollup − none) must not exceed those of one read (one − none),
// with a slack of a quarter of a buffer per out-of-line row; a second read
// per row costs at least one more buffer per such row. Both deployment
// values of random_page_cost.
func TestGuardReviewRollupReadsTheStampOncePerRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope, pairs = "ga-toast", 20
	toastedFlaggedPairs(t, pool, scope, pairs)
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	const flagged = `FROM context_blocks
		WHERE NOT is_archived AND guard_status IN ('needs_review', 'near_duplicate', 'possible_duplicate')
		GROUP BY ROLLUP (scope)`
	const counts = `(count(*) FILTER (WHERE guard_status = 'needs_review'))::int,
		(count(*) FILTER (WHERE guard_status = 'near_duplicate'))::int,
		(count(*) FILTER (WHERE guard_status = 'possible_duplicate'))::int`
	none := `SELECT scope, GROUPING(scope), ` + counts + `, min(updated_at) ` + flagged
	one := `SELECT scope, GROUPING(scope), ` + counts + `, min(updated_at), count(metadata->>'guard_checked_at') ` + flagged
	rowsOutOfLine := int64(2 * pairs)
	for _, rpc := range testdb.RandomPageCosts {
		p := testdb.WithRandomPageCost(t, pool, rpc)
		bNone := testdb.ExplainCustom(t, p, none).Buffers
		bOne := testdb.ExplainCustom(t, p, one).Buffers
		bRollup := testdb.ExplainCustom(t, p, guardGenRollupSQL).Buffers
		t.Logf("random_page_cost %s: buffers none %d, one read %d, rollup %d (%d rows out of line)", rpc, bNone, bOne, bRollup, rowsOutOfLine)
		if bOne-bNone < rowsOutOfLine {
			t.Fatalf("random_page_cost %s: one read of the key costs %d buffers over none for %d rows; the fixture's metadata is not out of line",
				rpc, bOne-bNone, rowsOutOfLine)
		}
		if bRollup-bNone > bOne-bNone+rowsOutOfLine/4 {
			t.Errorf("random_page_cost %s: the rollup spends %d buffers on the flag time, one read of the key per row costs %d: the key is read more than once per row",
				rpc, bRollup-bNone, bOne-bNone)
		}
	}
}
