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
// exactly the pre-A03-W2 literals: interactive before background.

package dispatch

import (
	"fmt"
	"time"
)

// defaultRankOrder is the rank table in effect for the zero value and for
// every invalid order: interactive before background (E-U1 — a waiting human
// is always served first). The config key dispatch.external_order and the
// named orders o1/o2 arrive with the first external class (A03-W3).
var defaultRankOrder = [...]Class{ClassInteractive, ClassBackground}

// Compile-time guard: the default order names exactly numClasses classes. A
// class added before numClasses without its rank here makes one of these
// array lengths negative, so the build fails instead of padding the default
// order with a zero value (a second ClassInteractive).
var (
	_ [len(defaultRankOrder) - int(numClasses)]struct{}
	_ [int(numClasses) - len(defaultRankOrder)]struct{}
)

// DefaultRankOrder returns the default rank table (interactive, background) —
// the value the owner side pins Settings.RankOrder to until its config key
// lands (A03-W3).
func DefaultRankOrder() [numClasses]Class { return defaultRankOrder }

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
// running interactive requests, R3, not softenable here). Callers pass
// w = 1; the real weight source is A03-W7. A class without a case is never
// admissible (fail-closed default, A03-W1 pattern).
func (d *Dispatcher) admissibleLocked(st *targetState, c Class, w, slots int) bool {
	if st.held+w > slots {
		return false
	}
	switch c {
	case ClassInteractive:
		return true
	case ClassBackground:
		return d.demand.Load() == 0
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
	default:
		d.logger.Error("dispatch: class without a queue case — no queue (fail-closed default)",
			"target", st.origin, "class", uint8(c))
		return false
	}
}

// pickAdmissibleLocked is the wake half of the rank scan for ONE class:
// it dequeues the head of c when that head is admissible (weight 1), or
// returns nil. Interactive dequeues under the fairness mode. Background
// additionally takes the FA aging escape (aging.go) when only the herald
// term refuses it — the scan reaches background only after every
// higher-ranked class yielded no admissible head, so the escape never
// overtakes a waiting admissible higher-ranked class. A class without a case
// is never picked (fail-closed default).
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
		if st.held+1 > slots || !d.agingEscapeLocked(st, now) {
			return nil
		}
		w := st.background.pop()
		if w != nil {
			d.agedAdmitLocked(st, w, now)
		}
		return w
	default:
		d.logger.Error("dispatch: class without a wake case — not picked (fail-closed default)",
			"target", st.origin, "class", uint8(c))
		return nil
	}
}
