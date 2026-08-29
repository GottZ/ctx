package goldset_test

// Wave C3-4a: the draw, the blind sheet and the calibration mechanics of
// amendment design/05a (§C3-2-D05-3, -5, -6, -7, -8).
//
// The fixtures are synthetic on purpose. The real gold directory is private and
// root-only, so a test bound to it could not run anywhere but this machine —
// the numbers each gate is read against are hand-computed in the test body and
// stated there, which is also what makes them checkable without the data.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/goldset"
)

// drawFixture builds a run of 40 queries with 40 pooled candidates and 5
// control draws each. Per query the pooled candidates are laid out so that the
// four strata of §C3-2-D05-3 are populated far beyond their allocation:
// 10 cells per query in each of S1 (judge=1, >=2 arms), S2 (judge=1, 1 arm),
// S3 (judge=0, best rank <=10) and S4 (judge=0, best rank >10).
type drawFixture struct {
	cells   []goldset.JudgeCell
	judged  map[string][]goldset.Judgement
	pool    []goldset.PoolEntry
	key     goldset.PoolKey
	regimes map[string]string
}

const (
	fxQueries    = 40
	fxCandidates = 40
	fxControls   = 5
	fxLocal      = 28 // the first 28 queries carry regime local, the rest global
)

func buildDrawFixture() drawFixture {
	fx := drawFixture{
		judged:  map[string][]goldset.Judgement{},
		key:     goldset.PoolKey{Version: 1, Seed: 20260812, Controls: fxControls, ControlIDs: map[string][]string{}},
		regimes: map[string]string{},
	}
	for q := 0; q < fxQueries; q++ {
		sha := goldset.SHA256Hex(fmt.Sprintf("fixture-query-%02d", q))
		k := goldset.CaseKey(goldset.SliceReal, q, sha)
		regime := goldset.RegimeLocal
		if q >= fxLocal {
			regime = goldset.RegimeGlobal
		}
		fx.regimes[sha] = regime
		entry := goldset.PoolEntry{Slice: goldset.SliceReal, Index: q, QuerySHA256: sha}
		for c := 0; c < fxCandidates; c++ {
			id := fmt.Sprintf("blk-%02d-%02d", q, c)
			fx.cells = append(fx.cells, goldset.JudgeCell{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha,
				Query: fmt.Sprintf("Frage %02d?", q), BlockID: id,
				Title: "Titel " + id, Excerpt: "Auszug " + id,
			})
			// group 0: S1, group 1: S2, group 2: S3, group 3: S4.
			group, rank := c/10, (c%10)+1
			relevant := group <= 1
			switch group {
			case 0: // two arms, so the cell is judge=1 with >=2 arms
				entry.Semantic = append(entry.Semantic, id)
				entry.FTSDe = append(entry.FTSDe, id)
			case 1: // one arm only
				entry.Semantic = append(entry.Semantic, id)
			case 2: // one arm, head ranks 1..10
				entry.Trigram = append(entry.Trigram, id)
			default: // one arm, ranks 11..20
				for len(entry.FTSEn) < 10 {
					entry.FTSEn = append(entry.FTSEn, fmt.Sprintf("pad-%02d-%02d", q, len(entry.FTSEn)))
				}
				entry.FTSEn = append(entry.FTSEn, id)
			}
			_ = rank
			fx.judged[k] = append(fx.judged[k], goldset.Judgement{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha, BlockID: id, Relevant: relevant,
			})
		}
		for c := 0; c < fxControls; c++ {
			id := fmt.Sprintf("ctl-%02d-%02d", q, c)
			fx.cells = append(fx.cells, goldset.JudgeCell{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha,
				Query: fmt.Sprintf("Frage %02d?", q), BlockID: id,
				Title: "Titel " + id, Excerpt: "Auszug " + id,
			})
			fx.judged[k] = append(fx.judged[k], goldset.Judgement{
				Slice: goldset.SliceReal, Index: q, QuerySHA256: sha, BlockID: id, Relevant: false,
			})
			fx.key.ControlIDs[k] = append(fx.key.ControlIDs[k], id)
		}
		fx.pool = append(fx.pool, entry)
	}
	return fx
}

func (fx drawFixture) input(spec goldset.DrawSpec) goldset.DrawInput {
	return goldset.DrawInput{
		SourceRun: "fixture", Cells: fx.cells, Judged: fx.judged,
		Pool: fx.pool, Key: fx.key, Regimes: fx.regimes, Spec: spec,
	}
}

// TestDrawAllocationC34A is gate 4 of §C3-2-D05-7: the strata are hit exactly,
// the core is 20 queries and every non-control cell of a core query is drawn.
func TestDrawAllocationC34A(t *testing.T) {
	fx := buildDrawFixture()
	spec := goldset.DefaultDrawSpec(20260829)
	key, err := goldset.Draw(fx.input(spec))
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	want := map[string]int{
		goldset.StratumS1: 120, goldset.StratumS2: 140,
		goldset.StratumS3: 140, goldset.StratumS4: 80, goldset.StratumS0: 60,
	}
	got := map[string]int{}
	core := map[string]int{}
	for _, c := range key.Cells {
		got[c.Stratum]++
		if c.Stratum == goldset.StratumCore {
			core[c.QuerySHA256]++
		}
	}
	for s, n := range want {
		if got[s] != n {
			t.Errorf("Schicht %s: %d Zellen, erwartet %d", s, got[s], n)
		}
	}
	if len(key.CoreQueries) != 20 {
		t.Errorf("Kern: %d Queries, erwartet 20", len(key.CoreQueries))
	}
	local, global := 0, 0
	for _, q := range key.CoreQueries {
		switch q.Regime {
		case goldset.RegimeLocal:
			local++
		case goldset.RegimeGlobal:
			global++
		}
	}
	if local != 14 || global != 6 {
		t.Errorf("Kern-Regime: local=%d global=%d, erwartet 14/6", local, global)
	}
	if len(core) != 20 {
		t.Errorf("Kern-Zellen verteilen sich auf %d Queries, erwartet 20", len(core))
	}
	for sha, n := range core {
		if n != fxCandidates {
			t.Errorf("Kern-Query %s: %d Zellen, erwartet %d (vollständig)", sha[:8], n, fxCandidates)
		}
	}
	// Horvitz-Thompson weights: N_h / n_h, and 1 on the fully judged core.
	for _, c := range key.Cells {
		switch {
		case c.Stratum == goldset.StratumCore && c.Weight != 1:
			t.Errorf("Kern-Zelle mit Gewicht %.4f, erwartet 1", c.Weight)
		case c.Stratum != goldset.StratumCore:
			w := float64(key.Population[c.Stratum]) / float64(key.Sampled[c.Stratum])
			if math.Abs(c.Weight-w) > 1e-9 {
				t.Errorf("Schicht %s: Gewicht %.6f, erwartet %.6f", c.Stratum, c.Weight, w)
			}
		}
	}
}

// TestDrawDeterministicC34A is gate 3: same seed, same bytes — key and sheet.
func TestDrawDeterministicC34A(t *testing.T) {
	fx := buildDrawFixture()
	spec := goldset.DefaultDrawSpec(20260829)
	k1, err := goldset.Draw(fx.input(spec))
	if err != nil {
		t.Fatalf("Draw 1: %v", err)
	}
	k2, err := goldset.Draw(fx.input(spec))
	if err != nil {
		t.Fatalf("Draw 2: %v", err)
	}
	j1, err := goldset.MarshalDrawKey(k1)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := goldset.MarshalDrawKey(k2)
	if err != nil {
		t.Fatal(err)
	}
	if string(j1) != string(j2) {
		t.Error("Ziehungs-Schlüssel nicht byte-identisch zwischen zwei Läufen")
	}
	// Two draws in one test run share the wall clock, so an equality check
	// alone would pass a key that carried a timestamp — and the end-to-end
	// probe on the real data showed exactly that. The key must be a function of
	// its inputs, not of the clock.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(j1, &raw); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"created_at", "drawn_at", "timestamp"} {
		if _, bad := raw[f]; bad {
			t.Errorf("der Ziehungs-Schlüssel führt %q — er wäre dann eine Funktion der Uhr", f)
		}
	}
	s1, err := goldset.RenderFableSheetJSONL(k1, fx.cells)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := goldset.RenderFableSheetJSONL(k2, fx.cells)
	if err != nil {
		t.Fatal(err)
	}
	if string(s1) != string(s2) {
		t.Error("Bogen nicht byte-identisch zwischen zwei Läufen")
	}
	// A different seed must move the draw, or the seed would be decoration.
	k3, err := goldset.Draw(fx.input(goldset.DefaultDrawSpec(20260830)))
	if err != nil {
		t.Fatal(err)
	}
	j3, err := goldset.MarshalDrawKey(k3)
	if err != nil {
		t.Fatal(err)
	}
	if string(j1) == string(j3) {
		t.Error("anderer Seed liefert denselben Schlüssel — die Ziehung hängt nicht am Seed")
	}
}

// TestFableSheetBlindC34A is gate 2: every judge proxy of §C3-2-D05-5 is absent
// from the sheet, and a sheet that carries one is refused.
func TestFableSheetBlindC34A(t *testing.T) {
	fx := buildDrawFixture()
	key, err := goldset.Draw(fx.input(goldset.DefaultDrawSpec(20260829)))
	if err != nil {
		t.Fatal(err)
	}
	sheet, err := goldset.RenderFableSheetJSONL(key, fx.cells)
	if err != nil {
		t.Fatal(err)
	}
	if err := goldset.AssertSheetBlind(sheet); err != nil {
		t.Fatalf("gezogener Bogen ist nicht blind: %v", err)
	}
	rows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(sheet)), "\n") {
		var m map[string]json.RawMessage
		if uerr := json.Unmarshal([]byte(line), &m); uerr != nil {
			t.Fatalf("Bogenzeile ist kein JSON: %v", uerr)
		}
		for _, f := range goldset.ForbiddenSheetFields() {
			if _, bad := m[f]; bad {
				t.Errorf("Bogen führt das verbotene Feld %q", f)
			}
		}
		rows++
	}
	if rows != len(key.Cells)+1 {
		t.Errorf("Bogen hat %d Zeilen, erwartet %d (Kopf + Zellen)", rows, len(key.Cells)+1)
	}
	for _, bad := range []string{
		`{"kind":"cell","query_sha256":"a","block_id":"b","llm_judgement":"1","verdict":""}`,
		`{"kind":"cell","query_sha256":"a","block_id":"b","stratum":"S1","verdict":""}`,
		`{"kind":"cell","query_sha256":"a","block_id":"b","is_control":true,"verdict":""}`,
		`{"kind":"cell","query_sha256":"a","block_id":"b","weight":3.9,"verdict":""}`,
		`{"kind":"cell","query_sha256":"a","block_id":"b","arms":2,"verdict":""}`,
		`{"kind":"cell","query_sha256":"a","block_id":"b","best_rank":4,"verdict":""}`,
	} {
		if err := goldset.AssertSheetBlind([]byte(bad + "\n")); err == nil {
			t.Errorf("Bogen mit Judge-Proxy wurde nicht abgewiesen: %s", bad)
		}
	}
}

// filled answers a drawn sheet from a decision function, in sheet order.
func filled(t *testing.T, key goldset.DrawKey, cells []goldset.JudgeCell,
	fn func(goldset.DrawCell) string,
) []goldset.FableJudgement {
	t.Helper()
	sheet, err := goldset.RenderFableSheetJSONL(key, cells)
	if err != nil {
		t.Fatal(err)
	}
	byCell := map[string]goldset.DrawCell{}
	for _, c := range key.Cells {
		byCell[c.QuerySHA256+"/"+c.BlockID] = c
	}
	var out []goldset.FableJudgement
	for _, line := range strings.Split(strings.TrimSpace(string(sheet)), "\n") {
		var r struct {
			Kind        string `json:"kind"`
			QuerySHA256 string `json:"query_sha256"`
			BlockID     string `json:"block_id"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind != "cell" {
			continue
		}
		v, err := goldset.ParseSheetVerdict(fn(byCell[r.QuerySHA256+"/"+r.BlockID]))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, goldset.FableJudgement{
			QuerySHA256: r.QuerySHA256, BlockID: r.BlockID, Verdict: v,
		})
	}
	return out
}

// handPairs is the hand-computed calibration fixture of gates 5 and 7: two
// strata with very different weights and one discordant cell each.
func handPairs(w1, w4 float64, n int) []goldset.CalibrationPair {
	var out []goldset.CalibrationPair
	add := func(stratum string, w float64, llm bool, fable goldset.SheetVerdict, count int) {
		for i := 0; i < count; i++ {
			out = append(out, goldset.CalibrationPair{
				Slice: goldset.SliceReal, Stratum: stratum, Weight: w,
				QuerySHA256: fmt.Sprintf("q%d", len(out)), BlockID: fmt.Sprintf("b%d", len(out)),
				LLM: llm, Fable: fable,
			})
		}
	}
	add(goldset.StratumS1, w1, true, goldset.SheetRelevant, n-1)
	add(goldset.StratumS1, w1, true, goldset.SheetIrrelevant, 1)
	add(goldset.StratumS4, w4, false, goldset.SheetIrrelevant, n-1)
	add(goldset.StratumS4, w4, false, goldset.SheetRelevant, 1)
	return out
}

// TestKappaWeightedC34A is gate 5: the HT-weighted kappa hits the hand-computed
// value and the unweighted one differs.
//
// Fixture: S1 weight 4 with 3 concordant + 1 judge-only cell, S4 weight 20 with
// 3 concordant + 1 fable-only cell.
//
//	unweighted: Po = 6/8 = 0.75, marginals 0.5/0.5, Pe = 0.5, k = 0.5
//	weighted:   W = 96, Po = 72/96 = 0.75, marginals 16/96 and 32/96,
//	            Pe = (1/6)(1/3) + (5/6)(2/3) = 11/18, k_w = 5/14 = 0.357142857
func TestKappaWeightedC34A(t *testing.T) {
	p := handPairs(4, 20, 4)
	kw := goldset.KappaWeighted(p)
	const wantW = 5.0 / 14.0
	if math.Abs(kw.Kappa-wantW) > 1e-9 {
		t.Errorf("κ_w = %.9f, handgerechnet %.9f", kw.Kappa, wantW)
	}
	if math.Abs(kw.Agreement-0.75) > 1e-9 {
		t.Errorf("gewichtete Übereinstimmung = %.9f, erwartet 0.75", kw.Agreement)
	}
	if math.Abs(kw.Expected-11.0/18.0) > 1e-9 {
		t.Errorf("gewichtete Zufalls-Übereinstimmung = %.9f, erwartet %.9f", kw.Expected, 11.0/18.0)
	}
	unw := goldset.Kappa(goldset.UnweightedPairs(p))
	if math.Abs(unw.Kappa-0.5) > 1e-9 {
		t.Errorf("ungewichtetes κ = %.9f, handgerechnet 0.5", unw.Kappa)
	}
	if math.Abs(unw.Kappa-kw.Kappa) < 1e-6 {
		t.Error("gewichtetes und ungewichtetes κ sind gleich — die Gewichte wirken nicht")
	}
}

// TestUnsureIsNotGoldC34A is gate 8: `?` parses, counts as 0 and is reported as
// a rate; above 0.10 in one stratum the gate is undecided and names it.
func TestUnsureIsNotGoldC34A(t *testing.T) {
	if _, err := goldset.ParseSheetVerdict("?"); err != nil {
		t.Fatalf("`?` wird nicht als Urteilsstufe angenommen: %v", err)
	}
	if goldset.SheetUnsure.Relevant() {
		t.Error("`?` zählt als relevant — es darf kein Gold erzeugen")
	}
	if _, err := goldset.ParseSheetVerdict(""); err == nil {
		t.Error("leere Zelle wurde angenommen — sie ist ErrUnjudged")
	}
	var pairs []goldset.CalibrationPair
	for i := 0; i < 20; i++ {
		v := goldset.SheetRelevant
		if i >= 3 {
			v = goldset.SheetIrrelevant
		}
		if i < 3 {
			v = goldset.SheetUnsure // 3 of 20 = 0.15 > 0.10
		}
		pairs = append(pairs, goldset.CalibrationPair{
			Slice: goldset.SliceReal, Stratum: goldset.StratumS3, Weight: 12,
			QuerySHA256: fmt.Sprintf("q%d", i), BlockID: fmt.Sprintf("b%d", i),
			LLM: true, Fable: v,
		})
	}
	res := goldset.Calibrate(pairs)
	var s3 goldset.StratumStats
	for _, s := range res.Strata {
		if s.Stratum == goldset.StratumS3 {
			s3 = s
		}
	}
	if s3.Unsure != 3 || math.Abs(s3.UnsureRate-0.15) > 1e-9 {
		t.Errorf("S3: unsure=%d rate=%.4f, erwartet 3 / 0.1500", s3.Unsure, s3.UnsureRate)
	}
	// `?` counted as 0 means every one of the 20 judge-positive cells is
	// discordant: LLMOnly = 20, ControlOnly = 0.
	if res.Unweighted.LLMOnly != 20 || res.Unweighted.Both != 0 {
		t.Errorf("`?` wurde nicht als 0 gewertet: both=%d llm_only=%d",
			res.Unweighted.Both, res.Unweighted.LLMOnly)
	}
	th := goldset.DefaultCalibrationThresholds()
	th.KappaMin = -1
	gates := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{}, th)
	if !gateReasonContains(gates, "G-REAL-MDE", goldset.StratumS3) {
		t.Errorf("das Gate nennt die Schicht mit zu hoher `?`-Rate nicht: %+v", gates)
	}
	if verdictOf(gates, "G-REAL-MDE") != goldset.GateUndecided {
		t.Error("`?`-Rate über der Schwelle lässt das Gate tragen")
	}
}

// TestRhoPiGateC34A is gate 7: a sample with sensitivity 0.5 leaves the gate
// undecided and the reason carries the number.
func TestRhoPiGateC34A(t *testing.T) {
	var pairs []goldset.CalibrationPair
	add := func(llm bool, fable goldset.SheetVerdict, n int) {
		for i := 0; i < n; i++ {
			pairs = append(pairs, goldset.CalibrationPair{
				Slice: goldset.SliceReal, Stratum: goldset.StratumS1, Weight: 2,
				QuerySHA256: fmt.Sprintf("q%d", len(pairs)), BlockID: fmt.Sprintf("b%d", len(pairs)),
				LLM: llm, Fable: fable,
			})
		}
	}
	add(true, goldset.SheetRelevant, 20)    // judge finds it, fable confirms
	add(false, goldset.SheetRelevant, 20)   // fable gold the judge missed
	add(false, goldset.SheetIrrelevant, 20) // agreed noise
	res := goldset.Calibrate(pairs)
	if math.Abs(res.Rho.Value-0.5) > 1e-9 {
		t.Errorf("ρ = %.6f, konstruiert 0.5", res.Rho.Value)
	}
	if math.Abs(res.Pi.Value-1.0) > 1e-9 {
		t.Errorf("π = %.6f, konstruiert 1.0", res.Pi.Value)
	}
	th := goldset.DefaultCalibrationThresholds()
	th.KappaMin = -1
	gates := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{}, th)
	if verdictOf(gates, "Splits") != goldset.GateUndecided {
		t.Error("ρ unter der Schranke lässt das Gate tragen")
	}
	if !gateReasonContains(gates, "Splits", "0.5000") {
		t.Errorf("die ρ-Begründung nennt die Zahl nicht: %+v", reasonsOf(gates, "Splits"))
	}
}

// TestMetricFlipGateC34A is the goldset half of gate 6: a flip on the core
// leaves the gate undecided even when kappa, rho and pi all clear.
func TestMetricFlipGateC34A(t *testing.T) {
	var pairs []goldset.CalibrationPair
	add := func(stratum string, llm bool, fable goldset.SheetVerdict, n int) {
		for i := 0; i < n; i++ {
			pairs = append(pairs, goldset.CalibrationPair{
				Slice: goldset.SliceReal, Stratum: stratum, Weight: 2,
				QuerySHA256: fmt.Sprintf("q%d", len(pairs)), BlockID: fmt.Sprintf("b%d", len(pairs)),
				LLM: llm, Fable: fable,
			})
		}
	}
	add(goldset.StratumS1, true, goldset.SheetRelevant, 45)
	add(goldset.StratumS1, true, goldset.SheetIrrelevant, 5)
	add(goldset.StratumS4, false, goldset.SheetIrrelevant, 45)
	add(goldset.StratumS4, false, goldset.SheetRelevant, 5)
	res := goldset.Calibrate(pairs)
	th := goldset.DefaultCalibrationThresholds()
	th.KappaMin = -1
	quiet := goldset.MetricFlip{
		Available: true, Metric: "nDCG@10", N: 20,
		DeltaFable: 0.031, DeltaJudge: 0.028, DiffCILo: -0.004, DiffCIHi: 0.011,
	}
	clean := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{goldset.SliceReal: quiet}, th)
	if verdictOf(clean, "G-REAL-MDE") != goldset.GateCarries {
		t.Fatalf("Vorbedingung verletzt — ohne Kipp muss das Gate tragen: %+v", reasonsOf(clean, "G-REAL-MDE"))
	}
	// A missing flip computation is fail-closed, not a pass.
	absent := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{}, th)
	if verdictOf(absent, "G-REAL-MDE") != goldset.GateUndecided {
		t.Error("fehlende Kipp-Rechnung lässt das Gate tragen")
	}
	flip := goldset.MetricFlip{
		Available: true, Metric: "nDCG@10", N: 20,
		DeltaFable: 0.031, DeltaJudge: -0.017, DiffCILo: 0.010, DiffCIHi: 0.086,
	}
	if !flip.SignFlip() {
		t.Error("Vorzeichenwechsel nicht erkannt")
	}
	if !flip.Flipped() {
		t.Error("Kipp nicht erkannt")
	}
	gates := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{goldset.SliceReal: flip}, th)
	if verdictOf(gates, "G-REAL-MDE") != goldset.GateUndecided {
		t.Error("Metrik-Kipp lässt das Gate tragen")
	}
	if !gateReasonContains(gates, "G-REAL-MDE", "Kipp") {
		t.Errorf("die Kipp-Begründung fehlt: %+v", reasonsOf(gates, "G-REAL-MDE"))
	}
	// The CI half of the rule must fire on its own, without a sign change.
	ciOnly := goldset.MetricFlip{
		Available: true, Metric: "nDCG@10", N: 20,
		DeltaFable: 0.031, DeltaJudge: 0.011, DiffCILo: 0.004, DiffCIHi: 0.037,
	}
	if ciOnly.SignFlip() {
		t.Error("gleiches Vorzeichen als Wechsel gewertet")
	}
	if !ciOnly.Flipped() {
		t.Error("gepaartes CI ohne 0 wurde nicht als Kipp gewertet")
	}
}

// TestKappaReachC34A is the §C3-2-D05-6 role change of the kappa threshold: it
// no longer decides a gate, it limits the reach of the judge labels.
func TestKappaReachC34A(t *testing.T) {
	// The same hand-computed fixture as gate 5: kappa_w = 5/14 = 0.3571, which
	// is below the stated 0.6 — the 40-cell variant lands at 0.9135 and would
	// test nothing.
	res := goldset.Calibrate(handPairs(4, 20, 4))
	if math.Abs(res.Weighted.Kappa-5.0/14.0) > 1e-9 {
		t.Fatalf("Vorbedingung: κ_w = %.6f, handgerechnet %.6f", res.Weighted.Kappa, 5.0/14.0)
	}
	th := goldset.DefaultCalibrationThresholds()
	th.KappaMin = 0.6
	gates := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{}, th)
	if reachOf(gates, "G-REAL-MDE") != goldset.GoldReachCoreOnly {
		t.Errorf("κ_w unter der Schranke beschränkt die Gold-Reichweite nicht: %q",
			reachOf(gates, "G-REAL-MDE"))
	}
	th.KappaMin = -1
	full := goldset.CalibratedGateReport(
		map[string]goldset.CalibrationResult{goldset.SliceReal: res},
		map[string]goldset.MetricFlip{}, th)
	if reachOf(full, "G-REAL-MDE") != goldset.GoldReachFull {
		t.Errorf("κ_w über der Schranke beschränkt die Reichweite trotzdem: %q",
			reachOf(full, "G-REAL-MDE"))
	}
}

// TestControlsStayOutOfKappaC34A is gate 10: S0 feeds ControlHitRate and
// nothing else — adding control cells must not move a single kappa digit.
func TestControlsStayOutOfKappaC34A(t *testing.T) {
	base := handPairs(4, 20, 40)
	withS0 := append([]goldset.CalibrationPair(nil), base...)
	for i := 0; i < 60; i++ {
		withS0 = append(withS0, goldset.CalibrationPair{
			Slice: goldset.SliceReal, Stratum: goldset.StratumS0, Weight: 12.5, Control: true,
			QuerySHA256: fmt.Sprintf("c%d", i), BlockID: fmt.Sprintf("cb%d", i),
			LLM: i < 30, Fable: goldset.SheetRelevant,
		})
	}
	a, b := goldset.Calibrate(base), goldset.Calibrate(withS0)
	if a.Unweighted != b.Unweighted {
		t.Errorf("Kontrollen verschieben das ungewichtete κ: %+v vs %+v", a.Unweighted, b.Unweighted)
	}
	if math.Abs(a.Weighted.Kappa-b.Weighted.Kappa) > 1e-12 {
		t.Errorf("Kontrollen verschieben κ_w: %.12f vs %.12f", a.Weighted.Kappa, b.Weighted.Kappa)
	}
	if b.ControlN != 60 || b.ControlHits != 60 || math.Abs(b.ControlRate-1) > 1e-12 {
		t.Errorf("Kontroll-Trefferquote: %d/%d = %.4f, erwartet 60/60 = 1.0000",
			b.ControlHits, b.ControlN, b.ControlRate)
	}
	if a.ControlN != 0 {
		t.Errorf("ohne S0 wird eine Kontroll-Menge von %d gemeldet", a.ControlN)
	}
}

// TestJoinCalibrationC34A binds a filled sheet back to the draw key over
// (query_sha256, block_id) — never over the line number (§C3-2-D05-5 (6)).
func TestJoinCalibrationC34A(t *testing.T) {
	fx := buildDrawFixture()
	key, err := goldset.Draw(fx.input(goldset.DefaultDrawSpec(20260829)))
	if err != nil {
		t.Fatal(err)
	}
	answers := filled(t, key, fx.cells, func(c goldset.DrawCell) string {
		if c.LLMRelevant {
			return "1"
		}
		return "0"
	})
	pairs, err := goldset.JoinCalibration(key, answers)
	if err != nil {
		t.Fatalf("JoinCalibration: %v", err)
	}
	if len(pairs) != len(key.Cells) {
		t.Errorf("%d Paare, erwartet %d", len(pairs), len(key.Cells))
	}
	for _, p := range pairs {
		if p.LLM != p.Fable.Relevant() {
			t.Fatalf("Zelle %s/%s falsch zugeordnet", p.QuerySHA256[:8], p.BlockID)
		}
	}
	res := goldset.Calibrate(pairs)
	if !res.Unweighted.NotComputable && math.Abs(res.Unweighted.Kappa-1) > 1e-9 {
		t.Errorf("bei identischen Urteilen ist κ = %.6f, erwartet 1", res.Unweighted.Kappa)
	}
	// A missing answer is an abort, never a silent 0.
	if _, err := goldset.JoinCalibration(key, answers[:len(answers)-1]); err == nil {
		t.Error("unvollständiger Bogen wurde angenommen")
	}
}

// TestApplyLabelsNamedC34A is §C3-2-D05-8 (i): two named gold sources side by
// side, the core restricted to its own queries.
func TestApplyLabelsNamedC34A(t *testing.T) {
	cases := []goldset.Case{
		{Slice: goldset.SliceReal, Index: 0, QuerySHA256: "aa", Query: "a"},
		{Slice: goldset.SliceReal, Index: 1, QuerySHA256: "bb", Query: "b"},
	}
	judged := map[string][]goldset.Judgement{
		goldset.CaseKey(goldset.SliceReal, 0, "aa"): {
			{Slice: goldset.SliceReal, Index: 0, QuerySHA256: "aa", BlockID: "x", Relevant: true},
			{Slice: goldset.SliceReal, Index: 0, QuerySHA256: "aa", BlockID: "y", Relevant: false},
		},
	}
	core, st, err := goldset.ApplyLabelsNamed(cases, judged, "fable-kern", true)
	if err != nil {
		t.Fatalf("ApplyLabelsNamed(restrict): %v", err)
	}
	if len(core) != 1 || core[0].GoldSource != "fable-kern" || len(core[0].GoldIDs) != 1 {
		t.Errorf("Kern-Variante: %d Fälle, Quelle %q, Gold %v", len(core), core[0].GoldSource, core[0].GoldIDs)
	}
	if st.Cases != 1 {
		t.Errorf("Kern-Statistik zählt %d Fälle, erwartet 1", st.Cases)
	}
	if _, _, err := goldset.ApplyLabelsNamed(cases, judged, "judge-uebertragen", false); err == nil {
		t.Error("die unbeschränkte Variante nahm einen ungeurteilten Fall an")
	}
}

func verdictOf(gates []goldset.GateVerdict, name string) string {
	for _, g := range gates {
		if g.Name == name {
			return g.Verdict
		}
	}
	return ""
}

func reachOf(gates []goldset.GateVerdict, name string) string {
	for _, g := range gates {
		if g.Name == name {
			return g.GoldReach
		}
	}
	return ""
}

func reasonsOf(gates []goldset.GateVerdict, name string) []string {
	for _, g := range gates {
		if g.Name == name {
			return g.Reasons
		}
	}
	return nil
}

func gateReasonContains(gates []goldset.GateVerdict, name, needle string) bool {
	for _, r := range reasonsOf(gates, name) {
		if strings.Contains(r, needle) {
			return true
		}
	}
	return false
}
