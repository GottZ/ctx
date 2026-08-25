package armsweep_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// goldDir resolves the REAL gold directory, or skips.
//
// The directory lives in the private .project submodule at mode 0700 and is not
// in the repository, so CI and any non-root run skip this file entirely. That
// is the point: the loader contract has to be checkable against the real bytes
// somewhere, and nothing else in the suite may depend on data that is not
// there.
//
// The test reads COUNTS and STRUCTURE only. No query text, no block id and no
// digest of a single case reaches the output — a failure message names a slice
// and a number, never a case.
func goldDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CTX_GOLDSET_DIR")
	if dir == "" {
		dir = "/compose/n8n/.project/" + goldset.DirName
	}
	if _, err := os.Stat(filepath.Join(dir, goldset.FileStamp)); err != nil {
		t.Skipf("gold directory unavailable (%s) — private data, CI skips this", dir)
	}
	return dir
}

// TestGoldLoaderCompatibility pins that the driver's loader agrees with the
// gold set B-W4 actually produced: three slices at 300/200/150, the G-Q split
// at 100/100, and labels present exactly where the design says they are.
func TestGoldLoaderCompatibility(t *testing.T) {
	dir := goldDir(t)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("gold guard: %v", err)
	}

	cases, err := armsweep.LoadCases(g, armsweep.CanonicalSlices())
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}

	perSlice := map[string]int{}
	labelled := map[string]int{}
	perSplit := map[string]int{}
	for _, c := range cases {
		perSlice[c.Slice]++
		if len(c.GoldIDs) > 0 {
			labelled[c.Slice]++
		}
		if c.Slice == goldset.SliceQ {
			perSplit[c.Split]++
		}
		if c.QuerySHA256 == "" {
			t.Fatalf("slice %s carries a case without a query digest — reports could not cite it", c.Slice)
		}
	}

	want := map[string]int{goldset.SliceKI: 300, goldset.SliceQ: 200, goldset.SliceReal: 150}
	for slice, n := range want {
		if perSlice[slice] != n {
			t.Errorf("slice %s: %d cases, want %d", slice, perSlice[slice], n)
		}
	}
	if perSplit[goldset.SplitDeriv] != 100 || perSplit[goldset.SplitHold] != 100 {
		t.Errorf("G-Q split = %d DERIV / %d HOLD, want 100/100",
			perSplit[goldset.SplitDeriv], perSplit[goldset.SplitHold])
	}
	if labelled[goldset.SliceKI] != 300 || labelled[goldset.SliceQ] != 200 {
		t.Errorf("labels = %d G-KI / %d G-Q, want every constructive case labelled",
			labelled[goldset.SliceKI], labelled[goldset.SliceQ])
	}
	if labelled[goldset.SliceReal] != 0 {
		t.Errorf("G-REAL carries %d labels — it must stay unlabelled until B-W6", labelled[goldset.SliceReal])
	}

	// The report slices the driver derives must partition the labelled cases.
	byReportSlice := map[string]int{}
	for _, c := range cases {
		byReportSlice[armsweep.SliceKeyOf(armsweep.Record{Slice: c.Slice, Split: c.Split})]++
	}
	if byReportSlice[armsweep.SliceQDeriv] != 100 || byReportSlice[armsweep.SliceQHold] != 100 {
		t.Errorf("report slices = %d %s / %d %s, want 100/100",
			byReportSlice[armsweep.SliceQDeriv], armsweep.SliceQDeriv,
			byReportSlice[armsweep.SliceQHold], armsweep.SliceQHold)
	}
	t.Logf("gold loader: %d cases total, %d/%d/%d per slice, split %d/%d",
		len(cases), perSlice[goldset.SliceKI], perSlice[goldset.SliceQ], perSlice[goldset.SliceReal],
		perSplit[goldset.SplitDeriv], perSplit[goldset.SplitHold])
}

// TestGoldStampIsReadable pins the stamp fields the env block copies. Values
// that identify the corpus (the draw instant, the seeds) are structural
// provenance, not content, and appear only as presence checks here.
func TestGoldStampIsReadable(t *testing.T) {
	dir := goldDir(t)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("gold guard: %v", err)
	}
	p, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		t.Fatalf("resolve stamp: %v", err)
	}
	s, err := goldset.ReadStamp(p)
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if s.CorpusMaxCreatedAt == "" {
		t.Error("stamp carries no corpus_max_created_at — the contamination probe would be inert")
	}
	if s.SampleSeed == 0 || s.SplitSeed == 0 {
		t.Errorf("stamp seeds = %d/%d, want both set", s.SampleSeed, s.SplitSeed)
	}
	if s.Generator == nil || s.Generator.Model == "" || s.Generator.Endpoint == "" {
		t.Error("stamp carries no generator provenance — the env block would omit it")
	}
	if s.RetrievableBlocks == 0 {
		t.Error("stamp reports no retrievable blocks")
	}
	for _, slice := range armsweep.CanonicalSlices() {
		st, ok := s.Slices[slice]
		if !ok {
			t.Errorf("stamp has no entry for slice %s", slice)
			continue
		}
		if st.SHA256 == "" || st.File == "" {
			t.Errorf("slice %s stamp lacks a file digest", slice)
		}
	}
}
