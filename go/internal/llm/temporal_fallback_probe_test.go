// Package llm — W0/W1 fallback-reachability probes (Vorhaben A, DimensionWeights).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Re-materialized 2026-07-05 (A-W1): the original W0 harness was deleted with
// a stopped build worktree; verdicts below carry over the documented probe set
// from .project/overnight-2026-07-05/dimweights-W0-messung.md. Every probe
// listed there with an explicit verdict is asserted here — this is the durable
// regression net for the fallback-reachable set, not a re-run of the full
// 57-probe characterization sweep.
//
// Verdict semantics (handler/query.go:417/426 dispatch):
//
//	fallback   = NormalizeTemporalRules == nil && HasTemporalIntent == true
//	             → query reaches the LLM fallback (the target set of Vorhaben A)
//	rules-hit  = NormalizeTemporalRules != nil
//	             → deterministic parser wins, fallback never reached
//	no-handling = NormalizeTemporalRules == nil && HasTemporalIntent == false
//	             → no temporal treatment at all (separate HasTemporalIntent
//	               vocabulary gap, documented out of scope for Vorhaben A)
//
// NOTE: external test package (llm_test) on purpose — rrf imports llm
// (rerank.go), so an in-package test importing rrf.DimensionSigma would be an
// import cycle. This is the same constraint the W2 allowlist has to respect.
//
// Source: https://github.com/GottZ/ctx
package llm_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// w0ReferenceDate is the frozen probe date from the W0 measurement
// (Wednesday 2026-05-06) — keeps rules-parser verdicts reproducible.
var w0ReferenceDate = time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

// probeVerdict classifies a query against the fallback dispatch chain.
func probeVerdict(query string, now time.Time) string {
	if llm.NormalizeTemporalRules(query, now) != nil {
		return "rules-hit"
	}
	if llm.HasTemporalIntent(query) {
		return "fallback"
	}
	return "no-handling"
}

// TestW0FallbackProbe asserts the documented W0 verdicts. The FALLBACK set is
// complete (all 10 from the measurement doc); rules-hit and no-handling carry
// the documented examples.
func TestW0FallbackProbe(t *testing.T) {
	probes := []struct {
		query   string
		verdict string
	}{
		// --- FALLBACK: the real target set (complete, W0 doc table) ---
		{"jeden Monat", "fallback"},
		{"im März", "fallback"},
		{"jeden dritten Monat", "fallback"},
		{"in jedem Monat", "fallback"},
		{"jede Woche", "fallback"},
		{"jede zweite Woche", "fallback"},
		{"in jeder Woche", "fallback"},
		{"jeden Monat im Quartal", "fallback"},
		{"jedes Jahr", "fallback"},
		{"jedes Jahr im Sommer", "fallback"},
		// --- rules-hit: weekday phrases are fully caught by the rules parser (R1) ---
		{"immer dienstags", "rules-hit"},
		{"dienstags", "rules-hit"},
		{"every tuesday", "rules-hit"},
		{"am Wochenende", "rules-hit"},
		// --- no-handling: HasTemporalIntent vocabulary gap (out of scope, documented) ---
		{"quartalsweise", "no-handling"},
		{"Q1 Reviews", "no-handling"},
		{"im Sommer", "no-handling"},
		{"im Frühling", "no-handling"},
		{"am Monatsersten", "no-handling"},
		{"jeden 15.", "no-handling"},
		{"täglich", "no-handling"},
		{"wöchentlich", "no-handling"},
		{"monatlich", "no-handling"},
	}

	counts := map[string]int{}
	for _, p := range probes {
		got := probeVerdict(p.query, w0ReferenceDate)
		counts[got]++
		if got != p.verdict {
			t.Errorf("probe %q: verdict = %s, want %s", p.query, got, p.verdict)
		} else {
			t.Logf("probe %q → %s", p.query, got)
		}
	}
	t.Logf("verdict counts: %v", counts)
}

// goldFile locates the eval-cyclic gold corpus. The corpus lives in the
// private .project submodule — broken stub in agent worktrees, so the path
// is overridable via CTX_EVAL_GOLD; missing file skips (not fails).
func goldFile(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CTX_EVAL_GOLD"); p != "" {
		return p
	}
	return filepath.Join("..", "..", "..", ".project", "eval-cyclic-gold.json")
}

// TestW1FallbackGoldCasesReachFallback proves the F-bucket gold cases against
// the real dispatch chain: every bucket-F case must reach the LLM fallback
// (rules parser nil, temporal intent true) — otherwise the case tests the
// rules parser, not the fallback feature, and its rationale is a false claim.
// Also validates expected_dim_weights keys against the canonical cyclic
// vocabulary (rrf.DimensionSigma + "linear"), so a stale label like "year"
// cannot enter the gold corpus unnoticed.
func TestW1FallbackGoldCasesReachFallback(t *testing.T) {
	path := goldFile(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("gold file not readable (%v) — private submodule absent; set CTX_EVAL_GOLD", err)
	}

	var gold struct {
		ReferenceDate string `json:"reference_date"`
		Cases         []struct {
			ID                 string             `json:"id"`
			Bucket             string             `json:"bucket"`
			Query              string             `json:"query"`
			ExpectedDimWeights map[string]float64 `json:"expected_dim_weights"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &gold); err != nil {
		t.Fatalf("gold file %s: invalid JSON: %v", path, err)
	}

	refDate := w0ReferenceDate
	if gold.ReferenceDate != "" {
		d, err := time.Parse("2006-01-02", gold.ReferenceDate)
		if err != nil {
			t.Fatalf("gold reference_date %q: %v", gold.ReferenceDate, err)
		}
		refDate = d.Add(12 * time.Hour) // midday, mirrors probe convention
	}

	nF := 0
	for _, c := range gold.Cases {
		if c.Bucket != "F" {
			continue
		}
		nF++
		if got := probeVerdict(c.Query, refDate); got != "fallback" {
			t.Errorf("gold case %s (%q): verdict = %s, want fallback — rationale claims fallback-reachable", c.ID, c.Query, got)
		}
		if len(c.ExpectedDimWeights) == 0 {
			t.Errorf("gold case %s: bucket F requires expected_dim_weights", c.ID)
		}
		for dim := range c.ExpectedDimWeights {
			if dim == "linear" {
				continue
			}
			if _, ok := rrf.DimensionSigma[dim]; !ok {
				t.Errorf("gold case %s: expected_dim_weights key %q not in canonical cyclic vocabulary (rrf.DimensionSigma)", c.ID, dim)
			}
		}
	}
	if nF == 0 {
		t.Fatal("gold file has no bucket-F cases — W1 gate has nothing to measure")
	}
	t.Logf("verified %d bucket-F gold cases fallback-reachable against %s", nF, path)
}
