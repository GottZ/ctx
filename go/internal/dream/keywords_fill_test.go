package dream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// fillFixtureBlock is a block whose content yields more than MaxKeywords
// tokenizer terms, so a fill can always reach MaxKeywords.
func fillFixtureBlock() BlockInfo {
	blk := srcBlock("00000000-0000-4000-8000-00000000000b")
	blk.Sensitivity = backends.SensInternal
	blk.Title = "Reverse Proxy Setup"
	blk.Content = "Ein Reverse Proxy ist ein Server der als Vermittler zwischen Clients und Backend-Servern fungiert. " +
		"Er terminiert TLS, verteilt Last und versteckt die Backend-Infrastruktur. Traefik ist unser Reverse Proxy auf INFRA-RP. " +
		"Zertifikate kommen von ACME, die Middleware setzt Header, der Balancer verteilt auf drei Replikas."
	return blk
}

// mockChatSequence installs a chatJSON stub that answers the n-th call with
// responses[n] (the last one repeats) and counts the calls.
func mockChatSequence(t *testing.T, responses ...string) *int {
	t.Helper()
	calls := 0
	mockChatJSON(t, func(ctx context.Context, host, apiKey, model string, think *bool, sys, user string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
		idx := calls
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		calls++
		return constResp(responses[idx])(ctx, host, apiKey, model, think, sys, user, opts, timeout)
	})
	return &calls
}

// TestGenerateKeywords_TooFewOnAllAttempts_FillsAfterRetries pins the PR #42
// behaviour as hardened: a model that stays below MinKeywords on every attempt
// still gets all its retries (too few remains an attempt failure), and once
// they are exhausted the LLM keywords are kept — first, verbatim — and topped
// up to MaxKeywords by the deterministic tokenizer instead of being replaced
// by a tokenizer-only list.
func TestGenerateKeywords_TooFewOnAllAttempts_FillsAfterRetries(t *testing.T) {
	r := newTestRouter()
	calls := mockChatSequence(t, `["Reverse Proxy","Load Balancing"]`)
	blk := fillFixtureBlock()

	kws, err := GenerateKeywords(context.Background(), nil, r, &blk)
	if err != nil {
		t.Fatalf("GenerateKeywords should fill a too-few result, got error: %v", err)
	}
	if *calls != KeywordsMaxRetries {
		t.Fatalf("LLM calls = %d, want %d (too few must still use every retry)", *calls, KeywordsMaxRetries)
	}
	if len(kws) != MaxKeywords {
		t.Fatalf("keywords = %v, want exactly %d (fill tops up to MaxKeywords)", kws, MaxKeywords)
	}
	if kws[0] != "Reverse Proxy" || kws[1] != "Load Balancing" {
		t.Fatalf("LLM keywords must come first and verbatim, got %v", kws)
	}
	assertNoSemanticDuplicates(t, kws)
}

// TestGenerateKeywords_TooFewThenFull_UsesRetry pins that the fill does not
// short-circuit the retry loop: a sparse first answer followed by a full one
// yields the full LLM list, untouched, after exactly two calls.
func TestGenerateKeywords_TooFewThenFull_UsesRetry(t *testing.T) {
	r := newTestRouter()
	calls := mockChatSequence(t,
		`["Reverse Proxy","Load Balancing"]`,
		`["Reverse Proxy","Load Balancing","TLS Termination","Traefik","INFRA-RP","ACME"]`)
	blk := fillFixtureBlock()

	kws, err := GenerateKeywords(context.Background(), nil, r, &blk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("LLM calls = %d, want 2 (retry must get its chance)", *calls)
	}
	want := []string{"Reverse Proxy", "Load Balancing", "TLS Termination", "Traefik", "INFRA-RP", "ACME"}
	if strings.Join(kws, "|") != strings.Join(want, "|") {
		t.Fatalf("keywords = %v, want the full LLM list %v", kws, want)
	}
}

// TestFillKeywords_DedupIsCaseAndTokenAware pins the dedup rules directly:
// the tokenizer emits lower-cased single tokens, so neither a case variant of
// an LLM keyword nor a token of a multi-word LLM keyword may be appended.
func TestFillKeywords_DedupIsCaseAndTokenAware(t *testing.T) {
	llmKws := []string{"Reverse Proxy", " Traefik ", "traefik", ""}
	filled := fillKeywords("Traefik Reverse Proxy Setup",
		"traefik traefik reverse proxy proxy reverse middleware middleware zertifikat balancer entrypoint", llmKws)

	if len(filled) != MaxKeywords {
		t.Fatalf("filled = %v, want exactly %d entries", filled, MaxKeywords)
	}
	if filled[0] != "Reverse Proxy" || filled[1] != "Traefik" {
		t.Fatalf("LLM keywords must lead, trimmed and in order: %v", filled)
	}
	for _, k := range filled[2:] {
		switch strings.ToLower(k) {
		case "traefik", "reverse", "proxy":
			t.Fatalf("tokenizer re-added %q, a case variant or token of an LLM keyword: %v", k, filled)
		}
	}
	assertNoSemanticDuplicates(t, filled)
}

// TestFillKeywords_StaysShortWithoutContent pins the contract the caller
// relies on: when the content has nothing to add, the (normalised) LLM list
// comes back below MinKeywords and the caller falls through to the fallback.
func TestFillKeywords_StaysShortWithoutContent(t *testing.T) {
	filled := fillKeywords("", "", []string{"  Traefik  ", "Traefik"})
	if len(filled) != 1 || filled[0] != "Traefik" {
		t.Fatalf("filled = %v, want the single normalised LLM keyword", filled)
	}
	if len(filled) >= MinKeywords {
		t.Fatalf("filled = %v must stay below MinKeywords=%d so the caller can fall back", filled, MinKeywords)
	}
}

// TestFillKeywords_CapsAtMaxKeywords pins the upper bound: an LLM list that is
// already long enough is neither extended nor allowed past MaxKeywords.
func TestFillKeywords_CapsAtMaxKeywords(t *testing.T) {
	llmKws := []string{"a1", "b2", "c3", "d4", "e5", "f6"}
	filled := fillKeywords("x", "alpha beta gamma delta epsilon", llmKws)
	if len(filled) != MaxKeywords {
		t.Fatalf("filled = %v, want %d entries", filled, MaxKeywords)
	}
	for i := range filled {
		if filled[i] != llmKws[i] {
			t.Fatalf("filled = %v, want the leading LLM keywords in order", filled)
		}
	}
}

// assertNoSemanticDuplicates fails when two keywords are equal ignoring case
// or when one is a token of another (the tokenizer's view of the same term).
func assertNoSemanticDuplicates(t *testing.T, kws []string) {
	t.Helper()
	seen := map[string]int{}
	for i, k := range kws {
		key := strings.ToLower(strings.TrimSpace(k))
		if j, dup := seen[key]; dup {
			t.Fatalf("keyword %d %q duplicates keyword %d: %v", i, k, j, kws)
		}
		seen[key] = i
	}
	for i, k := range kws {
		for _, tok := range tokenize(k) {
			if j, dup := seen[tok]; dup && j != i {
				t.Fatalf("keyword %d %q is a token of keyword %d %q: %v", j, kws[j], i, k, kws)
			}
		}
	}
}
