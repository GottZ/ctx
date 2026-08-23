// Options.NumPredictScale — the per-attempt output-cap widening dream's
// bounded cap-hit retry uses (issue #26 commit 2). The point of applying it
// inside applyModelParams rather than at the call site is the row override:
// these probes pin that the scale lands on the RESOLVED cap.
package llm

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestApplyModelParams_ScaleBeatsRowOverride is the load-bearing case. A row
// whose model_map pins num_predict overwrites the caller's cap
// unconditionally, so a caller-side doubling would be a no-op on exactly the
// rows that need it — the failure mode this placement exists to prevent.
func TestApplyModelParams_ScaleBeatsRowOverride(t *testing.T) {
	tests := []struct {
		name   string
		base   int
		scale  float64
		params map[string]any
		want   int
	}{
		// The row pins 600, the retry asks for twice the resolved cap.
		{"row-override-scaled", 400, 2, map[string]any{"num_predict": 600}, 1200},
		// The OpenAI spelling of the same param.
		{"row-max-tokens-scaled", 400, 2, map[string]any{"max_tokens": 600}, 1200},
		// No row param: the caller's own cap is scaled.
		{"no-params-scaled", 600, 2, nil, 1200},
		// Off-switch semantics: <= 1 changes nothing, on either path.
		{"scale-one-unchanged", 600, 1, nil, 600},
		{"scale-zero-unchanged", 600, 0, nil, 600},
		{"scale-below-one-unchanged", 600, 0.5, nil, 600},
		{"scale-one-with-row-override", 400, 1, map[string]any{"num_predict": 600}, 600},
		// Fractional factors are honoured, truncated toward zero.
		{"fractional-scale", 600, 1.5, nil, 900},
		// An uncapped call site has nothing to scale.
		{"uncapped-stays-uncapped", 0, 2, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := backends.Backend{Model: "m"}
			got, _ := applyModelParams(
				Options{NumPredict: tt.base, NumPredictScale: tt.scale}, tt.params, &b)
			if got.NumPredict != tt.want {
				t.Errorf("NumPredict = %d, want %d", got.NumPredict, tt.want)
			}
		})
	}
}

// TestApplyModelParams_ScaleLeavesOtherOptionsAlone pins that the scaling is
// surgical: only the output cap moves, every other sampling parameter travels
// untouched.
func TestApplyModelParams_ScaleLeavesOtherOptionsAlone(t *testing.T) {
	b := backends.Backend{Model: "m", NumCtx: 8192}
	in := Options{
		Temperature: 0.7, TopP: 0.8, TopK: 20,
		NumPredict: 600, NumPredictScale: 2,
	}
	got, _ := applyModelParams(in, nil, &b)
	if got.NumPredict != 1200 {
		t.Fatalf("NumPredict = %d, want 1200", got.NumPredict)
	}
	if got.Temperature != 0.7 || got.TopP != 0.8 || got.TopK != 20 {
		t.Errorf("sampling parameters disturbed: %+v", got)
	}
	if got.NumCtx != 8192 {
		t.Errorf("NumCtx = %d, want the row's 8192", got.NumCtx)
	}
	// The factor itself travels on, but it is ctx-internal: json:"-" keeps it
	// off the Ollama options object this struct is marshalled as.
	if got.NumPredictScale != 2 {
		t.Errorf("NumPredictScale = %v, want 2", got.NumPredictScale)
	}
}

// TestResolveMaxOut_SeesScaledCap pins the consistency the placement buys: the
// autowindow output-budget math walks applyModelParams too, so a scaled
// attempt is sized against the cap it will actually send. The extra_body row
// is the documented exception — it outranks Options entirely, which is why
// dream skips the retry on such rows.
func TestResolveMaxOut_SeesScaledCap(t *testing.T) {
	b := backends.Backend{Model: "m"}
	if got, floored := resolveMaxOut(Options{NumPredict: 600, NumPredictScale: 2}, nil, &b); got != 1200 || floored {
		t.Errorf("resolveMaxOut = (%d, %v), want (1200, false)", got, floored)
	}

	pinned := backends.Backend{Model: "m", ExtraBody: map[string]any{"max_tokens": float64(600)}}
	if got, floored := resolveMaxOut(Options{NumPredict: 600, NumPredictScale: 2}, nil, &pinned); got != 600 || floored {
		t.Errorf("resolveMaxOut on an extra_body row = (%d, %v), want (600, false) — extra_body wins on the wire", got, floored)
	}
}
