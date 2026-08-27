package armsweep_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// Wave X-W1a pins the measurement loader against the registry.
//
// The defect these gates were written for was MEASURED, not suspected: a
// `prime` over the seven slice names loaded 650 of 1000 cases and exited 0,
// because LoadCases walked its own three-entry table instead of the names it
// had been handed. A silently halved population is the worst failure a
// measurement path can have — every figure downstream stays plausible.
//
// Two properties are pinned, and the second matters more than the first: the
// loader must know every registry slice, AND it must REFUSE a name it cannot
// resolve. Loading more slices while still dropping typos would only move the
// silent gap.

// xw1aFiles is the file each registry slice lives in, written out here rather
// than read from the loader: a fixture that asked the code under test where the
// bytes are could not catch a mapping that forgot one.
var xw1aFiles = map[string]string{
	goldset.SliceKI:         goldset.FileKI,
	goldset.SliceQ:          goldset.FileQ,
	goldset.SliceReal:       goldset.FileReal,
	goldset.SliceSess:       goldset.FileSess,
	goldset.SliceMH:         goldset.FileMH,
	goldset.SliceGlob:       goldset.FileGlob,
	goldset.SliceGlobKonstr: goldset.FileGlobKonstr,
}

// xw1aAll is the registry, spelled out. Every gate below asserts against THIS
// list rather than against armsweep.CanonicalSlices(): a gate that measured the
// loader against the very list the loader derives its behaviour from would pass
// on any list, including the three-entry one that caused the defect.
var xw1aAll = []string{
	goldset.SliceKI, goldset.SliceQ, goldset.SliceReal,
	goldset.SliceSess, goldset.SliceMH, goldset.SliceGlob, goldset.SliceGlobKonstr,
}

// xw1aCounts is the fixture cardinality per slice. The numbers are distinct
// primes-ish on purpose: any subset of them sums to a value no other subset
// reaches, so a wrong total names the missing slice by arithmetic alone.
var xw1aCounts = map[string]int{
	goldset.SliceKI:         13,
	goldset.SliceQ:          8,
	goldset.SliceReal:       5,
	goldset.SliceSess:       4,
	goldset.SliceMH:         3,
	goldset.SliceGlob:       2,
	goldset.SliceGlobKonstr: 1,
}

// xw1aGold writes a synthetic gold directory. Synthetic on purpose: the real
// directory is private and root-only, so a loader contract that could only be
// checked there would not be checked at all on any other machine.
func xw1aGold(t *testing.T, counts map[string]int) *goldset.Guard {
	t.Helper()
	g, err := goldset.NewGuard(filepath.Join(t.TempDir(), goldset.DirName), false)
	if err != nil {
		t.Fatalf("gold guard: %v", err)
	}
	for slice, n := range counts {
		file, ok := xw1aFiles[slice]
		if !ok {
			t.Fatalf("fixture names slice %q, which the registry does not have", slice)
		}
		p, rerr := g.Resolve(file)
		if rerr != nil {
			t.Fatalf("resolve %s: %v", file, rerr)
		}
		cases := make([]goldset.Case, 0, n)
		for i := range n {
			q := slice + " Frage " + strconv.Itoa(i)
			cases = append(cases, goldset.Case{
				Slice: slice, Index: i, Query: q,
				QuerySHA256: goldset.SHA256Hex(q), Origin: "x-w1a-fixture",
				GoldIDs: xw1aGoldIDs(slice, i),
			})
		}
		if werr := goldset.WriteJSONL(p, cases); werr != nil {
			t.Fatalf("write %s: %v", file, werr)
		}
	}
	return g
}

// xw1aGoldIDs mirrors the real gold set's shape: the two slices whose
// judgements are still outstanding carry no ids, everything else carries some.
func xw1aGoldIDs(slice string, i int) []string {
	if slice == goldset.SliceReal || slice == goldset.SliceGlob {
		return nil
	}
	return []string{slice + "-gold-" + strconv.Itoa(i)}
}

func xw1aTotal(slices ...string) int {
	sum := 0
	for _, s := range slices {
		sum += xw1aCounts[s]
	}
	return sum
}

// --------------------------------------------------------------- gate 1 + 2.

// TestLoadCasesLoadsEveryRegistrySlice is the direct inverse of the measured
// defect: hand the loader all seven names and it must return all seven slices,
// in the canonical order every artefact and report is written in.
func TestLoadCasesLoadsEveryRegistrySlice(t *testing.T) {
	t.Parallel()
	g := xw1aGold(t, xw1aCounts)

	cases, err := armsweep.LoadCases(g, xw1aAll)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	want := xw1aTotal(xw1aAll...)
	if len(cases) != want {
		t.Fatalf("loaded %d cases, want %d — a slice was dropped without a word", len(cases), want)
	}

	perSlice := map[string]int{}
	var order []string
	for _, c := range cases {
		if perSlice[c.Slice] == 0 {
			order = append(order, c.Slice)
		}
		perSlice[c.Slice]++
	}
	for slice, n := range xw1aCounts {
		if perSlice[slice] != n {
			t.Errorf("slice %s: %d cases, want %d", slice, perSlice[slice], n)
		}
	}
	if strings.Join(order, ",") != strings.Join(xw1aAll, ",") {
		t.Errorf("slice order = %v, want %v — artefact order is not a detail, the pin file is read positionally",
			order, xw1aAll)
	}
}

// TestLoadCasesLoadsExactlyTheNamedSubset: the loader follows the NAMES, not a
// table of its own. A run over two slices loads those two and nothing else.
func TestLoadCasesLoadsExactlyTheNamedSubset(t *testing.T) {
	t.Parallel()
	g := xw1aGold(t, xw1aCounts)

	for _, names := range [][]string{
		{goldset.SliceSess, goldset.SliceGlob},
		{goldset.SliceGlobKonstr},
		{goldset.SliceKI, goldset.SliceMH},
	} {
		cases, err := armsweep.LoadCases(g, names)
		if err != nil {
			t.Fatalf("load %v: %v", names, err)
		}
		if len(cases) != xw1aTotal(names...) {
			t.Errorf("load %v: %d cases, want %d", names, len(cases), xw1aTotal(names...))
		}
		for _, c := range cases {
			if !slices.Contains(names, c.Slice) {
				t.Errorf("load %v returned a case of slice %s", names, c.Slice)
			}
		}
	}
}

// TestLoadCasesRefusesAnUnknownSliceName is the fail-closed half. A typo must
// be indistinguishable from nothing at the EXIT code and distinguishable in the
// MESSAGE — the old behaviour had it exactly the wrong way round: `G-SESS`
// among six good names produced a silent undercount and exit 0.
func TestLoadCasesRefusesAnUnknownSliceName(t *testing.T) {
	t.Parallel()
	g := xw1aGold(t, xw1aCounts)

	for _, names := range [][]string{
		{"G-SES"},
		{goldset.SliceKI, "G-SES"},
		{goldset.SliceKI, goldset.SliceQ, goldset.SliceReal, "G-GIBTSNICHT"},
	} {
		cases, err := armsweep.LoadCases(g, names)
		if err == nil {
			t.Fatalf("load %v returned %d cases and no error — an unresolvable name was skipped",
				names, len(cases))
		}
		if len(cases) != 0 {
			t.Errorf("load %v returned %d cases alongside the refusal", names, len(cases))
		}
		bad := names[len(names)-1]
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("the refusal does not name %q: %v", bad, err)
		}
		if !strings.Contains(err.Error(), goldset.SliceGlobKonstr) {
			t.Errorf("the refusal does not list the known slices: %v", err)
		}
	}
}

// TestLoadCasesRefusesASliceWithoutCases: a named slice whose file is there but
// empty contributes nothing. That is the same silent undercount as a dropped
// name, one layer down, and it is refused for the same reason.
func TestLoadCasesRefusesASliceWithoutCases(t *testing.T) {
	t.Parallel()
	g := xw1aGold(t, xw1aCounts)
	p, err := g.Resolve(goldset.FileGlob)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err = os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if cases, lerr := armsweep.LoadCases(g, []string{goldset.SliceKI, goldset.SliceGlob}); lerr == nil {
		t.Fatalf("load returned %d cases and no error over an empty slice file", len(cases))
	} else if !strings.Contains(lerr.Error(), goldset.SliceGlob) {
		t.Errorf("the refusal does not name the empty slice: %v", lerr)
	}
}

// TestLoadCasesRefusesWithoutAnySliceName: an empty name list is a measurement
// over nothing. The caller used to turn it into "keine Gold-Fälle geladen",
// which reads like an empty gold directory rather than an empty request.
func TestLoadCasesRefusesWithoutAnySliceName(t *testing.T) {
	t.Parallel()
	g := xw1aGold(t, xw1aCounts)
	if cases, err := armsweep.LoadCases(g, nil); err == nil {
		t.Fatalf("load with no names returned %d cases and no error", len(cases))
	}
}

// TestCanonicalSlicesIsTheWholeRegistry: the measurement order and the report
// registry describe the same seven slices. They are two lists — CanonicalSlices
// is the order of the loader and the pin file, ReportSlices/FloorSlices the
// order of the report — and this gate is what keeps them from drifting apart.
func TestCanonicalSlicesIsTheWholeRegistry(t *testing.T) {
	t.Parallel()
	want := []string{
		goldset.SliceKI, goldset.SliceQ, goldset.SliceReal,
		goldset.SliceSess, goldset.SliceMH, goldset.SliceGlob, goldset.SliceGlobKonstr,
	}
	got := armsweep.CanonicalSlices()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("CanonicalSlices() = %v, want %v", got, want)
	}

	// Every report row must be reachable from a measurable slice. G-Q is the one
	// slice that fans out (DERIV/HOLD), and the strata are a re-partition of
	// G-REAL rather than files of their own, so both are mapped, not compared.
	measured := map[string]bool{}
	for _, s := range got {
		if s == goldset.SliceQ {
			measured[armsweep.SliceQDeriv], measured[armsweep.SliceQHold] = true, true
			continue
		}
		measured[s] = true
	}
	for _, s := range append(armsweep.ReportSlices(), armsweep.FloorSlices()...) {
		if !measured[s] {
			t.Errorf("report slice %s has no measurable slice behind it — the report would ask for a row no run can fill", s)
		}
	}
	if !measured[armsweep.SliceRealName] && len(armsweep.StratumSlices()) > 0 {
		t.Errorf("the strata %v have no G-REAL behind them", armsweep.StratumSlices())
	}
}

// --------------------------------------------------------------------- gate 3.

// TestCombinedDigestSeparatesThreeSlicesFromSeven: the gold digest of the stamp
// must MOVE when the measured slice set grows. Without that, a three-slice
// noise pair and a seven-slice conditional dump would look like one campaign.
func TestCombinedDigestSeparatesThreeSlicesFromSeven(t *testing.T) {
	t.Parallel()
	digestsFor := func(names []string) []armsweep.SliceDigest {
		out := make([]armsweep.SliceDigest, 0, len(names))
		for _, s := range names {
			out = append(out, armsweep.SliceDigest{
				Slice: s, File: xw1aFiles[s], SHA256: goldset.SHA256Hex(s), N: xw1aCounts[s],
			})
		}
		return out
	}
	three := armsweep.CombinedDigest(digestsFor([]string{goldset.SliceKI, goldset.SliceQ, goldset.SliceReal}))
	seven := armsweep.CombinedDigest(digestsFor(xw1aAll))
	if three == seven {
		t.Fatal("the gold digest is the same over three and over seven slices — the congruence gate would pass a mixed campaign")
	}
	// Order must NOT matter: the digest sorts its lines, so the same set in a
	// different sequence is the same campaign.
	shuffled := []string{goldset.SliceQ, goldset.SliceReal, goldset.SliceKI}
	if armsweep.CombinedDigest(digestsFor(shuffled)) != three {
		t.Error("the gold digest depends on the order of the slice list")
	}
}

// TestCompareRefusesAThreeAgainstSevenSliceGoldSet drives the same difference
// through the real congruence gate (M-W3d gate (c)), so the pin covers the
// mechanism and not only the digest function.
func TestCompareRefusesAThreeAgainstSevenSliceGoldSet(t *testing.T) {
	t.Parallel()
	c := newCampaign(t, dumpOpts{}, dumpOpts{})
	in := c.input()
	for _, s := range xw1aAll {
		in.Cond.Stamp.SliceFiles = append(in.Cond.Stamp.SliceFiles, armsweep.SliceDigest{
			Slice: s, File: xw1aFiles[s], SHA256: goldset.SHA256Hex(s), N: xw1aCounts[s],
		})
	}
	_, err := armsweep.Compare(in)
	if !errors.Is(err, armsweep.ErrStampIncongruent) {
		t.Fatalf("Compare over a 3-slice and a 7-slice dump returned %v, want ErrStampIncongruent", err)
	}
	if !strings.Contains(err.Error(), "gold_sha256") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
}

// --------------------------------------------------------------------- gate 4.

// TestGoldCensusCountsLabelledCasesPerSlice is the D3 visibility gate against
// the REAL gold set: the report's slice census has to state, per slice, how
// many cases carry judgements — including the two that carry none.
//
// Two slices reading "0 labelled" in the census is the finding X-W1 could not
// put in a report, because the loader never got their cases as far as one.
//
// Counts and structure only. No query text and no block id reaches the output.
func TestGoldCensusCountsLabelledCasesPerSlice(t *testing.T) {
	dir := goldDir(t)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("gold guard: %v", err)
	}
	cases, err := armsweep.LoadCases(g, xw1aAll)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}

	recs := make([]armsweep.Record, 0, len(cases))
	for _, c := range cases {
		recs = append(recs, armsweep.Record{
			Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			Split: c.Split, GoldIDs: c.GoldIDs,
		})
	}

	type row struct{ n, labelled int }
	want := map[string]row{
		armsweep.SliceKI:         {300, 300},
		armsweep.SliceQDeriv:     {100, 100},
		armsweep.SliceQHold:      {100, 100},
		armsweep.SliceRealName:   {150, 0},
		armsweep.SliceSess:       {120, 120},
		armsweep.SliceMH:         {100, 100},
		armsweep.SliceGlob:       {80, 0},
		armsweep.SliceGlobKonstr: {50, 50},
	}

	got := map[string]armsweep.SliceProfile{}
	for _, p := range armsweep.BuildSliceProfiles(recs) {
		got[p.Slice] = p
	}
	for slice, w := range want {
		p, ok := got[slice]
		if !ok {
			t.Errorf("slice %s has no census row — an unmeasured slice is invisible in the report", slice)
			continue
		}
		if p.N != w.n || p.Labelled != w.labelled {
			t.Errorf("census %s: n=%d labelled=%d, want n=%d labelled=%d", slice, p.N, p.Labelled, w.n, w.labelled)
		}
		if p.Unlabelled != (w.labelled == 0) {
			t.Errorf("census %s: unlabelled=%v, want %v", slice, p.Unlabelled, w.labelled == 0)
		}
		if p.Note == "" && w.labelled == 0 {
			t.Errorf("census %s carries no note although it has no judgements", slice)
		}
	}
	if p := got[armsweep.SliceGlobKonstr]; p.RolloutCriterion {
		t.Error("the floor row claims to be a rollout criterion")
	}
	var census []string
	for _, p := range armsweep.BuildSliceProfiles(recs) {
		census = append(census, fmt.Sprintf("%s n=%d labelled=%d rollout=%v",
			p.Slice, p.N, p.Labelled, p.RolloutCriterion))
	}
	t.Logf("gold census over %d cases: %s", len(cases), strings.Join(census, " | "))
}
