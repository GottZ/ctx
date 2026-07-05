// Package llm — vocabulary gate for the D-B derivation (Vorhaben A, W2).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// External test package on purpose: rrf imports llm (rerank.go), so the
// vocabulary truth rrf.DimensionSigma is only reachable from OUTSIDE the llm
// package — the same import-cycle constraint the consumer allowlist
// (handler.collectDimWeights) respects. This test pins that every dimension
// DeriveDimensionWeights can emit exists in rrf.DimensionSigma (plus
// "linear"), so the derivation can never inflate the consumer's boost budget
// with an unknown key (design 01 R5 — the "year" failure class).
//
// Source: https://github.com/GottZ/ctx
package llm_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// TestDeriveDimensionWeights_VocabularySubset drives every derivation rule
// (weekday single/recurring, month, week, quarter, daily) and asserts the
// emitted keys against the canonical vocabulary. Red-proved: adding a bogus
// "year" emission to the derivation fails here.
func TestDeriveDimensionWeights_VocabularySubset(t *testing.T) {
	h := 9
	probes := []struct {
		query string
		dates []llm.TemporalDate
	}{
		{"was war am dienstag", []llm.TemporalDate{{Ref: "p", Date: "2026-06-30", Dir: "past"}}},
		{"immer dienstags", []llm.TemporalDate{{Ref: "p", Date: "2026-06-30", Dir: "past"}}},
		{"entscheidungen im märz", []llm.TemporalDate{{Ref: "p", Date: "2026-03-15", Dir: "past"}}},
		{"wöchentlich rotierender bericht", []llm.TemporalDate{{Ref: "p", Date: "2026-06-29", Dir: "past"}}},
		{"quartalsweise abrechnung", []llm.TemporalDate{{Ref: "p", Date: "2026-06-30", Dir: "past"}}},
		{"daily standup", []llm.TemporalDate{{Ref: "p", Date: "2026-06-30", Dir: "past", Hour: &h}}},
		{"vor zwei wochen", []llm.TemporalDate{{Ref: "p", Date: "2026-06-21", Dir: "past"}}},
	}

	seen := map[string]bool{}
	for _, p := range probes {
		for dim, w := range llm.DeriveDimensionWeights(p.query, p.dates) {
			seen[dim] = true
			if dim == "linear" {
				continue
			}
			if _, ok := rrf.DimensionSigma[dim]; !ok {
				t.Errorf("query %q emits %q (weight %v) — not in rrf.DimensionSigma, would inflate the boost budget", p.query, dim, w)
			}
		}
	}

	// The probe set must actually exercise the cyclic rules — an accidental
	// all-linear probe set would make the subset assert vacuous.
	for _, dim := range []string{"linear", "weekday", "month", "week", "quarter", "daily"} {
		if !seen[dim] {
			t.Errorf("probe set never emitted %q — vocabulary test is not exercising this rule", dim)
		}
	}
}
