// Unit half of gate C6-B (event-driven distiller tick): every decision the wake
// path takes WITHOUT a database. The catalog half — the real trg_block_write
// NOTIFY through the production WriteHandler, and the real filter SQL — lives in
// distill_c6b_integration_test.go.
//
// The filter seam is what makes this half possible at all: "no checkpoint in
// this window", "a checkpoint in this window" and "the filter could not answer"
// are three states a database cannot be told to produce on command, and the
// third one carries the fail-open posture that decides whether a compaction can
// be lost.
package events

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// c6bArm builds a poolless scheduler whose wake filter is steerable. A nil pool
// is deliberate: every path asserted here must decide without touching one, and
// a query slipping in would panic rather than pass quietly.
func c6bArm(t *testing.T, filter func(context.Context, []string) (bool, error)) *Scheduler {
	t.Helper()
	s := NewScheduler(nil, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	s.distillWakeFilter = filter
	return s
}

// c6bShortDebounce shortens the settle for the duration of one test (the
// dreamYieldWait precedent — with the 2 s production value every assertion below
// would cost two seconds and the burst probes would be untestable).
func c6bShortDebounce(t *testing.T, d time.Duration) {
	t.Helper()
	prev := distillWakeDebounce
	distillWakeDebounce = d
	t.Cleanup(func() { distillWakeDebounce = prev })
}

// c6bNotification builds the pgconn value pgxlisten hands the handler.
func c6bNotification(payload string) *pgconn.Notification {
	return &pgconn.Notification{Channel: channelBlockWrite, Payload: payload}
}

// c6bCountingFilter answers `hit` and records every call.
type c6bCountingFilter struct {
	hit   bool
	err   error
	calls int
	ids   [][]string
}

func (f *c6bCountingFilter) fn(_ context.Context, ids []string) (bool, error) {
	f.calls++
	f.ids = append(f.ids, append([]string(nil), ids...))
	return f.hit, f.err
}

// ── the wake window itself ───────────────────────────────────────────────────.

// TestDistillWakeWindow pins the bookkeeping the listener thread does: ids
// accumulate, the drain clears payload AND pending signal together, and an
// overflow degrades to "wake without asking" rather than to a dropped id.
func TestDistillWakeWindow(t *testing.T) {
	t.Run("DrainTakesTheWindowAndClearsTheSignal", func(t *testing.T) {
		s := c6bArm(t, nil)
		s.NotifyBlockInsert("a")
		s.NotifyBlockInsert("b")
		if len(s.distillWake) != 1 {
			t.Fatalf("pending signals = %d, want 1 — the send must coalesce", len(s.distillWake))
		}
		ids, overflow := s.drainDistillWake()
		if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
			t.Fatalf("drained %v, want [a b]", ids)
		}
		if overflow {
			t.Fatal("overflow on a two-id window")
		}
		if len(s.distillWake) != 0 {
			t.Fatal("the drain left a signal behind — the next wait would settle for nothing")
		}
		// Idempotent: a second drain finds an empty window, never the old ids.
		if ids, overflow := s.drainDistillWake(); len(ids) != 0 || overflow {
			t.Fatalf("second drain returned %v / %v, want empty", ids, overflow)
		}
	})

	t.Run("EmptyIDIgnored", func(t *testing.T) {
		s := c6bArm(t, nil)
		s.NotifyBlockInsert("")
		if len(s.distillWake) != 0 {
			t.Fatal("an empty id armed the arm — a malformed payload must not cost a tick")
		}
	})

	t.Run("OverflowWakesWithoutAsking", func(t *testing.T) {
		s := c6bArm(t, nil)
		for i := 0; i < distillWakeIDCap+10; i++ {
			s.NotifyBlockInsert(fmt.Sprintf("id-%d", i))
		}
		ids, overflow := s.drainDistillWake()
		if len(ids) != distillWakeIDCap {
			t.Fatalf("kept %d ids, want the cap %d", len(ids), distillWakeIDCap)
		}
		if !overflow {
			t.Fatal("no overflow past the cap — the ids above it would be silently dropped")
		}
		// Fail-open is the whole point of the flag.
		if !s.distillWakeHit(context.Background(), nil, true) {
			t.Fatal("an overflowed window did not wake the arm")
		}
	})

	t.Run("BacklogWakesWithoutIDs", func(t *testing.T) {
		s := c6bArm(t, nil)
		s.NotifyDistillBacklog()
		ids, overflow := s.drainDistillWake()
		if len(ids) != 0 || !overflow {
			t.Fatalf("backlog window = %v / %v, want no ids and overflow — the reconnect gap has no ids to filter on", ids, overflow)
		}
	})
}

// ── the filter's three answers ───────────────────────────────────────────────.

func TestDistillWakeHit(t *testing.T) {
	ctx := context.Background()

	t.Run("EmptyWindowNeverTicks", func(t *testing.T) {
		f := &c6bCountingFilter{hit: true}
		s := c6bArm(t, f.fn)
		if s.distillWakeHit(ctx, nil, false) {
			t.Fatal("an empty window woke the arm")
		}
		if f.calls != 0 {
			t.Fatalf("filter called %d times on an empty window, want 0 — no query without ids", f.calls)
		}
	})

	t.Run("ForeignWritesDoNotTick", func(t *testing.T) {
		f := &c6bCountingFilter{hit: false}
		s := c6bArm(t, f.fn)
		if s.distillWakeHit(ctx, []string{"x", "y"}, false) {
			t.Fatal("a window of foreign writes woke the arm — at target scale that is a tick per write burst")
		}
		if f.calls != 1 {
			t.Fatalf("filter called %d times, want exactly 1 per window", f.calls)
		}
	})

	t.Run("CheckpointWriteTicks", func(t *testing.T) {
		s := c6bArm(t, (&c6bCountingFilter{hit: true}).fn)
		if !s.distillWakeHit(ctx, []string{"x"}, false) {
			t.Fatal("a checkpoint write did not wake the arm")
		}
	})

	t.Run("UnanswerableFilterFailsOpen", func(t *testing.T) {
		s := c6bArm(t, (&c6bCountingFilter{hit: false, err: errors.New("pool gone")}).fn)
		if !s.distillWakeHit(ctx, []string{"x"}, false) {
			t.Fatal("a failing filter swallowed the window — being wrong here must cost a no_new_rows tick, never a lost compaction")
		}
	})
}

// ── the wait ─────────────────────────────────────────────────────────────────.

func TestDistillAwait(t *testing.T) {
	// Wake test (gate 1): a checkpoint write returns the wait one debounce
	// later, and DEEP below the fallback that used to be the only cadence.
	t.Run("WakeReturnsWithinDebounce", func(t *testing.T) {
		c6bShortDebounce(t, 100*time.Millisecond)
		f := &c6bCountingFilter{hit: true}
		s := c6bArm(t, f.fn)
		s.NotifyBlockInsert("ckpt")

		start := time.Now()
		if !s.distillAwait(context.Background(), 30*time.Second) {
			t.Fatal("distillAwait reported shutdown on a live context")
		}
		took := time.Since(start)
		if took < distillWakeDebounce {
			t.Fatalf("returned after %v, before the debounce %v — the burst was not allowed to settle", took, distillWakeDebounce)
		}
		if took > 2*time.Second {
			t.Fatalf("returned after %v — that is the poll, not the event", took)
		}
		if f.calls != 1 || len(f.ids[0]) != 1 || f.ids[0][0] != "ckpt" {
			t.Fatalf("filter saw %d calls / %v, want one call carrying the written id", f.calls, f.ids)
		}
	})

	// Debounce test (gate 2): N writes of one compaction collapse into ONE tick,
	// and — the half that a single call cannot show — leave nothing behind that
	// would make the NEXT wait return without a new event.
	t.Run("BurstCollapsesIntoOneTick", func(t *testing.T) {
		c6bShortDebounce(t, 150*time.Millisecond)
		f := &c6bCountingFilter{hit: true}
		s := c6bArm(t, f.fn)

		const burst = 20
		go func() {
			for i := 0; i < burst; i++ {
				s.NotifyBlockInsert(fmt.Sprintf("part-%d", i))
				time.Sleep(2 * time.Millisecond)
			}
		}()

		if !s.distillAwait(context.Background(), 30*time.Second) {
			t.Fatal("distillAwait reported shutdown on a live context")
		}
		if f.calls != 1 {
			t.Fatalf("filter called %d times for ONE compaction, want 1", f.calls)
		}
		if got := len(f.ids[0]); got != burst {
			t.Fatalf("the window carried %d of %d writes — the settle did not cover the burst", got, burst)
		}

		// The second wait must fall through to the fallback: a leftover signal
		// here would mean every compaction costs two ticks.
		start := time.Now()
		if !s.distillAwait(context.Background(), 400*time.Millisecond) {
			t.Fatal("distillAwait reported shutdown on a live context")
		}
		if took := time.Since(start); took < 350*time.Millisecond {
			t.Fatalf("the wait after a drained burst returned after %v — a signal survived the drain", took)
		}
		if f.calls != 1 {
			t.Fatalf("filter called %d times overall, want 1 — the second wait must not re-ask an empty window", f.calls)
		}
	})

	// Filter test (gate 3): a write the arm does not own must not cost a tick.
	t.Run("ForeignWriteDoesNotTick", func(t *testing.T) {
		c6bShortDebounce(t, 50*time.Millisecond)
		f := &c6bCountingFilter{hit: false}
		s := c6bArm(t, f.fn)
		s.NotifyBlockInsert("some-knowledge-block")

		start := time.Now()
		if !s.distillAwait(context.Background(), 600*time.Millisecond) {
			t.Fatal("distillAwait reported shutdown on a live context")
		}
		took := time.Since(start)
		if took < 500*time.Millisecond {
			t.Fatalf("a foreign write ticked the arm after %v — the wait must run out its fallback instead", took)
		}
		// AND the fallback deadline must not have been extended by the settle:
		// the arm is entitled to a tick within one interval no matter how much
		// unrelated traffic passes through the window.
		if took > 1200*time.Millisecond {
			t.Fatalf("the fallback fired after %v, want ~600ms — a foreign window extended the deadline", took)
		}
		if f.calls != 1 {
			t.Fatalf("filter called %d times, want 1", f.calls)
		}
	})

	// Fallback test (gate 4): the pre-wave cadence still exists underneath.
	t.Run("FallbackStillTicksWithoutEvents", func(t *testing.T) {
		s := c6bArm(t, (&c6bCountingFilter{hit: true}).fn)
		start := time.Now()
		if !s.distillAwait(context.Background(), 300*time.Millisecond) {
			t.Fatal("distillAwait reported shutdown on a live context")
		}
		if took := time.Since(start); took < 250*time.Millisecond || took > 2*time.Second {
			t.Fatalf("idle wait returned after %v, want ~300ms", took)
		}
	})

	// Shutdown test (gate 6), both edges: idle and mid-settle.
	t.Run("ShutdownEndsTheWait", func(t *testing.T) {
		c6bShortDebounce(t, 5*time.Second)
		for _, tc := range []struct {
			name string
			arm  bool
		}{
			{"idle", false},
			{"inside the settle", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := c6bArm(t, (&c6bCountingFilter{hit: true}).fn)
				ctx, cancel := context.WithCancel(context.Background())
				if tc.arm {
					s.NotifyBlockInsert("ckpt")
				}
				done := make(chan bool, 1)
				go func() { done <- s.distillAwait(ctx, time.Hour) }()
				time.Sleep(50 * time.Millisecond)
				cancel()
				select {
				case got := <-done:
					if got {
						t.Fatal("distillAwait reported a due tick on a cancelled context")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("distillAwait did not return after cancel — the arm goroutine leaks on shutdown")
				}
			})
		}
	})
}

// ── the listener's op filter ─────────────────────────────────────────────────.

// TestDistillWriteHandlerArmsOnInsert pins the routing decision the production
// handler makes on every ctx_block_write payload. UPDATE is excluded because the
// source's watermark is created_at: an update of a covered row carries nothing a
// tick could find, and at the target scale the dream/guard stamp traffic on that
// channel dwarfs the inserts.
func TestDistillWriteHandlerArmsOnInsert(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantArm bool
	}{
		{"insert arms", `{"id":"11111111-1111-1111-1111-111111111111","op":"INSERT"}`, true},
		{"update does not arm", `{"id":"11111111-1111-1111-1111-111111111111","op":"UPDATE"}`, false},
		{"unparsable payload does not arm", `not json`, false},
		{"insert without an id does not arm", `{"op":"INSERT"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := c6bArm(t, nil)
			h := &WriteHandler{scheduler: s}
			if err := h.HandleNotification(context.Background(),
				c6bNotification(tc.payload), nil); err != nil {
				// A handler error is connection-level for pgxlisten: a bad
				// payload must never take the LISTEN connection down.
				t.Fatalf("HandleNotification returned %v — that would drop the listener connection", err)
			}
			ids, overflow := s.drainDistillWake()
			if armed := len(ids) > 0 || overflow; armed != tc.wantArm {
				t.Fatalf("armed = %v, want %v (ids %v, overflow %v)", armed, tc.wantArm, ids, overflow)
			}
			// Guard and digest are signalled either way — this wave changes the
			// distiller's cadence and nothing else.
			s.mu.Lock()
			pending := s.guardPending && s.digestPending
			s.mu.Unlock()
			if !pending {
				t.Fatal("guard/digest were not signalled — the C6-B branch must be purely additive")
			}
		})
	}
}

// TestDistillBacklogArmsTheArm is gate 5: a reconnect must not cost the arm the
// compactions of the disconnect window.
func TestDistillBacklogArmsTheArm(t *testing.T) {
	s := c6bArm(t, nil)
	h := &WriteHandler{scheduler: s}
	if err := h.HandleBacklog(context.Background(), channelBlockWrite, nil); err != nil {
		t.Fatalf("HandleBacklog returned %v", err)
	}
	ids, overflow := s.drainDistillWake()
	if !overflow {
		t.Fatal("the reconnect backlog did not wake the distiller — every compaction of the gap waits for the fallback")
	}
	if len(ids) != 0 {
		t.Fatalf("backlog carried ids %v — the dropped notifications have none to carry", ids)
	}
}
