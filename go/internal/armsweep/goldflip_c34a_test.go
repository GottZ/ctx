package armsweep_test

// Wave C3-4a, gate 6 of design/05a §C3-2-D05-7: the metric flip test.
//
// The same records are scored twice — once against the Fable gold of the core
// and once against the judge gold — and the two ΔnDCG@10 are compared. A sign
// change, or a paired 95 % CI of the difference that excludes 0, means the gate
// is "nicht entschieden" (§C3-2-D05-6).

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// rankAt is a fixed arm rank. The field is a pointer because "not in this arm"
// and "rank 0" are different states in the dump format.
func rankAt(v int) *int { return &v }

// flipRecords builds a core of n cases whose two candidate ids are ranked in
// opposite order by the two configurations under test: id "a" wins under the
// semantic-only weights, id "b" under the trigram-only ones. Gold A names "a",
// gold B names "b" — so the SAME comparison has opposite signs on the two gold
// sources, which is exactly the situation the flip test must catch.
func flipRecords(n int) ([]armsweep.Record, map[string][]string, map[string][]string) {
	var recs []armsweep.Record
	goldA := map[string][]string{}
	goldB := map[string][]string{}
	for i := 0; i < n; i++ {
		sha := goldset.SHA256Hex("flip-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		rec := armsweep.Record{
			Slice: goldset.SliceReal, Index: i, QuerySHA256: sha,
			EffectiveQuery: "synthetic", Attempts: 1,
			Rows: []rrf.ArmRow{
				{ID: "a", RankSemantic: rankAt(1), RankTrigram: rankAt(5), MassFactor: 1, TypeFactor: 1},
				{ID: "b", RankSemantic: rankAt(5), RankTrigram: rankAt(1), MassFactor: 1, TypeFactor: 1},
			},
			GoldIDs: []string{"a"},
		}
		recs = append(recs, rec)
		goldA[rec.Key()] = []string{"a"}
		goldB[rec.Key()] = []string{"b"}
	}
	return recs, goldA, goldB
}

func TestGoldFlipDetectsSignChangeC34A(t *testing.T) {
	recs, goldA, goldB := flipRecords(20)
	base := mustConfig(t, "S1")    // semantic solo — ranks "a" first
	variant := mustConfig(t, "S4") // trigram solo — ranks "b" first
	flip := armsweep.GoldFlip(recs, base, variant, goldA, goldB, armsweep.PrimaryLevel, 42)
	if !flip.Available || flip.N != len(recs) {
		t.Fatalf("Kipp-Rechnung nicht verfügbar: %+v", flip)
	}
	if !(flip.DeltaFable < 0 && flip.DeltaJudge > 0) {
		t.Fatalf("Vorbedingung verletzt — die Fixture kippt nicht: ΔA=%.5f ΔB=%.5f",
			flip.DeltaFable, flip.DeltaJudge)
	}
	if !flip.SignFlip() {
		t.Error("Vorzeichenwechsel nicht erkannt")
	}
	if !flip.Flipped() {
		t.Error("Kipp nicht gemeldet")
	}
}

func TestGoldFlipStaysQuietOnAgreementC34A(t *testing.T) {
	recs, goldA, _ := flipRecords(20)
	base := mustConfig(t, "S1")
	variant := mustConfig(t, "S4")
	flip := armsweep.GoldFlip(recs, base, variant, goldA, goldA, armsweep.PrimaryLevel, 42)
	if flip.SignFlip() {
		t.Error("identische Gold-Mengen als Vorzeichenwechsel gewertet")
	}
	if flip.Flipped() {
		t.Errorf("identische Gold-Mengen als Kipp gewertet: CI [%.5f, %.5f]", flip.DiffCILo, flip.DiffCIHi)
	}
	if math.Abs(flip.DeltaFable-flip.DeltaJudge) > 1e-12 {
		t.Errorf("dieselbe Gold-Menge liefert verschiedene Δ: %.9f vs %.9f", flip.DeltaFable, flip.DeltaJudge)
	}
}

// TestScoreCaseGoldIsScoreCaseOnC34A pins the non-regression of §C3-2-D05-8 (j):
// the new entry point with rec.GoldIDs must reproduce the existing scorer.
func TestScoreCaseGoldIsScoreCaseOnC34A(t *testing.T) {
	recs := synthDump(t, 0x0c34a)
	cfg := mustConfig(t, "V0")
	for _, rec := range recs {
		want := armsweep.ScoreCaseOn(rec, cfg, armsweep.RankingBasisFused)
		got := armsweep.ScoreCaseGold(rec, cfg, armsweep.RankingBasisFused, rec.GoldIDs)
		if want != got {
			t.Fatalf("Fall %s: %+v vs %+v", rec.Key(), want, got)
		}
	}
}
