package armsweep_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// The five gates of wave X-W0b (design/05 §7 row X-W0, §4.4b):
//
//	1 rot         a G-REAL dump scores to ONE G-REAL row
//	2 grün        the two halves are their own rows, n_local + n_global = n_total,
//	              and the total row's figures are byte-identical to the run
//	              without the split
//	3 fail-closed one uncovered case refuses the run instead of forming a rest
//	              half — Score, Compare and the driver's exit code
//	4 Zuordnung   a swapped label file swaps the split figures (the rows really
//	              do compute on the labelled subsets)
//	5 Konsistenz  `compare` carries an MDE row per stratum, and every non-G-REAL
//	              row stays byte-identical
//
// Everything but TestRealLabelsPartitionGReal is synthetic. The real gold
// directory is private and root-only; the one gold-bound test skips without it.

// --------------------------------------------------------------- fixtures.

// realCase is one G-REAL case whose single gold id sits at fused position
// goldDepth (1-based). Depth is the knob the split has to be able to show: two
// halves built at different depths must produce different nDCG figures.
func realCase(i, goldDepth int) armsweep.Record {
	ids := caseIDs(goldset.SliceReal, i)
	rec := armsweep.Record{
		Slice: goldset.SliceReal, Index: i,
		QuerySHA256:    goldset.SHA256Hex(fmt.Sprintf("%s/%d", goldset.SliceReal, i)),
		EffectiveQuery: "synthetic",
		Selector:       armsweep.Selector{Mode: "ann", Reason: "grey", Estimate: 1000, ScanTuples: intp(60000)},
		Attempts:       1, LatencyMS: int64(100 + i),
		GoldIDs: []string{ids[0]},
	}
	order := make([]int, 0, len(ids))
	for p := 1; p < goldDepth && p < len(ids); p++ {
		order = append(order, p)
	}
	order = append(order, 0)
	for p := goldDepth; p < len(ids); p++ {
		order = append(order, p)
	}
	for pos, idx := range order {
		rec.Rows = append(rec.Rows, armRow(ids[idx], plainType, pos+1))
	}
	rec.FusionOrder = armsweep.FusedIDs(armsweep.Fuse(rec.Rows, armsweep.ConfigV0()))
	for j := 0; j < 5 && j < len(rec.FusionOrder); j++ {
		rec.Delivered = append(rec.Delivered, armsweep.Delivered{ID: rec.FusionOrder[j]})
	}
	return rec
}

// realShare is the fixture partition: the first nLocal cases are the shallow
// half, the rest the deep one. It is deliberately lopsided (8/4) the way the
// measured one is (131/19).
const (
	nLocalCases  = 8
	nGlobalCases = 4
	shallowDepth = 1
	deepDepth    = 7
)

// realDumpAt builds the stratified G-REAL fixture with a chosen gold depth per
// half: the first nLocalCases cases at localDepth, the rest at globalDepth. Two
// halves at different depths are what makes a split visible at all.
func realDumpAt(localDepth, globalDepth int) []armsweep.Record {
	out := make([]armsweep.Record, 0, nLocalCases+nGlobalCases)
	for i := 0; i < nLocalCases; i++ {
		out = append(out, realCase(i, localDepth))
	}
	for i := nLocalCases; i < nLocalCases+nGlobalCases; i++ {
		out = append(out, realCase(i, globalDepth))
	}
	return out
}

func realDump() []armsweep.Record { return realDumpAt(shallowDepth, deepDepth) }

// realSplit labels the fixture. swapped inverts the two halves — gate 4.
func realSplit(swapped bool) armsweep.RegimeSplit {
	regimes := map[string]string{}
	for i := 0; i < nLocalCases+nGlobalCases; i++ {
		regime := goldset.RegimeLocal
		if i >= nLocalCases {
			regime = goldset.RegimeGlobal
		}
		if swapped {
			if regime == goldset.RegimeLocal {
				regime = goldset.RegimeGlobal
			} else {
				regime = goldset.RegimeLocal
			}
		}
		regimes[goldset.SHA256Hex(fmt.Sprintf("%s/%d", goldset.SliceReal, i))] = regime
	}
	return armsweep.RegimeSplit{File: "x-w0-labels-fixture.jsonl", SHA256: goldset.SHA256Hex("fixture"), Regimes: regimes}
}

// realInput is a two-dump score input over the stratified fixture.
func realInput(split armsweep.RegimeSplit) armsweep.ScoreInput {
	a := realDump()
	b := realDump() // identical replicate: the noise floor is exactly zero
	sa, sb := synthStamp("A", a, nil), synthStamp("B", b, nil)
	return armsweep.ScoreInput{
		RecordsA: a, StampA: sa, RecordsB: b, StampB: &sb,
		RegimeSplit: split,
		Seed:        20260812, GitRevision: "deadbeef",
		GoldStamp: goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825,
			CorpusMaxCreatedAt: "2026-08-25T13:49:49.736510Z"},
	}
}

// rowDigests is the per-ROW byte image of a report: one sha256 per census row,
// per configuration row and per comparison row.
//
// Gate 2 is stated on rows rather than on the whole body on purpose — adding a
// row necessarily moves the body's bytes, and the claim under test is that
// nothing which was already there moved.
func rowDigests(t *testing.T, body armsweep.ReportBody) map[string]string {
	t.Helper()
	out := map[string]string{}
	put := func(key string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		out[key] = goldset.SHA256Hex(string(b))
	}
	for _, s := range body.Slices {
		put("slice/"+s.Slice, s)
	}
	for _, c := range body.Configs {
		for _, s := range c.Slices {
			put("config/"+c.Config.Name+"/"+s.Slice, s)
		}
	}
	for _, c := range body.Comparisons {
		put("cmp/"+c.Config+"/"+c.Slice, c)
	}
	for _, g := range body.Noise {
		put("noise/"+g.Slice, g)
	}
	return out
}

func sliceProfileOf(body armsweep.ReportBody, slice string) (armsweep.SliceProfile, bool) {
	for _, s := range body.Slices {
		if s.Slice == slice {
			return s, true
		}
	}
	return armsweep.SliceProfile{}, false
}

func metricsOfConfig(t *testing.T, body armsweep.ReportBody, config, slice string) armsweep.SliceMetrics {
	t.Helper()
	for _, c := range body.Configs {
		if c.Config.Name != config {
			continue
		}
		for _, s := range c.Slices {
			if s.Slice == slice {
				return s
			}
		}
	}
	t.Fatalf("no %s row for configuration %s", slice, config)
	return armsweep.SliceMetrics{}
}

// ------------------------------------------------------------- gates 1 + 2.

// TestSplitRowsAppearInTheScoreReport is gate 2 — and, against the code before
// this wave, gate 1: `score` then knows exactly one G-REAL row and this fails
// with "census carries no split rows".
func TestSplitRowsAppearInTheScoreReport(t *testing.T) {
	body := mustScore(t, realInput(realSplit(false)))

	total, ok := sliceProfileOf(body, armsweep.SliceRealName)
	if !ok {
		t.Fatalf("no %s row at all in the census", armsweep.SliceRealName)
	}
	local, okL := sliceProfileOf(body, armsweep.SliceRealLocal)
	global, okG := sliceProfileOf(body, armsweep.SliceRealGlobal)
	if !okL || !okG {
		t.Fatalf("census carries no split rows: %s present=%v, %s present=%v",
			armsweep.SliceRealLocal, okL, armsweep.SliceRealGlobal, okG)
	}
	if local.N+global.N != total.N {
		t.Errorf("split rows do not partition the total: %d + %d != %d", local.N, global.N, total.N)
	}
	if local.N != nLocalCases || global.N != nGlobalCases {
		t.Errorf("split = %d local / %d global, want %d/%d", local.N, global.N, nLocalCases, nGlobalCases)
	}
	// A stratum is never a rollout criterion: its cases already vote in the
	// total row, so a second vote would count them twice.
	if local.RolloutCriterion || global.RolloutCriterion {
		t.Error("a regime stratum claims to be a rollout criterion")
	}
	if !total.RolloutCriterion {
		t.Error("the total G-REAL row lost its rollout-criterion flag to the split")
	}
	if local.Note == "" || global.Note == "" {
		t.Error("a stratum row carries no note declaring what it is")
	}
	if body.Env.RegimeLabels == nil || body.Env.RegimeLabels.Labels != nLocalCases+nGlobalCases {
		t.Errorf("env carries no label provenance: %+v", body.Env.RegimeLabels)
	}
}

// TestSplitLeavesEveryExistingRowByteIdentical is the other half of gate 2 and
// the non-regression of gate 5: the split ADDS rows, it changes none.
func TestSplitLeavesEveryExistingRowByteIdentical(t *testing.T) {
	before := rowDigests(t, mustScore(t, realInput(armsweep.RegimeSplit{})))
	after := rowDigests(t, mustScore(t, realInput(realSplit(false))))

	for key, want := range before {
		got, ok := after[key]
		if !ok {
			t.Errorf("row %s vanished from the stratified report", key)
			continue
		}
		if got != want {
			t.Errorf("row %s changed: %s -> %s", key, want, got)
		}
	}
	for key := range after {
		if _, ok := before[key]; ok {
			continue
		}
		if !hasStratumSuffix(key) {
			t.Errorf("the split added a row that is not a stratum: %s", key)
		}
	}
	t.Logf("rows before %d, after %d; %s = %s (unchanged)",
		len(before), len(after), "slice/"+armsweep.SliceRealName, before["slice/"+armsweep.SliceRealName])
}

func hasStratumSuffix(key string) bool {
	for _, s := range armsweep.StratumSlices() {
		if len(key) >= len(s) && key[len(key)-len(s):] == s {
			return true
		}
	}
	return false
}

// TestSplitDoesNotChangeTheUnsplitReportBytes pins the continuity guarantee in
// its strongest form: without labels the whole body is the body of the wave
// before this one (score_nonregression_mw3d_test.go pins the same digest).
func TestSplitDoesNotChangeTheUnsplitReportBytes(t *testing.T) {
	body := mustScore(t, synthInput(t, true))
	for _, s := range body.Slices {
		if armsweep.IsStratum(s.Slice) {
			t.Errorf("a run without labels produced the stratum row %s", s.Slice)
		}
	}
	single := mustScore(t, synthInput(t, false))
	for _, c := range single.Configs {
		for _, s := range c.Slices {
			if armsweep.IsStratum(s.Slice) {
				t.Errorf("configuration %s carries an empty stratum row %s on a single-dump run",
					c.Config.Name, s.Slice)
			}
		}
	}
}

// ------------------------------------------------------------------ gate 3.

// TestScoreRefusesAnUncoveredCase is the fail-closed probe: one missing label
// refuses the whole run. The alternative — dropping the case into one half, or
// forming a "rest" half out of the unlabelled remainder — is a figure over a
// set nobody defined, and nothing in the report would say so.
func TestScoreRefusesAnUncoveredCase(t *testing.T) {
	split := realSplit(false)
	victim := goldset.SHA256Hex(fmt.Sprintf("%s/%d", goldset.SliceReal, nLocalCases))
	delete(split.Regimes, victim)

	body, err := armsweep.Score(realInput(split))
	if !errors.Is(err, armsweep.ErrRegimeLabelMissing) {
		t.Fatalf("Score error = %v, want %v", err, armsweep.ErrRegimeLabelMissing)
	}
	if len(body.Slices) != 0 || len(body.Configs) != 0 {
		t.Error("a refused run still produced a report body — the rest half would be readable")
	}
	if got := err.Error(); !containsAll(got, armsweep.ShortSHA(victim), split.File) {
		t.Errorf("the refusal names neither the case nor the label file: %s", got)
	}
}

// TestSplitNeverFormsARestHalf is the same probe read from the other side: the
// halves of a report that DOES exist always add up to the total.
func TestSplitNeverFormsARestHalf(t *testing.T) {
	body := mustScore(t, realInput(realSplit(false)))
	total, _ := sliceProfileOf(body, armsweep.SliceRealName)
	local, _ := sliceProfileOf(body, armsweep.SliceRealLocal)
	global, _ := sliceProfileOf(body, armsweep.SliceRealGlobal)
	if local.N+global.N != total.N || total.N == 0 {
		t.Fatalf("halves %d + %d do not add up to %d", local.N, global.N, total.N)
	}
	if local.Labelled+global.Labelled != total.Labelled {
		t.Errorf("label counts do not add up: %d + %d != %d", local.Labelled, global.Labelled, total.Labelled)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------ gate 4.

// TestSwappedLabelsSwapTheSplitFigures is the assignment probe: a label file
// with the two halves exchanged must move the figures. A split that computed on
// the wrong subset — or on no subset at all — would pass gate 2 unnoticed.
func TestSwappedLabelsSwapTheSplitFigures(t *testing.T) {
	straight := mustScore(t, realInput(realSplit(false)))
	swapped := mustScore(t, realInput(realSplit(true)))

	sLocal := metricsOfConfig(t, straight, armsweep.NameV0, armsweep.SliceRealLocal)
	sGlobal := metricsOfConfig(t, straight, armsweep.NameV0, armsweep.SliceRealGlobal)
	wLocal := metricsOfConfig(t, swapped, armsweep.NameV0, armsweep.SliceRealLocal)
	wGlobal := metricsOfConfig(t, swapped, armsweep.NameV0, armsweep.SliceRealGlobal)

	if math.Abs(sLocal.NDCG10-sGlobal.NDCG10) < 1e-9 {
		t.Fatalf("the fixture halves are indistinguishable (%v / %v) — the probe could not fail",
			sLocal.NDCG10, sGlobal.NDCG10)
	}
	if math.Abs(wLocal.NDCG10-sGlobal.NDCG10) > 1e-12 || math.Abs(wGlobal.NDCG10-sLocal.NDCG10) > 1e-12 {
		t.Errorf("swapping the labels did not swap the figures: local %v->%v, global %v->%v",
			sLocal.NDCG10, wLocal.NDCG10, sGlobal.NDCG10, wGlobal.NDCG10)
	}
	if wLocal.N != nGlobalCases || wGlobal.N != nLocalCases {
		t.Errorf("swapped counts = %d/%d, want %d/%d", wLocal.N, wGlobal.N, nGlobalCases, nLocalCases)
	}

	sTotal := metricsOfConfig(t, straight, armsweep.NameV0, armsweep.SliceRealName)
	wTotal := metricsOfConfig(t, swapped, armsweep.NameV0, armsweep.SliceRealName)
	if sTotal != wTotal {
		t.Errorf("the total row moved with the labels: %+v vs %+v", sTotal, wTotal)
	}
	t.Logf("straight local nDCG %.5f (n=%d) / global %.5f (n=%d); swapped %.5f / %.5f",
		sLocal.NDCG10, sLocal.N, sGlobal.NDCG10, sGlobal.N, wLocal.NDCG10, wGlobal.NDCG10)
}

// ------------------------------------------------------------------ gate 5.

// realCampaign is a congruent four-dump campaign whose G-REAL cases carry gold,
// so `compare` reaches the MDE and effect rows on them at all.
func realCampaign(t *testing.T, dumps ...[]armsweep.Record) armsweep.CompareInput {
	t.Helper()
	for len(dumps) < 4 {
		dumps = append(dumps, realDump())
	}
	dir := t.TempDir()
	mk := func(runID string, recs []armsweep.Record) armsweep.DumpRef {
		stamp := stampFor(runID, runID+".jsonl", recs)
		stamp.SliceFiles = []armsweep.SliceDigest{
			{Slice: goldset.SliceReal, File: goldset.FileReal, SHA256: goldset.SHA256Hex("real"), N: len(recs)},
		}
		return writeDump(t, dir, runID, recs, stamp)
	}
	return armsweep.CompareInput{
		Base: mk("BASE", dumps[0]), Cond: mk("COND", dumps[1]),
		NoisePair:   []armsweep.DumpRef{mk("NOISEA", dumps[2]), mk("NOISEB", dumps[3])},
		RegimeSplit: realSplit(false),
		Seed:        20260812, GitRevision: "deadbeef",
		GoldStamp: goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825,
			CorpusMaxCreatedAt: "2026-08-25T13:49:49.736510Z"},
	}
}

// TestCompareStratumFiguresUseTheLabelledHalves is the assignment probe on the
// comparison path: a condition that only improves the GLOBAL half, and a noise
// pair that only disagrees on it, must move the global row and leave the local
// one flat. A stratum row that quietly recomputed the whole slice would show
// the same figure twice.
func TestCompareStratumFiguresUseTheLabelledHalves(t *testing.T) {
	// The replicate disagrees on TWO of the four global cases and by different
	// amounts: identical deltas would leave the bootstrap CI zero wide, and an
	// MDE of 0 would then say nothing about which cases it was read on.
	noiseB := realDump()
	noiseB[nLocalCases] = realCase(nLocalCases, 5)
	noiseB[nLocalCases+1] = realCase(nLocalCases+1, 3)
	in := realCampaign(t,
		realDumpAt(shallowDepth, deepDepth), // base
		realDumpAt(shallowDepth, 2),         // cond: only the global half improves
		realDumpAt(shallowDepth, deepDepth), // noise a
		noiseB,                              // noise b: disagrees on the global half only
	)
	body, err := armsweep.Compare(in)
	if err != nil && !errors.Is(err, armsweep.ErrGateRefused) {
		t.Fatalf("Compare: %v", err)
	}

	localEff := mustEffect(t, body, armsweep.SliceRealLocal)
	globalEff := mustEffect(t, body, armsweep.SliceRealGlobal)
	if localEff.DeltaNDCG10 != 0 {
		t.Errorf("the local half moved although only global cases changed: %v", localEff.DeltaNDCG10)
	}
	if globalEff.DeltaNDCG10 <= 0 {
		t.Errorf("ΔnDCG@10 on the global half = %v, want > 0", globalEff.DeltaNDCG10)
	}

	localMDE, okL := mdeOn(body, armsweep.SliceRealLocal)
	globalMDE, okG := mdeOn(body, armsweep.SliceRealGlobal)
	totalMDE, okT := mdeOn(body, armsweep.SliceRealName)
	if !okL || !okG || !okT {
		t.Fatalf("MDE rows missing: local=%v global=%v total=%v", okL, okG, okT)
	}
	if localMDE.MDE != 0 {
		t.Errorf("MDE of the local half = %v, want 0 — its replicates are identical", localMDE.MDE)
	}
	if globalMDE.MDE <= 0 {
		t.Errorf("MDE of the global half = %v, want > 0 — its replicates disagree", globalMDE.MDE)
	}
	t.Logf("MDE total %.5f (n=%d) · local %.5f (n=%d) · global %.5f (n=%d); ΔnDCG local %.5f, global %.5f",
		totalMDE.MDE, totalMDE.N, localMDE.MDE, localMDE.N, globalMDE.MDE, globalMDE.N,
		localEff.DeltaNDCG10, globalEff.DeltaNDCG10)
}

// TestCompareCarriesAnMDERowPerStratum is gate 5: §4.4b demands the resolution
// row PER SLICE, and after this wave the two halves are slices.
func TestCompareCarriesAnMDERowPerStratum(t *testing.T) {
	body := mustCompare(t, realCampaign(t))

	for _, slice := range append([]string{armsweep.SliceRealName}, armsweep.StratumSlices()...) {
		m, ok := mdeOn(body, slice)
		if !ok {
			t.Errorf("no MDE row for %s", slice)
			continue
		}
		e, ok := effectOn(body, slice)
		if !ok {
			t.Errorf("no effect row for %s", slice)
			continue
		}
		t.Logf("%s: n=%d MDE=%.5f resolvable=%v, ΔnDCG=%.5f", slice, m.N, m.MDE, m.Resolvable, e.DeltaNDCG10)
	}
	local, _ := mdeOn(body, armsweep.SliceRealLocal)
	global, _ := mdeOn(body, armsweep.SliceRealGlobal)
	total, _ := mdeOn(body, armsweep.SliceRealName)
	if local.N+global.N != total.N {
		t.Errorf("compare halves %d + %d do not add up to %d", local.N, global.N, total.N)
	}

	// The strata are reported, never a refusal input: the G-NOISE verdict list
	// stays exactly the rollout slices.
	for _, g := range body.Noise {
		if armsweep.IsStratum(g.Slice) {
			t.Errorf("a stratum entered the G-NOISE verdict list: %s", g.Slice)
		}
	}
	for _, d := range body.Displacement {
		if armsweep.IsStratum(d.Slice) && d.RolloutCriterion {
			t.Errorf("displacement row %s claims to be a rollout criterion", d.Slice)
		}
	}
	if body.Env.RegimeLabels == nil {
		t.Error("the compare env carries no label provenance")
	}
}

// TestCompareLeavesEveryOtherRowByteIdentical is the non-regression half of
// gate 5 for the conditional comparison.
func TestCompareLeavesEveryOtherRowByteIdentical(t *testing.T) {
	plain := realCampaign(t)
	plain.RegimeSplit = armsweep.RegimeSplit{}
	before := mustCompare(t, plain)
	after := mustCompare(t, realCampaign(t))

	digest := func(t *testing.T, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return goldset.SHA256Hex(string(b))
	}
	for _, slice := range armsweep.ReportSlices() {
		b, okB := effectOn(before, slice)
		a, okA := effectOn(after, slice)
		if okB != okA {
			t.Errorf("effect row %s present=%v before, %v after", slice, okB, okA)
			continue
		}
		if okB && digest(t, b) != digest(t, a) {
			t.Errorf("effect row %s changed: %s -> %s", slice, digest(t, b), digest(t, a))
		}
	}
	if before.Refused != after.Refused {
		t.Errorf("the split changed the refusal verdict: %v -> %v", before.Refused, after.Refused)
	}
	if len(before.Noise) != len(after.Noise) {
		t.Errorf("the split changed the G-NOISE row count: %d -> %d", len(before.Noise), len(after.Noise))
	}
	t.Logf("compare %s effect row: %s (unchanged)", armsweep.SliceRealName,
		digest(t, mustEffect(t, after, armsweep.SliceRealName)))
}

func mustEffect(t *testing.T, body armsweep.CompareBody, slice string) armsweep.CompareEffect {
	t.Helper()
	e, ok := effectOn(body, slice)
	if !ok {
		t.Fatalf("no effect row for %s", slice)
	}
	return e
}

// TestCompareRefusesAnUncoveredCase is gate 3 on the streaming path: the
// comparison folds case by case, so the refusal has to bite mid-stream.
func TestCompareRefusesAnUncoveredCase(t *testing.T) {
	in := realCampaign(t)
	delete(in.RegimeSplit.Regimes, goldset.SHA256Hex(fmt.Sprintf("%s/%d", goldset.SliceReal, 0)))
	if _, err := armsweep.Compare(in); !errors.Is(err, armsweep.ErrRegimeLabelMissing) {
		t.Fatalf("Compare error = %v, want %v", err, armsweep.ErrRegimeLabelMissing)
	}
}

// ------------------------------------------------------ registry structure.

// TestStrataAreReportedButNeverGate pins the registry decision of this wave in
// the place it is made.
func TestStrataAreReportedButNeverGate(t *testing.T) {
	t.Parallel()
	for _, s := range armsweep.ReportSlices() {
		if armsweep.IsStratum(s) {
			t.Errorf("%s is in ReportSlices() — the stratum would vote a second time for cases the total row already votes for", s)
		}
	}
	want := []string{armsweep.SliceRealLocal, armsweep.SliceRealGlobal}
	got := armsweep.StratumSlices()
	if len(got) != len(want) {
		t.Fatalf("StratumSlices() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("StratumSlices()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	census := armsweep.CensusSlices()
	for _, s := range want {
		found := false
		for _, c := range census {
			if c == s {
				found = true
			}
		}
		if !found {
			t.Errorf("CensusSlices() = %v does not carry %s as its own row", census, s)
		}
	}
}

// TestSliceKeysOfFansOutOnlyForLabelledReal pins the fan-out rule: two keys for
// a stamped G-REAL case, one for everything else — including an unstamped
// G-REAL case, which is what a run without labels produces.
func TestSliceKeysOfFansOutOnlyForLabelledReal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rec  armsweep.Record
		want []string
	}{
		{armsweep.Record{Slice: goldset.SliceReal}, []string{armsweep.SliceRealName}},
		{armsweep.Record{Slice: goldset.SliceReal, Regime: goldset.RegimeLocal},
			[]string{armsweep.SliceRealName, armsweep.SliceRealLocal}},
		{armsweep.Record{Slice: goldset.SliceReal, Regime: goldset.RegimeGlobal},
			[]string{armsweep.SliceRealName, armsweep.SliceRealGlobal}},
		{armsweep.Record{Slice: goldset.SliceQ, Split: goldset.SplitHold}, []string{armsweep.SliceQHold}},
		{armsweep.Record{Slice: goldset.SliceKI}, []string{armsweep.SliceKI}},
		// A regime on a slice that has none must not invent a row.
		{armsweep.Record{Slice: goldset.SliceKI, Regime: goldset.RegimeGlobal}, []string{armsweep.SliceKI}},
	}
	for _, tc := range cases {
		got := armsweep.SliceKeysOf(tc.rec)
		if len(got) != len(tc.want) {
			t.Errorf("SliceKeysOf(%s/%s) = %v, want %v", tc.rec.Slice, tc.rec.Regime, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("SliceKeysOf(%s/%s)[%d] = %q, want %q", tc.rec.Slice, tc.rec.Regime, i, got[i], tc.want[i])
			}
		}
	}
}

// ------------------------------------------------------------- label loader.

func TestReadRegimeLabelsRejectsBrokenFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"empty.jsonl":     "\n\n",
		"nosha.jsonl":     `{"regime":"local"}`,
		"badregime.jsonl": `{"query_sha256":"aa","regime":"lokal"}`,
		"dupe.jsonl":      "{\"query_sha256\":\"aa\",\"regime\":\"local\"}\n{\"query_sha256\":\"aa\",\"regime\":\"global\"}",
		"garbage.jsonl":   "not json",
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := goldset.ReadRegimeLabels(p); err == nil {
			t.Errorf("%s was accepted — a broken label file must not produce a partition", name)
		}
	}
	good := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(good,
		[]byte("{\"query_sha256\":\"aa\",\"regime\":\"local\",\"session_bezogen\":true,\"grenzfall\":false}\n"+
			"{\"query_sha256\":\"bb\",\"regime\":\"global\"}\n"), 0o600); err != nil {
		t.Fatalf("write ok: %v", err)
	}
	m, err := goldset.ReadRegimeLabels(good)
	if err != nil {
		t.Fatalf("ReadRegimeLabels: %v", err)
	}
	if m["aa"] != goldset.RegimeLocal || m["bb"] != goldset.RegimeGlobal || len(m) != 2 {
		t.Errorf("labels = %v, want aa=local bb=global", m)
	}
}

// ------------------------------------------------------- the real labels.

// TestRealLabelsPartitionGReal is gate 2 on the MEASURED data: 131 + 19 = 150,
// every G-REAL case covered, no label pointing at a case that does not exist.
//
// Counts and structure only — no query text and no digest of a single case
// reaches the output, the same rule goldbound_test.go follows.
func TestRealLabelsPartitionGReal(t *testing.T) {
	dir := goldDir(t)
	labelPath := filepath.Join(dir, goldset.FileRegimeLabels)
	if _, err := os.Stat(labelPath); err != nil {
		t.Skipf("X-W0 label file unavailable (%s) — private data", goldset.FileRegimeLabels)
	}
	regimes, err := goldset.ReadRegimeLabels(labelPath)
	if err != nil {
		t.Fatalf("ReadRegimeLabels: %v", err)
	}
	cases, err := goldset.ReadJSONL(filepath.Join(dir, goldset.FileReal))
	if err != nil {
		t.Fatalf("read %s: %v", goldset.FileReal, err)
	}

	counts := map[string]int{}
	uncovered := 0
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.QuerySHA256] = true
		r, ok := regimes[c.QuerySHA256]
		if !ok {
			uncovered++
			continue
		}
		counts[r]++
	}
	if uncovered > 0 {
		t.Errorf("%d of %d G-REAL cases carry no regime label — the split would refuse", uncovered, len(cases))
	}
	stray := 0
	for sha := range regimes {
		if !seen[sha] {
			stray++
		}
	}
	if stray > 0 {
		t.Errorf("%d labels point at a query that is not in %s", stray, goldset.FileReal)
	}
	if counts[goldset.RegimeLocal] != 131 || counts[goldset.RegimeGlobal] != 19 {
		t.Errorf("regime split = %d local / %d global, want 131/19 (X-W0 report §5)",
			counts[goldset.RegimeLocal], counts[goldset.RegimeGlobal])
	}
	if counts[goldset.RegimeLocal]+counts[goldset.RegimeGlobal] != len(cases) || len(cases) != 150 {
		t.Errorf("%d + %d != %d cases", counts[goldset.RegimeLocal], counts[goldset.RegimeGlobal], len(cases))
	}
	t.Logf("real labels: %d local + %d global = %d of %d G-REAL cases",
		counts[goldset.RegimeLocal], counts[goldset.RegimeGlobal],
		counts[goldset.RegimeLocal]+counts[goldset.RegimeGlobal], len(cases))
}
