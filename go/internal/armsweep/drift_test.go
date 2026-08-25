package armsweep_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

const (
	t0 = "2026-08-26T10:00:00Z"
	t1 = "2026-08-26T10:12:00Z"
	// drawnAt stands in for STAMP.json's corpus_max_created_at.
	drawnAt = "2026-08-25T13:49:49.736510Z"
)

// census builds a stamp with one retrievable and one excluded type.
func census(at string, retrievable, nullEmb int, gold ...armsweep.GoldIDState) armsweep.DriftStamp {
	return armsweep.DriftStamp{
		At:                at,
		RetrievableBlocks: retrievable,
		Types: []armsweep.TypeDrift{
			{TypeName: "knowledge", Retrievable: true, Count: retrievable,
				MaxCreatedAt: drawnAt, MaxUpdatedAt: drawnAt, NullEmbedding: nullEmb},
			// checkpoint is retrieval=excluded and carries thousands of null
			// embeddings by standing policy — the rule must never see it.
			{TypeName: "checkpoint", Retrievable: false, Count: 5352, NullEmbedding: 5352},
		},
		GoldIDs: gold,
	}
}

func goldState(id, created, updated string) armsweep.GoldIDState {
	return armsweep.GoldIDState{ID: id, CreatedAt: created, UpdatedAt: updated}
}

// TestDriftCleanRunPasses is the control: nothing moved, nothing aborts.
func TestDriftCleanRunPasses(t *testing.T) {
	g := goldState("b1", drawnAt, drawnAt)
	v := armsweep.EvaluateDrift(census(t0, 1375, 0, g), census(t1, 1375, 0, g), drawnAt)
	if v.Abort {
		t.Errorf("clean run aborted: %v", v.Reasons)
	}
}

// TestDriftGoldMutationAborts covers the two gold-label rules of §4.7: a label
// rewritten during the run, and a label that disappeared.
func TestDriftGoldMutationAborts(t *testing.T) {
	cases := []struct {
		name   string
		before armsweep.DriftStamp
		after  armsweep.DriftStamp
		want   string
	}{
		{
			"gold block updated inside the measurement window",
			census(t0, 1375, 0, goldState("b1", drawnAt, drawnAt)),
			census(t1, 1375, 0, goldState("b1", drawnAt, "2026-08-26T10:05:00Z")),
			"was updated during the run",
		},
		{
			"gold block deleted during the run",
			census(t0, 1375, 0, goldState("b1", drawnAt, drawnAt)),
			census(t1, 1375, 0),
			"disappeared during the run",
		},
		{
			"gold block absent before the run",
			census(t0, 1375, 0),
			census(t1, 1375, 0, goldState("b1", drawnAt, drawnAt)),
			"was absent from the corpus before the run",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := armsweep.EvaluateDrift(tc.before, tc.after, drawnAt)
			if !v.Abort {
				t.Fatalf("no abort; reasons %v", v.Reasons)
			}
			if !strings.Contains(strings.Join(v.Reasons, " | "), tc.want) {
				t.Errorf("reasons %v do not mention %q", v.Reasons, tc.want)
			}
		})
	}
}

// TestDriftContaminationProbe is §5.3 b: a labelled block created AFTER the
// gold set was drawn cannot have been a label at draw time.
func TestDriftContaminationProbe(t *testing.T) {
	fresh := goldState("b1", "2026-08-26T09:00:00Z", "2026-08-26T09:00:00Z")
	v := armsweep.EvaluateDrift(census(t0, 1375, 0, fresh), census(t1, 1375, 0, fresh), drawnAt)
	if !v.Abort {
		t.Fatalf("contamination not caught; reasons %v", v.Reasons)
	}
	if !strings.Contains(strings.Join(v.Reasons, " | "), "contamination") {
		t.Errorf("reasons %v do not name the contamination rule", v.Reasons)
	}
}

// TestDriftNullEmbeddingRuleIgnoresExcludedTypes pins the 0 → >0 rule and the
// exemption that keeps it usable: the live corpus holds thousands of null
// embeddings in retrieval=excluded types as policy, and a rule counting those
// would abort every single run.
func TestDriftNullEmbeddingRuleIgnoresExcludedTypes(t *testing.T) {
	g := goldState("b1", drawnAt, drawnAt)

	v := armsweep.EvaluateDrift(census(t0, 1375, 0, g), census(t1, 1375, 3, g), drawnAt)
	if !v.Abort {
		t.Errorf("retrievable type gained null embeddings but the run did not abort: %v", v.Reasons)
	}

	// The excluded type is constant at 5352 in both censuses — proof that the
	// rule is not simply summing every null embedding it can see.
	clean := armsweep.EvaluateDrift(census(t0, 1375, 0, g), census(t1, 1375, 0, g), drawnAt)
	if clean.Abort {
		t.Errorf("excluded-type null embeddings triggered an abort: %v", clean.Reasons)
	}

	// Already-nonzero before ⇒ not a 0 → >0 transition, so not this rule's case.
	stable := armsweep.EvaluateDrift(census(t0, 1375, 4, g), census(t1, 1375, 9, g), drawnAt)
	if stable.Abort {
		t.Errorf("a nonzero-to-nonzero move is not the 0 → >0 rule: %v", stable.Reasons)
	}
}

// TestDriftRetrievableTolerance pins the ±0.5 % band.
func TestDriftRetrievableTolerance(t *testing.T) {
	g := goldState("b1", drawnAt, drawnAt)
	cases := []struct {
		name      string
		after     int
		wantAbort bool
	}{
		{"no movement", 1375, false},
		{"+0.36 % is inside the band", 1380, false},
		{"-0.36 % is inside the band", 1370, false},
		{"+0.73 % is outside", 1385, true},
		{"-0.73 % is outside", 1365, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := armsweep.EvaluateDrift(census(t0, 1375, 0, g), census(t1, tc.after, 0, g), drawnAt)
			if v.Abort != tc.wantAbort {
				t.Errorf("abort = %v (want %v); reasons %v notes %v", v.Abort, tc.wantAbort, v.Reasons, v.Notes)
			}
			if !tc.wantAbort && tc.after != 1375 && len(v.Notes) == 0 {
				t.Error("movement inside the band must still be NOTED, not swallowed")
			}
		})
	}
}

// TestPinRoundtrip pins the pin file contract, including the empty temporal pin
// — which is a VALUE ("explicitly no expansion"), not an omission.
func TestPinRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pins-x.jsonl")
	in := []armsweep.Pin{
		{Slice: goldset.SliceQ, Index: 7, QuerySHA256: goldset.SHA256Hex("b"), Translation: "database status", Temporal: "", EmbedModel: "qwen3-embedding"},
		{Slice: goldset.SliceKI, Index: 2, QuerySHA256: goldset.SHA256Hex("a"), Translation: "retention policy", Temporal: "2026 august"},
	}
	if err := armsweep.WritePins(p, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pin file mode = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
	got, err := armsweep.ReadPins(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d pins, want 2", len(got))
	}
	q := got[armsweep.CaseKey(goldset.SliceQ, 7, goldset.SHA256Hex("b"))]
	if q.Translation != "database status" || q.Temporal != "" {
		t.Errorf("pin roundtrip lost a value: %+v", q)
	}
	if k := got[armsweep.CaseKey(goldset.SliceKI, 2, goldset.SHA256Hex("a"))]; k.Temporal != "2026 august" {
		t.Errorf("temporal pin lost: %+v", k)
	}

	// Two priming runs concatenated must be refused, not silently halved.
	dup := filepath.Join(dir, "pins-dup.jsonl")
	if err := armsweep.WritePins(dup, append(in, in[0])); err != nil {
		t.Fatalf("write dup: %v", err)
	}
	if _, err := armsweep.ReadPins(dup); err == nil || !strings.Contains(err.Error(), "duplicate pin") {
		t.Errorf("duplicate pin accepted: %v", err)
	}
}

// TestPathGuardConfinesTheDumpSink is gate (e): a write outside the guarded
// root fails without the override and succeeds with it.
func TestPathGuardConfinesTheDumpSink(t *testing.T) {
	root := filepath.Join(t.TempDir(), armsweep.DumpDirName)
	g, err := goldset.NewNamedGuard(root, armsweep.DumpDirName, false)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if _, err := g.Resolve("run.jsonl"); err != nil {
		t.Errorf("a name inside the root was refused: %v", err)
	}
	for _, escape := range []string{"../outside.jsonl", "../../etc/passwd", "/tmp/armsweep.jsonl"} {
		if _, err := g.Resolve(escape); !errors.Is(err, goldset.ErrOutsideGoldset) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideGoldset", escape, err)
		}
	}

	// A root with the wrong basename is a typo, not a second dump directory.
	if _, err := goldset.NewNamedGuard(filepath.Join(t.TempDir(), "dmups"), armsweep.DumpDirName, false); !errors.Is(err, goldset.ErrOutsideGoldset) {
		t.Errorf("misnamed root accepted: %v", err)
	}

	// With the override both are allowed — and the caller must declare it.
	over, err := goldset.NewNamedGuard(filepath.Join(t.TempDir(), "anywhere"), armsweep.DumpDirName, true)
	if err != nil {
		t.Fatalf("override guard: %v", err)
	}
	if !over.AllowOutside() {
		t.Error("guard does not report the override — the report could not declare it")
	}
	if _, err := over.Resolve("/tmp/armsweep.jsonl"); err != nil {
		t.Errorf("override still refused an outside path: %v", err)
	}
}

// TestLatencySummary pins the p50/p95 profile gate (a) is reported through.
func TestLatencySummary(t *testing.T) {
	ms := make([]int64, 100)
	for i := range ms {
		ms[i] = int64(i + 1)
	}
	l := armsweep.SummariseLatency(ms)
	if l.N != 100 || l.P50 != 50 || l.P95 != 95 || l.Max != 100 {
		t.Errorf("latency = %+v, want n=100 p50=50 p95=95 max=100", l)
	}
	if e := armsweep.SummariseLatency(nil); e.N != 0 {
		t.Errorf("empty latency = %+v", e)
	}
}

// TestRecordRoundtrip pins the dump file contract, including the key-sorted
// order that makes a dump reproducible under concurrency.
func TestRecordRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "run.jsonl")
	recs := synthDump(t, 3)
	if err := armsweep.WriteRecords(p, recs); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := armsweep.ReadRecords(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("read %d records, want %d", len(got), len(recs))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Key() >= got[i].Key() {
			t.Fatalf("dump is not key-sorted at %d: %q then %q", i, got[i-1].Key(), got[i].Key())
		}
	}
	// Writing a shuffled copy must produce the same bytes.
	shuffled := append([]armsweep.Record(nil), recs[len(recs)/2:]...)
	shuffled = append(shuffled, recs[:len(recs)/2]...)
	q := filepath.Join(dir, "run2.jsonl")
	if err := armsweep.WriteRecords(q, shuffled); err != nil {
		t.Fatalf("write shuffled: %v", err)
	}
	a, _ := os.ReadFile(p)
	b, _ := os.ReadFile(q)
	if string(a) != string(b) {
		t.Error("dump bytes depend on the order records were collected in")
	}
}
