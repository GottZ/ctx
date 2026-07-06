// Aging escape (Vorhaben E wave MW25, design/04 §4.6 wave FA): a background
// waiter older than dispatch.background_aging_after may break the herald
// term ONCE — admitted despite demand > 0 — so background does not starve
// forever under sustained interactive demand (at target scale that means:
// embeddings age for ALL tenants, R7). The escape is F-B7-capped: it NEVER
// overtakes a waiting interactive acquire (structural — wakeLocked picks
// interactive first and consults the escape only when the interactive queue
// is empty), and on targets with an interactive role it requires
// preempt_background=true (coupling invariant, code predicate — on a
// non-preempt target an aged background lease would head-of-line-block a NEW
// interactive request for the full generation, up to 180 s + prompt
// processing). FA is the ONLY tag-1-built exception to the herald term (K7);
// options A (reserved slots) and C (admission dwell) stay named, un-built.
// Default 0 = off ⇒ the herald term is unweakened and behavior is
// byte-identical (E-F5). Anti-ping-pong is structural: an aged-admitted
// lease stays normally preemptable (worst case = preemption latency), and a
// preempted arm re-enqueues with a FRESH wait clock — the next escape is at
// least one aging period away. The aged admit/preempt counters feed the
// activation gate's waste metric (preempts_of_aged_leases, design/04 §4.6).
package dispatch

import "time"

// backgroundOnlyRoles are the routing roles ONLY scheduler arms pull chains
// for: backgroundRoles (enforcing.go) minus "embed", which also serves the
// interactive query-path embeds. A target whose rows carry any role OUTSIDE
// this set has an interactive role for the FA coupling invariant.
var backgroundOnlyRoles = map[string]struct{}{
	"dream":       {},
	"dream-embed": {},
	"digest":      {},
	"classify":    {},
}

// hasInteractiveRole reports whether ANY row of one origin (authoritative or
// not — serving interactive traffic is a physical property of the target,
// not a K2 authority question) carries an interactive-capable role.
// Fail-closed toward the protected good: a group without a single declared
// role counts as interactive-capable — an unknown topology must not open the
// aging escape on a latency-bearing target.
func hasInteractiveRole(group []BackendRow) bool {
	declared := false
	for _, r := range group {
		for _, role := range r.Roles {
			declared = true
			if _, bg := backgroundOnlyRoles[role]; !bg {
				return true
			}
		}
	}
	return !declared
}

// agingEscapeLocked is the FA admission predicate (Dispatcher.mu held): may
// the HEAD background waiter of st break the herald term now? True only when
// (1) the escape is armed (background_aging_after > 0), (2) the oldest
// background waiter has waited at least that long (FIFO: the head IS the
// oldest — later arrivals are younger and stay gated), and (3) the coupling
// invariant holds: on a target with an interactive role the dispatcher must
// be able to take the slot back via preemption (design/04 §4.6, F-B7).
// Interactive precedence is NOT re-checked here — the caller (wakeLocked)
// only reaches the background branch when the interactive queue is empty.
func (d *Dispatcher) agingEscapeLocked(st *targetState, now time.Time) bool {
	aging := d.currentSettings().BackgroundAgingAfter
	if aging <= 0 {
		return false // default off: herald term unweakened (E-F5/K6)
	}
	head := st.background.oldest()
	if head.IsZero() || now.Sub(head) < aging {
		return false
	}
	tp := d.policy.Load().Targets[st.origin]
	if tp.InteractiveRole && !tp.PreemptBackground {
		return false // coupling invariant (F-B7): no escape where preemption cannot reclaim
	}
	return true
}

// agedAdmitLocked marks one waiter as admitted via the aging escape
// (Dispatcher.mu held): the lease becomes distinguishable for the waste
// metric (preempts_of_aged_leases — the activation gate's NEGATIVE
// condition) and the admit itself is visible per target.
func (d *Dispatcher) agedAdmitLocked(st *targetState, w *waiter, now time.Time) {
	w.aged = true
	st.preempt.agedAdmits++
	d.logger.Info("dispatch: background admitted via aging escape despite interactive demand",
		"target", st.origin, "role", w.role, "waited", now.Sub(w.enqueued),
		"demand", d.demand.Load())
}
