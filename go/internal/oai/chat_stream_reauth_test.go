package oai_test

// A key that stops authenticating ends its running requests (W01-8,
// masterplan K22): the model-key door's in-flight registry cancels the
// request context it handed to the route with auth.ErrCredentialRevoked,
// and the route answers that cause — 401 invalid_api_key before the commit,
// the error frame invalid_api_key after it — instead of reading it as a gone
// client. Here on the revoker (chat_stream_reauth_harness_test.go), the
// stand-in of that registry; the real door and registry over a database is
// the integration half. A narrowed policy (a withdrawn grant) does not end
// a running request — it applies from the next one (design/01 §4.6
// "Grenze"). Plus the envelope pins of the stream path (G13 of W02-08): the
// code-bug sentinels before and after the commit.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

// revokeFrameBound is how soon after the revocation the error frame is due
// at the client: the route reads the cause as soon as the canceled upstream
// read returns — no tick in between. revokeFrameSlack is the scheduler's
// share on a loaded host; under -race both are logged, not judged.
const (
	revokeFrameBound = 50 * time.Millisecond
	revokeFrameSlack = 200 * time.Millisecond
)

// revokeRig is a stream rig on one upstream that answers steps; entered
// (optional) gets a value when the upstream sees the request.
func revokeRig(t *testing.T, steps []sseStep, entered chan<- struct{}, opts ...rigOption) (*chatRig, *upstream, *upstreamEnd) {
	t.Helper()
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	u.setRespond(sseRespond(steps, entered, end))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
		append([]rigOption{withStreamSettings}, opts...)...)
	return rig, u, end
}

// drainEvents collects the stream's remaining events until its body ends
// (false: it did not end within d).
func drainEvents(each <-chan sseEvent, d time.Duration) ([]sseEvent, bool) {
	var evs []sseEvent
	timeout := time.After(d)
	for {
		select {
		case e, ok := <-each:
			if !ok {
				return evs, true
			}
			evs = append(evs, e)
		case <-timeout:
			return evs, false
		}
	}
}

// TestWatchTickPin: the watcher's period is the design's 5 s (design/02
// §4.11.3 D8, lead decision O2 (a): a named constant).
func TestWatchTickPin(t *testing.T) {
	if oai.WatchTick != 5*time.Second {
		t.Fatalf("watch tick %v, want 5s", oai.WatchTick)
	}
}

// TestRevokedKeyEndsStream: the registry revokes the key of a running
// stream ⇒ (committed) the error frame invalid_api_key as the last event,
// no [DONE], at once (≤ revokeFrameBound + slack), the upstream aborted,
// the lease free, no health fault of the row (Z3), the row's attempt
// canceled; (before the commit, the upstream holding before its first
// frame) 401 invalid_api_key with WWW-Authenticate. Red (the route without
// the ErrCredentialRevoked verdict): it reads the cause as a gone client —
// no frame, an empty 200.
func TestRevokedKeyEndsStream(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		rv := newRevoker()
		rig, u, end := revokeRig(t, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil)
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rv.wrap(rig.router)), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		at := time.Now()
		if n := rv.revoke(); n != 1 {
			t.Fatalf("revoked %d running requests, want 1", n)
		}
		evs, ended := drainEvents(each, 5*time.Second)
		a := streamAnswer{events: evs}
		code, last := a.errorFrame(t)
		took := time.Duration(-1) // -1: no frame
		for _, e := range evs {
			if strings.Contains(e.data, `"error"`) {
				took = e.at.Sub(at)
				break
			}
		}
		t.Logf("revoke → error frame %q after %v (bound %v + %v; race %v)", code, took, revokeFrameBound, revokeFrameSlack, raceEnabled)
		if !ended || code != oai.CodeInvalidAPIKey || !last || a.dones() != 0 {
			t.Fatalf("frame %q last %v, [DONE] %d, body ended %v: %+v — want the invalid_api_key error frame as the last event, no [DONE]",
				code, last, a.dones(), ended, evs)
		}
		if !raceEnabled && took > revokeFrameBound+revokeFrameSlack {
			t.Errorf("error frame %v after the revocation, want ≤ %v + %v: the route waited for something else than the cause", took, revokeFrameBound, revokeFrameSlack)
		}
		if !end.wait(time.Second) || !end.canceled || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
			t.Error("the upstream was not aborted or the lease not freed")
		}
		if n := consecutiveFails(rig.pool, "spark"); n != 0 {
			t.Errorf("consecutive_fails %d, want 0 (Z3: a revocation is no fault of the row)", n)
		}
		// The row is written after the frame went out: wait for it.
		waitFor(time.Second, func() bool { return len(rig.rows.all()) == 1 })
		if rows := rig.rows.all(); len(rows) != 1 || len(rows[0].Attempts) == 0 || rows[0].Attempts[0].Class != backends.ClassCanceled.String() {
			t.Errorf("rows %+v, want one row whose attempt is canceled", rows)
		}
		if !waitFor(time.Second, func() bool { return rv.running.Load() == 0 }) {
			t.Errorf("%d requests still inside the registry", rv.running.Load())
		}
	})
	t.Run("before the commit", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		rv := newRevoker()
		entered := make(chan struct{}, 1)
		rig, u, end := revokeRig(t, []sseStep{{hold: hold}}, entered)
		srv := serveHTTP(t, rv.wrap(rig.router))
		done := make(chan streamAnswer, 1)
		go func() { done <- postStream(context.Background(), t, srv, streamBody("chat")) }()
		waitEnter(t, entered)
		at := time.Now()
		if n := rv.revoke(); n != 1 {
			t.Fatalf("revoked %d running requests, want 1", n)
		}
		var a streamAnswer
		select {
		case a = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("no answer within 5 s of the revocation")
		}
		t.Logf("revoke before the commit → %d after %v: %s", a.status, a.end.Sub(at), strings.TrimSpace(a.raw))
		if a.status != http.StatusUnauthorized || a.header.Get("WWW-Authenticate") == "" || !strings.Contains(a.raw, `"code":"invalid_api_key"`) {
			t.Fatalf("answer %d %v %s, want 401 invalid_api_key with WWW-Authenticate", a.status, a.header, a.raw)
		}
		if !end.wait(time.Second) || !end.canceled || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
			t.Error("the upstream was not aborted or the lease not freed")
		}
		if !waitFor(time.Second, func() bool { return rv.running.Load() == 0 }) {
			t.Errorf("%d requests still inside the registry", rv.running.Load())
		}
	})
}

// TestGrantWithdrawnKeepsRunningStream is the boundary of design/01 §4.6
// ("Grenze"): a narrowed policy applies from the next request. A consumer
// without all_models whose grant is withdrawn during a committed stream ⇒
// the stream runs to its [DONE] — the watcher reads the grant only when the
// alias registry moved, and it did not; no grant read during the run — and
// the next request is 404 model_not_found. Red (a tick that always reads
// the grant, force): the stream ends with the error frame model_not_found
// at the first tick after the withdrawal.
func TestGrantWithdrawnKeepsRunningStream(t *testing.T) {
	const tick = 50 * time.Millisecond
	t.Cleanup(oai.SetWatchEvery(tick))
	hold := make(chan struct{})
	h := &grantHang{} // counts the grant reads; none hangs
	rig, u, end := revokeRig(t, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold},
		{data: sglangChunk(" again", "stop")}, {data: sglangUsage}, {data: "[DONE]"}}, nil, withGrantHang(h))
	ca := urgentApp()
	ca.AllModels = false
	rig.ca.Store(ca)
	rig.setGrant(idChat, true)
	srv := serveHTTP(t, rig.router)
	ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
	defer func() { _ = ls.Close() }()
	if _, ok := nextData(each, 5*time.Second); !ok {
		close(hold)
		t.Fatal("no frame")
	}
	reads := h.reads.Load()
	rig.setGrant(idChat, false)
	time.Sleep(10 * tick) // ten watcher ticks with the grant withdrawn
	close(hold)
	evs, ended := drainEvents(each, 5*time.Second)
	a := streamAnswer{events: evs}
	code, _ := a.errorFrame(t)
	t.Logf("grant withdrawn mid-stream: error frame %q, [DONE] %d, grant reads during the run %d", code, a.dones(), h.reads.Load()-reads)
	if !ended || code != "" || a.dones() != 1 {
		t.Fatalf("frame %q, [DONE] %d, body ended %v: %+v — want the stream to run to its [DONE] (the withdrawal applies from the next request)",
			code, a.dones(), ended, evs)
	}
	if n := h.reads.Load() - reads; n != 0 {
		t.Errorf("%d grant reads during the run, want 0: the alias registry did not move", n)
	}
	if !end.wait(time.Second) || end.canceled || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
		t.Error("the upstream was aborted or the lease not freed")
	}
	next := postStream(context.Background(), t, srv, streamBody("chat"))
	if next.status != http.StatusNotFound || !strings.Contains(next.raw, `"code":"model_not_found"`) {
		t.Fatalf("next request %d %s, want 404 model_not_found", next.status, next.raw)
	}
}

// TestRevokedKeyEndsNonStream: the registry revokes the key of a running
// non-stream request ⇒ 401 invalid_api_key with WWW-Authenticate, never a
// 200, the upstream aborted within 1 s, the lease free. Red (the route
// without the ErrCredentialRevoked verdict): an empty 200.
func TestRevokedKeyEndsNonStream(t *testing.T) {
	rv := newRevoker()
	u := newUpstream(t, answerSGLang)
	b := newBlocker()
	defer b.open()
	u.setRespond(b.respond(answerSGLang))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	srv := serveHTTP(t, rv.wrap(rig.router))
	done := make(chan streamAnswer, 1)
	go func() { done <- postStream(context.Background(), t, srv, chatBody("chat")) }()
	waitEntered(t, b, 1)
	at := time.Now()
	if n := rv.revoke(); n != 1 {
		t.Fatalf("revoked %d running requests, want 1", n)
	}
	var a streamAnswer
	select {
	case a = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no answer within 5 s of the revocation")
	}
	t.Logf("revoke → %d after %v: %s", a.status, a.end.Sub(at), strings.TrimSpace(a.raw))
	if a.status != http.StatusUnauthorized || !strings.Contains(a.raw, `"code":"invalid_api_key"`) || a.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("answer %d %v %s, want 401 invalid_api_key with WWW-Authenticate", a.status, a.header, a.raw)
	}
	select {
	case abortedAt := <-b.aborted:
		if d := abortedAt.Sub(at); d > time.Second {
			t.Errorf("the upstream was aborted %v after the revocation, want ≤ 1 s", d)
		}
	case <-time.After(time.Second):
		t.Error("the upstream was not aborted within 1 s")
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
		t.Error("the lease is still held")
	}
	if !waitFor(time.Second, func() bool { return rv.running.Load() == 0 }) {
		t.Errorf("%d requests still inside the registry", rv.running.Load())
	}
}

// failNextPlanner's acquirer fails Next with a plain error — on the walk's
// failover after the first row failed: the admission writer's
// internal_error (a code-bug class, K29).
type failNextPlanner struct{ oai.AdmissionPlanner }

func (p failNextPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = failNext{plan.Acquirer}
	}
	return plan, err
}

type failNext struct{ oai.RequestAcquirer }

func (failNext) Next(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	return nil, nil, errors.New("W02-08 G13 probe: a failover acquire on a broken path")
}

// TestStreamCodeBugSentinels is G13's second pin: a code-bug sentinel of
// the admission in the stream path is the 503 internal_error before the
// commit (x-should-retry false) and the error frame internal_error after
// it — never a second status.
func TestStreamCodeBugSentinels(t *testing.T) {
	t.Run("before the commit", func(t *testing.T) {
		u := newUpstream(t, "")
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
			withDeps(func(d *oai.Deps) { d.Planner = chFaultPlanner{AdmissionPlanner: d.Planner, err: dispatch.ErrInvalidClass} }))
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if a.status != http.StatusServiceUnavailable || a.header.Get("x-should-retry") != "false" || !strings.Contains(a.raw, `"code":"internal_error"`) {
			t.Fatalf("answer %d %s, want 503 internal_error, x-should-retry false", a.status, a.raw)
		}
	})
	t.Run("after the commit", func(t *testing.T) {
		fastPhaseB(t, 100*time.Millisecond)
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		var failedAt atomic.Int64
		u1.setRespond(status502(400*time.Millisecond, &failedAt))
		rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 4, 1,
			withStreamSettings, withWait(100*time.Millisecond), withStream(0, 10*time.Second, 5*time.Second),
			withDeps(func(d *oai.Deps) { d.Planner = failNextPlanner{d.Planner} }))
		var bc byteClock
		a := postStream(context.Background(), t, serveHTTP(t, bc.wrap(rig.router)), streamBody("chat"))
		code, last := a.errorFrame(t)
		if a.status != http.StatusOK || code != oai.CodeInternalError || !last || bc.headers.Load() != 1 {
			t.Fatalf("answer %d frame %q last %v, %d WriteHeader: %s — want the internal_error frame after the commit, one status",
				a.status, code, last, bc.headers.Load(), a.raw)
		}
	})
}
