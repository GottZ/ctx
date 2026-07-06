package dispatch

// MW4 probes: ctx-bound class authorization (design/03 §4.1.1, D3-W2 gates).
// The package tests install the principal hook ONCE in TestMain (boot-once
// discipline, mirroring cmd/ctxd/main.go): withPrincipal is the ONLY
// injection path — exactly the structural claim of §4.1.1 that a principal
// travels in the request ctx, never as an Acquire parameter.

import (
	"context"
	"os"
	"testing"
)

// ctxPrincipalKey is the test twin of the handler auth ctx key.
type ctxPrincipalKey struct{}

// withPrincipal binds a caller principal to ctx — the test-side injector the
// TestMain hook resolves (production: handler.RequestPrincipal over the auth
// middleware's AuthResult).
func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxPrincipalKey{}, p)
}

// ictx is the shorthand for a fresh authenticated interactive caller ctx.
func ictx(p Principal) context.Context {
	return withPrincipal(context.Background(), p)
}

func TestMain(m *testing.M) {
	SetPrincipalHook(func(ctx context.Context) Principal {
		p, _ := ctx.Value(ctxPrincipalKey{}).(Principal)
		return p
	})
	os.Exit(m.Run())
}

// TestEmptyApiKeyIDDowngradesNotAnonymousBucket is the S9 gate (D3-W2): a
// ctx-bound AuthResult whose ApiKeyID is "" (the chat.go:52-59 synthetic-
// injection pattern) is a B8 violation — downgrade to background + counter +
// slog-ERROR — and must NEVER form an anonymous "" bucket in the interactive
// FIFO (under the B9 cap that bucket would couple all such requests
// tenant-übergreifend).
func TestEmptyApiKeyIDDowngradesNotAnonymousBucket(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(ictx(principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	// S9 fixture: AuthResult present, ApiKeyID empty (only scope set).
	s9 := Principal{TenantID: "tenant-s9", HomeScope: "scope-s9"}
	startWaiter(withPrincipal(ctx, s9), d, "s9", interactiveReq(), ch)
	waitFor(t, "S9 acquire queued as background", func() bool { return waitingBackground(d) == 1 })
	if got := waitingInteractive(d); got != 0 {
		t.Fatalf("empty-ApiKeyID principal sits in the interactive FIFO (anonymous bucket): waiting=%d", got)
	}
	if got := d.Snapshot().ClassDowngrades; got != 1 {
		t.Fatalf("downgrade counter: got %d want 1", got)
	}
	if !h.contains("downgraded to background") {
		t.Fatalf("expected slog-ERROR for the S9 downgrade")
	}
	occ.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("downgraded acquire must still be served: %v", a.err)
	}
	if a.lease.Class() != ClassBackground {
		t.Fatalf("lease class: got %v want background", a.lease.Class())
	}
	a.lease.Release()
}

// TestStoredPrincipalVariableIsInert pins the §4.1.1 core claim: an
// AuthResult-derived principal HELD IN A VARIABLE has no path into Acquire —
// Request carries no principal field (compile-enforced), and only the ctx
// counts. An interactive acquire on a detached ctx downgrades although the
// caller "owns" a perfectly valid principal value.
func TestStoredPrincipalVariableIsInert(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), Policy{})
	stored := principal("stolen") // e.g. captured by an API-triggered audit drainer
	_ = stored                    // there is NO way to pass it — that is the probe
	l, _, err := d.Acquire(context.Background(), interactiveReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()
	if l.Class() != ClassBackground {
		t.Fatalf("detached-ctx interactive must downgrade despite a stored principal: got %v", l.Class())
	}
	if got := d.Snapshot().ClassDowngrades; got != 1 {
		t.Fatalf("downgrade counter: got %d want 1", got)
	}
	if !h.contains("downgraded to background") {
		t.Fatalf("expected slog-ERROR for the downgrade")
	}
}

// TestCtxPrincipalGrantsInteractive is the positive D3-W2 gate: a regular
// request ctx with a non-empty ApiKeyID lands in the interactive class — no
// downgrade, no counter, no ERROR.
func TestCtxPrincipalGrantsInteractive(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	l, _, err := d.Acquire(ictx(principal("a")), interactiveReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if l.Class() != ClassInteractive {
		t.Fatalf("ctx-bound principal must grant interactive, got %v", l.Class())
	}
	l.Release()
	if got := d.Snapshot().ClassDowngrades; got != 0 {
		t.Fatalf("regular interactive must not increment class_downgrades: %d", got)
	}
	if h.contains("downgraded to background") {
		t.Fatalf("regular interactive must not log a downgrade")
	}
}

// TestCtxPrincipalFeedsDeckelDimensions pins that the ctx-derived principal
// (not some zero value) is what the Deckel-Staffel and fairness bucket see:
// the per-principal cap keys on the ctx ApiKeyID.
func TestCtxPrincipalFeedsDeckelDimensions(t *testing.T) {
	s := DefaultSettings()
	s.InteractiveQueuePerPrincipal = 1
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	occ, _, err := d.Acquire(ictx(principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	flooder := principal("flood")
	startWaiter(withPrincipal(ctx, flooder), d, "f1", interactiveReq(), ch)
	waitFor(t, "first flood waiter queued", func() bool { return waitingInteractive(d) == 1 })
	if _, _, err := d.Acquire(withPrincipal(ctx, flooder), interactiveReq()); err == nil || !IsRejection(err) {
		t.Fatalf("per-principal cap must key on the ctx principal, got err=%v", err)
	}
	// A different ctx principal is a different Deckel dimension.
	startWaiter(withPrincipal(ctx, principal("other")), d, "o1", interactiveReq(), ch)
	waitFor(t, "foreign ctx principal still admitted to the queue", func() bool { return waitingInteractive(d) == 2 })
}
