//go:build integration

// Wave W-L1 — the identity's third axis, database half (amendment C4-2,
// design/02-destillat-arm.md §A.2/A.3, wave gate §A.6 "W-L1").
//
// The half that needs no database — the title function, its inverse and the
// metadata key set — is in distill_block_test.go.
//
// WHAT THIS WAVE DOES NOT DO, and what therefore has no green probe here: it
// never MOVES from one shard to the next. The rollover at the blockFull point
// is W-L2's one change, so every run below writes exactly the shard its seed
// opened. Fixtures for the higher shards are therefore laid down through the
// production write path and not produced by a run.
//
//	go test -tags=integration ./internal/events/ -run TestDistillShard -count=1 -v
package events

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// wl1Opts is the write identity of every probe in this file — the one a8Config
// resolves in production, spelled out so a probe that seeds by hand and a run
// that writes agree on category, scope and type.
func wl1Opts() distillWriteOpts {
	o := a9Opts()
	o.scope = dfScope
	return o
}

// wl1Body renders a body the arm recognises as its own (distillSplitCarry
// accepts it), with a marker inside every claim so the carry a seed loaded is
// identifiable by its content rather than by its length.
func wl1Body(t *testing.T, marker string, n int) string {
	t.Helper()
	st := a9State(0)
	for i := 1; i <= n; i++ {
		st.insights = append(st.insights, distillKept{
			claim:   fmt.Sprintf("%s Kernaussage %d ueber den Retrieval-Pfad.", marker, i),
			quote:   strings.Repeat("Zitat-Text aus dem Roh-Transkript, wortgetreu uebernommen. ", 3),
			blockID: a9Part1, chunk: i,
		})
	}
	content, over := distillRenderBlock(st, a9Opts())
	if over != 0 {
		t.Fatalf("fixture body of %d insights does not fit the cap", n)
	}
	return content
}

// wl1FullBody is a body that fills its shard: it still fits
// distill.max_block_runes, and the state a SEED builds from it answers full().
//
// Grown against the production predicate rather than guessed at a number. Two
// reasons, both measured while writing this file: the cap is a rune count over
// rendered lines, so a literal would drift with every render change — and the
// frame of a seeded state is SHORTER than the frame of the state that rendered
// the fixture (a fresh accumulator names 0 parts and no manifest), so a body
// grown against the fixture's own frame comes back not-full one shard later.
func wl1FullBody(t *testing.T, marker string) string {
	t.Helper()
	opts := wl1Opts()
	stamp := time.Unix(1787893000, 0).UTC()
	newState := func() *distillBlockState {
		st := newDistillBlockState(dfRoot, "", 0)
		st.createdAt = stamp
		return st
	}
	var claims, evidence []string
	for n := 1; n <= 64; n++ {
		c, e := distillInsightLine(distillKept{
			claim:   fmt.Sprintf("%s Kernaussage %d ueber den Retrieval-Pfad.", marker, n),
			quote:   strings.Repeat("Zitat-Text aus dem Roh-Transkript, wortgetreu uebernommen. ", 3),
			blockID: a9Part1, chunk: n,
		})
		claims, evidence = append(claims, c), append(evidence, e)

		// Rendered WITHOUT the cap on purpose: distillRenderBlock reserves the
		// overflow note before it admits a line, so a body grown through it stops
		// a note's worth below the cap and the next-insight estimate never
		// crosses. The block under probe is a state, not a run — what has to hold
		// of it is that it fits the cap and that full() calls it full.
		body := distillRenderN(newState(), opts, claims, evidence, 0)
		probe := newState()
		carry, ok := distillSplitCarry(body)
		if !ok {
			t.Fatalf("the arm cannot read back its own fixture at n=%d", n)
		}
		probe.carry = carry
		if probe.full(opts) {
			if got := utf8.RuneCountInString(body); got > opts.maxRunes {
				t.Fatalf("the full fixture is %d runes over the cap of %d", got, opts.maxRunes)
			}
			return body
		}
	}
	t.Fatal("no body of marker insights fills a shard below the cap")
	return ""
}

// wl1Meta is the group's two discovery keys plus the two this wave adds.
// ordinal <= 0 leaves BOTH shard keys out, which is the shape of every block
// written before this wave (A.3 c: the 16 measured stock blocks).
func wl1Meta(root string, ordinal int) map[string]any {
	md := map[string]any{"root_session_id": root, "watermark_from": int64(0)}
	if ordinal > 0 {
		md[distillMetaShardOrdinal] = ordinal
		md[distillMetaShardOfWM] = int64(0)
	}
	return md
}

// wl1Put lays down one block through the production write path.
func wl1Put(t *testing.T, pool *pgxpool.Pool, title, body, typeName string, md map[string]any) {
	t.Helper()
	if _, err := store.UpsertBlock(context.Background(), pool, "session-insights", title, body,
		nil, md, dfScope, true,
		store.SensitivityWrite{Value: backends.SensCredentials, Derived: true}, typeName); err != nil {
		t.Fatalf("seed %q: %v", title, err)
	}
}

// wl1Chain lays down shards 1..n of dfRoot's epoch range, each with its own
// marker, and answers their titles.
func wl1Chain(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	titles := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		title := distillBlockTitle(dfRoot, 0, i)
		wl1Put(t, pool, title, wl1Body(t, fmt.Sprintf("SHARD%d", i), 2), "insight", wl1Meta(dfRoot, i))
		titles = append(titles, title)
	}
	return titles
}

// wl1Seed runs the production seed for dfRoot's epoch range.
func wl1Seed(t *testing.T, pool *pgxpool.Pool, root string) *distillBlockState {
	t.Helper()
	s := a8Scheduler(pool, a8Config(), a9Source(1, 2, 0), nil)
	st, err := s.distillSeedBlock(context.Background(), wl1Opts(), root, 0)
	if err != nil {
		t.Fatalf("seed %q: %v", root, err)
	}
	return st
}

func wl1Content(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var c string
	if err := pool.QueryRow(context.Background(),
		`SELECT content FROM context_blocks WHERE category='session-insights' AND title=$1 AND scope=$2
		   AND NOT is_archived`, title, dfScope).Scan(&c); err != nil {
		t.Fatalf("read %q: %v", title, err)
	}
	return c
}

// TestDistillShardSeed is the wave's green gate on the read side: the seed
// opens the RUNNING shard and loads ITS carry.
//
// The red is recorded in the wave report and was measured on the unchanged
// tree: with a hand-laid shard 2 present, the seed loaded shard 1's two claims
// (SHARD1=true, SHARD2=false) and the arm wrote into shard 1.
func TestDistillShardSeed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// GREEN, the gate's own wording: "der Seed öffnet bei drei vorhandenen
	// Shards den dritten und lädt DESSEN Carry".
	t.Run("three shards: the seed opens the third and loads its carry", func(t *testing.T) {
		a9Truncate(t, pool)
		wl1Chain(t, pool, 3)

		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 3 {
			t.Fatalf("ordinal = %d, want 3 — the running shard is the highest one", st.ordinal)
		}
		carried := strings.Join(st.carry.claims, "")
		if !strings.Contains(carried, "SHARD3") {
			t.Errorf("the carry does not come from shard 3: %q", carried)
		}
		for _, foreign := range []string{"SHARD1", "SHARD2"} {
			if strings.Contains(carried, foreign) {
				t.Errorf("the carry mixes in %s — the seed loaded more than the running shard", foreign)
			}
		}
		if st.carry.count() != 2 {
			t.Errorf("carry holds %d claims, want the 2 of shard 3", st.carry.count())
		}
	})

	// NEGATIVE PROBE, STOCK COMPATIBILITY (A.3 c). The 16 measured blocks of the
	// measure copy carry neither shard key; they must be read as shard 1 and
	// their carry must be identical to the pre-wave one.
	t.Run("stock blocks without a shard key are shard 1 and their carry is unchanged", func(t *testing.T) {
		a9Truncate(t, pool)
		type stock struct{ root, title, body string }
		var stocks []stock
		for i := 0; i < 16; i++ {
			root := fmt.Sprintf("2026081%d_1158%02d_ecbc11%02d", i%10, i, i)
			title := distillBlockTitle(root, 0, 1)
			body := wl1Body(t, fmt.Sprintf("BESTAND%02d", i), 2+i%3)
			wl1Put(t, pool, title, body, "insight", wl1Meta(root, 0)) // no shard keys
			stocks = append(stocks, stock{root, title, body})
		}
		var withKey int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks
			 WHERE category='session-insights' AND metadata ? $1`, distillMetaShardOrdinal).Scan(&withKey); err != nil {
			t.Fatalf("count: %v", err)
		}
		if withKey != 0 {
			t.Fatalf("%d of the stock fixtures carry shard_ordinal — they are not stock", withKey)
		}

		for _, s := range stocks {
			st := wl1Seed(t, pool, s.root)
			if st.ordinal != 1 {
				t.Errorf("%s: ordinal = %d, want 1 — a stock block is shard 1", s.root, st.ordinal)
				continue
			}
			// The carry a pre-wave seed would have produced, byte for byte.
			want, ok := distillSplitCarry(s.body)
			if !ok {
				t.Fatalf("%s: the fixture body is not readable at all", s.root)
			}
			if strings.Join(st.carry.claims, "") != strings.Join(want.claims, "") ||
				strings.Join(st.carry.evidence, "") != strings.Join(want.evidence, "") {
				t.Errorf("%s: the carry changed against the pre-wave read", s.root)
			}
		}
	})

	// NEGATIVE PROBE, NULL ORDINAL (W-L0 addendum to the gate). The stock block
	// has no shard_ordinal, so the amendment's `ORDER BY
	// (metadata->>'shard_ordinal')::int` sorts it AGAINST the shards. The
	// counter-version is executed here rather than described, so the reason this
	// wave derives the ordinal from the title stays checked.
	t.Run("a NULL ordinal is shard 1 and never the end of the order", func(t *testing.T) {
		a9Truncate(t, pool)
		// Shard 1 in its STOCK shape: no shard keys at all.
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 1), wl1Body(t, "BESTAND", 2), "insight", wl1Meta(dfRoot, 0))
		for _, n := range []int{2, 3} {
			wl1Put(t, pool, distillBlockTitle(dfRoot, 0, n), wl1Body(t, fmt.Sprintf("SHARD%d", n), 2),
				"insight", wl1Meta(dfRoot, n))
		}

		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 3 {
			t.Fatalf("ordinal = %d, want 3", st.ordinal)
		}
		if carried := strings.Join(st.carry.claims, ""); !strings.Contains(carried, "SHARD3") {
			t.Errorf("the seed did not open shard 3: %q", carried)
		}

		// THE COUNTER-VERSION, run as SQL: both naive orderings pick the stock
		// block. Measured on the unchanged tree as the wave's red point 2.
		naive := `SELECT COALESCE(metadata->>'shard_ordinal','NULL') FROM context_blocks
		           WHERE NOT is_archived AND category=$1 AND scope=$2
		             AND metadata @> jsonb_build_object('root_session_id', $3::text,
		                                                'watermark_from',  $4::bigint)
		           ORDER BY (metadata->>'shard_ordinal')::int`
		rows, err := pool.Query(ctx, naive, "session-insights", dfScope, dfRoot, int64(0))
		if err != nil {
			t.Fatalf("counter-version: %v", err)
		}
		var order []string
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				t.Fatalf("scan: %v", err)
			}
			order = append(order, o)
		}
		rows.Close()
		if len(order) != 3 || order[len(order)-1] != "NULL" {
			t.Fatalf("counter-version order = %v — the probe no longer shows the trap", order)
		}
		var desc string
		if err := pool.QueryRow(ctx, strings.Replace(naive,
			"ORDER BY (metadata->>'shard_ordinal')::int",
			"ORDER BY (metadata->>'shard_ordinal')::int DESC LIMIT 1", 1),
			"session-insights", dfScope, dfRoot, int64(0)).Scan(&desc); err != nil {
			t.Fatalf("counter-version desc: %v", err)
		}
		if desc != "NULL" {
			t.Fatalf("counter-version DESC picked %q — the probe no longer shows the trap", desc)
		}
		t.Logf("counter-version: ASC order %v (last row is the stock block), DESC LIMIT 1 picks %q — "+
			"both would open the stock block instead of shard 3", order, desc)
	})

	// NEGATIVE PROBE, GAP. A.3 (a) names the gap as the probing fallback's trap;
	// the primary variant sees every shard in one query and therefore opens the
	// highest one rather than the first hole. Probed because the archived middle
	// shard is the measure wells' standing practice (N-15).
	t.Run("an archived middle shard does not become the end of the chain", func(t *testing.T) {
		a9Truncate(t, pool)
		titles := wl1Chain(t, pool, 3)
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true
			  WHERE category='session-insights' AND title=$1 AND scope=$2`, titles[1], dfScope); err != nil {
			t.Fatalf("archive shard 2: %v", err)
		}
		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 3 {
			t.Fatalf("ordinal = %d, want 3 — the gap was read as the end of the chain", st.ordinal)
		}
		if carried := strings.Join(st.carry.claims, ""); !strings.Contains(carried, "SHARD3") {
			t.Errorf("carry = %q, want shard 3's", carried)
		}
	})

	// NEGATIVE PROBE, THE COUNTER-VERSION'S TITLE. A block named as the rejected
	// "suffix at n = 1" version would have named it is NOT a member of the
	// chain — otherwise two titles would claim ordinal 1.
	t.Run("a counter-version title is not a chain member", func(t *testing.T) {
		a9Truncate(t, pool)
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 1), wl1Body(t, "BESTAND", 2), "insight", wl1Meta(dfRoot, 0))
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 1)+distillShardSuffix+"1",
			wl1Body(t, "GEGENFASSUNG", 2), "insight", wl1Meta(dfRoot, 1))

		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 1 {
			t.Fatalf("ordinal = %d, want 1", st.ordinal)
		}
		if carried := strings.Join(st.carry.claims, ""); !strings.Contains(carried, "BESTAND") ||
			strings.Contains(carried, "GEGENFASSUNG") {
			t.Errorf("the seed opened the counter-version's block: %q", carried)
		}
	})

	// NEGATIVE PROBE, A PLANTED GROUP ROW. The metadata half of the query is a
	// discovery key, not an identity: a row that carries it under a foreign
	// title is left alone and does not redirect the arm.
	t.Run("a foreign title carrying the group keys is not opened", func(t *testing.T) {
		a9Truncate(t, pool)
		wl1Put(t, pool, distillBlockTitle(dfRoot, 0, 1), wl1Body(t, "SHARD1", 2), "insight", wl1Meta(dfRoot, 1))
		wl1Put(t, pool, "Ein ganz anderer Block", wl1Body(t, "FREMD", 2), "insight", wl1Meta(dfRoot, 9))

		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 1 {
			t.Fatalf("ordinal = %d, want 1 — a planted metadata row moved the identity", st.ordinal)
		}
		if carried := strings.Join(st.carry.claims, ""); !strings.Contains(carried, "SHARD1") ||
			strings.Contains(carried, "FREMD") {
			t.Errorf("carry = %q", carried)
		}
		if got := wl1Content(t, pool, "Ein ganz anderer Block"); !strings.Contains(got, "FREMD") {
			t.Error("the planted row was touched")
		}
	})

	// NEGATIVE PROBE, THE UNADDRESSABLE ROOT. §4.4.3 re-types every foreign
	// string, so a root that fails distillMetaValue stands in the metadata as ""
	// while the title carries it verbatim. The group query cannot find such a
	// block — and the arm must NOT therefore replace its own shard 1.
	t.Run("a root that fails the metadata type check stays on shard 1 and keeps its carry", func(t *testing.T) {
		a9Truncate(t, pool)
		root := "2026 07/12 root"
		if distillMetaValue(root) == root {
			t.Fatal("the fixture root survives distillMetaValue — the probe measures nothing")
		}
		// The row as the arm itself would have written it: title verbatim,
		// root_session_id emptied by the type check.
		wl1Put(t, pool, distillBlockTitle(root, 0, 1), wl1Body(t, "UNADDRESSABLE", 3), "insight",
			map[string]any{"root_session_id": "", "watermark_from": int64(0)})

		st := wl1Seed(t, pool, root)
		if st.ordinal != 1 {
			t.Fatalf("ordinal = %d, want 1", st.ordinal)
		}
		if st.carry.count() != 3 || !strings.Contains(strings.Join(st.carry.claims, ""), "UNADDRESSABLE") {
			t.Errorf("the carry of an unaddressable root was lost: %d claims", st.carry.count())
		}

		// The guard itself, pinned: a planted shard-2 row carrying the RAW root
		// in its metadata IS reachable by the group query — only the guard keeps
		// the arm off it. Without the guard this seed opens ordinal 2 and reads
		// the planted carry.
		wl1Put(t, pool, distillBlockTitle(root, 0, 2), wl1Body(t, "PLANT", 2), "insight",
			map[string]any{"root_session_id": root, "watermark_from": int64(0)})
		st = wl1Seed(t, pool, root)
		if st.ordinal != 1 {
			t.Fatalf("ordinal = %d, want 1 — the planted raw-root row redirected the arm", st.ordinal)
		}
		if carried := strings.Join(st.carry.claims, ""); strings.Contains(carried, "PLANT") ||
			!strings.Contains(carried, "UNADDRESSABLE") {
			t.Errorf("carry = %q, want the shard-1 carry, untouched by the plant", carried)
		}
	})

	// An empty range opens shard 1 with an empty carry — the cold start, and the
	// state every first run of a root is in.
	t.Run("an untouched range opens shard 1", func(t *testing.T) {
		a9Truncate(t, pool)
		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 1 || st.carry.count() != 0 {
			t.Errorf("ordinal/carry = %d/%d, want 1/0", st.ordinal, st.carry.count())
		}
	})
}

// TestDistillShardWrite is the wave's green gate on the write side, and it
// carries the type-guard negative probe the amendment asks for by name.
func TestDistillShardWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key := distillSourceKey(dfLabel, dfScope, dfRoot)

	journal := func(t *testing.T) (string, string) {
		t.Helper()
		var outcome, errClass string
		if err := pool.QueryRow(ctx, `
			SELECT outcome, COALESCE(error,'') FROM distill_run
			 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&outcome, &errClass); err != nil {
			t.Fatalf("journal: %v", err)
		}
		return outcome, errClass
	}

	// NEGATIVE PROBE, BEHAVIOURAL INVARIANCE. Without a shard chain the wave
	// changes nothing observable: one run, one block, the pre-wave title, and
	// the journal balance of the tree before it.
	t.Run("without a chain the run writes exactly one block under the stock title", func(t *testing.T) {
		a9Truncate(t, pool)
		a9Run(t, pool, a9Source(1, 2, 0))

		blocks := a9Blocks(t, pool)
		if len(blocks) != 1 {
			t.Fatalf("%d blocks, want 1", len(blocks))
		}
		if blocks[0].title != distillBlockTitle(dfRoot, 0, 1) {
			t.Errorf("title = %q, want the stock title", blocks[0].title)
		}
		if strings.Contains(blocks[0].title, distillShardSuffix) {
			t.Error("the first block of a root carries a shard suffix")
		}
		var ord, ofWM int64
		if err := pool.QueryRow(ctx, `SELECT (metadata->>$1)::bigint, (metadata->>$2)::bigint
			  FROM context_blocks WHERE title=$3 AND scope=$4`,
			distillMetaShardOrdinal, distillMetaShardOfWM,
			blocks[0].title, dfScope).Scan(&ord, &ofWM); err != nil {
			t.Fatalf("read shard keys: %v", err)
		}
		if ord != 1 || ofWM != 0 {
			t.Errorf("shard_ordinal/shard_of_watermark = %d/%d, want 1/0", ord, ofWM)
		}
		if outcome, errClass := journal(t); outcome != distillOutcomeOk || errClass != "" {
			t.Errorf("journal = %q/%q, want ok/\"\"", outcome, errClass)
		}
	})

	// GREEN, the whole wave in one behavioural probe: the run grows the RUNNING
	// shard and leaves every lower one byte-identical. It creates no fourth
	// block — this wave has no rollover.
	t.Run("the run grows the running shard and leaves the lower ones untouched", func(t *testing.T) {
		a9Truncate(t, pool)
		titles := wl1Chain(t, pool, 3)
		before := []string{wl1Content(t, pool, titles[0]), wl1Content(t, pool, titles[1])}

		a9Run(t, pool, a9Source(1, 2, 0))

		if blocks := a9Blocks(t, pool); len(blocks) != 3 {
			t.Fatalf("%d blocks after the run, want 3 — this wave rolls nothing over", len(blocks))
		}
		grown := wl1Content(t, pool, titles[2])
		if !strings.Contains(grown, a8Claim) {
			t.Error("the running shard did not grow — the run's insights are missing")
		}
		if !strings.Contains(grown, "SHARD3") {
			t.Error("the running shard lost its carry")
		}
		if !strings.HasPrefix(grown, "# "+titles[2]) {
			t.Errorf("the head of shard 3 does not name shard 3: %q", grown[:min(len(grown), 120)])
		}
		for i, was := range before {
			if now := wl1Content(t, pool, titles[i]); now != was {
				t.Errorf("shard %d changed although the run wrote shard 3", i+1)
			}
		}
		if outcome, errClass := journal(t); outcome != distillOutcomeOk || errClass != "" {
			t.Errorf("journal = %q/%q, want ok/\"\"", outcome, errClass)
		}
	})

	// NEGATIVE PROBE, TYPE GUARD PER SHARD (the gate's own wording): a foreign
	// type on the SHARD-2 title ends the run as failed/block_write_failed, and
	// shard 1 stays untouched — the guard must not kill the whole group, and it
	// must not write around the squatter either.
	t.Run("a foreign type on the shard-2 title refuses the run and leaves shard 1 alone", func(t *testing.T) {
		a9Truncate(t, pool)
		shard1 := distillBlockTitle(dfRoot, 0, 1)
		shard2 := distillBlockTitle(dfRoot, 0, 2)
		wl1Put(t, pool, shard1, wl1Body(t, "SHARD1", 2), "insight", wl1Meta(dfRoot, 1))
		wl1Put(t, pool, shard2, "fremder Typ auf dem Shard-2-Titel", "knowledge", wl1Meta(dfRoot, 2))
		was1 := wl1Content(t, pool, shard1)

		logs := n15CaptureLogs(t, func() { a9Run(t, pool, a9Source(1, 2, 0)) })

		// The C4-5 diagnosis has to survive the new axis: with several shards per
		// range the source key no longer names the block, and the remedy the log
		// prints ("archive the squatting block") needs an address.
		for _, want := range []string{"have_type=knowledge", "want_type=insight", "Teil 2"} {
			if !strings.Contains(logs, want) {
				t.Errorf("the refusal log does not carry %q — the operator cannot tell WHICH shard is "+
					"squatted.\nCaught: %s", want, logs)
			}
		}

		if outcome, errClass := journal(t); outcome != distillOutcomeFailed || errClass != distillErrBlockWriteFailed {
			t.Errorf("journal = %q/%q, want failed/block_write_failed", outcome, errClass)
		}
		if now := wl1Content(t, pool, shard1); now != was1 {
			t.Error("shard 1 was touched — the guard wrote around the squatted shard")
		}
		if now := wl1Content(t, pool, shard2); now != "fremder Typ auf dem Shard-2-Titel" {
			t.Errorf("the foreign row was replaced: %q", now)
		}
		if blocks := a9Blocks(t, pool); len(blocks) != 2 {
			t.Errorf("%d blocks, want 2 — the run created a block despite the refusal", len(blocks))
		}
		if rows := a8Rows(t, pool); len(rows) != 0 {
			t.Errorf("%d llm calls for a run that could never write", len(rows))
		}
	})

	// The deliberate SCOPE of the guard, made visible rather than left implicit:
	// a foreign type on a LOWER shard does not stop the arm. It writes the shard
	// it opened and leaves the squatter standing. When W-L2 starts reading every
	// shard of the group for the cross-shard dedup, that row becomes material
	// and this probe has to be revisited.
	t.Run("a foreign type on a lower shard does not kill the chain", func(t *testing.T) {
		a9Truncate(t, pool)
		shard1 := distillBlockTitle(dfRoot, 0, 1)
		shard2 := distillBlockTitle(dfRoot, 0, 2)
		wl1Put(t, pool, shard1, "fremder Typ auf dem Shard-1-Titel", "knowledge", wl1Meta(dfRoot, 1))
		wl1Put(t, pool, shard2, wl1Body(t, "SHARD2", 2), "insight", wl1Meta(dfRoot, 2))

		a9Run(t, pool, a9Source(1, 2, 0))

		if outcome, errClass := journal(t); outcome != distillOutcomeOk || errClass != "" {
			t.Errorf("journal = %q/%q, want ok/\"\"", outcome, errClass)
		}
		if now := wl1Content(t, pool, shard1); now != "fremder Typ auf dem Shard-1-Titel" {
			t.Errorf("the foreign lower shard was written into: %q", now)
		}
		if now := wl1Content(t, pool, shard2); !strings.Contains(now, a8Claim) {
			t.Error("the running shard did not grow")
		}
	})

	// NEGATIVE PROBE, THE STANDING STATE MOVES WITH THE CHAIN. A full RUNNING
	// shard answers skipped/budget exactly as a full block did before the wave —
	// this wave moves the state one shard up, it does not resolve it. Resolving
	// it is W-L2.
	t.Run("a full running shard still answers skipped budget", func(t *testing.T) {
		a9Truncate(t, pool)
		shard1 := distillBlockTitle(dfRoot, 0, 1)
		shard2 := distillBlockTitle(dfRoot, 0, 2)
		wl1Put(t, pool, shard1, wl1Body(t, "SHARD1", 2), "insight", wl1Meta(dfRoot, 1))
		wl1Put(t, pool, shard2, wl1FullBody(t, "SHARD2"), "insight", wl1Meta(dfRoot, 2))
		st := wl1Seed(t, pool, dfRoot)
		if st.ordinal != 2 || !st.full(wl1Opts()) {
			t.Fatalf("ordinal=%d full=%v — the fixture is not the state under probe", st.ordinal, st.full(wl1Opts()))
		}
		before := wl1Content(t, pool, shard2)

		a9Run(t, pool, a9Source(1, 2, 0))

		var outcome, reason string
		if err := pool.QueryRow(ctx, `
			SELECT outcome, COALESCE(skip_reason,'') FROM distill_run
			 WHERE source_key=$1 ORDER BY started_at DESC LIMIT 1`, key).Scan(&outcome, &reason); err != nil {
			// A skip that is throttled writes no row at all; then the block must be
			// untouched and no call may have been made, which the assertions below
			// carry on their own.
			t.Logf("no journal row for the throttled skip: %v", err)
		} else if outcome != distillOutcomeSkipped || reason != distillSkipBudget {
			t.Errorf("journal = %q/%q, want skipped/budget", outcome, reason)
		}
		if now := wl1Content(t, pool, shard2); now != before {
			t.Error("the full running shard was written into")
		}
		if rows := a8Rows(t, pool); len(rows) != 0 {
			t.Errorf("%d llm calls although the running shard is full", len(rows))
		}
	})
}
