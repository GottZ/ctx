// Package handler — activated_dim_weights response contract (A-W1).
// Part of ctx by GottZ and Damien Moon — The memory your LLM pretends to have.
//
// The eval-cyclic dim_weight_pass assert reads queryResponse.ActivatedDimWeights;
// this pins the resolution contract to the gravity-boost semantics (Step 6a):
// missing DimensionWeights means the boost runs pure-linear, and the response
// must say so instead of omitting the field.
//
// Source: https://github.com/GottZ/ctx
package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

func TestActivatedDimWeights(t *testing.T) {
	cases := []struct {
		name string
		tr   *llm.TemporalResult
		want map[string]float64
	}{
		{
			name: "nil result — no temporal treatment, field omitted",
			tr:   nil,
			want: nil,
		},
		{
			name: "empty dates — LLM found nothing, field omitted",
			tr:   &llm.TemporalResult{Query: "no dates"},
			want: nil,
		},
		{
			name: "dates without weights — backward-compat pure linear (query.go Step 6a default)",
			tr: &llm.TemporalResult{
				Dates: []llm.TemporalDate{{Ref: "im März", Date: "2026-03-01", Dir: "past"}},
			},
			want: map[string]float64{"linear": 1.0},
		},
		{
			name: "rules-parser weights pass through untouched",
			tr: &llm.TemporalResult{
				Dates:            []llm.TemporalDate{{Ref: "immer dienstags", Date: "2026-05-05", Dir: "past"}},
				DimensionWeights: map[string]float64{"weekday": 1.0},
			},
			want: map[string]float64{"weekday": 1.0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := activatedDimWeights(tc.tr)
			if len(got) != len(tc.want) {
				t.Fatalf("activatedDimWeights() = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("activatedDimWeights()[%q] = %v, want %v", k, got[k], v)
				}
			}
			if tc.want == nil && got != nil {
				t.Errorf("activatedDimWeights() = %v, want nil (field must be omitted)", got)
			}
		})
	}
}

// TestCollectDimWeights pins the consumer-side vocabulary gate (A-W2,
// design 01 R5): keys outside rrf.DimensionSigma must not enter cyclicDims or
// cyclicWeightSum — a hallucinated/legacy "year" would otherwise inflate the
// boost budget maxBoost*cyclicWeightSum past the ≤0.30 invariant while
// ComputeCyclicGravity contributes 0 for it. Red-proved: removing the
// DimensionSigma check makes the year case report cyclicWeightSum 1.4.
func TestCollectDimWeights(t *testing.T) {
	cases := []struct {
		name       string
		in         map[string]float64
		wantLinear float64
		wantDims   []string
		wantSum    float64
	}{
		{
			name:       "nil map — backward-compat pure linear",
			in:         nil,
			wantLinear: 1.0,
			wantDims:   nil,
			wantSum:    0,
		},
		{
			name:       "mixed linear+weekday (dwLinearWeekday shape)",
			in:         map[string]float64{"linear": 0.6, "weekday": 0.4},
			wantLinear: 0.6,
			wantDims:   []string{"weekday"},
			wantSum:    0.4,
		},
		{
			name:       "unknown year key skipped — budget stays uninflated",
			in:         map[string]float64{"linear": 0.6, "weekday": 0.4, "year": 1.0},
			wantLinear: 0.6,
			wantDims:   []string{"weekday"},
			wantSum:    0.4,
		},
		{
			name:       "zero and negative weights dropped",
			in:         map[string]float64{"linear": 1.0, "month": 0, "week": -0.2},
			wantLinear: 1.0,
			wantDims:   nil,
			wantSum:    0,
		},
		{
			name:       "pure-cyclic map — linear share is Go-zero (rules-parser dwWeekday)",
			in:         map[string]float64{"weekday": 1.0},
			wantLinear: 0,
			wantDims:   []string{"weekday"},
			wantSum:    1.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLinear, gotDims, gotSum := collectDimWeights(tc.in)
			if gotLinear != tc.wantLinear {
				t.Errorf("linearWeight = %v, want %v", gotLinear, tc.wantLinear)
			}
			if gotSum != tc.wantSum {
				t.Errorf("cyclicWeightSum = %v, want %v", gotSum, tc.wantSum)
			}
			if len(gotDims) != len(tc.wantDims) {
				t.Fatalf("cyclicDims = %v, want %v", gotDims, tc.wantDims)
			}
			for i, d := range tc.wantDims {
				if gotDims[i] != d {
					t.Errorf("cyclicDims[%d] = %q, want %q", i, gotDims[i], d)
				}
			}
		})
	}
}

// TestEffectiveDimWeights (issue #463): a window longer than the cycle of a
// cyclic dimension, and every open window, gives that dimension no weight; its
// weight goes to the linear share, so the weights sum as before. A single day
// and a window within the cycle keep the map the rules gave.
func TestEffectiveDimWeights(t *testing.T) {
	end := func(s string) *string { return &s }
	seit := map[string]float64{"linear": 0.8, "month": 0.2}
	vonBis := map[string]float64{"linear": 0.6, "weekday": 0.4}
	cases := []struct {
		name string
		d    llm.TemporalDate
		in   map[string]float64
		want map[string]float64
	}{
		{"single day", llm.TemporalDate{Date: "2024-03-15", Dir: "past"}, vonBis, vonBis},
		{"four days", llm.TemporalDate{Date: "2026-11-27", End: end("2026-11-30"), Dir: "range"}, vonBis, vonBis},
		{"seven days, one cycle", llm.TemporalDate{Date: "2026-11-23", End: end("2026-11-29"), Dir: "range"}, vonBis, vonBis},
		{"eight days", llm.TemporalDate{Date: "2026-11-23", End: end("2026-11-30"), Dir: "range"}, vonBis, map[string]float64{"linear": 1}},
		{"15 months, weekday", llm.TemporalDate{Date: "2024-03-01", End: end("2025-05-31"), Dir: "range"}, vonBis, map[string]float64{"linear": 1}},
		{"seit, 9 months", llm.TemporalDate{Date: "2026-03-01", End: end("2026-11-21"), Dir: "range"}, seit, seit},
		{"one year exactly", llm.TemporalDate{Date: "2024-03-01", End: end("2025-02-28"), Dir: "range"}, seit, seit},
		{"a year and a day", llm.TemporalDate{Date: "2024-03-01", End: end("2025-03-01"), Dir: "range"}, seit, map[string]float64{"linear": 1}},
		{"until", llm.TemporalDate{Date: "2026-03-31", Dir: llm.DirUntil}, seit, map[string]float64{"linear": 1}},
		{"from", llm.TemporalDate{Date: "2026-03-01", Dir: llm.DirFrom}, seit, map[string]float64{"linear": 1}},
		{"letzten Monat", llm.TemporalDate{Date: "2026-10-01", End: end("2026-10-31"), Dir: "range"},
			map[string]float64{"linear": 0.5, "month": 0.5}, map[string]float64{"linear": 0.5, "month": 0.5}},
		{"two months, monthday and month", llm.TemporalDate{Date: "2026-01-01", End: end("2026-02-28"), Dir: "range"},
			map[string]float64{"linear": 0.4, "monthday": 0.3, "month": 0.3}, map[string]float64{"linear": 0.7, "month": 0.3}},
		// The day of the month cycles in a calendar month, not in 31 days:
		// February 1 to March 1 is longer than a month (no producer puts the
		// monthday on a window today; the cycle is pinned all the same).
		{"one month and a day, monthday", llm.TemporalDate{Date: "2026-02-01", End: end("2026-03-01"), Dir: "range"},
			map[string]float64{"linear": 0.5, "monthday": 0.5}, map[string]float64{"linear": 1}},
		{"February, monthday", llm.TemporalDate{Date: "2026-02-01", End: end("2026-02-28"), Dir: "range"},
			map[string]float64{"linear": 0.5, "monthday": 0.5}, map[string]float64{"linear": 0.5, "monthday": 0.5}},
		{"no linear share", llm.TemporalDate{Date: "2026-01-01", Dir: llm.DirUntil},
			map[string]float64{"weekday": 1}, map[string]float64{"linear": 1}},
		{"unknown key stays", llm.TemporalDate{Date: "2026-01-01", Dir: llm.DirUntil},
			map[string]float64{"linear": 0.6, "weekday": 0.4, "year": 1}, map[string]float64{"linear": 1, "year": 1}},
		{"inverted window (LLM)", llm.TemporalDate{Date: "2026-11-21", End: end("2015-03-01"), Dir: "range"}, seit, seit},
		{"unparsable end (LLM)", llm.TemporalDate{Date: "2026-11-21", End: end("soon"), Dir: "range"}, seit, seit},
		{"unparsable start (LLM)", llm.TemporalDate{Date: "2026-03", End: end("2027-03-31"), Dir: "range"}, seit, seit},
	}
	for _, c := range cases {
		got := effectiveDimWeights(&llm.TemporalResult{Dates: []llm.TemporalDate{c.d}, DimensionWeights: c.in})
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if w, ok := got[k]; !ok || w < v-1e-12 || w > v+1e-12 {
				t.Errorf("%s: %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
	// The map the rules gave is never written to.
	if seit["linear"] != 0.8 || seit["month"] != 0.2 || len(seit) != 2 {
		t.Errorf("input changed: %v", seit)
	}
}
