//go:build integration

// Wave W-L2 — the rollover of the shard layout (amendment C4-2,
// design/02-destillat-arm.md §A.1, §A.3 b, §A.4 c/d, wave gate §A.6 "W-L2").
//
// WHAT THIS FILE PROBES. W-L1 gave the identity its capacity axis but left the
// deadlock of §A.1 standing: a full shard ended the run with skipped/budget,
// the watermark stayed put, and the same title came back on every tick. This
// wave turns that point into a HANDOVER — the run opens shard n+1 — and the
// probes below are the amendment's own gate list, in its own order.
//
// NO REAL LLM CALL: the stub sits behind the backend seam exactly as in A02-8,
// A02-9 and W-L1, so everything above it is production code. What is faked is
// the model, never the pipeline. No docker against the live system or the
// measure copy; every fixture goes down through the production write path.
//
//	go test -tags=integration ./internal/events/ -run TestDistillShardRollover -count=1 -v
package events

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
)

// wl2Opts is wl1Opts with a steerable cap — the one key this wave's probes vary.
func wl2Opts(maxRunes int) distillWriteOpts {
	o := wl1Opts()
	o.maxRunes = maxRunes
	return o
}

// wl2Stub is a9Stub with ONE change, and it is what makes the material-fidelity
// probe measure the arm instead of the fixture.
//
// a9Stub builds its claim from the address as the PROMPT spells it, and the
// prompt numbers its blocks LOCALLY (block="1", block="2" — promptguard's own
// numbering per call). Under a cap the arm groups its calls differently than it
// does without one, so the same corpus chunk reaches the model under a different
// local number and a9Stub answers a different claim STRING for identical
// material. Measured: 16 distinct claim lines over the fixture's 12 chunks.
//
// The chunk number in the prompt is the CORPUS chunk index (Origin.ChunkIndex,
// visible in every rendered anchor), so a claim built from it alone is a
// function of the material and of nothing else — which is the precondition for
// comparing two runs line by line.
func wl2Stub(req a8Request) (string, int) {
	addrs := a8Addrs(req.User)
	ins := make([]map[string]any, 0, len(addrs))
	for _, a := range addrs {
		q := a8QuoteFrom(req.User, a)
		if q == "" {
			continue
		}
		ins = append(ins, map[string]any{
			"claim": a8Claim + " Abschnitt " + strconv.Itoa(a.chunk),
			"quote": q, "block": a.block, "chunk": a.chunk, "kind": "finding",
		})
	}
	return a8Answer(ins...), http.StatusOK
}

// wl2Run drives ONE tick against the stub with a steerable
// distill.max_block_runes. 0 is the key's own off-switch ("no cap"), which is
// what makes the one-block baseline of the material-fidelity probe reachable
// without a second code path.
func wl2Run(t *testing.T, pool *pgxpool.Pool, src distillsource.Source, maxRunes int) *a8Stub {
	t.Helper()
	return wl2RunCtx(t, pool, context.Background(), src, maxRunes)
}

// wl2RunCtx is wl2Run with the caller's context — the progress probe needs a
// deadline, because a version WITHOUT the A.4 (c) condition does not terminate
// and a probe that hangs proves nothing.
func wl2RunCtx(t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	src distillsource.Source, maxRunes int,
) *a8Stub {
	t.Helper()
	stub := a8NewStub(t, wl2Stub)
	cfg := a8Config()
	cfg.Distill.MaxBlockRunes = maxRunes
	s := a8Scheduler(pool, cfg, src, a8Pool(stub.srv.URL))
	s.distillOnce(ctx, dfNoDemand)
	return stub
}

// wl2Shards answers the range's blocks in shard order, together with the
// rendered claim lines each one carries.
type wl2Shard struct {
	ordinal int
	title   string
	claims  []string
}

func wl2Shards(t *testing.T, pool *pgxpool.Pool, root string) []wl2Shard {
	t.Helper()
	var out []wl2Shard
	for _, b := range a9Blocks(t, pool) {
		n, ok := distillShardOrdinal(root, 0, b.title)
		if !ok {
			continue
		}
		c, split := distillSplitCarry(b.content)
		if !split {
			t.Fatalf("the arm cannot read back its own block %q", b.title)
		}
		out = append(out, wl2Shard{ordinal: n, title: b.title, claims: c.claims})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ordinal < out[j].ordinal })
	return out
}

// wl2Claims is the MULTISET of rendered claim lines over every shard of the
// range — the measurement §A.6's material-fidelity probe names by name ("gemessen
// als Zeilen-Multimenge über die gerenderten Claim-Zeilen").
func wl2Claims(t *testing.T, pool *pgxpool.Pool, root string) []string {
	t.Helper()
	var out []string
	for _, s := range wl2Shards(t, pool, root) {
		out = append(out, s.claims...)
	}
	sort.Strings(out)
	return out
}

// wl2Journal reads the newest run row's counters.
type wl2Row struct {
	outcome, skipReason string
	calls, dup, cred    int
	wmTo                int64
	blocks              int
}

// wl2Totals is the journal over ALL ticks of one source — the shape the red
// gate is written in ("über N Ticks ... calls = 0, das Wasserzeichen steht").
// The newest row alone would answer the wrong question: after a run that
// covered its range the next tick answers skipped/no_new_rows, and that row
// carries calls = 0 for a reason that has nothing to do with the cap.
type wl2Totals struct {
	calls        int
	wmTo         int64
	budgetSkips  int
	rows, blocks int
}

func wl2Sum(t *testing.T, pool *pgxpool.Pool, key string) wl2Totals {
	t.Helper()
	var s wl2Totals
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(sum(calls),0), COALESCE(max(watermark_to),0),
		       count(*) FILTER (WHERE outcome=$2 AND skip_reason=$3),
		       count(*), COALESCE(sum(blocks_written),0)
		  FROM distill_run WHERE source_key = $1`,
		key, distillOutcomeSkipped, distillSkipBudget).
		Scan(&s.calls, &s.wmTo, &s.budgetSkips, &s.rows, &s.blocks); err != nil {
		t.Fatalf("journal totals: %v", err)
	}
	return s
}

func wl2Journal(t *testing.T, pool *pgxpool.Pool, key string) wl2Row {
	t.Helper()
	var r wl2Row
	if err := pool.QueryRow(context.Background(), `
		SELECT outcome, COALESCE(skip_reason,''), calls, rows_dropped_dup, rows_dropped_cred,
		       watermark_to, blocks_written
		  FROM distill_run WHERE source_key = $1 ORDER BY started_at DESC LIMIT 1`, key).
		Scan(&r.outcome, &r.skipReason, &r.calls, &r.dup, &r.cred, &r.wmTo, &r.blocks); err != nil {
		t.Fatalf("journal: %v", err)
	}
	return r
}

// wl2Unseen answers which chunks of the fixture are NOT in the dedup ledger
// although the watermark claims to cover them.
//
// THAT IS THE WATERMARK'S WHOLE PROMISE (distillsource.go:184-192: "the HIGHEST
// FULLY COVERED watermark"), and it is the property the rejected variant of
// §A.2 (b) breaks: advancing the watermark at the rollover point books material
// as covered that never reached a call.
func wl2Unseen(t *testing.T, pool *pgxpool.Pool, key string, batches, perBatch int, upto int64) []string {
	t.Helper()
	var missing []string
	for idx := 1; idx <= batches; idx++ {
		if int64(idx)*10 > upto {
			continue
		}
		for i := 0; i < perBatch; i++ {
			text := fmt.Sprintf("[b%d c%d] %s", idx, i, a8Body)
			var n int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM distill_seen WHERE source_key=$1 AND row_hash=$2`,
				key, distillRowHash(text)).Scan(&n); err != nil {
				t.Fatalf("ledger: %v", err)
			}
			if n == 0 {
				missing = append(missing, fmt.Sprintf("b%d c%d", idx, i))
			}
		}
	}
	return missing
}

// wl2CapBelowOneInsight is the cap of the progress probe: a fresh, EMPTY shard
// cannot fit a single insight into it.
//
// Derived from the arm's own render rather than written as a literal, for the
// reason wl1FullBody gives for its own construction: a number would drift with
// every render change and the probe would silently stop measuring. The frame of
// an empty accumulator plus one rune less than the shortest possible insight is
// below the meter's own arithmetic by construction (distillUsedRunes adds the
// overflow-note reserve on top of this frame), so the FIRST call of a fresh
// shard is refused — which is exactly "max_block_runes knapp unter dem
// Erstcall-Bedarf".
func wl2CapBelowOneInsight() int {
	st := newDistillBlockState(dfRoot, "", 0)
	st.createdAt = time.Unix(1787893000, 0).UTC()
	return utf8.RuneCountInString(distillRenderN(st, wl2Opts(0), nil, nil, 0)) + distillMinInsightRunes - 1
}

// wl2AssertSealed checks that the body standing under `title` really fills its
// shard.
//
// ON A HAND-BUILT STATE, and that is the point: since wave W-L2 the SEED never
// returns a full shard — it seals it and opens the next — so asking the seed
// whether the fixture is full would answer "no" for the very reason the probe
// exists. The predicate itself is the production one (distillBlockState.full),
// so a render change that stops filling the fixture still fails loudly.
func wl2AssertSealed(t *testing.T, pool *pgxpool.Pool, title string) {
	t.Helper()
	st := newDistillBlockState(dfRoot, "", 0)
	carry, ok := distillSplitCarry(wl1Content(t, pool, title))
	if !ok {
		t.Fatalf("the fixture body of %q is not readable at all", title)
	}
	st.carry = carry
	if !st.full(wl1Opts()) {
		t.Fatalf("the fixture body of %q does not fill its shard — the probe measures nothing", title)
	}
}

// TestDistillShardRollover is the wave's gate list.
func TestDistillShardRollover(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)

	// THE AMENDMENT'S OWN RED, TURNED GREEN (§A.6 "W-L2", first two bullets).
	// Red on the unchanged tree, measured over three ticks: skipped/budget with
	// calls = 0, the watermark standing at 0 and exactly ONE block — the chain of
	// §A.1 step by step. Green: the same root gets shard 2 and the watermark
	// moves as soon as the batch runs through.
	t.Run("a full running shard opens the next one instead of standing still", func(t *testing.T) {
		a9Truncate(t, pool)
		shard1 := distillBlockTitle(dfRoot, 0, 1)
		wl1Put(t, pool, shard1, wl1FullBody(t, "SHARD1"), "insight", wl1Meta(dfRoot, 1))
		before := wl1Content(t, pool, shard1)

		// The fixture is the standing state of §A.1: shard 1 is full. Asserted
		// rather than assumed, so a render change that stops filling the shard
		// fails the probe instead of hollowing it out — and asserted on a
		// hand-built state, because the seed no longer ANSWERS "full": it opens
		// the next shard, which is what this probe is about.
		wl2AssertSealed(t, pool, shard1)
		if st := wl1Seed(t, pool, dfRoot); st.ordinal != 2 || st.carry.count() != 0 {
			t.Fatalf("the seed opened shard %d with %d carried claims, want an empty shard 2",
				st.ordinal, st.carry.count())
		}

		for tick := 1; tick <= 3; tick++ {
			a8ClearWindow(t, pool)
			wl2Run(t, pool, a9Source(2, 3, 0), 6000)
		}

		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) < 2 {
			t.Fatalf("%d shard(s) after three ticks — the full shard is still a standing state", len(shards))
		}
		if shards[1].ordinal != 2 {
			t.Errorf("the second shard is ordinal %d, want 2", shards[1].ordinal)
		}
		if len(shards[1].claims) == 0 {
			t.Error("shard 2 carries no claim — the run rolled over but bought nothing")
		}
		if now := wl1Content(t, pool, shard1); now != before {
			t.Error("shard 1 changed — the rollover wrote into the sealed shard")
		}
		sum := wl2Sum(t, pool, key)
		if sum.calls == 0 {
			t.Error("calls = 0 over three ticks — the run still spends nothing on a full shard")
		}
		if sum.wmTo == 0 {
			t.Error("watermark_to = 0 — the watermark did not move although the batch ran through")
		}
		if sum.budgetSkips != 0 {
			t.Errorf("%d skipped/budget row(s) — the standing state of §A.1 is unchanged",
				sum.budgetSkips)
		}
	})

	// NEGATIVE PROBE, MATERIAL FIDELITY — the most important of the wave
	// (§A.6): the union of the shards carries EVERY claim the one-block version
	// would have written, and none of them twice. Measured as a multiset over the
	// rendered claim lines, against the SAME fixture run without a cap at all.
	t.Run("the union of the shards carries every claim of the one-block version exactly once", func(t *testing.T) {
		a9Truncate(t, pool)
		wl2Run(t, pool, a9Source(2, 6, 0), 0)
		if n := len(a9Blocks(t, pool)); n != 1 {
			t.Fatalf("the uncapped baseline wrote %d blocks, want 1", n)
		}
		baseline := wl2Claims(t, pool, dfRoot)
		baseCalls := wl2Journal(t, pool, key).calls
		if len(baseline) != 12 {
			t.Fatalf("the baseline holds %d claims, want the fixture's 12", len(baseline))
		}

		a9Truncate(t, pool)
		wl2Run(t, pool, a9Source(2, 6, 0), 2600)
		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) < 2 {
			t.Fatalf("%d shard(s) under the cap — the run never rolled over", len(shards))
		}
		if len(shards) > 8 {
			t.Errorf("%d shards — the rollover is not bounded by the material", len(shards))
		}
		got := wl2Claims(t, pool, dfRoot)

		if len(got) != len(baseline) {
			t.Errorf("the shards carry %d claims, the one-block version carried %d",
				len(got), len(baseline))
		}
		for i := range baseline {
			if i >= len(got) || got[i] != baseline[i] {
				t.Errorf("claim %d differs.\nshards:   %q\nonebl.:   %q", i,
					wl2At(got, i), baseline[i])
				break
			}
		}
		seen := map[string]int{}
		for _, c := range got {
			seen[c]++
		}
		for c, n := range seen {
			if n != 1 {
				t.Errorf("claim appears %d times across the shards: %q", n, strings.TrimSpace(c))
			}
		}

		// NEGATIVE PROBE, COST (§A.6, last bullet, in the form the W-L2 review
		// left standing): the amendment's second half-sentence — "a rollover buys
		// a re-read, NO call" — is falsified at other cuts (a batch the rune
		// meter stops mid-way re-reads after the rollover, and the remaining
		// chunks form a call group of their own; 3×5 pays 5 calls where the
		// uncapped run pays 3). What HOLDS, and is pinned here and across three
		// cuts below: a rollover fragments at most ONE call group, so the capped
		// run pays at most one extra call per shard it opened — and every extra
		// call is visible as rows_dropped_dup.
		row := wl2Journal(t, pool, key)
		if row.calls > baseCalls+len(shards)-1 {
			t.Errorf("calls = %d against %d uncapped over %d shards — more than one extra call "+
				"per rollover", row.calls, baseCalls, len(shards))
		}
		if row.dup == 0 {
			t.Error("rows_dropped_dup = 0 — the re-read the rollover pays for is not visible")
		}
		if row.blocks < len(shards) {
			t.Errorf("blocks_written = %d for %d shards", row.blocks, len(shards))
		}
	})

	// NEGATIVE PROBE, COST ACROSS CUTS (W-L2 review, finding #1): one fixture is
	// not a cost statement. Three cuts, each measured against its own uncapped
	// baseline; the capped run MAY pay more calls (the fragmentation named
	// above — asserting equality here would re-state the falsified amendment
	// sentence, and the review measured 3×5 at 5 against 3), but at most one
	// extra call per rollover, every extra call leaves its re-read trace, and
	// no material is lost to the extra calls.
	t.Run("across three cuts a rollover buys at most one extra call", func(t *testing.T) {
		for _, cut := range []struct{ batches, rows int }{{2, 6}, {3, 5}, {5, 4}} {
			a9Truncate(t, pool)
			wl2Run(t, pool, a9Source(cut.batches, cut.rows, 0), 0)
			baseline := wl2Claims(t, pool, dfRoot)
			baseCalls := wl2Journal(t, pool, key).calls

			a9Truncate(t, pool)
			wl2Run(t, pool, a9Source(cut.batches, cut.rows, 0), 2600)
			got := wl2Claims(t, pool, dfRoot)
			row := wl2Journal(t, pool, key)
			shards := wl2Shards(t, pool, dfRoot)

			if len(got) != len(baseline) {
				t.Errorf("%d×%d: %d claims against %d uncapped", cut.batches, cut.rows,
					len(got), len(baseline))
				continue
			}
			if row.calls > baseCalls+len(shards)-1 {
				t.Errorf("%d×%d: calls = %d against %d uncapped over %d shards — more than one "+
					"extra call per rollover", cut.batches, cut.rows, row.calls, baseCalls, len(shards))
			}
			if row.calls > baseCalls && row.dup == 0 {
				t.Errorf("%d×%d: %d calls against %d uncapped with rows_dropped_dup = 0 — extra "+
					"calls without a visible re-read", cut.batches, cut.rows, row.calls, baseCalls)
			}
		}
	})

	// NEGATIVE PROBE, WATERMARK INTEGRITY (§A.6): every watermark_to covers only
	// material that reached a call. The rejected variant of §A.2 (b) — rolling
	// over by ADVANCING the watermark — provably skips chunks; it is run as a
	// counter-version in the wave report, and the property it breaks is this one.
	t.Run("every watermark_to covers only material that reached a call", func(t *testing.T) {
		a9Truncate(t, pool)
		wl2Run(t, pool, a9Source(2, 6, 0), 2600)

		row := wl2Journal(t, pool, key)
		if row.cred != 0 {
			t.Fatalf("rows_dropped_cred = %d — a deliberately discarded chunk would make the "+
				"probe ambiguous", row.cred)
		}
		if row.wmTo <= 0 {
			t.Fatalf("watermark_to = %d — nothing was covered, the probe is vacuous", row.wmTo)
		}
		if missing := wl2Unseen(t, pool, key, 2, 6, row.wmTo); len(missing) > 0 {
			t.Errorf("watermark_to = %d covers %d chunk(s) that never reached a call: %v",
				row.wmTo, len(missing), missing)
		}
	})

	// NEGATIVE PROBE, ROLLOVER CRASH (§A.6; the idempotency seam of §A.3 b). The
	// barrier aborts between the durable block write and the dedup ledger —
	// exactly the state a SIGTERM leaves — AFTER a rollover has happened. On the
	// restart the arm re-reads the same chunks and re-extracts identical claims,
	// and none of them may end up in two shards.
	t.Run("a crash after the rollover creates no duplicate between shard n and n+1", func(t *testing.T) {
		a9Truncate(t, pool)

		fired := 0
		distillWriteBarrier = func(context.Context) error {
			fired++
			return fmt.Errorf("probe: killed between the block write and the ledger")
		}
		wl2Run(t, pool, a9Source(2, 6, 0), 2600)
		distillWriteBarrier = func(context.Context) error { return nil }

		if fired == 0 {
			t.Fatal("the barrier never fired — the probe measures nothing")
		}
		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) < 2 {
			t.Fatalf("%d shard(s) after the abort — no rollover happened before the crash", len(shards))
		}
		var unseen int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM distill_seen WHERE source_key=$1`,
			key).Scan(&unseen); err != nil {
			t.Fatalf("ledger: %v", err)
		}
		if unseen != 0 {
			t.Fatalf("%d ledger rows after the abort — the crash did not land before the ledger", unseen)
		}

		a8ClearWindow(t, pool)
		wl2Run(t, pool, a9Source(2, 6, 0), 2600)

		got := wl2Claims(t, pool, dfRoot)
		seen := map[string]int{}
		for _, c := range got {
			seen[c]++
		}
		for c, n := range seen {
			if n != 1 {
				t.Errorf("after the restart the claim stands in %d shards: %q", n, strings.TrimSpace(c))
			}
		}
		// And nothing was lost across the crash either: the restart re-reads the
		// whole range, so the union has to be the full material of the fixture.
		if len(seen) != 12 {
			t.Errorf("%d distinct claims after the crash and the restart, want the fixture's 12",
				len(seen))
		}
	})

	// NEGATIVE PROBE, THE PROGRESS CONDITION (§A.4 c): a shard that has not seen
	// a single call must not roll over. With a cap below one insight the run
	// therefore ends instead of opening shard after shard — the counter-version
	// WITHOUT the condition runs the batch loop unbounded and is measured as such
	// in the wave report.
	t.Run("a shard that saw no call does not roll over", func(t *testing.T) {
		a9Truncate(t, pool)
		cap := wl2CapBelowOneInsight()

		deadline, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		wl2RunCtx(t, pool, deadline, a9Source(2, 6, 0), cap)
		if deadline.Err() != nil {
			t.Fatal("the run did not terminate inside 60 s — the batch loop is unbounded")
		}

		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks at a cap of %d runes — a shard without a call rolled over",
				len(blocks), cap)
		}
		row := wl2Journal(t, pool, key)
		if row.calls != 0 {
			t.Errorf("calls = %d — the cap does not refuse the first call, the probe measures "+
				"something else", row.calls)
		}
		if row.skipReason != distillSkipBudget {
			t.Errorf("skip_reason = %q, want %q", row.skipReason, distillSkipBudget)
		}
	})

	// THE E-4 TYPE GATE, GROWN WITH THE DEDUP (W-L1 report §2 E-4, handed over by
	// name). A LOWER shard is material from this wave on, so the two halves are
	// probed against each other: the arm's own type suppresses a repeated claim,
	// a FOREIGN type on the same title does not — its lines never enter the dedup
	// set, and it is left standing rather than refused.
	t.Run("a lower shard is dedup material only under the arm's own type", func(t *testing.T) {
		// The byte-exact lines the arm produces for this fixture — taken from a
		// real run, because the quote is cut out of the prompt the arm built.
		a9Truncate(t, pool)
		wl2Run(t, pool, a9Source(1, 3, 0), 0)
		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks in the reference run, want 1", len(blocks))
		}
		body := blocks[0].content

		for _, tc := range []struct {
			name, typeName string
			wantGrowth     bool
		}{
			{"the arm's own type suppresses the repeated claims", "insight", false},
			{"a foreign type on a lower shard is not read", "knowledge", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a9Truncate(t, pool)
				shard1 := distillBlockTitle(dfRoot, 0, 1)
				shard2 := distillBlockTitle(dfRoot, 0, 2)
				wl1Put(t, pool, shard1, body, tc.typeName, wl1Meta(dfRoot, 1))
				wl1Put(t, pool, shard2, wl1Body(t, "SHARD2", 1), "insight", wl1Meta(dfRoot, 2))
				was1 := wl1Content(t, pool, shard1)

				wl2Run(t, pool, a9Source(1, 3, 0), 6000)

				// a8Claim is the run's claim text and appears in no fixture body — the
				// rendered evidence line carries "Abschnitt <n>" for every insight, so
				// that word alone would match the fixture itself.
				grown := strings.Contains(wl1Content(t, pool, shard2), a8Claim)
				if grown != tc.wantGrowth {
					t.Errorf("the running shard grew = %v, want %v", grown, tc.wantGrowth)
				}
				if now := wl1Content(t, pool, shard1); now != was1 {
					t.Error("the lower shard was written into")
				}
			})
		}
	})

	// W-L1 REVIEW, MINOR #3: the one standing refusal this wave leaves behind —
	// a body the arm cannot read — has to name the shard it refused. Before this
	// wave it reached the generic log branch with source_key and the error text
	// only, and with several shards per range that is not an address.
	t.Run("an unreadable body names the shard it refused", func(t *testing.T) {
		a9Truncate(t, pool)
		shard1 := distillBlockTitle(dfRoot, 0, 1)
		shard2 := distillBlockTitle(dfRoot, 0, 2)
		wl1Put(t, pool, shard1, wl1Body(t, "SHARD1", 2), "insight", wl1Meta(dfRoot, 1))
		wl1Put(t, pool, shard2, "kein Abschnitt, den dieser Arm je geschrieben hat", "insight",
			wl1Meta(dfRoot, 2))

		logs := n15CaptureLogs(t, func() { wl2Run(t, pool, a9Source(1, 3, 0), 6000) })

		if !strings.Contains(logs, "Teil 2") {
			t.Errorf("the refusal log does not name the shard it refused.\nCaught: %s", logs)
		}
		row := wl2Journal(t, pool, key)
		if row.outcome != distillOutcomeFailed {
			t.Errorf("journal outcome = %q, want failed", row.outcome)
		}
	})
}

// wl2At is a bounds-safe read for the diff message of the fidelity probe.
func wl2At(s []string, i int) string {
	if i >= len(s) {
		return "<missing>"
	}
	return s[i]
}
