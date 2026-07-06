package events

import (
	"context"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedcache"
)

// tryAdmitter is the non-blocking admission surface the Q-I3 tx guard needs
// on top of the consumer-facing dispatch.Admitter (design/03 §4.4). The
// process-wide *dispatch.Dispatcher provides both doors; the interface stays
// local so dispatch.Admitter keeps its deliberately blocking-only shape.
type tryAdmitter interface {
	dispatch.Admitter
	TryAcquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error)
}

// txGuardAdmitter enforces Q-I3 for the span of ONE held row-lock tx
// (design/03 §4.4, D3-W5): the first chain target's background lease was
// acquired BEFORE BeginTx (lease-then-tx order) and is handed through on its
// first matching acquire; EVERY other acquire under the tx — failover
// targets, or a pick whose chain starts on a different origin than the
// peeked block's — goes through non-blocking TryAcquire. The rule is
// MECHANICAL, not policy-conditional: a hot-reloaded target policy can never
// re-open a blocking acquire under the open tx (the TOCTOU window of the
// earlier policy-conditional rule), and under pass-through policy TryAcquire
// admits unconditionally, so nothing is lost. Single-goroutine use only —
// EmbedChain walks its chain sequentially (no mutex by design).
type txGuardAdmitter struct {
	try       tryAdmitter
	preOrigin string // normalized origin the pre-acquired lease is valid for
	preLease  *dispatch.Lease
	preCtx    context.Context
}

// Acquire satisfies dispatch.Admitter for embedcache.EmbedChain: hand the
// pre-acquired lease through exactly once (origin-matched — a model-less
// first link means EmbedChain's first acquire is a LATER link on another
// origin), otherwise answer via the non-blocking door.
func (a *txGuardAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	if a.preLease != nil {
		if origin, err := dispatch.NormalizeOrigin(req.Target.Origin); err == nil && origin == a.preOrigin {
			l, rc := a.preLease, a.preCtx
			a.preLease, a.preCtx = nil, nil
			return l, rc, nil
		}
	}
	return a.try.TryAcquire(ctx, req)
}

// acquireBackfillLease performs the Q-I3 pre-tx admission for one backfill
// round: it resolves the FIRST wire target EmbedChain will attempt on the
// given chain (the first link carrying a model for the role — model-less
// links are skipped without an acquire, embedcache.go) and BLOCKS on its
// background lease while NO transaction is open. Returned are the tx-guard
// admission wrapping the held lease and its release func — release is
// idempotent and must be deferred by the caller: it settles the lease on
// every path where EmbedChain never claims it (empty pick, mismatched
// in-tx chain, error before the wire). A chain without any attemptable
// link yields a guard without pre-lease (EmbedChain then exhausts the
// chain acquire-free; a TryAcquire-only walk stays covered).
func acquireBackfillLease(ctx context.Context, adm embedcache.Admission, chain []backends.Backend, role string) (embedcache.Admission, func(), error) {
	if adm.Admitter == nil {
		// Same loud zero-Admission doctrine as embedcache (MW5, I-D1) —
		// just surfaced before the tx instead of inside it.
		return embedcache.Admission{}, nil, fmt.Errorf("events: backfill without dispatch admitter (I-D1)")
	}
	try, ok := adm.Admitter.(tryAdmitter)
	if !ok {
		// Fail loud: silently falling back to blocking Acquire would
		// reintroduce the S7 hazard the guard exists to prevent.
		return embedcache.Admission{}, nil, fmt.Errorf("events: backfill admitter %T lacks TryAcquire — Q-I3 (lease before tx) cannot be enforced", adm.Admitter)
	}

	guard := &txGuardAdmitter{try: try}
	release := func() {}
	for i := range chain {
		if chain[i].ModelFor(role).Model == "" {
			continue
		}
		lease, runCtx, err := try.Acquire(ctx, dispatch.Request{
			Target: dispatch.Target{Origin: chain[i].Host}, // Acquire normalizes defensively (design/01 §4.3)
			Class:  adm.Class,
			Role:   role,
		})
		if err != nil {
			// Admission errors are terminal, not attempts (doctrine §4.3).
			return embedcache.Admission{}, nil, err
		}
		if origin, nerr := dispatch.NormalizeOrigin(chain[i].Host); nerr == nil {
			guard.preOrigin = origin
			guard.preLease = lease
			guard.preCtx = runCtx
			release = lease.Release // idempotent (B1)
		} else {
			// Unnormalizable origin: the guard could never match it, so the
			// lease would leak as "pre" — settle it and let the walk go
			// through TryAcquire (the wire attempt will fail on its own).
			lease.Release()
		}
		break
	}
	return embedcache.Admission{Admitter: guard, Class: adm.Class}, release, nil
}
