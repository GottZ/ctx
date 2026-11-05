package dispatch

import (
	"math"
	"time"
)

// Prediction, relational cap and Retry-After of the external class (wave
// A03-W6, design/03 §4.9, Findings 17/18/20, E-03-13). All three read the
// PUBLISHED lease-duration aggregate of the target's external class
// (metrics.go — never a sort under the dispatcher mutex) and the own position
// of the acquire, kept O(1) by the fairness bookkeeping (fairness.go: waiting
// and waitW per consumer, waitingConsumers and waitFW per tenant,
// waitingTenants per queue):
//
//   - own position: units = Σ the weights of the identity's waiting acquires
//     (continuations included — they are served first) plus the newcomer's
//     weight w (lead decision 1: weight is the unit of the partition since
//     A03-W7, §4.7, and of heldBy, A03-W4; design §4.9 writes q_id + 1).
//   - own share of E_eff, the two levels of the A03-W4 pick (tenant first,
//     then consumer by fair weight) over the WAITING demand: E_id = E_eff ·
//     (1 / T_w) · (fw / Σfw), T_w = the tenants with a waiting acquire, Σfw =
//     the fair weights of the waiting consumers of the own tenant — the own
//     tenant and consumer counted, with the newcomer's fair weight. Only
//     waiting identities compete for freed units (work conservation: a lease
//     holder without a waiter claims none). design §4.9 writes E_eff / n_w over
//     all identities; the flat count contradicts the two-level pick (review 1
//     F-1: a tenant's eight waiting consumers made a lone consumer of another
//     tenant predicted at 25 s while the pick served it at the second
//     release) and would let consumer minting shed other tenants (X15). For
//     one tenant with equal fair weights both forms coincide.
//   - ŵ = ⌈units / E_id⌉ · L_p50: the rounds of L_p50 the identity's work
//     needs at its share.
//   - prediction (Finding 17): an external acquire that would have to wait is
//     refused at once with ErrWaitExpired (reason predicted) when ŵ exceeds
//     its MaxWait_eff — no wait slot, no timer, no goroutine parked for a 429.
//   - relational cap: R = ⌈k · E_eff · external_max_wait / L_p50⌉, the
//     waiters a whole wait budget can serve (k = 1.5 headroom), apportioned
//     like the tenant shares: the own tenant may hold at most ⌈R / T_w⌉
//     waiting acquires (a tenant alone gets all of R; Σ ≤ R + T_w). Next to
//     the absolute external_queue_max (the memory ceiling) it bounds the
//     queue where the per-identity prediction alone overfills (identities
//     that queued while they were alone keep their places as others join),
//     and one tenant never fills it for all (review 1 F-1 (c): the base's
//     tenant cap 32 < aggregate cap 64 left room for a second tenant).
//   - Retry-After (E-03-13, X11): Q(ŵ) of the refused acquire's own position,
//     never the class-wide depth nor other tenants' consumer counts — only
//     the number of waiting tenants enters; Q rounds up onto
//     Settings.ExternalRetryAfterSteps.
//
// Cold window (fewer than leaseColdSamples published samples) and targets
// without an external cap (a self-admitting target without
// external_max_inflight — no E): no prediction, no relational cap, ŝ = 0,
// and the Retry-After is the class formula of the external class, quantized.

const (
	// relationalCapFactor is the shed factor k of the relational aggregate
	// cap (design/03 §3.3: a mechanism constant, hardcoded-legitimate).
	relationalCapFactor = 1.5
	// leaseColdSamples is the cold-start threshold of the lease-duration
	// window (design/03 §3.3: a mechanism constant): below it there is no
	// prediction, no relational cap and ŝ = 0.
	leaseColdSamples = 32
)

// leaseView is the published lease-duration view of one target's external
// class: p50 (L_p50, the round of the prediction) and p90 (ŝ, the predicted
// lease duration of the ServeBy term); warm only from leaseColdSamples
// published samples on and with p50 > 0. The zero value is cold.
type leaseView struct {
	p50, p90 time.Duration
	warm     bool
}

// externalLeaseView reads the published lease-duration aggregate of the
// external class — lock-free, allocation-free (an atomic load).
func (st *targetState) externalLeaseView() leaseView {
	a := st.leaseRing(ClassExternal).published()
	if a.samples < leaseColdSamples || a.p50 <= 0 {
		return leaseView{}
	}
	return leaseView{p50: a.p50, p90: a.p90, warm: true}
}

// boundedExternalCapacity is E_eff of origin when the external class has a
// cap there (S − R on a slot-managed target, external_max_inflight on a
// self-admitting one), else 0 — not offered, or offered without an external
// cap: no prediction and no relational term (design/03 §4.9: E_eff
// unbounded).
func (d *Dispatcher) boundedExternalCapacity(origin string) int {
	offered, capacity := d.externalOffer(origin)
	if !offered {
		return 0
	}
	return capacity
}

// position is the own position of an external acquire in its class.
type position struct {
	// units is w's weight plus the weights of its identity's waiting acquires.
	units int
	// tenants is T_w, the tenants with a waiting acquire, w's own counted.
	tenants int
	// sumFW is Σfw of the waiting consumers of w's tenant with w's consumer
	// counted at w's fair weight; fw is w's fair weight.
	sumFW, fw int
}

// externalPositionLocked is the own position of w: two record lookups, O(1).
func externalPositionLocked(st *targetState, w *waiter) position {
	f := st.external.fair
	p := position{units: w.weight, tenants: f.waitingTenants, fw: fairWeightOf(w.principal)}
	p.sumFW = p.fw
	var t *fairTenant
	if c := f.consumers[w.principal.identityKey()]; c != nil {
		t = c.tenant
		if c.waiting > 0 {
			p.units += c.waitW
			p.sumFW -= c.fw // counted below in waitFW, at the newcomer's weight instead
		}
	} else {
		t = f.tenants[w.principal.TenantID]
	}
	if t != nil && t.waitingConsumers > 0 {
		p.sumFW += t.waitFW
	} else {
		p.tenants++
	}
	return p
}

// wait is ŵ = ⌈units / E_id⌉ · perRound with E_id = capacity · fw /
// (tenants · Σfw), saturating at maxDuration. In float64 the products of
// counts are exact far beyond any reachable queue, and the quotient of exact
// integers rounds correctly, so the ceiling is the integer ceiling.
func (p position) wait(capacity int, perRound time.Duration) time.Duration {
	num := float64(p.units) * float64(max(p.tenants, 1)) * float64(max(p.sumFW, 1))
	rounds := math.Ceil(num / (float64(max(capacity, 1)) * float64(max(p.fw, 1))))
	if est := rounds * float64(perRound); est < float64(maxDuration) {
		return time.Duration(est)
	}
	return maxDuration
}

// relationalQueueCap is R = ⌈k · E_eff · maxWait / L_p50⌉ (≥ 1, bounded by
// maxLimitCount, a representation bound), 0 without a positive wait budget —
// the external class never waits then.
func relationalQueueCap(capacity int, maxWait, p50 time.Duration) int {
	if maxWait <= 0 || p50 <= 0 {
		return 0
	}
	if r := math.Ceil(relationalCapFactor * float64(capacity) * float64(maxWait) / float64(p50)); r < float64(maxLimitCount) {
		return int(r)
	}
	return maxLimitCount
}

// externalRetryAfterLocked is the Retry-After of an external rejection of w
// (E-03-13): Q(ŵ) of its own position on a warm window of a target with an
// external cap, else the class formula of the external class (depth ×
// published wait p95 — the cold rule of §4.9), quantized onto
// Settings.ExternalRetryAfterSteps. Lock-free reads and O(1) under the held
// mutex; for an expired waiter, called after it left its queue.
func (d *Dispatcher) externalRetryAfterLocked(st *targetState, w *waiter) time.Duration {
	steps := d.currentSettings().ExternalRetryAfterSteps
	lv := st.externalLeaseView()
	if capacity := d.boundedExternalCapacity(st.origin); lv.warm && capacity > 0 {
		return quantizeRetryAfter(externalPositionLocked(st, w).wait(capacity, lv.p50), steps)
	}
	return quantizeRetryAfter(classEstimate(st.external.total, st.ring(ClassExternal).published().p95), steps)
}

// externalRejectLocked is the error of an external rejection: the sentinel
// wrapped with the Retry-After of w's own position (RejectionError).
func (d *Dispatcher) externalRejectLocked(st *targetState, w *waiter, sentinel error) error {
	return &RejectionError{Err: sentinel, RetryAfter: d.externalRetryAfterLocked(st, w)}
}

// enqueueExternalLocked queues an external waiter that is no continuation
// (A03-W6): the absolute cap ladder consumer → tenant → aggregate first — the
// most specific offender first, a refusal counts Cap — then, on a warm window
// of a target with an external cap, the relational cap (w's tenant at its
// share ⌈R / T_w⌉ ⇒ ErrTargetSaturated, Cap) and the prediction against the
// acquire's budget (ŵ > MaxWait_eff ⇒ ErrWaitExpired, reason predicted).
// Every refusal carries the Retry-After of w's own position. Only an acquire
// that passes all of them is queued.
func (d *Dispatcher) enqueueExternalLocked(st *targetState, w *waiter, set Settings, budget time.Duration) error {
	q := st.external
	if err := capLadder(q, w, set.ExternalQueuePerConsumer, set.ExternalQueuePerTenant, set.ExternalQueueMax); err != nil {
		st.extRejects.Cap++
		return d.externalRejectLocked(st, w, err)
	}
	if lv, capacity := st.externalLeaseView(), d.boundedExternalCapacity(st.origin); lv.warm && capacity > 0 {
		pos := externalPositionLocked(st, w)
		if r := relationalQueueCap(capacity, set.ExternalMaxWait, lv.p50); r > 0 && q.byTenant[w.principal.TenantID] >= (r+max(pos.tenants, 1)-1)/max(pos.tenants, 1) {
			st.extRejects.Cap++
			return d.externalRejectLocked(st, w, ErrTargetSaturated)
		}
		if pos.wait(capacity, lv.p50) > budget {
			st.extRejects.Predicted++
			return d.externalRejectLocked(st, w, ErrWaitExpired)
		}
	}
	pushLocked(st, q, w)
	return nil
}
