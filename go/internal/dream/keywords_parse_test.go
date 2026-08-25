package dream

import (
	"testing"
)

// TestParseKeywords_ObjectDrift pins the object-drift handling (prod
// 2026-08-25, Ornith 1.5): the model answers the array request with a JSON
// OBJECT. The parser must take the VALUES (the concepts) — not the keys,
// which are often long sentence-fragments — and yield >= MinKeywords.
func TestParseKeywords_ObjectDrift(t *testing.T) {
	raw := `{"CTX-Modellwechsel":"Ornith 1.5 35B NVFP4","GLM 4. Flash":"LiteLLM/DGX","Dream-Fixes PR #39":"CapLocked deterministischer Fallback","Tokenize 12h-Cooldown":"MODELLUNABHÄNGIG","dream.num_predict":"Budget"}`
	kws, err := parseKeywords(raw)
	if err != nil {
		t.Fatalf("parseKeywords object-drift failed: %v", err)
	}
	if len(kws) < MinKeywords {
		t.Fatalf("keywords = %v, want >= %d (values of the object)", kws, MinKeywords)
	}
	// Values must be present; keys (long phrases) must NOT leak as keywords.
	for _, k := range kws {
		if k == "CTX-Modellwechsel" {
			t.Errorf("key leaked as keyword: %q", k)
		}
	}
}

// TestParseKeywords_ArrayStillWorks pins that a normal array answer is
// unaffected by the object-drift path.
func TestParseKeywords_ArrayStillWorks(t *testing.T) {
	raw := `["flash attention","KV cache","prompt eviction"]`
	kws, err := parseKeywords(raw)
	if err != nil {
		t.Fatalf("parseKeywords array failed: %v", err)
	}
	if len(kws) != 3 {
		t.Fatalf("keywords = %v, want 3", kws)
	}
}
