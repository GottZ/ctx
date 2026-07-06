// Preempt path (Vorhaben E wave MW18, design/02 §4.2): when an interactive
// acquire meets a target with exhausted slots AND preempt_background=true,
// the dispatcher cancels the YOUNGEST running background lease (E-P1/K3 —
// minimizes sunk GPU work at identical slot gain) with cause ErrPreempted
// (K1 wrap doctrine). The slot is NOT freed at cancel but at the victim's
// regular release (wire return) — the preempt path has no admission way of
// its own, the normal direct handoff applies unchanged. A watchdog
// force-releases a victim whose release stays out past
// dispatch.preempt_release_timeout (E-P2) — the THIRD legitimate release
// path next to regular Release and the reaper. Interactive leases are never
// canceled (I-D1): their cancel funcs do not exist in the registry, so an
// interactive preempt is structurally impossible, not just forbidden.
// Behavior-neutral until the deliberate data step (MW21) sets
// preempt_background=true on a target row — default false everywhere.
package dispatch

import (
	"sort"
	"time"
)

// defaultPreemptReleaseTimeout mirrors the dispatch.preempt_release_timeout
// registry default (E-P2). The watchdog guards the CLIENT half of the abort
// latency (cancel → wire return, purely Go-side, healthy in milliseconds) —
// 2 s is a ≥3-orders-of-magnitude safety factor against scheduler/GC/
// transport outliers, calibrated against preempt_release_ms_max after MW20.
const defaultPreemptReleaseTimeout = 2 * time.Second

// providerClassLlamaCpp mirrors backends.ProviderLlamaCpp without the import
// (leaf rule) — the value is the validate.go enum anchor.
const providerClassLlamaCpp = "llamacpp"

// preemptClassAllowed is the external guard of the policy derivation
// (design/02 §3.1 no. 2, E-P5): preempt_background counts only on LOCAL
// provider classes — canceling an external pass-through (openrouter/openai
// "generic") only closes OUR connection while upstream keeps generating and
// BILLING; paid damage without latency gain. Allow-list, fail-closed: an
// unknown or empty class degrades to admission-control (= today's behavior),
// never to a preempt.
func preemptClassAllowed(providerClass string) bool {
	return providerClass == providerClassLlamaCpp
}

// preemptState is the per-target preempt bookkeeping, guarded by
// Dispatcher.mu like the rest of targetState (design/02 §3.3 telemetry).
type preemptState struct {
	// preempting registers victims whose cancel is spoken but whose release
	// is still outstanding ("in eviction") — they count against the deficit
	// of the single-preempt guard, keyed to their cancel time for the
	// release-latency sample. Lazily allocated on the first preempt.
	preempting map[*Lease]time.Time
	// preemptsTotal counts spoken cancels; wastedMs sums now−admitted of the
	// victim at cancel time (sunk occupancy, the honest input of the MW21
	// thrash trigger — preemptsTotal alone would rate few but expensive
	// discarded 90k-PP starts as harmless, design/02 PB3).
	preemptsTotal int64
	wastedMs      int64
	// releaseMsLast/-Max sample the cancel→release span of regular victim
	// releases — the client-measurable half of the slot-free latency. Forced
	// releases deliberately leave no sample (they would record the timeout,
	// not a wire return); forcedReleases is their divergence metric, target
	// value 0, every entry a slog-ERROR.
	releaseMsLast  int64
	releaseMsMax   int64
	forcedReleases int64
}

// PreemptStats is the snapshot view of one target's preempt telemetry
// (design/02 §3.3; exposure rules of design/01 §6 apply unchanged).
type PreemptStats struct {
	PreemptsTotal       int64
	WastedMsTotal       int64
	ReleaseMsLast       int64
	ReleaseMsMax        int64
	ForcedReleasesTotal int64
}

// statsLocked exports the counters (Dispatcher.mu held).
func (p *preemptState) statsLocked() PreemptStats {
	return PreemptStats{
		PreemptsTotal:       p.preemptsTotal,
		WastedMsTotal:       p.wastedMs,
		ReleaseMsLast:       p.releaseMsLast,
		ReleaseMsMax:        p.releaseMsMax,
		ForcedReleasesTotal: p.forcedReleases,
	}
}

// maybePreemptLocked runs after an interactive waiter enqueued on st
// (Dispatcher.mu held). Trigger doctrine (design/02 §4.2.1): only an
// interactive ACQUIRE opens a preempt — never a background acquire, never
// the herald alone. Single-preempt guard (§4.2.3): at most as many cancels
// as slots are missing to cover the waiting interactive acquires, a victim
// in eviction counts as covering — two waiters on a 1-slot target speak ONE
// cancel, not two (PB7, no cascade). A canceled trigger ctx does NOT revoke
// the cancel (PB10): the freed slot goes to the next waiter or back into
// regular admission, wastedMs counts the loss honestly.
func (d *Dispatcher) maybePreemptLocked(st *targetState, slots int) {
	if !d.policy.Load().Targets[st.origin].PreemptBackground {
		return // degradation rule §4.5: pure admission control
	}
	want := st.interactive.total
	if want > slots {
		want = slots // more waiters than slots: freeing beyond capacity is waste
	}
	free := slots - st.held
	if free < 0 {
		free = 0
	}
	need := want - free - len(st.preempt.preempting)
	if need <= 0 {
		return
	}
	victims := pickVictimsLocked(st, need)
	if len(victims) == 0 {
		// Target full with interactive-only leases: no-op + WARN (01 B5 —
		// I-D1 forbids the only theoretical victims; the waiter waits).
		d.logger.Warn("dispatch: interactive demand on full target without cancelable background lease",
			"target", st.origin, "waiting_interactive", st.interactive.total, "held", st.held)
		return
	}
	timeout := d.currentSettings().PreemptReleaseTimeout
	if timeout <= 0 {
		timeout = defaultPreemptReleaseTimeout // fail-closed toward the protected good
	}
	if st.preempt.preempting == nil {
		st.preempt.preempting = make(map[*Lease]time.Time)
	}
	now := time.Now()
	for _, l := range victims {
		cancel := st.cancelable[l]
		st.preempt.preempting[l] = now
		st.preempt.preemptsTotal++
		st.preempt.wastedMs += now.Sub(l.admitted).Milliseconds()
		cancel(ErrPreempted)
		d.logger.Warn("dispatch: background lease preempted for interactive demand",
			"target", st.origin, "role", l.role, "victim_age", now.Sub(l.admitted),
			"waiting_interactive", st.interactive.total)
		victim := l
		time.AfterFunc(timeout, func() { d.preemptWatchdog(victim) })
	}
}

// pickVictimsLocked selects up to need victims among the slot-holding
// background leases not already in eviction — youngest first (shortest
// now−admitted, E-P1/K3): the oldest lease has invested the most and is
// closest to completion; killing it maximizes the loss at the same slot
// gain. Only background cancel funcs exist in st.cancelable (I-D1), so the
// candidate set is structurally interactive-free.
func pickVictimsLocked(st *targetState, need int) []*Lease {
	cands := make([]*Lease, 0, len(st.cancelable))
	for l := range st.cancelable {
		if _, evicting := st.preempt.preempting[l]; evicting {
			continue
		}
		cands = append(cands, l)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].admitted.After(cands[j].admitted) })
	if len(cands) > need {
		cands = cands[:need]
	}
	return cands
}

// preemptWatchdog is the E-P2 force-release: the victim's wire call did not
// return within preempt_release_timeout after its cancel (transport hang —
// theoretical, Go guarantees the return, but the protected good demands the
// second layer, PB2). It cancels NOTHING (the lease IS canceled — the 01 §5
// B4 doctrine "only preemption and reaper cancel" stays intact); it only
// frees the slot. Residual risk = over-admission toward today's behavior
// (llama.cpp defer queue), alarmed by forcedReleases + slog-ERROR. A lease
// already settled (regular release won the race) is a no-op.
func (d *Dispatcher) preemptWatchdog(l *Lease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l.done {
		return
	}
	if _, evicting := l.st.preempt.preempting[l]; !evicting {
		return
	}
	// Drop the eviction entry BEFORE finishLocked so the settle hook records
	// no release_ms sample — a forced entry would report the timeout value,
	// not a measured wire return.
	delete(l.st.preempt.preempting, l)
	l.st.preempt.forcedReleases++
	d.logger.Error("dispatch: preempted lease force-released after preempt_release_timeout",
		"target", l.st.origin, "role", l.role, "age", time.Since(l.admitted))
	d.finishLocked(l)
}

// preemptSettledLocked is the finishLocked hook: a victim released through
// the REGULAR path (wire return after cancel) samples the cancel→release
// span — the client-measurable half of the slot-free latency, calibration
// source for preempt_release_timeout (E-P2). Non-victims exit immediately.
func (d *Dispatcher) preemptSettledLocked(l *Lease) {
	at, ok := l.st.preempt.preempting[l]
	if !ok {
		return
	}
	delete(l.st.preempt.preempting, l)
	ms := time.Since(at).Milliseconds()
	l.st.preempt.releaseMsLast = ms
	if ms > l.st.preempt.releaseMsMax {
		l.st.preempt.releaseMsMax = ms
	}
}
