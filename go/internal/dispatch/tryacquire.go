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
// Pass-through targets (disabled / no declared policy) admit in-house
// classes unconditionally, exactly like Acquire — behavior neutrality with
// empty policy; an external request is never pass-through (A03-W3: refused
// with ErrExternalNotOffered where the target does not offer it, the
// llm.Admission try path for /v1 is A03-W17). Same door check order as
// Acquire: ctx.Err(), then the class
// (ErrInvalidClass outside the class table, A03-W1), then the principal gate
// (ErrPrincipalConflict, ErrClassUnauthorized — I-K10, W01-7), then B8.
func (d *Dispatcher) TryAcquire(ctx context.Context, req Request) (*Lease, context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err // canceled ctx never admits
	}
	if !req.Class.valid() {
		return nil, nil, d.refuseInvalidClass("try-acquire", req.Class, req.Target.Origin, req.Role)
	}
	principal := principalFromContext(ctx)
	if err := d.refusePrincipal("try-acquire", principal, req); err != nil {
		return nil, nil, err
	}
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
	// Same immediate half as Acquire (admitNowLocked): pass-through for the
	// in-house classes on a target without slots, ErrExternalNotOffered for
	// an external request on a target that does not offer it.
	l, err := d.admitNowLocked(st, w, now)
	d.opDone(opStart)
	d.mu.Unlock()
	if l != nil {
		return l, runCtx, nil
	}
	cancel(context.Canceled)
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, ErrWouldBlock
}
