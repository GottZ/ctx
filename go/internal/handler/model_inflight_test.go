package handler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
)

// Unit pins of the in-flight registry (W01-8) without a database: a fake
// ctx_model_auth_live, real contexts. The database half is
// model_inflight_integration_test.go.

type fakeLive struct {
	mu     sync.Mutex
	dead   map[string]bool
	err    error
	calls  [][]string
	during func() // runs inside the statement, outside f.mu
}

func (f *fakeLive) fn(_ context.Context, ids []string) ([]string, error) {
	f.mu.Lock()
	during := f.during
	f.mu.Unlock()
	if during != nil {
		during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), ids...))
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for _, id := range ids {
		if f.dead[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeLive) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type unitEntry struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	unreg  func()
}

func unitRegistry(f *fakeLive, forgotten *[]string) *InflightRegistry {
	var mu sync.Mutex
	return newInflightRegistry(f.fn, func() time.Duration { return time.Hour },
		func(h string) { mu.Lock(); *forgotten = append(*forgotten, h); mu.Unlock() }, time.Now, slog.New(slog.DiscardHandler))
}

func unitRegister(r *InflightRegistry, key, consumer, tenant, scope string, exp *time.Time) unitEntry {
	ctx, cancel := context.WithCancelCause(context.Background())
	ca := &auth.ConsumerAuth{ModelKeyID: key, ConsumerID: consumer, TenantID: tenant, TenantScope: scope, KeyExpiresAt: exp, IsValid: true}
	return unitEntry{ctx: ctx, cancel: cancel, unreg: r.register(ca, "hash-"+key, cancel, r.touchMark())}
}

func revoked(e unitEntry) bool { return errors.Is(context.Cause(e.ctx), auth.ErrCredentialRevoked) }

// TestInflightCancelKeyCounts: CancelKey ends every running request of the
// key and counts them — also the ones already ending for the same reason —,
// leaves other keys alone, forgets the hash; a key without requests is 0.
// CancelConsumer counts the same way.
func TestInflightCancelKeyCounts(t *testing.T) {
	f := &fakeLive{}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	a1 := unitRegister(r, "k1", "c1", "t1", "", nil)
	a2 := unitRegister(r, "k1", "c1", "t1", "", nil)
	b := unitRegister(r, "k2", "c1", "t1", "", nil)
	if n := r.CancelKey("k1"); n != 2 || !revoked(a1) || !revoked(a2) || revoked(b) {
		t.Fatalf("CancelKey(k1) = %d, revoked %v %v %v — want 2, both k1 requests, not k2", n, revoked(a1), revoked(a2), revoked(b))
	}
	if n := r.CancelKey("k1"); n != 2 {
		t.Errorf("a second CancelKey(k1) while both still unwind = %d, want 2 (they end for the same reason)", n)
	}
	if len(forgotten) != 2 || forgotten[0] != "hash-k1" {
		t.Errorf("forgotten hashes %v, want hash-k1 twice (once per ended request)", forgotten)
	}
	a1.unreg()
	a2.unreg()
	a1.unreg() // idempotent
	if n := r.CancelKey("k1"); n != 0 {
		t.Errorf("CancelKey of a key without requests = %d, want 0", n)
	}
	if n := r.CancelConsumer("c1"); n != 1 || !revoked(b) {
		t.Errorf("CancelConsumer(c1) = %d (k2 revoked %v), want 1", n, revoked(b))
	}
	// The same count for a consumer (Fremd-Review 2 Y8): a second
	// model-consumer-delete while the request still unwinds counts it.
	if n := r.CancelConsumer("c1"); n != 1 {
		t.Errorf("a second CancelConsumer(c1) while k2's request still unwinds = %d, want 1 (it ends for the same reason)", n)
	}
	b.unreg()
	if s := r.stats(); s.running != 0 || s.keys != 0 || s.cancelled != 3 {
		t.Errorf("stats %+v, want empty with 3 ended", s)
	}
}

// TestInflightTimerPerKey: one timer per key at the earliest expiry of its
// requests, never more timers than keys; the timer ends the requests whose
// expiry passed and moves to the next one; a NOTIFY's expires_at moves it
// (null stops it); the last request gone stops it.
func TestInflightTimerPerKey(t *testing.T) {
	f := &fakeLive{}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	soon, later := time.Now().Add(150*time.Millisecond), time.Now().Add(time.Hour)
	a := unitRegister(r, "k1", "c1", "t1", "", &soon)
	b := unitRegister(r, "k1", "c1", "t1", "", &later)
	c := unitRegister(r, "k2", "c2", "t1", "", &later)
	d := unitRegister(r, "k3", "c3", "t1", "", nil)
	if s := r.stats(); s.timers != 2 || s.keys != 3 {
		t.Fatalf("stats %+v, want 2 timers (k1, k2) over 3 keys", s)
	}
	select {
	case <-a.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the expired request did not end")
	}
	if !revoked(a) || revoked(b) || revoked(c) || revoked(d) {
		t.Fatalf("revoked a %v b %v c %v d %v — want only a", revoked(a), revoked(b), revoked(c), revoked(d))
	}
	// A rotation's grace: k2's expiry moves to 100 ms from now.
	at := time.Now().Add(100 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	r.ModelAccessNotify(`{"table":"context_model_keys","op":"UPDATE","id":"k2","consumer_id":"c2","expires_at":"` + at + `"}`)
	select {
	case <-c.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the moved expiry did not end k2's request")
	}
	// expires_at null stops k1's timer.
	r.ModelAccessNotify(`{"table":"context_model_keys","op":"UPDATE","id":"k1","consumer_id":"c1","expires_at":null}`)
	if s := r.stats(); s.timers != 0 {
		t.Errorf("%d timers after k1's expiry was cleared and k2's fired, want 0", s.timers)
	}
	for _, e := range []unitEntry{a, b, c, d} {
		e.unreg()
	}
	if s := r.stats(); s.running != 0 || s.keys != 0 || s.timers != 0 {
		t.Errorf("stats %+v, want empty", s)
	}
}

// TestInflightNotifyChecksTouchedKeys: a NOTIFY touches the registered keys
// of its row (key, consumer, tenant, scope) and the worker checks exactly
// those in ONE statement; an untouched row asks nothing; an unreadable
// payload or an unknown table asks for a full sweep; a failed check ends
// nothing and asks again with the next one.
func TestInflightNotifyChecksTouchedKeys(t *testing.T) {
	f := &fakeLive{dead: map[string]bool{"k1": true, "k2": true, "k3": true}}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Go(func() { r.Run(ctx) })
	defer func() { cancel(); done.Wait() }()
	k1 := unitRegister(r, "k1", "c1", "t1", "s1", nil)
	k2 := unitRegister(r, "k2", "c2", "t2", "s2", nil)
	k3 := unitRegister(r, "k3", "c3", "t3", "s3", nil)
	defer func() { k1.unreg(); k2.unreg(); k3.unreg() }()
	wait := func(e unitEntry) bool {
		select {
		case <-e.ctx.Done():
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	r.ModelAccessNotify(`{"table":"context_model_consumers","op":"UPDATE","id":"nobody"}`)
	r.ModelAccessNotify(`{"table":"context_tenant_scopes","op":"DELETE","scope":"s1","tenant_id":"t1"}`)
	if !wait(k1) || revoked(k2) || revoked(k3) {
		t.Fatalf("scope s1 deleted: k1 %v k2 %v k3 %v — want only k1", revoked(k1), revoked(k2), revoked(k3))
	}
	if n := f.n(); n != 1 || len(f.calls[0]) != 1 || f.calls[0][0] != "k1" {
		t.Errorf("checks %v, want one statement over k1 (the untouched row asked nothing)", f.calls)
	}
	f.mu.Lock()
	f.err = errors.New("database down (fake)")
	f.mu.Unlock()
	r.ModelAccessNotify(`{"table":"context_tenants","op":"UPDATE","id":"t2"}`)
	if !waitUnit(func() bool { return r.sweepErrors.Load() == 1 }) || revoked(k2) {
		t.Fatalf("failed check: errors %d, k2 revoked %v — want 1 error, nothing ended", r.sweepErrors.Load(), revoked(k2))
	}
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	r.ModelAccessNotify(`not json`)
	if !wait(k2) || !wait(k3) {
		t.Fatalf("an unreadable payload must sweep everything: k2 %v k3 %v", revoked(k2), revoked(k3))
	}
}

func waitUnit(ok func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return true
}

// TestInflightSweepFloorIsV49: the registry's floor equals what V49 lets
// through — 1 s passes validation, 999 ms does not — and a zero interval
// sweeps at the floor instead of spinning.
func TestInflightSweepFloorIsV49(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "w018-test")
	for _, c := range []struct {
		v  string
		ok bool
	}{{"1", true}, {"0.999", false}} {
		_, issues := config.Build([]config.Override{{Key: "gateway.inflight_sweep_interval", Value: c.v}}, nil)
		if (len(issues) == 0) != c.ok {
			t.Errorf("gateway.inflight_sweep_interval=%s: issues %v, want ok=%v", c.v, issues, c.ok)
		}
	}
	if inflightSweepFloor != time.Second {
		t.Errorf("inflightSweepFloor %v, want V49's 1 s", inflightSweepFloor)
	}
	var calls atomic.Int64
	r := newInflightRegistry(func(context.Context, []string) ([]string, error) { calls.Add(1); return nil, nil },
		func() time.Duration { return 0 }, func(string) {}, time.Now, slog.New(slog.DiscardHandler))
	if got := r.period(); got != inflightSweepFloor {
		t.Errorf("period with a zero interval = %v, want the floor", got)
	}
}

// TestInflightRegistrationRace: a revocation signal that arrives while a
// request's database check runs finds no entry to end; the registration
// after the check sees the moved mark and has the key checked once more —
// the dead key's request ends. A registration without a signal in between
// asks nothing. A check ends only entries registered before its statement.
func TestInflightRegistrationRace(t *testing.T) {
	f := &fakeLive{dead: map[string]bool{"k1": true}}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Go(func() { r.Run(ctx) })
	defer func() { cancel(); done.Wait() }()

	quiet := unitRegister(r, "k2", "c2", "t1", "", nil)
	defer quiet.unreg()
	time.Sleep(20 * time.Millisecond)
	if n := f.n(); n != 0 {
		t.Fatalf("%d checks for a registration without a signal, want 0", n)
	}
	mark := r.touchMark()
	// The signal arrives while the door's check for k1 still runs.
	r.ModelAccessNotify(`{"table":"context_model_keys","op":"UPDATE","id":"k1","consumer_id":"c1","expires_at":null}`)
	rctx, rcancel := context.WithCancelCause(context.Background())
	unreg := r.register(&auth.ConsumerAuth{ModelKeyID: "k1", ConsumerID: "c1", TenantID: "t1", IsValid: true}, "hash-k1", rcancel, mark)
	defer unreg()
	select {
	case <-rctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the request registered after a signal during its check was not checked again")
	}
	if !errors.Is(context.Cause(rctx), auth.ErrCredentialRevoked) {
		t.Errorf("cause %v, want auth.ErrCredentialRevoked", context.Cause(rctx))
	}
	if revoked(quiet) {
		t.Error("the live key's request was ended")
	}
}

// TestInflightCheckEndsOnlyOlderEntries: a request registered while a
// liveness statement runs passed its own database check after the
// statement's snapshot; the statement's "dead" does not end it (a tenant
// suspended and resumed in between), the older request on the key ends.
func TestInflightCheckEndsOnlyOlderEntries(t *testing.T) {
	f := &fakeLive{dead: map[string]bool{"k1": true}}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	old := unitRegister(r, "k1", "c1", "t1", "", nil)
	defer old.unreg()
	var young unitEntry
	f.during = func() { young = unitRegister(r, "k1", "c1", "t1", "", nil) }
	r.ModelAccessNotify(`{"table":"context_tenants","op":"UPDATE","id":"t1"}`)
	r.check(context.Background(), false)
	defer young.unreg()
	if !revoked(old) || revoked(young) {
		t.Fatalf("old revoked %v, young revoked %v — want only the request registered before the statement", revoked(old), revoked(young))
	}
}

// TestInflightRegistrationTakesTheMovedExpiry (Fremd-Review 1 B1): a
// rotation's NOTIFY that shortens a key's expiry while a request's database
// check runs finds no entry; the request registers with the expiry the door
// read before — and takes the moved one, so it ends at the grace end, not
// at the next sweep. A registration without a NOTIFY in between keeps its
// own expiry.
func TestInflightRegistrationTakesTheMovedExpiry(t *testing.T) {
	f := &fakeLive{}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	late := time.Now().Add(time.Hour)
	mark := r.touchMark()
	grace := time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	r.ModelAccessNotify(`{"table":"context_model_keys","op":"UPDATE","id":"k1","consumer_id":"c1","expires_at":"` + grace + `"}`)
	ctx, cancel := context.WithCancelCause(context.Background())
	unreg := r.register(&auth.ConsumerAuth{ModelKeyID: "k1", ConsumerID: "c1", TenantID: "t1", KeyExpiresAt: &late, IsValid: true}, "hash-k1", cancel, mark)
	defer unreg()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the request that raced the rotation did not end at the moved expiry")
	}
	if !errors.Is(context.Cause(ctx), auth.ErrCredentialRevoked) {
		t.Errorf("cause %v, want auth.ErrCredentialRevoked", context.Cause(ctx))
	}
	// A later registration (its check began after the NOTIFY) keeps the
	// expiry it read.
	fresh := unitRegister(r, "k1", "c1", "t1", "", &late)
	defer fresh.unreg()
	time.Sleep(300 * time.Millisecond)
	if revoked(fresh) {
		t.Error("a registration after the NOTIFY took the remembered expiry")
	}
}

// TestInflightFailedCheckIsRetried (Fremd-Review 1 X11): a NOTIFY's
// liveness check that meets a database error ends nothing and is asked
// again after the door's database deadline without a new signal.
func TestInflightFailedCheckIsRetried(t *testing.T) {
	f := &fakeLive{dead: map[string]bool{"k1": true}, err: errors.New("database hiccup (fake)")}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Go(func() { r.Run(ctx) })
	defer func() { cancel(); done.Wait() }()
	k1 := unitRegister(r, "k1", "c1", "t1", "", nil)
	defer k1.unreg()
	r.ModelAccessNotify(`{"table":"context_model_consumers","op":"UPDATE","id":"c1"}`)
	if !waitUnit(func() bool { return r.sweepErrors.Load() == 1 }) || revoked(k1) {
		t.Fatalf("first check: errors %d, revoked %v — want one failure, nothing ended", r.sweepErrors.Load(), revoked(k1))
	}
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	select {
	case <-k1.ctx.Done():
	case <-time.After(modelAuthDBDeadline + time.Second):
		t.Fatalf("the failed check was not asked again within %v (checks %v)", modelAuthDBDeadline+time.Second, f.calls)
	}
}

// TestInflightExtensionMovesTheTimer (Fremd-Review 1 X6): a NOTIFY that
// moves a key's expiry later moves its timer later — the request does not
// end at the expiry it was authenticated with.
func TestInflightExtensionMovesTheTimer(t *testing.T) {
	f := &fakeLive{}
	var forgotten []string
	r := unitRegistry(f, &forgotten)
	soon := time.Now().Add(150 * time.Millisecond)
	e := unitRegister(r, "k1", "c1", "t1", "", &soon)
	defer e.unreg()
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	r.ModelAccessNotify(`{"table":"context_model_keys","op":"UPDATE","id":"k1","consumer_id":"c1","expires_at":"` + later + `"}`)
	time.Sleep(400 * time.Millisecond)
	if revoked(e) {
		t.Fatal("the request ended at the old expiry although a NOTIFY extended it")
	}
	if s := r.stats(); s.timers != 1 {
		t.Errorf("%d timers, want 1 (moved to the extended expiry)", s.timers)
	}
}
