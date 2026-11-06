package oai_test

// Gates from Delta-Review 3 of W02-08 (reports/review/W02-08-fremdreview-3.md,
// part D of the rebase round): the grant read of the watcher's tick runs
// under the grant deadline (L-b, mutant B2), the check right before a
// non-streamed write does too and answers 503 internal_error (L-a, a named
// deviation; mutant B4), and a re-auth period resolves with the consumer it
// has just read (L-c, mutant B12). Templates: the reviewer's probes
// (review3/sa-mut/probe-src/zz_samut3_probe_test.go).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestWatcherTickReadUnderTheDeadline (D-1, L-b): the alias generation moves
// (a reload that keeps the alias) while every grant read hangs, so the
// tick's unforced grant read is the one in flight. The kill switch, resp. a
// revoked key, still ends the committed stream: the switch within one tick
// + the grant deadline, the revocation within the grant deadline once the
// tick's read is in flight — the tick's read ends at its deadline and the
// re-auth period due meanwhile runs next. Red (B2, the tick's read without a
// deadline): the watcher hangs in the read — no frame, no re-auth.
//
// The revocation runs with the production ratio tick > grant deadline
// (tick 1.5 s, period 2 s, the period first due while the tick's read
// hangs): with a tick shorter than the deadline the tick is due again when
// the read returns, and the watcher's select picks the tick or the period
// at random — each lost draw costs one more hanging read (measured 1.9 s and
// 4.9 s with tick 200 ms, period 300 ms).
func TestWatcherTickReadUnderTheDeadline(t *testing.T) {
	const slack = time.Second
	run := func(t *testing.T, tick, period time.Duration, f *reauthFake, act func(rig *chatRig), want string, bound time.Duration) {
		t.Cleanup(oai.SetWatchEvery(tick))
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		rig := grantRig(t, h, f, period, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		h.fromNow()
		rig.reg.reload(t, chatAliases()) // the generation moves, the alias stays
		if !waitFor(tick+time.Second, func() bool { return h.hung.Load() >= 1 }) {
			t.Fatal("the tick made no grant read after the generation moved")
		}
		if n := f.calls.Load(); n != 0 {
			t.Fatalf("%d re-auth calls before the tick's read — the hanging read must be the tick's", n)
		}
		time.Sleep(100 * time.Millisecond)
		at := time.Now()
		act(rig)
		e, ok := nextData(each, bound+5*time.Second)
		took := time.Duration(-1)
		if ok {
			took = e.at.Sub(at)
		}
		t.Logf("tick read hangs, act → frame %q after %v (bound %v); hanging reads %d, returned %d, re-auth calls %d",
			e.data, took.Round(time.Millisecond), bound, h.hung.Load(), h.back.Load(), f.calls.Load())
		if !ok || !strings.Contains(e.data, `"code":"`+want+`"`) || took > bound {
			t.Fatalf("frame %q (got one: %v) after %v, want %s within %v: the tick's grant read must end at its deadline", e.data, ok, took, want, bound)
		}
	}
	t.Run("kill switch", func(t *testing.T) {
		const tick = 200 * time.Millisecond
		run(t, tick, time.Minute, &reauthFake{}, func(rig *chatRig) { rig.setSettings(func(s *oai.Settings) { s.Enabled = false }) },
			oai.CodeNotFound, tick+grantDeadline+slack)
	})
	t.Run("revoked key", func(t *testing.T) {
		f := &reauthFake{}
		run(t, 1500*time.Millisecond, 2*time.Second, f, func(*chatRig) { f.set(reauthAnswer{ok: false}) }, oai.CodeInvalidAPIKey, grantDeadline+slack)
	})
}

// TestPreWriteGrantReadUnderTheDeadline (D-2, L-a — a named deviation of the
// hardening after Review 2): the check right before a non-streamed answer is
// written reads the grant under the same 1-s deadline as every grant read;
// past it the finished answer is not sent — 503 internal_error, x-should-
// retry false (masterplan K5), right after the deadline, not at the request
// deadline. Here the upstream answers at once and every grant read after the
// entry hangs. Red (B4, the pre-write read without the deadline): the answer
// waits for the request deadline (6 s).
func TestPreWriteGrantReadUnderTheDeadline(t *testing.T) {
	t.Cleanup(oai.SetReauthEvery(time.Minute))
	h := &grantHang{}
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withReauth(&reauthFake{}), withGrantHang(h))
	ca := urgentApp()
	ca.AllModels = false
	rig.ca.Store(ca)
	rig.setGrant(idChat, true)
	rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = 6 * time.Second })
	h.set(func(n int64) bool { return n > 1 }) // read 1 = the entry's
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	a := postStream(ctx, t, serveHTTP(t, rig.router), chatBody("chat"))
	took := a.end.Sub(a.start)
	t.Logf("non-stream, the pre-write grant read hangs: %d after %v, x-should-retry %q (reads hung %d, returned %d, upstream hits %d): %s",
		a.status, took.Round(time.Millisecond), a.header.Get("X-Should-Retry"), h.hung.Load(), h.back.Load(), u.hits.Load(), strings.TrimSpace(a.raw))
	if u.hits.Load() != 1 || h.hung.Load() != 1 {
		t.Fatalf("upstream hits %d, hanging reads %d — want the answer done and exactly the pre-write read hanging", u.hits.Load(), h.hung.Load())
	}
	if a.status != http.StatusServiceUnavailable || !strings.Contains(a.raw, `"code":"internal_error"`) || a.header.Get("X-Should-Retry") != "false" {
		t.Errorf("answer %d x-should-retry %q: %s — want 503 internal_error, x-should-retry false", a.status, a.header.Get("X-Should-Retry"), a.raw)
	}
	if took < grantDeadline || took > grantDeadline+2*time.Second {
		t.Errorf("answered after %v, want right after the grant deadline %v (the request deadline is 6 s)", took, grantDeadline)
	}
}

// TestReauthPeriodActsWithTheConsumerItRead (D-3, L-c): the door hands back
// a consumer without all_models (and without a grant) from the first period
// on — the stream ends in that period: exactly one re-auth call until the
// end. Red (B12, the fresh consumer stored only after the period's check):
// the change acts one period later, two calls.
func TestReauthPeriodActsWithTheConsumerItRead(t *testing.T) {
	const period = 300 * time.Millisecond
	hold := make(chan struct{})
	defer close(hold)
	ca := urgentApp()
	ca.AllModels = false
	f := &reauthFake{}
	f.set(reauthAnswer{ok: true, ca: ca})
	rig, _, _ := reauthRig(t, f, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
	t.Cleanup(oai.SetReauthEvery(period))
	ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
	defer func() { _ = ls.Close() }()
	if _, ok := nextData(each, 5*time.Second); !ok {
		t.Fatal("no frame")
	}
	at := time.Now()
	e, ok := nextData(each, 3*period+3*time.Second)
	time.Sleep(50 * time.Millisecond)
	t.Logf("fresh consumer without all_models: frame %q after %v, re-auth calls %d", e.data, e.at.Sub(at).Round(time.Millisecond), f.calls.Load())
	if !ok || !strings.Contains(e.data, `"code":"model_not_found"`) {
		t.Fatalf("frame %q (got one: %v), want model_not_found", e.data, ok)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d re-auth calls until the end, want 1: the period resolves with the consumer it has just read", n)
	}
}
