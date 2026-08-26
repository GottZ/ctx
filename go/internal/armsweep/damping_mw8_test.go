package armsweep_test

import (
	"math"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// Wave M-W8 — the damping dimension of the offline sweep.
//
// The fixture below is built so damping can actually decide something: in every
// case the swept type holds semantic rank 1 and the gold block sits at rank 2
// with a per-case mass margin, so the gold overtakes the head exactly when the
// damping factor drops below that case's flip point. The flip points are placed
// strictly BETWEEN the ten support points, which is what turns the curve into
// ten different numbers instead of two plateaus.
//
// A fixture without that type mixture in the top ranks would let every one of
// these tests pass while doing nothing — see design 05 §7, M-W8 gate 2.

const (
	dampedType   = "catalog-proxy"
	undampedType = "knowledge"
)

// dampFlipPoints are the ten per-case flip points, one strictly inside each gap
// of armsweep.DampingStops (the last one lies above the whole grid, so that case
// is won at every support point and the curve has a constant to move against).
var dampFlipPoints = []float64{0.075, 0.125, 0.175, 0.25, 0.325, 0.425, 0.60, 0.775, 0.925, 1.10}

func rankPtr(v int) *int { return &v }

// dampRecords builds one case per flip point.
//
// Only the semantic arm fires, so the comparison at the top of each case is
// exactly mass·type/(k+rank) and the flip point is arithmetic, not luck:
// the gold row at rank 2 beats the head row at rank 1 iff
// factor < mass_gold · (k+1)/(k+2).
func dampRecords(headTypeFactor float64, withTypeNames bool) []armsweep.Record {
	out := make([]armsweep.Record, 0, len(dampFlipPoints))
	for i, flip := range dampFlipPoints {
		gold := "gold-" + string(rune('a'+i))
		head := "head-" + string(rune('a'+i))
		rows := []rrf.ArmRow{
			{ID: head, RankSemantic: rankPtr(1), MassFactor: 1, TypeFactor: headTypeFactor},
			{ID: gold, RankSemantic: rankPtr(2), TypeFactor: 1,
				MassFactor: flip * (armsweep.LiveK + 2) / (armsweep.LiveK + 1) / headTypeFactor},
		}
		for j := 3; j <= 12; j++ {
			rows = append(rows, rrf.ArmRow{
				ID:           "fill-" + string(rune('a'+i)) + string(rune('a'+j)),
				RankSemantic: rankPtr(j), MassFactor: 0.01, TypeFactor: 1,
			})
		}
		if withTypeNames {
			rows[0].TypeName = dampedType
			for j := 1; j < len(rows); j++ {
				rows[j].TypeName = undampedType
			}
		}
		rec := armsweep.Record{
			Slice: goldset.SliceKI, Index: i,
			QuerySHA256:    goldset.SHA256Hex("m-w8-" + gold),
			GoldIDs:        []string{gold},
			Rows:           rows,
			EffectiveQuery: "synthetic",
			Selector:       armsweep.Selector{Mode: "ann", Reason: "disabled"},
			Attempts:       1, LatencyMS: 100,
		}
		rec.FusionOrder = armsweep.FusedIDs(armsweep.Fuse(rec.Rows, armsweep.ConfigV0()))
		out = append(out, rec)
	}
	return out
}

func dampStamp(recs []armsweep.Record, migrationsMax int) armsweep.DumpStamp {
	return armsweep.DumpStamp{
		RunID: "MW8", CreatedAt: "2026-08-27T00:00:00Z", BaseURL: "http://ctx",
		Records: len(recs), DumpFile: "dumps/MW8.jsonl",
		PinFile: "pins-MW8.jsonl", PinRunID: "MW8", PinSHA256: goldset.SHA256Hex("MW8"),
		GoldStamp: goldset.SHA256Hex("stamp"), MigrationsMax: migrationsMax,
		PostFusionStages: map[string]any{
			"cluster.enabled": false, "cluster.inject_max": float64(0),
			"graph.enabled": false, "rerank.enabled": false,
		},
		SliceFiles: []armsweep.SliceDigest{
			{Slice: goldset.SliceKI, File: goldset.FileKI, SHA256: goldset.SHA256Hex("ki"), N: len(recs)},
		},
		Latency: armsweep.SummariseLatency([]int64{100, 200, 300}),
	}
}

func dampInput(migrationsMax int, withTypeNames bool, dampingType string) armsweep.ScoreInput {
	recs := dampRecords(1.0, withTypeNames)
	return armsweep.ScoreInput{
		RecordsA: recs, StampA: dampStamp(recs, migrationsMax), DampingType: dampingType,
		Seed: 20260812, GitRevision: "deadbeef",
		GoldStamp: goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825,
			CorpusMaxCreatedAt: "2026-08-25T13:49:49.736510Z"},
	}
}

func bodySHA(t *testing.T, body armsweep.ReportBody) string {
	t.Helper()
	b, err := armsweep.MarshalBody(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return goldset.SHA256Hex(string(b))
}

// --------------------------------------------------------------------------
// Gate 1 (green half): a damping value has to reach the report.

// TestDampingCurveMovesTheReport is the counterpart of the wave's red probe:
// before M-W8 a sweep "with damping 0.15" and one "with damping 0.85" produced
// byte-identical reports, because type_factor came from the dump row and no
// configuration touched it.
func TestDampingCurveMovesTheReport(t *testing.T) {
	plain := bodySHA(t, mustScore(t, dampInput(142, true, "")))
	curve := mustScore(t, dampInput(142, true, dampedType))
	withCurve := bodySHA(t, curve)
	t.Logf("without damping: %s", plain)
	t.Logf("with damping on %s: %s", dampedType, withCurve)
	if plain == withCurve {
		t.Fatalf("the damping curve left the report bytes untouched (sha256 %s)", plain)
	}
	if len(curve.Damping) != len(armsweep.DampingStops) {
		t.Fatalf("damping section holds %d entries, want %d", len(curve.Damping), len(armsweep.DampingStops))
	}
	if curve.DampingType != dampedType {
		t.Errorf("report names damping type %q, want %q", curve.DampingType, dampedType)
	}
	for i, c := range curve.Damping {
		want := armsweep.DampingName(armsweep.DampingStops[i])
		if c.Config.Name != want {
			t.Errorf("support point %d is %q, want %q", i, c.Config.Name, want)
		}
		if got := c.Config.Damping[dampedType]; got != armsweep.DampingStops[i] {
			t.Errorf("%s carries factor %v for %s, want %v", c.Config.Name, got, dampedType, armsweep.DampingStops[i])
		}
	}
	// The curve is a family of its own: it must not enter the variant table,
	// whose Bonferroni level is fixed at SecondaryComparisons.
	for _, cmp := range curve.Comparisons {
		if strings.HasPrefix(cmp.Config, "D0") || cmp.Config == armsweep.DampingName(1.0) {
			t.Errorf("damping support point %q leaked into the variant-vs-V0 table", cmp.Config)
		}
	}
	for _, w := range curve.Wins {
		if strings.HasPrefix(w.Config, "D0") || w.Config == armsweep.DampingName(1.0) {
			t.Errorf("damping support point %q carries a G-WIN verdict", w.Config)
		}
	}
	md := armsweep.RenderMarkdown("2026-08-27T00:00:00Z", curve)
	if !strings.Contains(md, "## Damping-Kurve") {
		t.Error("markdown report has no damping section")
	}
}

// --------------------------------------------------------------------------
// Gate 2: ten support points, ten different numbers.

// TestDampingStopsProduceTenDistinctNDCG is the wave's central gate. The
// fixture's flip points sit between the support points, so each stop wins a
// different number of cases and the ten nDCG@10 means must all differ.
func TestDampingStopsProduceTenDistinctNDCG(t *testing.T) {
	body := mustScore(t, dampInput(142, true, dampedType))
	seen := map[float64]string{}
	for _, c := range body.Damping {
		var ndcg float64
		found := false
		for _, s := range c.Slices {
			if s.Slice == armsweep.SliceKI {
				ndcg, found = s.NDCG10, true
			}
		}
		if !found {
			t.Fatalf("%s has no %s row", c.Config.Name, armsweep.SliceKI)
		}
		t.Logf("%s (factor %.2f): nDCG@10 = %.6f", c.Config.Name, c.Config.Damping[dampedType], ndcg)
		if prev, dup := seen[ndcg]; dup {
			t.Errorf("%s and %s both score nDCG@10 %.6f — the curve is flat there", prev, c.Config.Name, ndcg)
		}
		seen[ndcg] = c.Config.Name
	}
	if len(seen) != len(armsweep.DampingStops) {
		t.Fatalf("%d distinct nDCG@10 values over %d support points", len(seen), len(armsweep.DampingStops))
	}
}

// TestDampingCurveIsFlatWhenTheSweptTypeIsNotInTheRanking is the control for
// the gate above: it shows WHY the ten values differ. Sweep a type that holds
// no candidate and the same ten support points collapse onto one number — a
// fixture without the swept type in the top ranks would have let gate 2 pass
// while measuring nothing.
func TestDampingCurveIsFlatWhenTheSweptTypeIsNotInTheRanking(t *testing.T) {
	body := mustScore(t, dampInput(142, true, "type-that-holds-no-candidate"))
	seen := map[float64]bool{}
	for _, c := range body.Damping {
		for _, s := range c.Slices {
			if s.Slice == armsweep.SliceKI {
				seen[s.NDCG10] = true
			}
		}
	}
	if len(seen) != 1 {
		t.Errorf("%d distinct nDCG@10 values although no row carries the swept type", len(seen))
	}
}

// --------------------------------------------------------------------------
// Gate 3: the status quo is IN the curve and reproduces V0 exactly.

// TestDampingAtLiveFactorReproducesV0 pins the reproduction requirement of §7:
// the support point equal to the type's live damping factor must reproduce the
// V0 fusion bit for bit, not approximately.
func TestDampingAtLiveFactorReproducesV0(t *testing.T) {
	const live = 0.30 // auditTrailDamping, blocktype/builtin.go:32
	recs := dampRecords(live, true)
	var at *armsweep.Config
	for _, c := range armsweep.DampingConfigs(dampedType) {
		if c.Damping[dampedType] == live {
			cc := c
			at = &cc
		}
	}
	if at == nil {
		t.Fatalf("the live factor %.2f is not a support point of %v", live, armsweep.DampingStops)
	}
	maxDelta := 0.0
	for _, rec := range recs {
		base := armsweep.Fuse(rec.Rows, armsweep.ConfigV0())
		curve := armsweep.Fuse(rec.Rows, *at)
		if len(base) != len(curve) {
			t.Fatalf("case %d: %d rows against %d", rec.Index, len(base), len(curve))
		}
		for i := range base {
			if base[i].ID != curve[i].ID {
				t.Fatalf("case %d position %d: %s against %s — the order moved", rec.Index, i, base[i].ID, curve[i].ID)
			}
			if d := math.Abs(base[i].Score - curve[i].Score); d > maxDelta {
				maxDelta = d
			}
		}
	}
	t.Logf("max |Δscore| over %d cases at damping = live factor %.2f: %g", len(recs), live, maxDelta)
	if maxDelta >= 1e-12 {
		t.Fatalf("max |Δscore| = %g, want < 1e-12", maxDelta)
	}
}

// --------------------------------------------------------------------------
// Gate 4 (the "and back" half): rows without a type name are never damped.

// TestDampingLeavesRowsWithoutATypeNameAlone is the negative probe of the
// M-W1 review finding. A dump written before migration 142 carries the empty
// string in every row, and a lookup that treated that as a key would rescale
// the whole dump at once. Mutation proof: dropping the `r.TypeName == ""`
// guard in typeFactor and keying the map on "" makes this test red.
func TestDampingLeavesRowsWithoutATypeNameAlone(t *testing.T) {
	recs := dampRecords(1.0, false) // pre-142 shape: no type_name anywhere
	fallback := armsweep.Config{
		Name: "D-fallback", Weights: armsweep.LiveWeights, K: armsweep.LiveK,
		Damping: map[string]float64{"": 0.05, dampedType: 0.05},
	}
	for _, rec := range recs {
		base := armsweep.Fuse(rec.Rows, armsweep.ConfigV0())
		damped := armsweep.Fuse(rec.Rows, fallback)
		for i := range base {
			if base[i].ID != damped[i].ID || base[i].Score != damped[i].Score {
				t.Fatalf("case %d position %d: a damping map moved an untyped row (%s %v against %s %v)",
					rec.Index, i, base[i].ID, base[i].Score, damped[i].ID, damped[i].Score)
			}
		}
	}
}

// --------------------------------------------------------------------------
// The M-W1-review gate: pre-142 dumps are refused, not silently flattened.

// TestScoreRefusesADampingSweepOverAPre142Dump pins the binding half of the
// M-W1 review finding: the refusal is a hard error naming the migration, and it
// fires only when a damping curve was actually asked for.
func TestScoreRefusesADampingSweepOverAPre142Dump(t *testing.T) {
	t.Run("pre-142 dump plus damping is refused", func(t *testing.T) {
		_, err := armsweep.Score(dampInput(141, false, dampedType))
		if err == nil {
			t.Fatal("a damping sweep over a pre-142 dump was accepted — the curve would be flat by construction")
		}
		if !strings.Contains(err.Error(), "142") || !strings.Contains(err.Error(), dampedType) {
			t.Errorf("refusal names neither the migration nor the type: %v", err)
		}
		t.Logf("refusal: %v", err)
	})
	t.Run("pre-142 dump without damping still scores", func(t *testing.T) {
		if _, err := armsweep.Score(dampInput(141, false, "")); err != nil {
			t.Fatalf("an ordinary sweep over an old dump was refused: %v", err)
		}
	})
	t.Run("142 dump plus damping scores", func(t *testing.T) {
		if _, err := armsweep.Score(dampInput(armsweep.TypeNameMigration, true, dampedType)); err != nil {
			t.Fatalf("a damping sweep over a 142 dump was refused: %v", err)
		}
	})
	t.Run("a pre-142 replicate is refused too", func(t *testing.T) {
		in := dampInput(142, true, dampedType)
		b := dampRecords(1.0, true)
		sb := dampStamp(b, 141)
		in.RecordsB, in.StampB = b, &sb
		_, err := armsweep.Score(in)
		if err == nil {
			t.Fatal("a replicate pair straddling the 142 boundary was accepted")
		}
		if !strings.Contains(err.Error(), "dump B") {
			t.Errorf("refusal does not name the offending dump: %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Non-regression: the weight sweep is untouched.

// TestReportWithoutDampingCarriesNoDampingKeys is the byte-identity guard of
// the wave: the two new fields are omitempty, so a run that sweeps no damping
// type produces the bytes it produced before M-W8 existed.
func TestReportWithoutDampingCarriesNoDampingKeys(t *testing.T) {
	b, err := armsweep.MarshalBody(mustScore(t, dampInput(141, false, "")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"damping"`, `"damping_type"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("a report without a damping sweep carries %s", key)
		}
	}
}

// TestStaticConfigurationsCarryNoDampingMap pins the other half: the fourteen
// literal configurations and the two derived ones must reach scoreRow with an
// empty map, so their arithmetic is the expression it was before this wave.
func TestStaticConfigurationsCarryNoDampingMap(t *testing.T) {
	static := 0
	for _, name := range armsweep.ConfigNames() {
		c, ok := armsweep.ConfigByName(name)
		if !ok {
			continue // V6a/V6b are derived, checked below
		}
		static++
		if c.Damping != nil {
			t.Errorf("static configuration %s carries a damping map: %v", name, c.Damping)
		}
	}
	if static != 14 {
		t.Errorf("%d static configurations, want 14", static)
	}
	for _, name := range []string{armsweep.NameV6a, armsweep.NameV6b} {
		c := armsweep.DeriveV6(name, armsweep.SoloNDCG{Semantic: 0.5, FTSDe: 0.2, FTSEn: 0.2, Trigram: 0.1}, 1)
		if c.Damping != nil {
			t.Errorf("derived configuration %s carries a damping map: %v", name, c.Damping)
		}
	}
}
