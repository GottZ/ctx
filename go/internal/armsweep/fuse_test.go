package armsweep_test

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/rrf"
)

func rank(v int) *int { return &v }

// rc is the reciprocal of an arm rank at the live k, computed at RUNTIME.
//
// It exists because Go folds an all-constant expression at arbitrary precision:
// writing the expected score as `0.20*1*1*(1.0/62) + 0.25*1*1*(1.0/63)` yields
// the correctly rounded value of the REAL sum, which differs from the float64
// the same expression produces at runtime — measured here at 9e-19 on case "b".
// Every expectation below therefore runs through float64 variables, so the test
// compares float64 against float64 and can afford exact equality.
func rc(r int, k float64) float64 { return 1.0 / (k + float64(r)) }

// row builds one ctx_rrf_arms row with neutral factors unless overridden.
func row(id string, sem, de, en, tri *int, mass, typ float64) rrf.ArmRow {
	return rrf.ArmRow{
		ID: id, RankSemantic: sem, RankFTSDe: de, RankFTSEn: en, RankTrigram: tri,
		MassFactor: mass, TypeFactor: typ,
	}
}

// TestFuseHandComputed pins the production fusion against numbers worked out by
// hand, in the SQL's term order (139:335-339 `w * mass * type * recip`, summed
// semantic → fts_de → fts_en → trigram). A regression here cannot hide behind
// "the database agrees with it", because the database is not in this test.
func TestFuseHandComputed(t *testing.T) {
	rows := []rrf.ArmRow{
		row("a", rank(1), rank(1), rank(1), rank(1), 1, 1),
		row("b", nil, rank(2), rank(3), nil, 1, 1),
		row("c", rank(2), nil, nil, nil, 0.5, 0.3),
		row("d", nil, nil, nil, nil, 1, 1),
	}
	w, one := armsweep.LiveWeights, 1.0
	wantA := w.Semantic*one*one*rc(1, 60) + w.FTSDe*one*one*rc(1, 60) +
		w.FTSEn*one*one*rc(1, 60) + w.Trigram*one*one*rc(1, 60)
	wantB := w.FTSDe*one*one*rc(2, 60) + w.FTSEn*one*one*rc(3, 60)
	mtC := 0.5 * 0.3
	wantC := w.Semantic * mtC * rc(2, 60)

	got := armsweep.Fuse(rows, armsweep.ConfigV0())
	want := []struct {
		id    string
		score float64
	}{{"a", wantA}, {"b", wantB}, {"c", wantC}, {"d", 0}}
	if len(got) != len(want) {
		t.Fatalf("Fuse returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].id {
			t.Errorf("position %d: id = %s, want %s", i, got[i].ID, want[i].id)
		}
		if got[i].Score != want[i].score {
			t.Errorf("position %d (%s): score = %.17g, want %.17g", i, got[i].ID, got[i].Score, want[i].score)
		}
	}
}

// TestFuseTiebreakIsIDAscending pins the Gen-17 tiebreak (migration 139: ORDER
// BY r.score DESC, cb.id) on an EXACT float64 tie, not a near-tie.
//
// The pair is 0.20·2⁻⁶ (fts_de rank 4) against 0.25·fl(1/80) (fts_en rank 20).
// Both weights are scaled by a power of two, so both products are exact
// rescalings of fl(0.2): 0.20·2⁻⁶ = fl(0.2)/64 and 0.25·fl(1/80) =
// fl(0.2)/16/4 = fl(0.2)/64. Equality is bit-level, so the ordering rests
// entirely on the id comparison.
func TestFuseTiebreakIsIDAscending(t *testing.T) {
	de := row("zzz", nil, rank(4), nil, nil, 1, 1)
	en := row("aaa", nil, nil, rank(20), nil, 1, 1)

	got := armsweep.Fuse([]rrf.ArmRow{de, en}, armsweep.ConfigV0())
	if got[0].Score != got[1].Score {
		t.Fatalf("fixture is not an exact tie: %.20g vs %.20g", got[0].Score, got[1].Score)
	}
	if got[0].ID != "aaa" || got[1].ID != "zzz" {
		t.Errorf("tie order = %s,%s — want aaa,zzz (score DESC, id ASC)", got[0].ID, got[1].ID)
	}

	// Input order must not decide it either.
	got = armsweep.Fuse([]rrf.ArmRow{en, de}, armsweep.ConfigV0())
	if got[0].ID != "aaa" {
		t.Errorf("tie order depends on input order: head = %s", got[0].ID)
	}
}

// TestFuseNilRanksContributeZero pins the COALESCE(1/(60+rank), 0) semantics:
// a missing arm removes a term, never the candidate.
func TestFuseNilRanksContributeZero(t *testing.T) {
	got := armsweep.Fuse([]rrf.ArmRow{row("x", nil, nil, nil, nil, 1, 1)}, armsweep.ConfigV0())
	if len(got) != 1 || got[0].Score != 0 {
		t.Errorf("all-nil row = %v, want one row scoring exactly 0", got)
	}
}

// TestFuseV1MergesTheFTSArms pins V1: the two FTS arms collapse to one whose
// rank is min(rank_de, rank_en), weighted 0.45 next to semantic 0.45 and
// trigram 0.10.
func TestFuseV1MergesTheFTSArms(t *testing.T) {
	cfg := mustConfig(t, "V1")
	one := 1.0
	r := row("a", nil, rank(7), rank(3), nil, 1, 1)
	want := cfg.Weights.FTSDe * one * rc(3, 60) // min(7,3) = 3
	got := armsweep.Fuse([]rrf.ArmRow{r}, cfg)
	if got[0].Score != want {
		t.Errorf("V1 score = %.17g, want %.17g (min rank 3, weight 0.45)", got[0].Score, want)
	}

	// Only one arm present: the merge must not turn a nil into rank 0.
	r2 := row("b", nil, rank(7), nil, nil, 1, 1)
	want2 := cfg.Weights.FTSDe * one * rc(7, 60)
	if got2 := armsweep.Fuse([]rrf.ArmRow{r2}, cfg); got2[0].Score != want2 {
		t.Errorf("V1 one-sided score = %.17g, want %.17g", got2[0].Score, want2)
	}

	// Neither arm present: zero, and no panic on a nil-nil minimum.
	r3 := row("c", rank(1), nil, nil, nil, 1, 1)
	want3 := cfg.Weights.Semantic * one * rc(1, 60)
	if got3 := armsweep.Fuse([]rrf.ArmRow{r3}, cfg); got3[0].Score != want3 {
		t.Errorf("V1 no-FTS score = %.17g, want %.17g", got3[0].Score, want3)
	}

	// The merged arm must carry the SUM of the two weights it replaces, so the
	// comparison isolates the merge rather than a reweighting inside it.
	if cfg.Weights.FTSEn != 0 {
		t.Errorf("V1 fts_en weight = %v, want 0 (the merged arm rides on fts_de)", cfg.Weights.FTSEn)
	}
	if sum := armsweep.LiveWeights.FTSDe + armsweep.LiveWeights.FTSEn; cfg.Weights.FTSDe != sum {
		t.Errorf("V1 merged weight = %v, want %v (0.20 + 0.25)", cfg.Weights.FTSDe, sum)
	}
}

// TestFuseV5PlacesTheFactorsOutsideTheSum pins V5: mass·type multiplies the
// SUM, not each term. The two forms are the same real number and, at these
// magnitudes, generally not the same float64 — which is the entire point of
// the configuration, so the test asserts the exact expression, not a tolerance.
func TestFuseV5PlacesTheFactorsOutsideTheSum(t *testing.T) {
	// mass = 1/17, type = 3/23: a pair searched for BECAUSE the two placements
	// disagree there. At the obvious round values (0.7, 0.3) they happen to
	// coincide bit for bit, and a test on those numbers would pass against an
	// implementation that ignored the flag entirely.
	mass, typ := 1.0/17.0, 3.0/23.0
	w := armsweep.LiveWeights
	r := row("a", rank(1), rank(2), rank(3), rank(4), mass, typ)
	want := mass * typ * (w.Semantic*rc(1, 60) + w.FTSDe*rc(2, 60) + w.FTSEn*rc(3, 60) + w.Trigram*rc(4, 60))
	got := armsweep.Fuse([]rrf.ArmRow{r}, mustConfig(t, "V5"))
	if got[0].Score != want {
		t.Errorf("V5 score = %.20g, want %.20g", got[0].Score, want)
	}

	mt := mass * typ
	inside := w.Semantic*mt*rc(1, 60) + w.FTSDe*mt*rc(2, 60) + w.FTSEn*mt*rc(3, 60) + w.Trigram*mt*rc(4, 60)
	if inside == want {
		t.Fatal("fixture does not discriminate: both factor placements yield the same float64")
	}
	if got[0].Score == inside {
		t.Error("V5 scored with the factors INSIDE the sum — the flag had no effect")
	}
	t.Logf("factor placement delta: outside = %.20g, inside = %.20g, delta = %.3g",
		want, inside, math.Abs(want-inside))
}

// TestFuseSoloArms pins S1-S4: exactly one weight survives.
func TestFuseSoloArms(t *testing.T) {
	r := row("a", rank(1), rank(2), rank(3), rank(4), 1, 1)
	for _, tc := range []struct {
		name string
		want float64
	}{
		{"S1", rc(1, 60)},
		{"S2", rc(2, 60)},
		{"S3", rc(3, 60)},
		{"S4", rc(4, 60)},
	} {
		got := armsweep.Fuse([]rrf.ArmRow{r}, mustConfig(t, tc.name))
		if got[0].Score != tc.want {
			t.Errorf("%s score = %.17g, want %.17g", tc.name, got[0].Score, tc.want)
		}
	}
}

// TestFuseKSweep pins V7a-c: only the reciprocal constant moves.
func TestFuseKSweep(t *testing.T) {
	r := row("a", rank(1), nil, nil, nil, 1, 1)
	for _, tc := range []struct {
		name string
		k    float64
	}{{"V7a", 10}, {"V7b", 30}, {"V7c", 120}} {
		cfg := mustConfig(t, tc.name)
		if cfg.K != tc.k {
			t.Errorf("%s k = %v, want %v", tc.name, cfg.K, tc.k)
		}
		want := armsweep.LiveWeights.Semantic * 1.0 * rc(1, tc.k)
		if got := armsweep.Fuse([]rrf.ArmRow{r}, cfg); got[0].Score != want {
			t.Errorf("%s score = %.17g, want %.17g", tc.name, got[0].Score, want)
		}
	}
}

// TestDeriveV6 pins the nDCG-proportional weight derivation: w ∝ nDCG_solo^β,
// normalised to the live weight sum of 1. β=1 is proportional, β=2 sharpens.
func TestDeriveV6(t *testing.T) {
	solo := armsweep.SoloNDCG{Semantic: 0.4, FTSDe: 0.2, FTSEn: 0.3, Trigram: 0.1}

	a := armsweep.DeriveV6("V6a", solo, 1)
	sum := a.Weights.Semantic + a.Weights.FTSDe + a.Weights.FTSEn + a.Weights.Trigram
	if math.Abs(sum-1) > 1e-12 {
		t.Errorf("V6a weights sum to %v, want 1", sum)
	}
	if math.Abs(a.Weights.Semantic-0.4) > 1e-12 || math.Abs(a.Weights.Trigram-0.1) > 1e-12 {
		t.Errorf("V6a weights = %+v, want the normalised solo profile", a.Weights)
	}

	b := armsweep.DeriveV6("V6b", solo, 2)
	den := 0.16 + 0.04 + 0.09 + 0.01
	if math.Abs(b.Weights.Semantic-0.16/den) > 1e-12 {
		t.Errorf("V6b semantic = %v, want %v", b.Weights.Semantic, 0.16/den)
	}
	if b.Weights.Semantic <= a.Weights.Semantic {
		t.Errorf("β=2 must sharpen the profile: %v vs %v", b.Weights.Semantic, a.Weights.Semantic)
	}

	// A degenerate profile (every arm at 0) must not divide by zero; the live
	// weights are the documented fallback and the note records it.
	z := armsweep.DeriveV6("V6a", armsweep.SoloNDCG{}, 1)
	if z.Weights != armsweep.LiveWeights {
		t.Errorf("degenerate solo profile: weights = %+v, want the live weights", z.Weights)
	}
	if z.Note == "" {
		t.Error("degenerate derivation must carry a note")
	}
}

// TestConfigsAreCanonicalAndComplete pins the report's column set and its
// order: 16 configurations, V0 and V0' identical in parameters (they differ
// only in WHICH dump they are scored on), and no duplicate names.
func TestConfigsAreCanonicalAndComplete(t *testing.T) {
	names := armsweep.ConfigNames()
	want := []string{"V0", "V0'", "S1", "S2", "S3", "S4", "V1", "V2", "V3", "V4", "V5",
		"V6a", "V6b", "V7a", "V7b", "V7c"}
	if len(names) != len(want) {
		t.Fatalf("ConfigNames = %v (%d), want %d entries", names, len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, names[i], want[i])
		}
	}
	if mustConfig(t, "V0'").Weights != armsweep.ConfigV0().Weights {
		t.Error("V0' must carry the live weights — it is the replicate, not a variant")
	}
	if armsweep.ConfigV0().Weights != armsweep.LiveWeights || armsweep.ConfigV0().K != 60 {
		t.Errorf("V0 = %+v, want the live fusion (0.45/0.20/0.25/0.10, k=60)", armsweep.ConfigV0())
	}
}
