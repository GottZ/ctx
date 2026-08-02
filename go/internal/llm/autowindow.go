// AUTO context windows for openrouter-class chain members (E10-W2).
//
// H12 established the floor: a chain member without a declared window refuses
// the prompt, because the model-level context_length of a multi-provider
// router is the MAXIMUM over providers and sizing against it overflows on
// every provider below the top. This wave keeps that floor and adds the
// missing knowledge instead of relaxing it — a NULL num_ctx on an
// openrouter-class row now means AUTO:
//
//  1. PLAN — the member's window becomes the best window its providers
//     actually offer (max over the discovered endpoints) minus the completion
//     reserve. It enters promptguard.ChainRuneBudget as a normal window, so
//     the min() over the chain still holds: a small fixed member next to an
//     AUTO one keeps sizing the prompt, which is correct — the prompt must
//     survive every failover leg.
//  2. CONSTRAIN — after the prompt exists, its measured size decides WHICH
//     providers may serve it. OpenRouter's routing does not filter on prompt
//     size (it filters output-side on max_tokens), so ctx does the input side
//     itself: provider.only carries exactly the endpoints that hold
//     inputTokens + maxOut, and max_tokens carries the output side as the
//     second layer.
//
// A member whose eligible set is empty is SKIPPED for this one request — the
// call fails over to the next chain link rather than failing. That is the same
// doctrine as every other per-request exclusion in the chain walk: the member
// is not down, it is merely unable to serve THIS prompt.
package llm

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/promptguard"
)

// AutoWindowOutputFloor is the minimum completion head-room, in TOKENS, an
// AUTO window reserves out of the provider's context length.
//
// Derivation, in the order the number is actually constrained:
//
//  1. It must be >= every max_tokens this code sends, or the arithmetic
//     contradicts itself: the plan promises a window of ctx-reserve, the
//     eligibility test then demands ctx >= input+maxOut, and with
//     reserve < maxOut the very provider that DEFINED the window could fail
//     its own test. Hence the effective reserve is max(maxOut, this floor) —
//     the floor is a lower bound on head-room, never a cap on the request.
//  2. The largest completion bound in the repo is 1000 tokens
//     (dream/validate_temporal); synthesis asks for 500. A floor at that order
//     would leave nothing for the part max_tokens does NOT bound: reasoning
//     models on OpenRouter spend their chain-of-thought inside the same
//     window, and ctx only disables reasoning when think is explicitly false.
//  3. 8192 is one generous reasoning trace plus the answer — an order above
//     the largest bound in (2), and irrelevant at the sizes it actually meets:
//     at a 262 144-token provider the planned window lands ~250 k tokens,
//     which promptguard.BudgetSynthesis (40 000 runes) truncates by three
//     orders of magnitude anyway. The reserve exists to keep (1) true, not to
//     shape prompt size.
const AutoWindowOutputFloor = 8192

// autoMember is the per-request AUTO bookkeeping of ONE chain member: which
// providers exist and what output bound the wire will carry for this call.
type autoMember struct {
	endpoints []backends.ProviderEndpoint
	// maxOut is the max_tokens value this member's request WILL carry — the
	// wire truth, not an intention: extra_body override first (it wins the
	// body merge), then the resolved options, then the floor (which is then
	// injected, so the two never disagree).
	maxOut int
	// injectMaxTokens is set when maxOut does not already reach the wire
	// through Options.NumPredict or extra_body and must be merged in.
	injectMaxTokens bool
}

// autoPlan is the resolved window plan of one chain for one request.
// windows[i] feeds promptguard.ChainRuneBudget; auto[i] is non-nil exactly for
// the members whose window was discovered rather than declared.
type autoPlan struct {
	windows []int
	auto    []*autoMember
}

// planAutoWindows resolves the chain's planning windows. Priority per member,
// in this order (decision E10 / E10-W2):
//
//	(a) num_ctx > 0                        → the declared window, unchanged
//	(b) NULL + openrouter + discovery data → AUTO (max provider ctx − reserve)
//	(c) otherwise                          → 0, i.e. the operator fallback
//	(d) …and without one, ErrUndeclaredWindow downstream
//
// Every failure inside (b) — no model, no data, a window smaller than its own
// reserve — falls through to (c). Discovery never invents a window; it either
// knows one or steps aside.
func planAutoWindows(ctx context.Context, chain []backends.Backend, role string,
	baseOpts Options, ttl time.Duration,
) autoPlan {
	plan := autoPlan{windows: chainWindows(chain), auto: make([]*autoMember, len(chain))}
	for i := range chain {
		b := &chain[i]
		if b.NumCtx > 0 || b.ProviderClass != backends.ProviderOpenRouter {
			continue
		}
		spec := b.ModelFor(role)
		if spec.Model == "" {
			continue
		}
		eps, ok := backends.DefaultEndpointCache().Endpoints(ctx, *b, spec.Model, ttl)
		if !ok || len(eps) == 0 {
			continue
		}
		m := &autoMember{endpoints: eps}
		m.maxOut, m.injectMaxTokens = resolveMaxOut(baseOpts, spec.Params, b)
		window := maxContextLength(eps) - reserveFor(m.maxOut)
		if window <= 0 {
			// The best provider cannot even hold the completion — there is no
			// prompt size this member could serve. Treat it as undiscovered.
			continue
		}
		plan.windows[i] = window
		plan.auto[i] = m
	}
	return plan
}

// constrain applies the per-request provider constraint to the chain: AUTO
// members get provider.only (plus max_tokens where the wire would not carry
// it), members without an eligible provider are dropped for this request.
// Non-AUTO members pass through untouched — byte-identical wire behaviour for
// every backend that declares its own window.
//
// promptRunes is the size of the ACTUAL prompt, not the budget it was fitted
// against: the budget charges the parts, the builder renders markup around
// them, and it is the rendered document the provider must hold.
func (p autoPlan) constrain(chain []backends.Backend, promptRunes int) []backends.Backend {
	if len(p.auto) != len(chain) {
		// The plan is index-aligned with the chain it was built from. A caller
		// that re-resolved the chain in between has invalidated it — and the
		// two ways out are "constrain nothing" (an unconstrained prompt to a
		// provider mix nobody checked) or "serve nothing". Fail closed, loudly.
		slog.Error("llm: auto-window plan does not match the chain it is applied to",
			"plan", len(p.auto), "chain", len(chain))
		return nil
	}
	inputTokens := promptguard.TokenEstimate(promptRunes)
	out := make([]backends.Backend, 0, len(chain))
	for i := range chain {
		m := p.auto[i]
		if m == nil {
			out = append(out, chain[i])
			continue
		}
		eligible := eligibleProviders(m.endpoints, inputTokens, m.maxOut)
		merged, ok := mergeProviderConstraint(chain[i].ExtraBody, eligible, m)
		if !ok {
			// Skipped, not failed: no provider of THIS member can hold the
			// prompt, so the request belongs to the next chain link.
			continue
		}
		member := chain[i]
		member.ExtraBody = merged
		out = append(out, member)
	}
	return out
}

// eligibleProviders names the endpoints that can serve a prompt of inputTokens
// with an output bound of maxOut. Both conditions are load-bearing and neither
// implies the other: context_length is the INPUT+output ceiling (a 262 144-ctx
// provider that caps completions at 65 536 fails a 70 000-token answer), while
// max_completion_tokens bounds the answer alone. An endpoint that declares no
// completion bound is not disqualified — unknown is not "too small", and the
// wire-level max_tokens still filters it server-side.
func eligibleProviders(eps []backends.ProviderEndpoint, inputTokens, maxOut int) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		if e.ContextLength < inputTokens+maxOut {
			continue
		}
		if e.MaxCompletionTokens > 0 && e.MaxCompletionTokens < maxOut {
			continue
		}
		out = append(out, e.ProviderName)
	}
	return out
}

// maxContextLength is the best window the provider mix offers — the AUTO
// planning window before the completion reserve. The MAXIMUM is deliberate:
// the prompt may grow to what the best provider carries, because the
// constraint step then routes it to exactly those providers. Taking the
// minimum would give away the capacity this wave exists to use; taking the
// model-level context_length would take capacity that no single provider has.
func maxContextLength(eps []backends.ProviderEndpoint) int {
	best := 0
	for _, e := range eps {
		if e.ContextLength > best {
			best = e.ContextLength
		}
	}
	return best
}

// reserveFor is the completion head-room subtracted from an AUTO window: never
// less than what the request may generate (see AutoWindowOutputFloor, point 1).
func reserveFor(maxOut int) int {
	if maxOut > AutoWindowOutputFloor {
		return maxOut
	}
	return AutoWindowOutputFloor
}

// resolveMaxOut determines the output bound the member's request will carry,
// and whether it has to be injected to get there. Order = wire precedence:
// extra_body.max_tokens survives the body merge (last write wins,
// applyOpenAIBodyExtras), Options.NumPredict is marshalled as max_tokens, and
// an unbounded call gets the floor — which then MUST be injected, otherwise
// the eligibility test would filter against a number the provider never sees.
func resolveMaxOut(base Options, params map[string]any, b *backends.Backend) (int, bool) {
	if v, ok := numericValue(b.ExtraBody["max_tokens"]); ok && v > 0 {
		return v, false
	}
	resolved := *b
	opts, _ := applyModelParams(base, params, &resolved)
	if opts.NumPredict > 0 {
		return opts.NumPredict, false
	}
	return AutoWindowOutputFloor, true
}

// mergeProviderConstraint builds THIS request's extra_body: the operator's own
// fields carried through, the computed constraint merged on top.
//
// Merge rule (the operator's intent is never silently discarded, and the
// result is never looser than either side):
//
//   - provider.ignore is SUBTRACTED from the eligible set — an ignored
//     provider must not reappear inside an only-list ctx computed.
//   - provider.only, when the operator set one, is INTERSECTED with the
//     eligible set. Never replaced: only is a restriction, and a restriction
//     merged with another restriction is their intersection.
//   - every other provider field (order, allow_fallbacks, sort, quantizations,
//     require_parameters, …) and every other extra_body key is passed through
//     verbatim.
//   - an empty result is a SKIP verdict (ok=false), not an empty only-list:
//     provider.only=[] would either 400 or, worse, be ignored and route
//     anywhere.
//
// The returned maps are fresh copies. That is not hygiene, it is a
// requirement: the source map belongs to the immutable pool snapshot, and
// applyOpenAIBodyExtras writes provider.zdr/data_collection into whatever map
// it finds under "provider" — mutating the snapshot's map would leak one
// request's constraint into every later request on that backend.
func mergeProviderConstraint(extra map[string]any, eligible []string, m *autoMember) (map[string]any, bool) {
	out := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		out[k] = v
	}
	prov := make(map[string]any)
	if p, ok := extra["provider"].(map[string]any); ok {
		for k, v := range p {
			prov[k] = v
		}
	}
	only := subtract(eligible, stringList(prov["ignore"]))
	if userOnly := stringList(prov["only"]); len(userOnly) > 0 {
		only = intersect(only, userOnly)
	}
	if len(only) == 0 {
		return nil, false
	}
	prov["only"] = toAnySlice(only)
	out["provider"] = prov
	if m.injectMaxTokens {
		out["max_tokens"] = m.maxOut
	}
	return out, true
}

// stringList reads a JSONB-shaped list of provider names. []any is the form a
// row's extra_body arrives in (encoding/json); []string is what a Go-built
// tuple may carry. Non-string members are dropped — a malformed entry must not
// silently widen or narrow the set by matching nothing under a wrong type.
func stringList(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// toAnySlice renders the computed set in the JSONB shape the wire body uses.
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func subtract(in, remove []string) []string {
	if len(remove) == 0 {
		return in
	}
	drop := make(map[string]bool, len(remove))
	for _, r := range remove {
		drop[r] = true
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !drop[s] {
			out = append(out, s)
		}
	}
	return out
}

func intersect(in, keep []string) []string {
	allow := make(map[string]bool, len(keep))
	for _, k := range keep {
		allow[k] = true
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if allow[s] {
			out = append(out, s)
		}
	}
	return out
}

// numericValue reads a JSON number in either of the two shapes a settings/JSONB
// round-trip produces.
func numericValue(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}
