package dispatch

// Abort settle (wave A03-W10, design/03 §1.3 I-03-7, §4.10 no. 2): the lease
// end follows the engine, not the client. A slot-holding lease whose run
// context ended BEFORE its Release — the client went away, the request
// deadline passed, the dispatcher preempted it — leaves an engine that may
// still generate: SGLang notices a dropped non-stream request only at its
// next abort poll (≈ 4 s, design/03 §2 no. 9). Releasing the unit at once
// would admit the next request into the engine's own FCFS queue, invisible to
// the books. Such a lease therefore stays booked as settling for the
// target's abort_settle_ms (stream or nonstream by Request.Stream): it keeps
// its units in held, heldExt, classHeld and the fairness record's heldBy,
// counts in the snapshot's Held/HeldWeight (the foreign-load detector reads
// those, A-E5 no. 3) and is shown as Settling. Then finishLocked ends it —
// exactly once: the settle timer, the reaper (a settling lease past its reap
// deadline ends there — not counted as a reap, its caller did release it) and
// Close all end it through finishLocked, whose done flag makes the first of
// them the only one.
//
// Nothing is canceled additionally (I-D1 stays untouched): a settle only
// releases later. The default 0/0 settles nothing — the release is the one
// before this wave, byte for byte. The values are data per target (A-E2
// no. 2: abort_settle_ms in context_backends.limits); the measurement that
// sets them is A03-W11, the activation A03-W12.

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// AbortSettleMaxMs bounds each field of limits.abort_settle_ms (design/03
// §3.2: 0…30 000). A parse bound with a reason: the expected non-stream
// settle is SGLang's ≈ 4 s abort poll (§4.10), and 30 s equals the registry
// default of dispatch.lease_reap_grace — the slack the leak net grants a
// lease past its deadline; a settle is never allowed to hold a unit longer
// than that. backends' write path mirrors it (abortSettleMaxMs), pinned equal
// by events' TestAbortSettleWritePathAndDerivationAgree.
const AbortSettleMaxMs = 30000

// The two fields of limits.abort_settle_ms.
const (
	abortSettleStream    = "stream"
	abortSettleNonstream = "nonstream"
)

// mergeAbortSettle derives limits.abort_settle_ms of one origin: MAX per field
// over the authoritative rows (design/03 §3.2 — the longer settle is the side
// of the protected good). A row's value must be an object with only the
// fields stream and nonstream, each an integer 0…AbortSettleMaxMs (a missing
// field reads 0); any other form is ignored with a WARN and adds nothing —
// fail-safe toward the release before this wave. Missing everywhere ⇒ 0/0.
func mergeAbortSettle(origin string, auth []BackendRow, logger *slog.Logger) (stream, nonstream time.Duration) {
	for _, r := range auth {
		v, ok := r.Limits[limitAbortSettle]
		if !ok {
			continue
		}
		s, n, valid := parseAbortSettle(v)
		if !valid {
			logger.Warn("dispatch: invalid limits.abort_settle_ms ignored (an object {\"stream\": ms, \"nonstream\": ms}, each an integer 0..max_ms)",
				"backend", r.Name, "origin", origin, "value", v, "max_ms", AbortSettleMaxMs)
			continue
		}
		stream, nonstream = max(stream, s), max(nonstream, n)
	}
	return stream, nonstream
}

// parseAbortSettle reads one row's abort_settle_ms (JSONB numbers arrive as
// float64, in-process callers may pass int forms).
func parseAbortSettle(v any) (stream, nonstream time.Duration, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	for k, f := range m {
		ms, valid := settleMillis(f)
		if !valid {
			return 0, 0, false
		}
		switch k {
		case abortSettleStream:
			stream = ms
		case abortSettleNonstream:
			nonstream = ms
		default:
			return 0, 0, false
		}
	}
	return stream, nonstream, true
}

// settleMillis reads one field: an integer 0…AbortSettleMaxMs, as a duration.
func settleMillis(v any) (time.Duration, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, false
	}
	if f < 0 || f > AbortSettleMaxMs || f != math.Trunc(f) {
		return 0, false
	}
	return time.Duration(f) * time.Millisecond, true
}

// releaseLocked is the release of a lease's caller (Lease.Release) and of the
// preempt watchdog's forced Go half: settling when settleForLocked names a
// settle, else finishLocked at once. A lease already done or already
// settling is left alone — its end is due elsewhere (idempotent Release).
func (d *Dispatcher) releaseLocked(l *Lease) {
	if l.done || l.settling {
		return
	}
	if wait := d.settleForLocked(l); wait > 0 {
		d.settleLocked(l, wait)
		return
	}
	d.finishLocked(l)
}

// settleForLocked is the settle a release of l starts (0: none): only a
// slot-holding lease (a pass-through lease holds no unit a settle could keep)
// whose run context ended before the release (context.Cause set: client gone,
// deadline, preempt), on a dispatcher that is not closed, with the target's
// abort_settle_ms field of the lease's form — read from the policy in force
// at the release.
func (d *Dispatcher) settleForLocked(l *Lease) time.Duration {
	if !l.slotHeld || d.closed || l.runCtx == nil || context.Cause(l.runCtx) == nil {
		return 0
	}
	tp := d.policy.Load().Targets[l.st.origin]
	if l.stream {
		return tp.AbortSettleStream
	}
	return tp.AbortSettleNonstream
}

// settleLocked parks l as settling for wait: the units stay booked, the
// preempt release-latency sample (the Go half, MW18) is taken now — a victim
// leaves "in eviction" and covers as settling instead (maybePreemptLocked) —
// and the timer ends it through finishLocked.
func (d *Dispatcher) settleLocked(l *Lease, wait time.Duration) {
	d.preemptSettledLocked(l)
	l.settling = true
	l.st.settling++
	l.st.settlingWeight += l.weight
	if l.preempted {
		l.st.settlingVictims += l.weight
	}
	l.settleTimer = time.AfterFunc(wait, func() { d.endSettle(l) })
}

// endSettle is a settle timer's end of its lease.
func (d *Dispatcher) endSettle(l *Lease) {
	d.mu.Lock()
	opStart := time.Now()
	d.finishLocked(l) // a no-op when the reaper or Close came first
	d.opDone(opStart)
	d.mu.Unlock()
}

// leaveSettlingLocked takes a settling lease out of the settle books — called
// by finishLocked on every lease end, whoever ends it (timer, reaper, Close);
// a lease that never settled passes through untouched.
func (st *targetState) leaveSettlingLocked(l *Lease) {
	if !l.settling {
		return
	}
	l.settling = false
	if l.settleTimer != nil {
		l.settleTimer.Stop()
	}
	st.settling--
	st.settlingWeight -= l.weight
	if l.preempted {
		st.settlingVictims -= l.weight
	}
}

// closeSettlingLeases is Close's half of the settle (A03-W10): no timer
// starts any more, and every settling lease ends now with its timer stopped —
// a timer that already fired and waits for mu finds its lease done.
func (d *Dispatcher) closeSettlingLeases() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for _, st := range d.targets {
		if st.settling == 0 {
			continue
		}
		var ending []*Lease
		for l := range st.inflight {
			if l.settling {
				ending = append(ending, l)
			}
		}
		for _, l := range ending {
			d.finishLocked(l)
		}
	}
}
