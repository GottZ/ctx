package dispatch

// W01-7 gate (design/01 §4.5 I-K10, §5 B6/B7/B7b): a consumer principal
// (ConsumerID != "", ApiKeyID == "") never takes a lease in a class whose
// External() is false, and a poisoned principal (Conflict) never takes any
// lease. Both refusals sit at BOTH admission doors after the class check and
// BEFORE the B8 block: no waiter, no wait slot, no downgrade, no lease — on a
// held, a free and a pass-through target alike (an empty policy is today's
// live state), and with the kill switch off. Every refusal is counted and logged at ERROR. The principal
// travels ONLY in the ctx through the TestMain hook (principal_test.go).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// consumerPrincipal is the shape handler.RequestPrincipal yields for a valid
// model-key request: no ApiKeyID, no HomeScope, no PrincipalID (I-K0).
func consumerPrincipal(n string) Principal {
	return Principal{TenantID: "tenant-" + n, ConsumerID: "consumer-" + n, ConsumerKind: "app", FairWeight: 10}
}

// levelRecorder records message AND level, so the gates can pin that a
// refusal is logged at ERROR (recordingHandler keeps messages only).
type levelRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r)
	h.mu.Unlock()
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

// errorLines counts ERROR records containing sub — and every record
// containing sub at all, so a downgraded level is visible as a mismatch.
func (h *levelRecorder) errorLines(sub string) (atError, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if !strings.Contains(r.Message, sub) {
			continue
		}
		total++
		if r.Level == slog.LevelError {
			atError++
		}
	}
	return atError, total
}

// door is one admission entry point.
type door struct {
	name string
	call func(d *Dispatcher, ctx context.Context, req Request) (*Lease, context.Context, error)
}

var bothDoors = []door{
	{"acquire", func(d *Dispatcher, ctx context.Context, req Request) (*Lease, context.Context, error) {
		return d.Acquire(ctx, req)
	}},
	{"try-acquire", func(d *Dispatcher, ctx context.Context, req Request) (*Lease, context.Context, error) {
		return d.TryAcquire(ctx, req)
	}},
}

// gateTarget is one admission situation of the test origin.
type gateTarget struct {
	name     string
	pol      Policy
	occupied bool // an interactive knowledge lease holds the only slot
	killed   bool // dispatch.enabled=false: every acquire is a pass-through
}

var gateTargets = []gateTarget{
	{"held 1-slot target", onSlotPolicy(1), true, false},
	{"free 1-slot target", onSlotPolicy(1), false, false},
	{"pass-through target", Policy{}, false, false},
	{"kill switch off", onSlotPolicy(1), false, true},
}

// doorOutcome is what one door call left behind: its return values, whether
// it returned before parking as a waiter, the target row while a parked
// waiter (if any) still sat in its queue, and the dispatcher counters.
type doorOutcome struct {
	lease   *Lease
	runCtx  context.Context
	err     error
	parked  TargetSnapshot
	counter Snapshot
}

// callDoor runs one door call in a goroutine so a regressed gate (the call
// parks as a waiter) cannot hang the test: it waits until the call returned
// or a waiter is queued, samples the target and counters, then cancels and
// releases whatever was admitted.
func callDoor(t *testing.T, d *Dispatcher, dr door, ctx context.Context, req Request) doorOutcome {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var returned atomic.Bool
	ch := make(chan doorOutcome, 1)
	go func() {
		l, rc, err := dr.call(d, ctx, req)
		returned.Store(true)
		ch <- doorOutcome{lease: l, runCtx: rc, err: err}
	}()
	waitFor(t, dr.name+" returned or queued", func() bool {
		ts := target(t, d)
		return returned.Load() || ts.Interactive.Waiting+ts.Background.Waiting > 0
	})
	parked, counter := target(t, d), d.Snapshot()
	cancel()
	out := <-ch
	out.parked, out.counter = parked, counter
	if out.lease != nil {
		out.lease.Release()
	}
	return out
}

// assertRefused checks that one door call was refused with want and left no
// trace beyond its own counter (occupierHeld: the occupier's slot).
func assertRefused(t *testing.T, o doorOutcome, want error, occupierHeld int) {
	t.Helper()
	if !errors.Is(o.err, want) || o.lease != nil || o.runCtx != nil {
		t.Errorf("door = (lease %v, runCtx %v, err %v), want (nil, nil, %v)", o.lease != nil, o.runCtx != nil, o.err, want)
	}
	if o.parked.Interactive.Waiting != 0 || o.parked.Background.Waiting != 0 {
		t.Errorf("Interactive.Waiting=%d Background.Waiting=%d, want 0/0 (no waiter, no wait slot)",
			o.parked.Interactive.Waiting, o.parked.Background.Waiting)
	}
	if o.parked.Held != occupierHeld || o.parked.Inflight != occupierHeld {
		t.Errorf("Held=%d Inflight=%d, want %d/%d (no lease beyond the occupier)",
			o.parked.Held, o.parked.Inflight, occupierHeld, occupierHeld)
	}
	if o.counter.ClassDowngrades != 0 {
		t.Errorf("ClassDowngrades=%d, want 0 (a refusal is never a B8 downgrade)", o.counter.ClassDowngrades)
	}
	if o.counter.InvalidClass != 0 {
		t.Errorf("InvalidClass=%d, want 0 (a principal refusal is not a class refusal)", o.counter.InvalidClass)
	}
}

// assertOneErrorLine pins exactly one log line for the refusal, at ERROR.
func assertOneErrorLine(t *testing.T, h *levelRecorder, sub string) {
	t.Helper()
	if atError, total := h.errorLines(sub); atError != 1 || total != 1 {
		t.Errorf("log lines %q: %d at ERROR of %d total, want exactly 1 at ERROR", sub, atError, total)
	}
}

// setupGateTarget builds a dispatcher for one situation; with occupied it
// holds the only slot with an interactive knowledge lease.
func setupGateTarget(t *testing.T, gt gateTarget) (*Dispatcher, *levelRecorder, int) {
	t.Helper()
	s := DefaultSettings()
	s.Enabled = !gt.killed
	h := &levelRecorder{}
	d := New(slog.New(h), s)
	d.UpdatePolicy(gt.pol)
	t.Cleanup(d.Close)
	if !gt.occupied {
		return d, h, 0
	}
	occ, _, err := d.Acquire(ictx(principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	t.Cleanup(occ.Release)
	return d, h, 1
}

// TestConsumerPrincipalRefusedInInhouseClasses pins I-K10 for both in-house
// classes at both doors: ErrClassUnauthorized, counted, logged at ERROR.
// The gate keys on ConsumerID alone: a principal that also carries a
// knowledge ApiKeyID (unreachable through handler.RequestPrincipal, which
// maps both credentials to Conflict) is refused just the same — a later
// refactor toward the K-03-1 form must not soften the in-house refusal.
func TestConsumerPrincipalRefusedInInhouseClasses(t *testing.T) {
	mixed := consumerPrincipal("m")
	mixed.ApiKeyID = "key-m"
	consumers := map[string]Principal{"consumer": consumerPrincipal("c"), "consumer with knowledge key id": mixed}
	for pname, p := range consumers {
		for _, gt := range gateTargets {
			for _, dr := range bothDoors {
				for _, class := range []Class{ClassInteractive, ClassBackground} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", pname, gt.name, dr.name, class), func(t *testing.T) {
						d, h, held := setupGateTarget(t, gt)
						req := Request{Target: Target{Origin: testOrigin}, Class: class, Role: "test"}
						o := callDoor(t, d, dr, withPrincipal(context.Background(), p), req)
						assertRefused(t, o, ErrClassUnauthorized, held)
						if o.counter.ClassUnauthorized != 1 || o.counter.PrincipalConflicts != 0 {
							t.Errorf("ClassUnauthorized=%d PrincipalConflicts=%d, want 1/0",
								o.counter.ClassUnauthorized, o.counter.PrincipalConflicts)
						}
						assertOneErrorLine(t, h, "consumer principal in a non-external class")
					})
				}
			}
		}
	}
}

// TestConflictPrincipalRefusedAtBothDoors pins the poisoned principal (B6):
// whatever else it carries — nothing, a knowledge ApiKeyID that would grant
// interactive, a ConsumerID — it is refused with ErrPrincipalConflict, which
// precedes the consumer check.
func TestConflictPrincipalRefusedAtBothDoors(t *testing.T) {
	poisoned := map[string]Principal{
		"bare":             {Conflict: true},
		"with knowledge":   {Conflict: true, ApiKeyID: "key-k", TenantID: "tenant-k", HomeScope: "scope-k"},
		"with consumer id": {Conflict: true, ConsumerID: "consumer-k", TenantID: "tenant-k"},
	}
	for pname, p := range poisoned {
		for _, gt := range gateTargets {
			for _, dr := range bothDoors {
				for _, class := range []Class{ClassInteractive, ClassBackground} {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", pname, gt.name, dr.name, class), func(t *testing.T) {
						d, h, held := setupGateTarget(t, gt)
						req := Request{Target: Target{Origin: testOrigin}, Class: class, Role: "test"}
						o := callDoor(t, d, dr, withPrincipal(context.Background(), p), req)
						assertRefused(t, o, ErrPrincipalConflict, held)
						if o.counter.PrincipalConflicts != 1 || o.counter.ClassUnauthorized != 0 {
							t.Errorf("PrincipalConflicts=%d ClassUnauthorized=%d, want 1/0",
								o.counter.PrincipalConflicts, o.counter.ClassUnauthorized)
						}
						assertOneErrorLine(t, h, "conflicting request principal")
					})
				}
			}
		}
	}
}

// TestPrincipalGateDoorOrder pins the entry order of both doors: ctx.Err()
// → ErrInvalidClass → ErrPrincipalConflict → ErrClassUnauthorized → B8. A
// canceled ctx and an out-of-table class are answered before the principal
// is even looked at, so neither principal counter moves.
func TestPrincipalGateDoorOrder(t *testing.T) {
	invalid := Request{Target: Target{Origin: testOrigin}, Class: numClasses, Role: "test"}
	for _, dr := range bothDoors {
		for pname, p := range map[string]Principal{"conflict": {Conflict: true}, "consumer": consumerPrincipal("o")} {
			t.Run(dr.name+"/"+pname+"/canceled ctx", func(t *testing.T) {
				d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
				ctx, cancel := context.WithCancel(withPrincipal(context.Background(), p))
				cancel()
				if _, _, err := dr.call(d, ctx, interactiveReq()); !errors.Is(err, context.Canceled) {
					t.Errorf("err = %v, want context.Canceled first", err)
				}
				if s := d.Snapshot(); s.PrincipalConflicts != 0 || s.ClassUnauthorized != 0 || s.InvalidClass != 0 {
					t.Errorf("counters %d/%d/%d, want 0/0/0", s.PrincipalConflicts, s.ClassUnauthorized, s.InvalidClass)
				}
			})
			t.Run(dr.name+"/"+pname+"/invalid class", func(t *testing.T) {
				d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
				l, _, err := dr.call(d, withPrincipal(context.Background(), p), invalid)
				if l != nil {
					l.Release()
				}
				if !errors.Is(err, ErrInvalidClass) {
					t.Errorf("err = %v, want ErrInvalidClass before the principal gate", err)
				}
				if s := d.Snapshot(); s.InvalidClass != 1 || s.PrincipalConflicts != 0 || s.ClassUnauthorized != 0 {
					t.Errorf("InvalidClass=%d PrincipalConflicts=%d ClassUnauthorized=%d, want 1/0/0",
						s.InvalidClass, s.PrincipalConflicts, s.ClassUnauthorized)
				}
			})
		}
	}
}

// TestPrincipalGateSentinelsAreNotRejections pins K29/K-02-8: both sentinels
// are code-bug answers (503 server_error, x-should-retry false), never a
// capacity verdict — IsRejection (429 + Retry-After) stays false, wrapped or
// not, and neither is a cancel cause.
func TestPrincipalGateSentinelsAreNotRejections(t *testing.T) {
	for _, err := range []error{ErrClassUnauthorized, ErrPrincipalConflict} {
		if IsRejection(err) || IsRejection(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("IsRejection(%v) = true, want false", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("%v wraps context.Canceled, want a plain sentinel", err)
		}
	}
	if errors.Is(ErrClassUnauthorized, ErrPrincipalConflict) || errors.Is(ErrPrincipalConflict, ErrClassUnauthorized) {
		t.Error("the two sentinels must be distinct")
	}
}

// TestInhouseClassesAreNotExternal pins the classifier the gate reads: no
// in-house class and no out-of-table value is external — until the external
// classes arrive (A03-W3) no consumer can take any lease.
func TestInhouseClassesAreNotExternal(t *testing.T) {
	for _, c := range []Class{ClassInteractive, ClassBackground, numClasses, Class(255)} {
		if c.External() {
			t.Errorf("Class(%d).External() = true, want false", uint8(c))
		}
	}
}
