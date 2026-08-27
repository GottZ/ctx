package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// Wave X-W1a, driver half: the CLI must be able to MEASURE every registry
// slice, and its stamp must say which ones it measured.
//
// The stamp is the load-bearing part. A prime that quietly measured three of
// seven slices still writes a prime stamp, and that stamp is what a later
// `compare` reads to decide whether two dumps are one campaign — an undercount
// that reaches the stamp is an undercount nothing downstream can notice.

// xw1aSliceFiles is the file each registry slice lives in. Written out here on
// purpose: a fixture that asked the code under test where the bytes are could
// not catch a mapping that forgot one.
var xw1aSliceFiles = map[string]string{
	goldset.SliceKI:         goldset.FileKI,
	goldset.SliceQ:          goldset.FileQ,
	goldset.SliceReal:       goldset.FileReal,
	goldset.SliceSess:       goldset.FileSess,
	goldset.SliceMH:         goldset.FileMH,
	goldset.SliceGlob:       goldset.FileGlob,
	goldset.SliceGlobKonstr: goldset.FileGlobKonstr,
}

// xw1aAllSlices is the registry, spelled out. The gates assert against THIS
// list rather than against armsweep.CanonicalSlices(): a gate measured against
// the list the code derives its behaviour from would pass on any list.
var xw1aAllSlices = []string{
	goldset.SliceKI, goldset.SliceQ, goldset.SliceReal,
	goldset.SliceSess, goldset.SliceMH, goldset.SliceGlob, goldset.SliceGlobKonstr,
}

const xw1aPerSlice = 2

// fullGold writes a gold directory carrying ALL registry slices, two cases
// each. Synthetic — the real gold set is private and root-only.
func fullGold(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), goldset.DirName)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	for slice, file := range xw1aSliceFiles {
		p, rerr := g.Resolve(file)
		if rerr != nil {
			t.Fatalf("resolve %s: %v", file, rerr)
		}
		cases := make([]goldset.Case, 0, xw1aPerSlice)
		for i := range xw1aPerSlice {
			q := slice + " Frage " + strconv.Itoa(i)
			cases = append(cases, goldset.Case{
				Slice: slice, Index: i, Query: q, QuerySHA256: goldset.SHA256Hex(q),
				Origin: "x-w1a-fixture", GoldIDs: []string{slice + "-gold-" + strconv.Itoa(i)},
			})
		}
		if werr := goldset.WriteJSONL(p, cases); werr != nil {
			t.Fatalf("write %s: %v", file, werr)
		}
	}
	sp, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		t.Fatalf("resolve stamp: %v", err)
	}
	if err = goldset.WriteStamp(sp, goldset.Stamp{Version: 1, SampleSeed: 1, SplitSeed: 2,
		CorpusMaxCreatedAt: "2026-08-25T13:49:49Z", Slices: map[string]goldset.SliceStamp{}}); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	return dir
}

// TestPrimeDryRunMeasuresAndStampsEverySlice is the end-to-end shape of the
// defect X-W1 measured: seven names in, seven slices primed, and the prime
// stamp naming all seven.
func TestPrimeDryRunMeasuresAndStampsEverySlice(t *testing.T) {
	dir := fullGold(t)
	c := dryCommon(dir)
	c.slices = strings.Join(xw1aAllSlices, ",")

	if err := cmdPrime(context.Background(), c); err != nil {
		t.Fatalf("prime: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "prime-"+c.id()+".json"))
	if err != nil {
		t.Fatalf("read prime stamp: %v", err)
	}
	var stamp armsweep.PrimeStamp
	if err = json.Unmarshal(b, &stamp); err != nil {
		t.Fatalf("decode prime stamp: %v", err)
	}

	want := len(xw1aAllSlices) * xw1aPerSlice
	if stamp.Pins != want {
		t.Errorf("prime stamp reports %d pins, want %d — cases were dropped between the flag and the sweep",
			stamp.Pins, want)
	}
	if strings.Join(stamp.Slices, ",") != strings.Join(xw1aAllSlices, ",") {
		t.Errorf("prime stamp names slices %v, want %v — the stamp undercounts what was measured",
			stamp.Slices, xw1aAllSlices)
	}
}

// TestPrimeRefusesAnUnknownSliceName: the CLI must fail loudly on a typo. The
// old behaviour turned `-slices G-SESS,...` into a silent 650-case run, and a
// single unknown name into "keine Gold-Fälle geladen" — the same message an
// empty gold directory produces.
func TestPrimeRefusesAnUnknownSliceName(t *testing.T) {
	dir := fullGold(t)
	c := dryCommon(dir)
	c.slices = goldset.SliceKI + ",G-SES"

	err := cmdPrime(context.Background(), c)
	if err == nil {
		t.Fatal("prime over an unknown slice name returned no error")
	}
	if !strings.Contains(err.Error(), "G-SES") {
		t.Errorf("the refusal does not name the unknown slice: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "prime-"+c.id()+".json")); serr == nil {
		t.Error("the refused run still wrote a prime stamp")
	}
}

// TestGoldDigestsCoverEverySliceMeasured is the stamp half of the congruence
// gate: `gold_sha256` must be computed over the slice files that were actually
// measured, or a seven-slice dump and a three-slice dump would carry the same
// value and compare as one campaign.
func TestGoldDigestsCoverEverySliceMeasured(t *testing.T) {
	dir := fullGold(t)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}

	digestOver := func(names []string) (armsweep.DumpStamp, string) {
		t.Helper()
		cases, lerr := armsweep.LoadCases(g, names)
		if lerr != nil {
			t.Fatalf("load %v: %v", names, lerr)
		}
		var stamp armsweep.DumpStamp
		if ferr := fillGoldDigests(g, cases, &stamp); ferr != nil {
			t.Fatalf("fill gold digests %v: %v", names, ferr)
		}
		return stamp, armsweep.CombinedDigest(stamp.SliceFiles)
	}

	all := xw1aAllSlices
	three := []string{goldset.SliceKI, goldset.SliceQ, goldset.SliceReal}

	sevenStamp, sevenDigest := digestOver(all)
	threeStamp, threeDigest := digestOver(three)

	if len(sevenStamp.SliceFiles) != len(all) {
		t.Errorf("stamp carries %d slice digests, want %d", len(sevenStamp.SliceFiles), len(all))
	}
	seen := map[string]armsweep.SliceDigest{}
	for _, sd := range sevenStamp.SliceFiles {
		seen[sd.Slice] = sd
	}
	for _, slice := range all {
		sd, ok := seen[slice]
		if !ok {
			t.Errorf("stamp carries no digest for slice %s — gold_sha256 describes a run that was not made", slice)
			continue
		}
		if sd.File != xw1aSliceFiles[slice] {
			t.Errorf("slice %s stamped as file %q, want %q", slice, sd.File, xw1aSliceFiles[slice])
		}
		if sd.N != xw1aPerSlice {
			t.Errorf("slice %s stamped with n=%d, want %d", slice, sd.N, xw1aPerSlice)
		}
		if sd.SHA256 == "" {
			t.Errorf("slice %s stamped without a digest", slice)
		}
	}

	// The three-slice stamp stays a three-slice stamp: only what was measured is
	// stamped, which is what keeps an existing three-slice campaign congruent.
	if len(threeStamp.SliceFiles) != len(three) {
		t.Errorf("three-slice stamp carries %d digests, want %d", len(threeStamp.SliceFiles), len(three))
	}
	if sevenDigest == threeDigest {
		t.Error("gold_sha256 is identical over three and seven slices — the congruence gate would pass a mixed campaign")
	}
}

// TestSlicesFlagDefaultsToTheWholeRegistry: the default of -slices is the
// registry, not a subset of it. A default that measures three of seven slices
// makes the undercount the OMISSION case, which is the one nobody checks.
func TestSlicesFlagDefaultsToTheWholeRegistry(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	(&common{}).bind(fs)
	f := fs.Lookup("slices")
	if f == nil {
		t.Fatal("no -slices flag")
	}
	want := strings.Join(xw1aAllSlices, ",")
	if f.DefValue != want {
		t.Errorf("-slices default = %q, want %q", f.DefValue, want)
	}
}
