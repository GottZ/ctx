package armsweep_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// synthDump builds a deterministic record set: three slices, gold labels that
// the live weights rank well, and enough arms firing that a weight change can
// actually move something.
//
// It is NOT drawn from the real gold data. The gold directory is private and
// root-only, and a unit test that needed it could not run in CI — the
// gold-bound tests are separate and skip without the files.
func synthDump(t *testing.T, seed uint64) []armsweep.Record {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 0x62773573)) // "bw5s"
	var out []armsweep.Record

	add := func(slice, split string, n, rows int, labelled bool) {
		for i := 0; i < n; i++ {
			rec := armsweep.Record{
				Slice: slice, Index: i, Split: split,
				QuerySHA256:    goldset.SHA256Hex(slice + string(rune('a'+i%26)) + split),
				EffectiveQuery: "synthetic",
				Selector:       armsweep.Selector{Mode: "ann", Reason: "disabled"},
				Attempts:       1, LatencyMS: int64(100 + i),
			}
			if i%5 == 0 {
				rec.EffectiveTemporal = "2026"
			}
			ids := make([]string, rows)
			for j := 0; j < rows; j++ {
				ids[j] = idFor(slice, i, j)
				rec.Rows = append(rec.Rows, rrf.ArmRow{
					ID:           ids[j],
					RankSemantic: maybeRank(r, j+1, 0.85),
					RankFTSDe:    maybeRank(r, rows-j, 0.55),
					RankFTSEn:    maybeRank(r, 1+(j*3)%rows, 0.55),
					RankTrigram:  maybeRank(r, 1+(j*7)%rows, 0.30),
					MassFactor:   1, TypeFactor: 1,
				})
			}
			if labelled {
				rec.GoldIDs = []string{ids[i%3]}
			}
			for j := 0; j < 5 && j < rows; j++ {
				rec.Delivered = append(rec.Delivered, armsweep.Delivered{ID: ids[j]})
			}
			rec.FusionOrder = armsweep.FusedIDs(armsweep.Fuse(rec.Rows, armsweep.ConfigV0()))
			out = append(out, rec)
		}
	}
	add(goldset.SliceKI, "", 30, 12, true)
	add(goldset.SliceQ, goldset.SplitDeriv, 24, 12, true)
	add(goldset.SliceQ, goldset.SplitHold, 24, 12, true)
	add(goldset.SliceReal, "", 10, 12, false)
	return out
}

func idFor(slice string, caseIdx, row int) string {
	return goldset.SHA256Hex(slice)[:8] + "-" + string(rune('A'+caseIdx%26)) + "-" + string(rune('a'+row))
}

// maybeRank returns a rank with probability p, nil otherwise — the FULL OUTER
// JOIN shape where an arm simply did not find the candidate.
func maybeRank(r *rand.Rand, v int, p float64) *int {
	if r.Float64() > p {
		return nil
	}
	out := v
	return &out
}

func synthStamp(runID string, recs []armsweep.Record, excluded []armsweep.ExcludedCase) armsweep.DumpStamp {
	return armsweep.DumpStamp{
		RunID: runID, CreatedAt: "2026-08-26T00:00:00Z", BaseURL: "http://ctx",
		Records: len(recs), DumpFile: "dumps/" + runID + ".jsonl",
		PinFile: "pins-" + runID + ".jsonl", PinRunID: runID, PinSHA256: goldset.SHA256Hex(runID),
		GoldStamp: goldset.SHA256Hex("stamp"), MigrationsMax: 139,
		PostFusionStages: map[string]any{
			"cluster.enabled": false, "cluster.inject_max": float64(0),
			"graph.enabled": false, "rerank.enabled": false,
		},
		SliceFiles: []armsweep.SliceDigest{
			{Slice: goldset.SliceKI, File: goldset.FileKI, SHA256: goldset.SHA256Hex("ki"), N: 30},
			{Slice: goldset.SliceQ, File: goldset.FileQ, SHA256: goldset.SHA256Hex("q"), N: 48},
		},
		Excluded: excluded,
		Latency:  armsweep.SummariseLatency([]int64{100, 200, 300}),
	}
}

func synthInput(t *testing.T, withB bool) armsweep.ScoreInput {
	t.Helper()
	a := synthDump(t, 7)
	in := armsweep.ScoreInput{
		RecordsA: a, StampA: synthStamp("A", a, nil),
		Seed: 20260812, GitRevision: "deadbeef",
		GoldStamp: goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825,
			CorpusMaxCreatedAt: "2026-08-25T13:49:49.736510Z"},
	}
	if withB {
		b := synthDump(t, 7) // same seed ⇒ the clean replicate
		sb := synthStamp("B", b, nil)
		in.RecordsB, in.StampB = b, &sb
	}
	return in
}

// --------------------------------------------------------------------------
// Gate (c): report determinism.

// TestReportIsByteIdentical is gate (c): scoring the same dump twice must
// produce the same bytes.
//
// The failure mode it exists for is a map range without a sort — Go randomises
// map iteration deliberately, so such a bug is invisible in a single run and
// shows up as a diff in every second one. Mutation proof: replacing the sorted
// walk in unionExcluded with a bare `for k := range byKey` fails this test with
// `report body differs between two runs of Score`.
func TestReportIsByteIdentical(t *testing.T) {
	in := synthInput(t, true)
	// Exclusions on BOTH sides, deliberately: they are the report's one
	// map-derived list, so a run without them would leave the map-order path
	// unexercised and the gate would be green for the wrong reason.
	in.StampA.Excluded = []armsweep.ExcludedCase{
		{Slice: in.RecordsA[3].Slice, Index: 3, QuerySHA256: in.RecordsA[3].QuerySHA256, Attempts: 3, Reason: "embed backend down"},
		{Slice: in.RecordsA[9].Slice, Index: 9, QuerySHA256: in.RecordsA[9].QuerySHA256, Attempts: 3, Reason: "statement timeout"},
	}
	in.StampB.Excluded = []armsweep.ExcludedCase{
		{Slice: in.RecordsB[40].Slice, Index: in.RecordsB[40].Index, QuerySHA256: in.RecordsB[40].QuerySHA256, Attempts: 3, Reason: "connection reset"},
		{Slice: in.RecordsB[3].Slice, Index: 3, QuerySHA256: in.RecordsB[3].QuerySHA256, Attempts: 3, Reason: "embed backend down"},
	}
	first, err := armsweep.MarshalBody(mustScore(t, in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := armsweep.MarshalBody(mustScore(t, in))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("report body differs between two runs of Score (iteration %d)\nfirst %d bytes, again %d bytes\n%s",
				i, len(first), len(again), firstDiff(first, again))
		}
	}
}

// TestReportFileIsByteIdenticalBelowTheHeader pins the ON-DISK shape: the
// timestamp lives in the header line, and everything below it is stable.
func TestReportFileIsByteIdenticalBelowTheHeader(t *testing.T) {
	dir := t.TempDir()
	body := mustScore(t, synthInput(t, true))
	p1, p2 := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := armsweep.WriteReport(p1, "2026-08-26T10:00:00Z", body); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := armsweep.WriteReport(p2, "2026-08-26T23:59:59Z", body); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	b1, err := armsweep.ReadReportBody(p1)
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	b2, err := armsweep.ReadReportBody(p2)
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("report bodies differ although only the header timestamp changed\n%s", firstDiff(b1, b2))
	}
	raw1, _ := os.ReadFile(p1)
	raw2, _ := os.ReadFile(p2)
	if bytes.Equal(raw1, raw2) {
		t.Error("the two files are identical — the header is not carrying the timestamp")
	}
	if fi, err := os.Stat(p1); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("report mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
}

// TestMarkdownIsByteIdenticalBelowTheHeader pins the same property for the
// human-readable half.
func TestMarkdownIsByteIdenticalBelowTheHeader(t *testing.T) {
	body := mustScore(t, synthInput(t, true))
	a := armsweep.RenderMarkdown("2026-08-26T10:00:00Z", body)
	b := armsweep.RenderMarkdown("2026-08-26T23:59:59Z", body)
	if a == b {
		t.Fatal("markdown ignored the timestamp entirely")
	}
	if strings.SplitN(a, "\n", 2)[1] != strings.SplitN(b, "\n", 2)[1] {
		t.Error("markdown body differs below the header line")
	}
	if strings.Contains(a, "synthetic") {
		t.Error("markdown carries an effective query text — reports cite slice+index+sha only")
	}
}

func firstDiff(a, b []byte) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			lo := i - 80
			if lo < 0 {
				lo = 0
			}
			hi := i + 80
			if hi > n {
				hi = n
			}
			return fmt.Sprintf("first difference at byte %d\nA: %s\nB: %s", i, a[lo:hi], b[lo:hi])
		}
	}
	return "one is a prefix of the other"
}

// --------------------------------------------------------------------------
// G-NOISE.

// TestNoiseGatePassesOnACleanReplicate is the control: two identical dumps
// disagree about nothing, so the gate must pass. Without this half the negative
// probe below could pass for the wrong reason.
func TestNoiseGatePassesOnACleanReplicate(t *testing.T) {
	body := mustScore(t, synthInput(t, true))
	if len(body.Noise) == 0 {
		t.Fatal("no G-NOISE verdict although a second dump was supplied")
	}
	for _, g := range body.Noise {
		if !g.Pass {
			t.Errorf("slice %s: G-NOISE failed on an identical replicate: %v", g.Slice, g.Reasons)
		}
	}
	if !body.Interpretable {
		t.Error("report marked uninterpretable although G-NOISE passed on every slice")
	}
}

// TestNoiseGateFailsOnAPerturbedReplicate is the §4.9 negative probe: ten
// seeded rank swaps per case in the replicate must turn G-NOISE red. A gate
// that cannot be made to fail is not a gate.
func TestNoiseGateFailsOnAPerturbedReplicate(t *testing.T) {
	in := synthInput(t, true)
	in.RecordsB = perturbRanks(in.RecordsB, 10, 424242)

	body := mustScore(t, in)
	failed := false
	for _, g := range body.Noise {
		t.Logf("slice %s: discordance %.4f (limit %.2f), CI [%.5f, %.5f], pass=%v",
			g.Slice, g.Discordance, g.Threshold, g.CILo, g.CIHi, g.Pass)
		if !g.Pass {
			failed = true
		}
	}
	if !failed {
		t.Error("G-NOISE passed on a perturbed replicate — the gate does not bite")
	}
	if body.Interpretable {
		t.Error("report marked interpretable although G-NOISE failed")
	}
	if len(body.Notes) == 0 || !strings.Contains(strings.Join(body.Notes, " "), "G-NOISE") {
		t.Error("a failed G-NOISE must be stated in the report notes")
	}
}

// perturbRanks swaps semantic ranks between random row pairs of each record —
// a corpus that reordered slightly between the two dumps, expressed directly in
// the artefact.
func perturbRanks(recs []armsweep.Record, swaps int, seed uint64) []armsweep.Record {
	r := rand.New(rand.NewPCG(seed, 0x70657274)) // "pert"
	out := make([]armsweep.Record, len(recs))
	for i, rec := range recs {
		rows := append([]rrf.ArmRow(nil), rec.Rows...)
		for s := 0; s < swaps && len(rows) > 1; s++ {
			a, b := r.IntN(len(rows)), r.IntN(len(rows))
			rows[a].RankSemantic, rows[b].RankSemantic = rows[b].RankSemantic, rows[a].RankSemantic
		}
		rec.Rows = rows
		out[i] = rec
	}
	return out
}

// --------------------------------------------------------------------------
// Slices, exclusions, structure.

// TestUnlabelledSliceIsSkippedNotScored pins the G-REAL rule: no labels yet
// (they land in B-W6), so the slice is REPORTED and skipped — never scored as
// a column of zeros that would read as "this configuration is terrible here".
func TestUnlabelledSliceIsSkippedNotScored(t *testing.T) {
	body := mustScore(t, synthInput(t, true))

	var real *armsweep.SliceProfile
	for i := range body.Slices {
		if body.Slices[i].Slice == goldset.SliceReal {
			real = &body.Slices[i]
		}
	}
	if real == nil {
		t.Fatal("G-REAL missing from the slice profile — an unlabelled slice must still be reported")
	}
	if !real.Unlabelled || real.Note == "" {
		t.Errorf("G-REAL profile = %+v, want unlabelled with a note", *real)
	}
	for _, c := range body.Configs {
		for _, s := range c.Slices {
			if s.Slice != goldset.SliceReal {
				continue
			}
			if !s.Unlabelled {
				t.Errorf("%s scored G-REAL although it carries no labels", c.Config.Name)
			}
			if s.NDCG10 != 0 || s.Recall5 != 0 {
				t.Errorf("%s reported metrics on the unlabelled slice: %+v", c.Config.Name, s)
			}
		}
	}
	for _, cmp := range body.Comparisons {
		if cmp.Slice == goldset.SliceReal {
			t.Errorf("comparison %s on the unlabelled slice %s", cmp.Config, cmp.Slice)
		}
	}
}

// TestExclusionsAreTheUnionOverTheDumpPair pins §4.9: a case excluded from
// EITHER dump leaves BOTH, or the replicate comparison would run over two
// different populations.
func TestExclusionsAreTheUnionOverTheDumpPair(t *testing.T) {
	in := synthInput(t, true)
	fromA := in.RecordsA[0]
	fromB := in.RecordsB[5]
	in.StampA.Excluded = []armsweep.ExcludedCase{
		{Slice: fromA.Slice, Index: fromA.Index, QuerySHA256: fromA.QuerySHA256, Attempts: 3, Reason: "embed backend down"},
	}
	in.StampB.Excluded = []armsweep.ExcludedCase{
		{Slice: fromB.Slice, Index: fromB.Index, QuerySHA256: fromB.QuerySHA256, Attempts: 3, Reason: "statement timeout"},
	}

	body := mustScore(t, in)
	if len(body.Excluded) != 2 {
		t.Fatalf("report lists %d exclusions, want the union of 2: %+v", len(body.Excluded), body.Excluded)
	}
	if body.Excluded[0].Key() >= body.Excluded[1].Key() {
		t.Error("exclusion list is not key-sorted — the report would not be byte-stable")
	}

	base := mustScore(t, synthInput(t, true))
	for i, s := range body.Slices {
		if s.Slice == fromA.Slice || s.Slice == armsweep.SliceKeyOf(fromB) {
			if s.N >= base.Slices[i].N {
				t.Errorf("slice %s kept %d cases, want fewer than the unexcluded %d", s.Slice, s.N, base.Slices[i].N)
			}
		}
	}
}

// TestSingleDumpCannotBeInterpreted pins the missing-replicate case: without a
// second dump G-NOISE has nothing to measure, so nothing may be read as a
// result — and the report has to say so rather than omitting the gate.
func TestSingleDumpCannotBeInterpreted(t *testing.T) {
	body := mustScore(t, synthInput(t, false))
	if body.Interpretable {
		t.Error("a single dump was marked interpretable")
	}
	if len(body.Noise) != 0 {
		t.Errorf("G-NOISE verdicts without a replicate: %+v", body.Noise)
	}
	if !strings.Contains(strings.Join(body.Notes, " "), "G-NOISE not evaluated") {
		t.Errorf("notes do not state the missing replicate: %v", body.Notes)
	}
}

// TestReportCoversAllSixteenConfigurations pins the column set and the report
// order, V6a/V6b derived in place.
func TestReportCoversAllSixteenConfigurations(t *testing.T) {
	body := mustScore(t, synthInput(t, true))
	if len(body.Configs) != 16 {
		t.Fatalf("%d configurations in the report, want 16", len(body.Configs))
	}
	want := armsweep.ConfigNames()
	for i, c := range body.Configs {
		if c.Config.Name != want[i] {
			t.Errorf("position %d = %q, want %q", i, c.Config.Name, want[i])
		}
	}
	for _, c := range body.Configs {
		if c.Config.Name == armsweep.NameV0Prime && c.Dump != "B" {
			t.Errorf("V0' scored on dump %q, want B — it is the replicate", c.Dump)
		}
		if c.Config.Name == armsweep.NameV6a && c.Config.Note == "" {
			t.Error("V6a carries no derivation note")
		}
	}
}

// TestWinGateLevels pins the Bonferroni split: V1 is the one primary
// comparison at 0.95, every other variant is read at 1−0.05/13 and labelled a
// candidate rather than a result.
func TestWinGateLevels(t *testing.T) {
	body := mustScore(t, synthInput(t, true))
	seen := map[string]armsweep.WinGate{}
	for _, w := range body.Wins {
		seen[w.Config] = w
	}
	v1, ok := seen[armsweep.NameV1]
	if !ok {
		t.Fatal("no G-WIN verdict for V1")
	}
	if !v1.Primary || v1.Level != armsweep.PrimaryLevel {
		t.Errorf("V1 gate = %+v, want the primary comparison at %.2f", v1, armsweep.PrimaryLevel)
	}
	v2, ok := seen["V2"]
	if !ok {
		t.Fatalf("no G-WIN verdict for V2; have %v", seen)
	}
	if v2.Primary || v2.Level != armsweep.SecondaryLevel {
		t.Errorf("V2 gate = %+v, want a secondary comparison at %.6f", v2, armsweep.SecondaryLevel)
	}
	if _, ok := seen[armsweep.NameV0Prime]; ok {
		t.Error("the replicate V0' was evaluated as a win candidate")
	}
	if _, ok := seen[armsweep.NameV0]; ok {
		t.Error("the baseline V0 was compared against itself")
	}
}
