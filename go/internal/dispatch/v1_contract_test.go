package dispatch

// The two dispatcher halves of the /v1 admission contract (wave A03-W8):
// the reader of the resolved Retry-After steps (the largest step answers an
// ErrExternalNotOffered, A03-W6 handover) and the Retry-After of an external
// would-block (lead decision O1: TryAcquire answers an external acquire that
// is not eligible right now with a RejectionError carrying the Retry-After of
// its own position — the same resolver as Acquire's refusals, never the
// class-wide RetryAfterHint).

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"
)

func TestExternalRetryAfterStepsReader(t *testing.T) {
	d := New(slog.New(slog.DiscardHandler), DefaultSettings())
	t.Cleanup(d.Close)
	if got, want := d.ExternalRetryAfterSteps(), DefaultSettings().ExternalRetryAfterSteps; !slices.Equal(got, want) {
		t.Fatalf("default steps = %v, want %v", got, want)
	}
	set := DefaultSettings()
	set.ExternalRetryAfterSteps = []time.Duration{3 * time.Second, 7 * time.Second}
	d.UpdateSettings(set)
	got := d.ExternalRetryAfterSteps()
	if want := []time.Duration{3 * time.Second, 7 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("steps after the swap = %v, want %v", got, want)
	}
	got[0] = time.Hour // a copy: the caller cannot reach the dispatcher's list
	if again := d.ExternalRetryAfterSteps(); again[0] != 3*time.Second {
		t.Fatalf("the reader aliases the resolved steps: %v after mutating the returned slice", again)
	}
	set.ExternalRetryAfterSteps = nil // nil reads as the default (resolveRetryAfterSteps)
	d.UpdateSettings(set)
	if got, want := d.ExternalRetryAfterSteps(), DefaultSettings().ExternalRetryAfterSteps; !slices.Equal(got, want) {
		t.Fatalf("nil steps = %v, want the default %v", got, want)
	}
	set.ExternalRetryAfterSteps = []time.Duration{5 * time.Second, 2 * time.Second} // invalid ⇒ the default
	d.UpdateSettings(set)
	if got, want := d.ExternalRetryAfterSteps(), DefaultSettings().ExternalRetryAfterSteps; !slices.Equal(got, want) {
		t.Fatalf("invalid steps = %v, want the default %v", got, want)
	}
}

// TestTryAcquireExternalWouldBlockCarriesRetryAfter (O1): an external try on
// a full partition answers at once with a RejectionError around
// ErrWouldBlock — errors.Is still sees the sentinel, IsRejection stays false
// (defer semantics for the in-house consumers of the try door), and the
// Retry-After is one of the steps, from the own position (cold window: the
// class formula, quantized — here the smallest step, 3 s). The control in the
// other direction: an in-house try on a busy target keeps the bare sentinel.
func TestTryAcquireExternalWouldBlockCarriesRetryAfter(t *testing.T) {
	set := DefaultSettings()
	set.ExternalRetryAfterSteps = []time.Duration{3 * time.Second, 7 * time.Second}
	d := New(slog.New(slog.DiscardHandler), set)
	t.Cleanup(d.Close)
	const origin = "http://xu-try:8000"
	d.UpdatePolicy(Policy{Targets: map[string]TargetPolicy{origin: {Slots: 2, InternalReserved: 1}}})
	ctxA := ictx(Principal{ConsumerID: "c-a", TenantID: "t-a", FairWeight: 1})
	ctxB := ictx(Principal{ConsumerID: "c-b", TenantID: "t-b", FairWeight: 1})
	held, _, err := d.Acquire(ctxA, Request{Target: Target{Origin: origin}, Class: ClassExternal, Role: "chat"})
	if err != nil {
		t.Fatalf("first external lease: %v", err)
	}
	defer held.Release()

	start := time.Now()
	l, _, err := d.TryAcquire(ctxB, Request{Target: Target{Origin: origin}, Class: ClassExternal, Role: "chat"})
	if l != nil {
		l.Release()
		t.Fatal("external try on a full partition was admitted")
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("external try took %v, want an immediate answer (<= 50 ms)", took)
	}
	if !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("external try = %v, want ErrWouldBlock", err)
	}
	if IsRejection(err) {
		t.Fatal("the external would-block classifies as a rejection — IsRejection must stay unchanged")
	}
	var re *RejectionError
	if !errors.As(err, &re) {
		t.Fatalf("external would-block %T %v carries no RejectionError (O1: the Retry-After travels with the error)", err, err)
	}
	if re.RetryAfter != 3*time.Second {
		t.Fatalf("external would-block Retry-After = %v, want 3s (cold window: class formula 0, quantized onto the smallest step)", re.RetryAfter)
	}

	// Control: the in-house try door keeps the bare sentinel (Q-I3 consumers).
	const bgOrigin = "http://bg-try:8000"
	d.UpdatePolicy(Policy{Targets: map[string]TargetPolicy{origin: {Slots: 2, InternalReserved: 1}, bgOrigin: {Slots: 1}}})
	bgHeld, _, err := d.Acquire(context.Background(), Request{Target: Target{Origin: bgOrigin}, Class: ClassBackground, Role: "dream"})
	if err != nil {
		t.Fatalf("background occupier: %v", err)
	}
	defer bgHeld.Release()
	_, _, err = d.TryAcquire(context.Background(), Request{Target: Target{Origin: bgOrigin}, Class: ClassBackground, Role: "backfill"})
	if !errors.Is(err, ErrWouldBlock) || errors.As(err, &re) {
		t.Fatalf("in-house try on a busy target = %T %v, want the bare ErrWouldBlock", err, err)
	}
}
