package oai

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelalias"
)

// The request watcher (design/02 §4.11.3 D8, B12): one goroutine per running
// inference request, in memory, that ends the request's run when the model
// API is switched off or the alias stops being the consumer's — the switch
// acts within one tick instead of at the end of a request that may run for
// its whole timeout. Non-stream also checks the same right before it writes
// (stillServes).
//
// A key that stops authenticating — revoked, expired, its consumer
// deactivated, its tenant suspended or gone, its tenant_scope moved — is not
// the watcher's: the model-key door's in-flight registry
// (handler.InflightRegistry, W01-8, masterplan K22) ends the request's
// context with auth.ErrCredentialRevoked, at once on a local revocation, on
// the NOTIFY of migration 165, on the key's expiry timer or at the latest at
// its periodic sweep. The run, a child of that context, ends with it; the
// route reads the cause (context.Cause) and answers 401 invalid_api_key, a
// committed stream its error frame. The periodic re-authentication of
// W02-08 (every 60 s through the door) is gone with it. A narrowed policy —
// a withdrawn grant, all_models withdrawn, another tenant_scope — applies
// from the next request (design/01 §4.6 "Grenze"); the watcher still ends a
// request whose alias is gone, disabled or no longer visible, and checks the
// grant again when the alias registry moved.
//
// Every grant read of a running request — a tick's, the one right before a
// non-stream write — runs under grantReadDeadline, and the switch and the
// alias registry are checked in memory before it: a database that hangs (an
// exhausted pool, a locked grants table) delays the kill switch and the
// alias check by at most that deadline, never longer (Fremd-Review 2 H-1).

// watchTick is the watcher's period (design/02 §4.11.3 D8: "Takt 5 s").
// Lead decision O2 (a): a named constant, not a setting, until an operator
// need for another period is shown — it bounds how long a switched-off or
// revoked request keeps its lease (≤ one tick plus the upstream abort).
const watchTick = 5 * time.Second

// watchEvery is the period in use; tests shorten it (export_test.go).
var watchEvery = watchTick

// grantReadDeadline bounds every grant read of the gateway's alias
// resolution — at the entry and in the watcher (Fremd-Review 2 H-1): the
// door's own database deadline (handler modelAuthDBDeadline, 1 s), a named
// constant beside it, not a setting (lead decision of the hardening). The
// entry and the door share one database pool; a read that has not answered
// within the door's own budget will not answer a running request in time
// either, and without the bound a hanging read held the watcher — and with
// it the kill switch — for as long as the database hung.
const grantReadDeadline = time.Second

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

// watchTarget is what the watcher re-checks: the consumer as the door read
// it at entry, the alias name it asked for, the id that name resolved to at
// entry and the registry generation read right before that Resolve (gen):
// the watcher starts from it, so a change between the entry and the watcher
// start is resolved on the first tick (Copilot review on PR #144).
type watchTarget struct {
	ca      *auth.ConsumerAuth
	name    string
	aliasID string
	gen     uint64
}

// newWatchTarget is the target of one request.
func newWatchTarget(ca *auth.ConsumerAuth, name, aliasID string, gen uint64) watchTarget {
	return watchTarget{ca: ca, name: name, aliasID: aliasID, gen: gen}
}

// watch starts the watcher of one request; stop ends it and returns once the
// goroutine is gone (no goroutine outlives its request); idempotent. cancel ends the
// request's run with the watcher's cause. A grant read in flight when stop
// is called is canceled, not waited out.
func (d Deps) watch(ctx context.Context, cancel context.CancelCauseFunc, t watchTarget) (stop func()) {
	done := make(chan struct{})
	wctx, quit := context.WithCancel(ctx)
	gen := t.gen
	go func() {
		defer close(done)
		tick := time.NewTicker(watchEvery)
		defer tick.Stop()
		for {
			var cause error
			select {
			case <-wctx.Done():
				return
			case <-tick.C:
				gen, cause = d.recheck(wctx, t, gen, false)
			}
			if wctx.Err() != nil {
				return // stopped (or the run ended) while checking: no verdict
			}
			if cause != nil {
				cancel(cause)
				return
			}
		}
	}()
	return func() {
		quit()
		<-done
	}
}

// recheck is one watcher check: the switch, and — when the alias registry
// moved since gen, or always (force: the check right before the write) —
// the alias again, first in the registry (memory: a removed, disabled or no
// longer visible alias ends the request without a database read), then
// through Resolve, whose grant read runs under grantReadDeadline (H-1). A
// failed grant read during a tick keeps the run (the check before the write
// decides, fail-closed there; the next tick reads again while the
// generation stays moved); forced it is the caller's failed check.
func (d Deps) recheck(ctx context.Context, t watchTarget, gen uint64, force bool) (uint64, error) {
	if !d.Settings().Enabled {
		return gen, errGatewayOff
	}
	now := d.Aliases.Generation()
	if !force && now == gen {
		return gen, nil
	}
	ca := t.ca
	if a, ok := modelalias.Lookup(d.Aliases.Snapshot(), t.name, aliasScope(ca)); !ok || a.ID != t.aliasID {
		return now, errModelGone
	}
	rctx, cancel := context.WithTimeout(ctx, grantReadDeadline)
	defer cancel()
	a, ok, err := d.Resolve(rctx, ca, t.name)
	switch {
	case err != nil && force:
		return now, err
	case err != nil:
		slog.Warn("gateway: watcher grant re-read failed; the next tick and the check before the write decide", "error", err)
		return gen, nil
	case !ok || a.ID != t.aliasID:
		return now, errModelGone
	}
	return now, nil
}

// stillServes is the same check right before the answer is written (D8:
// "Non-stream prüft dasselbe zusätzlich direkt vor dem Schreiben"), with the
// grant read under grantReadDeadline (H-1). nil = write; errGatewayOff / errModelGone = the
// 404s; anything else is a grant read error (503).
func (d Deps) stillServes(ctx context.Context, t watchTarget) error {
	_, cause := d.recheck(ctx, t, 0, true)
	return cause
}
