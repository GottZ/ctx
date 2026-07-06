// K1 wire-level pin (external test package: the probe consumes the REAL
// backends.Classify — backends does not import dispatch, so no cycle). Go ≥
// 1.23 places the context CAUSE into the http error chain; the wrap doctrine
// (ErrPreempted/ErrReaped wrap context.Canceled) is what keeps a preempted /
// reaped wire call in ClassCanceled — chain stops WITHOUT a health report.
// The naked-sentinel contrast documents R1: an unwrapped errors.New cause
// classifies as ClassServerFault and would health-poison the healthy target.
package dispatch_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// hangingServer blocks every request until the test ends.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv
}

// wireErr performs one GET under ctx against the hanging server and returns
// the transport error after cancelWith fires.
func wireErr(t *testing.T, ctx context.Context, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected a transport error, got a response")
	}
	return err
}

func TestErrSentinelsWrapContextCanceled(t *testing.T) {
	if !errors.Is(dispatch.ErrPreempted, context.Canceled) {
		t.Fatalf("ErrPreempted must wrap context.Canceled (K1)")
	}
	if !errors.Is(dispatch.ErrReaped, context.Canceled) {
		t.Fatalf("ErrReaped must wrap context.Canceled (K1)")
	}
}

// The K1 probe both ways: the wrapped cause is recognized via errors.Is over
// the http error chain AND a Classify consumer does NOT rate it ServerFault.
func TestK1WrappedCauseClassifiesCanceled(t *testing.T) {
	srv := hangingServer(t)
	runCtx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel(dispatch.ErrPreempted)
	}()
	err := wireErr(t, runCtx, srv.URL)
	if !errors.Is(err, dispatch.ErrPreempted) {
		t.Fatalf("wrapped cause not recognized through the wire error chain: %v", err)
	}
	if got := backends.Classify(err, backends.ProviderGeneric); got != backends.ClassCanceled {
		t.Fatalf("Classify(preempted wire err) = %v, want ClassCanceled — health poisoning (R1)", got)
	}
}

// Contrast pin (the R1 failure mode, kept as executable documentation): a
// NAKED errors.New sentinel as cancel cause lands in ClassServerFault — the
// exact misclassification the wrap doctrine exists to prevent.
func TestK1NakedSentinelWouldServerFault(t *testing.T) {
	srv := hangingServer(t)
	naked := errors.New("dispatch: preempted by interactive demand (naked)")
	runCtx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel(naked)
	}()
	err := wireErr(t, runCtx, srv.URL)
	if !errors.Is(err, naked) {
		t.Fatalf("premise broken: cause not in the chain: %v", err)
	}
	if got := backends.Classify(err, backends.ProviderGeneric); got != backends.ClassServerFault {
		t.Fatalf("contrast pin drifted: Classify(naked cause) = %v, want ClassServerFault", got)
	}
}

// End-to-end through the dispatcher: a reaped background lease's REAL wire
// call errors with ErrReaped in the chain and classifies ClassCanceled.
func TestK1ReapedLeaseWireClassification(t *testing.T) {
	srv := hangingServer(t)
	s := dispatch.DefaultSettings()
	s.LeaseMaxAge = 20 * time.Millisecond
	s.LeaseReapGrace = 5 * time.Millisecond
	d := dispatch.New(slog.Default(), s)
	t.Cleanup(d.Close)
	origin, err := dispatch.NormalizeOrigin(srv.URL)
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})

	lease, runCtx, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: srv.URL},
		Class:  dispatch.ClassBackground,
		Role:   "dream",
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	// No hint, no ctx deadline ⇒ the reaper's max_age fallback cancels the
	// running wire call (real ticker cadence is lazy; the wire error is what
	// this pin is about, so waiting on the real reaper would slow the suite —
	// trigger the wire abort through the identical path: max_age elapses and
	// the next reap tick fires; here we simply wait for the lazy tick's
	// equivalent by letting the request run until the cancel arrives).
	wireDone := make(chan error, 1)
	go func() { wireDone <- wireErr(t, runCtx, srv.URL) }()
	time.Sleep(40 * time.Millisecond)
	d.ReapForTest(time.Now())
	err = <-wireDone
	if !errors.Is(err, dispatch.ErrReaped) {
		t.Fatalf("reaped wire error must carry ErrReaped: %v", err)
	}
	if got := backends.Classify(err, backends.ProviderGeneric); got != backends.ClassCanceled {
		t.Fatalf("Classify(reaped wire err) = %v, want ClassCanceled", got)
	}
}
