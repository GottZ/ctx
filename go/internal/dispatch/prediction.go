package dispatch

import (
	"math"
	"time"
)

// Prediction, relational cap and Retry-After of the external class (wave
// A03-W6, design/03 §4.9, Findings 17/18/20, E-03-13; hardened after review
// 2). All three read the PUBLISHED lease-duration aggregate of the target's
// external class (metrics.go — never a sort under the dispatcher mutex) and
// the own position of the acquire, kept O(1) by the fairness bookkeeping
// (fairness.go: waiting and waitW per consumer, waitingConsumers and waitFW
// per tenant, waitingTenants per queue):
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
//   - ŵ = r̂ + (rounds − 1) · L_p50 with rounds = ⌈units / E_id⌉ ≥ 1: the
//     first freed unit comes after the REMAINING time of the held leases, r̂ =
//     max(0, L_p50 − the age of the oldest held external lease) — 0 when the
//     class holds none — and every further round after a full L_p50 (review 2
//     F-2 (b): a full first round put ŵ ≥ L_p50, so with L_p50 above the
//     budget no external acquire ever queued, even one second before a lease
//     ended). The fairness-first head waits its first round only: a queued
//     acquire that is the head externalFirst picks now gets rounds = 1, ŵ = r̂
//     (review 2 F-2 (a): the slot-time pick serves a fresh or light tenant at
//     the very next release, the share model would have refused it).
//   - prediction (Finding 17), after the push (review 2 F-1): an acquire
//     that its own enqueue makes the pick — the fairness-first head that
//     fits a free unit — is never refused, its enqueue wake admits it; any
//     other with ŵ above its MaxWait_eff leaves the queue again in the same
//     mutex section BEFORE the wake runs, ErrWaitExpired (reason predicted) —
//     no timer, no goroutine parked for a 429, and no wake acting on the
//     shares of a request that never waits (review 3 C1: such a phantom
//     wake admitted another head past the head that holds the class).
//   - relational cap: R = ⌈k · E_eff · external_max_wait / L_p50⌉, the
//     waiters a whole wait budget can serve (k = 1.5 headroom), apportioned
//     like the tenant shares: the own tenant may hold at most ⌈R / T_w⌉
//     waiting acquires — a tenant alone gets all of R; tenants that queue one
//     after another keep the places they took, so Σ waiting external acquires
//     ≤ min(external_queue_max, Σₖ ⌈R / k⌉) over the arrivals k = 1, 2, …
//     (review 2 F-3; Σ ≤ R + T_w holds only for simultaneous arrival).
//     external_queue_max is the absolute ceiling and stays the only aggregate
//     one (lead decision: the isolation of a late tenant ranks above an
//     aggregate cap at R); one tenant never fills the queue for all (review 1
//     F-1 (c): the base's tenant cap 32 < aggregate cap 64 left room for a
//     second tenant).
//   - Retry-After (E-03-13, X11): Q(ŵ) of the refused acquire's own position,
//     never the class-wide depth nor other tenants' consumer counts — the
//     number of waiting tenants and, through r̂, the age of the oldest held
//     external lease enter; Q rounds up onto Settings.ExternalRetryAfterSteps.
//     An expired waiter's Retry-After is floored at the time it waited
//     (review 2 F-4: a stale short window predicts ŵ ≈ 0 for the waiter that
//     just waited its budget out).
//
// The model's limits, both sides named: the slot-time order of the pick
// beyond its head is not modelled — a light or fresh tenant in second place
// can be overestimated and refused (fail-closed), a heavy tenant whose leases
// hold much of E underestimated (fail-open, the wait timer bounds it); with
// every held lease fresh and L_p50 above MaxWait_eff, r̂ ≈ L_p50 still refuses
// at once (fail-closed). r̂ is a proxy, not an oracle, on both sides: one
// held lease past L_p50 (a long stream) makes the first round free, r̂ = 0,
// however fresh the others are, and the fairness-first head of weight w > 1
// waits only for the first freed unit although it needs w (fail-open by at
// most a round, the timer bounds it). The Retry-After of a refused
// fairness-first head still counts its full rounds (fail-closed: a later
// retry). The refusal itself is O(1) to decide (O(E) for r̂, one pass over
// the bucket heads for the pick and the fairness-first head beyond the
// rounds bound); the push and its removal maintain the queue like any
// enqueue and cancel, O(B) in the B waiting buckets (B ≤
// external_queue_max). design §4.9's flat ⌈(q+1) · n_w / E⌉ · L_p50 is the
// build's deviation 1 (reports/bau/A03-W6.md).
//
// Cold window (fewer than leaseColdSamples published samples): no
// prediction, no relational cap, ŝ = 0, and the Retry-After is the class
// formula of the external class, quantized. A target without an external cap
// (a self-admitting target without external_max_inflight — no E) has no
// prediction and no relational cap either, and its Retry-After is the class
// formula; ŝ applies on every warm window, E or not (waitbudget.go).

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

// externalPositionLocked is the own position of the newcomer w (not queued):
// two record lookups, O(1).
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

// queuedPositionLocked is the own position of the queued w — the same counts
// as its newcomer position, read from the records it entered (waiterEnter):
// its consumer's waiting Σw (w's own included; every other waiter of the
// consumer is ahead of the newest), T_w, its tenant's waiting Σfw (the
// consumer at the record's fair weight, which enter set to w's), the
// record's fair weight. O(1).
func queuedPositionLocked(st *targetState, w *waiter) position {
	c := w.fair
	return position{units: c.waitW, tenants: st.external.fair.waitingTenants, sumFW: c.tenant.waitFW, fw: c.fw}
}

// rounds is ⌈units / E_id⌉ ≥ 1 with E_id = capacity · fw / (tenants · Σfw):
// the rounds of freed units the identity's work needs at its share. In
// float64 the products of counts are exact far beyond any reachable queue,
// and the quotient of exact integers rounds correctly, so the ceiling is the
// integer ceiling.
func (p position) rounds(capacity int) float64 {
	num := float64(p.units) * float64(max(p.tenants, 1)) * float64(max(p.sumFW, 1))
	return max(math.Ceil(num/(float64(max(capacity, 1))*float64(max(p.fw, 1)))), 1)
}

// wait is ŵ = first + (rounds − 1) · perRound of the position.
func (p position) wait(capacity int, first, perRound time.Duration) time.Duration {
	return roundsWait(p.rounds(capacity), first, perRound)
}

// roundsWait is first + (rounds − 1) · perRound: the first round after the
// remaining time first, every further one after a full perRound; saturating
// at maxDuration.
func roundsWait(rounds float64, first, perRound time.Duration) time.Duration {
	if est := float64(first) + (rounds-1)*float64(perRound); est < float64(maxDuration) {
		return time.Duration(est)
	}
	return maxDuration
}

// firstRoundLocked is r̂ of the target's external class (review 2 F-2 (b)):
// max(0, p50 − the age of the oldest held external lease) — the remaining
// time until the first freed unit, read off the lease that has run longest
// (at most p50: a lease is admitted before now, under this mutex or an
// earlier section of it) — and 0 when the class holds no lease: its units are free,
// the acquire waits only for the in-house classes or the pick. One pass over
// the held external leases (extCancel holds exactly them, at most E_eff of
// them; their cancel funcs are never called here) — run only where the
// rounds bound alone refuses (predictAfterPushLocked) and for the
// Retry-After of a rejection.
func (st *targetState) firstRoundLocked(p50 time.Duration, now time.Time) time.Duration {
	var oldest time.Time
	for l := range st.extCancel {
		if oldest.IsZero() || l.admitted.Before(oldest) {
			oldest = l.admitted
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return max(p50-now.Sub(oldest), 0)
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
// (E-03-13): Q(ŵ) of its own position as a newcomer, ŵ = r̂ + (rounds − 1) ·
// L_p50, on a warm window of a target with an external cap, else the class
// formula of the external class (depth × published wait p95 — the cold rule
// of §4.9) — never below floor (the waited time of an expired waiter, review
// 2 F-4; 0 for every other reason), quantized onto
// Settings.ExternalRetryAfterSteps. Lock-free reads, O(1) under the held
// mutex plus the pass over the held external leases (r̂); for an expired
// waiter, called after it left its queue.
func (d *Dispatcher) externalRetryAfterLocked(st *targetState, w *waiter, floor time.Duration) time.Duration {
	steps := d.currentSettings().ExternalRetryAfterSteps
	var est time.Duration
	if lv, capacity := st.externalLeaseView(), d.boundedExternalCapacity(st.origin); lv.warm && capacity > 0 {
		est = externalPositionLocked(st, w).wait(capacity, st.firstRoundLocked(lv.p50, time.Now()), lv.p50)
	} else {
		est = classEstimate(st.external.total, st.ring(ClassExternal).published().p95)
	}
	return quantizeRetryAfter(max(est, floor), steps)
}

// externalRejectLocked is the error of an external rejection: the sentinel
// wrapped with the Retry-After of w's own position (RejectionError), floored
// at floor.
func (d *Dispatcher) externalRejectLocked(st *targetState, w *waiter, sentinel error, floor time.Duration) error {
	return &RejectionError{Err: sentinel, RetryAfter: d.externalRetryAfterLocked(st, w, floor)}
}

// enqueueExternalLocked queues an external waiter that is no continuation
// (A03-W6): the absolute cap ladder consumer → tenant → aggregate first — the
// most specific offender first, a refusal counts Cap — then, on a warm window
// of a target with an external cap, the relational cap (w's tenant at its
// share ⌈R / T_w⌉ ⇒ ErrTargetSaturated, Cap). Every refusal carries the
// Retry-After of w's own position. An acquire that passes both is queued;
// the prediction follows the push, before the enqueue wake
// (predictAfterPushLocked).
func (d *Dispatcher) enqueueExternalLocked(st *targetState, w *waiter, set Settings) error {
	q := st.external
	if err := capLadder(q, w, set.ExternalQueuePerConsumer, set.ExternalQueuePerTenant, set.ExternalQueueMax); err != nil {
		st.extRejects.Cap++
		return d.externalRejectLocked(st, w, err, 0)
	}
	if lv, capacity := st.externalLeaseView(), d.boundedExternalCapacity(st.origin); lv.warm && capacity > 0 {
		tenants := max(externalPositionLocked(st, w).tenants, 1)
		if r := relationalQueueCap(capacity, set.ExternalMaxWait, lv.p50); r > 0 && q.byTenant[w.principal.TenantID] >= (r+tenants-1)/tenants {
			st.extRejects.Cap++
			return d.externalRejectLocked(st, w, ErrTargetSaturated, 0)
		}
	}
	pushLocked(st, q, w)
	return nil
}

// predictAfterPushLocked is the prediction (Finding 17) of the external
// acquire w right after its push, before the enqueue wake (review 2 F-1,
// review 3 C1). The wake may make w the fairness-first head and admit it into
// a free unit — a prediction before the push refused exactly the request that
// could use it — so w is never refused while it is the waiter the wake would
// hand units to (externalPickLocked). And the wake acts on the shares w's push
// changed (T_act, Σ fair_weight): run for a w that is then refused, it could
// admit another head past the head that holds the class — a phantom event of
// a request that never waits (review 3 C1). So the decision comes first and
// the wake follows only for a w that stays (Acquire). The pick and the
// fairness-first head are read on the order the wake would read, at now
// (review 3 delta D-1); a refusal restores the view of the last event. Only a queued w that is
// no continuation (A03-W17), on a warm window of a target with an external
// cap: w stays when ŵ = r̂ + (rounds − 1) · L_p50 ≤ budget, when it is the
// pick, or when r̂ ≤ budget and it is the fairness-first head (rounds = 1,
// review 2 F-2 (a)). Otherwise it leaves the queue again without a wake — the
// queue is back to the state before its push, which the last wake left
// without an admissible head — and no timer is armed (Acquire arms it only
// after a nil error), no goroutine parks: counted Predicted, answered
// ErrWaitExpired with the Retry-After Q(ŵ) of the comparison. O(1) while the
// rounds bound fits (r̂ ≤ L_p50); beyond it one pass over the held external
// leases (r̂), the pick (O(1) while not even the lightest head fits, else one
// or two passes over the bucket heads) and one pass for the fairness-first
// head, each only when the terms before it did not decide.
func (d *Dispatcher) predictAfterPushLocked(st *targetState, w *waiter, budget time.Duration) error {
	lv, capacity := st.externalLeaseView(), d.boundedExternalCapacity(st.origin)
	if w.cont || !lv.warm || capacity <= 0 {
		return nil
	}
	rounds := queuedPositionLocked(st, w).rounds(capacity)
	if roundsWait(rounds, lv.p50, lv.p50) <= budget {
		return nil // r̂ ≤ L_p50: within the rounds bound, ŵ fits as well
	}
	first := st.firstRoundLocked(lv.p50, time.Now())
	est := roundsWait(rounds, first, lv.p50)
	if est <= budget {
		return nil
	}
	// The pick and the fairness-first head on the order the enqueue wake
	// reads — at now, not at the last event (review 3 delta D-1: an arrival
	// is an event, its wake re-reads the order first) — but a refusal does
	// not persist that view: it is no event of the order (FR4-2).
	f := st.external.fair
	viewAt := f.viewAt
	f.touch(d.fairClock(time.Now()))
	if pick, _ := d.externalPickLocked(st, d.slotsLocked(st.origin)); pick == w {
		return nil // the wake admits w now
	}
	if first <= budget && d.externalFairnessFirstLocked(st, w) {
		return nil
	}
	f.viewAt = viewAt
	st.external.remove(w) // no wake: nothing the push changed may act (C1)
	st.extRejects.Predicted++
	return &RejectionError{Err: ErrWaitExpired, RetryAfter: quantizeRetryAfter(est, d.currentSettings().ExternalRetryAfterSteps)}
}

// externalFairnessFirstLocked reports whether the queued w is the
// fairness-first head of the external class — the bucket head externalFirst
// picks: no continuation waits (that one is the class head), w heads its own
// bucket, and no other bucket head ranks before it. O(1) for the first two
// terms, one pass over the bucket heads for the third.
func (d *Dispatcher) externalFairnessFirstLocked(st *targetState, w *waiter) bool {
	q := st.external
	if len(q.cont) > 0 {
		return false
	}
	if b := q.buckets[q.bucketKey(w.principal)]; len(b) == 0 || b[0] != w {
		return false
	}
	_, capacity := d.externalOffer(st.origin)
	first, _, _ := externalFirst(st, d.fairViewLocked(q.fair, capacity), d.slotsLocked(st.origin), capacity)
	return first == w
}
