//go:build integration

// Wave W-L4 — the shard chain in the block text, measured on STORED rows
// (amendment C4-2, design/02-destillat-arm.md §A.6 "W-L4", gates :3310-3321).
//
// WHAT THIS FILE PROBES AND THE UNIT TESTS CANNOT. The chain line's whole
// purpose is what a reader of the row sees — MCP get, the web UI, the synthesis
// prompt — so the line has to survive the production write path and come back
// out of the database, and it must stay invisible to the two parsers that read
// those same rows: the running shard's carry and the cross-shard dedup set of
// distillReadShardGroup.
//
// NO REAL LLM CALL: the stub sits behind the backend seam exactly as in W-L1,
// W-L2 and W-L3, so everything above it is production code. No docker against
// the live system or the measure copy; every fixture goes down through the
// production write path.
//
//	go test -tags=integration ./internal/events/ -run TestDistillShardChain -count=1 -v
package events

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/redact"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/internal/util"
)

// wl4Body renders a body of n marker insights AS SHARD `ordinal` — i.e. with
// the chain line at every ordinal above 1, which is the shape a stored shard 2
// has from this wave on.
//
// It uses dfRoot and the epoch watermark because the chain line names a title,
// and a fixture whose body names another range than its row would prove nothing
// about the round trip.
func wl4Body(t *testing.T, marker string, n, ordinal int) string {
	t.Helper()
	st := newDistillBlockState(dfRoot, "", 0)
	st.createdAt = time.Unix(1787893000, 0).UTC()
	st.ordinal = ordinal
	for i := 1; i <= n; i++ {
		st.insights = append(st.insights, distillKept{
			claim:   fmt.Sprintf("%s Kernaussage %d ueber den Retrieval-Pfad.", marker, i),
			quote:   strings.Repeat("Zitat-Text aus dem Roh-Transkript, wortgetreu uebernommen. ", 3),
			blockID: a9Part1, chunk: i,
		})
	}
	content, over := distillRenderBlock(st, wl1Opts())
	if over != 0 {
		t.Fatalf("fixture body of %d insights does not fit the cap", n)
	}
	return content
}

func TestDistillShardChainInWrittenBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// THE WAVE'S RED, ON A ROW THE ARM ITSELF WROTE (design/02:3311): before
	// this wave the stored shard-2 body named its predecessor nowhere, and a
	// reader over MCP get saw an excerpt that reads as a whole account. The red
	// was measured on the unchanged tree with the same construction (report §3).
	t.Run("a shard the arm writes after a rollover names its predecessor", func(t *testing.T) {
		a9Truncate(t, pool)
		shard1 := distillBlockTitle(dfRoot, 0, 1)
		wl1Put(t, pool, shard1, wl1FullBody(t, "SHARD1"), "insight", wl1Meta(dfRoot, 1))
		before := wl1Content(t, pool, shard1)

		a8ClearWindow(t, pool)
		wl2Run(t, pool, a9Source(2, 3, 0), 6000)

		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) < 2 {
			t.Fatalf("%d shard(s) — the run did not roll over, the probe measures nothing", len(shards))
		}
		if now := wl1Content(t, pool, shard1); now != before {
			t.Error("shard 1 changed — the rollover wrote into the sealed shard")
		}
		if line := wl4Line(before); line != "" {
			t.Errorf("the sealed shard 1 carries a chain line: %q", line)
		}

		body := wl1Content(t, pool, shards[1].title)
		line := wl4Line(body)
		if line == "" {
			t.Fatalf("the stored shard 2 carries no chain line:\n%s", body)
		}
		if !strings.HasPrefix(line, "Teil 2 dieses Bereichs") || !strings.Contains(line, shard1) {
			t.Errorf("the stored chain line does not name part and predecessor: %q", line)
		}
		// THE GREEN GATE ON A STORED ROW: inside the window the synthesis prompt
		// sees, whole, measured at the cut and not on the source text.
		cut := util.TruncateRunesWithSuffix(body, redact.Truncated, llm.MaxBlockChars)
		if !strings.Contains(cut, line) {
			t.Errorf("the chain line is not complete inside the first %d runes of the stored body",
				llm.MaxBlockChars)
		}
		if i := strings.Index(cut, "**UNTRUSTED, abgeleitet.**"); i < 0 ||
			utf8.RuneCountInString(cut[:i]) > 200 {
			t.Error("UNTRUSTED left the first 200 runes of the stored body")
		}
		t.Logf("stored shard 2 head:\n%s", body[:strings.Index(body, distillSecClaims)])
	})

	// THE ROUND TRIP THE BRIEFING ASKS FOR BY NAME: a shard 2 whose body carries
	// the chain line must hand the seed the SAME carry as the same body without
	// it — the line may never be read as a claim, neither by the carry nor by
	// the dedup set of the sealed shards.
	t.Run("the chain line changes neither the carry nor the cross-shard dedup set", func(t *testing.T) {
		withChain := wl4Body(t, "SHARD2", 2, 2)
		chain := distillChainLine(dfRoot, 0, 2)
		if !strings.Contains(withChain, chain) {
			t.Fatal("the fixture body carries no chain line — the probe measures nothing")
		}
		plain := strings.Replace(withChain, chain, "", 1)

		seed := func(body string) *distillBlockState {
			a9Truncate(t, pool)
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 1), wl1Body(t, "SHARD1", 2), "insight",
				wl1Meta(dfRoot, 1))
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 2), body, "insight", wl1Meta(dfRoot, 2))
			return wl1Seed(t, pool, dfRoot)
		}

		a, b := seed(withChain), seed(plain)
		if a.ordinal != 2 || b.ordinal != 2 {
			t.Fatalf("the seed opened shard %d / %d, want 2 in both cases", a.ordinal, b.ordinal)
		}
		if a.carry.count() != b.carry.count() || a.carry.count() != 2 {
			t.Fatalf("carry with the chain line holds %d claims, without it %d — want 2 in both",
				a.carry.count(), b.carry.count())
		}
		for i := range a.carry.claims {
			if a.carry.claims[i] != b.carry.claims[i] {
				t.Errorf("carried claim %d differs:\n with %q\nwithout %q",
					i, a.carry.claims[i], b.carry.claims[i])
			}
		}
		for i := range a.carry.evidence {
			if a.carry.evidence[i] != b.carry.evidence[i] {
				t.Errorf("carried evidence %d differs:\n with %q\nwithout %q",
					i, a.carry.evidence[i], b.carry.evidence[i])
			}
		}
		for _, l := range append(append([]string{}, a.carry.claims...), a.carry.evidence...) {
			if strings.Contains(l, "Fortsetzung von") {
				t.Errorf("the chain line was carried as a per-insight line: %q", l)
			}
		}

		// The dedup set of the SEALED shards, read the way the run reads it.
		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		g, err := s.distillReadShardGroup(ctx, wl1Opts(), dfRoot, 0)
		if err != nil {
			t.Fatalf("group read: %v", err)
		}
		if g.running != 2 {
			t.Errorf("the group read answers running shard %d, want 2", g.running)
		}
		if len(g.lower) != 2 {
			t.Errorf("the dedup set holds %d lines, want shard 1's 2 claims", len(g.lower))
		}
		for l := range g.lower {
			if strings.Contains(l, "Fortsetzung von") || !strings.HasPrefix(l, "- ") {
				t.Errorf("a non-claim line entered the dedup set: %q", l)
			}
		}
	})

	// MATERIAL FIDELITY WITH THE LINE IN PLACE: the chain line is charged to
	// distill.max_block_runes, so a shard holds fewer insights than before — and
	// the insights it can no longer hold must roll on, not fall out. Measured
	// over a chain the run grows itself, against the dedup ledger: the stub
	// answers exactly one claim per chunk, so "claims == ledger rows" means
	// nothing bought was thrown away.
	t.Run("the runes the chain line costs are handed on, not lost", func(t *testing.T) {
		a9Truncate(t, pool)
		key := distillSourceKey(dfLabel, dfScope, dfRoot)
		for tick := 1; tick <= 4; tick++ {
			a8ClearWindow(t, pool)
			wl3Run(t, pool, a9Source(2, 6, 0), 2600, 0)
		}
		shards := wl2Shards(t, pool, dfRoot)
		if len(shards) < 2 {
			t.Fatalf("%d shard(s) at a cap of 2600 runes — the probe measures no chain", len(shards))
		}
		claims := len(wl3AllClaims(t, pool))
		if seen := wl3Seen(t, pool, key); claims != seen {
			t.Errorf("%d claims over %d shards against %d ledger rows — the chain line's runes cost "+
				"material", claims, len(shards), seen)
		}
		for _, sh := range shards {
			body := wl1Content(t, pool, sh.title)
			line := wl4Line(body)
			if sh.ordinal == 1 && line != "" {
				t.Errorf("shard 1 carries a chain line: %q", line)
			}
			if sh.ordinal > 1 && !strings.Contains(line, distillBlockTitle(dfRoot, 0, sh.ordinal-1)) {
				t.Errorf("shard %d names %q as predecessor, want %q",
					sh.ordinal, line, distillBlockTitle(dfRoot, 0, sh.ordinal-1))
			}
		}
		t.Logf("%d shards, %d claims, ledger %d", len(shards), claims, wl3Seen(t, pool, key))
	})

	// THE HANDOVER THAT OVERFLOWS — the wave's own hold path (distillRollShard).
	// At max_block_runes 2200 a shard held exactly two insight pairs with 22
	// runes to spare before this wave; the chain line takes 124 of them, so a
	// handover of two insights places one and the fresh shard is full again.
	//
	// Red on the same tree with the hold removed (report §7, mutation NA):
	// 10 claims over the shards against 12 ledger rows, watermark past them,
	// source at rest. Green: the run ends with budget, the batch is cut, and the
	// range waits with everything it bought still in a shard.
	t.Run("a handover that fills the next shard holds its batch back", func(t *testing.T) {
		a9Truncate(t, pool)
		key := distillSourceKey(dfLabel, dfScope, dfRoot)
		rest := wl3RunUntilQuiet(t, pool, key, 2200, 4, 12)
		claims, ledger := len(wl3AllClaims(t, pool)), wl3Seen(t, pool, key)
		if claims != ledger {
			t.Errorf("%d claims over the shards against %d ledger rows — the handover threw bought "+
				"material away", claims, ledger)
		}
		if !strings.Contains(rest.logs, "handed over into a shard that is full again") {
			t.Error("no run reported a handover into a full shard — the probe measures another path")
		}
		if missing := wl2Unseen(t, pool, key, 2, 6, rest.totals.wmTo); len(missing) > 0 {
			t.Errorf("watermark %d covers %d chunk(s) that never reached a call: %v",
				rest.totals.wmTo, len(missing), missing)
		}
		t.Logf("claims=%d ledger=%d ticks=%d stalled=%v watermark=%d",
			claims, ledger, rest.ticks, rest.stalled, rest.totals.wmTo)
	})

	// THE BAND BETWEEN THE TWO FLOORS — the wave's operating cost, sonded
	// (review finding #1). A shard above the first carries a longer frame than
	// shard 1 (title suffix + chain line), so `distill.max_block_runes` has TWO
	// floors, and between them shard 1 still takes an insight while its
	// successors take none.
	//
	// Red on this tree before the refusal in distillRollShard, at the reviewer's
	// geometry (1900, no cap): 9 shards after 8 ticks, 8 of them EMPTY at ~1 726
	// runes of pure frame — one written block per tick, up to the hard bound of
	// 256, each one carried by every later group read. Green: the range rests
	// after one hand-over, keeps everything it bought, and resumes the moment the
	// key is raised.
	t.Run("a max_block_runes inside the shard band rests instead of growing empty shards", func(t *testing.T) {
		a9Truncate(t, pool)
		key := distillSourceKey(dfLabel, dfScope, dfRoot)
		for tick := 1; tick <= 8; tick++ {
			a8ClearWindow(t, pool)
			wl3Run(t, pool, a9Source(2, 6, 0), 1900, 0)
		}
		shards := wl2Shards(t, pool, dfRoot)
		empty := 0
		for _, sh := range shards {
			if len(sh.claims) == 0 {
				empty++
			}
		}
		// One hand-over may still happen before anything is moving: with an empty
		// overflow the question falls back on the theoretical floor, and that one
		// fits. What must NOT happen is a shard per tick.
		if len(shards) > 2 {
			t.Errorf("%d shards after 8 ticks in the band — the range grows a shard per tick "+
				"instead of resting", len(shards))
		}
		if empty > 1 {
			t.Errorf("%d empty shards — each one is corpus that no later run removes", empty)
		}
		if claims, ledger := len(wl3AllClaims(t, pool)), wl3Seen(t, pool, key); claims != ledger {
			t.Errorf("%d claims against %d ledger rows — resting in the band cost material",
				claims, ledger)
		}
		t.Logf("band 1900: %d shards (%d empty) after 8 ticks", len(shards), empty)

		// AND IT WAKES UP. The band is a configuration, not a state the range
		// cannot leave: with the key raised the same source covers its material.
		for tick := 1; tick <= 6; tick++ {
			a8ClearWindow(t, pool)
			wl3Run(t, pool, a9Source(2, 6, 0), 6000, 0)
		}
		claims, ledger := len(wl3AllClaims(t, pool)), wl3Seen(t, pool, key)
		if claims != 12 || claims != ledger {
			t.Errorf("after raising max_block_runes: %d claims / %d ledger rows, want the fixture's "+
				"12 in both — the band must be a resting state, not a dead end", claims, ledger)
		}
		t.Logf("after raising the key: %d claims, %d ledger rows, %d shards",
			claims, ledger, len(wl2Shards(t, pool, dfRoot)))
	})

	// THE POST-WRITE HOLD, ISOLATED (review finding #6). The exit at
	// distill.go:1064 is the wave's heaviest — it is what keeps a hand-over that
	// placed less than it moved from booking the difference — and until this
	// round it was only reachable through a whole run. Here it is one call:
	// a state whose successor CAN take the first moved insight but not all of
	// them, so the write happens and the overflow survives it.
	t.Run("a hand-over that places less than it moved answers held", func(t *testing.T) {
		a9Truncate(t, pool)
		s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
		opts := wl1Opts()

		st := newDistillBlockState(dfRoot, "", 0)
		st.createdAt = time.Unix(1787893000, 0).UTC()
		st.shardCalls = 1
		for i := 1; i <= 3; i++ {
			st.overflowInsights = append(st.overflowInsights, distillKept{
				claim:   fmt.Sprintf("MOVED %d ueber den Retrieval-Pfad.", i),
				quote:   strings.Repeat("Zitat-Text aus dem Roh-Transkript, wortgetreu uebernommen. ", 3),
				blockID: a9Part1, chunk: i,
			})
		}
		st.overflow = len(st.overflowInsights)
		// A cap that takes the successor's frame plus TWO of the three moved
		// insights — measured against the production arithmetic, not guessed.
		next := *st
		next.ordinal++
		next.carry = distillCarry{}
		c, e := distillInsightLine(st.overflowInsights[0])
		pair := utf8.RuneCountInString(c) + utf8.RuneCountInString(e)
		opts.maxRunes = distillFrameRunes(&next, opts, 3) + 2*pair

		var l distillLedger
		stop, rolled, held, err := s.distillRollShard(ctx, distillTick{block: st, write: opts},
			"probe", distillExtractResult{}, &l)
		if err != nil {
			t.Fatalf("roll: %v", err)
		}
		if !rolled {
			t.Fatalf("the hand-over was refused (stop=%q) — the probe would measure the refusal, "+
				"not the hold", stop)
		}
		if stop != distillSkipBudget {
			t.Errorf("stop = %q, want %q — a hand-over that could not place everything ends the run",
				stop, distillSkipBudget)
		}
		if !held {
			t.Error("held = false — the insights the successor could not take would be booked as " +
				"covered, which is the loss this exit exists to prevent")
		}
		if l.blocksWritten != 1 {
			t.Errorf("blocksWritten = %d, want 1 — the successor was written before the hold",
				l.blocksWritten)
		}
		if n := len(st.overflowInsights); n != 1 {
			t.Errorf("%d insight(s) left over after the hand-over, want 1", n)
		}
		if st.ordinal != 2 {
			t.Errorf("ordinal = %d, want 2", st.ordinal)
		}
		body := wl1Content(t, pool, distillBlockTitle(dfRoot, 0, 2))
		if !strings.Contains(body, "MOVED 2 ") || strings.Contains(body, "MOVED 3 ") {
			t.Error("the written successor does not hold exactly the two insights it could take")
		}
	})
}
