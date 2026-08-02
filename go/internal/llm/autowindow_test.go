// E10-W2 AUTO-window unit probes: eligibility arithmetic, the token estimate's
// DIRECTION, and the extra_body merge rule. The wire-level counterparts live in
// synthesize_autowindow_test.go — these cases pin the arithmetic that would
// otherwise only be observable through a prompt-size coincidence.
package llm

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/promptguard"
)

// liveMix is the provider mix of qwen/qwen3.6-27b as measured on the public
// endpoints route (2026-08-02). It is the fixture precisely because it is not
// tidy: three providers share the 262 144 context length while their
// completion bounds differ by 4x, and one provider sits an order of magnitude
// below the rest.
var liveMix = []backends.ProviderEndpoint{
	{ProviderName: "Io Net", ContextLength: 32768, MaxCompletionTokens: 32768},
	{ProviderName: "Morph", ContextLength: 131072, MaxCompletionTokens: 131072},
	{ProviderName: "Chutes", ContextLength: 262144, MaxCompletionTokens: 65536},
	{ProviderName: "SiliconFlow", ContextLength: 262144, MaxCompletionTokens: 262144},
	{ProviderName: "Phala", ContextLength: 262144, MaxCompletionTokens: 262140},
	{ProviderName: "DeepInfra", ContextLength: 262144, MaxCompletionTokens: 81920},
}

// TestEligibleProvidersDropsSmallWindows is probe (a) in arithmetic form: a
// prompt that only the 262 144-token providers can hold must leave the smaller
// ones out of provider.only.
//
// Falsifying implementation: planning and filtering against the MODEL-level
// context_length (262144 for all six). Every provider would pass every test —
// the only-list would name all six, and OpenRouter would happily route the
// prompt to Io Net's 32 768-token window, which is the overflow this whole
// wave exists to prevent.
func TestEligibleProvidersDropsSmallWindows(t *testing.T) {
	// 150 000 runes of prompt => 83 334 tokens conservatively; with a 500-token
	// answer that fits 131 072 and 262 144, never 32 768.
	inputTokens := promptguard.TokenEstimate(150000)
	got := eligibleProviders(liveMix, inputTokens, 500)
	want := []string{"Morph", "Chutes", "SiliconFlow", "Phala", "DeepInfra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("eligible = %v, want %v (Io Net's 32768-token window cannot hold %d input tokens)",
			got, want, inputTokens)
	}
}

// TestEligibleProvidersHonoursCompletionBound is probe (c): context_length and
// max_completion_tokens are independent constraints.
//
// Falsifying implementation: checking context_length only. Chutes (262144 /
// 65536) then survives a 70 000-token answer request — the provider accepts
// the prompt and truncates or rejects the completion, and the failure surfaces
// as a mid-answer cut rather than as a routing decision.
func TestEligibleProvidersHonoursCompletionBound(t *testing.T) {
	got := eligibleProviders(liveMix, 10000, 70000)
	// Chutes is the discriminator: it carries the LARGEST context length in the
	// mix (262 144, tied for first) and still cannot serve this request,
	// because its completion bound is 65 536. Io Net fails on context length
	// instead, so the two conditions are visible as separate causes.
	want := []string{"Morph", "SiliconFlow", "Phala", "DeepInfra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("eligible = %v, want %v (Chutes has the largest window but caps completions at 65536)", got, want)
	}
}

// TestEligibleProvidersKeepsUndeclaredCompletionBound: an endpoint that
// declares no completion bound is UNKNOWN, not "too small" — dropping it would
// silently shrink the routable set on missing metadata, and the wire-level
// max_tokens still filters it server-side.
func TestEligibleProvidersKeepsUndeclaredCompletionBound(t *testing.T) {
	eps := []backends.ProviderEndpoint{{ProviderName: "Quiet", ContextLength: 262144}}
	if got := eligibleProviders(eps, 10000, 70000); !reflect.DeepEqual(got, []string{"Quiet"}) {
		t.Errorf("eligible = %v, want [Quiet]", got)
	}
}

// TestTokenEstimateIsConservative is probe (d): the rune→token direction runs
// over the MEASURED MINIMUM density (1.8 runes/token), not over a mean.
//
// Falsifying implementation: the corpus mean (~2.8 runes/token, i.e.
// runes*10/28). The fixture is the boundary where the two disagree about
// routing: 40 000 prompt runes and a 500-token answer against a 20 000-token
// provider. Conservative → 22 723 tokens, provider excluded. Mean → 14 786
// tokens, provider admitted and the request overflows on a window ctx chose
// for it.
func TestTokenEstimateIsConservative(t *testing.T) {
	const (
		promptRunes  = 40000
		meanEstimate = 14286 // what runes/2.8 would report for the same prompt
	)
	conservative := promptguard.TokenEstimate(promptRunes)
	if want := 22223; conservative != want {
		t.Errorf("TokenEstimate(%d) = %d, want %d (9/5 fraction, rounded up)", promptRunes, conservative, want)
	}

	tight := []backends.ProviderEndpoint{{ProviderName: "Tight", ContextLength: 20000, MaxCompletionTokens: 20000}}
	// The consequence, not the constant: under the mean the provider is routed
	// to and overflows; under the minimum it is excluded.
	if got := eligibleProviders(tight, meanEstimate, 500); len(got) == 0 {
		t.Fatalf("fixture broken: the mean estimate must ADMIT the tight provider, otherwise the two implementations are indistinguishable here")
	}
	if got := eligibleProviders(tight, conservative, 500); len(got) != 0 {
		t.Errorf("eligible = %v at %d estimated input tokens — a 20000-token window cannot hold input+500, so the estimate came from a mean rather than the measured minimum",
			got, conservative)
	}
}

// TestTokenEstimateRoundsUp: a fractional token is a whole token on the wire.
func TestTokenEstimateRoundsUp(t *testing.T) {
	for _, tc := range []struct{ runes, want int }{{0, 0}, {-5, 0}, {1, 1}, {9, 5}, {10, 6}, {18, 10}} {
		if got := promptguard.TokenEstimate(tc.runes); got != tc.want {
			t.Errorf("TokenEstimate(%d) = %d, want %d", tc.runes, got, tc.want)
		}
	}
}

// TestReserveNeverBelowTheRequestedCompletion pins the self-consistency rule
// behind AutoWindowOutputFloor: the provider that DEFINES the AUTO window must
// survive its own eligibility test on an empty prompt. With a reserve smaller
// than maxOut it would not — the plan would promise a window the filter then
// refuses, and the member would skip itself out of every chain.
func TestReserveNeverBelowTheRequestedCompletion(t *testing.T) {
	for _, maxOut := range []int{1, 500, AutoWindowOutputFloor, 70000} {
		best := maxContextLength(liveMix)
		window := best - reserveFor(maxOut)
		if window <= 0 {
			t.Fatalf("maxOut %d: window %d — fixture cannot express the invariant", maxOut, window)
		}
		// The largest prompt the plan permits, expressed back in tokens.
		input := promptguard.TokenEstimate(promptguard.RuneBudget(window))
		if got := eligibleProviders(liveMix, input, maxOut); len(got) == 0 {
			t.Errorf("maxOut %d: a prompt sized to the planned window has no eligible provider — reserve %d is below the completion bound",
				maxOut, reserveFor(maxOut))
		}
	}
}

// TestMaxContextLengthTakesTheBest: the AUTO window is the best provider's
// window, not the worst. Taking the minimum would give away the capacity the
// per-request constraint exists to make safe.
func TestMaxContextLengthTakesTheBest(t *testing.T) {
	if got := maxContextLength(liveMix); got != 262144 {
		t.Errorf("maxContextLength = %d, want 262144", got)
	}
}

// --- merge rule (probe g, arithmetic half) ---.

func TestMergeProviderConstraintPreservesOperatorFields(t *testing.T) {
	extra := map[string]any{
		"provider":        map[string]any{"ignore": []any{"Phala"}, "allow_fallbacks": false},
		"require_special": true,
	}
	got, ok := mergeProviderConstraint(extra, []string{"Phala", "DeepInfra"}, &autoMember{maxOut: 500})
	if !ok {
		t.Fatal("merge refused a satisfiable constraint")
	}
	prov, _ := got["provider"].(map[string]any)
	if !reflect.DeepEqual(prov["only"], []any{"DeepInfra"}) {
		t.Errorf("provider.only = %v, want [DeepInfra] (an ignored provider must not reappear in only)", prov["only"])
	}
	if !reflect.DeepEqual(prov["ignore"], []any{"Phala"}) {
		t.Errorf("provider.ignore = %v — the operator's field was dropped", prov["ignore"])
	}
	if prov["allow_fallbacks"] != false {
		t.Errorf("provider.allow_fallbacks = %v — the operator's field was dropped", prov["allow_fallbacks"])
	}
	if got["require_special"] != true {
		t.Errorf("non-provider extra_body key lost: %v", got)
	}
	// The pool snapshot's maps are shared across every request on this
	// backend; a merge that writes into them leaks one request's constraint
	// into all later ones (and applyOpenAIBodyExtras then adds zdr/deny to the
	// snapshot too).
	if _, leaked := extra["provider"].(map[string]any)["only"]; leaked {
		t.Error("merge wrote into the source extra_body — the pool snapshot is shared, not per-request")
	}
}

func TestMergeProviderConstraintIntersectsOperatorOnly(t *testing.T) {
	extra := map[string]any{"provider": map[string]any{"only": []any{"Chutes", "DeepInfra"}}}
	got, ok := mergeProviderConstraint(extra, []string{"DeepInfra", "SiliconFlow"}, &autoMember{maxOut: 500})
	if !ok {
		t.Fatal("merge refused a satisfiable constraint")
	}
	prov, _ := got["provider"].(map[string]any)
	if !reflect.DeepEqual(prov["only"], []any{"DeepInfra"}) {
		t.Errorf("provider.only = %v, want [DeepInfra] — two restrictions merge to their intersection, never to a union or a replacement", prov["only"])
	}
}

func TestMergeProviderConstraintRefusesEmptyResult(t *testing.T) {
	extra := map[string]any{"provider": map[string]any{"only": []any{"Chutes"}}}
	if _, ok := mergeProviderConstraint(extra, []string{"DeepInfra"}, &autoMember{maxOut: 500}); ok {
		t.Error("merge produced a constraint from an empty intersection — provider.only=[] routes anywhere or 400s")
	}
	if _, ok := mergeProviderConstraint(nil, nil, &autoMember{maxOut: 500}); ok {
		t.Error("merge produced a constraint from an empty eligible set")
	}
}

func TestMergeProviderConstraintInjectsMaxTokensOnlyWhenNeeded(t *testing.T) {
	got, _ := mergeProviderConstraint(nil, []string{"DeepInfra"}, &autoMember{maxOut: 8192, injectMaxTokens: true})
	if got["max_tokens"] != 8192 {
		t.Errorf("max_tokens = %v, want 8192 — an output bound the filter used must also reach the wire", got["max_tokens"])
	}
	got, _ = mergeProviderConstraint(nil, []string{"DeepInfra"}, &autoMember{maxOut: 500})
	if _, present := got["max_tokens"]; present {
		t.Error("max_tokens injected although Options.NumPredict already carries it — the injection would override the resolved bound")
	}
}

// TestResolveMaxOutFollowsWirePrecedence: the number the eligibility filter
// uses must be the number the request actually carries.
func TestResolveMaxOutFollowsWirePrecedence(t *testing.T) {
	base := SynthesisOptions(0) // NumPredict 500
	b := backends.Backend{}

	if got, inject := resolveMaxOut(base, nil, &b); got != 500 || inject {
		t.Errorf("plain = (%d,%v), want (500,false) — Options.NumPredict is marshalled as max_tokens", got, inject)
	}
	if got, inject := resolveMaxOut(base, map[string]any{"max_tokens": float64(70000)}, &b); got != 70000 || inject {
		t.Errorf("model_map params = (%d,%v), want (70000,false)", got, inject)
	}
	withExtra := backends.Backend{ExtraBody: map[string]any{"max_tokens": float64(1234)}}
	if got, inject := resolveMaxOut(base, map[string]any{"max_tokens": float64(70000)}, &withExtra); got != 1234 || inject {
		t.Errorf("extra_body = (%d,%v), want (1234,false) — extra_body wins the body merge, so it wins here", got, inject)
	}
	if got, inject := resolveMaxOut(Options{}, nil, &b); got != AutoWindowOutputFloor || !inject {
		t.Errorf("unbounded = (%d,%v), want (%d,true) — an unbounded call must inject the bound it filtered with",
			got, inject, AutoWindowOutputFloor)
	}
}

// TestConstrainFailsClosedOnAPlanMismatch: a plan applied to a chain it was
// not built from can no longer say which member is AUTO. The alternative to
// serving nothing is serving an UNCONSTRAINED prompt to a provider mix nobody
// checked — so the mismatch resolves to the empty chain.
func TestConstrainFailsClosedOnAPlanMismatch(t *testing.T) {
	plan := autoPlan{windows: []int{262144}, auto: []*autoMember{{endpoints: liveMix, maxOut: 500}}}
	chain := []backends.Backend{{Name: "a"}, {Name: "b"}}
	if got := plan.constrain(chain, 1000); len(got) != 0 {
		t.Errorf("constrain returned %d members for a mismatched plan — an unchecked prompt would go out", len(got))
	}
}

// TestConstrainLeavesNonAutoMembersUntouched: every backend that declares its
// own window keeps byte-identical wire behaviour — this wave adds a path, it
// does not change the existing one.
func TestConstrainLeavesNonAutoMembersUntouched(t *testing.T) {
	chain := []backends.Backend{{Name: "local", NumCtx: 32768, ExtraBody: map[string]any{"k": "v"}}}
	plan := autoPlan{windows: []int{32768}, auto: []*autoMember{nil}}
	got := plan.constrain(chain, 40000)
	if len(got) != 1 {
		t.Fatalf("chain length %d, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].ExtraBody, map[string]any{"k": "v"}) {
		t.Errorf("extra_body = %v — a non-AUTO member was rewritten", got[0].ExtraBody)
	}
}
