package dispatch

// Review-2 gates of A03-W5 (reports/review/A03-W5-fremdreview-2.md): the
// ServeBy edges including the saturating Sub (F-1: a ServeBy more than ~292
// years back must still read as "predicted at once", not as a full budget),
// the ServeBy term anchored at the arrival like the setting term (F-2, mutant
// M26), and the expiry path canceling the runCtx it created (F-3, mutant M24).

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func r2Settings(maxWait time.Duration) Settings {
	s := DefaultSettings()
	s.ExternalMaxWait = maxWait
	return s
}

// ServeBy edges: past, inside and outside the margin, far future, and the
// saturating Sub of a ServeBy centuries back (F-1).
func TestWaitBudgetServeByEdges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serveBy   func() time.Time
		predicted bool
		wantAbout time.Duration
	}{
		{"past 1s", func() time.Time { return time.Now().Add(-time.Second) }, true, 0},
		{"now+499ms", func() time.Time { return time.Now().Add(499 * time.Millisecond) }, true, 0},
		{"now+500ms", func() time.Time { return time.Now().Add(500 * time.Millisecond) }, true, 0},
		{"now+700ms", func() time.Time { return time.Now().Add(700 * time.Millisecond) }, false, 200 * time.Millisecond},
		{"far future", func() time.Time { return time.Now().AddDate(1000, 0, 0) }, false, 300 * time.Millisecond},
		// Sub saturates at minDuration for a ServeBy > ~292 years back;
		// minDuration − serveMargin wraps around to a huge positive value.
		{"year 1600", func() time.Time { return time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC) }, true, 0},
		{"year 1 + 1ns", func() time.Time { return time.Time{}.Add(time.Nanosecond) }, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, r2Settings(300*time.Millisecond), partitionPolicy(2, 1))
			holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-a"), externalReq())
			defer holder.Release()
			req := externalReq()
			req.ServeBy = tc.serveBy()
			ctx, cancel := context.WithTimeout(xctx(context.Background(), "c1", "tenant-b"), 5*time.Second)
			defer cancel()
			start := time.Now()
			_, _, err := d.Acquire(ctx, req)
			el := time.Since(start)
			ts := target(t, d)
			t.Logf("%s: elapsed %v err %v rejects %+v", tc.name, el, err, ts.ExternalRejects)
			if !errors.Is(err, ErrWaitExpired) {
				t.Fatalf("err %v", err)
			}
			if tc.predicted {
				if ts.ExternalRejects != (ExternalRejectStats{Predicted: 1}) || el > 20*time.Millisecond {
					t.Fatalf("want predicted at once: elapsed %v rejects %+v", el, ts.ExternalRejects)
				}
				return
			}
			if ts.ExternalRejects != (ExternalRejectStats{Expired: 1}) || el < tc.wantAbout-50*time.Millisecond || el > tc.wantAbout+50*time.Millisecond {
				t.Fatalf("want expired after %v: elapsed %v rejects %+v", tc.wantAbout, el, ts.ExternalRejects)
			}
		})
	}
}

// F-2: the ServeBy term is anchored at the arrival like the setting term — a
// mutex wait before the enqueue must not be counted twice (kills M26).
func TestWaitBudgetServeByAnchorsAtArrival(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-a"), externalReq())
	defer holder.Release()
	ch := make(chan admission, 1)
	req := externalReq()
	start := time.Now()
	req.ServeBy = start.Add(800 * time.Millisecond)
	d.mu.Lock()
	startWaiter(xctx(context.Background(), "c1", "tenant-b"), d, "late", req, ch)
	time.Sleep(200 * time.Millisecond)
	d.mu.Unlock()
	a := recvAdmission(t, ch, "expiry")
	el := time.Since(start)
	t.Logf("ServeBy now+800ms, 200ms mutex hold: %v after arrival (%v)", el, a.err)
	if !errors.Is(a.err, ErrWaitExpired) || el < 250*time.Millisecond || el > 350*time.Millisecond {
		t.Fatalf("expiry %v (%v), want ≈ 300ms after arrival (ServeBy − m)", el, a.err)
	}
}

// r2DoneCtx is a parent ctx that is not a stdlib cancelCtx: a derived
// WithCancelCause child needs a propagation goroutine until it is canceled.
type r2DoneCtx struct {
	context.Context
	done chan struct{}
}

func (c r2DoneCtx) Done() <-chan struct{} { return c.done }

// F-3: the expiry path cancels the runCtx it created (kills M24 via the
// propagation goroutine of a non-stdlib parent).
func TestWaitBudgetExpiryCancelsRunCtx(t *testing.T) {
	d, _ := newTestDispatcher(t, r2Settings(50*time.Millisecond), partitionPolicy(2, 1))
	holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-a"), externalReq())
	defer holder.Release()
	parent := r2DoneCtx{Context: xctx(context.Background(), "c1", "tenant-b"), done: make(chan struct{})}
	defer close(parent.done)
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if _, _, err := d.Acquire(parent, externalReq()); !errors.Is(err, ErrWaitExpired) {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Only growth is a leak: goroutines of earlier tests in the package (waiters
	// ending at their budget, reapers of closed dispatchers) may exit meanwhile.
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d → %d after 20 expiries under a live non-stdlib parent ctx (runCtx not canceled)", before, after)
	}
}
