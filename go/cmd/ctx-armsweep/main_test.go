package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// tinyGold writes a two-case gold directory. Synthetic on purpose — the real
// one is private, and a CLI wiring test has no business reading it.
func tinyGold(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), goldset.DirName)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	p, err := g.Resolve(goldset.FileKI)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := goldset.WriteJSONL(p, []goldset.Case{
		{Slice: goldset.SliceKI, Query: "erste frage", Origin: "test", GoldIDs: []string{"id-a"}},
		{Slice: goldset.SliceKI, Query: "zweite frage", Origin: "test", GoldIDs: []string{"id-b"}},
	}); err != nil {
		t.Fatalf("write slice: %v", err)
	}
	sp, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		t.Fatalf("resolve stamp: %v", err)
	}
	if err := goldset.WriteStamp(sp, goldset.Stamp{Version: 1, SampleSeed: 1, SplitSeed: 2,
		CorpusMaxCreatedAt: "2026-08-25T13:49:49Z", Slices: map[string]goldset.SliceStamp{}}); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	return dir
}

func dryCommon(dir string) *common {
	return &common{
		dir: dir, dryRun: true, quiet: true, seed: 20260812,
		concurrency: 1, timeout: 5, slices: goldset.SliceKI,
		retries: armsweep.DefaultRetries, runID: "testrun",
	}
}

// TestDryRunPipeline walks prime → dump → score without an instance: it pins
// the artefact NAMES and LOCATIONS, which is the part of the tool an operator
// touches and the part a refactor breaks silently.
func TestDryRunPipeline(t *testing.T) {
	dir := tinyGold(t)
	c := dryCommon(dir)

	if err := cmdPrime(context.Background(), c); err != nil {
		t.Fatalf("prime: %v", err)
	}
	for _, name := range []string{"pins-testrun.jsonl", "prime-testrun.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("prime did not write %s: %v", name, err)
		}
	}

	if err := cmdDump(context.Background(), c, "pins-testrun.jsonl"); err != nil {
		t.Fatalf("dump: %v", err)
	}
	dumpPath := filepath.Join(dir, armsweep.DumpDirName, "testrun.jsonl")
	if _, err := os.Stat(dumpPath); err != nil {
		t.Fatalf("dump file missing: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, armsweep.DumpDirName)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dumps directory mode = %v (err %v), want 0700", fi.Mode().Perm(), err)
	}

	reports := filepath.Join(t.TempDir(), armsweep.ReportDirName)
	if err := cmdScore(c, "testrun.jsonl", "", reports, "r", ""); err != nil {
		t.Fatalf("score: %v", err)
	}
	body, err := armsweep.ReadReportBody(filepath.Join(reports, "r.json"))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var parsed armsweep.ReportBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if parsed.Interpretable {
		t.Error("a single-dump report claims interpretability")
	}
	if parsed.Env.AllowOutsideGoldset {
		t.Error("the report declares an override that was never set")
	}
	if _, err := os.Stat(filepath.Join(reports, "r.md")); err != nil {
		t.Errorf("markdown report missing: %v", err)
	}
}

// TestReportGuardRefusesAForeignDirectory is the CLI half of gate (e): the
// report sink must be a `reports/` directory unless the override says otherwise,
// and the override has to reach the report.
func TestReportGuardRefusesAForeignDirectory(t *testing.T) {
	dir := tinyGold(t)
	c := dryCommon(dir)
	if err := cmdPrime(context.Background(), c); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if err := cmdDump(context.Background(), c, "pins-testrun.jsonl"); err != nil {
		t.Fatalf("dump: %v", err)
	}

	foreign := filepath.Join(t.TempDir(), "somewhere-else")
	err := cmdScore(c, "testrun.jsonl", "", foreign, "r", "")
	if !errors.Is(err, goldset.ErrOutsideGoldset) {
		t.Fatalf("score into %q returned %v, want ErrOutsideGoldset", foreign, err)
	}

	over := dryCommon(dir)
	over.allowOutside = true
	if err := cmdScore(over, "testrun.jsonl", "", foreign, "r", ""); err != nil {
		t.Fatalf("score with the override: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "r.json")); err != nil {
		t.Errorf("override did not produce a report: %v", err)
	}
}

// TestLatestPinFilePicksTheNewestRun pins the convenience default of `dump`.
func TestLatestPinFilePicksTheNewestRun(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"pins-20260101T000000Z.jsonl", "pins-20260826T120000Z.jsonl", "prime-x.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	got, err := latestPinFile(dir)
	if err != nil {
		t.Fatalf("latestPinFile: %v", err)
	}
	if got != "pins-20260826T120000Z.jsonl" {
		t.Errorf("latestPinFile = %q, want the newest run id", got)
	}
	if _, err := latestPinFile(t.TempDir()); err == nil || !strings.Contains(err.Error(), "prime") {
		t.Errorf("an empty directory must point at `prime`, got %v", err)
	}
}
