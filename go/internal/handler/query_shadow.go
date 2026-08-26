package handler

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/rrf"
)

// M-W2 — the shadow-visibility seam of design/05 §4.2.
//
// A measurement has to compare "with the catalog" against "without the
// catalog", so the catalog blocks must EXIST. Exist retrievably and they are
// live before anything about them has been shown; do not exist and there is
// nothing to measure. `shadow_types` is the way out: named types are added to
// the p_types_visible list of ctx_rrf and ctx_rrf_arms for ONE request, and to
// nothing else in the pipeline.
//
// Everything in this file is the price of that seam being paid honestly. Seven
// gates, all fail-closed, plus one obligation that follows from the sixth:
//
//	G1 not an admin                                  → 403
//	G2 without arm_ranks:true                        → 400
//	G3 with synthesize != false                      → 400
//	G4 a name that is not a key of the registry      → 400
//	G5 a name without retrieval.shadow_measurable    → 400
//	   (checkpoint and system-meta additionally by hard deny-list)
//	G6 rerank is FORCED off for the request           (not a status)
//	G7 together with include_content:true            → 400
//	+  every backend chain the request can still resolve must be locality=lan
//
// The order matters and is not cosmetic: G1 runs first, so a non-admin never
// learns from the status code whether the rest of the body would have been
// well-formed.

// shadowDenyTypes is the hard deny-list: two names that can never be measured,
// whatever their registry row says.
//
// Not configurable, and not derived from a policy. Live these two are exactly
// the types that satisfied the rule revision 1 wanted to use ("a shadow type
// must be retrieval.policy=excluded"): checkpoint carries 5 955 blocks, 13 of
// them sensitivity=credentials and 5 942 internal, and system-meta is the
// server's own bookkeeping. §5 B3 rests the protection of that pile on "not
// retrievable" — so the one seam that can lift that property must not be able
// to reach it, not even through a registry row somebody flipped by accident
// (design/05 changelog F-1).
var shadowDenyTypes = map[string]bool{
	"checkpoint":  true,
	"system-meta": true,
}

// shadowChainRoles are the roles a shadow request can still resolve a backend
// chain for, and therefore the roles the locality obligation covers.
//
// Derived, not guessed: synthesis is out because G3 forces synthesize:false,
// rerank is out because G6 forces the stage off — what remains is the embedding
// of the query text (always, unless the embed cache answers) and the
// translation stage, which the temporal LLM fallback shares since F3 §2.2.
var shadowChainRoles = []string{backends.RoleEmbed, backends.RoleTranslate}

// measureVisibleTypesFor is the two-slice rule of §4.2 in one function.
//
// Without shadow types it returns the visible slice ITSELF — same header, same
// backing array, no allocation — which is what makes a request without the
// field byte-identical to production down to the allocation profile.
//
// With shadow types it returns a CLONE plus the names. The clone is the whole
// point: Set.VisibleTypes() hands out a slice shared by every holder of that
// registry generation, and a plain append writes into its spare capacity when
// there is any. That failure mode shows up only for some capacities, which is
// precisely the kind that survives a review and ships.
func measureVisibleTypesFor(visible, shadow []string) []string {
	if len(shadow) == 0 {
		return visible
	}
	return append(slices.Clone(visible), shadow...)
}

// forceRerankOffForShadow is G6. The reranker is not merely expected to be off
// on this path, it is SWITCHED off: rerank.enabled is mut:"hot" and
// tenancy:"tenant-overridable" (config.go), and rrf.Rerank builds its judge
// prompt out of block CONTENT and ships it over the synthesis chain. A security
// invariant may not hang on a hot, tenant-overridable value.
func forceRerankOffForShadow(cfg rrf.RerankConfig, shadow []string) rrf.RerankConfig {
	if len(shadow) == 0 {
		return cfg
	}
	cfg.Enabled = false
	return cfg
}

// dropShadowResults removes every result of a shadow type before the response
// is built (§4.2, the addition to G7).
//
// A 400 on include_content alone would not be enough: sources[] carries title
// and category ALWAYS, so "no content in the return value" was only ever true
// of the arm_ranks sub-block. After this filter a shadow block leaves the
// server as a UUID plus numbers inside arm_ranks — no title, no category, no
// score row, no access-log entry — and the statement is about the whole
// request instead of one sub-block.
//
// It runs AFTER truncation on purpose: a shadow block that displaced a real
// block from the delivered window really did displace it, and back-filling the
// slot would hide exactly the effect the measurement is about.
func dropShadowResults(results []rrf.SearchResult, shadow []string) []rrf.SearchResult {
	if len(shadow) == 0 {
		return results
	}
	deny := make(map[string]bool, len(shadow))
	for _, n := range shadow {
		deny[n] = true
	}
	out := results[:0:0]
	for _, r := range results {
		if deny[r.TypeName] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// shadowGate is G1-G5 and G7. It returns the HTTP status and the client-facing
// message of the refusal, plus a server-side detail for the log; status 0 means
// the request may proceed (which includes every request that names no shadow
// type at all).
//
// The three type gates share ONE client-facing message on purpose. G4 (name not
// in the registry) and G5 (registered, but not measurable) are different bugs
// and are logged as such, but answering them differently would turn the seam
// into a membership oracle for the global type registry. The operator gets the
// precise reason from the server log, which is where a sweep operator already
// looks.
func shadowGate(req *queryRequest, isAdmin, armRanks bool, typeSet *blocktype.Set) (int, string, string) {
	if len(req.ShadowTypes) == 0 {
		return 0, "", ""
	}
	// G1 — first, so the refusal of a non-admin says nothing about the body.
	if !isAdmin {
		return http.StatusForbidden, "shadow_types requires an admin key", "not an admin"
	}
	// G2 — a measurement field may never act on a production path.
	if !armRanks {
		return http.StatusBadRequest, "shadow_types requires arm_ranks:true", "arm_ranks missing"
	}
	// G3 — necessary, not sufficient: it keeps synthesis out, G6 keeps the
	// rerank judge out, and only the two together mean "no LLM sees a block".
	if req.Synthesize == nil || *req.Synthesize {
		return http.StatusBadRequest, "shadow_types requires synthesize:false", "synthesize not false"
	}
	const typeMsg = "shadow_types names a type that is not shadow-measurable"
	for _, name := range req.ShadowTypes {
		// G4 — before any policy is read. Set.IsUntrusted is deliberately
		// fail-open for unknown names, and its argument rests on an invariant
		// this very field lifts (design/05 changelog F-10).
		if _, ok := typeSet.Resolve(name); !ok {
			return http.StatusBadRequest, typeMsg, fmt.Sprintf("type %q is not registered", name)
		}
		// G5, deny-list half — checked BEFORE the flag, so a wrongly flipped
		// registry row cannot open the protected pile.
		if shadowDenyTypes[name] {
			return http.StatusBadRequest, typeMsg, fmt.Sprintf("type %q is on the hard deny-list", name)
		}
		// G5, flag half.
		if !typeSet.IsShadowMeasurable(name) {
			return http.StatusBadRequest, typeMsg,
				fmt.Sprintf("type %q does not carry retrieval.shadow_measurable", name)
		}
	}
	// G7 — the leak that a "no content in the sub-block" reading missed: the
	// SURROUNDING response carries up to maxRetrievalSnippet runes per source.
	if req.IncludeContent {
		return http.StatusBadRequest, "shadow_types cannot be combined with include_content",
			"include_content set"
	}
	return 0, "", ""
}

// shadowChainLocality is the obligation that follows from G6: every role a
// shadow request can still resolve a chain for must resolve to locality=lan.
//
// Without it the seam would rest on a configuration fact instead of a request
// property. openrouter stands in the live pool with locality=external,
// trust=no-credentials, priority 20, enabled — and no-credentials lets
// everything up to sensitivity=personal through (backends/trust.go). "No LLM
// runs anyway" is then a statement about today's settings, not about this
// request.
//
// An EMPTY chain is not a violation: nothing is reachable, so nothing can
// leave. Anything else — external, local, or a row that carries no locality at
// all — refuses. Only lan passes, exactly as §4.2 words it; a stricter reading
// than the egress risk alone would demand, and stricter is the safe direction
// for a gate whose failure mode is a silent corpus leak.
//
// Returns the server-side detail of the first violation, and ok=false with it.
// A request that names no shadow type resolves NO chain here — the production
// path pays nothing for this gate, not even two pool lookups.
func (h *QueryHandler) shadowChainLocality(shadow []string, sens backends.Sensitivity, homeScope string) (string, bool) {
	if len(shadow) == 0 {
		return "", true
	}
	for _, role := range shadowChainRoles {
		chain, err := h.backendPool.Chain(role, sens, homeScope)
		if err != nil {
			continue // empty chain: unreachable, therefore harmless
		}
		for _, b := range chain {
			if b.Locality != backends.LocalityLAN {
				return fmt.Sprintf("role %s resolves backend %q with locality %q, want %q",
					role, b.Name, b.Locality, backends.LocalityLAN), false
			}
		}
	}
	return "", true
}
