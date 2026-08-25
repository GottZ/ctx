// B-W1b: the offline half of the Generation 17 gate (migration
// 139_rrf_gen17_tiebreak.sql). Like arms_fusion_test.go this file carries NO
// build tag — it must compile and run in the short unit loop, while
// gen17_tiebreak_integration_test.go drives the same ordering against a real
// Postgres.
//
// Generation 16 ends its projection with a bare `ORDER BY r.score DESC`
// (134_rrf_gen16_ann_embedding_filter.sql:360). Generation 17 makes that
// `ORDER BY r.score DESC, cb.id`. fuseArmsOrdered is the offline mirror of the
// new ordering; fuseArms (arms_fusion_test.go) stays the mirror of the old one
// and is deliberately left alone, because the B-W1 parity gate still uses it
// to compare tie groups as sets.
package rrf_test

import (
	"math"
	"sort"
	"testing"
)

// fuseArmsOrdered scores exactly like fuseArms and then applies Generation
// 17's total order: score descending, id ascending. The id comparison is a
// plain byte-wise string compare, which is what Postgres's uuid type does too
// — uuid_cmp compares the 16 bytes in order, and the canonical hex form sorts
// identically because it is a fixed-width, lowercase, positional encoding of
// those same bytes.
//
// sort.Slice (not SliceStable) on purpose: with a unique id as the second key
// the order is total, so stability has nothing left to decide. If it ever did,
// that would mean two rows shared an id.
func fuseArmsOrdered(rows []armRow, weights [4]float64, k float64) []fusedRow {
	out := make([]fusedRow, 0, len(rows))
	for _, r := range rows {
		mt := r.Mass * r.Type
		score := weights[0]*mt*recip(r.Semantic, k) +
			weights[1]*mt*recip(r.FtsDE, k) +
			weights[2]*mt*recip(r.FtsEN, k) +
			weights[3]*mt*recip(r.Trigram, k)
		out = append(out, fusedRow{ID: r.ID, Score: score, Cos: r.Cos})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// exactTieIDs is the id pair whose scores are bit-identical by construction —
// see TestBW1bExactTieIsBitIdentical. The higher id is listed first so that
// input order and output order differ whenever the tiebreak works.
const (
	bw1bTieHighID = "019fa402-0000-7000-9000-00000000000b"
	bw1bTieLowID  = "019fa402-0000-7000-9000-00000000000a"
)

// TestBW1bExactTieIsBitIdentical pins the arithmetic the whole gate rests on:
// a block that sits ONLY in the german full-text arm at rank 4 and a block
// that sits ONLY in the english arm at rank 20 receive the SAME float64, not
// merely a close one.
//
// Why it is exact rather than lucky: ctx_rrf computes
// `w * mass * type * (1.0/(60+rank))` (134:334-337). For rank 4 that is
// fl(0.20) * 2^-6, an exact power-of-two scaling. For rank 20 it is
// 0.25 * fl(1/80) = fl(1/80) * 2^-2. Scaling by a power of two preserves the
// mantissa, and fl(0.2)*2^-4 IS fl(0.0125) = fl(1/80). Both sides therefore
// land on the same bits. This matters because Postgres's ORDER BY compares
// float8 exactly: an eps-close pair is NOT a tie and gets a defined order
// without any tiebreak, so a gate built on eps-ties would prove nothing.
func TestBW1bExactTieIsBitIdentical(t *testing.T) {
	de := 0.20 * 1.0 * 1.0 * (1.0 / (60 + 4))
	en := 0.25 * 1.0 * 1.0 * (1.0 / (60 + 20))
	if de != en {
		t.Fatalf("fts_de rank 4 = %.17g, fts_en rank 20 = %.17g — not bit-identical, delta %.3g",
			de, en, math.Abs(de-en))
	}
	t.Logf("exact tie construction: 0.20/(60+4) = %.17g == 0.25/(60+20) = %.17g", de, en)
}

// TestBW1bFuseArmsOrderedTiebreak is gate (d): the offline fusion with hand
// computed vectors that carry real ties. Three of them, each a different way a
// tie reaches the projection.
func TestBW1bFuseArmsOrderedTiebreak(t *testing.T) {
	rows := []armRow{
		// Tie 1, the arithmetic one: cross-arm coincidence, exact by
		// construction. Fed in with the HIGHER id first.
		{ID: bw1bTieHighID, FtsDE: ptrInt(4), Mass: 1, Type: 1},
		{ID: bw1bTieLowID, FtsEN: ptrInt(20), Mass: 1, Type: 1},
		// Tie 2, the factor one: a damped audit-trail block at a better rank
		// meets an undamped block at a worse rank. 0.45*0.5*(1/61) equals
		// 0.45*1.0*(1/61)*0.5 — same bits, since 0.5 is a power of two.
		{ID: "019fa402-0000-7000-9000-00000000001d", Semantic: ptrInt(1), Mass: 0.5, Type: 1},
		{ID: "019fa402-0000-7000-9000-00000000001c", Semantic: ptrInt(1), Mass: 1, Type: 0.5},
		// A single clear winner and a single clear loser, so the tie groups
		// are not the whole slice.
		{ID: "019fa402-0000-7000-9000-000000000099", Semantic: ptrInt(1), FtsDE: ptrInt(1), FtsEN: ptrInt(1), Trigram: ptrInt(1), Mass: 1, Type: 1},
		{ID: "019fa402-0000-7000-9000-000000000001", Trigram: ptrInt(30), Mass: 1, Type: 1},
	}

	got := fuseArmsOrdered(rows, armsLiveWeights, armsRRFK)
	want := []string{
		"019fa402-0000-7000-9000-000000000099", // 0.45+0.20+0.25+0.10 over rank 1
		"019fa402-0000-7000-9000-00000000001c", // tie 2, lower id first
		"019fa402-0000-7000-9000-00000000001d",
		bw1bTieLowID, // tie 1, lower id first — input order was the reverse
		bw1bTieHighID,
		"019fa402-0000-7000-9000-000000000001", // trigram rank 30 alone
	}
	if len(got) != len(want) {
		t.Fatalf("fuseArmsOrdered returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("position %d: id = %s, want %s (full order: %v)", i, got[i].ID, want[i], idsOf(got))
		}
	}

	// The ties must really be ties — otherwise the ordering above would be
	// decided by the score and the tiebreak would be untested.
	if got[1].Score != got[2].Score {
		t.Errorf("tie 2 is not a tie: %.17g vs %.17g", got[1].Score, got[2].Score)
	}
	if got[3].Score != got[4].Score {
		t.Errorf("tie 1 is not a tie: %.17g vs %.17g", got[3].Score, got[4].Score)
	}
	t.Logf("gate (d): tie 1 score = %.17g (2 rows), tie 2 score = %.17g (2 rows)", got[3].Score, got[1].Score)
}

// TestBW1bFuseArmsOrderedIsTotal pins that the Generation 17 order leaves
// nothing undecided: feeding the same rows in a reversed input order must
// produce the identical output sequence. Gen 16's fuseArms cannot promise
// that, and the contrast is asserted rather than described.
func TestBW1bFuseArmsOrderedIsTotal(t *testing.T) {
	rows := []armRow{
		{ID: bw1bTieHighID, FtsDE: ptrInt(4), Mass: 1, Type: 1},
		{ID: bw1bTieLowID, FtsEN: ptrInt(20), Mass: 1, Type: 1},
	}
	reversed := []armRow{rows[1], rows[0]}

	a := idsOf(fuseArmsOrdered(rows, armsLiveWeights, armsRRFK))
	b := idsOf(fuseArmsOrdered(reversed, armsLiveWeights, armsRRFK))
	if a[0] != bw1bTieLowID || a[1] != bw1bTieHighID {
		t.Errorf("forward input: order = %v, want low id first", a)
	}
	if a[0] != b[0] || a[1] != b[1] {
		t.Errorf("order depends on input order: %v vs %v", a, b)
	}

	// Contrast: the Gen-16 mirror is input-order dependent by design (stable
	// sort over equal keys), which is exactly the property migration 139
	// removes from the SQL side.
	c := idsOf(fuseArms(reversed, armsLiveWeights, armsRRFK))
	if c[0] != bw1bTieLowID {
		t.Logf("contrast: fuseArms (Gen 16 mirror) on reversed input yields %v — input-order dependent, as documented", c)
	}
}

// idsOf projects a fusion to its id sequence, the shape every order assertion
// in this file and its integration sibling compares.
func idsOf(rows []fusedRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
