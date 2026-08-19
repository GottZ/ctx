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
// applyModelParams in llm/chain.go), so a row can still override it at dispatch.
// A cap actually hit at runtime surfaces as metadata cap_hit on the llmlog row.
func TestDreamOptions_NumPredictCoversObjectMapForm(t *testing.T) {
	opts := DreamOptions()
	if opts.NumPredict < minDreamNumPredict {
		t.Fatalf("DreamOptions NumPredict = %d, want >= %d (five links in the object-map drift form cost ~500 tokens pretty-printed)",
			opts.NumPredict, minDreamNumPredict)
	}
}
