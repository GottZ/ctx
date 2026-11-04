package backends

// The chain filters of A's model API (design/04 §4.5 "Kettenfilter auf A für
// /v1//v2", R-02-13; F-W9): pure functions over a chain Pool.Chain already
// built, so the caller (W02-07's chain build: Pool.Chain → FilterChain →
// export filter → llm.WalkChain) applies them in the fixed order of
// federation.FilterChain. No network, no lock, the input chain is never
// written. An "instance hop" is a row that leads to another ctx instance:
// provider class ctx, or a row whose answers taught this instance a peer
// instance id (NotePeerInstance — a ctx instance registered as generic).

// LearnedInstances is the snapshot of the learned peer instances: backend id
// → the last Ctx-Instance the row answered with. One lock acquisition per
// call, not one per row — a chain filter takes the snapshot once per request
// and reads it lock-free. A copy: later learning does not change a snapshot
// already taken. nil when nothing was learned. (Learning itself, its reset
// on reload and the lock-free atomic.Pointer form are F-W11's; until F-W12
// sends Ctx-Instance to peers, nothing in production learns.)
func (p *Pool) LearnedInstances() map[string]string {
	p.healthM.Lock()
	defer p.healthM.Unlock()
	var out map[string]string
	for id, h := range p.health {
		if h.peerInstance == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[id] = h.peerInstance
	}
	return out
}

// isInstanceHop reports whether b leads to another ctx instance.
func isInstanceHop(b *Backend, learned map[string]string) bool {
	return b.ProviderClass == ProviderCtx || learned[b.ID] != ""
}

// keep returns the rows of chain for which ok holds, in order, in a new slice.
func keep(chain []Backend, ok func(*Backend) bool) []Backend {
	out := make([]Backend, 0, len(chain))
	for i := range chain {
		if ok(&chain[i]) {
			out = append(out, chain[i])
		}
	}
	return out
}

// ExcludeHopChain drops every row whose learned instance is on the request's
// Ctx-Path: that instance has already handled the request, a row to it is a
// loop the 508 would only confirm one round trip later.
func ExcludeHopChain(chain []Backend, path []string, learned map[string]string) []Backend {
	if len(path) == 0 || len(learned) == 0 {
		return keep(chain, func(*Backend) bool { return true })
	}
	onPath := make(map[string]bool, len(path))
	for _, id := range path {
		onPath[id] = true
	}
	return keep(chain, func(b *Backend) bool {
		inst := learned[b.ID]
		return inst == "" || !onPath[inst]
	})
}

// ExcludeForEgressLocal is Ctx-Egress: local — "only A's own engines, no
// further instance hop" (design/04 §4.5, Finding 3, D5). It drops (a) every
// row of provider class ctx, (b) every row with a learned instance (a ctx
// instance registered as generic) and (c) every row that is not local or
// LAN: external, and fail-closed any locality outside the vocabulary.
// Rev. 0 dropped the external rows only — a prompt B never meant to leave A
// would have reached a LAN instance C. Residual risk, named: a ctx instance
// registered as generic whose instance A has not learned stays (§5.5 no. 2).
func ExcludeForEgressLocal(chain []Backend, learned map[string]string) []Backend {
	return keep(chain, func(b *Backend) bool {
		if isInstanceHop(b, learned) {
			return false
		}
		return b.Locality == LocalityLocal || b.Locality == LocalityLAN
	})
}

// TransitCap is the transit rule (no Ctx-Egress: local): at most the FIRST
// instance-hop row of the chain's order stays, every later one is dropped;
// local and external rows stay. One instance hop per hop makes the mesh's
// failover fan-out linear in the depth instead of multiplicative (design/04
// §5.1 L8, Finding 7.b).
func TransitCap(chain []Backend, learned map[string]string) []Backend {
	seen := false
	return keep(chain, func(b *Backend) bool {
		if !isInstanceHop(b, learned) {
			return true
		}
		if seen {
			return false
		}
		seen = true
		return true
	})
}
