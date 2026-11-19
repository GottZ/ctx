package rrf

// Issue #223: the deadline of the query path's cross-encoder call is the
// settings key rerank.timeout, capped by the wire's own rerank.Timeout and
// anchored at admission like the constant it replaces — the external rerank
// door's per-alias timeout_s has the same shape (oai/rerank.go attemptTimeout).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/rerank"
)

// stalledCrossEncoder never answers on its own: it holds every request until
// the client gives up (or a generous safety bound passes). The body is read
// first — the server notices a client that went away only once the request
// body is consumed, and the cleanup would otherwise wait out the bound.
func stalledCrossEncoder(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestRerankCrossEncoderTimeoutFollowsSetting: a configured deadline below the
// cap ends a stalled call at that deadline — fail-open, the order unchanged,
// the call still counted as wired — and is the admission's deadline hint.
//
// Falsifying implementation: the constant left in place. The call then runs
// into the stall's own 20 s bound and the elapsed-time assertion fails.
func TestRerankCrossEncoderTimeoutFollowsSetting(t *testing.T) {
	url := stalledCrossEncoder(t)
	rec := newCERecAdmitter(t)
	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}

	start := time.Now()
	out, tel, err := RerankCrossEncoder(context.Background(), backends.Backend{Host: url}, "m", 50, 1.0,
		1*time.Second, "q", results, ceAdmission(t, rec))
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("stalled call returned after %v — the configured 1 s deadline did not bind", elapsed)
	}
	if !tel.Wired {
		t.Errorf("telemetry = %+v, want wired: the lease was held and the wire contacted", tel)
	}
	for i, id := range []string{"A", "B", "C"} {
		if out[i].ID != id || out[i].RerankScore != nil {
			t.Errorf("out[%d] = %+v, want the pre-rerank order unscored (fail-open)", i, out[i])
		}
	}
	reqs := rec.recorded()
	if len(reqs) != 1 || reqs[0].DeadlineIn != 1*time.Second {
		t.Errorf("admission requests = %+v, want one with DeadlineIn 1s", reqs)
	}
}

// TestCrossEncoderTimeoutDefaultsAndCap: without the key (0) the deadline is
// rerank.Timeout, today's value; a value above it is capped there, the way
// the external door caps an alias's timeout_s. Read off the admission hint,
// which is the same value the wire deadline is set from.
func TestCrossEncoderTimeoutDefaultsAndCap(t *testing.T) {
	url, _, _ := usageCrossEncoderServer(t, 0, false)
	for _, tc := range []struct {
		configured time.Duration
		want       time.Duration
	}{
		{0, rerank.Timeout},
		{-time.Second, rerank.Timeout},
		{rerank.Timeout, rerank.Timeout},
		{10 * rerank.Timeout, rerank.Timeout},
		{30 * time.Second, 30 * time.Second},
	} {
		if got := crossEncoderTimeout(tc.configured); got != tc.want {
			t.Errorf("crossEncoderTimeout(%v) = %v, want %v", tc.configured, got, tc.want)
		}
		rec := newCERecAdmitter(t)
		results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
		if _, _, err := RerankCrossEncoder(context.Background(), backends.Backend{Host: url}, "m", 50, 1.0,
			tc.configured, "q", results, ceAdmission(t, rec)); err != nil {
			t.Fatalf("configured %v: %v", tc.configured, err)
		}
		if reqs := rec.recorded(); len(reqs) != 1 || reqs[0].DeadlineIn != tc.want {
			t.Errorf("configured %v: admission requests = %+v, want DeadlineIn %v", tc.configured, reqs, tc.want)
		}
	}
}

// slowAdmitter admits after a fixed wait — the lease wait a queued query sees
// on a rerank target with declared slots.
type slowAdmitter struct {
	*ceRecAdmitter
	wait time.Duration
}

func (s *slowAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	time.Sleep(s.wait)
	return s.ceRecAdmitter.Acquire(ctx, req)
}

// TestRerankCrossEncoderTimeoutStartsAtAdmission: the deadline counts from
// admission, like the constant did — a lease wait longer than the configured
// deadline does not end a call that then answers at once.
//
// Falsifying implementation: the deadline laid over ctx BEFORE Acquire — the
// 1.5 s wait then spends the whole 1 s budget and the call fails.
func TestRerankCrossEncoderTimeoutStartsAtAdmission(t *testing.T) {
	url, _, _ := usageCrossEncoderServer(t, 0, false)
	adm := &slowAdmitter{ceRecAdmitter: newCERecAdmitter(t), wait: 1500 * time.Millisecond}
	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	out, _, err := RerankCrossEncoder(context.Background(), backends.Backend{Host: url}, "m", 50, 1.0,
		1*time.Second, "q", results, ceAdmission(t, adm))
	if err != nil {
		t.Fatalf("RerankCrossEncoder after a 1.5 s lease wait with a 1 s deadline: %v — the deadline must start at admission", err)
	}
	if out[0].RerankScore == nil {
		t.Error("RerankScore not set — the sidecar answer was not consumed")
	}
}
