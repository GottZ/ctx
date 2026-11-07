package oai_test

// The re-authentication of running requests (W02-08; design/02 §4.11.3 D6,
// masterplan K22) and the envelope pins of the stream path (G13): a revoked
// key, the failure rule N = 2 after the commit (lead decision O-08-2), the
// forced grant read of the re-auth period (O-08-5), re-auth for non-stream
// (Z6), the code-bug sentinels before and after the commit. The real door
// (handler.ReauthConsumer over a database) is the integration half
// (chat_stream_reauth_integration_test.go).

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

// errAuthDB stands for ErrModelAuthUnavailable (the door's database share).
var errAuthDB = errors.New("model auth: check unavailable (fake)")

// reauthRig is a stream rig on one upstream that sends a frame and then
// holds until hold closes (or sends frames over d when d > 0), with a
// shortened re-auth period and the fake.
func reauthRig(t *testing.T, f *reauthFake, frames []sseStep, opts ...rigOption) (*chatRig, *upstream, *upstreamEnd) {
	t.Helper()
	t.Cleanup(oai.SetReauthEvery(100 * time.Millisecond))
	end := newUpstreamEnd()
	u := newUpstream(t, "")
	u.setRespond(sseRespond(frames, nil, end))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
		append([]rigOption{withStreamSettings, withReauth(f)}, opts...)...)
	return rig, u, end
}

// TestReauthPins: the re-auth period is the design's 60 s (O-08-1 (a): a
// named constant next to the watcher tick) and the failure limit 2 (O-08-2).
func TestReauthPins(t *testing.T) {
	if oai.ReauthTick != 60*time.Second || oai.ReauthFailLimit != 2 || oai.WatchTick != 5*time.Second {
		t.Fatalf("reauth tick %v, limit %d, watch tick %v — want 60s, 2, 5s", oai.ReauthTick, oai.ReauthFailLimit, oai.WatchTick)
	}
}

// TestRevokedKeyEndsStream is G10's revocation half on the fake (the real
// door: TestRevokedKeyEndsStreamOnReauth): the re-auth answers "no longer
// valid" ⇒ a committed stream ends with the error frame invalid_api_key
// within a period + 1 s (lease free, upstream aborted, no health fault —
// Z3), a stream before its commit with 401 invalid_api_key and
// WWW-Authenticate. Red: no re-auth in the watcher — the stream runs on.
func TestRevokedKeyEndsStream(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		f := &reauthFake{}
		f.set(reauthAnswer{ok: true}, reauthAnswer{ok: false})
		rig, u, end := reauthRig(t, f, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		e, ok := nextData(each, 2*100*time.Millisecond+time.Second)
		if !ok || !strings.Contains(e.data, `"code":"invalid_api_key"`) {
			t.Fatalf("frame %q, want the invalid_api_key error frame within the second period + 1 s", e.data)
		}
		if !end.wait(time.Second) || !end.canceled || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
			t.Error("the upstream was not aborted or the lease not freed")
		}
		if n := consecutiveFails(rig.pool, "spark"); n != 0 {
			t.Errorf("consecutive_fails %d, want 0 (Z3: a revocation is no fault of the row)", n)
		}
		// The row is written after the frame went out: wait for it.
		waitFor(time.Second, func() bool { return len(rig.rows.all()) == 1 })
		if rows := rig.rows.all(); len(rows) != 1 || rows[0].Attempts[0].Class != backends.ClassCanceled.String() {
			t.Errorf("rows %+v, want one row whose attempt is canceled", rows)
		}
	})
	t.Run("before the commit", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		f := &reauthFake{}
		f.set(reauthAnswer{ok: false})
		rig, _, _ := reauthRig(t, f, []sseStep{{hold: hold}})
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if a.status != http.StatusUnauthorized || a.header.Get("WWW-Authenticate") == "" || !strings.Contains(a.raw, `"code":"invalid_api_key"`) {
			t.Fatalf("answer %d %v %s, want 401 invalid_api_key with WWW-Authenticate", a.status, a.header, a.raw)
		}
	})
}

// TestReauthFailureRule is lead decision O-08-2 (b), N = 2, after the
// commit: (i) one failure, then a success ⇒ the stream runs on and the
// count starts again (alternating failures never end it); (ii) two
// failures in a row ⇒ the error frame auth_unavailable, the lease free;
// (iii) "no longer valid" ends it at once (TestRevokedKeyEndsStream). Red:
// N = 1 ((i) ends), no limit ((ii) runs on).
func TestReauthFailureRule(t *testing.T) {
	t.Run("alternating failures run through", func(t *testing.T) {
		f := &reauthFake{}
		script := make([]reauthAnswer, 0, 40)
		for i := 0; i < 20; i++ {
			script = append(script, reauthAnswer{err: errAuthDB}, reauthAnswer{ok: true})
		}
		f.set(script...)
		steps := []sseStep{{data: sglangChunk("hello", "")}, {wait: 600 * time.Millisecond, data: sglangChunk(" from", "")},
			{wait: 600 * time.Millisecond, data: sglangChunk(" the engine", "stop")}, {data: "[DONE]"}}
		rig, _, _ := reauthRig(t, f, steps)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		checkRelay(t, a)
		if n := f.calls.Load(); n < 6 {
			t.Errorf("%d re-auth calls during 1.2 s at 100 ms, want >= 6", n)
		}
	})
	t.Run("two failures in a row end it", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		f := &reauthFake{}
		f.set(reauthAnswer{ok: true}, reauthAnswer{err: errAuthDB}, reauthAnswer{err: errAuthDB})
		rig, u, _ := reauthRig(t, f, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		code, last := a.errorFrame(t)
		if code != oai.CodeAuthUnavailable || !last || a.dones() != 0 || f.calls.Load() != 3 {
			t.Fatalf("frame %q last %v, [DONE] %d, %d re-auth calls: %s — want auth_unavailable after the second failure in a row",
				code, last, a.dones(), f.calls.Load(), a.raw)
		}
		if !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
			t.Error("the lease is still held")
		}
	})
}

// TestReauthErrorBeforeTheCommit is G13's first pin: a re-auth that cannot
// run before the answer began — a stream before its commit, any
// non-stream request — is 503 auth_unavailable with x-should-retry false.
func TestReauthErrorBeforeTheCommit(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "non-stream"}[stream], func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			f := &reauthFake{}
			f.set(reauthAnswer{err: errAuthDB}, reauthAnswer{ok: true}) // one failure, then the check works again
			rig, _, _ := reauthRig(t, f, []sseStep{{hold: hold}})
			body := chatBody("chat")
			if stream {
				body = streamBody("chat")
			} else {
				b := newBlocker()
				defer b.open()
				u2 := newUpstream(t, answerSGLang)
				u2.setRespond(b.respond(answerSGLang))
				rig = newChatRig(t, []backends.Backend{u2.row("spark", backends.TrustFull, 10)}, 4, 1, withReauth(f))
			}
			a := postStream(context.Background(), t, serveHTTP(t, rig.router), body)
			if a.status != http.StatusServiceUnavailable || a.header.Get("x-should-retry") != "false" || !strings.Contains(a.raw, `"code":"auth_unavailable"`) {
				t.Fatalf("answer %d %v %s, want 503 auth_unavailable, x-should-retry false", a.status, a.header, a.raw)
			}
			if n := f.calls.Load(); n != 1 {
				t.Errorf("%d re-auth calls, want 1 (the first failure before the answer began is the answer)", n)
			}
		})
	}
}

// TestGrantWithdrawnEndsStream is lead decision O-08-5 (a): a consumer
// without all_models whose grant is withdrawn during a committed stream ⇒
// the error frame model_not_found within a re-auth period + 1 s, the lease
// free — the forced grant read of the re-auth period (the alias generation
// does not move, W02-07 review 2 F2-3). Red: the re-auth period resolves
// without force (the grant read is skipped, the stream runs on).
func TestGrantWithdrawnEndsStream(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	f := &reauthFake{}
	rig, u, end := reauthRig(t, f, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
	ca := urgentApp()
	ca.AllModels = false
	rig.ca.Store(ca)
	rig.setGrant(idChat, true)
	ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
	defer func() { _ = ls.Close() }()
	if _, ok := nextData(each, 5*time.Second); !ok {
		t.Fatal("no frame")
	}
	at := time.Now()
	rig.setGrant(idChat, false)
	e, ok := nextData(each, 100*time.Millisecond+time.Second)
	if !ok || !strings.Contains(e.data, `"code":"model_not_found"`) {
		t.Fatalf("frame %q, want model_not_found within a period + 1 s", e.data)
	}
	if !end.wait(time.Second) || !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
		t.Error("the upstream was not aborted or the lease not freed")
	}
	t.Logf("grant withdrawn: frame %v later", e.at.Sub(at))
}

// TestReauthNonStream is Z6: the re-auth runs for non-stream requests too —
// "no longer valid" during the attempt ⇒ 401 invalid_api_key, never a 200,
// the upstream aborted within a period + 1 s. Red: re-auth for streams
// only.
func TestReauthNonStream(t *testing.T) {
	t.Cleanup(oai.SetReauthEvery(100 * time.Millisecond))
	f := &reauthFake{}
	f.set(reauthAnswer{ok: false})
	u := newUpstream(t, answerSGLang)
	b := newBlocker()
	defer b.open()
	u.setRespond(b.respond(answerSGLang))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withReauth(f))
	start := time.Now()
	a := rig.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	if a.status != http.StatusUnauthorized || code != oai.CodeInvalidAPIKey || a.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("answer %d %s, want 401 invalid_api_key", a.status, a.body)
	}
	if d := time.Since(start); d > 100*time.Millisecond+time.Second {
		t.Errorf("answered after %v, want within a period + 1 s", d)
	}
	if !waitHeld(rig.disp, u.srv.URL, 0, time.Second) {
		t.Error("the lease is still held")
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
