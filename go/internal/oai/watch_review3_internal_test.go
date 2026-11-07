package oai

// Internal gates from Delta-Review 3 of W02-08 (part D of the rebase round),
// on the internal API only — no seam in the production code: the watcher
// ends at its own verdict (D-4, mutant R5), and the grace timer writes
// nothing once freeze has handed the outcome to the route (D-5, mutant MD3).
// Templates: the reviewer's probes (review3/sa-sock/proben/wt/
// zz_review3_sock_internal_test.go).

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
)

// TestWatcherStopsAtItsVerdict (D-4): the cancel the watcher gets records the
// cause and does NOT end the watcher's context (an unbuffered channel, no
// select race); the watcher must still make exactly one check and hand over
// exactly one verdict. Red (R5, cancel without return): the watcher loops on
// — a check and a cancel per tick.
func TestWatcherStopsAtItsVerdict(t *testing.T) {
	prev := watchEvery
	watchEvery = 5 * time.Millisecond
	defer func() { watchEvery = prev }()
	var checks, cancels atomic.Int64
	d := Deps{Settings: func() Settings { checks.Add(1); return Settings{Enabled: false} }}
	verdict := make(chan error) // unbuffered: the first verdict is handed over, a second one blocks the loop and is counted
	stop := d.watch(context.Background(), func(cause error) { cancels.Add(1); verdict <- cause }, newWatchTarget(&auth.ConsumerAuth{}, "chat", "id", 0))
	select {
	case c := <-verdict:
		if !errors.Is(c, errGatewayOff) {
			t.Fatalf("verdict %v, want errGatewayOff", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no verdict")
	}
	time.Sleep(20 * watchEvery)
	n, k := cancels.Load(), checks.Load()
	go func() { // unblock a looping watcher so that stop can return
		for range verdict {
		}
	}()
	stop()
	close(verdict)
	t.Logf("after the verdict and 20 ticks: %d cancels, %d checks", n, k)
	if n != 1 || k != 1 {
		t.Errorf("%d cancels, %d checks after the verdict, want 1 and 1: the watcher went on after its verdict", n, k)
	}
}

// md3Writer records whether anything reached the response writer after
// freeze returned.
type md3Writer struct {
	h      http.Header
	frozen atomic.Bool
	after  atomic.Int64
}

func (w *md3Writer) Header() http.Header { return w.h }
func (w *md3Writer) Write(p []byte) (int, error) {
	if w.frozen.Load() {
		w.after.Add(1)
	}
	return len(p), nil
}
func (w *md3Writer) WriteHeader(int) {
	if w.frozen.Load() {
		w.after.Add(1)
	}
}
func (w *md3Writer) Flush() {}

// md3Phase is how long TestTimerWritesNothingAfterFreeze stresses each
// phase: the window is microseconds wide, the reviewer measured 2–21 (open)
// and 6–67 (committed) hits of the mutant per 5 s.
const md3Phase = 5 * time.Second

// TestTimerWritesNothingAfterFreeze (D-5): the window "the grace timer fired
// and waits for mu while freeze runs", stressed without a seam for md3Phase
// per phase — (open) the timer armed ~200 µs ahead, freeze at a random
// offset around it; (committed) a frame commits, the keepalive ticks every
// 50 µs (below V38's 1-s floor, set directly), freeze at a random moment.
// Any write after freeze returned is a hit; the writer never may. Red (MD3,
// tick without its frozen check): writes after freeze in both phases.
func TestTimerWritesNothingAfterFreeze(t *testing.T) {
	for _, phase := range []string{"open", "committed"} {
		t.Run(phase, func(t *testing.T) {
			deadline := time.Now().Add(md3Phase)
			iters, hits, writesAfter := 0, 0, int64(0)
			for time.Now().Before(deadline) {
				iters++
				rw := &md3Writer{h: http.Header{}}
				s := newStreamWriter(rw, Settings{KeepaliveInterval: 50 * time.Microsecond}, func(error) {}, nil)
				switch phase {
				case "open":
					at := time.Now().Add(200 * time.Microsecond)
					s.arm(at)
					target := at.Add(time.Duration(rand.IntN(100)-20) * time.Microsecond)
					for time.Now().Before(target) {
					}
				case "committed":
					s.arm(time.Now().Add(time.Hour))
					if err := s.frame([]byte(`{"x":1}`)); err != nil {
						t.Fatal(err)
					}
					target := time.Now().Add(time.Duration(50+rand.IntN(400)) * time.Microsecond)
					for time.Now().Before(target) {
					}
				}
				s.freeze()
				rw.frozen.Store(true)
				time.Sleep(500 * time.Microsecond)
				s.freeze() // a timer the mutant re-armed after freeze is stopped again
				if a := rw.after.Load(); a > 0 {
					hits++
					writesAfter += a
				}
			}
			t.Logf("phase %s: %d iterations, %d with a write after freeze (%d writes)", phase, iters, hits, writesAfter)
			if hits > 0 {
				t.Errorf("%d of %d iterations wrote after freeze: the timer wrote after the route took the outcome", hits, iters)
			}
		})
	}
}
