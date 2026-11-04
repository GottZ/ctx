package federation

import (
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
)

// The exclusion reasons of FilterChain: fixed texts for the admin log of an
// empty chain — they name the filter, never a peer's instance id or the
// chain (topology-free, backends.ErrNoEligibleBackend doctrine).
const (
	reasonHopChain    = "federation: its learned instance is on the request's Ctx-Path"
	reasonEgressLocal = "federation: Ctx-Egress local admits this instance's own engines only"
	reasonTransitCap  = "federation: transit admits one instance hop per request"
)

// FilterChain applies the chain filters of A's model API to a chain
// Pool.Chain built (design/04 §4.5, normative order, R-02-13):
//
//	ExcludeHopChain(chain, in.Path, learned)
//	EgressLocal ? ExcludeForEgressLocal(…, learned) : TransitCap(…, learned)
//
// learned is one Pool.LearnedInstances() snapshot per request. An empty
// result is *backends.ErrNoEligibleBackend with the requirement and one
// fixed reason per dropped row (the caller answers 503 no_eligible_backend
// without topology; its Role is the caller's to set). The input chain is not
// written. The chain build of the route handlers (W02-07) calls it between
// Pool.Chain and the export filter.
func FilterChain(chain []backends.Backend, in fedhop.Inbound, pol Policy, learned map[string]string) ([]backends.Backend, error) {
	var excluded []backends.ExclusionReason
	drop := func(before, after []backends.Backend, reason string) {
		left := make(map[string]bool, len(after))
		for _, b := range after {
			left[b.ID] = true
		}
		for _, b := range before {
			if !left[b.ID] {
				excluded = append(excluded, backends.ExclusionReason{Backend: b.Name, Reason: reason})
			}
		}
	}
	out := backends.ExcludeHopChain(chain, in.Path, learned)
	drop(chain, out, reasonHopChain)
	var next []backends.Backend
	if pol.EgressLocal {
		next = backends.ExcludeForEgressLocal(out, learned)
		drop(out, next, reasonEgressLocal)
	} else {
		next = backends.TransitCap(out, learned)
		drop(out, next, reasonTransitCap)
	}
	if len(next) == 0 {
		return nil, &backends.ErrNoEligibleBackend{Required: pol.Required, Excluded: excluded}
	}
	return next, nil
}
