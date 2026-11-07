package oai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelalias"
)

// The request watcher (design/02 §4.11.3 D8, B12): one goroutine per running
// inference request, in memory, that ends the request's run when the model
// API is switched off or the alias stops being the consumer's — the switch
// and a revocation act within one tick instead of at the end of a request
// that may run for its whole timeout. Non-stream also checks the same right
// before it writes (stillServes).
//
// Every reauthEvery the watcher also checks the request's model key again
// (design/02 §4.11.3 D6, masterplan K22; wave W02-08) through Deps.Reauth —
// the door's own check (handler.ReauthConsumer), never the knowledge
// credential path — and resolves the alias with the consumer the door reads
// now and a forced grant read (lead decision O-08-5 (a)): a revoked key, a
// withdrawn grant, all_models withdrawn or a tenant_scope that no longer
// shows the alias end a running request within one re-auth period, stream
// and non-stream alike. W01-8's revocation registry replaces both (K22).
//
// Every grant read of a running request — the period's, a tick's, the one
// right before a non-stream write — runs under grantReadDeadline, and the
// switch and the alias registry are checked in memory before it: a
// database that hangs (an exhausted pool, a locked grants table) delays the
// kill switch and the alias check by at most that deadline, never longer
// (Fremd-Review 2 H-1).

// watchTick is the watcher's period (design/02 §4.11.3 D8: "Takt 5 s").
// Lead decision O2 (a): a named constant, not a setting, until an operator
// need for another period is shown — it bounds how long a switched-off or
// revoked request keeps its lease (≤ one tick plus the upstream abort).
const watchTick = 5 * time.Second

// watchEvery is the period in use; tests shorten it (export_test.go).
var watchEvery = watchTick

// reauthTick is the re-authentication period of a running request
// (design/02 §4.11.3 D6: "alle 60 s"; masterplan K22). Lead decision O-08-1
// (a): a named constant beside watchTick, not a setting — W01-8's
// revocation registry replaces the re-check (K22), a setting would lose its
// reader then. It bounds how long a revoked key keeps a running request:
// one period while the auth database answers, reauthFailLimit periods plus
// the door's 1-s deadline while it does not (O-08-2; 121 s with the
// defaults) — for an all_models consumer and a grant consumer alike, since
// the grant read is bounded too (grantReadDeadline).
const reauthTick = 60 * time.Second

// reauthEvery is the period in use; tests shorten it (export_test.go).
var reauthEvery = reauthTick

// reauthFailLimit is how many failed re-checks in a row a committed stream
// survives (lead decision O-08-2 (b), N = 2). The re-check runs under the
// door's database semaphore (gateway.auth_db_concurrency) and fails after a
// second of waiting: ending a stream on the first failure would let a
// login spike at the door end every stream whose re-check falls into it,
// never ending one would let a revoked key run unbounded while the database
// is down. With 2: a revocation acts within one period while the database
// answers, within two while it does not. Before the commit — and for every
// non-stream request, which commits only with its answer — the first
// failure is the answer (503 auth_unavailable, x-should-retry false, G13).
// A re-check that answers "no longer valid" is no failure: it ends the
// request at once. A period is one verdict: its grant read failing or
// running past grantReadDeadline is the same failed check as a failed key
// check, in the same count (Fremd-Review 2 H-1).
const reauthFailLimit = 2

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
	// errKeyRevoked: the re-authentication says the key no longer
	// authenticates (revoked, disabled, expired; D6) ⇒ 401 invalid_api_key,
	// in a committed stream its error frame. W01-8's NOTIFY abort
	// (context.Cause == ErrCredentialRevoked) takes this place (K22).
	errKeyRevoked = fmt.Errorf("oai: the model key no longer authenticates: %w", context.Canceled)
	// errAuthUnavailable: the re-authentication could not run (the door's
	// database share; O-08-2) ⇒ 503 auth_unavailable, x-should-retry false.
	errAuthUnavailable = fmt.Errorf("oai: the model key could not be checked again: %w", context.Canceled)
)

// watchTarget is what the watcher re-checks: the consumer, the alias name it
// asked for, the id that name resolved to at entry and the registry
// generation read right before that Resolve (gen): the watcher starts from
// it, so a change between the entry and the watcher start is resolved on
// the first tick (Copilot review on PR #144).
type watchTarget struct {
	// ca is the consumer as the door read it last: the entry's, then the
	// one every passed re-authentication hands back — all_models and
	// tenant_scope can change under a running request (Fremd-Review 2 M-1).
	// Shared by the watcher and the check before a non-stream write.
	ca      *atomic.Pointer[auth.ConsumerAuth]
	name    string
	aliasID string
	gen     uint64
	// committed reports whether the request's answer has begun (a stream
	// past its commit); nil = never before the answer (non-stream). It
	// selects the re-auth failure rule (reauthFailLimit).
	committed func() bool
}

// newWatchTarget is the target of one request, starting from the entry's
// consumer.
func newWatchTarget(ca *auth.ConsumerAuth, name, aliasID string, gen uint64) watchTarget {
	t := watchTarget{ca: new(atomic.Pointer[auth.ConsumerAuth]), name: name, aliasID: aliasID, gen: gen}
	t.ca.Store(ca)
	return t
}

// watch starts the watcher of one request; stop ends it and returns once the
// goroutine is gone (no goroutine outlives its request); idempotent. cancel ends the
// request's run with the watcher's cause. A re-authentication in flight
// when stop is called is canceled, not waited out.
func (d Deps) watch(ctx context.Context, cancel context.CancelCauseFunc, t watchTarget) (stop func()) {
	done := make(chan struct{})
	wctx, quit := context.WithCancel(ctx)
	gen := t.gen
	go func() {
		defer close(done)
		tick := time.NewTicker(watchEvery)
		defer tick.Stop()
		var reauth <-chan time.Time
		if d.Reauth != nil {
			rt := time.NewTicker(reauthEvery)
			defer rt.Stop()
			reauth = rt.C
		}
		fails := 0
		for {
			var cause error
			select {
			case <-wctx.Done():
				return
			case <-tick.C:
				gen, cause = d.recheck(wctx, t, gen, false)
			case <-reauth:
				gen, fails, cause = d.reauthPeriod(wctx, t, gen, fails)
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

// reauthPeriod is one re-auth period (D6, K22) and one verdict: the door
// checks the key again and hands back the consumer as it reads it now
// (Fremd-Review 2 M-1), then the alias is resolved with that consumer and a
// forced grant read (O-08-5 (a): a grant withdrawn while the request runs
// does not move the alias generation, Fremd-Review 2 F2-3 of W02-07 — one
// HasGrant probe per running request and period). "No longer valid" ends
// the request; so do the switch and an alias the consumer no longer has. A
// failed key check and a failed grant read (an error or grantReadDeadline
// passed) are the same failed check (Fremd-Review 2 H-1): it ends the
// request before the answer has begun and, in a committed stream, at the
// reauthFailLimit-th failed period in a row (O-08-2); a period that passes
// resets the count.
func (d Deps) reauthPeriod(ctx context.Context, t watchTarget, gen uint64, fails int) (uint64, int, error) {
	fresh, err := d.Reauth(ctx, t.ca.Load())
	if err == nil {
		if fresh == nil {
			return gen, 0, errKeyRevoked
		}
		t.ca.Store(fresh)
		next, cause := d.recheck(ctx, t, gen, true)
		if cause == nil || errors.Is(cause, errGatewayOff) || errors.Is(cause, errModelGone) {
			return next, 0, cause
		}
		err = cause
	}
	select {
	case <-ctx.Done():
		return gen, fails, nil // stopped (or the run ended) while checking: no verdict
	default:
	}
	fails++
	if t.committed == nil || !t.committed() || fails >= reauthFailLimit {
		slog.Warn("gateway: model key re-check failed; the request ends", "failures", fails, "error", err)
		return gen, fails, errAuthUnavailable
	}
	slog.Warn("gateway: model key re-check failed; the stream runs until the next check", "failures", fails, "error", err)
	return gen, fails, nil
}

// recheck is one watcher check: the switch, and — when the alias registry
// moved since gen, or always (force: the re-auth period, the check right
// before the write) — the alias again, first in the registry (memory: a
// removed, disabled or no longer visible alias ends the request without a
// database read), then through Resolve, whose grant read runs under
// grantReadDeadline (H-1). A failed grant read during a tick keeps the run
// (the re-auth period and the pre-write check decide, fail-closed there);
// forced it is the caller's failed check.
func (d Deps) recheck(ctx context.Context, t watchTarget, gen uint64, force bool) (uint64, error) {
	if !d.Settings().Enabled {
		return gen, errGatewayOff
	}
	now := d.Aliases.Generation()
	if !force && now == gen {
		return gen, nil
	}
	ca := t.ca.Load()
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
		slog.Warn("gateway: watcher grant re-read failed; the re-auth period and the check before the write decide", "error", err)
		return gen, nil
	case !ok || a.ID != t.aliasID:
		return now, errModelGone
	}
	return now, nil
}

// stillServes is the same check right before the answer is written (D8:
// "Non-stream prüft dasselbe zusätzlich direkt vor dem Schreiben"), with the
// consumer the last re-authentication read (M-1) and the grant read under
// grantReadDeadline (H-1). nil = write; errGatewayOff / errModelGone = the
// 404s; anything else is a grant read error (503).
func (d Deps) stillServes(ctx context.Context, t watchTarget) error {
	_, cause := d.recheck(ctx, t, 0, true)
	return cause
}
