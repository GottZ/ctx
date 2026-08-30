// Unit half of the C6-A gate: the two decisions the fan-out makes without a
// database — how many workers a snapshot licenses, and how the tick's
// candidates are cut into chains. The occupancy half (does the pool actually
// run several sources at once, and never two on one root) needs a journal and
// lives in distill_concurrency_integration_test.go.
package events

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
)

// goroutineID reads the id off the runtime's own stack header ("goroutine 17
// [running]:"). It is the only way to say "this ran in the CALLER's goroutine"
// from inside a callback, and it stays confined to this file — the production
// code never asks who it is running as.
func goroutineID() string {
	var buf [64]byte
	s := strings.TrimPrefix(string(buf[:runtime.Stack(buf[:], false)]), "goroutine ")
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// TestDistillConcurrencyClamp is the runtime half of V34. The validator is the
// authority an operator meets; this is what an UNVALIDATED snapshot gets — a
// hand-built Config in a test, or a generation that never passed Validate. Both
// ends clamp into the range V34 refuses outside of, and the low end is the one
// that matters: a 0 that reached the fan-out would be a tick that touches no
// source while every gate reports healthy.
func TestDistillConcurrencyClamp(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"configured", 4, 4},
		{"the default is the serial arm", 1, 1},
		{"zero is not an off-switch", 0, 1},
		{"negative clamps up", -3, 1},
		{"the bound holds", config.DistillMaxConcurrency, config.DistillMaxConcurrency},
		{"above the bound clamps down", config.DistillMaxConcurrency + 7, config.DistillMaxConcurrency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distillConcurrency(tc.in); got != tc.want {
				t.Fatalf("distillConcurrency(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestDistillChains pins the two properties the watermark rests on: one root is
// one chain (so it can only ever be in one worker), and the chains come out in
// order of first appearance (so a duplicate-free list — every list the
// production source can produce — is walked exactly as before this wave).
func TestDistillChains(t *testing.T) {
	refs := func(names ...string) []distillsource.Ref {
		out := make([]distillsource.Ref, 0, len(names))
		for _, n := range names {
			out = append(out, distillsource.Ref{Session: n})
		}
		return out
	}

	t.Run("distinct roots keep their order, one each", func(t *testing.T) {
		chains := distillChains(refs("a", "b", "c"))
		if len(chains) != 3 {
			t.Fatalf("chains = %d, want 3", len(chains))
		}
		for i, want := range []string{"a", "b", "c"} {
			if len(chains[i]) != 1 || chains[i][0].Session != want {
				t.Fatalf("chain %d = %v, want exactly [%s]", i, chains[i], want)
			}
		}
	})

	t.Run("a duplicate root joins its own chain", func(t *testing.T) {
		chains := distillChains(refs("a", "b", "a", "c", "a"))
		if len(chains) != 3 {
			t.Fatalf("chains = %d, want 3 — a duplicate must not open a second series", len(chains))
		}
		if len(chains[0]) != 3 {
			t.Fatalf("chain of a = %v, want three entries", chains[0])
		}
		if chains[1][0].Session != "b" || chains[2][0].Session != "c" {
			t.Fatalf("first-appearance order broken: %v", chains)
		}
	})

	t.Run("nothing is dropped", func(t *testing.T) {
		in := refs("a", "a", "b")
		var n int
		for _, chain := range distillChains(in) {
			n += len(chain)
		}
		if n != len(in) {
			t.Fatalf("chains hold %d refs, want %d — the fan-out is a dispatch, not a filter", n, len(in))
		}
	})

	t.Run("the empty list yields no chain", func(t *testing.T) {
		if got := distillChains(nil); len(got) != 0 {
			t.Fatalf("chains = %v, want none", got)
		}
	})
}

// TestDistillFanOutSerialAtOne is the shape assertion behind "default 1 is
// today's behaviour": at concurrency 1 the work runs in the CALLING goroutine,
// not in a pool of one. A pool would pass an order check too — what it would not
// pass is this, and the difference is what carries distillOnce's recover and
// every goroutine-local assumption the arm has made since A02-5.
//
// THE HOLD ON THE FIRST CHAIN IS THE PROBE, not padding: a goroutine that must
// not exist gets a quarter of a second of an otherwise idle queue to show
// itself. Without it the caller can walk all three chains before a wrongly
// started worker is ever scheduled, and a red implementation would pass by
// timing — measured exactly that way against a fan-out patched to start
// min(workers, chains) goroutines.
func TestDistillFanOutSerialAtOne(t *testing.T) {
	self := goroutineID()
	var seen []string
	var inside []string
	distillFanOut(context.Background(), 1,
		distillChains([]distillsource.Ref{{Session: "a"}, {Session: "b"}, {Session: "c"}}),
		func(_ context.Context, ref distillsource.Ref) {
			if len(seen) == 0 {
				time.Sleep(250 * time.Millisecond)
			}
			seen = append(seen, ref.Session)
			inside = append(inside, goroutineID())
		})
	if len(seen) != 3 || seen[0] != "a" || seen[1] != "b" || seen[2] != "c" {
		t.Fatalf("order = %v, want [a b c]", seen)
	}
	for i, g := range inside {
		if g != self {
			t.Fatalf("ref %d ran in goroutine %q, want the caller's own %q — concurrency 1 must start no goroutine at all", i, g, self)
		}
	}
}

// TestDistillFanOutPanicAtOneUnwinds is the other half of "identical to before
// the wave": at concurrency 1 a panicking source still unwinds into the caller,
// where distillOnce's recover has caught it since A02-5. A fan-out that
// swallowed it here would turn a tick-ending fault into a silently skipped root.
func TestDistillFanOutPanicAtOneUnwinds(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("the panic did not reach the caller — distillOnce's recover would never see it")
		}
	}()
	distillFanOut(context.Background(), 1,
		distillChains([]distillsource.Ref{{Session: "a"}}),
		func(_ context.Context, _ distillsource.Ref) { panic("a root the arm cannot handle") })
	t.Error("distillFanOut returned normally from a panicking source")
}

// TestDistillFanOutStopsOnCancel: a cancelled tick leaves its remaining chains
// alone and every worker returns, so distillOnce's deferred Close never runs
// against a source somebody is still reading.
func TestDistillFanOutStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var done int
	chains := distillChains([]distillsource.Ref{
		{Session: "a"}, {Session: "b"}, {Session: "c"}, {Session: "d"}, {Session: "e"},
	})
	distillFanOut(ctx, 2, chains, func(_ context.Context, _ distillsource.Ref) {
		mu.Lock()
		done++
		n := done
		mu.Unlock()
		if n == 1 {
			cancel()
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if done == len(chains) {
		t.Fatalf("every one of the %d chains ran after the cancel — the fan-out ignores shutdown", done)
	}
}

// TestDistillFanOutSurvivesAWorkerPanic: a panic inside a worker goroutine has
// no recover above it — runDistiller's and distillOnce's both sit in another
// goroutine — so the fan-out has to carry its own or one bad root takes the
// whole daemon down.
//
// WHO PANICS IS DECIDED BY WHERE THE CALLBACK RUNS, not by which root it holds:
// the queue hands chains out dynamically, so keying the panic on a session id
// would panic in the CALLER whenever it happened to draw that chain — the
// unwind path of the test above, not the one under test here. The caller
// therefore waits for a worker to have panicked, which also makes "a worker ran
// at all" an assertion instead of an assumption.
func TestDistillFanOutSurvivesAWorkerPanic(t *testing.T) {
	chains := distillChains([]distillsource.Ref{
		{Session: "a"}, {Session: "b"}, {Session: "c"}, {Session: "d"},
	})
	self := goroutineID()
	panicked := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var done int
	distillFanOut(context.Background(), 4, chains, func(_ context.Context, _ distillsource.Ref) {
		if goroutineID() != self {
			defer once.Do(func() { close(panicked) })
			panic("a root the arm cannot handle")
		}
		select {
		case <-panicked:
		case <-time.After(10 * time.Second):
			t.Error("no worker goroutine ever entered the callback — the probe would prove nothing")
		}
		mu.Lock()
		done++
		mu.Unlock()
	})
	mu.Lock()
	defer mu.Unlock()
	if done == 0 {
		t.Fatal("no chain survived the panicking one")
	}
}
