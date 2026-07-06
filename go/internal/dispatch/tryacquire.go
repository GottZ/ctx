package dispatch

import (
	"context"
	"errors"
	"time"
)

// ErrWouldBlock is TryAcquire's immediate answer when the request is not
// eligible right now (slot busy, waiters ahead, or the herald term gating a
// background acquire). It is deliberately NOT part of IsRejection: a
// would-block is defer semantics for consumers that must not wait (Q-I3,
// design/03 §4.4 — no blocking acquire under a held DB transaction), never a
// client-facing 429. Like every admission error it is terminal for a chain
// walk (doctrine §4.3: not an attempt — no Classify, no health report, no
// llmlog row).
var ErrWouldBlock = errors.New("dispatch: target busy, would block")

// TryAcquire is the non-blocking Acquire variant (Q-I3, design/03 §4.4,
// built for D3-W5's tx-holding backfill): an eligible-now request is
// admitted through the exact same predicate and lease bookkeeping as
// Acquire; every other outcome returns ErrWouldBlock immediately — no
// waiter is enqueued, no wait slot taken, no preempt opened (preemption is
// reserved for WAITING interactive acquires, design/02 §4.2.1). No-barging
// holds by construction: eligibleNowLocked admits only when no same-or-
// higher-class waiter is ahead, so a try can never overtake the queue.
// Pass-through targets (disabled / no declared policy) admit
// unconditionally, exactly like Acquire — behavior neutrality with empty
// policy.
func (d *Dispatcher) TryAcquire(ctx context.Context, req Request) (*Lease, context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err // canceled ctx never admits
	}
	principal := principalFromContext(ctx)
	class := req.Class
	if class == ClassInteractive && principal.ApiKeyID == "" {
		// Same B8 class authorization as Acquire (design/03 §4.1.1): every
		// admission door downgrades an unprincipaled interactive request
		// fail-closed toward the protected good.
		class = ClassBackground
		d.classDowngrades.Add(1)
		d.logger.Error("dispatch: interactive try-acquire without ctx-bound principal downgraded to background",
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
		runCtx:     runCtx,
		cancel:     cancel,
	}

	d.mu.Lock()
	opStart := time.Now()
	st := d.targetLocked(canonicalOrigin(req.Target.Origin))
	slots := d.slotsLocked(st.origin)
	if slots <= 0 {
		// Pass-through: disabled or no declared policy — exactly today's
		// behavior, tracked for telemetry only (same clause as Acquire).
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
	d.opDone(opStart)
	d.mu.Unlock()
	cancel(context.Canceled)
	return nil, nil, ErrWouldBlock
}
