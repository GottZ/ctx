package dream

import "testing"

// minDreamNumPredict is the floor the dream-eval output cap must not fall below:
// the measured worst-case answer is five links in the object-map drift form
// (form 2, qwen3.8-local), pretty-printed, at ~500 tokens on the Qwen3 tokenizer
// — ~420 compact, against ~250/330 for the array form the prompt asks for — plus
// the ~100 tokens of margin the current default carries. See DreamOptions.
const minDreamNumPredict = 600

// TestDreamOptions_NumPredictCoversObjectMapForm guards the COMPILE-TIME default
// against a DROP below the measured object-map cost: at 400 the pretty object-map
// form truncated mid-JSON ("unexpected end of JSON input") and the whole
// evaluation was lost. The assertion is a LOWER BOUND, not a pin on the exact
// value — raising the cap is a tuning decision that should not have to edit a
// test; lowering it past the measured cost is the regression worth catching.
//
// It does NOT guard production: the effective cap is this default merged with the
// serving backend row's model_map params (num_predict / max_tokens, merged by
// applyModelParams in llm/chain.go), so a row can still override it at dispatch —
// and since dream.num_predict the scheduler can move it too (DreamOptionsFor).
// This test deliberately keeps its own 600 literal instead of asserting against
// DefaultNumPredict: the point is a measured floor an edit must justify, and a
// comparison of the constant with itself would assert nothing. The config side is
// pinned to the same constant by TestDefaultNumPredictMatchesRegistry.
// A cap actually hit at runtime surfaces as metadata cap_hit on the llmlog row.
func TestDreamOptions_NumPredictCoversObjectMapForm(t *testing.T) {
	opts := DreamOptions()
	if opts.NumPredict < minDreamNumPredict {
		t.Fatalf("DreamOptions NumPredict = %d, want >= %d (five links in the object-map drift form cost ~500 tokens pretty-printed)",
			opts.NumPredict, minDreamNumPredict)
	}
}

// TestDreamOptionsFor pins the sentinel contract dream.num_predict is documented
// with — 0 and (defensively) any non-positive value mean "package default", a
// positive value wins — and that the resolver touches nothing else in the
// options. Mutation killed: return DreamOptions() unconditionally, or drop the
// `numPredict > 0` branch, and the 900 case goes red.
func TestDreamOptionsFor(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"sentinel zero is the package default", 0, DefaultNumPredict},
		{"negative is the package default too (V18 rejects it upstream)", -1, DefaultNumPredict},
		{"configured value wins", 900, 900},
		{"a value below the default still wins (V18b warns, never clamps)", 300, 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := DreamOptionsFor(c.in)
			if opts.NumPredict != c.want {
				t.Errorf("DreamOptionsFor(%d).NumPredict = %d, want %d", c.in, opts.NumPredict, c.want)
			}
			// The sampling axes are not the cap's business.
			base := DreamOptions()
			if opts.Temperature != base.Temperature || opts.TopP != base.TopP || opts.TopK != base.TopK {
				t.Errorf("DreamOptionsFor(%d) changed the sampling tuple: %+v, want %+v beside NumPredict", c.in, opts, base)
			}
		})
	}
}
