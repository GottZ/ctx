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
	// held counts slot-holding leases (pass-through leases excluded).
	held int
	// inflight is the registry of ALL leases incl. pass-through (telemetry,
	// divergence metric, reaper).
	inflight map[*Lease]struct{}
	// cancelable holds the cancel funcs of slot-holding BACKGROUND leases
	// only — interactive cancel funcs are never registered, so a dispatcher
	// cancel of interactive is structurally impossible, not just forbidden
	// (I-D1/B5).
	cancelable  map[*Lease]context.CancelCauseFunc
	interactive *classQueue
	background  bgQueue
	// usage is the per-fairness-bucket token meter (MW22/F2, usage.go).
	usage map[string]*usageWindow
	// preempt is the MW18 preempt-path bookkeeping (victims in eviction +
	// telemetry counters, design/02 §3.3) — see preempt.go.
	preempt preemptState
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
	// stats are the MW22 meter counters: uncharged_calls + the hot-path
	// measurement hooks (usage.go).
	stats usageStats

	stopReaper chan struct{}
	stopOnce   sync.Once
}

// New constructs the dispatcher and starts the reaper goroutine. logger nil
// falls back to slog.Default().
func New(logger *slog.Logger, s Settings) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dispatcher{
		logger:     logger,
		warnAfter:  interactiveWaitWarn,
		targets:    make(map[string]*targetState),
		stopReaper: make(chan struct{}),
	}
	d.settings.Store(&s)
	d.policy.Store(&Policy{})
	go d.reapLoop()
	return d
}

// Close stops the reaper goroutine (process shutdown; waiters drain via
// their own ctx).
func (d *Dispatcher) Close() {
	d.stopOnce.Do(func() { close(d.stopReaper) })
}

// UpdateSettings hot-swaps the config-derived knobs. A flip to
// Enabled=false drains every waiter as pass-through immediately (B2: the
// emergency stop must never leave goroutines stuck behind a dead gate).
func (d *Dispatcher) UpdateSettings(s Settings) {
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
// scheduler's activeQueries counter (consumed by the LLM-free arms, wave
// MW15).
func (d *Dispatcher) InteractiveDemand() int {
	return int(d.demand.Load())
}

// Acquire blocks until the request is admitted on its target, the caller ctx
// ends, or a Deckel rejects it. The returned runCtx derives from ctx; for
// background leases the dispatcher holds its cancel (preempt/reap), for
// interactive it never cancels (I-D1). Acquire errors are NOT attempts: no
// Classify, no health report, no llmlog line, terminal for the chain walk
// (design/01 §4.3).
func (d *Dispatcher) Acquire(ctx context.Context, req Request) (*Lease, context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err // canceled ctx never takes a wait slot
	}
	class := req.Class
	if class == ClassInteractive && req.Principal.ApiKeyID == "" {
		// B8 class authorization: interactive requires a principal.
		// Scheduler arms (empty principal) are structurally background;
		// a violating call is downgraded fail-closed toward the protected
		// good, never rejected (the work itself is legitimate).
		class = ClassBackground
		d.classDowngrades.Add(1)
		d.logger.Error("dispatch: interactive acquire without principal downgraded to background",
			"target", req.Target.Origin, "role", req.Role)
	}
	now := time.Now()
	runCtx, cancel := context.WithCancelCause(ctx)
	w := &waiter{
		class:     class,
		principal: req.Principal,
		role:      req.Role,
		enqueued:  now,
		reapRef:   reapReference(req.Deadline, ctx),
		runCtx:    runCtx,
		cancel:    cancel,
		admitCh:   make(chan struct{}),
	}

	d.mu.Lock()
	opStart := time.Now()
	st := d.targetLocked(canonicalOrigin(req.Target.Origin))
	slots := d.slotsLocked(st.origin)
	if slots <= 0 {
		// Pass-through: disabled or no declared policy — exactly today's
		// behavior, tracked for telemetry only.
		l := d.admitLocked(st, w, now, false)
		d.opDone(opStart)
		d.mu.Unlock()
		return l, runCtx, nil
	}
	if d.eligibleNowLocked(st, class, slots) {
		l := d.admitLocked(st, w, now, true)
		d.opDone(opStart)
		d.mu.Unlock()
		return l, runCtx, nil
	}
	err := d.enqueueLocked(st, w)
	if err == nil && class == ClassInteractive {
		// Preempt trigger (MW18, design/02 §4.2.1): only an interactive
		// acquire that has to WAIT may open a preempt on a preempt-enabled
		// target — never a background acquire, never the herald alone.
		d.maybePreemptLocked(st, slots)
	}
	d.opDone(opStart)
	d.mu.Unlock()
	if err != nil {
		cancel(context.Canceled)
		return nil, nil, err
	}

	select {
	case <-w.admitCh:
		return w.lease, runCtx, nil
	case <-ctx.Done():
		d.mu.Lock()
		if w.admitted {
			// The direct handoff raced the cancel: the slot is already
			// ours — settle it properly so the next waiter wakes.
			d.finishLocked(w.lease)
		} else {
			d.dropWaiterLocked(st, w)
		}
		d.mu.Unlock()
		cancel(context.Canceled)
		return nil, nil, ctx.Err()
	}
}

// eligibleNowLocked is the admission predicate (design/01 §4.4).
// interactive: slot free AND no waiting interactive (FIFO, no barging — a
// newcomer never overtakes a waiter of its own or a higher class of the SAME
// bucket; bucket order is policy). background: slot free AND both wait
// queues empty AND demand == 0 — the herald term keeps background out of the
// inter-lease gaps of running interactive requests (R3, not softenable).
func (d *Dispatcher) eligibleNowLocked(st *targetState, class Class, slots int) bool {
	if st.held >= slots {
		return false
	}
	if class == ClassInteractive {
		return st.interactive.total == 0
	}
	return st.interactive.total == 0 && st.background.len() == 0 && d.demand.Load() == 0
}

// enqueueLocked places w in its class queue or rejects it fast. Check order
// of the Deckel-Staffel: principal → tenant → aggregate (the most specific
// offender first, D3-U4/E-U6).
func (d *Dispatcher) enqueueLocked(st *targetState, w *waiter) error {
	set := d.currentSettings()
	if w.class == ClassBackground {
		if set.BackgroundQueueMax > 0 && st.background.len() >= set.BackgroundQueueMax {
			return ErrQueueFull
		}
		st.seq++
		w.seq = st.seq
		st.background.push(w)
		return nil
	}
	q := st.interactive
	if set.InteractiveQueuePerPrincipal > 0 && q.byKey[w.principal.ApiKeyID] >= set.InteractiveQueuePerPrincipal {
		return ErrPrincipalSaturated
	}
	// The tenant cap applies only to a non-empty TenantID: an "" bucket
	// would couple unrelated sentinel principals into one shared cap; the
	// aggregate cap still bounds them.
	if set.InteractiveQueuePerTenant > 0 && w.principal.TenantID != "" &&
		q.byTenant[w.principal.TenantID] >= set.InteractiveQueuePerTenant {
		return ErrTenantSaturated
	}
	if set.InteractiveQueueMax > 0 && q.total >= set.InteractiveQueueMax {
		return ErrTargetSaturated
	}
	st.seq++
	w.seq = st.seq
	q.push(w)
	return nil
}

// admitLocked turns a waiter into a live lease (immediate admission and
// direct handoff share it).
func (d *Dispatcher) admitLocked(st *targetState, w *waiter, now time.Time, slotHeld bool) *Lease {
	l := &Lease{
		d:         d,
		st:        st,
		class:     w.class,
		principal: w.principal,
		role:      w.role,
		enqueued:  w.enqueued,
		admitted:  now,
		reapRef:   w.reapRef,
		cancel:    w.cancel,
		slotHeld:  slotHeld,
		agedAdmit: w.aged,
	}
	st.inflight[l] = struct{}{}
	if slotHeld {
		st.held++
		if l.class == ClassBackground {
			st.cancelable[l] = w.cancel
		}
	}
	if l.class == ClassInteractive {
		if wait := now.Sub(w.enqueued); wait > d.warnAfter {
			d.warnWaitLocked(st, wait)
		}
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
// waiter in (release, reap and the cancel-race path all end here).
func (d *Dispatcher) finishLocked(l *Lease) {
	if l.done {
		return
	}
	l.done = true
	d.chargeLocked(l, time.Now())
	d.preemptSettledLocked(l) // release-latency sample for a regular victim release (MW18)
	delete(l.st.inflight, l)
	delete(l.st.cancelable, l)
	if l.slotHeld {
		l.st.held--
		d.wakeLocked(l.st)
	}
}

// wakeLocked re-evaluates admission on one target: strict class order,
// direct handoff, herald term for background — softened ONLY by the FA
// aging escape (aging.go): with demand > 0 an aged background HEAD may still
// be admitted, re-checked per iteration (a younger next head stays gated).
// Interactive precedence is structural: the escape is consulted only after
// the interactive pick returned nil. With slots ≤ 0 (emergency stop or
// cleared policy) every waiter drains as pass-through.
func (d *Dispatcher) wakeLocked(st *targetState) {
	slots := d.slotsLocked(st.origin)
	if slots <= 0 {
		for {
			w := st.interactive.pick(FairnessFIFO)
			if w == nil {
				w = st.background.pop()
			}
			if w == nil {
				return
			}
			d.handoffLocked(st, w, false)
		}
	}
	mode := d.currentSettings().Fairness
	for st.held < slots {
		if w := st.interactive.pick(mode); w != nil {
			d.handoffLocked(st, w, true)
			continue
		}
		now := time.Now()
		if demandFree := d.demand.Load() == 0; demandFree || d.agingEscapeLocked(st, now) {
			if w := st.background.pop(); w != nil {
				if !demandFree {
					d.agedAdmitLocked(st, w, now)
				}
				d.handoffLocked(st, w, true)
				continue
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
// vacated, counters corrected — no leak, W1 negative probe).
func (d *Dispatcher) dropWaiterLocked(st *targetState, w *waiter) {
	if w.class == ClassInteractive {
		st.interactive.remove(w)
		return
	}
	st.background.remove(w)
}

// targetLocked returns (or creates) the state of one normalized origin.
func (d *Dispatcher) targetLocked(origin string) *targetState {
	st, ok := d.targets[origin]
	if !ok {
		st = &targetState{
			origin:      origin,
			inflight:    make(map[*Lease]struct{}),
			cancelable:  make(map[*Lease]context.CancelCauseFunc),
			interactive: newClassQueue(),
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
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stopReaper:
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
// invisible llama.cpp defer queue. Interactive leases are never canceled
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
			ref := l.reapRef
			if ref.IsZero() {
				ref = l.admitted.Add(set.LeaseMaxAge)
			}
			if now.After(ref.Add(set.LeaseReapGrace)) {
				victims = append(victims, l)
			}
		}
		for _, l := range victims {
			if cancel, ok := st.cancelable[l]; ok {
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

// WaitStats is one class's queue view in a target snapshot.
type WaitStats struct {
	Waiting int
	// OldestWait is the wait time of the longest-queued acquire.
	OldestWait time.Duration
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
	Held              int
	Inflight          int
	Interactive       WaitStats
	Background        WaitStats
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
		Enabled:         set.Enabled,
		Demand:          int(d.demand.Load()),
		ReapsTotal:      d.reapsTotal.Load(),
		ClassDowngrades: d.classDowngrades.Load(),
		UnchargedCalls:  d.stats.unchargedCalls.Load(),
		OpsTotal:        d.stats.opsTotal.Load(),
		MaxOpDur:        time.Duration(d.stats.maxOpNs.Load()),
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
			Held:              st.held,
			Inflight:          len(st.inflight),
			Interactive:       WaitStats{Waiting: st.interactive.total},
			Background:        WaitStats{Waiting: st.background.len()},
			Preempt:           st.preempt.statsLocked(),
		}
		if t := st.interactive.oldest(); !t.IsZero() {
			ts.Interactive.OldestWait = now.Sub(t)
		}
		if t := st.background.oldest(); !t.IsZero() {
			ts.Background.OldestWait = now.Sub(t)
		}
		ts.Buckets = d.bucketSnapshotsLocked(st, now)
		out.Targets = append(out.Targets, ts)
	}
	return out
}
