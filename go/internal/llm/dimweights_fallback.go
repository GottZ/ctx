// Package llm — deterministic DimensionWeights post-derivation for the LLM
// temporal fallback (Vorhaben A, wave W2, mechanism D-B).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// The LLM fallback (NormalizeTemporal) resolves {dates, query} only — it is
// NEVER asked for weights (design 01 §4 D-B: no Qwen number risk, ethos
// 019d39be). The weights are derived HERE, deterministically, from the same
// detectors the rules parser uses (ruleTokenize / hasRecurrence / findWeekday
// / lookupMonth), so rules path and fallback path speak the same weight
// language for the same query class.
//
// Hard gates from the adversarial review (design 01 §5, R4/R5):
//
//   - Phase coherence: a cyclic dimension is only set when Dates[0] actually
//     carries the matching phase (weekday label ⇒ the resolved date falls on
//     that weekday). The fallback date is LLM-resolved and thus less
//     trustworthy than a rules-parser date — an incoherent phase would make
//     the cyclic boost rank actively WRONG, worse than today.
//   - Never pure-cyclic: every derived set keeps an explicit "linear" share
//     ≥ minLinearFallback. linearWeight is read as dimWeights["linear"]
//     (query.go Step 6a) — a missing key is Go-zero and would switch the
//     linear path OFF entirely.
//   - Closed vocabulary: only dimensions this file emits exist; the set is
//     asserted against rrf.DimensionSigma in dimweights_vocab_test.go
//     (external test package — rrf imports llm, so the in-package code must
//     not import rrf; the emitting rules below ARE the vocabulary source).
//
// "seasonal" is deliberately NOT derived: the rules parser detects seasons
// only inside full matchers (no leaf token detector exists), and a season
// phrase that survives all ~40 matchers carries no token this derivation
// could anchor a phase check on (signal poverty, design 01 N6 — accepted).
// "monatlich"/"jährlich" are likewise skipped: no phase anchor (which day of
// the month/year?) — the phase-coherence gate cannot be satisfied.
//
// Source: https://github.com/GottZ/ctx
// Contributors: https://github.com/GottZ/ctx/graphs/contributors
package llm

import (
	"time"
)

// minLinearFallback is the floor for the linear share of every fallback-derived
// weight set (design 01 §5 gate ii). The rules parser MAY emit pure-cyclic
// (dwWeekday {weekday:1.0}) because its date phase is deterministic; the
// fallback date is LLM-resolved, so a linear fallback share always remains as
// the safety net against a subtly wrong phase.
const minLinearFallback = 0.2

// recurrence keyword → cyclic dimension, for recurrence classes without an
// own token detector. Only classes with a trivially coherent phase are
// mapped: "wöchentlich" (any date is in some ISO week) and "quartalsweise"
// (any date is in some quarter). NOTE quarter has NO rules-parser factory
// (design 01 R3) — 0.5/0.5 is set here as the canonical fallback split, same
// shape as dwLinearMonth.
var recurrenceDimension = map[string]string{
	"wöchentlich":   "week",
	"woechentlich":  "week",
	"weekly":        "week",
	"quartalsweise": "quarter",
	"quarterly":     "quarter",
}

// DeriveDimensionWeights derives the cyclic DimensionWeights for an
// LLM-fallback temporal result from query text + resolved dates (mechanism
// D-B). Deterministic, O(tokens). Always returns a non-nil map with an
// explicit "linear" key; {"linear": 1.0} is the safe default whenever no
// rule fires or the phase-coherence gate rejects.
//
// Weight sets mirror the rules-parser factories where one exists
// (dwLinearWeekday 0.6/0.4, dwLinearMonth 0.5/0.5, dwLinearWeek 0.7/0.3);
// pure-cyclic factories are capped to linear ≥ minLinearFallback. Every set
// sums to 1.0 (query.go Step 6a budget: boostWeight = maxBoost *
// cyclicWeightSum — a sum above 1.0 would inflate the ≤0.30 invariant).
func DeriveDimensionWeights(query string, dates []TemporalDate) map[string]float64 {
	linearOnly := map[string]float64{"linear": 1.0}
	if len(dates) == 0 {
		return linearOnly
	}
	target, err := time.Parse("2006-01-02", dates[0].Date)
	if err != nil {
		// No parseable anchor date ⇒ no phase to be coherent WITH.
		return linearOnly
	}

	tokens := ruleTokenize(query)

	// Rule 1 — weekday token (findWeekday also matches German plural forms
	// and EN plurals, the recurrence shapes). Phase gate: the LLM-resolved
	// date must fall on the detected weekday, else linear-only.
	if wd, _, ok := findWeekday(tokens); ok {
		if target.Weekday() != wd {
			return linearOnly
		}
		if hasRecurrence(tokens) {
			// Recurring weekday ("immer dienstags"): rules factory is pure
			// dwWeekday {1.0} — capped here, never pure-cyclic (gate ii).
			return map[string]float64{"linear": minLinearFallback, "weekday": 1.0 - minLinearFallback}
		}
		return map[string]float64{"linear": 0.6, "weekday": 0.4} // dwLinearWeekday
	}

	// Rule 2 — month name token. Phase gate: resolved date is in that month.
	for _, tok := range tokens {
		if m, ok := lookupMonth(tok); ok {
			if target.Month() != m {
				return linearOnly
			}
			return map[string]float64{"linear": 0.5, "month": 0.5} // dwLinearMonth
		}
	}

	// Rule 3 — recurrence classes with trivially coherent phase (week,
	// quarter): any resolved date carries a valid ISO-week/quarter phase.
	for _, tok := range tokens {
		switch recurrenceDimension[tok] {
		case "week":
			return map[string]float64{"linear": 0.7, "week": 0.3} // dwLinearWeek
		case "quarter":
			return map[string]float64{"linear": 0.5, "quarter": 0.5} // no factory (R3), canonical fallback split
		}
	}

	// Rule 4 — daily ("täglich"/"daily"): the daily phase is hour-of-day, so
	// it is only coherent when the resolved date carries an Hour (time-of-day
	// resolution); a bare date anchors midnight and would boost wrongly.
	if dates[0].Hour != nil {
		for _, tok := range tokens {
			if tok == "täglich" || tok == "taeglich" || tok == "daily" {
				// Rules factory is pure dwDaily {1.0} — capped (gate ii).
				return map[string]float64{"linear": minLinearFallback, "daily": 1.0 - minLinearFallback}
			}
		}
	}

	return linearOnly
}
