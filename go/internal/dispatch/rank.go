// Rank scan (design/03 §4.2, wave A03-W2): the admission order of the classes
// is policy data — the rank table Settings.RankOrder (A-E2 no. 1) — and the
// two admission predicates read it instead of class literals. eligibleNowLocked
// admits a newcomer only when its own queue is empty and no higher-ranked
// class has a waiting, admissible head; wakeLocked hands each free slot to the
// head of the first class in rank order whose head is admissible. A waiting
// but inadmissible higher-ranked class does not block a lower one — the
// work-conservation rule. The FA aging escape (aging.go) stays the only herald
// bypass, for background only, and is consulted only when the scan reaches
// background, so it never overtakes a waiting admissible higher-ranked class
// (design/03 §4.8 doctrine line). For the two in-house classes the scan yields
// exactly the pre-A03-W2 literals: interactive before background. The
// external class (A03-W3) ranks between them (o1) or after background (o2) —
// a named order selected by dispatch.external_order — and is admissible only
// inside the external partition, so at the partition border it never blocks
// a lower class.

package dispatch

import (
	"fmt"
	"time"
)

// defaultRankOrder is the rank table in effect for the zero value and for
// every invalid order: the named order o1 (decision E2) — interactive, then
// external, then background. A waiting human is always served first (E-U1).
var defaultRankOrder = [...]Class{ClassInteractive, ClassExternal, ClassBackground}

// Compile-time guard: the default order names exactly numClasses classes. A
// class added before numClasses without its rank here makes one of these
// array lengths negative, so the build fails instead of padding the default
// order with a zero value (a second ClassInteractive).
var (
	_ [len(defaultRankOrder) - int(numClasses)]struct{}
	_ [int(numClasses) - len(defaultRankOrder)]struct{}
)

// DefaultRankOrder returns the default rank table (o1: interactive, external,
// background).
func DefaultRankOrder() [numClasses]Class { return defaultRankOrder }

// The named admission orders of dispatch.external_order (DECISIONS A-E2 no. 1:
// "auch 02 durch einen config switch im backend ermöglichen").
const (
	// ExternalOrderO1 is I ≻ XU ≻ B: an external requester goes before ctx'
	// own background work (decision E2, the default).
	ExternalOrderO1 = "o1"
	// ExternalOrderO2 is I ≻ B ≻ XU: ctx' background work goes first.
	ExternalOrderO2 = "o2"
)

// namedRankOrders is the data table behind dispatch.external_order. The
// internal partition R holds in both orders (it is an admission term, not a
// rank). The external bulk class XB joins both as the last rank (A03-W9).
var namedRankOrders = map[string][numClasses]Class{
	ExternalOrderO1: defaultRankOrder,
	ExternalOrderO2: {ClassInteractive, ClassBackground, ClassExternal},
}

// RankOrderFor returns the rank table of one named order (exact name — the
// config layer trims and lowers, V42) and whether the name is known.
func RankOrderFor(name string) ([numClasses]Class, bool) {
	o, ok := namedRankOrders[name]
	return o, ok
}

// validRankOrder reports whether order is a permutation of all classes with
// ClassInteractive on rank 0 (I-03-2: no class is ever served before a
// waiting, admissible interactive waiter).
func validRankOrder(order [numClasses]Class) bool {
	if order[0] != ClassInteractive {
		return false
	}
	var seen [numClasses]bool
	for _, c := range order {
		if !c.valid() || seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}

// resolveRankOrder returns the rank table a settings swap puts into effect.
// The zero value reads as the default order (like an empty Fairness reads as
// fifo); since ClassInteractive is 0, an all-interactive order is that zero
// value. Any other order must be valid (validRankOrder); otherwise the
// default is in effect and one WARN names the rejected order — never a silent
// recalibration (A-E2 no. 3).
func (d *Dispatcher) resolveRankOrder(order [numClasses]Class) [numClasses]Class {
	if order == ([numClasses]Class{}) {
		return defaultRankOrder
	}
	if !validRankOrder(order) {
		// fmt.Sprint renders the class names (the JSON handler would print
		// bare numbers for the array).
		d.logger.Warn("dispatch: invalid rank order — default order in effect",
			"order", fmt.Sprint(order), "default", fmt.Sprint(defaultRankOrder))
		return defaultRankOrder
	}
	return order
}

// admissibleLocked is the admission term of class c for a waiter of weight w
// on a target with slots slots (design/03 §4.2), independent of queue order:
// interactive held + w ≤ slots; background held + w ≤ slots ∧ demand == 0
// (the herald term — it keeps background out of the inter-lease gaps of
// running interactive requests, R3, not softenable here) ∧ the units are not
// reserved for a waiting external head (o1 only, A03-W4 d2 —
// externalReservedLocked); external see externalAdmissibleLocked. w is the waiter's weight in slot units (A03-W7:
// Request.Weight for the external class, always 1 in-house — requestWeight),
// and held is the sum of the weights. A class without a case is never
// admissible (fail-closed default, A03-W1 pattern).
func (d *Dispatcher) admissibleLocked(st *targetState, c Class, w, slots int) bool {
	switch c {
	case ClassInteractive:
		return st.held+w <= slots
	case ClassBackground:
		return st.held+w <= slots && d.demand.Load() == 0 && !d.externalReservedLocked(st, w, slots)
	case ClassExternal:
		return d.externalAdmissibleLocked(st, w, slots)
	default:
		d.logger.Error("dispatch: class without an eligibility case — not admissible (fail-closed default)",
			"target", st.origin, "class", uint8(c))
		return false
	}
}

// queuedLocked reports whether class c has a waiter queued on st. A class
// without a case has no queue (fail-closed default: enqueueLocked never
// queues it).
func (d *Dispatcher) queuedLocked(st *targetState, c Class) bool {
	switch c {
	case ClassInteractive:
		return st.interactive.total > 0
	case ClassBackground:
		return st.background.len() > 0
	case ClassExternal:
		return st.external.total > 0
	default:
		d.logger.Error("dispatch: class without a queue case — no queue (fail-closed default)",
			"target", st.origin, "class", uint8(c))
		return false
	}
}

// pickAdmissibleLocked is the wake half of the rank scan for ONE class:
// it dequeues the head of c when that head is admissible with its weight
// (1 in-house), or returns nil. Interactive dequeues under the fairness
// mode. The external class dequeues the fairness pick (A03-W4,
// externalPickLocked): the head of the first bucket in fairness order that
// fits with its own weight (A03-W7) — a head that does not fit blocks only
// its own bucket (d1), never another one. Background additionally takes the
// FA aging escape (aging.go) when only the herald term refuses it — the scan
// reaches background only after every higher-ranked class yielded no
// admissible head, so the escape never overtakes a waiting admissible
// higher-ranked class, and it never takes units reserved for a waiting
// external head (d2). A class without a case is never picked (fail-closed
// default).
func (d *Dispatcher) pickAdmissibleLocked(st *targetState, c Class, mode FairnessMode, slots int) *waiter {
	switch c {
	case ClassInteractive:
		if !d.admissibleLocked(st, c, 1, slots) {
			return nil
		}
		return st.interactive.pick(mode)
	case ClassBackground:
		if d.admissibleLocked(st, c, 1, slots) {
			return st.background.pop()
		}
		now := time.Now()
		if st.held+1 > slots || d.externalReservedLocked(st, 1, slots) || !d.agingEscapeLocked(st, now) {
			return nil
		}
		w := st.background.pop()
		if w != nil {
			d.agedAdmitLocked(st, w, now)
		}
		return w
	case ClassExternal:
		w, key := d.externalPickLocked(st, slots)
		switch {
		case w == nil:
			return nil
		case w.cont:
			return st.external.popCont()
		}
		return st.external.popHead(key)
	default:
		d.logger.Error("dispatch: class without a wake case — not picked (fail-closed default)",
			"target", st.origin, "class", uint8(c))
		return nil
	}
}

// headAdmissibleLocked reports whether class c has a queued head that is
// admissible with its own weight (A03-W7): the head the wake would pick next.
// For the external class that is the fairness pick itself
// (externalPickLocked, A03-W4) — the very head the wake dequeues, so the rank
// scan of a newcomer and the wake never look at two different heads; an
// in-house head always weighs 1. False with an empty queue.
func (d *Dispatcher) headAdmissibleLocked(st *targetState, c Class, slots int) bool {
	if !d.queuedLocked(st, c) {
		return false
	}
	if c == ClassExternal {
		w, _ := d.externalPickLocked(st, slots)
		return w != nil
	}
	return d.admissibleLocked(st, c, 1, slots)
}

// externalAdmissibleLocked is the admission term of the external class
// (design/03 §4.2/§4.3): never on a target that does not offer it; on a
// slot-managed target held + w ≤ S ∧ heldExt + w ≤ E with E = S − R (the
// internal partition: ctx keeps R slots whatever the external load); on a
// self-admitting target (no slots) heldExt + w ≤ external_max_inflight, or
// always without that cap — the remote admission acts. No herald term
// (design/03 §4.8a): external work occupies only the external partition, so
// it cannot fill the inter-lease gaps of an interactive request.
func (d *Dispatcher) externalAdmissibleLocked(st *targetState, w, slots int) bool {
	offered, capacity := d.externalOffer(st.origin)
	if !offered {
		return false
	}
	if slots > 0 && st.held+w > slots {
		return false
	}
	return capacity == 0 || st.heldExt+w <= capacity
}

// externalOffer resolves whether a target offers the external class
// right now and its external capacity E (0 on an offered self-admitting
// target = no external cap): never with the emergency stop, else the derived
// policy (TargetPolicy.externalCapacity). Two atomic loads, no mutex — the
// admission path calls it under Dispatcher.mu, ExternalAdmissible without.
func (d *Dispatcher) externalOffer(origin string) (offered bool, capacity int) {
	if !d.currentSettings().Enabled {
		return false, 0
	}
	return d.policy.Load().Targets[origin].externalCapacity()
}
