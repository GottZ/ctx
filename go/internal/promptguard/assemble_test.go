// Wave H12 budget probes (design/04 §7-H12 a-d). Each case names the
// FALSIFYING implementation in its comment: the mutation the case is measured
// against, so a green run is evidence rather than decoration.
package promptguard

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func rulePart(payload string) Part {
	return Part{Kind: "rule", Payload: payload, Priority: PriorityRule}
}

// TestAssembleRefusesBudgetBelowTheRule is probe (a): a budget too small for
// the security rule is an ERROR and yields NO prompt.
//
// Falsifying implementation: an Assemble that treats the rule like any other
// part and shortens it to fit. That variant returns a non-empty prompt here —
// a prompt whose markers claim a nonce binding the truncated rule no longer
// states. "No prompt" is the only correct degradation.
func TestAssembleRefusesBudgetBelowTheRule(t *testing.T) {
	rule := CanonicalRule()
	parts := []Part{
		rulePart(rule),
		{Kind: "question", Payload: "why", Priority: PriorityQuestion},
		{Kind: "source", Ref: "1", Payload: strings.Repeat("x", 500), Priority: PriorityContent},
	}

	got, rep := Assemble(parts, utf8.RuneCountInString(rule)-1)
	if !errors.Is(rep.Err, ErrRuleOverBudget) {
		t.Errorf("Err = %v, want ErrRuleOverBudget", rep.Err)
	}
	if got != "" {
		t.Errorf("prompt = %q, want the empty string — a shortened rule is not a degraded prompt, it is a false claim", got)
	}
	// A budget that holds the rule EXACTLY and nothing else must refuse too.
	// Found by this probe against the first implementation, which happily
	// emitted the rule alone with Err == nil: a prompt carrying a security
	// rule and no task is not a smaller prompt, it is a rule with nothing to
	// apply it to, and the model answers from whatever content survived.
	if got, rep := Assemble(parts, utf8.RuneCountInString(rule)); got != "" || rep.Err == nil {
		t.Errorf("prompt = %q (err %v): the rule alone is not a prompt", got, rep.Err)
	}
}

// TestAssembleDropsFromBelowKeepingTheFirstCandidate is probe (b): 500
// candidates injected past every call-site cap must still produce a prompt
// under budget, with Dropped > 0 and the FIRST candidate surviving.
//
// Falsifying implementations, both caught here: (1) shortening from the top
// (the rule or the question would go first, and the first candidate would die
// before the last), (2) shortening the earliest part of a priority class
// instead of the latest — candidate 1 is the highest-ranked one at every call
// site, so it is the one that must not be evicted for candidate 500.
func TestAssembleDropsFromBelowKeepingTheFirstCandidate(t *testing.T) {
	const budget = 20000
	rule := CanonicalRule()
	parts := []Part{rulePart(rule), {Kind: "question", Payload: "the question", Priority: PriorityQuestion}}
	for i := 1; i <= 500; i++ {
		parts = append(parts, Part{
			Kind: "source", Ref: itoa(i),
			Payload:  "candidate " + itoa(i) + " " + strings.Repeat("y", 1500),
			Priority: PriorityContent,
		})
	}

	got, rep := Assemble(parts, budget)
	if rep.Err != nil {
		t.Fatalf("Err = %v, want nil", rep.Err)
	}
	if n := utf8.RuneCountInString(got); n > budget {
		t.Errorf("prompt = %d runes, want <= %d", n, budget)
	}
	if rep.Runes != utf8.RuneCountInString(got) {
		t.Errorf("Report.Runes = %d, actual %d", rep.Runes, utf8.RuneCountInString(got))
	}
	if rep.Dropped == 0 {
		t.Errorf("Dropped = 0: 500 candidates at 1500 runes cannot fit %d without dropping", budget)
	}
	if !strings.Contains(got, rule) {
		t.Errorf("prompt lost the security rule")
	}
	if !strings.Contains(got, "the question") {
		t.Errorf("prompt lost the question")
	}
	if !strings.Contains(got, "candidate 1 ") {
		t.Errorf("prompt lost candidate 1 — eviction ran from the top of the class, not the bottom")
	}
	if strings.Contains(got, "candidate 500 ") {
		t.Errorf("prompt kept candidate 500 while dropping earlier ones")
	}
	// Every dropped ref must be a content ref, never the rule or the question.
	for _, ref := range rep.DroppedRefs {
		if ref == "" {
			t.Errorf("DroppedRefs contains an unnamed part — a singleton part was evicted")
		}
	}
}

// TestAssembleTruncationIsRuneSafe: a cut must never split a multi-byte rune.
// Falsifying implementation: a byte slice (payload[:n]) — it produces invalid
// UTF-8 on this input, which is the defect Issue #4 named.
func TestAssembleTruncationIsRuneSafe(t *testing.T) {
	parts := []Part{
		rulePart("R"),
		{Kind: "source", Ref: "1", Payload: strings.Repeat("ä", 4000), Priority: PriorityContent},
	}
	got, rep := Assemble(parts, 1000)
	if rep.Err != nil {
		t.Fatalf("Err = %v", rep.Err)
	}
	if !utf8.ValidString(got) {
		t.Errorf("assembled prompt is not valid UTF-8 — the cut split a rune")
	}
	if n := utf8.RuneCountInString(got); n > 1000 {
		t.Errorf("prompt = %d runes, want <= 1000", n)
	}
}

// TestChainRuneBudgetFollowsTheWeakestLink is probe (c): chain [98304, NULL]
// WITH a configured fallback resolves against the fallback, not against the
// first link.
//
// Falsifying implementation: a resolver reading windows[0] (or max instead of
// min). Both produce RuneBudget(98304) here, which the assertion rejects by
// value — the failover leg is walked with the prompt the first leg's window
// allowed, and that is the leg running exactly when the first is unavailable.
func TestChainRuneBudgetFollowsTheWeakestLink(t *testing.T) {
	const (
		first = 98304
		// The fallback is sized so its rune budget lands BELOW the pipeline cap.
		// That is load-bearing: at 32768 the cap binds for BOTH the correct and
		// the first-link implementation, and the case passes for the wrong
		// reason — measured, the first-link mutant survived a 32768 fixture.
		fallback = 8192
	)
	got, err := ChainRuneBudget([]int{first, 0}, fallback, BudgetSynthesis)
	if err != nil {
		t.Fatalf("ChainRuneBudget: %v", err)
	}
	want := RuneBudget(fallback)
	if want >= BudgetSynthesis {
		t.Fatalf("fixture broken: the fallback budget %d does not sit below the pipeline cap %d, so the cap would mask the weakest-link rule",
			want, BudgetSynthesis)
	}
	if got != want {
		t.Errorf("budget = %d, want %d (the NULL link's fallback)", got, want)
	}
	if got == RuneBudget(first) || got == BudgetSynthesis {
		t.Errorf("budget = %d followed the FIRST link (or the bare cap) — the chain's weakest member bounds the prompt", got)
	}
	// The pipeline ceiling is the other half of the min: a huge chain window
	// must not lift the prompt past what the pipeline accepts.
	if got, err := ChainRuneBudget([]int{262144, 262144}, 0, BudgetSynthesis); err != nil || got != BudgetSynthesis {
		t.Errorf("budget = %d (err %v), want the pipeline cap %d", got, err, BudgetSynthesis)
	}
}

// TestChainRuneBudgetFailsClosedOnUndeclaredWindow is probe (d): a chain
// carrying a link without a declared window and NO fallback must refuse.
//
// Falsifying implementation: a resolver substituting a hardcoded rate value
// ("assume 32768", or the model-level context_length). For the live openrouter
// row that number is the MAXIMUM over the routed providers (32 768 … 262 144),
// so a prompt sized against it overflows on every provider below the top. The
// variant returns a budget here and lets the prompt be built; this case
// requires the error.
func TestChainRuneBudgetFailsClosedOnUndeclaredWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		windows []int
	}{
		{"undeclared tail", []int{98304, 0}},
		{"undeclared head", []int{0, 98304}},
		{"all undeclared", []int{0}},
		{"empty chain", nil},
		{"negative window", []int{-1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChainRuneBudget(tc.windows, 0, BudgetSynthesis)
			if !errors.Is(err, ErrUndeclaredWindow) {
				t.Errorf("err = %v, want ErrUndeclaredWindow — a rate value is wrong here, not merely imprecise", err)
			}
			if got != 0 {
				t.Errorf("budget = %d, want 0", got)
			}
		})
	}
}

// TestRuneBudgetUsesTheConservativeRatio pins the measured constant: the
// conversion must use the MINIMUM observed chars/token (1.8), never one of the
// per-pipeline means (2.53 … 3.24). A mean-based variant returns a budget
// ~40-80 % too large on exactly the pipelines whose text tokenises densest.
func TestRuneBudgetUsesTheConservativeRatio(t *testing.T) {
	if got, want := RuneBudget(10000), 18000; got != want {
		t.Errorf("RuneBudget(10000) = %d, want %d (1.8 runes/token)", got, want)
	}
	if got := RuneBudget(10000); got >= 25300 {
		t.Errorf("RuneBudget(10000) = %d — that is the MEAN ratio, not the conservative minimum", got)
	}
	if got := RuneBudget(0); got != 0 {
		t.Errorf("RuneBudget(0) = %d, want 0 (never 'unlimited')", got)
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
