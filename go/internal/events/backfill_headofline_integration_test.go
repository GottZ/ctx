//go:build integration

// Rot-Gate 1 for the Evokoa-Clean-Room-Plan Achse 04 W04-2 (design/04 §4.4,
// §7 wave row W04-2): the head-of-line class of Vorfall 2026-07-10
// (docs/operations.md "Backfill head-of-line caveat") — the scheduler's
// embed-backfill retried a NULL-embedding block INDEFINITELY once it
// exceeded the embed backend's slot window (no memo, no skip), starving
// every younger pending block behind it.
//
// This file has TWO halves:
//
//  1. red_repro_without_memo_mechanism reconstructs the EXACT pre-W04-2
//     shape — the peek/pick SQL with NO context_embed_failures exclusion, the
//     literal query text backfillOneEmbedding ran before this wave (see the
//     migration-109-era scheduler.go — quoted verbatim below) — and proves
//     it directly: with nothing recording the failed attempt, two
//     consecutive peeks return the SAME oldest (unembeddable) block; the
//     younger pending block is never reached. This needs no production code
//     from this wave (self-contained old-shape SQL, same methodology as the
//     qi3 file's probe_sees_old_shape fixture in this package) — it runs
//     identically whether or not the fix below is applied, which is exactly
//     what "the probe CAN see the hazard" needs to mean.
//  2. TestBackfillHeadOfLine_Integration/fixed_two_cycles drives the ACTUAL
//     (post-fix) s.backfillOneEmbedding across two real cycles against a
//     fake embed backend that 400s a "oversize" block and 200s a "normal"
//     one: cycle 1 writes the failure memo (last_class=oversize,
//     next_attempt_at=infinity) and reports no block backfilled; cycle 2's
//     peek is now excluded from the oversize block and picks the younger
//     block instead — embedding it. The old block stays parked (memoized,
//     visible), never silently dropped, never retried forever.
//
// Run: go test -tags=integration ./internal/events/ -run TestBackfillHeadOfLine_Integration -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/testdb"
)

// oversizeMarker appears in the "oversize" fixture's embed input so the fake
// backend can tell it apart from the "normal" fixture without depending on
// any token-estimate logic (the RED subtest below runs with zero production
// code from this wave, so it cannot rely on a config knob that doesn't yet
// exist in the pre-fix world it emulates).
const oversizeMarker = "OVERSIZE_MARKER_W04_2"

// red_repro_without_memo_mechanism is the Rot-Gate 1 proof. It literally
// reproduces the pre-W04-2 peek/pick query shape (scheduler.go before this
// wave: `SELECT ... FROM context_blocks WHERE embedding IS NULL AND NOT
// is_archived ORDER BY created_at ASC LIMIT 1` — no exclusion clause,
// because context_embed_failures did not exist) and the pre-W04-2 failure
// handling (nothing written to the DB on a failed embed — the old
// backfillOneEmbedding returned an error and the deferred tx.Rollback
// discarded everything). Ist-Verhalten: two consecutive peeks/picks return
// the SAME oldest block; the younger pending block is never reached.
func TestBackfillHeadOfLine_RedRepro_OldShape(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedPendingBlock(t, pool, "red-old-oversize", 2*time.Hour)
	seedPendingBlock(t, pool, "red-young-normal", time.Hour)

	// pre-W04-2 peek — verbatim shape, no failure-exclusion clause.
	oldShapePeek := func() string {
		var title string
		if err := pool.QueryRow(ctx,
			`SELECT title FROM context_blocks
			 WHERE embedding IS NULL AND NOT is_archived
			 ORDER BY created_at ASC
			 LIMIT 1`).Scan(&title); err != nil {
			t.Fatalf("old-shape peek: %v", err)
		}
		return title
	}

	// Cycle 1: peek picks the oldest block (the oversize one). The pre-W04-2
	// embed attempt fails and — critically — nothing is written to the DB
	// (no memo table existed): context_blocks is byte-identical afterwards.
	cycle1 := oldShapePeek()
	if cycle1 != "red-old-oversize" {
		t.Fatalf("cycle 1 picked %q, want %q (oldest-first)", cycle1, "red-old-oversize")
	}

	// Cycle 2: nothing in the DB changed since cycle 1 (old code recorded no
	// failure), so the SAME query returns the SAME row again — the
	// younger, perfectly embeddable block never gets a turn. This is the
	// Vorfall-2026-07-10 head-of-line hazard, reproduced directly.
	cycle2 := oldShapePeek()
	if cycle2 != "red-old-oversize" {
		t.Fatalf("cycle 2 picked %q, want %q (RED premise: old shape re-picks the same block)", cycle2, "red-old-oversize")
	}
	t.Logf("RED confirmed: cycles 1 and 2 both picked %q — %q was never reached", cycle1, "red-young-normal")
}

// headOfLineEmbedServer serves the ollama /api/embed wire shape: 400 with an
// exceed_context_size-bearing body when the input carries oversizeMarker,
// 200 with a quality-gate-passing vector otherwise.
func headOfLineEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Input, oversizeMarker) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"exceed_context_size_error","message":"input exceeds the configured context size"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings":        [][]float64{vec},
			"prompt_eval_count": 3,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// headOfLineCfg carries an EmbedBackfill surface tuned for this test: the
// pre-wire token estimate is DISABLED (MaxTokens<=0) so the 400 genuinely
// comes off the wire (the Vorfall-2026-07-10 repro is a WIRE rejection, not
// a pre-check skip — the pre-check is a separate, additional safety net
// covered by the store-level integration test), and the backoff window is
// generously larger than this test's wall-clock runtime so the memoized
// block stays excluded between cycle 1 and cycle 2.
func headOfLineCfg() *config.Config {
	return &config.Config{
		EmbedBackfill: config.EmbedBackfillConfig{
			MaxTokens:   0,
			BackoffBase: time.Hour,
			BackoffCap:  24 * time.Hour,
		},
	}
}

// TestBackfillHeadOfLine_Integration is the GREEN half: the actual
// (post-fix) backfillOneEmbedding across two real cycles.
func TestBackfillHeadOfLine_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	t.Run("fixed_two_cycles", func(t *testing.T) {
		seedPendingBlock(t, pool, "fixed-old-oversize "+oversizeMarker, 2*time.Hour)
		seedPendingBlock(t, pool, "fixed-young-normal", time.Hour)
		defer clearBlocks(t, pool)

		srv := headOfLineEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings()) // empty policy: pass-through, no slot limits
		t.Cleanup(d.Close)
		router := backfillRouter(bpool, d)
		cfg := headOfLineCfg()

		// Cycle 1: peek picks the OLDER block (oversize) — the 400 comes
		// back off the wire, gets classified 'oversize', and the memo is
		// written+committed (next_attempt_at='infinity'). No block is
		// reported backfilled this cycle.
		ok, err := s.backfillOneEmbedding(ctx, router, cfg)
		if err != nil {
			t.Fatalf("cycle 1: unexpected error %v", err)
		}
		if ok {
			t.Fatalf("cycle 1: backfilled=true, want false (the oversize block must NOT count as backfilled)")
		}

		var lastClass string
		var isInfinity bool
		if err := pool.QueryRow(ctx,
			`SELECT f.last_class, f.next_attempt_at = 'infinity'
			 FROM context_embed_failures f
			 JOIN context_blocks cb ON cb.id = f.block_id
			 WHERE cb.title = $1`, "fixed-old-oversize "+oversizeMarker).
			Scan(&lastClass, &isInfinity); err != nil {
			t.Fatalf("cycle 1: read failure memo: %v", err)
		}
		if lastClass != "oversize" {
			t.Errorf("cycle 1: last_class = %q, want %q", lastClass, "oversize")
		}
		if !isInfinity {
			t.Errorf("cycle 1: next_attempt_at is not infinity for the oversize memo")
		}
		if got := pendingCount(t, pool); got != 2 {
			t.Fatalf("cycle 1: pending = %d, want 2 (nothing embedded, oversize block still NULL but memoized)", got)
		}

		// Cycle 2: the oversize block is now excluded by the memo — the
		// peek reaches the YOUNGER block instead and embeds it. This is the
		// structural fix: the next PEEK, not a Blind-Retry of the same row.
		ok, err = s.backfillOneEmbedding(ctx, router, cfg)
		if err != nil {
			t.Fatalf("cycle 2: unexpected error %v", err)
		}
		if !ok {
			t.Fatalf("cycle 2: backfilled=false, want true (the younger block must now get its turn)")
		}

		var youngEmbedded, oldEmbedded bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = $1`, "fixed-young-normal").
			Scan(&youngEmbedded); err != nil {
			t.Fatalf("read young block: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = $1`, "fixed-old-oversize "+oversizeMarker).
			Scan(&oldEmbedded); err != nil {
			t.Fatalf("read old block: %v", err)
		}
		if !youngEmbedded {
			t.Errorf("cycle 2: younger block was NOT embedded — head-of-line still blocking")
		}
		if oldEmbedded {
			t.Errorf("cycle 2: oversize block got an embedding — it should stay parked (never wired successfully)")
		}
		if got := pendingCount(t, pool); got != 1 {
			t.Errorf("cycle 2: pending = %d, want 1 (only the parked oversize block remains)", got)
		}
	})
}
