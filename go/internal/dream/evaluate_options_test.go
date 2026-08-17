package dream

import "testing"

// TestDreamOptions_PinNumPredict pins the dream-eval output cap. Raised from 400
// to 600 (2026-08): the object-map drift form (form 2, qwen3.8-local) emits each
// link as {uuid: {target_id, type, confidence}} at ~2x the array-form cost, so 400
// truncated mid-JSON ("unexpected end of JSON input") and the whole evaluation was
// lost. 600 fits the prompt's "Maximum 5 entries" with margin. This test exists so a
// future tuning pass that drops the cap below the object-map cost is caught at review
// time instead of surfacing as truncated-JSON parse errors in production.
func TestDreamOptions_PinNumPredict(t *testing.T) {
	opts := DreamOptions()
	if opts.NumPredict != 600 {
		t.Fatalf("DreamOptions NumPredict = %d, want 600 (object-map drift cost is ~2x array form)", opts.NumPredict)
	}
}
