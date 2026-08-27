//go:build integration

// Gate A02-3, live half: the assertions that only the real corpus can answer —
// how many roots the candidate query finds, how many parts and chunks a full
// walk yields, whether every listed part resolves, whether the part-1 invariant
// still holds and whether the watermark is still monotonic against block order.
//
// It is OPT-IN via CTX_A02_3_LIVE_DSN and skips otherwise, because a test that
// silently depends on one host's data is not a gate anybody else can run.
//
// The consequence has to be stated rather than discovered: without that
// variable these gates are neither green nor red in CI, they are ABSENT. Gates
// 2, 4, 8 and the live halves of 5 and 9 therefore do not protect the regular
// build — a later wave must not read a green pipeline as covering them.
//
// The connection is forced READ ONLY at the protocol level:
// default_transaction_read_only is set as a startup parameter, so every
// transaction from this pool refuses to write. That is an enforced property of
// the handle, not a promise about the statements below.

package ctxcheckpoint_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/distillsource/ctxcheckpoint"
)

// liveScope and liveCategory are the values the live corpus actually carries:
// all 5961 checkpoint blocks sit in scope private, category
// compaction-checkpoints (measured, single-valued in both columns).
const (
	liveScope    = "private"
	liveCategory = "compaction-checkpoints"
)

func liveReadOnlyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CTX_A02_3_LIVE_DSN")
	if dsn == "" {
		t.Skip("CTX_A02_3_LIVE_DSN not set — live corpus assertions skipped")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// Enforced, not promised: the server rejects any write from this pool.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Prove the read-only enforcement before trusting it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = pool.Exec(ctx, `CREATE TEMP TABLE a02_3_write_probe (x int)`)
	if err == nil {
		t.Fatal("read-only enforcement failed: a write succeeded on the live pool")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only probe failed with an unexpected error: %v", err)
	}
	return pool
}

func liveOpts() ctxcheckpoint.Options {
	return ctxcheckpoint.Options{
		Label:    "ctx-checkpoint",
		Scope:    liveScope,
		Category: liveCategory,
		// The horizon is off here: the gate counts the whole corpus, and a
		// 30-day cap would count the last month instead.
		MaxSessions:  100_000,
		MaxManifests: 10_000,
	}
}

// TestLiveCorpusInventory walks the entire live corpus through the reader and
// reports what it found. The numbers are MEASURED and logged; the assertions
// are the invariants, not the counts, because the corpus grows between runs.
func TestLiveCorpusInventory(t *testing.T) {
	pool := liveReadOnlyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	src, err := ctxcheckpoint.New(pool, liveOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refs, err := src.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("Sessions found no root at all — a silent null operation")
	}

	var (
		items       int
		blocks      = map[string]int{}
		withRole    int
		maxChunkIdx int
		shaInBody   int
	)
	for _, ref := range refs {
		b, err := src.Read(ctx, ref.Session, 0, 1_000_000, 4000)
		if err != nil {
			t.Fatalf("Read(%q): %v", ref.Session, err)
		}
		if !b.Complete {
			t.Errorf("Read(%q) reported Complete=false on a full walk", ref.Session)
		}
		for _, it := range b.Items {
			items++
			blocks[it.Origin.BlockID]++
			if it.Origin.Role != "" {
				withRole++
			}
			if it.Origin.ChunkIndex > maxChunkIdx {
				maxChunkIdx = it.Origin.ChunkIndex
			}

			// Boilerplate gate, over every item of the corpus.
			//
			// "Compaction source evidence" is the discriminating marker: it
			// appears ONLY in the head, so a single hit means the strip failed.
			if strings.Contains(it.Text, "Compaction source evidence") {
				t.Errorf("item from block %s carries the boilerplate marker %q",
					it.Origin.BlockID, "Compaction source evidence")
			}
			// "Transcript SHA-256" is counted rather than forbidden, because
			// the string also occurs as TRANSCRIPT CONTENT — someone talking
			// about the checkpoint mechanism. Each hit is checked below for
			// consistency with that (the string sits behind the transcript
			// marker in the source block); the check is a plausibility test,
			// not a proof, and the marker above is what actually holds the
			// gate.
			if strings.Contains(it.Text, "Transcript SHA-256") {
				shaInBody++
				assertMarkerIsContent(t, ctx, pool, it.Origin.BlockID)
			}
			// Rune cap, over every item of the corpus.
			if n := len([]rune(it.Text)); n > 4000 {
				t.Errorf("item from block %s holds %d runes, cap is 4000", it.Origin.BlockID, n)
			}
			if it.Text == "" {
				t.Errorf("item from block %s is empty", it.Origin.BlockID)
			}
			if it.Truncated {
				t.Errorf("item from block %s is marked truncated — this source splits", it.Origin.BlockID)
			}
		}
	}

	t.Logf("live inventory: %d roots, %d distinct parts, %d chunks, max chunk index %d, %d chunks (%.1f %%) carry a role",
		len(refs), len(blocks), items, maxChunkIdx, withRole, 100*float64(withRole)/float64(items))
	t.Logf("items carrying %q as transcript content: %d", "Transcript SHA-256", shaInBody)

	// The invariant behind "0 parts without a resolvable manifest": every part
	// the reader delivered came out of a manifest, so a part that resolved to
	// nothing simply produced no item. What must hold is that the walk found
	// material at all and that the chunk count exceeds the part count — the
	// latter is the difference between chunking and head capping.
	if items <= len(blocks) {
		t.Errorf("%d chunks from %d parts — chunking produced no split at all", items, len(blocks))
	}
}

// assertMarkerIsContent checks that a "Transcript SHA-256" hit is CONSISTENT
// with transcript content: the marker occurs after the transcript marker in the
// source block.
//
// It is a plausibility check, not a proof, and the difference matters: a block
// whose head leaked AND that mentions the string in its body would pass here.
// What actually carries the boilerplate gate is the unconditional ban on
// "Compaction source evidence" above — that string appears in all 5477 heads
// and in 0 bodies, so a leaked head goes red regardless of this function.
func assertMarkerIsContent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blockID string) {
	t.Helper()
	var markerPos, shaPos int
	err := pool.QueryRow(ctx, `
SELECT position('## Direct transcript' in content),
       position('Transcript SHA-256' in substr(content, position('## Direct transcript' in content)))
  FROM context_blocks WHERE id = $1::uuid`, blockID).Scan(&markerPos, &shaPos)
	if err != nil {
		t.Fatalf("verify marker origin for %s: %v", blockID, err)
	}
	if markerPos == 0 {
		t.Errorf("block %s has no transcript marker at all", blockID)
		return
	}
	if shaPos == 0 {
		t.Errorf("block %s: %q reached an item but does not occur behind the transcript marker — the head leaked",
			blockID, "Transcript SHA-256")
	}
}

// TestLiveUnresolvableListedParts counts listed part ids that no live block
// answers. The design measured 0; the assertion is that the reader NAMES the
// number rather than that the number is zero, because an unresolvable id is a
// property of the corpus and not a defect of this code.
func TestLiveUnresolvableListedParts(t *testing.T) {
	pool := liveReadOnlyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var listed, distinct, unresolvable int
	err := pool.QueryRow(ctx, `
WITH listed AS (
  SELECT (jsonb_array_elements_text(metadata->'source_block_ids'))::uuid AS pid
    FROM context_blocks
   WHERE type_name = 'checkpoint' AND scope = $1 AND category = $2
     AND NOT is_archived AND metadata ? 'source_block_ids'
)
SELECT count(*), count(DISTINCT pid),
       count(*) FILTER (WHERE NOT EXISTS (
         SELECT 1 FROM context_blocks b
          WHERE b.id = listed.pid AND b.type_name = 'checkpoint'
            AND b.scope = $1 AND NOT b.is_archived))
  FROM listed`, liveScope, liveCategory).Scan(&listed, &distinct, &unresolvable)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("listed part ids: %d total, %d distinct, %d unresolvable (%d listed more than once)",
		listed, distinct, unresolvable, listed-distinct)
	if unresolvable != 0 {
		t.Errorf("%d listed part ids do not resolve to a live block", unresolvable)
	}
}

// TestLivePartOneInvariant is the part-1 assertion: a part carrying
// metadata.part = '1' always carries a message header, which is what makes the
// carry chain start with a known attribution instead of an empty one.
func TestLivePartOneInvariant(t *testing.T) {
	pool := liveReadOnlyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var total, without int
	err := pool.QueryRow(ctx, `
SELECT count(*),
       count(*) FILTER (WHERE position('### Message ' in content) = 0)
  FROM context_blocks
 WHERE type_name = 'checkpoint' AND scope = $1 AND category = $2
   AND NOT is_archived AND metadata->>'part' = '1'`, liveScope, liveCategory).Scan(&total, &without)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("part-1 blocks: %d total, %d without a header", total, without)
	if total == 0 {
		t.Fatal("no part-1 blocks found — the invariant would be vacuous")
	}
	if without != 0 {
		t.Errorf("%d part-1 blocks carry no header — the carry chain starts unattributed", without)
	}
}

// TestLiveWatermarkMonotonic is the regression assertion behind the watermark
// derivation: over the checkpoint corpus, ORDER BY id yields the same order as
// ORDER BY created_at, id.
//
// It is a REGRESSION assertion, not a correctness precondition. Correctness
// rests on the group completeness rule, which holds whether or not this one
// does — uuidv7 is only millisecond-monotonic, so this will eventually break at
// scale and must not be what the reader depends on.
func TestLiveWatermarkMonotonic(t *testing.T) {
	pool := liveReadOnlyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var scanned, deviations int
	err := pool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE r_id <> r_ts) FROM (
  SELECT row_number() OVER (ORDER BY id) AS r_id,
         row_number() OVER (ORDER BY created_at, id) AS r_ts
    FROM context_blocks WHERE type_name = 'checkpoint'
) t`).Scan(&scanned, &deviations)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("watermark monotonicity: %d checkpoint blocks scanned, %d deviations", scanned, deviations)
	if deviations != 0 {
		t.Errorf("%d of %d blocks order differently by id than by (created_at, id)", deviations, scanned)
	}
}

// TestLiveNoSilentNullOperation is the gate against the failure mode that has
// no error message: the reader finds nothing while checkpoint material exists
// in a scope it was not pointed at. A reader in that state journals no_new_rows
// forever and looks healthy doing it.
func TestLiveNoSilentNullOperation(t *testing.T) {
	pool := liveReadOnlyPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	src, err := ctxcheckpoint.New(pool, liveOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refs, err := src.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}

	var elsewhere int
	var scopes []string
	rows, err := pool.Query(ctx, `
SELECT scope, count(*) FROM context_blocks
 WHERE type_name = 'checkpoint' AND NOT is_archived AND scope <> $1
 GROUP BY 1 ORDER BY 2 DESC`, liveScope)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var n int
		if err := rows.Scan(&scope, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		scopes = append(scopes, scope)
		elsewhere += n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	t.Logf("candidate roots in scope %q: %d; checkpoint blocks in other scopes: %d %v",
		liveScope, len(refs), elsewhere, scopes)
	if len(refs) == 0 && elsewhere > 0 {
		t.Errorf("reader found 0 roots in scope %q while %d checkpoint blocks live in %v — silent null operation",
			liveScope, elsewhere, scopes)
	}
}
