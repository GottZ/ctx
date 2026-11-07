package oai_test

// The deadline on every grant read of a running request and of the entry
// (W02-08 hardening after Fremd-Review 2, H-1), on a fake grant read that
// hangs until its context ends — the way a pgx query waits for an exhausted
// pool or a locked grants table. The real database half is
// chat_reauth_db_integration_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// grantDeadline is the bound of one grant read: the door's own database
// deadline (1 s; pinned by TestGrantReadDeadlinePin).
const grantDeadline = time.Second

// grantHang makes chosen grant reads hang until their context ends.
type grantHang struct {
	mu    sync.Mutex
	rule  func(n int64) bool // whether grant read n (1-based) hangs; nil = none
	reads atomic.Int64
	hung  atomic.Int64 // hanging reads started
	back  atomic.Int64 // hanging reads returned
}

func (h *grantHang) set(rule func(n int64) bool) {
	h.mu.Lock()
	h.rule = rule
	h.mu.Unlock()
}

// fromNow lets every grant read after the ones already made hang.
func (h *grantHang) fromNow() {
	k := h.reads.Load()
	h.set(func(n int64) bool { return n > k })
}

// onlyNext lets exactly the next grant read hang.
func (h *grantHang) onlyNext() {
	k := h.reads.Load()
	h.set(func(n int64) bool { return n == k+1 })
}

func (h *grantHang) hangs(n int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rule != nil && h.rule(n)
}

func withGrantHang(h *grantHang) rigOption {
	return func(r *chatRig) {
		inner := r.deps.HasGrant
		r.deps.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
			if !h.hangs(h.reads.Add(1)) {
				return inner(ctx, consumerID, aliasID)
			}
			h.hung.Add(1)
			defer h.back.Add(1)
			<-ctx.Done()
			return false, fmt.Errorf("grant probe (fake: hangs until its context ends): %w", ctx.Err())
		}
	}
}

// grantRig is a stream rig of a grant consumer (no all_models, a grant on
// "chat") over an upstream that sends a frame and holds, with the re-auth
// period shortened to period and the grant reads behind h.
func grantRig(t *testing.T, h *grantHang, f *reauthFake, period time.Duration, steps []sseStep) *chatRig {
	t.Helper()
	t.Cleanup(oai.SetReauthEvery(period))
	u := newUpstream(t, answerSGLang)
	if steps != nil {
		u.setRespond(sseRespond(steps, nil, nil))
	} else {
		b := newBlocker()
		t.Cleanup(b.open)
		u.setRespond(b.respond(answerSGLang))
	}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withReauth(f), withGrantHang(h))
	ca := urgentApp()
	ca.AllModels = false
	rig.ca.Store(ca)
	rig.setGrant(idChat, true)
	return rig
}

// TestWatcherNeverWaitsBehindAGrantRead is H-1's ordering pin: while the
// re-auth period's grant read hangs, the kill switch and an alias disabled
// in the registry end the committed stream within one watcher tick plus the
// grant deadline — the switch and the alias are checked in memory, never
// behind a database read, and the read itself ends at its deadline. Red:
// the grant read without a deadline (the watcher hangs, no frame), the
// alias check only through the grant read (auth_unavailable instead of
// model_not_found), the switch checked after the read.
func TestWatcherNeverWaitsBehindAGrantRead(t *testing.T) {
	const period, tick = 300 * time.Millisecond, 200 * time.Millisecond
	run := func(t *testing.T, act func(rig *chatRig), want string) {
		t.Cleanup(oai.SetWatchEvery(tick))
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		rig := grantRig(t, h, &reauthFake{}, period, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		h.fromNow()
		if !waitFor(period+time.Second, func() bool { return h.hung.Load() >= 1 }) {
			t.Fatal("the re-auth period made no grant read")
		}
		time.Sleep(100 * time.Millisecond) // well inside the hanging read
		at := time.Now()
		act(rig)
		e, ok := nextData(each, tick+grantDeadline+3*time.Second)
		took := time.Duration(-1) // -1: no frame
		if ok {
			took = e.at.Sub(at)
		}
		t.Logf("act → frame %q after %v (bound tick + deadline %v); hanging reads %d, returned %d", e.data, took.Round(time.Millisecond),
			tick+grantDeadline, h.hung.Load(), h.back.Load())
		if !ok || !strings.Contains(e.data, `"code":"`+want+`"`) {
			t.Fatalf("frame %q (got one: %v), want the error frame %s", e.data, ok, want)
		}
		if took > tick+grantDeadline+300*time.Millisecond {
			t.Errorf("frame %v after the act, want ≤ one tick + the grant deadline (%v): the check waited behind the grant read", took, tick+grantDeadline)
		}
	}
	t.Run("kill switch", func(t *testing.T) {
		run(t, func(rig *chatRig) { rig.setSettings(func(s *oai.Settings) { s.Enabled = false }) }, oai.CodeNotFound)
	})
	// The watcher is idle (between two periods) when the switch flips, and
	// every later grant read hangs: the switch acts within one tick — no
	// grant read before it. Green against the base too (it checked the
	// switch first); the pin against a reordering that reads the grant
	// before the switch (mutant MH1c: ≈ period + deadline).
	t.Run("kill switch while idle, later reads hang", func(t *testing.T) {
		t.Cleanup(oai.SetWatchEvery(tick))
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		f := &reauthFake{}
		rig := grantRig(t, h, f, period, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		if !waitFor(period+time.Second, func() bool { return f.calls.Load() >= 1 }) {
			t.Fatal("no re-auth")
		}
		time.Sleep(20 * time.Millisecond) // the period's grant read (not hanging) is done
		at := time.Now()
		h.fromNow()
		rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
		e, ok := nextData(each, tick+grantDeadline+3*time.Second)
		took := time.Duration(-1) // -1: no frame
		if ok {
			took = e.at.Sub(at)
		}
		t.Logf("idle watcher, switch flipped → frame %q after %v (bound tick %v + 300 ms); hanging reads %d", e.data, took.Round(time.Millisecond), tick, h.hung.Load())
		if !ok || !strings.Contains(e.data, `"code":"not_found"`) {
			t.Fatalf("frame %q (got one: %v), want not_found", e.data, ok)
		}
		if took > tick+300*time.Millisecond {
			t.Errorf("frame %v after the flip, want ≤ one tick (%v) + 300 ms: a grant read came before the switch", took, tick)
		}
	})
	t.Run("alias disabled", func(t *testing.T) {
		run(t, func(rig *chatRig) {
			rows := chatAliases()
			for i := range rows {
				if rows[i].name == "chat" {
					rows[i].enabled = false
				}
			}
			rig.reg.reload(t, rows)
		}, oai.CodeModelNotFound)
	})
}

// TestGrantReadPastTheDeadlineIsAFailedCheck is H-1's counting rule: a
// grant read of the re-auth period that runs past its deadline is the
// period's failed check, in the same count as a failed key check
// (reauthFailLimit, one period = one verdict). A committed stream whose
// grant reads keep hanging ends with auth_unavailable after two periods; a
// failed key check followed by a period whose grant read hangs is two
// failures in a row; a non-stream request answers the first one with 503
// auth_unavailable. Red: no deadline (the watcher hangs), a timeout that
// does not count (the stream runs on), two counts (the key's and the
// grant's: the second case runs on).
func TestGrantReadPastTheDeadlineIsAFailedCheck(t *testing.T) {
	const period = 300 * time.Millisecond
	t.Run("committed stream, every grant read hangs", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		rig := grantRig(t, h, &reauthFake{}, period, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		at := time.Now()
		h.fromNow()
		bound := 2 * (period + grantDeadline)
		e, ok := nextData(each, bound+3*time.Second)
		took := time.Duration(-1) // -1: no frame
		if ok {
			took = e.at.Sub(at)
		}
		t.Logf("hanging grant reads → frame %q after %v (bound %v); hanging reads %d", e.data, took.Round(time.Millisecond), bound, h.hung.Load())
		if !ok || !strings.Contains(e.data, `"code":"auth_unavailable"`) {
			t.Fatalf("frame %q (got one: %v), want auth_unavailable: a grant read past its deadline is a failed check", e.data, ok)
		}
		if took > bound+500*time.Millisecond {
			t.Errorf("frame %v after the reads began to hang, want ≤ two periods with their deadlines (%v)", took, bound)
		}
		if n := h.hung.Load(); n != 2 {
			t.Errorf("%d hanging grant reads before the end, want 2 (one per period)", n)
		}
		if !waitFor(time.Second, func() bool { return h.back.Load() == h.hung.Load() }) {
			t.Errorf("a grant read is still hanging: %d started, %d returned", h.hung.Load(), h.back.Load())
		}
	})
	t.Run("a failed key check, then a hanging grant read", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		f := &reauthFake{}
		f.set(reauthAnswer{err: errAuthDB}, reauthAnswer{ok: true})
		rig := grantRig(t, h, f, period, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		if !waitFor(2*period+time.Second, func() bool { return f.calls.Load() >= 1 }) {
			t.Fatal("no re-auth")
		}
		at := time.Now()
		h.onlyNext() // the next period's grant read, and only that one, hangs
		bound := period + grantDeadline
		e, ok := nextData(each, bound+3*time.Second)
		took := time.Duration(-1) // -1: no frame
		if ok {
			took = e.at.Sub(at)
		}
		t.Logf("key check failed, next grant read hung → frame %q after %v (bound %v); re-auth calls %d", e.data, took.Round(time.Millisecond), bound, f.calls.Load())
		if !ok || !strings.Contains(e.data, `"code":"auth_unavailable"`) {
			t.Fatalf("frame %q (got one: %v), want auth_unavailable: two failed periods in a row (one count)", e.data, ok)
		}
		if took > bound+500*time.Millisecond {
			t.Errorf("frame %v after the first failure, want ≤ the next period + the grant deadline (%v)", took, bound)
		}
	})
	t.Run("non-stream, the first hanging grant read of a period", func(t *testing.T) {
		h := &grantHang{}
		rig := grantRig(t, h, &reauthFake{}, period, nil)
		h.set(func(n int64) bool { return n > 1 }) // the entry's read passes, every later one hangs
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		a := postStream(ctx, t, serveHTTP(t, rig.router), chatBody("chat"))
		took := a.end.Sub(a.start)
		t.Logf("non-stream with a hanging grant read: %d x-should-retry=%q after %v: %s", a.status, a.header.Get("x-should-retry"), took.Round(time.Millisecond), strings.TrimSpace(a.raw))
		if a.status != http.StatusServiceUnavailable || !strings.Contains(a.raw, `"code":"auth_unavailable"`) || a.header.Get("x-should-retry") != "false" {
			t.Fatalf("answer %d %s, want 503 auth_unavailable with x-should-retry false", a.status, a.raw)
		}
		if took > period+grantDeadline+500*time.Millisecond {
			t.Errorf("answered after %v, want ≤ one period + the grant deadline (%v)", took, period+grantDeadline)
		}
	})
}

// TestEntryGrantReadDeadline is H-1's entry half: the entry's grant read
// runs under the same deadline, a stream (which carries no request
// deadline) and a non-stream request alike, and a read that runs past it is
// a failed grant read — 503 internal_error (design/02 §4.10.2,
// "Auth-DB-Fehler"). Red: the entry's read without a deadline (the request
// waits until the client gives up).
func TestEntryGrantReadDeadline(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			h := &grantHang{}
			h.set(func(int64) bool { return true })
			var steps []sseStep
			body := chatBody("chat")
			if stream {
				steps, body = streamShapes[0].steps(), streamBody("chat")
			}
			rig := grantRig(t, h, &reauthFake{}, time.Minute, steps)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a := postStream(ctx, t, serveHTTP(t, rig.router), body)
			took := a.end.Sub(a.start)
			t.Logf("entry with a hanging grant read: %d after %v: %s", a.status, took.Round(time.Millisecond), strings.TrimSpace(a.raw))
			if a.status != http.StatusServiceUnavailable || !strings.Contains(a.raw, `"code":"internal_error"`) {
				t.Fatalf("answer %d %s, want 503 internal_error (the grant read could not run)", a.status, a.raw)
			}
			if took > grantDeadline+500*time.Millisecond {
				t.Errorf("answered after %v, want ≤ the grant deadline + 500 ms", took)
			}
			if h.back.Load() != h.hung.Load() {
				t.Errorf("%d hanging reads, %d returned", h.hung.Load(), h.back.Load())
			}
		})
	}
}
