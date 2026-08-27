package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// The CLI half of wave M-W3d: the flags, the artefact names and — above all —
// the EXIT CODES, because that is the whole interface a campaign driver has to
// the gates. A refusal that comes back as exit 1 is indistinguishable from a
// crash to whatever runs the campaign.

const cliCases = 8

// cliCampaign writes four congruent dumps into the gold directory's dumps sink
// and returns the gold root.
func cliCampaign(t *testing.T) string {
	t.Helper()
	gold := tinyGold(t)
	dumps := filepath.Join(gold, armsweep.DumpDirName)
	if err := os.MkdirAll(dumps, 0o700); err != nil {
		t.Fatalf("mkdir dumps: %v", err)
	}
	for _, run := range []string{"BASE", "COND", "V0", "V0P"} {
		recs := cliRecords(run == "COND")
		if err := armsweep.WriteRecords(filepath.Join(dumps, run+".jsonl"), recs); err != nil {
			t.Fatalf("write dump %s: %v", run, err)
		}
		if err := armsweep.WriteJSONFile(filepath.Join(dumps, run+".stamp.json"), cliStamp(run, recs)); err != nil {
			t.Fatalf("write stamp %s: %v", run, err)
		}
	}
	return gold
}

// cliRecords is a minimal measured case set: two candidates per query, the gold
// one first. In the conditional dump the two swap, which moves every metric.
func cliRecords(swapped bool) []armsweep.Record {
	scan := 60000
	out := make([]armsweep.Record, 0, cliCases)
	for i := 0; i < cliCases; i++ {
		idA, idB := fmt.Sprintf("a-%02d", i), fmt.Sprintf("b-%02d", i)
		rankA, rankB := 1, 2
		if swapped {
			rankA, rankB = 2, 1
		}
		out = append(out, armsweep.Record{
			Slice: goldset.SliceKI, Index: i,
			QuerySHA256: goldset.SHA256Hex(fmt.Sprintf("case/%d", i)),
			GoldIDs:     []string{idA},
			Rows: []rrf.ArmRow{
				{ID: idA, RankSemantic: &rankA, MassFactor: 1, TypeFactor: 1, TypeName: "knowledge"},
				{ID: idB, RankSemantic: &rankB, MassFactor: 1, TypeFactor: 1, TypeName: "knowledge"},
			},
			Selector: armsweep.Selector{Mode: "ann", Reason: "grey", ScanTuples: &scan},
			Attempts: 1,
		})
	}
	return out
}

func cliStamp(run string, recs []armsweep.Record) armsweep.DumpStamp {
	return armsweep.DumpStamp{
		RunID: run, CreatedAt: "2026-08-27T00:00:00Z", BaseURL: "http://ctx",
		Records: len(recs), DumpFile: "dumps/" + run + ".jsonl",
		PinFile: "pins-CAMP.jsonl", PinRunID: "CAMP", PinSHA256: goldset.SHA256Hex("pins"),
		GoldStamp: goldset.SHA256Hex("stamp"), MigrationsMax: armsweep.TypeNameMigration,
		PostFusionStages: map[string]any{"cluster.enabled": false, "cluster.inject_max": float64(0),
			"graph.enabled": false, "rerank.enabled": false},
		SliceFiles: []armsweep.SliceDigest{
			{Slice: goldset.SliceKI, File: goldset.FileKI, SHA256: goldset.SHA256Hex("ki"), N: len(recs)},
		},
		InstanceKind: armsweep.InstanceKindMeasureCopy,
		EfSearch:     "40 (default)",
	}
}

// TestCompareCLIWritesBothReports is the wiring gate: the subcommand produces a
// JSON and a markdown artefact under `reports/`, and two runs over the same
// dumps produce the same body (gate (d) through the CLI).
func TestCompareCLIWritesBothReports(t *testing.T) {
	gold := cliCampaign(t)
	c := dryCommon(gold)
	reports := filepath.Join(t.TempDir(), armsweep.ReportDirName)

	if err := cmdCompare(c, "BASE.jsonl", "COND.jsonl", "V0.jsonl,V0P.jsonl", reports, "one"); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if err := cmdCompare(c, "BASE.jsonl", "COND.jsonl", "V0.jsonl,V0P.jsonl", reports, "two"); err != nil {
		t.Fatalf("compare (second run): %v", err)
	}
	for _, name := range []string{"one.json", "one.md", "two.json", "two.md"} {
		if _, err := os.Stat(filepath.Join(reports, name)); err != nil {
			t.Errorf("missing artefact %s: %v", name, err)
		}
	}
	b1, err := armsweep.ReadReportBody(filepath.Join(reports, "one.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b2, err := armsweep.ReadReportBody(filepath.Join(reports, "two.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if goldset.SHA256Hex(string(b1)) != goldset.SHA256Hex(string(b2)) {
		t.Errorf("two CLI runs produced different bodies: %s vs %s",
			goldset.SHA256Hex(string(b1)), goldset.SHA256Hex(string(b2)))
	}
}

// TestCompareCLIExitCodes walks the three refusals a campaign driver has to be
// able to tell apart.
func TestCompareCLIExitCodes(t *testing.T) {
	gold := cliCampaign(t)
	reports := filepath.Join(t.TempDir(), armsweep.ReportDirName)

	t.Run("gate (a): no noise pair", func(t *testing.T) {
		err := cmdCompare(dryCommon(gold), "BASE.jsonl", "COND.jsonl", "", reports, "a")
		if !errors.Is(err, armsweep.ErrGateRefused) {
			t.Fatalf("compare without -noise-pair returned %v, want ErrGateRefused", err)
		}
		if got := exitCodeFor(err); got != 3 {
			t.Errorf("exit code = %d, want 3", got)
		}
	})

	t.Run("gate (a): one dump is not a pair", func(t *testing.T) {
		err := cmdCompare(dryCommon(gold), "BASE.jsonl", "COND.jsonl", "V0.jsonl", reports, "a1")
		if got := exitCodeFor(err); got != 3 {
			t.Errorf("exit code = %d for a single noise dump, want 3 (%v)", got, err)
		}
	})

	t.Run("gate (c): incongruent stamp", func(t *testing.T) {
		dumps := filepath.Join(gold, armsweep.DumpDirName)
		var stamp armsweep.DumpStamp
		if err := armsweep.ReadJSONFile(filepath.Join(dumps, "COND.stamp.json"), &stamp); err != nil {
			t.Fatalf("read stamp: %v", err)
		}
		stamp.MigrationsMax = armsweep.TypeNameMigration + 1
		if err := armsweep.WriteJSONFile(filepath.Join(dumps, "COND.stamp.json"), stamp); err != nil {
			t.Fatalf("write stamp: %v", err)
		}
		err := cmdCompare(dryCommon(gold), "BASE.jsonl", "COND.jsonl", "V0.jsonl,V0P.jsonl", reports, "c")
		if !errors.Is(err, armsweep.ErrStampIncongruent) {
			t.Fatalf("compare over an incongruent stamp returned %v, want ErrStampIncongruent", err)
		}
		if got := exitCodeFor(err); got != 4 {
			t.Errorf("exit code = %d, want 4 (dump discarded, not a retryable gate refusal)", got)
		}
		if _, err := os.Stat(filepath.Join(reports, "c.json")); err == nil {
			t.Error("an incongruent dump set produced a report — there was nothing measured to report")
		}
	})
}

// TestCompareCLIRefusedNoiseStillWritesTheReport pins the artefact contract of
// gate (b): the comparison refuses AND leaves the evidence behind, the way an
// aborted dump keeps its file.
func TestCompareCLIRefusedNoiseStillWritesTheReport(t *testing.T) {
	gold := cliCampaign(t)
	dumps := filepath.Join(gold, armsweep.DumpDirName)
	// Make the second replicate disagree with the first on every case: 100 %
	// discordance, twenty times the G-NOISE ceiling.
	if err := armsweep.WriteRecords(filepath.Join(dumps, "V0P.jsonl"), cliRecords(true)); err != nil {
		t.Fatalf("rewrite replicate: %v", err)
	}
	reports := filepath.Join(t.TempDir(), armsweep.ReportDirName)
	err := cmdCompare(dryCommon(gold), "BASE.jsonl", "COND.jsonl", "V0.jsonl,V0P.jsonl", reports, "b")
	if !errors.Is(err, armsweep.ErrGateRefused) {
		t.Fatalf("compare over a red noise floor returned %v, want ErrGateRefused", err)
	}
	if got := exitCodeFor(err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	if _, err := os.Stat(filepath.Join(reports, "b.json")); err != nil {
		t.Errorf("the refused comparison wrote no report: %v", err)
	}
}

// TestExitCodeCascade pins the whole cascade in one place — the contract a
// campaign scheduler reads.
func TestExitCodeCascade(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"clean", nil, 0},
		{"outside the gold roots", fmt.Errorf("x: %w", goldset.ErrOutsideGoldset), 2},
		{"gate refused", fmt.Errorf("x: %w", armsweep.ErrGateRefused), 3},
		{"drift aborted the dump", fmt.Errorf("x: %w", errDumpAborted), 4},
		{"dump predates type_name", fmt.Errorf("x: %w", armsweep.ErrDumpPredatesTypeName), 4},
		{"stamps incongruent", fmt.Errorf("x: %w", armsweep.ErrStampIncongruent), 4},
		{"not a measure copy", fmt.Errorf("x: %w", armsweep.ErrNotMeasureCopy), 5},
		{"anything else", errors.New("boom"), 1},
	}
	for _, tc := range cases {
		if got := exitCodeFor(tc.err); got != tc.want {
			t.Errorf("%s: exit code = %d, want %d", tc.name, got, tc.want)
		}
	}
}
