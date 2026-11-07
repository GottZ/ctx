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
// "chat") over an upstream that answers steps (nil: a non-stream upstream
// that holds until the test ends), with the grant reads behind h.
func grantRig(t *testing.T, h *grantHang, steps []sseStep) *chatRig {
	t.Helper()
	u := newUpstream(t, answerSGLang)
	if steps != nil {
		u.setRespond(sseRespond(steps, nil, nil))
	} else {
		b := newBlocker()
		t.Cleanup(b.open)
		u.setRespond(b.respond(answerSGLang))
	}
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withGrantHang(h))
	ca := urgentApp()
	ca.AllModels = false
	rig.ca.Store(ca)
	rig.setGrant(idChat, true)
	return rig
}

// TestWatcherNeverWaitsBehindAGrantRead is H-1's ordering pin: while the
// grant read of a watcher tick hangs (the alias generation moved — a reload
// that keeps the alias), the kill switch and an alias disabled in the
// registry end the committed stream within one watcher tick plus the grant
// deadline — the switch and the alias are checked in memory, never behind a
// database read, and the read itself ends at its deadline. Red: the grant
// read without a deadline (the watcher hangs, no frame), the alias check
// only through the grant read, the switch checked after the read.
func TestWatcherNeverWaitsBehindAGrantRead(t *testing.T) {
	const tick = 200 * time.Millisecond
	run := func(t *testing.T, act func(rig *chatRig), want string) {
		t.Cleanup(oai.SetWatchEvery(tick))
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		rig := grantRig(t, h, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
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
	// The watcher is idle (between two ticks) when the switch flips; right
	// after it the alias generation moves and every later grant read hangs:
	// the switch acts within one tick — no grant read before it. Green
	// against the base too (it checks the switch first); the pin against a
	// reordering that reads the grant before the switch (mutant MH1c:
	// ≈ tick + deadline, one hanging read).
	t.Run("kill switch while idle, later reads hang", func(t *testing.T) {
		t.Cleanup(oai.SetWatchEvery(tick))
		hold := make(chan struct{})
		defer close(hold)
		h := &grantHang{}
		rig := grantRig(t, h, []sseStep{{data: sglangChunk("hello", "")}, {hold: hold}})
		ls, each := openAndRead(t, context.Background(), serveHTTP(t, rig.router), streamBody("chat"))
		defer func() { _ = ls.Close() }()
		if _, ok := nextData(each, 5*time.Second); !ok {
			t.Fatal("no frame")
		}
		at := time.Now()
		h.fromNow()
		rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
		rig.reg.reload(t, chatAliases()) // after the flip: a tick that reads the grant first would hang
		e, ok := nextData(each, tick+grantDeadline+3*time.Second)
		took := time.Duration(-1) // -1: no frame
		if ok {
			took = e.at.Sub(at)
		}
		t.Logf("idle watcher, switch flipped → frame %q after %v (bound tick %v + 300 ms); hanging reads %d", e.data, took.Round(time.Millisecond), tick, h.hung.Load())
		if !ok || !strings.Contains(e.data, `"code":"not_found"`) {
			t.Fatalf("frame %q (got one: %v), want not_found", e.data, ok)
		}
		if took > tick+300*time.Millisecond || h.hung.Load() != 0 {
			t.Errorf("frame %v after the flip, %d hanging reads — want ≤ one tick (%v) + 300 ms and none: a grant read came before the switch", took, h.hung.Load(), tick)
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
			rig := grantRig(t, h, steps)
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
