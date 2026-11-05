package oai

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/auth"
)

// The request watcher (design/02 §4.11.3 D8, B12): one goroutine per running
// inference request, in memory, that ends the request's run when the model
// API is switched off or the alias stops being the consumer's — the switch
// and a revocation act within one tick instead of at the end of a request
// that may run for its whole timeout. Non-stream also checks the same right
// before it writes (stillServes). The 60-s re-authentication of streams is
// W02-08 (K22).

// watchTick is the watcher's period (design/02 §4.11.3 D8: "Takt 5 s").
// Lead decision O2 (a): a named constant, not a setting, until an operator
// need for another period is shown — it bounds how long a switched-off or
// revoked request keeps its lease (≤ one tick plus the upstream abort).
const watchTick = 5 * time.Second

// watchEvery is the period in use; tests shorten it (export_test.go).
var watchEvery = watchTick

// The causes the watcher cancels a run with. The handler reads them through
// context.Cause and answers them itself — the client is still there.
//
// Each wraps context.Canceled: net/http hands the cause to the aborted wire
// call, and the walker classifies by it — a cancellation (ClassCanceled: no
// health report, the walk stops), never a server_fault of a healthy row
// (Fremd-Review 1 F-3).
var (
	// errGatewayOff: gateway.enabled went false during the request ⇒ the
	// one 404 of a switched-off model API (the same bytes as at the gate).
	errGatewayOff = fmt.Errorf("oai: the model API was switched off during the request: %w", context.Canceled)
	// errModelGone: the alias is gone, disabled, renamed away or no longer
	// granted ⇒ the one 404 model_not_found (B3).
	errModelGone = fmt.Errorf("oai: the model is no longer available to the consumer: %w", context.Canceled)
	// errShutdown: the server is stopping (D7) ⇒ 503, retry elsewhere.
	errShutdown = fmt.Errorf("oai: the server is shutting down: %w", context.Canceled)
)

// watchTarget is what the watcher re-checks: the consumer, the alias name it
// asked for, the id that name resolved to at entry and the registry
// generation read right before that Resolve (gen): the watcher starts from
// it, so a change between the entry and the watcher start is resolved on
// the first tick (Copilot review on PR #144).
type watchTarget struct {
	ca      *auth.ConsumerAuth
	name    string
	aliasID string
	gen     uint64
}

// watch starts the watcher of one request; stop ends it and returns once the
// goroutine is gone (no goroutine outlives its request); idempotent. cancel ends the
// request's run with the watcher's cause.
func (d Deps) watch(ctx context.Context, cancel context.CancelCauseFunc, t watchTarget) (stop func()) {
	done := make(chan struct{})
	quit := make(chan struct{})
	gen := t.gen
	go func() {
		defer close(done)
		tick := time.NewTicker(watchEvery)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			next, cause := d.recheck(ctx, t, gen, false)
			gen = next
			if cause != nil {
				cancel(cause)
				return
			}
		}
	}()
	// An atomic flag, not sync.Once: the oai wire census refuses every .Do
	// call in this package (internal/chat TestOAIPackageHasNoWireCalls).
	var stopped atomic.Bool
	return func() {
		if stopped.CompareAndSwap(false, true) {
			close(quit)
		}
		<-done
	}
}

// recheck is one watcher check: the switch, and — when the alias registry
// moved since gen, or always (force, the check right before the write) —
// Resolve again. A failed grant read during a tick keeps the run (the
// pre-write check decides, fail-closed there); before the write it is the
// answer's own fault.
func (d Deps) recheck(ctx context.Context, t watchTarget, gen uint64, force bool) (uint64, error) {
	if !d.Settings().Enabled {
		return gen, errGatewayOff
	}
	now := d.Aliases.Generation()
	if !force && now == gen {
		return gen, nil
	}
	a, ok, err := d.Resolve(ctx, t.ca, t.name)
	switch {
	case err != nil && force:
		return now, err
	case err != nil:
		slog.Warn("gateway: watcher grant re-read failed; the check before the write decides", "error", err)
		return gen, nil
	case !ok || a.ID != t.aliasID:
		return now, errModelGone
	}
	return now, nil
}

// stillServes is the same check right before the answer is written (D8:
// "Non-stream prüft dasselbe zusätzlich direkt vor dem Schreiben"). nil =
// write; errGatewayOff / errModelGone = the 404s; anything else is a grant
// read error (503).
func (d Deps) stillServes(ctx context.Context, t watchTarget) error {
	_, cause := d.recheck(ctx, t, 0, true)
	return cause
}
