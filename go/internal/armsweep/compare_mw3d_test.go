package armsweep_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// The eight gates of wave M-W3d (design/05 §7, row M-W3d) live in this file,
// one test each:
//
//	(a) no -noise-pair              => ErrGateRefused (exit 3)
//	(b) red G-NOISE                 => ErrGateRefused (exit 3)
//	(c) migrations_max / instance_kind incongruent => ErrStampIncongruent (4)
//	(d) two runs                    => byte-identical report body
//	(e) shadow block at rank 1      => exactly one displaced block per query
//	(f) four 290k-line dumps        => streaming stays under an RSS cap
//	    (its own file: compare_stream_mw3d_test.go, it writes ~1.2M records)
//	(g) an artificially noised pair => the reported MDE rises
//	(h) diverging GUCs              => ErrStampIncongruent (4)
//
// Every fixture is synthetic. The real gold directory is private and root-only,
// and a gate that needed it could not run in CI.

const (
	shadowType = "insight"
	plainType  = "knowledge"
	labelledN  = 40
	plainN     = 6
)

// dumpOpts are the knobs one synthetic dump differs from the base dump in —
// every gate of this wave is one knob turned.
type dumpOpts struct {
	// shadow inserts a shadow-type row at fused rank 1 (gate (e)).
	shadow bool
	// shadowRows widens that to several shadow rows — enough of them push the
	// gold rows out of the top five and flip Hit@5, which is what a McNemar
	// discordance is counted on.
	shadowRows int
	// goldDepth places the first gold row at that fused position (1-based); 0
	// keeps it at 1. It moves nDCG WITHOUT moving Hit@5 as long as it stays
	// inside the top five — which is what separates the MDE probe (g) from the
	// discordance probe (b).
	goldDepth func(i int) int
	// pushOut drops the gold rows out of the top five for that case: the
	// Hit@5 flip a McNemar discordance is counted on (gate (b)).
	pushOut func(i int) bool
	// mode/scanTuples are the per-case selector state the GUC congruence of
	// gate (h) is read off.
	mode       string
	scanTuples *int
}

func intp(v int) *int { return &v }

// caseIDs are the eight candidate ids of one case, in the order the live
// weights rank them when every row carries its position as semantic rank.
func caseIDs(slice string, i int) []string {
	out := make([]string, 8)
	for j := range out {
		out[j] = fmt.Sprintf("%s-%03d-%d", strings.ToLower(slice), i, j)
	}
	return out
}

// order returns the row order the fusion produces under o for case i: the
// index at position p gets semantic rank p+1, so the fused ranking is exactly
// this permutation (only the semantic and fts_de arms fire, both monotone in
// the position, so the sort is by position).
func rowOrder(o dumpOpts, i int) []int {
	pos := []int{0, 1, 2, 3, 4, 5, 6, 7}
	depth := 1
	if o.goldDepth != nil {
		depth = o.goldDepth(i)
	}
	if o.pushOut != nil && o.pushOut(i) {
		depth = 7
	}
	if depth <= 1 || depth > len(pos) {
		return pos
	}
	// Move row 0 (the gold row) to position depth-1, shifting the rest up.
	out := append([]int{}, pos[1:depth]...)
	out = append(out, 0)
	return append(out, pos[depth:]...)
}

// records builds one dump: labelledN cases on G-KI carrying gold ids and
// plainN unlabelled cases on G-REAL, so the unlabelled path is exercised too.
func records(o dumpOpts) []armsweep.Record {
	var out []armsweep.Record
	add := func(slice string, n int, labelled bool) {
		for i := 0; i < n; i++ {
			ids := caseIDs(slice, i)
			rec := armsweep.Record{
				Slice: slice, Index: i,
				QuerySHA256:    goldset.SHA256Hex(fmt.Sprintf("%s/%d", slice, i)),
				EffectiveQuery: "synthetic",
				Selector: armsweep.Selector{
					Mode: mode(o), Reason: "grey", Estimate: 1000, ScanTuples: scanTuples(o),
				},
				Attempts: 1, LatencyMS: int64(100 + i),
			}
			if labelled {
				rec.GoldIDs = []string{ids[0]}
				if i%4 == 0 {
					// A second gold id at position 4: the one that gets pushed
					// out of the top five when a shadow block takes rank 1, so
					// the displacement table's "labelled" column is not always 0.
					rec.GoldIDs = append(rec.GoldIDs, ids[4])
				}
			}
			pos := 1
			for s := 0; s < shadowRowCount(o); s++ {
				rec.Rows = append(rec.Rows,
					armRow(fmt.Sprintf("shadow-%s-%03d-%d", strings.ToLower(slice), i, s), shadowType, pos))
				pos++
			}
			for _, idx := range rowOrder(o, i) {
				rec.Rows = append(rec.Rows, armRow(ids[idx], plainType, pos))
				pos++
			}
			rec.FusionOrder = armsweep.FusedIDs(armsweep.Fuse(rec.Rows, armsweep.ConfigV0()))
			for j := 0; j < 5 && j < len(rec.FusionOrder); j++ {
				rec.Delivered = append(rec.Delivered, armsweep.Delivered{ID: rec.FusionOrder[j]})
			}
			out = append(out, rec)
		}
	}
	add(goldset.SliceKI, labelledN, true)
	add(goldset.SliceReal, plainN, false)
	return out
}

func shadowRowCount(o dumpOpts) int {
	if o.shadowRows > 0 {
		return o.shadowRows
	}
	if o.shadow {
		return 1
	}
	return 0
}

func mode(o dumpOpts) string {
	if o.mode != "" {
		return o.mode
	}
	return "ann"
}

func scanTuples(o dumpOpts) *int {
	if o.scanTuples != nil {
		return o.scanTuples
	}
	return intp(60000)
}

// armRow puts a candidate at one fused position: the semantic arm carries the
// position as its rank, the German FTS arm the same for the first four rows.
func armRow(id, typeName string, pos int) rrf.ArmRow {
	row := rrf.ArmRow{ID: id, RankSemantic: intp(pos), MassFactor: 1, TypeFactor: 1, TypeName: typeName}
	if pos <= 4 {
		row.RankFTSDe = intp(pos)
	}
	return row
}

// stampFor is the congruent stamp every dump of the campaign carries. The
// campaign anchor is the PIN run id: `prime` collects the pins for BOTH dumps
// of a condition pair (cmd/ctx-armsweep/commands.go:19-26), so two dumps that
// were pinned from different priming runs are not one campaign.
func stampFor(runID, file string, recs []armsweep.Record) armsweep.DumpStamp {
	return armsweep.DumpStamp{
		RunID: runID, CreatedAt: "2026-08-27T00:00:00Z", BaseURL: "http://ctx",
		Records: len(recs), DumpFile: "dumps/" + file,
		PinFile: "pins-CAMP.jsonl", PinRunID: "CAMP", PinSHA256: goldset.SHA256Hex("pins"),
		GoldStamp: goldset.SHA256Hex("stamp"), MigrationsMax: armsweep.TypeNameMigration,
		PostFusionStages: map[string]any{
			"cluster.enabled": false, "cluster.inject_max": float64(0),
			"graph.enabled": false, "rerank.enabled": false,
		},
		SliceFiles: []armsweep.SliceDigest{
			{Slice: goldset.SliceKI, File: goldset.FileKI, SHA256: goldset.SHA256Hex("ki"), N: labelledN},
			{Slice: goldset.SliceReal, File: goldset.FileReal, SHA256: goldset.SHA256Hex("real"), N: plainN},
		},
		InstanceKind: armsweep.InstanceKindMeasureCopy,
		EfSearch:     "40 (default)",
		Latency:      armsweep.SummariseLatency([]int64{100, 200, 300}),
	}
}

// writeDump persists one dump plus its stamp and returns the reference the
// comparison consumes.
func writeDump(t *testing.T, dir, runID string, recs []armsweep.Record, stamp armsweep.DumpStamp) armsweep.DumpRef {
	t.Helper()
	file := runID + ".jsonl"
	path := filepath.Join(dir, file)
	if err := armsweep.WriteRecords(path, recs); err != nil {
		t.Fatalf("write dump %s: %v", runID, err)
	}
	return armsweep.DumpRef{Role: strings.ToLower(runID), Path: path, Stamp: stamp}
}

// campaign is a congruent four-dump campaign: base, cond and the V0/V0'
// replicate pair that measures the noise floor.
type campaign struct {
	dir                    string
	base, cond, na, nb     armsweep.DumpRef
	baseRecs, condRecs     []armsweep.Record
	noiseARecs, noiseBRecs []armsweep.Record
}

func newCampaign(t *testing.T, condOpts, noiseBOpts dumpOpts, noiseAOpts ...dumpOpts) campaign {
	t.Helper()
	dir := t.TempDir()
	c := campaign{dir: dir}
	na := dumpOpts{}
	if len(noiseAOpts) > 0 {
		na = noiseAOpts[0]
	}
	c.baseRecs = records(dumpOpts{})
	c.condRecs = records(condOpts)
	c.noiseARecs = records(na)
	c.noiseBRecs = records(noiseBOpts)
	c.base = writeDump(t, dir, "BASE", c.baseRecs, stampFor("BASE", "BASE.jsonl", c.baseRecs))
	condStamp := stampFor("COND", "COND.jsonl", c.condRecs)
	if shadowRowCount(condOpts) > 0 {
		condStamp.ShadowTypes = []string{shadowType}
	}
	c.cond = writeDump(t, dir, "COND", c.condRecs, condStamp)
	c.na = writeDump(t, dir, "NOISEA", c.noiseARecs, stampFor("NOISEA", "NOISEA.jsonl", c.noiseARecs))
	c.nb = writeDump(t, dir, "NOISEB", c.noiseBRecs, stampFor("NOISEB", "NOISEB.jsonl", c.noiseBRecs))
	return c
}

func (c campaign) input() armsweep.CompareInput {
	return armsweep.CompareInput{
		Base: c.base, Cond: c.cond,
		NoisePair:   []armsweep.DumpRef{c.na, c.nb},
		Seed:        20260812,
		GitRevision: "deadbeef",
		GoldStamp: goldset.Stamp{SampleSeed: 20260812, SplitSeed: 20260825,
			CorpusMaxCreatedAt: "2026-08-25T13:49:49.736510Z"},
	}
}

func mustCompare(t *testing.T, in armsweep.CompareInput) armsweep.CompareBody {
	t.Helper()
	body, err := armsweep.Compare(in)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return body
}

func effectOn(body armsweep.CompareBody, slice string) (armsweep.CompareEffect, bool) {
	for _, e := range body.Effects {
		if e.Slice == slice {
			return e, true
		}
	}
	return armsweep.CompareEffect{}, false
}

func mdeOn(body armsweep.CompareBody, slice string) (armsweep.MDEReport, bool) {
	for _, m := range body.MDE {
		if m.Slice == slice {
			return m, true
		}
	}
	return armsweep.MDEReport{}, false
}

func displacementOn(body armsweep.CompareBody, slice string) (armsweep.DisplacementRow, bool) {
	for _, d := range body.Displacement {
		if d.Slice == slice {
			return d, true
		}
	}
	return armsweep.DisplacementRow{}, false
}

// --------------------------------------------------------------- gate (a).

// TestCompareRefusesWithoutNoisePair is gate (a): without the V0/V0' pair the
// instrument has no measured noise floor, so a difference between base and cond
// cannot be told from the instrument's own repeat disagreement.
func TestCompareRefusesWithoutNoisePair(t *testing.T) {
	c := newCampaign(t, dumpOpts{}, dumpOpts{})
	for name, pair := range map[string][]armsweep.DumpRef{
		"none": nil,
		"one":  {c.na},
	} {
		in := c.input()
		in.NoisePair = pair
		_, err := armsweep.Compare(in)
		if !errors.Is(err, armsweep.ErrGateRefused) {
			t.Errorf("noise pair %q: Compare returned %v, want ErrGateRefused", name, err)
		}
	}
}

// --------------------------------------------------------------- gate (b).

// TestCompareRefusesRedNoiseFloor is gate (b): the replicate pair disagrees
// beyond the §4.9 tolerance, so nothing measured against it is a result.
func TestCompareRefusesRedNoiseFloor(t *testing.T) {
	// Every fourth case loses its gold rows out of the top five in the second
	// replicate: 25 % discordance on Recall@5, five times the G-NOISE ceiling.
	c := newCampaign(t, dumpOpts{}, dumpOpts{pushOut: func(i int) bool { return i%4 == 0 }})
	body, err := armsweep.Compare(c.input())
	if !errors.Is(err, armsweep.ErrGateRefused) {
		t.Fatalf("Compare over a red noise floor returned %v, want ErrGateRefused", err)
	}
	if !body.Refused {
		t.Error("the refused body does not declare itself refused")
	}
	if len(body.RefusalReasons) == 0 {
		t.Error("a refusal without a reason is not evidence")
	}
	var red bool
	for _, g := range body.Noise {
		if !g.Pass {
			red = true
		}
	}
	if !red {
		t.Error("no noise gate is red, yet the comparison was refused")
	}
	// The evidence has to survive the refusal: the report body is what an
	// operator reads to find the determinism source.
	if len(body.Effects) == 0 {
		t.Error("the refused body carries no effect table — the evidence was suppressed")
	}
}

// --------------------------------------------------------------- gate (c).

// TestCompareRefusesIncongruentStamps is gate (c): a dump pair measured against
// two different schema generations, or against two different instances, is not
// a pair. Both are exit 4 (dump discarded), never exit 3 — a scheduler that
// retries a gate refusal must not retry these.
func TestCompareRefusesIncongruentStamps(t *testing.T) {
	t.Run("migrations_max", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{}, dumpOpts{})
		in := c.input()
		in.Cond.Stamp.MigrationsMax = armsweep.TypeNameMigration + 1
		_, err := armsweep.Compare(in)
		if !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare returned %v, want ErrStampIncongruent", err)
		}
		if !strings.Contains(err.Error(), "migrations_max") {
			t.Errorf("the refusal does not name the field: %v", err)
		}
	})

	// The instance kind is read off the RAW dump stamps. The report env merges
	// the kinds of a pair into one string since the M-W2 nachbesserung
	// (armsweep/report.go:331-341), and a gate computed on that merge would see
	// one value where there are two.
	t.Run("instance_kind", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{}, dumpOpts{})
		in := c.input()
		in.NoisePair[1].Stamp.InstanceKind = armsweep.InstanceKindLive
		_, err := armsweep.Compare(in)
		if !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare returned %v, want ErrStampIncongruent", err)
		}
		if !strings.Contains(err.Error(), "instance_kind") {
			t.Errorf("the refusal does not name the field: %v", err)
		}
	})

	t.Run("gold set", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{}, dumpOpts{})
		in := c.input()
		in.Cond.Stamp.SliceFiles = []armsweep.SliceDigest{
			{Slice: goldset.SliceKI, File: goldset.FileKI, SHA256: goldset.SHA256Hex("other"), N: labelledN},
		}
		if _, err := armsweep.Compare(in); !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare over two gold sets returned %v, want ErrStampIncongruent", err)
		}
	})

	t.Run("campaign anchor", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{}, dumpOpts{})
		in := c.input()
		in.Cond.Stamp.PinRunID = "OTHER"
		if _, err := armsweep.Compare(in); !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare over two priming runs returned %v, want ErrStampIncongruent", err)
		}
	})

	t.Run("post fusion stages", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{}, dumpOpts{})
		in := c.input()
		in.Cond.Stamp.PostFusionStages = map[string]any{
			"cluster.enabled": true, "cluster.inject_max": float64(3),
			"graph.enabled": false, "rerank.enabled": false,
		}
		if _, err := armsweep.Compare(in); !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare over two post-fusion states returned %v, want ErrStampIncongruent", err)
		}
	})
}

// --------------------------------------------------------------- gate (d).

// TestCompareReportIsByteIdentical is gate (d): two runs over the same four
// dumps produce the same bytes. Nothing volatile may reach the body — the
// generation timestamp lives in the header line, as it does for `score`.
func TestCompareReportIsByteIdentical(t *testing.T) {
	c := newCampaign(t, dumpOpts{shadow: true}, dumpOpts{})
	first, err := armsweep.MarshalCompareBody(mustCompare(t, c.input()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := armsweep.MarshalCompareBody(mustCompare(t, c.input()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two comparisons over the same dumps produced different bodies")
	}

	// The same over the artefact on disk: header line volatile, body stable.
	dir := t.TempDir()
	one := filepath.Join(dir, "one.json")
	two := filepath.Join(dir, "two.json")
	if err := armsweep.WriteCompareReport(one, "2026-08-27T01:00:00Z", mustCompare(t, c.input())); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := armsweep.WriteCompareReport(two, "2026-08-27T02:00:00Z", mustCompare(t, c.input())); err != nil {
		t.Fatalf("write: %v", err)
	}
	b1, err := armsweep.ReadReportBody(one)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b2, err := armsweep.ReadReportBody(two)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if goldset.SHA256Hex(string(b1)) != goldset.SHA256Hex(string(b2)) {
		t.Errorf("report bodies differ: %s vs %s", goldset.SHA256Hex(string(b1)), goldset.SHA256Hex(string(b2)))
	}
	head1, _ := os.ReadFile(one)
	head2, _ := os.ReadFile(two)
	if string(head1) == string(head2) {
		t.Error("the two files are identical including the header — the generation timestamp is missing")
	}

	// The markdown half is deterministic from line 2 on for the same reason.
	m1 := armsweep.RenderCompareMarkdown("2026-08-27T01:00:00Z", mustCompare(t, c.input()))
	m2 := armsweep.RenderCompareMarkdown("2026-08-27T02:00:00Z", mustCompare(t, c.input()))
	if body1, body2 := afterFirstLine(m1), afterFirstLine(m2); body1 != body2 {
		t.Error("the markdown body is not deterministic")
	}
}

func afterFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// --------------------------------------------------------------- gate (e).

// TestCompareDisplacementTable is gate (e): a conditional dump in which a
// shadow block takes rank 1 pushes exactly ONE block out of the top five per
// query, and the table says which type lost the place.
func TestCompareDisplacementTable(t *testing.T) {
	c := newCampaign(t, dumpOpts{shadow: true}, dumpOpts{})
	body := mustCompare(t, c.input())

	row, ok := displacementOn(body, goldset.SliceKI)
	if !ok {
		t.Fatalf("no displacement row for %s", goldset.SliceKI)
	}
	if row.Cases != labelledN {
		t.Errorf("displacement over %d cases, want %d", row.Cases, labelledN)
	}
	if row.Displaced != labelledN || row.MinPerCase != 1 || row.MaxPerCase != 1 {
		t.Errorf("displaced=%d min=%d max=%d, want exactly one displaced block per query (%d cases)",
			row.Displaced, row.MinPerCase, row.MaxPerCase, labelledN)
	}
	if row.ShadowAtRank1 != labelledN || row.ShadowInTopK != labelledN {
		t.Errorf("shadow at rank 1: %d, in top %d: %d — want %d each",
			row.ShadowAtRank1, armsweep.DisplacementCut, row.ShadowInTopK, labelledN)
	}
	if len(row.DisplacedByType) != 1 || row.DisplacedByType[0].TypeName != plainType {
		t.Errorf("displaced types = %+v, want %d× %q", row.DisplacedByType, labelledN, plainType)
	}
	if len(row.EntrantsByType) != 1 || row.EntrantsByType[0].TypeName != shadowType {
		t.Errorf("entrant types = %+v, want %q", row.EntrantsByType, shadowType)
	}
	if !row.LabelsAvailable {
		t.Error("the labelled column is declared unavailable on a labelled slice")
	}
	// Every fourth case carries a second gold id at position 4 — exactly the
	// place the shadow block pushes out.
	if want := labelledN / 4; row.DisplacedLabelled != want {
		t.Errorf("displaced labelled blocks = %d, want %d", row.DisplacedLabelled, want)
	}

	// The unlabelled slice reports the same mechanics with an empty label column.
	plain, ok := displacementOn(body, goldset.SliceReal)
	if !ok {
		t.Fatalf("no displacement row for %s", goldset.SliceReal)
	}
	if plain.LabelsAvailable || plain.DisplacedLabelled != 0 {
		t.Errorf("unlabelled slice claims label information: %+v", plain)
	}
	if plain.Displaced != plainN {
		t.Errorf("unlabelled slice displaced %d, want %d", plain.Displaced, plainN)
	}
}

// TestCompareEffectCarriesTheThreeDeltas pins the metric block of §4.3: three
// deltas per slice, each with its own paired bootstrap CI, plus McNemar and the
// F-32 separability rule against the measured noise floor.
func TestCompareEffectCarriesTheThreeDeltas(t *testing.T) {
	// Six shadow rows: the whole top-five window is shadow, every gold row
	// leaves it, and Hit@5 flips on every case — the movement a McNemar
	// discordance is counted on.
	c := newCampaign(t, dumpOpts{shadowRows: 6}, dumpOpts{})
	body := mustCompare(t, c.input())

	e, ok := effectOn(body, goldset.SliceKI)
	if !ok {
		t.Fatalf("no effect row for %s", goldset.SliceKI)
	}
	if e.N != labelledN {
		t.Errorf("effect over %d pairs, want %d", e.N, labelledN)
	}
	// The shadow rows take the whole window, so every gold row moves down:
	// nDCG@10, MRR@10 and Recall@5 must all fall.
	if !(e.DeltaNDCG10 < 0) {
		t.Errorf("ΔnDCG@10 = %.5f, want a loss when a shadow block takes rank 1", e.DeltaNDCG10)
	}
	if !(e.DeltaMRR10 < 0) {
		t.Errorf("ΔMRR@10 = %.5f, want a loss", e.DeltaMRR10)
	}
	if !(e.DeltaRecall5 < 0) {
		t.Errorf("ΔRecall@5 = %.5f, want a loss on the cases with a second gold id", e.DeltaRecall5)
	}
	if !(e.NDCGCIHi < 0) {
		t.Errorf("the ΔnDCG CI [%.5f, %.5f] does not exclude 0 from below", e.NDCGCILo, e.NDCGCIHi)
	}
	if e.RecallCILo == 0 && e.RecallCIHi == 0 {
		t.Error("ΔRecall@5 carries no CI")
	}
	if e.MRRCILo == 0 && e.MRRCIHi == 0 {
		t.Error("ΔMRR@10 carries no CI")
	}
	if e.Level != armsweep.PrimaryLevel {
		t.Errorf("effect level = %v, want the primary level %v", e.Level, armsweep.PrimaryLevel)
	}
	// F-32: the condition moves more cases than the replicate pair does. The
	// replicate pair here is identical, so its discordance is 0.
	if e.NoiseDiscordance != 0 {
		t.Errorf("noise discordance = %.4f over an identical replicate pair", e.NoiseDiscordance)
	}
	if e.Discordance != 1 {
		t.Errorf("discordance = %.4f, want 1 — every case loses its gold out of the window", e.Discordance)
	}
	if !e.Separable {
		t.Errorf("an effect with discordance %.4f over a noise floor of %.4f is declared inseparable",
			e.Discordance, e.NoiseDiscordance)
	}
	if !e.Readable {
		t.Errorf("a separable effect with a CI excluding 0 is not readable: %v", e.Reasons)
	}
}

// TestCompareSingleShadowBlockIsNotSeparable is the F-32 rule on a REAL
// borderline: one shadow block at rank 1 moves every ranking, but it flips no
// Hit@5 — so the condition moves no more cases than the replicate pair does and
// the effect must not be read, however clean its CI looks.
func TestCompareSingleShadowBlockIsNotSeparable(t *testing.T) {
	c := newCampaign(t, dumpOpts{shadow: true}, dumpOpts{})
	e, ok := effectOn(mustCompare(t, c.input()), goldset.SliceKI)
	if !ok {
		t.Fatalf("no effect row for %s", goldset.SliceKI)
	}
	if e.Discordance != 0 {
		t.Errorf("discordance = %.4f, want 0 — no gold leaves the window", e.Discordance)
	}
	if e.Separable || e.Readable {
		t.Errorf("an effect at the noise floor is declared separable/readable: %+v", e.Reasons)
	}
	if !(e.NDCGCIHi < 0) {
		t.Errorf("the ΔnDCG CI [%.5f, %.5f] does not show the loss the ranking took", e.NDCGCILo, e.NDCGCIHi)
	}
	var named bool
	for _, r := range e.Reasons {
		if strings.Contains(r, "F-32") {
			named = true
		}
	}
	if !named {
		t.Errorf("the reasons do not name the separability rule: %v", e.Reasons)
	}
}

// TestCompareNeutralConditionIsNotSeparable is the other half of the F-32 rule:
// a condition that moves nothing must not come out as a finding.
func TestCompareNeutralConditionIsNotSeparable(t *testing.T) {
	c := newCampaign(t, dumpOpts{}, dumpOpts{})
	body := mustCompare(t, c.input())
	e, ok := effectOn(body, goldset.SliceKI)
	if !ok {
		t.Fatalf("no effect row for %s", goldset.SliceKI)
	}
	if e.DeltaNDCG10 != 0 {
		t.Errorf("ΔnDCG@10 = %.5f over two identical dumps", e.DeltaNDCG10)
	}
	if e.Separable {
		t.Error("a condition that moves no case is declared separable from the noise")
	}
	if e.AboveMDE {
		t.Error("a zero effect is declared above the minimal detectable effect")
	}
}

// --------------------------------------------------------------- gate (g).

// TestCompareMDERisesWithANoisierPair is gate (g): the reported MDE is a
// MEASUREMENT of the replicate pair, so a noisier pair has to raise it — and
// with it the "not resolvable on this slice" verdict of §4.4b.
func TestCompareMDERisesWithANoisierPair(t *testing.T) {
	clean := newCampaign(t, dumpOpts{}, dumpOpts{})
	cleanMDE, ok := mdeOn(mustCompare(t, clean.input()), goldset.SliceKI)
	if !ok {
		t.Fatalf("no MDE row for %s", goldset.SliceKI)
	}
	if cleanMDE.MDE != 0 {
		t.Errorf("MDE = %.5f over an identical replicate pair, want 0", cleanMDE.MDE)
	}
	if !cleanMDE.Resolvable {
		t.Error("a slice with MDE 0 is declared unresolvable")
	}

	// The noised replicate pair moves the gold row between fused rank 1 and
	// rank 3 in OPPOSITE directions on alternating cases: Hit@5 never flips (so
	// G-NOISE stays green and the report stays readable) and the per-case
	// ΔnDCG@10 swings symmetrically, which is exactly the rank churn §4.4b
	// attributes to the non-deterministic ann arm.
	deeperOnEven := dumpOpts{goldDepth: func(i int) int {
		if i%2 == 0 {
			return 3
		}
		return 1
	}}
	deeperOnOdd := dumpOpts{goldDepth: func(i int) int {
		if i%2 == 0 {
			return 1
		}
		return 3
	}}
	noisy := newCampaign(t, dumpOpts{}, deeperOnOdd, deeperOnEven)
	noisyBody := mustCompare(t, noisy.input())
	noisyMDE, ok := mdeOn(noisyBody, goldset.SliceKI)
	if !ok {
		t.Fatalf("no MDE row for %s", goldset.SliceKI)
	}
	if !(noisyMDE.MDE > cleanMDE.MDE) {
		t.Fatalf("MDE did not rise: %.5f (noised) vs %.5f (clean)", noisyMDE.MDE, cleanMDE.MDE)
	}
	if noisyMDE.MDE <= armsweep.MDEThresholdNDCG {
		t.Errorf("MDE %.5f stayed below the %.2f resolution line — the probe is not measuring what it claims",
			noisyMDE.MDE, armsweep.MDEThresholdNDCG)
	}
	if noisyMDE.Resolvable {
		t.Errorf("a slice with MDE %.5f is declared resolvable at threshold %.2f", noisyMDE.MDE, armsweep.MDEThresholdNDCG)
	}
	if noisyMDE.Note == "" {
		t.Error("an unresolvable slice must say so in the report")
	}
}

// --------------------------------------------------------------- gate (h).

// TestCompareRefusesDivergingGUCs is gate (h): the ANN determinism knobs
// (design/05 §4.4b, F-23) must be identical across the campaign. The semantic
// arm carries the highest fusion weight and runs under
// iterative_scan='relaxed_order' with a scan budget (142:216-220) — two dumps
// measured under different budgets differ for a reason no report would show.
func TestCompareRefusesDivergingGUCs(t *testing.T) {
	t.Run("hnsw.ef_search", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{}, dumpOpts{})
		in := c.input()
		in.Cond.Stamp.EfSearch = "200"
		_, err := armsweep.Compare(in)
		if !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare returned %v, want ErrStampIncongruent", err)
		}
		if !strings.Contains(err.Error(), "hnsw.ef_search") {
			t.Errorf("the refusal does not name the GUC: %v", err)
		}
	})

	t.Run("hnsw.max_scan_tuples", func(t *testing.T) {
		c := newCampaign(t, dumpOpts{scanTuples: intp(40000)}, dumpOpts{})
		_, err := armsweep.Compare(c.input())
		if !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare returned %v, want ErrStampIncongruent", err)
		}
		if !strings.Contains(err.Error(), "hnsw.max_scan_tuples") {
			t.Errorf("the refusal does not name the GUC: %v", err)
		}
	})

	t.Run("hnsw.iterative_scan", func(t *testing.T) {
		// The exact path sets no iterative_scan and no scan budget at all
		// (142:216-220), so a case measured in exact mode ran under a different
		// GUC state than the same case measured in ann mode.
		c := newCampaign(t, dumpOpts{mode: "exact"}, dumpOpts{})
		_, err := armsweep.Compare(c.input())
		if !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("Compare returned %v, want ErrStampIncongruent", err)
		}
		if !strings.Contains(err.Error(), "hnsw.iterative_scan") {
			t.Errorf("the refusal does not name the GUC: %v", err)
		}
	})
}

// ------------------------------------------------------- pairing + census.

// TestCompareCountsUnpairedCases pins the pairing rule: a case that is not in
// all four dumps is not comparable and is reported as such instead of being
// paired with whatever record sat at the same offset.
func TestCompareCountsUnpairedCases(t *testing.T) {
	c := newCampaign(t, dumpOpts{}, dumpOpts{})
	// Drop the first two cases from the conditional dump.
	if err := armsweep.WriteRecords(c.cond.Path, c.condRecs[2:]); err != nil {
		t.Fatalf("rewrite cond: %v", err)
	}
	body := mustCompare(t, c.input())
	if body.Paired != labelledN+plainN-2 {
		t.Errorf("paired %d cases, want %d", body.Paired, labelledN+plainN-2)
	}
	if body.UnpairedTotal != 2 {
		t.Errorf("unpaired %d cases, want 2", body.UnpairedTotal)
	}
	if len(body.Unpaired) != 2 {
		t.Errorf("the report names %d unpaired cases, want 2", len(body.Unpaired))
	}
	e, ok := effectOn(body, goldset.SliceKI)
	if !ok || e.N != labelledN-2 {
		t.Errorf("the effect was computed over %d pairs, want %d", e.N, labelledN-2)
	}
}

// TestCompareRefusesUnsortedDumps guards the streaming assumption: the merge
// walks four sorted key streams, and WriteRecords sorts. A hand-written dump
// that does not is refused instead of silently mis-pairing.
func TestCompareRefusesUnsortedDumps(t *testing.T) {
	c := newCampaign(t, dumpOpts{}, dumpOpts{})
	recs := append([]armsweep.Record(nil), c.condRecs...)
	recs[0], recs[1] = recs[1], recs[0]
	if err := writeUnsorted(c.cond.Path, recs); err != nil {
		t.Fatalf("write unsorted: %v", err)
	}
	if _, err := armsweep.Compare(c.input()); !errors.Is(err, armsweep.ErrDumpUnsorted) {
		t.Fatalf("Compare over an unsorted dump returned %v, want ErrDumpUnsorted", err)
	}
}

// writeUnsorted writes records in the given order — the one thing
// armsweep.WriteRecords refuses to do, because it sorts.
func writeUnsorted(path string, recs []armsweep.Record) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for i := range recs {
		if err := enc.Encode(recs[i]); err != nil {
			return err
		}
	}
	return f.Close()
}
