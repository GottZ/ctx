package oai_test

// Gates from Delta-Review 3 of W02-08 (reports/review/W02-08-fremdreview-3.md,
// part D of the rebase round): the grant read of the watcher's tick runs
// under the grant deadline (L-b, mutant B2), and the check right before a
// non-streamed write does too and answers 503 internal_error (L-a, a named
// deviation; mutant B4). Templates: the reviewer's probes
// (review3/sa-mut/probe-src/zz_samut3_probe_test.go). The revocation half
// of D-1 runs on the revoker (W01-8): the registry's cancel, not the
// watcher, ends a revoked key's request.

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
// tick's unforced grant read is the one in flight. (kill switch) The switch
// still ends the committed stream within one tick + the grant deadline: the
// tick's read ends at its deadline. Red (B2, the tick's read without a
// deadline): the watcher hangs in the read — no frame. (revoked key) A
// revocation through the registry ends the stream at once, far inside the
// grant deadline, whatever the watcher is waiting for: the run is a child
// of the request context the registry cancels, and so is the hanging read
// — it returns with it. Red (the route without the ErrCredentialRevoked
// verdict): no frame.
func TestWatcherTickReadUnderTheDeadline(t *testing.T) {
	const tick = 200 * time.Millisecond
	open := func(t *testing.T, rv *revoker) (*chatRig, *grantHang, <-chan sseEvent) {
		t.Cleanup(oai.SetWatchEvery(tick))
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		h := &grantHang{}
		rig := grantRig(t, h, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rv.wrap(rig.router)), streamBody("chat"))
		t.Cleanup(func() { _ = ls.Close() })
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		h.fromNow()
		rig.reg.reload(t, chatAliases()) // the generation moves, the alias stays
		if !waitFor(tick+time.Second, func() bool { return h.hung.Load() >= 1 }) {
			t.Fatal("the tick made no grant read after the generation moved")
		}
		time.Sleep(100 * time.Millisecond)
		return rig, h, each
	}
	frameAfter := func(each <-chan sseEvent, at time.Time, within time.Duration) (sseEvent, time.Duration, bool) {
		e, ok := nextData(each, within)
		if !ok {
			return e, -1, false
		}
		return e, e.at.Sub(at), true
	}
	t.Run("kill switch", func(t *testing.T) {
		const bound = tick + grantDeadline + time.Second
		rig, h, each := open(t, newRevoker())
		at := time.Now()
		rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
		e, took, ok := frameAfter(each, at, bound+5*time.Second)
		t.Logf("tick read hangs, switch off → frame %q after %v (bound %v); hanging reads %d, returned %d",
			e.data, took.Round(time.Millisecond), bound, h.hung.Load(), h.back.Load())
		if !ok || !strings.Contains(e.data, `"code":"`+oai.CodeNotFound+`"`) || took > bound {
			t.Fatalf("frame %q (got one: %v) after %v, want not_found within %v: the tick's grant read must end at its deadline", e.data, ok, took, bound)
		}
	})
	t.Run("revoked key", func(t *testing.T) {
		const bound = 300 * time.Millisecond // ≪ the grant deadline
		rv := newRevoker()
		_, h, each := open(t, rv)
		at := time.Now()
		if n := rv.revoke(); n != 1 {
			t.Fatalf("revoked %d running requests, want 1", n)
		}
		e, took, ok := frameAfter(each, at, grantDeadline+5*time.Second)
		back := waitFor(time.Second, func() bool { return h.back.Load() == h.hung.Load() })
		t.Logf("tick read hangs, revoke → frame %q after %v (bound %v; race %v); hanging reads %d, returned %d",
			e.data, took, bound, raceEnabled, h.hung.Load(), h.back.Load())
		if !ok || !strings.Contains(e.data, `"code":"`+oai.CodeInvalidAPIKey+`"`) {
			t.Fatalf("frame %q (got one: %v), want invalid_api_key", e.data, ok)
		}
		if !raceEnabled && took > bound {
			t.Errorf("frame %v after the revocation, want ≤ %v: the revocation waited behind the tick's grant read", took, bound)
		}
		if !back {
			t.Errorf("a grant read is still hanging: %d started, %d returned", h.hung.Load(), h.back.Load())
		}
		if !waitFor(time.Second, func() bool { return rv.running.Load() == 0 }) {
			t.Errorf("%d requests still inside the registry", rv.running.Load())
		}
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
	h := &grantHang{}
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withGrantHang(h))
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
