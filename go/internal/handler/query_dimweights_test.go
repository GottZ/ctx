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
