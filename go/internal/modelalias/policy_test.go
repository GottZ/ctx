package modelalias

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// designKeys is design/02 §3.2 as data: every policy key, the alias kinds it
// applies to and its static ceiling — the column "Decke (Code)". 0 = no
// static ceiling (none, or a setting-bound one that its consumer applies:
// max_output_tokens / default_max_tokens under
// gateway.max_output_tokens_ceiling, the non-stream timeout under the
// request deadline, vision_max_bytes also under the request cap,
// embedding.max_inputs × native_dims under gateway.embedding_max_floats).
var designKeys = map[string]struct {
	kinds   string
	ceiling int
}{
	"max_output_tokens":         {"chat", 0},
	"default_max_tokens":        {"chat", 0},
	"max_n":                     {"chat", 8},
	"timeout_s":                 {"chat,embedding,rerank", 3600},
	"reasoning":                 {"chat", 0},
	"vision":                    {"chat", 0},
	"vision_max_bytes":          {"chat", 8 << 20},
	"context_length":            {"chat", 0},
	"tools.max_count":           {"chat", 256},
	"tools.max_bytes":           {"chat", 1 << 20},
	"response_format.max_bytes": {"chat", 256 << 10},
	"logit_bias.max_entries":    {"chat", 1024},
	"stop.max_entries":          {"chat", 16},
	"embedding.native_dims":     {"embedding", 0},
	"embedding.matryoshka_min":  {"embedding", 0},
	"embedding.max_inputs":      {"embedding", 256},
	"embedding.sub_batch":       {"embedding", 256},
	"embedding.max_input_runes": {"embedding", 32768},
	"rerank.max_documents":      {"rerank", 1000},
	"rerank.sub_batch":          {"rerank", 1000},
}

// TestPolicyKeyTablePin: the parser knows exactly the keys of design/02
// §3.2, with their kinds and static ceilings.
func TestPolicyKeyTablePin(t *testing.T) {
	got := map[string]bool{}
	for _, k := range policyKeys {
		got[k.name] = true
		want, ok := designKeys[k.name]
		if !ok {
			t.Errorf("parser knows %q, design/02 §3.2 does not", k.name)
			continue
		}
		var kinds []string
		for _, kind := range []Kind{KindChat, KindEmbedding, KindRerank} {
			if k.kinds.has(kind) {
				kinds = append(kinds, string(kind))
			}
		}
		if strings.Join(kinds, ",") != want.kinds || k.ceiling != want.ceiling {
			t.Errorf("%s: kinds [%s] ceiling %d, want [%s] %d", k.name, strings.Join(kinds, ","), k.ceiling, want.kinds, want.ceiling)
		}
	}
	for name := range designKeys {
		if !got[name] {
			t.Errorf("design/02 §3.2 key %q unknown to the parser", name)
		}
	}
}

func TestParsePolicyAcceptsEveryKey(t *testing.T) {
	chat, err := ParsePolicy(KindChat, []byte(`{"max_output_tokens": 8192, "default_max_tokens": 1024, "max_n": 8,
		"timeout_s": 3600, "reasoning": "passthrough", "vision": true, "vision_max_bytes": 8388608,
		"context_length": 262144, "tools.max_count": 256, "tools.max_bytes": 1048576,
		"response_format.max_bytes": 262144, "logit_bias.max_entries": 1024, "stop.max_entries": 16}`))
	if err != nil {
		t.Fatalf("chat policy at every ceiling: %v", err)
	}
	if chat.MaxOutputTokens != (Opt[int]{8192, true}) || chat.DefaultMaxTokens != (Opt[int]{1024, true}) ||
		chat.MaxN != (Opt[int]{8, true}) || chat.TimeoutS != (Opt[int]{3600, true}) ||
		chat.Reasoning != (Opt[Reasoning]{ReasoningPassthrough, true}) || chat.Vision != (Opt[bool]{true, true}) ||
		chat.VisionMaxBytes != (Opt[int]{8 << 20, true}) || chat.ContextLength != (Opt[int]{262144, true}) ||
		chat.ToolsMaxCount != (Opt[int]{256, true}) || chat.ToolsMaxBytes != (Opt[int]{1 << 20, true}) ||
		chat.ResponseFormatMaxBytes != (Opt[int]{256 << 10, true}) || chat.LogitBiasMaxEntries != (Opt[int]{1024, true}) ||
		chat.StopMaxEntries != (Opt[int]{16, true}) {
		t.Errorf("chat policy parsed to %+v", chat)
	}
	if chat.EmbeddingMaxInputs.Set || chat.RerankMaxDocuments.Set {
		t.Errorf("chat policy sets keys it does not name: %+v", chat)
	}

	emb, err := ParsePolicy(KindEmbedding, []byte(`{"timeout_s": 30, "embedding.native_dims": 4096,
		"embedding.matryoshka_min": 32, "embedding.max_inputs": 256, "embedding.sub_batch": 32,
		"embedding.max_input_runes": 32768}`))
	if err != nil {
		t.Fatalf("embedding policy: %v", err)
	}
	if emb.EmbeddingNativeDims != (Opt[int]{4096, true}) || emb.EmbeddingMatryoshkaMin != (Opt[int]{32, true}) ||
		emb.EmbeddingMaxInputs != (Opt[int]{256, true}) || emb.EmbeddingSubBatch != (Opt[int]{32, true}) ||
		emb.EmbeddingMaxInputRunes != (Opt[int]{32768, true}) || emb.TimeoutS != (Opt[int]{30, true}) {
		t.Errorf("embedding policy parsed to %+v", emb)
	}

	rr, err := ParsePolicy(KindRerank, []byte(`{"rerank.max_documents": 1000, "rerank.sub_batch": 125}`))
	if err != nil {
		t.Fatalf("rerank policy: %v", err)
	}
	if rr.RerankMaxDocuments != (Opt[int]{1000, true}) || rr.RerankSubBatch != (Opt[int]{125, true}) {
		t.Errorf("rerank policy parsed to %+v", rr)
	}

	for _, kind := range []Kind{KindChat, KindEmbedding, KindRerank} {
		if p, err := ParsePolicy(kind, []byte(` { } `)); err != nil || p != (Policy{}) {
			t.Errorf("%s: empty object: %+v %v, want the zero policy (every default)", kind, p, err)
		}
	}
}

// TestParsePolicyFailClosed: every malformed policy is refused with a
// *PolicyError that names the key (or "" for the document).
func TestParsePolicyFailClosed(t *testing.T) {
	cases := []struct {
		kind Kind
		raw  string
		key  string
	}{
		// Not one JSON object.
		{KindChat, ``, ""},
		{KindChat, `null`, ""},
		{KindChat, `[]`, ""},
		{KindChat, `"max_n"`, ""},
		{KindChat, `{`, ""},
		{KindChat, `{"max_n": 1`, ""},
		{KindChat, `{} {}`, ""},
		{KindChat, `{"max_n": 1} x`, ""},
		// Unknown key, duplicate key, key of another kind.
		{KindChat, `{"max_tokens": 5}`, "max_tokens"},
		{KindChat, `{"Max_N": 1}`, "Max_N"},
		{KindChat, `{"max_n": 1, "max_n": 2}`, "max_n"},
		{KindEmbedding, `{"max_n": 1}`, "max_n"},
		{KindChat, `{"embedding.max_inputs": 1}`, "embedding.max_inputs"},
		{KindRerank, `{"embedding.sub_batch": 1}`, "embedding.sub_batch"},
		{Kind("image"), `{}`, ""},
		// Wrong type.
		{KindChat, `{"max_n": "2"}`, "max_n"},
		{KindChat, `{"max_n": 2.5}`, "max_n"},
		{KindChat, `{"max_n": 2.0}`, "max_n"},
		{KindChat, `{"max_n": 1e1}`, "max_n"},
		{KindChat, `{"max_n": true}`, "max_n"},
		{KindChat, `{"max_n": null}`, "max_n"},
		{KindChat, `{"max_n": [1]}`, "max_n"},
		{KindChat, `{"max_n": {"v": 1}}`, "max_n"},
		{KindChat, `{"vision": "true"}`, "vision"},
		{KindChat, `{"vision": 1}`, "vision"},
		{KindChat, `{"vision": null}`, "vision"},
		{KindChat, `{"reasoning": "on"}`, "reasoning"},
		{KindChat, `{"reasoning": "OFF"}`, "reasoning"},
		{KindChat, `{"reasoning": 1}`, "reasoning"},
		// Out of range.
		{KindChat, `{"max_n": 0}`, "max_n"},
		{KindChat, `{"max_n": -1}`, "max_n"},
		{KindChat, `{"context_length": 2147483648}`, "context_length"},
		{KindChat, `{"max_output_tokens": 99999999999999999999}`, "max_output_tokens"},
		// Invariants of design/02 §3.2.
		{KindEmbedding, `{"embedding.max_inputs": 256, "embedding.sub_batch": 16}`, "embedding.sub_batch"},
		{KindEmbedding, `{"embedding.sub_batch": 3}`, "embedding.sub_batch"},
		{KindRerank, `{"rerank.max_documents": 1000, "rerank.sub_batch": 100}`, "rerank.sub_batch"},
		{KindRerank, `{"rerank.sub_batch": 15}`, "rerank.sub_batch"},
		{KindEmbedding, `{"embedding.native_dims": 1024, "embedding.matryoshka_min": 2048}`, "embedding.matryoshka_min"},
	}
	// Every static ceiling + 1.
	for _, k := range policyKeys {
		if k.ceiling == 0 {
			continue
		}
		cases = append(cases, struct {
			kind Kind
			raw  string
			key  string
		}{firstKind(k.kinds), fmt.Sprintf(`{%q: %d}`, k.name, k.ceiling+1), k.name})
	}
	for _, c := range cases {
		p, err := ParsePolicy(c.kind, []byte(c.raw))
		var pe *PolicyError
		if !errors.As(err, &pe) {
			t.Errorf("%s %s: policy %+v err %v, want a *PolicyError", c.kind, c.raw, p, err)
			continue
		}
		if pe.Key != c.key {
			t.Errorf("%s %s: error names key %q (%v), want %q", c.kind, c.raw, pe.Key, err, c.key)
		}
		if p != (Policy{}) {
			t.Errorf("%s %s: a refused policy returned values %+v", c.kind, c.raw, p)
		}
	}
}

// TestParsePolicyCeilingIsInclusive: a value exactly at its static ceiling is
// valid; the invariant ⌈max/sub_batch⌉ ≤ 8 holds at its boundary.
func TestParsePolicyCeilingIsInclusive(t *testing.T) {
	var names []string
	for _, k := range policyKeys {
		if k.ceiling == 0 {
			continue
		}
		raw := fmt.Sprintf(`{%q: %d}`, k.name, k.ceiling)
		switch k.name {
		case "embedding.sub_batch":
			raw = fmt.Sprintf(`{"embedding.max_inputs": %d, "embedding.sub_batch": %d}`, CeilingEmbeddingMaxInputs, k.ceiling)
		case "rerank.sub_batch":
			raw = fmt.Sprintf(`{"rerank.max_documents": %d, "rerank.sub_batch": %d}`, CeilingRerankMaxDocuments, k.ceiling)
		}
		if _, err := ParsePolicy(firstKind(k.kinds), []byte(raw)); err != nil {
			t.Errorf("%s at its ceiling: %v", raw, err)
		}
		names = append(names, k.name)
	}
	sort.Strings(names)
	if len(names) != 13 {
		t.Errorf("%d keys with a static ceiling %v, want the 13 of design/02 §3.2", len(names), names)
	}
	for _, raw := range []string{
		`{"embedding.max_inputs": 256}`,                           // default sub_batch 32 ⇒ 8 blocks
		`{"embedding.sub_batch": 4}`,                              // default max_inputs 32 ⇒ 8 blocks
		`{"rerank.max_documents": 1000, "rerank.sub_batch": 125}`, // 8 blocks
		`{"rerank.sub_batch": 16}`,                                // default max_documents 128 ⇒ 8 blocks
		`{"embedding.native_dims": 1024, "embedding.matryoshka_min": 1024}`,
	} {
		kind := KindEmbedding
		if strings.Contains(raw, "rerank") {
			kind = KindRerank
		}
		if _, err := ParsePolicy(kind, []byte(raw)); err != nil {
			t.Errorf("%s: %v, want valid", raw, err)
		}
	}
}

func firstKind(s kindSet) Kind {
	for _, k := range []Kind{KindChat, KindEmbedding, KindRerank} {
		if s.has(k) {
			return k
		}
	}
	return ""
}
