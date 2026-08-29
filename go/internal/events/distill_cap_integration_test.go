//go:build integration

// Wave W-L3 — the shard cap and the observability of the shard layout
// (amendment C4-2, design/02-destillat-arm.md §A.4 b/d, §A.6 "W-L3", plus the
// two conditions the W-L2 review left on this wave: finding #2, the group read
// that moved 8,0 MiB, and note #8, the seed loop nothing bounded).
//
// WHAT THIS FILE PROBES. W-L2 turned a full block from a run's end into a
// handover; nothing bounded how far that chain may grow, and nothing said how
// often a run had handed over. This wave adds the bound
// (distill.max_blocks_per_root, 0 = off) and the number
// (coverage.shard_rollovers) — and it bounds the group read that both rest on.
//
// NO REAL LLM CALL: the stub sits behind the backend seam exactly as in A02-8,
// A02-9, W-L1 and W-L2, so everything above it is production code. No docker
// against the live system or the measure copy; every fixture goes down through
// the production write path.
//
//	go test -tags=integration ./internal/events/ -run 'TestDistillShardCap|TestDistillShardGroupRead|TestDistillShardRolloverCoverage' -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
)

// wl3Opts is wl2Opts with the wave's own key — the shard cap.
func wl3Opts(maxRunes, maxShards int) distillWriteOpts {
	o := wl2Opts(maxRunes)
	o.maxShards = maxShards
	return o
}

// wl3Run drives ONE tick with a steerable cap on BOTH axes: the rune cap of one
// block (C3-1) and the shard cap of one range (this wave). It is wl2Run plus the
// key, kept separate so W-L2's probes keep measuring what they measured.
func wl3Run(t *testing.T, pool *pgxpool.Pool, src distillsource.Source, maxRunes, maxShards int) {
	t.Helper()
	stub := a8NewStub(t, wl2Stub)
	cfg := a8Config()
	cfg.Distill.MaxBlockRunes = maxRunes
	cfg.Distill.MaxBlocksPerRoot = maxShards
	s := a8Scheduler(pool, cfg, src, a8Pool(stub.srv.URL))
	s.distillOnce(context.Background(), dfNoDemand)
}

// wl3Coverage answers one block's coverage object.
func wl3Coverage(t *testing.T, pool *pgxpool.Pool, title string) map[string]int {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(metadata->'coverage','{}'::jsonb) FROM context_blocks
		  WHERE category='session-insights' AND title=$1 AND scope=$2 AND NOT is_archived`,
		title, dfScope).Scan(&raw); err != nil {
		t.Fatalf("read coverage of %q: %v", title, err)
	}
	var cov map[string]int
	if err := json.Unmarshal(raw, &cov); err != nil {
		t.Fatalf("decode coverage of %q: %v", title, err)
	}
	return cov
}

// wl3AllClaims is the multiset of rendered claim lines over EVERY insight block
// of the corpus — across all ranges of the root, not just one (root, wmFrom)
// group.
//
// IT IS THE CAP'S MATERIAL-FIDELITY MEASUREMENT, and it has to look wider than
// wl2Claims because of what the cap actually does: it bounds ONE range's chain,
// and a run whose range is capped hands the rest of the material to the NEXT
// range as soon as the watermark moves. Measuring one group would report that
// material as lost when it is one title away.
func wl3AllClaims(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var out []string
	for _, b := range a9Blocks(t, pool) {
		c, split := distillSplitCarry(b.content)
		if !split {
			t.Fatalf("the arm cannot read back its own block %q", b.title)
		}
		out = append(out, c.claims...)
	}
	sort.Strings(out)
	return out
}

// wl3Chains answers how many shards each (root, watermark_from) range holds —
// the axis the cap binds.
func wl3Chains(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT COALESCE(metadata->>'watermark_from','?'), count(*)
		  FROM context_blocks
		 WHERE category='session-insights' AND scope=$1 AND NOT is_archived
		 GROUP BY 1`, dfScope)
	if err != nil {
		t.Fatalf("chain census: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var from string
		var n int
		if err := rows.Scan(&from, &n); err != nil {
			t.Fatalf("scan census: %v", err)
		}
		out[from] = n
	}
	return out
}

// wl3Rest is what a source's own history looks like once it stops changing.
type wl3Rest struct {
	row     wl2Row    // the journal row the source came to rest on
	totals  wl2Totals // the accumulated journal at rest
	ticks   int       // how many ticks it took
	stalled bool      // true when the source rests WITHOUT having covered its range
	logs    string
}

// wl3RunUntilQuiet ticks until the source stops changing, and answers HOW it
// stopped.
//
// TWO RESTING STATES, and the wave has to tell them apart (review finding #2).
// The ordinary one is `skipped/no_new_rows`: the range is covered and the arm
// has nothing left to do. The other is the CAPPED one: the cap binds inside a
// batch, so the watermark never moves and every further tick reproduces the
// same answer — a resting state too, but a waiting one. It is recognised by its
// own signature rather than by a tick count: two consecutive ticks with the same
// call count, the same watermark and the same number of blocks.
func wl3RunUntilQuiet(t *testing.T, pool *pgxpool.Pool, key string, maxRunes, maxShards, maxTicks int) wl3Rest {
	t.Helper()
	var r wl3Rest
	var prev wl2Totals
	for tick := 1; tick <= maxTicks; tick++ {
		a8ClearWindow(t, pool)
		r.logs += n15CaptureLogs(t, func() { wl3Run(t, pool, a9Source(2, 6, 0), maxRunes, maxShards) })
		r.ticks = tick
		r.row = wl2Journal(t, pool, key)
		now := wl2Sum(t, pool, key)
		now.rows = len(a9Blocks(t, pool)) // reuse the field for the block census
		if r.row.outcome == distillOutcomeSkipped && r.row.skipReason == distillSkipNoNewRows {
			r.totals = now
			return r
		}
		if tick > 1 && now.calls == prev.calls && now.wmTo == prev.wmTo && now.rows == prev.rows {
			r.totals, r.stalled = now, true
			return r
		}
		prev = now
	}
	t.Fatalf("the source neither covered its range nor came to a standstill in %d ticks; last row = %q/%q",
		maxTicks, r.row.outcome, r.row.skipReason)
	return r
}

// wl3Seen counts the chunks of the fixture that carry a dedup-ledger row, i.e.
// the material that actually reached a call.
func wl3Seen(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM distill_seen WHERE source_key=$1`, key).Scan(&n); err != nil {
		t.Fatalf("ledger count: %v", err)
	}
	return n
}

// TestDistillShardCap is the wave's gate list for distill.max_blocks_per_root.
//
// The red state is in the wave report and was measured on the unchanged tree:
// the same fixture grows to four shards and no setting could have stopped it at
// two (zzwl3_rot_integration_test.go, "nothing bounds the number of shards one
// range may grow"), while GET /api/settings answers the key with nothing at all.
func TestDistillShardCap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	key := distillSourceKey(dfLabel, dfScope, dfRoot)

	// THE GATE ITSELF (design/02:3304): cap = 2 ⇒ the arm stops after shard 2
	// and answers budget, WITHOUT losing material. Both halves are measured:
	// the chain stops where the cap says, and every chunk that did not reach a
	// call is still missing from the dedup ledger — i.e. the next tick reads it
	// again the moment the cap is raised.
	t.Run("the cap ends the chain after the last allowed shard without losing material", func(t *testing.T) {
		a9Truncate(t, pool)
		wl3Run(t, pool, a9Source(2, 6, 0), 2600, 2)

		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) != 2 {
			t.Fatalf("%d shard(s) under a cap of 2 — the cap did not bind", len(shards))
		}
		row := wl2Journal(t, pool, key)
		if row.skipReason != distillSkipBudget {
			t.Errorf("skip_reason = %q, want %q — the cap must answer in the journal's own vocabulary",
				row.skipReason, distillSkipBudget)
		}

		// NO MATERIAL LOSS, and this is the half the gate names in brackets ("das
		// Wasserzeichen steht, der Rest wartet"). The stub answers exactly one
		// claim per chunk, so a chunk that reached a call is a claim in some
		// shard: claims == ledger rows means nothing bought was thrown away, and
		// the fixture's remaining chunks are neither in the ledger nor under the
		// watermark.
		claims := len(wl2Claims(t, pool, dfRoot))
		seen := wl3Seen(t, pool, key)
		if claims != seen {
			t.Errorf("%d claims over the shards against %d ledger rows — the cap discarded paid material",
				claims, seen)
		}
		if claims >= 12 {
			t.Fatalf("%d of 12 claims — the cap did not stop this run, the probe measures nothing", claims)
		}
		if missing := wl2Unseen(t, pool, key, 2, 6, row.wmTo); len(missing) > 0 {
			t.Errorf("watermark_to = %d covers %d chunk(s) that never reached a call: %v",
				row.wmTo, len(missing), missing)
		}
	})

	// THE SAME GATE OVER A WHOLE BACKLOG, AND OVER A FAN OF CUTS — this is the
	// probe the W-L3 review's blocker #1 falsified, rebuilt on its own sweep.
	//
	// WHAT WENT WRONG THE FIRST TIME (W19, verbatim from the Präambel: a fixture
	// must not share the code's assumption): the probe pinned exactly ONE cut,
	// `2600 / cap 2`, at which the render overflow happens to land on a batch
	// boundary where the watermark moves anyway. At `2600 / cap 3` and
	// `2200 / cap 4` the same code loses two paid claims for good — the reviewer
	// measured 10 of 12, ledger 12, watermark past them, source at rest. One cut
	// is not a property.
	//
	// WHAT THE PROBE PINS NOW, over every cut of the reviewer's sweep that
	// showed the loss: every bought claim stands in a shard (claims == ledger),
	// the multiset of claims under a cap equals the one without it wherever the
	// range covered its material, no range grows past the cap, nothing is bought
	// twice, and no chunk of the fixture is left both unclaimed and covered.
	//
	// WAVE W-L4 SPLIT THE COMPARISON, and the reason is in the body: a cap that
	// truly binds leaves material WAITING, which the first version could not tell
	// from material LOST. The loss half is now measured directly against the
	// dedup ledger and therefore in both resting states.
	t.Run("no cut of the cap loses paid material", func(t *testing.T) {
		a9Truncate(t, pool)
		free := wl3RunUntilQuiet(t, pool, key, 2600, 0, 12)
		freeClaims := wl3AllClaims(t, pool)
		if len(freeClaims) != 12 || free.stalled {
			t.Fatalf("the uncapped baseline holds %d claims (stalled=%v) — the probe has no reference",
				len(freeClaims), free.stalled)
		}
		freeCalls := free.totals.calls

		for _, tc := range []struct{ runes, cap int }{
			{2600, 2}, // the cut the first version pinned
			{2600, 3}, // reviewer R8: 10 of 12 claims, ledger 12, watermark past them
			{2200, 4}, // reviewer R2: same shape at another cut
		} {
			t.Run(fmt.Sprintf("max_block_runes %d, cap %d", tc.runes, tc.cap), func(t *testing.T) {
				a9Truncate(t, pool)
				rest := wl3RunUntilQuiet(t, pool, key, tc.runes, tc.cap, 12)
				capped := wl3AllClaims(t, pool)
				chains := wl3Chains(t, pool)

				for from, n := range chains {
					if n > tc.cap {
						t.Errorf("the range starting at %s holds %d shards under a cap of %d",
							from, n, tc.cap)
					}
				}
				// THE DIRECT FORM OF THE PROPERTY, and it holds in both resting
				// states (wave W-L4): every claim the arm bought is in some shard.
				// The reviewer's failure signature — 10 claims against a ledger of
				// 12, watermark past them — is red here without any assumption about
				// how much a capped chain can hold.
				ledger := wl3Seen(t, pool, key)
				if len(capped) != ledger {
					t.Errorf("%d claims over the shards against %d ledger rows — material was bought "+
						"and thrown away", len(capped), ledger)
				}

				// TWO RESTING STATES, TOLD APART (wave W-L4). A range that COVERED
				// its material must hold exactly the claims the uncapped run holds.
				// A range the cap STALLED holds fewer — and that is not a loss but
				// the not-aus doing its job (design/02:3304: "das Wasserzeichen
				// steht, der Rest wartet"): what it holds must be part of the
				// baseline, and nothing it did not buy may sit below the watermark.
				//
				// The distinction became necessary with the chain line: it takes
				// runes out of every shard above the first, and the 2200-rune cut
				// had 22 runes of slack, so four shards no longer hold this fixture
				// and the cap binds for real (measured: 5 claims, ledger 5,
				// watermark 0, against 12/12 before the line).
				if rest.stalled {
					inFree := map[string]struct{}{}
					for _, c := range freeClaims {
						inFree[c] = struct{}{}
					}
					for _, c := range capped {
						if _, ok := inFree[c]; !ok {
							t.Errorf("the stalled range holds a claim the uncapped run never wrote: %q",
								strings.TrimSpace(c))
						}
					}
					if len(capped) >= len(freeClaims) {
						t.Errorf("the range rests stalled with %d of %d claims — a range that holds "+
							"everything must rest covered, not waiting", len(capped), len(freeClaims))
					}
					if missing := wl2Unseen(t, pool, key, 2, 6, rest.totals.wmTo); len(missing) > 0 {
						t.Errorf("the stalled range covers %d chunk(s) that never reached a call: %v — "+
							"the waiting material is not readable again", len(missing), missing)
					}
				} else {
					if len(capped) != len(freeClaims) {
						t.Errorf("%d claims under the cap against %d without it — the cap lost paid material",
							len(capped), len(freeClaims))
					}
					for i := range freeClaims {
						if i >= len(capped) || capped[i] != freeClaims[i] {
							t.Errorf("claim %d differs.\ncapped: %q\nfree:   %q",
								i, wl2At(capped, i), freeClaims[i])
							break
						}
					}
				}
				seen := map[string]int{}
				for _, c := range capped {
					seen[c]++
				}
				for c, n := range seen {
					if n != 1 {
						t.Errorf("claim appears %d times under the cap: %q", n, strings.TrimSpace(c))
					}
				}
				if rest.totals.calls > freeCalls {
					t.Errorf("calls %d under the cap against %d without it — the cap bought material twice",
						rest.totals.calls, freeCalls)
				}
				t.Logf("cap %d @ %d runes: %d claims over %d range(s), %d calls, %d ticks, stalled=%v",
					tc.cap, tc.runes, len(capped), len(chains), rest.totals.calls, rest.ticks, rest.stalled)
			})
		}
	})

	// THE MECHANISM BEHIND THE FAN, pinned on its own so a future change cannot
	// keep the multiset property by accident (review blocker #1): a batch whose
	// insights the cap could not place is cut at the first held-back insight —
	// what the shard took is ledgered, the rest is NOT, and the watermark does
	// not move over it.
	t.Run("the cap holds back exactly the chunks whose insights found no shard", func(t *testing.T) {
		a9Truncate(t, pool)
		wl3Run(t, pool, a9Source(2, 6, 0), 2600, 3)
		row := wl2Journal(t, pool, key)
		claims := len(wl2Claims(t, pool, dfRoot))
		seen := wl3Seen(t, pool, key)

		if claims != seen {
			t.Errorf("%d claims against %d ledger rows — the ledger and the blocks disagree about "+
				"what was placed", claims, seen)
		}
		if seen >= 12 {
			t.Fatalf("%d of 12 chunks ledgered — the cap did not hold anything back here", seen)
		}
		// The held-back chunks must still be readable: they are neither in the
		// ledger nor under the watermark.
		if missing := wl2Unseen(t, pool, key, 2, 6, row.wmTo); len(missing) > 0 {
			t.Errorf("watermark_to = %d covers %d chunk(s) that never reached a shard: %v",
				row.wmTo, len(missing), missing)
		}
		// R2-#3: the block was written with the batch's own watermark before the
		// hold decision, so it may state MORE coverage than the journal books —
		// never less. That direction is the conservative one and is pinned here
		// rather than left to the comment.
		var blockWM int64
		if err := pool.QueryRow(context.Background(),
			`SELECT max((metadata->>'watermark_to')::bigint) FROM context_blocks
			  WHERE category='session-insights' AND scope=$1 AND NOT is_archived`,
			dfScope).Scan(&blockWM); err != nil {
			t.Fatalf("read block watermark: %v", err)
		}
		if blockWM < row.wmTo {
			t.Errorf("the blocks state watermark_to %d, the journal %d — a block must never claim "+
				"LESS coverage than the run row", blockWM, row.wmTo)
		}
		// R3-#4: the second half of the rollover() promise — "never more material
		// than it holds" — had no assertion. It is checkable: insight_count is the
		// block's own statement about how many insights it carries, so it must
		// equal the claim lines its body actually renders, on every shard.
		for _, b := range a9Blocks(t, pool) {
			c, split := distillSplitCarry(b.content)
			if !split {
				t.Fatalf("the arm cannot read back %q", b.title)
			}
			if b.insightCount != len(c.claims) {
				t.Errorf("%q states insight_count=%d but renders %d claim line(s) — a block claims "+
					"more material than it holds", b.title, b.insightCount, len(c.claims))
			}
		}
		t.Logf("cap 3 @ 2600: %d claims, %d ledger rows, journal watermark %d, block watermark %d",
			claims, seen, row.wmTo, blockWM)
	})

	// THE CAPPED RESTING STATE, named and pinned (review finding #2). Where the
	// cap binds INSIDE a batch the watermark cannot move, so the range does not
	// hand its remainder to a next range — it WAITS, which is what the amendment
	// gate says in its own words ("das Wasserzeichen steht, der Rest wartet").
	//
	// The property that makes waiting acceptable rather than a deadlock is that
	// it is CHEAP and REVERSIBLE, and both halves are asserted here: at rest the
	// arm buys nothing (the rune meter refuses the call before it is paid for),
	// writes no block and moves no watermark — and raising the cap resumes the
	// very same range, with the material that waited.
	//
	// "CHEAP" HAS A CONDITION, and the re-review measured where it ends (R2-#6):
	// it holds while max_block_runes carries at least one insight. Below that the
	// meter has no measurement to brake on, the batch is re-read and re-bought
	// every tick (20 ticks / 20 calls at 1 800 runes) — C3-1's first-call
	// boundary, which the uncapped run pays at the same rate. This probe runs at
	// 2 600, i.e. inside the regime the promise covers, and the promise is now
	// written with that condition in docs and in the key's own doc.
	t.Run("a range the cap holds waits cheaply and resumes when the cap is raised", func(t *testing.T) {
		a9Truncate(t, pool)
		held := wl3RunUntilQuiet(t, pool, key, 2600, 1, 12)
		if !held.stalled {
			t.Fatalf("cap 1 covered its range in %d ticks — the probe measures nothing", held.ticks)
		}
		atRest := held.totals
		blocks := len(a9Blocks(t, pool))
		claims := len(wl3AllClaims(t, pool))

		// Three more ticks in the holding state: no call, no block, no watermark.
		logs := n15CaptureLogs(t, func() {
			for tick := 1; tick <= 3; tick++ {
				a8ClearWindow(t, pool)
				wl3Run(t, pool, a9Source(2, 6, 0), 2600, 1)
			}
		})
		after := wl2Sum(t, pool, key)
		if after.calls != atRest.calls {
			t.Errorf("calls %d → %d while the cap holds — waiting is not free", atRest.calls, after.calls)
		}
		if after.wmTo != atRest.wmTo {
			t.Errorf("watermark_to %d → %d while the cap holds", atRest.wmTo, after.wmTo)
		}
		if n := len(a9Blocks(t, pool)); n != blocks {
			t.Errorf("%d → %d blocks while the cap holds", blocks, n)
		}
		if !strings.Contains(logs, "max_blocks_per_root=1") {
			t.Errorf("a holding range does not say so in the log.\nCaught: %s", logs)
		}

		// AND IT WAKES UP. The cap is the only thing holding it, so raising it
		// resumes the same range — the material that waited is still readable.
		resumed := wl3RunUntilQuiet(t, pool, key, 2600, 0, 12)
		if resumed.stalled {
			t.Fatal("the range did not resume after the cap was raised — the hold is not reversible")
		}
		if got := len(wl3AllClaims(t, pool)); got != 12 {
			t.Errorf("%d of 12 claims after the cap was raised — the held material did not come back", got)
		}
		if n := len(a9Blocks(t, pool)); n <= blocks {
			t.Errorf("%d blocks after the cap was raised, want more than the %d it held", n, blocks)
		}
		t.Logf("held at %d claims / %d blocks / %d calls, resumed to %d claims / %d blocks",
			claims, blocks, atRest.calls, len(wl3AllClaims(t, pool)), len(a9Blocks(t, pool)))
	})

	// NEGATIVE PROBE, OFF SEMANTICS (design/02:3307): 0 is "no cap", never "no
	// blocks". Measured on the SAME fixture as the capped run above — the one
	// that stops at two shards with a cap of 2 runs to its end with 0 — and on
	// the ordinary uncapped run, which must still write its one block.
	t.Run("zero is no cap, never no blocks", func(t *testing.T) {
		a9Truncate(t, pool)
		wl3Run(t, pool, a9Source(2, 6, 0), 2600, 0)
		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) <= 2 {
			t.Errorf("%d shard(s) at cap 0 — a zero cap braked the chain", len(shards))
		}
		if got := len(wl2Claims(t, pool, dfRoot)); got != 12 {
			t.Errorf("%d of the fixture's 12 claims at cap 0 — a zero cap lost material", got)
		}
		if row := wl2Journal(t, pool, key); row.skipReason == distillSkipBudget && row.calls == 0 {
			t.Error("cap 0 answered budget without a single call — 0 was read as \"no blocks\"")
		}

		// The plainest form of the same statement: the ordinary run of a root
		// with no chain at all still writes its block under a zero cap.
		a9Truncate(t, pool)
		wl3Run(t, pool, a9Source(1, 2, 0), 6000, 0)
		if n := len(a9Blocks(t, pool)); n != 1 {
			t.Errorf("%d block(s) for an ordinary run at cap 0, want 1", n)
		}
	})

	// REVIEW FINDING #4: W-L2's note #8 has to be closed at the DEFAULT, not only
	// where an operator set a cap. A planted chain of full, arm-typed rows WITHOUT
	// the group keys is invisible to the group read, so the seed walks it one
	// point lookup at a time — and at cap 0 nothing bounded that walk.
	//
	// Red on the reviewed commit (own reproduction of R4, scaled past the hard
	// bound): a chain of distillShardGroupMaxRows + 4 links makes the seed take
	// all of them.
	t.Run("the seed walk is bounded even with no cap configured", func(t *testing.T) {
		a9Truncate(t, pool)
		body := wl1FullBody(t, "PLANTED")
		links := distillShardGroupMaxRows + 4
		for n := 1; n <= links; n++ {
			// No group keys at all — exactly the shape note #8 describes, and the
			// reason the group read cannot see this chain.
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), body, "insight", map[string]any{})
		}

		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		st, err := s.distillSeedBlock(context.Background(), wl3Opts(6000, 0), dfRoot, 0)
		if err != nil {
			t.Fatalf("seed over the planted chain: %v", err)
		}
		if st.rollovers >= links {
			t.Errorf("the seed walked %d links of a %d-link chain at cap 0 — the walk is unbounded",
				st.rollovers, links)
		}
		if st.ordinal > distillShardGroupMaxRows {
			t.Errorf("the seed reached ordinal %d, want at most the hard bound %d",
				st.ordinal, distillShardGroupMaxRows)
		}
		if !st.capped {
			t.Error("the seed stopped at the hard bound without saying so — the run would write anyway")
		}
		t.Logf("chain=%d links, seed stopped at ordinal %d after %d steps (capped=%v)",
			links, st.ordinal, st.rollovers, st.capped)
	})

	// R2-#2: the hard bound has to hold on BOTH sides. The re-review planted 254
	// full shards, ran ONE tick at cap 0 and measured the chain at 258 — the
	// write path had no bound at all, so the arm could write a chain its own seed
	// then refuses to walk (`capped=true` with no key to lift it).
	//
	// Red on a5fc128b: `blocks=258, highest ordinal=258, hardBound-Log=false`.
	t.Run("the write path never grows a chain past the hard bound", func(t *testing.T) {
		a9Truncate(t, pool)
		body := wl1FullBody(t, "SEALED")
		start := distillShardGroupMaxRows - 2
		for n := 1; n <= start; n++ {
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), body, "insight", wl1Meta(dfRoot, n))
		}

		logs := n15CaptureLogs(t, func() { wl3Run(t, pool, a9Source(2, 6, 0), 2600, 0) })

		highest := 0
		for _, s := range wl2Shards(t, pool, dfRoot) {
			if s.ordinal > highest {
				highest = s.ordinal
			}
		}
		if highest > distillShardGroupMaxRows {
			t.Errorf("the run wrote up to ordinal %d, past the hard bound %d — the arm built a chain "+
				"it can no longer seed", highest, distillShardGroupMaxRows)
		}
		if !strings.Contains(logs, "hard shard bound") {
			t.Errorf("the run passed the bound without saying so.\nCaught: %s",
				logs[max(0, len(logs)-600):])
		}
		// AND THE STATE IS NOT AN END STATION: the bound is the LARGER of the
		// constant and the operator's cap, so raising the cap above 256 lets the
		// same chain grow again. Measured at the seed, because a second run would
		// open the NEXT range (its watermark has moved) and say nothing about this
		// chain.
		// The last allowed shard is filled by hand, because the run left it with
		// room — the bound only answers once there is nowhere left inside it.
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, distillShardGroupMaxRows), body, "insight",
			wl1Meta(dfRoot, distillShardGroupMaxRows))
		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		atBound, err := s.distillSeedBlock(context.Background(), wl3Opts(6000, 0), dfRoot, 0)
		if err != nil {
			t.Fatalf("seed at the bound: %v", err)
		}
		if !atBound.capped {
			t.Errorf("the seed opened shard %d at the bound without saying capped", atBound.ordinal)
		}
		lifted, err := s.distillSeedBlock(context.Background(),
			wl3Opts(6000, distillShardGroupMaxRows+8), dfRoot, 0)
		if err != nil {
			t.Fatalf("seed with the cap raised: %v", err)
		}
		if lifted.capped || lifted.ordinal <= distillShardGroupMaxRows {
			t.Errorf("with the cap at %d the seed answers ordinal %d capped=%v — the bound has no key",
				distillShardGroupMaxRows+8, lifted.ordinal, lifted.capped)
		}
		t.Logf("chain %d → %d written at the bound; seed at bound: ordinal %d capped=%v, "+
			"with the cap raised: ordinal %d capped=%v",
			start, highest, atBound.ordinal, atBound.capped, lifted.ordinal, lifted.capped)
	})

	// NEGATIVE PROBE, THE SEED SIDE OF THE CAP (W-L2 review note #8): the seed
	// walks a chain of sealed shards, and before this wave nothing bounded that
	// walk. With the cap it stops AT the cap — without a run row's calls, without
	// touching the chain, and without opening the shard above it.
	t.Run("the seed refuses to open a shard above the cap", func(t *testing.T) {
		a9Truncate(t, pool)
		var titles []string
		for n := 1; n <= 3; n++ {
			title := distillBlockTitle(dfRoot, 0, n)
			wl1Put(t, pool, title, wl1FullBody(t, "SEALED"), "insight", wl1Meta(dfRoot, n))
			wl2AssertSealed(t, pool, title)
			titles = append(titles, title)
		}

		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		st, err := s.distillSeedBlock(context.Background(), wl3Opts(6000, 3), dfRoot, 0)
		if err != nil {
			t.Fatalf("seed under the cap: %v", err)
		}
		if !st.capped {
			t.Errorf("the seed opened shard %d although the cap is 3 — nothing bounds the walk",
				st.ordinal)
		}
		if st.ordinal != 3 {
			t.Errorf("ordinal = %d, want 3 — the cap must stop ON the last allowed shard", st.ordinal)
		}

		// Above the cap the run must not even start: no call, no block, and the
		// three sealed shards byte-identical.
		before := []string{wl1Content(t, pool, titles[0]), wl1Content(t, pool, titles[1]),
			wl1Content(t, pool, titles[2])}
		wl3Run(t, pool, a9Source(1, 2, 0), 6000, 3)
		if n := len(a9Blocks(t, pool)); n != 3 {
			t.Errorf("%d block(s) after a capped run, want the 3 of the fixture", n)
		}
		for i, was := range before {
			if now := wl1Content(t, pool, titles[i]); now != was {
				t.Errorf("shard %d changed although the cap refused the run", i+1)
			}
		}
		if row := wl2Journal(t, pool, key); row.calls != 0 || row.skipReason != distillSkipBudget {
			t.Errorf("journal calls/skip_reason = %d/%q, want 0/%q",
				row.calls, row.skipReason, distillSkipBudget)
		}

		// And the counter-proof against a cap that simply switches the arm off:
		// one more allowed shard and the same fixture opens shard 4.
		wl3Run(t, pool, a9Source(1, 2, 0), 6000, 4)
		if n := len(a9Blocks(t, pool)); n != 4 {
			t.Errorf("%d block(s) with the cap raised to 4 — the cap is an off-switch, not a bound", n)
		}
	})
}

// TestDistillShardRolloverCoverage is the wave's observability gate
// (design/02:3303): the number of handovers a run made is readable on the block.
//
// Red on the unchanged tree, measured over a four-shard run: not one of the four
// coverage objects carried the key (zzwl3_rot_integration_test.go).
func TestDistillShardRolloverCoverage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// A run that hands over several times: every shard it wrote states how many
	// handovers had happened when it was last written, and the difference to its
	// own ordinal is the ordinal the run STARTED on — identical on every shard,
	// which is what makes the number checkable rather than merely present.
	t.Run("every shard of a rolling run states the run's handover count", func(t *testing.T) {
		a9Truncate(t, pool)
		wl3Run(t, pool, a9Source(2, 6, 0), 2600, 0)
		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) < 3 {
			t.Fatalf("%d shard(s) — the fixture did not roll over, the probe measures nothing",
				len(shards))
		}
		start := -1
		for _, s := range shards {
			cov := wl3Coverage(t, pool, s.title)
			got, ok := cov["shard_rollovers"]
			if !ok {
				t.Fatalf("shard %d carries no shard_rollovers key: %v", s.ordinal, cov)
			}
			if got != s.ordinal-1 {
				t.Errorf("shard %d states %d handovers, want %d", s.ordinal, got, s.ordinal-1)
			}
			if start == -1 {
				start = s.ordinal - got
			} else if s.ordinal-got != start {
				t.Errorf("shard %d: ordinal − rollovers = %d, want %d on every shard of one run",
					s.ordinal, s.ordinal-got, start)
			}
		}
		if start != 1 {
			t.Errorf("the run started on ordinal %d, want 1 for an untouched range", start)
		}
	})

	// A run that hands over NOTHING says so: 0 is a value, not an absent key.
	// Without it a reader could not tell a first shard from a missing counter.
	t.Run("a run without a handover states zero", func(t *testing.T) {
		a9Truncate(t, pool)
		wl3Run(t, pool, a9Source(1, 2, 0), 6000, 0)
		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d block(s), want 1", len(blocks))
		}
		cov := wl3Coverage(t, pool, blocks[0].title)
		if got, ok := cov["shard_rollovers"]; !ok || got != 0 {
			t.Errorf("shard_rollovers = %d (present=%v), want 0", got, ok)
		}
	})

	// THE SEED'S OWN HANDOVER COUNTS TOO, and this probe pins where the counting
	// STARTS — measured at the code after the first version of this probe
	// expected two steps over two sealed shards and got one.
	//
	// The group query hands the seed the HIGHEST existing shard in a single read;
	// the seed does not walk the chain from the bottom. So a run over two sealed
	// shards opens ON shard 2, finds it sealed, hands over once and writes shard
	// 3 — one handover, and `ordinal − rollovers = 2` names the shard the run
	// opened on rather than the range's first. That is the honest reading of the
	// subtraction, and the field's doc says so.
	t.Run("a run that opens on a sealed chain counts from the shard it found", func(t *testing.T) {
		a9Truncate(t, pool)
		for n := 1; n <= 2; n++ {
			title := distillBlockTitle(dfRoot, 0, n)
			wl1Put(t, pool, title, wl1FullBody(t, "SEALED"), "insight", wl1Meta(dfRoot, n))
			wl2AssertSealed(t, pool, title)
		}

		wl3Run(t, pool, a9Source(1, 2, 0), 6000, 0)

		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) != 3 {
			t.Fatalf("%d shard(s), want the two sealed ones plus the opened third", len(shards))
		}
		cov := wl3Coverage(t, pool, shards[2].title)
		if got := cov["shard_rollovers"]; got != 1 {
			t.Errorf("shard 3 states %d handovers, want the 1 the seed made from shard 2", got)
		}
		if got := shards[2].ordinal - cov["shard_rollovers"]; got != 2 {
			t.Errorf("ordinal − rollovers = %d, want 2 — the running shard the seed found", got)
		}
		// The counter stands on the shards a run WROTE, and on no others: the
		// hand-laid fixture carries no coverage object at all, so one appearing on
		// it would mean this run had rewritten a sealed shard.
		if got := wl3Coverage(t, pool, shards[1].title); len(got) != 0 {
			t.Errorf("the sealed shard 2 carries a coverage block — this run rewrote it: %v", got)
		}
	})
}

// TestDistillShardGroupRead is the W-L2 review's finding #2, closed and pinned:
// the group read moves the bodies of the arm's OWN rows and is bounded in rows.
//
// Red on the unchanged tree, measured with this exact fixture: 8 390 671 B
// (8,0 MiB) over three rows at max_block_runes = 6 000
// (zzwl3_rot_integration_test.go, "TestWL3RotGroupBytes").
func TestDistillShardGroupRead(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// wl3GroupBytes runs the arm's own query with the arm's own arguments and
	// sums what comes back — the byte axis the review measured.
	wl3GroupBytes := func(t *testing.T, opts distillWriteOpts) (bytes, rows int) {
		t.Helper()
		args, _ := distillShardGroupArgs(opts, dfRoot, 0)
		r, err := pool.Query(context.Background(), distillShardGroupQuery, args...)
		if err != nil {
			t.Fatalf("group query: %v", err)
		}
		defer r.Close()
		for r.Next() {
			var title, typeName, content string
			var hint *string
			if err := r.Scan(&title, &hint, &typeName, &content); err != nil {
				t.Fatalf("scan: %v", err)
			}
			rows++
			bytes += len(content)
			t.Logf("row %q type=%s content=%d B", title, typeName, len(content))
		}
		return bytes, rows
	}

	// THE MASS GROUP, in the review's own shape: the arm's own shard 1, a
	// stranger carrying the group keys under a foreign title with a 4 MiB body,
	// and a foreign-typed shard 2 with another 4 MiB.
	t.Run("a planted mass group no longer moves its foreign bodies", func(t *testing.T) {
		a9Truncate(t, pool)
		mass := strings.Repeat("x", 4<<20)
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 1), wl1Body(t, "SHARD1", 2), "insight",
			wl1Meta(dfRoot, 1))
		wl1Put(t, pool, "Eine voellig fremde Zeile", mass, "knowledge", wl1Meta(dfRoot, 9))
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 2), mass, "knowledge", wl1Meta(dfRoot, 2))

		opts := wl3Opts(6000, 0)
		bytes, rows := wl3GroupBytes(t, opts)
		t.Logf("the group read moved %d B over %d rows", bytes, rows)
		// TWO rows, not the fixture's three: the stranger carries the group keys
		// but not a chain title, and since the title predicate (review major #3)
		// it does not reach the read at all. The foreign-TYPED shard 2 does — it
		// is a chain title, and the seed has to see it to refuse over it.
		if rows != 2 {
			t.Errorf("%d rows, want the 2 chain rows of the fixture", rows)
		}
		if bytes > opts.maxRunes {
			t.Errorf("the group read moved %d B — more than one block's cap of %d, so a foreign body "+
				"still travels", bytes, opts.maxRunes)
		}

		// THE IDENTITY MUST SURVIVE THE BYTE BOUND, and this is the half a
		// WHERE-clause type predicate would have broken: the foreign type on the
		// RUNNING shard is still seen, so the seed refuses instead of writing into
		// the shard below it (W-L1's own gate).
		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		_, err := s.distillSeedBlock(context.Background(), opts, dfRoot, 0)
		var held *distillTypeHeld
		if err == nil || !errors.As(err, &held) {
			t.Fatalf("seed error = %v, want a type refusal on the shard-2 title", err)
		}
		if held.have != "knowledge" || !strings.Contains(held.title, "Teil 2") {
			t.Errorf("refusal names %q on %q, want knowledge on the shard-2 title", held.have, held.title)
		}
	})

	// REVIEW FINDING #3 AND ITS REST R2-#1: the row limit must bound the CHAIN,
	// never let planted rows evict it — and the planted rows have to be the
	// HOSTILE class, not the one the filter removes anyway.
	//
	// WHAT THE FIRST FIX GOT WRONG (W19, second occurrence in this wave, named
	// rather than buried): the probe planted `"Eine voellig fremde Zeile %d"` —
	// titles WITHOUT the chain prefix, exactly what the new title predicate drops
	// server-side. The re-review planted titles WITH the prefix and reproduced the
	// original eviction byte for byte: `<base> — Teil ZZZZ…`, `<base> — Teil 2x`
	// and `<base> — Teil 99999999993` all pass a prefix LIKE, and the
	// `length(title) DESC` order even puts the long junk FIRST, so it survives the
	// truncation reliably. Measured on a5fc128b: `running=1 found=false lower=0`.
	//
	// Every row this table plants carries the group keys and the arm's own type,
	// i.e. the strongest form the attacker model allows (a write into the
	// reserved category).
	t.Run("planted chain-prefix titles do not evict the real chain", func(t *testing.T) {
		base := distillBlockTitle(dfRoot, 0, 1)
		for _, tc := range []struct{ name, title string }{
			{"long junk ordinal", base + distillShardSuffix + strings.Repeat("Z", 40)},
			{"trailing garbage", base + distillShardSuffix + "2x"},
			{"absurd ordinal", base + distillShardSuffix + "99999999993"},
			{"leading zero", base + distillShardSuffix + "007"},
			// Five digits: past the identity's FORM limit, so not a chain title at
			// all. It replaces the earlier "above the hard bound" case (ordinal
			// 265), which stopped belonging here with re-review R3: an ordinal
			// above the operating bound but inside the form IS canonical, the read
			// must see it — otherwise a lowered cap orphans the chain silently —
			// and the answer to it is the seed's `capped`, probed in
			// "a chain above the cap stays visible after the cap is lowered".
			{"past the form limit", base + distillShardSuffix +
				strconv.Itoa(distillShardMaxOrdinal+1)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a9Truncate(t, pool)
				// Three planted rows first, so they take the LIMIT's slots if the
				// filter lets them.
				for i := 1; i <= 3; i++ {
					wl1Put(t, pool, tc.title+strings.Repeat(" ", i-1), wl1Body(t, "PLANT", 1),
						"insight", wl1Meta(dfRoot, 9))
				}
				wl1Put(t, pool, base, wl1Body(t, "SHARD1", 2), "insight", wl1Meta(dfRoot, 1))
				wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 2), wl1Body(t, "SHARD2", 2), "insight",
					wl1Meta(dfRoot, 2))

				s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
				g, err := s.distillReadShardGroup(context.Background(), wl3Opts(6000, 2), dfRoot, 0)
				if err != nil {
					t.Fatalf("group read: %v", err)
				}
				if g.running != 2 || !g.found {
					t.Errorf("running/found = %d/%v, want 2/true — planted rows took the chain's slots",
						g.running, g.found)
				}
				if len(g.lower) == 0 {
					t.Error("the cross-shard dedup set is empty — the sealed shard was evicted")
				}
				t.Logf("running=%d found=%v lower=%d", g.running, g.found, len(g.lower))
			})
		}
	})

	// R2-#5: the LIKE escape has to be sharp, and only a row that EXPLOITS the
	// unescaped wildcard under slot pressure can show it. `dfRoot` carries two
	// `_`, so without the escape a title differing exactly there
	// (`…20260712X205012…`) matches this root's chain pattern; with two-digit
	// ordinals it is also LONGER than the real shards, so `length(title) DESC`
	// hands it the limit's slots first.
	//
	// The Go side would still refuse to OPEN such a row (its prefix is not this
	// chain's), which is why the probe measures the slots — that is where the
	// escape is load-bearing. Verified discriminating: with
	// distillShardLikeEscape reduced to the identity this probe goes red.
	t.Run("an unescaped wildcard row does not take the chain's slots", func(t *testing.T) {
		a9Truncate(t, pool)
		base := distillBlockTitle(dfRoot, 0, 1)
		near := strings.Replace(base, "20260712_205012", "20260712X205012", 1)
		if near == base || !strings.Contains(base, "_") {
			t.Fatalf("the fixture root carries no `_` — the probe measures nothing (%q)", base)
		}
		for n := 10; n <= 12; n++ {
			wl1Put(t, pool, near+distillShardSuffix+strconv.Itoa(n), wl1Body(t, "WILDCARD", 1),
				"insight", wl1Meta(dfRoot, n))
		}
		wl1Put(t, pool, base, wl1Body(t, "SHARD1", 2), "insight", wl1Meta(dfRoot, 1))
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 2), wl1Body(t, "SHARD2", 2), "insight",
			wl1Meta(dfRoot, 2))

		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		g, err := s.distillReadShardGroup(context.Background(), wl3Opts(6000, 2), dfRoot, 0)
		if err != nil {
			t.Fatalf("group read: %v", err)
		}
		if g.running != 2 || !g.found {
			t.Errorf("running/found = %d/%v, want 2/true — rows differing only at the `_` took the "+
				"chain's slots", g.running, g.found)
		}
		if len(g.lower) == 0 {
			t.Error("the cross-shard dedup set is empty — the wildcard rows evicted the sealed shard")
		}
		if now := wl1Content(t, pool, near+distillShardSuffix+"12"); !strings.Contains(now, "WILDCARD") {
			t.Error("a near-miss row was written into")
		}
	})

	// R3-#1: a chain that grew under a HIGHER cap has to stay visible after the
	// cap is lowered — reading is bounded by the identity's FORM, never by the
	// operating cap. Otherwise lowering the cap makes the upper shards vanish
	// from the read without a signal (the truncation warning hangs on
	// `read >= limit`, and four visible rows never reach a limit of 256), the seed
	// opens a shard BELOW the orphan, and the chain forks silently.
	//
	// Red on 2c1ab943 with exactly this chain (re-review probe D): at cap 0 the
	// read answered `running=256` and shards 257/300/400 were invisible; with
	// shards 1 and 2 full the seed then wrote a NEW shard 3 under the orphaned
	// 300 — ordinals [1 2 3 300], dedup set blind, no warning.
	t.Run("a chain above the cap stays visible after the cap is lowered", func(t *testing.T) {
		a9Truncate(t, pool)
		chain := []int{1, 2, 200, 256, 257, 300, 400}
		for _, n := range chain {
			// A distinct marker per shard: g.lower is a set of rendered LINES, so
			// identical bodies would collapse into one entry and the assertion
			// below would measure nothing.
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), wl1Body(t, fmt.Sprintf("SHARD%d", n), 1),
				"insight", wl1Meta(dfRoot, n))
		}
		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)

		// The cap the chain grew under, and the lowered one: both must see the
		// same running shard, because the cap bounds WRITING and not READING.
		for _, cap := range []int{400, 0} {
			g, err := s.distillReadShardGroup(context.Background(), wl3Opts(6000, cap), dfRoot, 0)
			if err != nil {
				t.Fatalf("group read at cap %d: %v", cap, err)
			}
			if g.running != 400 {
				t.Errorf("cap %d: running = %d, want 400 — the chain above the cap fell out of the "+
					"read", cap, g.running)
			}
			if len(g.lower) != len(chain)-1 {
				t.Errorf("cap %d: dedup set holds %d shards, want %d", cap, len(g.lower), len(chain)-1)
			}
		}

		// AND THE FOLLOW-ON: with the lower shards full, the seed must not open a
		// shard UNDER the orphan. It answers `capped` instead — loud and lossless.
		a9Truncate(t, pool)
		full := wl1FullBody(t, "SEALED")
		for _, n := range []int{1, 2} {
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), full, "insight", wl1Meta(dfRoot, n))
		}
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 300), full, "insight", wl1Meta(dfRoot, 300))

		st, err := s.distillSeedBlock(context.Background(), wl3Opts(6000, 0), dfRoot, 0)
		if err != nil {
			t.Fatalf("seed over the orphaned chain: %v", err)
		}
		if st.ordinal != 300 || !st.capped {
			t.Errorf("the seed answers ordinal %d capped=%v, want 300/true — it opened a shard under "+
				"the chain's head", st.ordinal, st.capped)
		}

		logs := n15CaptureLogs(t, func() { wl3Run(t, pool, a9Source(1, 2, 0), 6000, 0) })
		var got []int
		for _, sh := range wl2Shards(t, pool, dfRoot) {
			got = append(got, sh.ordinal)
		}
		if len(got) != 3 {
			t.Errorf("ordinals %v after the run, want the fixture's three — the chain forked", got)
		}
		if !strings.Contains(logs, "hard shard bound") {
			t.Errorf("the run stopped without naming the bound.\nCaught: %s",
				logs[max(0, len(logs)-400):])
		}
		t.Logf("ordinals after the capped run: %v", got)
	})

	// R2-#4: the ORDER BY has to be pinned, or a later change takes it back
	// silently. A chain across the digit boundary with a small limit: the read
	// must keep the HIGHEST ordinals, which is what makes g.running correct and
	// what makes a truncated read cost only the lowest members of the dedup set.
	t.Run("a truncated read keeps the highest shards", func(t *testing.T) {
		a9Truncate(t, pool)
		for n := 1; n <= 12; n++ {
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), wl1Body(t, "SHARD", 1), "insight",
				wl1Meta(dfRoot, n))
		}
		opts := wl3Opts(6000, 2) // LIMIT 3
		args, limit := distillShardGroupArgs(opts, dfRoot, 0)
		rows, err := pool.Query(context.Background(), distillShardGroupQuery, args...)
		if err != nil {
			t.Fatalf("group query: %v", err)
		}
		defer rows.Close()
		var got []int
		base := distillBlockTitle(dfRoot, 0, 1)
		for rows.Next() {
			var title, typeName, content string
			var hint *string
			if err := rows.Scan(&title, &hint, &typeName, &content); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n, ok := distillShardOrdinalFrom(base, title)
			if !ok {
				t.Fatalf("the read returned a non-chain title: %q", title)
			}
			got = append(got, n)
		}
		want := []int{12, 11, 10}
		if len(got) != limit {
			t.Fatalf("the read returned %d rows at limit %d", len(got), limit)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("read ordinals %v, want %v — a truncated read must keep the highest shards",
					got, want)
				break
			}
		}
		sch := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		g, err := sch.distillReadShardGroup(context.Background(), opts, dfRoot, 0)
		if err != nil {
			t.Fatalf("group read: %v", err)
		}
		if g.running != 12 {
			t.Errorf("running = %d, want 12 — the truncated read lost the running shard", g.running)
		}
	})

	// THE ROW BOUND, both shapes: tied to the cap where one is configured, and
	// the named constant where it is not.
	t.Run("the read is bounded in rows", func(t *testing.T) {
		a9Truncate(t, pool)
		for n := 1; n <= 5; n++ {
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), wl1Body(t, "SHARD", 1), "insight",
				wl1Meta(dfRoot, n))
		}
		if _, rows := wl3GroupBytes(t, wl3Opts(6000, 2)); rows != 3 {
			t.Errorf("%d rows at cap 2, want 3 (the cap plus the one row of head room)", rows)
		}
		if _, rows := wl3GroupBytes(t, wl3Opts(6000, 0)); rows != 5 {
			t.Errorf("%d rows at cap 0, want all 5 — %d is the bound, and it must not bind here",
				rows, distillShardGroupMaxRows)
		}
	})
}
