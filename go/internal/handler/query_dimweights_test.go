// Package handler — activated_dim_weights response contract (A-W1).
// Part of ctx by GottZ — The memory your LLM pretends to have.
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
