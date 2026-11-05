package handler

// W01-7 gates over the REAL wiring (design/01 §4.5 and §7 W01-7, I-K10, B6,
// B7, B7b): a real dispatch.Dispatcher with the production principal hook
// installed exactly like cmd/ctxd/main.go (dispatch.SetPrincipalHook(
// RequestPrincipal)) and the credentials in the request ctx the way the two
// doors put them there. A consumer never takes a lease in an in-house class
// — not by a downgrade into the background queue, not on a free slot, not
// through the pass-through clause — and a context with both credentials (or
// an invalid ConsumerAuth) is a poisoned principal that never takes any
// lease. Observable without test exports: Snapshot().Targets[i].
// Interactive/Background.Waiting, .Held, .Inflight and the counters. The
// hook is process-global: these tests are deliberately not parallel and
// restore the nil hook on cleanup.

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dispatch"
)

const (
	gateHeldOrigin = "http://gpu-held:8089" // 1 slot, held by a knowledge lease
	gateFreeOrigin = "http://gpu-free:8089" // 1 slot, free
	gatePassOrigin = "http://gpu-pass:8089" // no policy row: pass-through (today's live state)
)

// installRequestPrincipalHook wires the production hook for one test.
func installRequestPrincipalHook(t *testing.T) {
	t.Helper()
	dispatch.SetPrincipalHook(RequestPrincipal)
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
}

func testConsumerAuth() *auth.ConsumerAuth {
	return &auth.ConsumerAuth{
		ModelKeyID:  "mk-1",
		ConsumerID:  "consumer-1",
		TenantID:    "tenant-1",
		TenantScope: "scope-1",
		Kind:        auth.KindApp,
		FairWeight:  7,
		IsValid:     true,
	}
}

func withConsumerAuth(ctx context.Context, ca *auth.ConsumerAuth) context.Context {
	return context.WithValue(ctx, consumerAuthKey, ca)
}

func withAuthResult(ctx context.Context, ar *auth.AuthResult) context.Context {
	return context.WithValue(ctx, authResultKey, ar)
}

// newGateDispatcher builds the three-target fixture; the held target's only
// slot is taken by an interactive acquire on a knowledge ctx — which also
// pins that the knowledge branch still grants interactive through the
// installed hook.
func newGateDispatcher(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		gateHeldOrigin: {Slots: 1},
		gateFreeOrigin: {Slots: 1},
	}})
	occ, _, err := d.Acquire(withAuthResult(context.Background(), testAuthResult()),
		dispatch.Request{Target: dispatch.Target{Origin: gateHeldOrigin}, Class: dispatch.ClassInteractive, Role: "occupier"})
	if err != nil {
		t.Fatalf("knowledge occupier: %v", err)
	}
	if occ.Class() != dispatch.ClassInteractive {
		t.Fatalf("knowledge occupier class = %v, want interactive (knowledge branch must stay byte-identical)", occ.Class())
	}
	t.Cleanup(occ.Release)
	return d
}

func targetSnap(d *dispatch.Dispatcher, origin string) dispatch.TargetSnapshot {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == origin {
			return ts
		}
	}
	return dispatch.TargetSnapshot{}
}

// gateCase is one door × class × target probe.
type gateCase struct {
	name   string
	try    bool
	class  dispatch.Class
	origin string
	held   int // slots held by the fixture occupier on this origin
}

func gateCases() []gateCase {
	var out []gateCase
	for _, tg := range []struct {
		name, origin string
		held         int
	}{{"held", gateHeldOrigin, 1}, {"free", gateFreeOrigin, 0}, {"pass-through", gatePassOrigin, 0}} {
		for _, door := range []struct {
			name string
			try  bool
		}{{"acquire", false}, {"try-acquire", true}} {
			for _, c := range []dispatch.Class{dispatch.ClassInteractive, dispatch.ClassBackground} {
				out = append(out, gateCase{door.name + "/" + c.String() + "/" + tg.name, door.try, c, tg.origin, tg.held})
			}
		}
	}
	return out
}

// gateOutcome is what one door call left behind, sampled while a parked
// waiter (regressed gate) still sat in its queue.
type gateOutcome struct {
	lease  *dispatch.Lease
	runCtx context.Context
	err    error
	ts     dispatch.TargetSnapshot
	before dispatch.Snapshot
	after  dispatch.Snapshot
}

// runGateCase calls one door in a goroutine so a regressed gate cannot hang
// the test: it waits until the call returned or parked, samples, then
// cancels and releases whatever was admitted.
func runGateCase(t *testing.T, d *dispatch.Dispatcher, ctx context.Context, gc gateCase) gateOutcome {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := gateOutcome{before: d.Snapshot()}
	req := dispatch.Request{Target: dispatch.Target{Origin: gc.origin}, Class: gc.class, Role: "gate"}
	var returned atomic.Bool
	ch := make(chan gateOutcome, 1)
	go func() {
		var o gateOutcome
		if gc.try {
			o.lease, o.runCtx, o.err = d.TryAcquire(ctx, req)
		} else {
			o.lease, o.runCtx, o.err = d.Acquire(ctx, req)
		}
		returned.Store(true)
		ch <- o
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !returned.Load() {
		if ts := targetSnap(d, gc.origin); ts.Interactive.Waiting+ts.Background.Waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: neither returned nor queued within 3s", gc.name)
		}
		time.Sleep(2 * time.Millisecond)
	}
	out.ts, out.after = targetSnap(d, gc.origin), d.Snapshot()
	cancel()
	o := <-ch
	out.lease, out.runCtx, out.err = o.lease, o.runCtx, o.err
	if out.lease != nil {
		out.lease.Release()
	}
	return out
}

// assertGateRefused checks one refused door call: the sentinel, no lease, no
// waiter in either queue, no slot beyond the occupier's, no downgrade, and
// exactly one tick of the expected counter.
func assertGateRefused(t *testing.T, gc gateCase, o gateOutcome, want error, counter func(dispatch.Snapshot) int64) {
	t.Helper()
	if !errors.Is(o.err, want) || o.lease != nil || o.runCtx != nil {
		t.Errorf("%s: (lease %v, runCtx %v, err %v), want (nil, nil, %v)", gc.name, o.lease != nil, o.runCtx != nil, o.err, want)
	}
	if o.ts.Interactive.Waiting != 0 || o.ts.Background.Waiting != 0 {
		t.Errorf("%s: Interactive.Waiting=%d Background.Waiting=%d, want 0/0", gc.name, o.ts.Interactive.Waiting, o.ts.Background.Waiting)
	}
	if o.ts.Held != gc.held || o.ts.Inflight != gc.held {
		t.Errorf("%s: Held=%d Inflight=%d, want %d/%d (occupier only)", gc.name, o.ts.Held, o.ts.Inflight, gc.held, gc.held)
	}
	if dg := o.after.ClassDowngrades - o.before.ClassDowngrades; dg != 0 {
		t.Errorf("%s: ClassDowngrades +%d, want +0", gc.name, dg)
	}
	if ic := o.after.InvalidClass - o.before.InvalidClass; ic != 0 {
		t.Errorf("%s: InvalidClass +%d, want +0", gc.name, ic)
	}
	if n := counter(o.after) - counter(o.before); n != 1 {
		t.Errorf("%s: refusal counter +%d, want +1", gc.name, n)
	}
}

// TestConsumerPrincipalNeverEntersBestandsklassen is the I-K10 gate (B7,
// B7b): a valid consumer ctx through the production hook is refused with
// ErrClassUnauthorized at both doors in both in-house classes.
func TestConsumerPrincipalNeverEntersBestandsklassen(t *testing.T) {
	installRequestPrincipalHook(t)
	for _, gc := range gateCases() {
		t.Run(gc.name, func(t *testing.T) {
			d := newGateDispatcher(t)
			o := runGateCase(t, d, withConsumerAuth(context.Background(), testConsumerAuth()), gc)
			assertGateRefused(t, gc, o, dispatch.ErrClassUnauthorized, func(s dispatch.Snapshot) int64 { return s.ClassUnauthorized })
			if n := o.after.PrincipalConflicts - o.before.PrincipalConflicts; n != 0 {
				t.Errorf("%s: PrincipalConflicts +%d, want +0", gc.name, n)
			}
		})
	}
}

// TestRequestPrincipalConflictRefused is the B6 gate: a ctx with both
// credentials — or an invalid ConsumerAuth, which ModelAuth never stores —
// resolves to a poisoned principal (never the zero principal, which is ctx'
// own detached work) and is refused with ErrPrincipalConflict at both doors.
func TestRequestPrincipalConflictRefused(t *testing.T) {
	installRequestPrincipalHook(t)
	invalid := testConsumerAuth()
	invalid.IsValid = false
	for cname, ctx := range map[string]context.Context{
		"both credentials":      withAuthResult(withConsumerAuth(context.Background(), testConsumerAuth()), testAuthResult()),
		"invalid consumer auth": withConsumerAuth(context.Background(), invalid),
	} {
		for _, gc := range gateCases() {
			t.Run(cname+"/"+gc.name, func(t *testing.T) {
				d := newGateDispatcher(t)
				o := runGateCase(t, d, ctx, gc)
				assertGateRefused(t, gc, o, dispatch.ErrPrincipalConflict, func(s dispatch.Snapshot) int64 { return s.PrincipalConflicts })
				if n := o.after.ClassUnauthorized - o.before.ClassUnauthorized; n != 0 {
					t.Errorf("%s: ClassUnauthorized +%d, want +0", gc.name, n)
				}
			})
		}
	}
}

// TestRequestPrincipalConsumerBranch pins the §4.5 field mapping and I-K0:
// a consumer principal carries ApiKeyID == "" and ConsumerID != "" (no
// HomeScope, no PrincipalID — a consumer has no corpus home and is no
// person); a conflict is exactly {Conflict: true}, never the zero principal.
// The knowledge branch keeps its full four-field mapping (incl. PrincipalID,
// which the TestRequestPrincipal fixture leaves empty).
func TestRequestPrincipalConsumerBranch(t *testing.T) {
	ctx := context.Background()
	got := RequestPrincipal(withConsumerAuth(ctx, testConsumerAuth()))
	want := dispatch.Principal{TenantID: "tenant-1", ConsumerID: "consumer-1", ConsumerKind: "app", FairWeight: 7}
	if got != want {
		t.Errorf("consumer principal = %+v, want %+v", got, want)
	}
	if got.ApiKeyID != "" || got.ConsumerID == "" || got.HomeScope != "" || got.PrincipalID != "" || got.Conflict {
		t.Errorf("I-K0 violated: %+v (want ApiKeyID \"\", ConsumerID set, no HomeScope/PrincipalID, no Conflict)", got)
	}

	peer := testConsumerAuth()
	peer.Kind = auth.KindCtxPeer
	if got := RequestPrincipal(withConsumerAuth(ctx, peer)); got.ConsumerKind != "ctx-peer" || got.ApiKeyID != "" {
		t.Errorf("ctx-peer principal = %+v, want ConsumerKind ctx-peer, ApiKeyID \"\"", got)
	}

	conflict := dispatch.Principal{Conflict: true}
	both := withAuthResult(withConsumerAuth(ctx, testConsumerAuth()), testAuthResult())
	if got := RequestPrincipal(both); got != conflict {
		t.Errorf("both credentials = %+v, want %+v (never the zero principal, never a mix)", got, conflict)
	}
	invalid := testConsumerAuth()
	invalid.IsValid = false
	if got := RequestPrincipal(withConsumerAuth(ctx, invalid)); got != conflict {
		t.Errorf("invalid ConsumerAuth = %+v, want %+v", got, conflict)
	}
	knowledge := &auth.AuthResult{IsValid: true, ApiKeyID: "key-2", TenantID: "tenant-2", HomeScope: "scope-2", PrincipalID: "person-2"}
	wantKnowledge := dispatch.Principal{ApiKeyID: "key-2", TenantID: "tenant-2", HomeScope: "scope-2", PrincipalID: "person-2"}
	if got := RequestPrincipal(withAuthResult(ctx, knowledge)); got != wantKnowledge {
		t.Errorf("knowledge principal = %+v, want %+v (no consumer fields, no Conflict)", got, wantKnowledge)
	}
}
