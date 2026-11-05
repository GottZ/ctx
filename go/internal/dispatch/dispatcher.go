package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// interactiveWaitWarn is the WARN threshold for interactive wait time
// (design/01 §4.4 W1 visibility — the protected good needs a measurable
// BEFORE the observability wave). Held per Dispatcher (warnAfter) so tests
// can shrink it without a global write race.
const interactiveWaitWarn = 5 * time.Second

// reapInterval is the reaper's lazy cadence. The reaper is a leak net, not a
// scheduler — precision comes from lease_reap_grace, not the tick.
const reapInterval = 5 * time.Second

// Lease is one admitted (or passed-through) wire-call permit. Release is
// idempotent and `defer lease.Release()` is the only allowed call form at
// the call sites (review gate B1).
type Lease struct {
	d         *Dispatcher
	st        *targetState
	class     Class
	principal Principal
	role      string
	enqueued  time.Time
	admitted  time.Time
	// reapRef is the reap reference (earlier of deadline hint and ctx
	// deadline); zero = admitted + lease_max_age fallback.
	reapRef time.Time
	cancel  context.CancelCauseFunc
	// slotHeld is false for pass-through leases (disabled / empty policy /
	// external targets): tracked in the registry for telemetry, but they
	// never occupy a slot and the dispatcher never cancels them (behavior
	// neutrality before activation).
	slotHeld bool
	// weight is the number of slot units a slot-holding lease holds (A03-W7,
	// design/03 §4.7): its Request.Weight for the external class, 1 for the
	// in-house classes (requestWeight).
	weight int
	// done guards single release (Dispatcher.mu). Both Release and the
	// reaper settle a lease through finishLocked exactly once.
	done bool
	// usage is the backend usage reported via ReportUsage before Release;
	// nil at settle time counts as uncharged (MW22/C1, usage.go).
	usage *Usage
	// agedAdmit marks a lease admitted via the aging escape (MW25,
	// aging.go): a preempt of it bumps aged_preempts — the waste metric
	// that is the NEGATIVE condition of the FA activation gate.
	agedAdmit bool
	// fair / slotStart are the external lease's fairness record and the
	// start of its slot time on the fairness clock (A03-W4, fairness.go).
	fair      *fairConsumer
	slotStart time.Time
}

// Release returns the slot and wakes the next waiter (strict class order,
// direct handoff). Idempotent: double release is a no-op.
func (l *Lease) Release() {
	l.d.mu.Lock()
	opStart := time.Now()
	l.d.finishLocked(l)
	l.d.opDone(opStart)
	l.d.mu.Unlock()
	// Free the derived runCtx resources — Release means the caller's wire
	// call has ended. For an already-reaped lease the cause is set; a
	// CancelCauseFunc ignores subsequent calls (first cause wins).
	l.cancel(context.Canceled)
}

// WaitDur is admitted − enqueued: the single source of the queue_wait_ms
// telemetry (observability wave).
func (l *Lease) WaitDur() time.Duration { return l.admitted.Sub(l.enqueued) }

// Class returns the admission class of the lease.
func (l *Lease) Class() Class { return l.class }

// targetState is the runtime state of one physical origin. All fields are
// guarded by Dispatcher.mu — ONE process-wide mutex section over map+FIFO
// operations, O(1)ish against wire latencies of seconds to minutes; per-target
// sharding would be stock complexity without a named problem (design/01 §6).
type targetState struct {
	origin string
	// seq is the monotone per-target arrival order (design/03 §4.2: seq, not
	// timestamp — no tie path).
	seq uint64
	// held is the sum of the weights (Σw, A03-W7, design/03 §4.7) of the
	// slot-holding leases (pass-through leases excluded) of ALL classes —
	// internal and external: the unit of every admission term, of the wake
	// scan and of the preempt need. heldExt is its external part (an
	// external lease always holds units of the external partition, it is
	// never pass-through); extLeases counts the external leases. An in-house
	// lease weighs 1, so held − heldExt is both the internal weight and the
	// number of internal slot-holding leases.
	held      int
	heldExt   int
	extLeases int
	// inflight is the registry of ALL leases incl. pass-through (telemetry,
	// divergence metric, reaper).
	inflight map[*Lease]struct{}
	// cancelable holds the cancel funcs of slot-holding BACKGROUND leases
	// only — interactive cancel funcs are never registered, so a dispatcher
	// cancel of interactive is structurally impossible, not just forbidden
	// (I-D1/B5).
	cancelable map[*Lease]context.CancelCauseFunc
	// extCancel holds the cancel funcs of the external leases — read ONLY by
	// the reaper (I-03-3: an external lease is never a preempt victim; the
	// leak net past admitted + external_lease_max is its only cancel path).
	// A map of its own, so pickVictimsLocked cannot reach it structurally.
	// Its keys are exactly the held external leases: the prediction reads
	// their admission instants for r̂ (A03-W6, prediction.go
	// firstRoundLocked), never a cancel func.
	extCancel   map[*Lease]context.CancelCauseFunc
	interactive *classQueue
	background  bgQueue
	// external is the wait queue of the external class (identity buckets
	// _consumer:<id>, its own cap ladder — A03-W3).
	external *classQueue
	// extRejects counts the external refusals per reason (design/03 §3.4:
	// no llmlog line per external rejection, a counter instead).
	extRejects ExternalRejectStats
	// usage is the per-fairness-bucket token meter (MW22/F2, usage.go).
	usage map[string]*usageWindow
	// preempt is the MW18 preempt-path bookkeeping (victims in eviction +
	// telemetry counters, design/02 §3.3) — see preempt.go.
	preempt preemptState
	// waits are the per-class rolling wait-sample windows (MW7/D3-W3), one
	// per class below numClasses, accessed ONLY through ring — see metrics.go.
	waits [numClasses]waitRing
	// leases are the per-class lease-duration windows (A03-W6, design/03
	// §4.9): finish − admitted of every lease end, accessed ONLY through
	// leaseRing — the published p50/p90 feed the external prediction.
	leases [numClasses]waitRing
}

// Dispatcher is the process-wide admission layer (I-D1: exactly one). Policy
// and settings are atomic snapshots, hot-swapped by the owner side on NOTIFY
// / config reload; swaps act on the NEXT operation, running leases stay
// untouched (attrition, never a reload cancel — B4).
type Dispatcher struct {
	logger *slog.Logger
	// warnAfter is the interactive wait WARN threshold (const default;
	// per-instance so tests can shrink it race-free).
	warnAfter time.Duration

	mu      sync.Mutex
	targets map[string]*targetState

	// demand counts interactive requests in the house (herald, §4.5): from
	// HTTP ingress to handler end, NOT per wire call — it closes the
	// inter-lease gaps of multi-lease requests (R3).
	demand atomic.Int64

	policy   atomic.Pointer[Policy]
	settings atomic.Pointer[Settings]

	reapsTotal      atomic.Int64
	classDowngrades atomic.Int64
	// invalidClass counts refusals with ErrInvalidClass (A03-W1) — every
	// one is a code bug, like a class downgrade.
	invalidClass atomic.Int64
	// classUnauthorized / principalConflicts count the principal-gate
	// refusals (W01-7, design/01 §4.5 I-K10): ErrClassUnauthorized and
	// ErrPrincipalConflict — every one a wiring defect, like a downgrade.
	classUnauthorized  atomic.Int64
	principalConflicts atomic.Int64
	// stats are the MW22 meter counters: uncharged_calls + the hot-path
	// measurement hooks (usage.go).
	stats usageStats

	// spellings maps a raw origin spelling a Retry-After hint was asked for
	// (the rejection mapping passes b.Host, A03-W17) to its target, so the
	// hint parses an origin once per spelling, not once per call — only
	// spellings of an existing target are kept (bounded by the backend rows'
	// hosts; targets are never deleted). Guarded by mu.
	spellings map[string]*targetState
	// pubMu serializes the wait publication passes (publishWaits, metrics.go)
	// and guards their reusable scratch; lock order pubMu → mu.
	pubMu    sync.Mutex
	pubBuf   []int64
	pubRings []*waitRing

	// stopLoops ends the reaper and the wait publisher; loops counts them so
	// Close returns only after both are gone (no ticker outlives Close).
	stopLoops chan struct{}
	stopOnce  sync.Once
	loops     sync.WaitGroup

	// fairNow is the test-only clock seam of the external fairness meter
	// (fairClock, A03-W4); nil in production. Guarded by mu.
	fairNow func() time.Time
}

// New constructs the dispatcher and starts its two goroutines: the reaper
// and the wait publisher (A03-W17, metrics.go). logger nil falls back to
// slog.Default().
func New(logger *slog.Logger, s Settings) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dispatcher{
		logger:    logger,
		warnAfter: interactiveWaitWarn,
		targets:   make(map[string]*targetState),
		spellings: make(map[string]*targetState),
		stopLoops: make(chan struct{}),
	}
	s.RankOrder = d.resolveRankOrder(s.RankOrder)
	s.ExternalRetryAfterSteps = d.resolveRetryAfterSteps(s.ExternalRetryAfterSteps)
	d.settings.Store(&s)
	d.policy.Store(&Policy{})
	d.loops.Add(1)
	go d.reapLoop()
	d.loops.Add(1)
	go d.publishLoop()
	return d
}

// Close stops the reaper and the wait publisher and returns once both have
// ended (process shutdown; waiters drain via their own ctx). Idempotent.
func (d *Dispatcher) Close() {
	d.stopOnce.Do(func() { close(d.stopLoops) })
	d.loops.Wait()
}

// UpdateSettings hot-swaps the config-derived knobs. A flip to
// Enabled=false drains every in-house waiter as pass-through immediately and
// ends every external waiter with ErrExternalNotOffered (B2: the emergency
// stop must never leave goroutines stuck behind a dead gate; X9: never an
// external pass-through). The
// rank table resolves here once per swap (rank.go: zero ⇒ default, invalid ⇒
// default + one WARN); the next wake reads it. The external Retry-After steps
// resolve the same way (resolveRetryAfterSteps).
func (d *Dispatcher) UpdateSettings(s Settings) {
	s.RankOrder = d.resolveRankOrder(s.RankOrder)
	s.ExternalRetryAfterSteps = d.resolveRetryAfterSteps(s.ExternalRetryAfterSteps)
	d.settings.Store(&s)
	d.wakeAll()
}

// UpdatePolicy hot-swaps the derived per-origin policy and wakes every
// target: grown slots admit more, a cleared policy (limits='{}') drains the
// wait queues as pass-through within NOTIFY latency (the W7 return path).
func (d *Dispatcher) UpdatePolicy(p Policy) {
	d.policy.Store(&p)
	d.wakeAll()
}

// InteractiveArrived registers one interactive request entering the house
// (WithScheduler mounts + HandleDaily — wave MW2). The returned done is
// idempotent and MUST run at handler end; when demand drops to zero the
// dispatcher re-evaluates background admission on every target (the
// flip-wakes-waitQ guarantee).
func (d *Dispatcher) InteractiveArrived() func() {
	d.demand.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			if d.demand.Add(-1) == 0 {
				d.wakeAll()
			}
		})
	}
}

// InteractiveDemand is the process-wide demand signal — the successor of the
// scheduler's removed query-demand counter, consumed by the signal-driven
// arms (guard/digest/overview/dream/audit) since A5-W0 (MW15).
func (d *Dispatcher) InteractiveDemand() int {
	return int(d.demand.Load())
}

// Acquire blocks until the request is admitted on its target, the caller ctx
// ends, or a Deckel rejects it — for an external class at most its wait
// budget (ErrWaitExpired, A03-W5, waitbudget.go). The returned runCtx derives from ctx; for
// background leases the dispatcher holds its cancel (preempt/reap), for
// interactive it never cancels (I-D1). Acquire errors are NOT attempts: no
// Classify, no health report, no llmlog line, terminal for the chain walk
// (design/01 §4.3). Check order at the door: ctx.Err(), then the class
// (ErrInvalidClass for a value outside the class table — never queued, never
// downgraded, A03-W1), then the principal gate (ErrPrincipalConflict,
// ErrClassUnauthorized — I-K10, W01-7), then the B8 class authorization.
func (d *Dispatcher) Acquire(ctx context.Context, req Request) (*Lease, context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err // canceled ctx never takes a wait slot
	}
	if !req.Class.valid() {
		return nil, nil, d.refuseInvalidClass("acquire", req.Class, req.Target.Origin, req.Role)
	}
	principal := principalFromContext(ctx)
	if err := d.refusePrincipal("acquire", principal, req); err != nil {
		return nil, nil, err
	}
	class := req.Class
	if class == ClassInteractive && principal.ApiKeyID == "" {
		// B8 class authorization, ctx-bound (design/03 §4.1.1): interactive
		// requires a principal derived from the request ctx — emptiness is
		// pinned on ApiKeyID != "", so a synthetic AuthResult with an empty
		// key id (the S9 finding, handler/chat.go RunQuery) downgrades too
		// instead of forming an anonymous "" bucket in the interactive FIFO.
		// Detached scheduler ctx (no AuthResult) are structurally background;
		// a violating call is downgraded fail-closed toward the protected
		// good, never rejected (the work itself is legitimate). Every
		// downgrade is a code bug: counted (class_downgrades, WARN > 0) and
		// logged at ERROR so it cannot drown in slog noise.
		class = ClassBackground
		d.classDowngrades.Add(1)
		d.logger.Error("dispatch: interactive acquire without ctx-bound principal downgraded to background",
			"target", req.Target.Origin, "role", req.Role)
	}
	now := time.Now()
	runCtx, cancel := context.WithCancelCause(ctx)
	w := &waiter{
		class:      class,
		principal:  principal,
		role:       req.Role,
		enqueued:   now,
		reapRef:    reapReference(req.Deadline, ctx),
		deadlineIn: req.DeadlineIn,
		weight:     requestWeight(class, req.Weight),
		cont:       req.Continuation && class.External(),
		runCtx:     runCtx,
		cancel:     cancel,
		admitCh:    make(chan struct{}),
	}

	d.mu.Lock()
	opStart := time.Now()
	st := d.targetLocked(canonicalOrigin(req.Target.Origin))
	l, err := d.admitNowLocked(st, w, now)
	if l != nil || err != nil {
		d.opDone(opStart)
		d.mu.Unlock()
		if err != nil {
			cancel(context.Canceled)
			return nil, nil, err
		}
		return l, runCtx, nil
	}
	// The wait budget (A03-W5, waitbudget.go): external only, resolved
	// before the enqueue — a budget already ≤ 0 is ErrWaitExpired at once; a
	// continuation has none (A03-W17). The external enqueue runs the cap
	// ladder and the relational cap (A03-W6, prediction.go).
	budget, err := d.waitBudgetLocked(st, w, req, now)
	if err == nil {
		err = d.enqueueLocked(st, w)
		if err == nil && class == ClassExternal {
			// The prediction against the budget runs first (A03-W6,
			// prediction.go): a predicted waiter leaves the queue again in
			// this section, before any wake and any timer — never one the
			// wake would admit (review 2 F-1). An external enqueue changes
			// T_act and Σ fair_weight and with them the shares: the
			// fairness-first head can change — the waiter itself may be it
			// now and fit (A03-W4 review 4 FR4-1). The wake hands the pick
			// its units (it may admit w itself; the wait below then
			// returns at once) — only for a waiter that stays (review 3 C1).
			if err = d.predictAfterPushLocked(st, w, budget, time.Now()); err == nil {
				d.wakeLocked(st)
			}
		}
	}
	if err == nil && class == ClassInteractive {
		// Preempt trigger (MW18, design/02 §4.2.1): only an interactive
		// acquire that has to WAIT may open a preempt on a preempt-enabled
		// target — never a background acquire, never the herald alone.
		d.maybePreemptLocked(st, d.slotsLocked(st.origin))
	}
	d.opDone(opStart)
	d.mu.Unlock()
	if err != nil {
		cancel(context.Canceled)
		return nil, nil, err
	}
	expiry, stopTimer := waitTimer(budget, now) // nil channel without a budget
	defer stopTimer()

	select {
	case <-w.admitCh:
		if w.err != nil {
			// Ended without an admission (A03-W3 waiter error channel): the
			// target stopped offering the external class.
			cancel(context.Canceled)
			return nil, nil, w.err
		}
		return w.lease, runCtx, nil
	case <-ctx.Done():
		d.mu.Lock()
		if w.admitted {
			// The direct handoff raced the cancel: the slot is already
			// ours — settle it properly so the next waiter wakes.
			d.finishLocked(w.lease)
		} else if w.err == nil {
			d.dropWaiterLocked(st, w)
		}
		d.mu.Unlock()
		cancel(context.Canceled)
		return nil, nil, ctx.Err()
	case <-expiry:
		lease, werr := d.expireWaiter(st, w)
		if werr != nil {
			cancel(context.Canceled)
			return nil, nil, werr
		}
		return lease, runCtx, nil
	}
}

// eligibleNowLocked is the immediate-admission half of the rank scan
// (design/03 §4.2, rank.go): class is admissible with the newcomer's weight
// w AND its own queue is empty (FIFO, no barging — a newcomer never
// overtakes a waiter of its own class; bucket order is policy) AND every
// class ranked above it in Settings.RankOrder has an empty queue or a head
// that is inadmissible with ITS weight (rankClearLocked). With the
// default order this is exactly the pre-A03-W2 predicate: interactive = slot
// free AND no waiting interactive; background = slot free AND both wait
// queues empty AND demand == 0 (the herald term, R3). A class outside the
// class table is never eligible (fail-closed default in admissibleLocked,
// A03-W1; pre-A03-W1 it got the background predicate, K6); a class absent
// from the rank table — reachable only through the unvalidated test hook —
// is never eligible either. A continuation newcomer has its own own-queue
// term (continuationEligibleNowLocked); an external newcomer takes
// externalEligibleNowLocked (A03-W4: its own bucket, not its class, must be
// empty — d1).
func (d *Dispatcher) eligibleNowLocked(st *targetState, class Class, w, slots int) bool {
	if !d.admissibleLocked(st, class, w, slots) || d.queuedLocked(st, class) {
		return false
	}
	return d.rankClearLocked(st, class, slots)
}

// continuationEligibleNowLocked is eligibleNowLocked for a continuation of
// weight w (A03-W17): it is the head of the external class, so only a
// waiting continuation is ahead of it — the waiting non-continuations are
// behind it, whatever their bucket. Partition and rank order apply
// unchanged: admissible within E and S, and no higher-ranked class with a
// waiting, admissible head.
func (d *Dispatcher) continuationEligibleNowLocked(st *targetState, w, slots int) bool {
	if !d.admissibleLocked(st, ClassExternal, w, slots) || len(st.external.cont) > 0 {
		return false
	}
	return d.rankClearLocked(st, ClassExternal, slots)
}

// rankClearLocked reports whether no class ranked above class in
// Settings.RankOrder has a waiting head that is admissible with its weight
// (headAdmissibleLocked, A03-W7) — false for a class absent from the table.
func (d *Dispatcher) rankClearLocked(st *targetState, class Class, slots int) bool {
	for _, h := range d.currentSettings().RankOrder {
		if h == class {
			return true
		}
		if d.headAdmissibleLocked(st, h, slots) {
			return false // a waiting, admissible higher-ranked head goes first
		}
	}
	return false
}

// enqueueLocked places w in its class queue or rejects it fast. Check order
// of the Deckel-Staffel: principal → tenant → aggregate (the most specific
// offender first, D3-U4/E-U6). The external class runs the same ladder over
// its own queue and its own caps (consumer → tenant → aggregate, A03-W3; the
// aggregate cap relational since A03-W6, enqueueExternalLocked; the
// prediction follows the push in Acquire). A continuation (A03-W17)
// skips both: it goes to the class head and is never refused — it still
// counts in the depth the ladder reads.
func (d *Dispatcher) enqueueLocked(st *targetState, w *waiter) error {
	set := d.currentSettings()
	switch w.class {
	case ClassInteractive:
		return d.pushCappedLocked(st, st.interactive, w,
			set.InteractiveQueuePerPrincipal, set.InteractiveQueuePerTenant, set.InteractiveQueueMax)
	case ClassExternal:
		if w.cont {
			st.seq++
			w.seq = st.seq
			st.external.pushCont(w)
			return nil
		}
		return d.enqueueExternalLocked(st, w, set)
	case ClassBackground:
		if set.BackgroundQueueMax > 0 && st.background.len() >= set.BackgroundQueueMax {
			return ErrQueueFull
		}
		st.seq++
		w.seq = st.seq
		st.background.push(w)
		return nil
	default:
		// Fail-closed (A03-W1): behind the door check only a table class
		// without a case here lands in this branch — it never enters a queue
		// (pre-A03-W1 any third value fell into the interactive queue and its
		// Deckel-Staffel, K7).
		return d.refuseInvalidClass("enqueue", w.class, st.origin, w.role)
	}
}

// pushCappedLocked runs the Deckel-Staffel of one per-principal queue and
// enqueues w (capLadder, then pushLocked).
func (d *Dispatcher) pushCappedLocked(st *targetState, q *classQueue, w *waiter, perPrincipal, perTenant, maxTotal int) error {
	if err := capLadder(q, w, perPrincipal, perTenant, maxTotal); err != nil {
		return err
	}
	pushLocked(st, q, w)
	return nil
}

// capLadder is the Deckel-Staffel check of one per-principal queue, without
// a side effect: per principal (the queue's cap key) → per tenant →
// aggregate, each cap off at 0. The tenant cap applies only to a non-empty
// TenantID: an "" bucket would couple unrelated sentinel principals into one
// shared cap; the aggregate cap still bounds them (an external principal
// always carries a tenant — the door refuses it otherwise).
func capLadder(q *classQueue, w *waiter, perPrincipal, perTenant, maxTotal int) error {
	if perPrincipal > 0 && q.byKey[q.capKey(w.principal)] >= perPrincipal {
		return ErrPrincipalSaturated
	}
	if perTenant > 0 && w.principal.TenantID != "" && q.byTenant[w.principal.TenantID] >= perTenant {
		return ErrTenantSaturated
	}
	if maxTotal > 0 && q.total >= maxTotal {
		return ErrTargetSaturated
	}
	return nil
}

// pushLocked enqueues w with the next arrival seq of its target.
func pushLocked(st *targetState, q *classQueue, w *waiter) {
	st.seq++
	w.seq = st.seq
	q.push(w)
}

// admitLocked turns a waiter into a live lease (immediate admission and
// direct handoff share it). The admission-anchored deadline hint
// (Request.DeadlineIn) resolves HERE — admitted+DeadlineIn is the rule-V1
// wire deadline regardless of wait time; the reap reference stays the
// earliest of all provided references.
func (d *Dispatcher) admitLocked(st *targetState, w *waiter, now time.Time, slotHeld bool) *Lease {
	reapRef := w.reapRef
	if w.deadlineIn > 0 {
		if rel := now.Add(w.deadlineIn); reapRef.IsZero() || rel.Before(reapRef) {
			reapRef = rel
		}
	}
	l := &Lease{
		d:         d,
		st:        st,
		class:     w.class,
		principal: w.principal,
		role:      w.role,
		enqueued:  w.enqueued,
		admitted:  now,
		reapRef:   reapRef,
		cancel:    w.cancel,
		slotHeld:  slotHeld,
		agedAdmit: w.aged,
	}
	st.inflight[l] = struct{}{}
	if slotHeld {
		// Σw (A03-W7): the lease holds its weight in units — 1 for the
		// in-house classes by construction (requestWeight).
		l.weight = w.weight
		st.held += l.weight
		switch l.class {
		case ClassBackground:
			st.cancelable[l] = w.cancel
		case ClassExternal:
			// Reaper-only cancel (I-03-3): never in cancelable.
			st.heldExt += l.weight
			st.extLeases++
			st.extCancel[l] = w.cancel
			st.external.fair.leaseEnter(l, w.fair, d.fairClock(now))
		}
	}
	wait := now.Sub(w.enqueued)
	if r := st.ring(l.class); r != nil {
		r.add(wait) // MW7/D3-W3 wait sample, O(1)
	} else {
		// Fail-closed (A03-W1), unreachable behind the door check: no
		// class's window absorbs a sample of a class outside the table.
		d.logger.Error("dispatch: class outside the class table — wait sample dropped (fail-closed default)",
			"target", st.origin, "class", uint8(l.class))
	}
	if l.class == ClassInteractive && wait > d.warnAfter {
		d.warnWaitLocked(st, wait)
	}
	return l
}

// handoffLocked admits a picked waiter under the mutex — the slot is never
// "free" for newcomers while the wait queue is non-empty (structural
// no-barging, design/01 §4.4).
func (d *Dispatcher) handoffLocked(st *targetState, w *waiter, slotHeld bool) {
	w.lease = d.admitLocked(st, w, time.Now(), slotHeld)
	w.admitted = true
	close(w.admitCh)
}

// finishLocked settles a lease exactly once: registry out, slot back, next
// waiter in (release, reap and the cancel-race path all end here). Every
// lease end — pass-through or slot-holding, continuation leases (A03-W17)
// included — is one sample finish − admitted of its class's lease-duration
// window (A03-W6), next to the slot-time booking of an external lease
// (leaseLeave). The sample is per LEASE, not per weight unit: ŝ is the time a
// lease of the class takes to end, and L_p50 the length of one round of
// freed units — the weight enters the prediction through the units ahead.
func (d *Dispatcher) finishLocked(l *Lease) {
	if l.done {
		return
	}
	l.done = true
	now := time.Now()
	l.st.leaseRing(l.class).add(now.Sub(l.admitted))
	d.chargeLocked(l, now)
	d.preemptSettledLocked(l) // release-latency sample for a regular victim release (MW18)
	delete(l.st.inflight, l)
	delete(l.st.cancelable, l)
	delete(l.st.extCancel, l)
	if l.slotHeld {
		l.st.held -= l.weight
		if l.class == ClassExternal {
			l.st.heldExt -= l.weight
			l.st.extLeases--
			l.st.external.fair.leaseLeave(l, d.fairClock(now), d.usageWindowDur())
		}
		d.wakeLocked(l.st)
	}
}

// wakeLocked re-evaluates admission on one target — the wake half of the
// rank scan (design/03 §4.2, rank.go): while a slot unit is free, walk
// Settings.RankOrder and hand the units to the head of the first class whose
// head is admissible with its weight (direct handoff, A03-W7); when no class
// yields one, the wake ends. The rank table is read once per wake (hot: a settings swap acts on
// the next wake). The herald term for background is softened ONLY by the FA
// aging escape (aging.go): with demand > 0 an aged background HEAD may still
// be admitted, re-checked per iteration (a younger next head stays gated).
// Interactive precedence is structural: interactive holds rank 0
// (validRankOrder), so the escape is consulted only after the interactive
// pick returned nil. With slots ≤ 0 (emergency stop or cleared policy) every
// in-house waiter drains as pass-through. External waiters NEVER drain as
// pass-through (X9): on a target that stopped offering the external class
// (emergency stop, R removed, external_offer withdrawn) they end with
// ErrExternalNotOffered through the waiter error channel, and so do the
// external waiters heavier than the target's current external capacity (a
// policy swap shrank E below their weight — they could never be admitted,
// A03-W7); on an offered self-admitting target (no slots) they are admitted
// against external_max_inflight.
func (d *Dispatcher) wakeLocked(st *targetState) {
	// The wake re-reads the fairness order of the external class at this
	// instant (A03-W4 review 4 FR4-2: frozen between events).
	st.external.fair.touch(d.fairClock(time.Now()))
	if st.external.total > 0 {
		if capacity := d.externalWeightCap(st.origin); capacity == 0 {
			d.failExternalWaitersLocked(st, ErrExternalNotOffered)
		} else {
			d.failOverweightExternalWaitersLocked(st, capacity)
		}
	}
	slots := d.slotsLocked(st.origin)
	if slots <= 0 {
		for {
			w := st.interactive.pick(FairnessFIFO)
			if w == nil {
				w = st.background.pop()
			}
			if w == nil {
				break
			}
			d.handoffLocked(st, w, false)
		}
		for {
			w := d.pickAdmissibleLocked(st, ClassExternal, FairnessFIFO, slots)
			if w == nil {
				return
			}
			d.handoffLocked(st, w, true)
		}
	}
	set := d.currentSettings()
scan:
	for st.held < slots {
		for _, c := range set.RankOrder {
			if w := d.pickAdmissibleLocked(st, c, set.Fairness, slots); w != nil {
				d.handoffLocked(st, w, true)
				continue scan
			}
		}
		return
	}
}

// wakeAll re-evaluates every target (settings/policy swap, demand → 0).
func (d *Dispatcher) wakeAll() {
	d.mu.Lock()
	for _, st := range d.targets {
		d.wakeLocked(st)
	}
	d.mu.Unlock()
}

// dropWaiterLocked removes a canceled waiter from its queue (wait slot
// vacated, counters corrected — no leak, W1 negative probe). A dropped
// external waiter wakes the target (A03-W7): under weight the waiter behind
// it may fit where it did not — a canceled heavy head would otherwise strand
// a lighter next head until the next release, and in o1 that admissible but
// unserved head would refuse every background newcomer (eligibleNow). An
// in-house drop changes no head's admissibility (weight 1, the head of each
// class was already inadmissible), so it needs no wake.
func (d *Dispatcher) dropWaiterLocked(st *targetState, w *waiter) {
	switch w.class {
	case ClassInteractive:
		st.interactive.remove(w)
	case ClassBackground:
		st.background.remove(w)
	case ClassExternal:
		st.external.remove(w)
		d.wakeLocked(st)
	default:
		// Fail-closed (A03-W1), unreachable: a class without a case here
		// never entered a queue (door check + enqueueLocked default), so
		// there is nothing to remove (pre-A03-W1 the cancel removed from the
		// WRONG queue and left a dead interactive waiter for the next release
		// to hand a slot to, K12).
		d.logger.Error("dispatch: class without a drop case — waiter drop ignored (fail-closed default)",
			"target", st.origin, "class", uint8(w.class))
	}
}

// refuseInvalidClass is the class check of both admission doors and the
// enqueue default (A03-W1, design/03 §4.1 C7). At a door (site acquire /
// try-acquire) the class is outside the class table; at site enqueue —
// unreachable for such a value behind the doors — it is a table class
// without an enqueue case (TestEveryClassHasEveryBranch is red). Either way a
// code bug: refused before any waiter or lease exists, counted
// (Snapshot.InvalidClass) and logged at ERROR so it cannot drown in slog
// noise. Never a downgrade: an unhandled value has no nearest class.
func (d *Dispatcher) refuseInvalidClass(site string, class Class, origin, role string) error {
	d.invalidClass.Add(1)
	d.logger.Error("dispatch: acquire with an invalid class refused",
		"site", site, "class", uint8(class), "target", origin, "role", role)
	return ErrInvalidClass
}

// refusePrincipal is the principal gate of both admission doors (design/01
// §4.5 I-K10, wave W01-7), after the class check and BEFORE the B8 block: a
// poisoned principal (Conflict — the request ctx carried both credential
// kinds or an invalid model credential) is refused with ErrPrincipalConflict
// whatever else it carries; a consumer principal (ConsumerID != "") in a
// class with External() == false with ErrClassUnauthorized, and — the reverse
// direction, A03-W3 — an external class for anything but a consumer principal
// (ConsumerID set, ApiKeyID empty, TenantID set; K-03-1) with
// ErrClassUnauthorized too. Never a downgrade: the background queue has no
// competing principals by invariant (queue.go), and external load there
// would starve dream/digest (ErrQueueFull) and become preempt-/reapable —
// the inverse of "ctx first". Both are wiring defects: counted and logged at
// ERROR, refused before any waiter, wait slot or lease exists. nil admits
// the principal to B8, which stays byte-identical.
func (d *Dispatcher) refusePrincipal(site string, p Principal, req Request) error {
	if p.Conflict {
		d.principalConflicts.Add(1)
		d.logger.Error("dispatch: conflicting request principal (knowledge and model credential, or an invalid model credential) refused",
			"site", site, "class", req.Class.String(), "target", req.Target.Origin, "role", req.Role)
		return ErrPrincipalConflict
	}
	if p.ConsumerID != "" && !req.Class.External() {
		d.classUnauthorized.Add(1)
		d.logger.Error("dispatch: consumer principal in a non-external class refused",
			"site", site, "class", req.Class.String(), "consumer", p.ConsumerID,
			"target", req.Target.Origin, "role", req.Role)
		return ErrClassUnauthorized
	}
	if req.Class.External() && (p.ConsumerID == "" || p.ApiKeyID != "" || p.TenantID == "") {
		// The reverse direction (A03-W3, K-03-1): an external class only for
		// a consumer principal — consumer set, no knowledge key (I-K0), and a
		// tenant (migration 161 makes tenant_id NOT NULL; an empty tenant
		// would bypass the external tenant cap). Never a downgrade: the
		// zero principal is ctx' own detached work, a knowledge key a human.
		d.classUnauthorized.Add(1)
		d.logger.Error("dispatch: external class without a consumer principal refused",
			"site", site, "class", req.Class.String(), "consumer", p.ConsumerID,
			"knowledge_key", p.ApiKeyID != "", "tenant", p.TenantID != "",
			"target", req.Target.Origin, "role", req.Role)
		return ErrClassUnauthorized
	}
	return nil
}

// targetLocked returns (or creates) the state of one normalized origin.
func (d *Dispatcher) targetLocked(origin string) *targetState {
	st, ok := d.targets[origin]
	if !ok {
		st = &targetState{
			origin:      origin,
			inflight:    make(map[*Lease]struct{}),
			cancelable:  make(map[*Lease]context.CancelCauseFunc),
			extCancel:   make(map[*Lease]context.CancelCauseFunc),
			interactive: newClassQueue(),
			external:    newExternalQueue(),
			usage:       make(map[string]*usageWindow),
		}
		d.targets[origin] = st
	}
	return st
}

// slotsLocked resolves the effective slot cap of one origin: 0 when the
// dispatcher is disabled (emergency stop) or the policy declares nothing.
func (d *Dispatcher) slotsLocked(origin string) int {
	if !d.currentSettings().Enabled {
		return 0
	}
	return d.policy.Load().Targets[origin].Slots
}

func (d *Dispatcher) currentSettings() Settings { return *d.settings.Load() }

// warnWaitLocked emits the W1 visibility WARN: target, queue depths and the
// age of the oldest blocking lease (design/01 §4.4).
func (d *Dispatcher) warnWaitLocked(st *targetState, wait time.Duration) {
	var oldest time.Duration
	now := time.Now()
	for l := range st.inflight {
		if age := now.Sub(l.admitted); age > oldest {
			oldest = age
		}
	}
	d.logger.Warn("dispatch: interactive wait above threshold",
		"target", st.origin,
		"wait", wait,
		"waiting_interactive", st.interactive.total,
		"waiting_background", st.background.len(),
		"inflight", len(st.inflight),
		"oldest_lease_age", oldest)
}

// canonicalOrigin normalizes defensively; an unparseable origin keys on its
// literal string (it can never collide with a policy row — derivation drops
// unparseable rows too, so it stays pass-through).
func canonicalOrigin(raw string) string {
	if o, err := NormalizeOrigin(raw); err == nil {
		return o
	}
	return raw
}

// reapReference picks the earlier of the deadline hint and the ctx deadline;
// zero when both are absent (→ lease_max_age fallback).
func reapReference(hint time.Time, ctx context.Context) time.Time {
	ctxDl, ok := ctx.Deadline()
	if !ok {
		return hint
	}
	if hint.IsZero() || ctxDl.Before(hint) {
		return ctxDl
	}
	return hint
}

// reapLoop runs the reaper on its lazy cadence until Close.
func (d *Dispatcher) reapLoop() {
	defer d.loops.Done()
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stopLoops:
			return
		case now := <-t.C:
			d.reapNow(now)
		}
	}
}

// reapNow force-releases leases past their reap reference + grace (B1: a
// leaked lease must never keep a target permanently closed). Background
// leases are canceled with cause ErrReaped BEFORE the release — the
// underlying wire call ends instead of silently over-admitting into the
// invisible llama.cpp defer queue; external leases likewise, with their reap
// reference capped at admitted + external_lease_max (reapDeadline; the
// reaper is their only canceller, I-03-3). Interactive leases are never canceled
// (I-D1); their over-admission window is an accepted degradation toward
// today's behavior, alarmed by the divergence metric. Pass-through leases
// are evicted from the registry WITHOUT cancel (behavior neutrality). The
// same pass also emits the >threshold wait WARNs for still-queued
// interactive acquires (once per waiter).
func (d *Dispatcher) reapNow(now time.Time) {
	set := d.currentSettings()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, st := range d.targets {
		var victims []*Lease
		for l := range st.inflight {
			if now.After(reapDeadline(l, set)) {
				victims = append(victims, l)
			}
		}
		for _, l := range victims {
			if cancel, ok := st.cancelable[l]; ok {
				cancel(ErrReaped)
			} else if cancel, ok := st.extCancel[l]; ok {
				// I-03-3: the reaper is the ONLY canceller of an external
				// lease (leak net); the cancel funcs of extCancel are read
				// nowhere else (the prediction reads only its keys' admission
				// instants, prediction.go firstRoundLocked).
				cancel(ErrReaped)
			}
			d.reapsTotal.Add(1)
			d.logger.Error("dispatch: lease reaped",
				"target", st.origin, "class", l.class.String(), "role", l.role,
				"held", l.slotHeld, "age", now.Sub(l.admitted))
			d.finishLocked(l)
		}
		st.interactive.each(func(w *waiter) {
			if !w.warned && now.Sub(w.enqueued) > d.warnAfter {
				w.warned = true
				d.warnWaitLocked(st, now.Sub(w.enqueued))
			}
		})
		d.sweepUsageLocked(st, now)
		st.external.fair.sweep(d.fairClock(now), d.usageWindowDur())
		if set.BackgroundAgingAfter > 0 {
			// FA trigger (b), design/04 §4.6: with the escape armed the reaper
			// tick re-evaluates admission — an IDLE target (free slot, no
			// running lease, process-wide demand > 0) has no release event, so
			// without this an aged background waiter there would wait forever.
			// Gated on the knob so the default-0 path stays byte-identical.
			d.wakeLocked(st)
		}
	}
}

// reapDeadline is the instant past which the reaper force-releases l: its
// reap reference (lease_max_age fallback when none was given) — for an
// external lease capped at admitted + external_lease_max (I-03-3, design/03
// §4.6: alias timeouts up to 3600 s must not hold external partition capacity
// that long) — plus lease_reap_grace.
func reapDeadline(l *Lease, set Settings) time.Time {
	ref := l.reapRef
	if ref.IsZero() {
		ref = l.admitted.Add(set.LeaseMaxAge)
	}
	if l.class == ClassExternal && set.ExternalLeaseMax > 0 {
		if lim := l.admitted.Add(set.ExternalLeaseMax); lim.Before(ref) {
			ref = lim
		}
	}
	return ref.Add(set.LeaseReapGrace)
}

// WaitStats is one class's queue view in a target snapshot.
type WaitStats struct {
	Waiting int
	// OldestWait is the wait time of the longest-queued acquire.
	OldestWait time.Duration
	// P95Wait / MaxWait are aggregates over the rolling window of the last
	// waitRingK admitted wait samples of this target × class (MW7/D3-W3,
	// metrics.go); Samples is the window size they were computed from
	// (design/03 §4.6.1 window honesty). Exposure rule (§4.6.1, decided):
	// target×class wait aggregates are server-admin-only — a tenant-pollable
	// target aggregate would be a live activity oracle over foreign load
	// (the C2 leak type via the status channel).
	P95Wait time.Duration
	MaxWait time.Duration
	Samples int
}

// LeaseDurStats is one class's lease-duration window in a target snapshot
// (A03-W6): nearest-rank p50 and p90 of finish − admitted and the window
// size they were computed from (window honesty, like WaitStats.Samples).
type LeaseDurStats struct {
	P50, P90 time.Duration
	Samples  int
}

// TargetSnapshot is the introspection view of one origin (queue depths,
// inflight, policy) — data source for WARN correlation and the status waves.
// Exposure rule (design/01 §6, binding): registry-derived status data is
// server-admin-only OR filtered to the caller's own principals. Since MW22
// the snapshot DOES carry principal detail (Buckets keys on fairKey) —
// every status surface must be server-admin-only or filter Buckets to the
// caller's own tenant before exposure (F-B3, probe lands with MW12).
type TargetSnapshot struct {
	Origin            string
	Slots             int
	PreemptBackground bool
	HeraldScope       HeraldScope
	// Held is the number of slot-holding leases of ALL classes, internal AND
	// external — the foreign-load detector contract (handler/foreign_load.go
	// heldOf, DECISIONS A-E5 no. 3): the engine sees both, so the comparison
	// must too. HeldInternal + HeldExternal == Held. It stays a LEASE count
	// under weight (A03-W7): the engine's http_requests_active counts
	// requests, so active pairs with Held; a sum of weights there would hide
	// a foreign request waiting in a saturated engine in the gap Σw − leases.
	Held int
	// HeldWeight is the sum of the weights of the slot-holding leases (Σw,
	// design/03 §4.7: engine sequences) — the admission unit, and the
	// counterpart of the engine's running sequences for the first wave that
	// sends w > 1 to a probe-marked origin; HeldExternalWeight its external
	// part. An in-house lease weighs 1, so HeldWeight − HeldExternalWeight
	// == HeldInternal.
	HeldWeight         int
	HeldExternalWeight int
	Inflight           int
	// HeldInternal / HeldExternal split Held by side (design/03 §4.14);
	// InflightInternal counts the internal leases incl. pass-through (an
	// external lease is never pass-through, so Inflight − InflightInternal
	// == HeldExternal). The tenant view reads ONLY the internal sizes (X23).
	HeldInternal     int
	HeldExternal     int
	InflightInternal int
	// InternalReserved is the validated internal partition R (0 = none);
	// ExternalOffered whether the target admits the external class right now
	// (I-03-6, false under the emergency stop), ExternalCapacity its E in
	// weight units (S − R on a slot-managed target, external_max_inflight on
	// a self-admitting one; 0 = not offered or no external cap).
	InternalReserved int
	ExternalOffered  bool
	ExternalCapacity int
	Interactive      WaitStats
	Background       WaitStats
	// External is the wait view of the external class (own queue, own
	// K = 512 window).
	External WaitStats
	// LeaseDur is the lease-duration view per class, indexed by Class
	// (A03-W6): p50/p90 over the window of the last waitRingK lease ends —
	// exact here, the server-admin pull (design/03 §4.14); the external
	// prediction reads the published copy.
	LeaseDur [numClasses]LeaseDurStats
	// ExternalRejects counts the external refusals per reason.
	ExternalRejects ExternalRejectStats
	// Buckets is the per-fairness-bucket wait + token-window view (MW22,
	// usage.go) — the primary gate source for the fairness/budget waves.
	Buckets []BucketSnapshot
	// Preempt carries the per-target preempt telemetry (design/02 §3.3);
	// same exposure rule as the rest of the snapshot.
	Preempt PreemptStats
}

// Snapshot is the dispatcher-wide introspection view.
type Snapshot struct {
	Enabled         bool
	Demand          int
	ReapsTotal      int64
	ClassDowngrades int64
	// InvalidClass counts acquires refused with ErrInvalidClass (a class
	// outside the class table, A03-W1). Dispatcher-internal for now: the
	// status surface gains it with the class-generic status fields (A03-W3).
	InvalidClass int64
	// ClassUnauthorized counts acquires refused with ErrClassUnauthorized (a
	// consumer principal in a class with External() == false, W01-7);
	// PrincipalConflicts those refused with ErrPrincipalConflict (a poisoned
	// principal). Both are server-admin status fields next to
	// class_downgrades.
	ClassUnauthorized  int64
	PrincipalConflicts int64
	// UnchargedCalls counts interactive releases without reported usage
	// (charge 0 — visibility instead of estimation, MW22/C1).
	UnchargedCalls int64
	// OpsTotal / MaxOpDur are the hot-path measurement hooks (design/04 §6):
	// Acquire+Release mutex sections and the longest observed section.
	OpsTotal int64
	MaxOpDur time.Duration
	Targets  []TargetSnapshot
}

// Snapshot returns the current introspection view (W1 visibility).
func (d *Dispatcher) Snapshot() Snapshot {
	set := d.currentSettings()
	pol := d.policy.Load()
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := Snapshot{
		Enabled:            set.Enabled,
		Demand:             int(d.demand.Load()),
		ReapsTotal:         d.reapsTotal.Load(),
		ClassDowngrades:    d.classDowngrades.Load(),
		InvalidClass:       d.invalidClass.Load(),
		ClassUnauthorized:  d.classUnauthorized.Load(),
		PrincipalConflicts: d.principalConflicts.Load(),
		UnchargedCalls:     d.stats.unchargedCalls.Load(),
		OpsTotal:           d.stats.opsTotal.Load(),
		MaxOpDur:           time.Duration(d.stats.maxOpNs.Load()),
	}
	for _, origin := range sortedOrigins(d.targets) {
		st := d.targets[origin]
		tp := pol.Targets[origin]
		hs := tp.HeraldScope
		if hs == "" {
			hs = HeraldGlobal
		}
		ts := TargetSnapshot{
			Origin:            origin,
			Slots:             tp.Slots,
			PreemptBackground: tp.PreemptBackground,
			HeraldScope:       hs,
			Held:              st.held - st.heldExt + st.extLeases,
			Inflight:          len(st.inflight),
			HeldInternal:      st.held - st.heldExt,
			HeldExternal:      st.extLeases,
			InflightInternal:  len(st.inflight) - st.extLeases,
			InternalReserved:  tp.InternalReserved,
			Interactive:       WaitStats{Waiting: st.interactive.total},
			Background:        WaitStats{Waiting: st.background.len()},
			External:          WaitStats{Waiting: st.external.total},
			ExternalRejects:   st.extRejects,
			Preempt:           st.preempt.statsLocked(),
		}
		// Σw next to the lease counts (A03-W7): Held stays the lease count.
		ts.HeldWeight, ts.HeldExternalWeight = st.held, st.heldExt
		if set.Enabled {
			ts.ExternalOffered, ts.ExternalCapacity = tp.externalCapacity()
		}
		if t := st.interactive.oldest(); !t.IsZero() {
			ts.Interactive.OldestWait = now.Sub(t)
		}
		if t := st.background.oldest(); !t.IsZero() {
			ts.Background.OldestWait = now.Sub(t)
		}
		if t := st.external.oldest(); !t.IsZero() {
			ts.External.OldestWait = now.Sub(t)
		}
		st.ring(ClassInteractive).statsInto(&ts.Interactive)
		st.ring(ClassBackground).statsInto(&ts.Background)
		st.ring(ClassExternal).statsInto(&ts.External)
		for c := range ts.LeaseDur {
			st.leaseRing(Class(c)).leaseStatsInto(&ts.LeaseDur[c])
		}
		ts.Buckets = d.bucketSnapshotsLocked(st, now)
		out.Targets = append(out.Targets, ts)
	}
	return out
}
