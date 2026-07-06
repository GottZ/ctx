package events

// MW9 unit probes for the Q-I3 tx guard (design/03 §4.4 / D3-W5), DB-free:
// the pre-acquired lease is handed through exactly once on its origin, every
// other acquire under the guard goes through the non-blocking door, and
// acquireBackfillLease targets the first ATTEMPTABLE chain link (model-less
// links never earn the pre-lease). Real leases come from a pass-through
// dispatcher — dispatch.Lease is deliberately not constructible from here.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedcache"
)

// methodRecordingAdmitter wraps a real dispatcher and records WHICH door
// each admission went through (the guard semantics under test). scripted
// TryAcquire errors simulate a busy target without policy setup.
type methodRecordingAdmitter struct {
	d          *dispatch.Dispatcher
	mu         sync.Mutex
	acquires   []string // origins seen by the blocking door
	tries      []string // origins seen by the non-blocking door
	tryReject  error
}

func newMethodRecordingAdmitter(t *testing.T) *methodRecordingAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &methodRecordingAdmitter{d: d}
}

func (m *methodRecordingAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	m.mu.Lock()
	m.acquires = append(m.acquires, req.Target.Origin)
	m.mu.Unlock()
	return m.d.Acquire(ctx, req)
}

func (m *methodRecordingAdmitter) TryAcquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	m.mu.Lock()
	m.tries = append(m.tries, req.Target.Origin)
	rej := m.tryReject
	m.mu.Unlock()
	if rej != nil {
		return nil, nil, rej
	}
	return m.d.TryAcquire(ctx, req)
}

func modelBackend(name, host string) backends.Backend {
	return backends.Backend{
		ID: name, Name: name, Host: host,
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "test-embed"}},
	}
}

func modellessBackend(name, host string) backends.Backend {
	return backends.Backend{ID: name, Name: name, Host: host}
}

// TestAcquireBackfillLeaseTargetsFirstAttemptableLink pins the pre-acquire
// target choice: the first link CARRYING a model earns the blocking
// pre-acquire (mirroring EmbedChain's first actual wire attempt); model-less
// links ahead of it never see an admission door.
func TestAcquireBackfillLeaseTargetsFirstAttemptableLink(t *testing.T) {
	rec := newMethodRecordingAdmitter(t)
	adm := embedcache.Admission{Admitter: rec, Class: dispatch.ClassBackground}
	chain := []backends.Backend{
		modellessBackend("no-model", "http://skipped:1"),
		modelBackend("first-real", "http://first:2"),
	}

	guarded, release, err := acquireBackfillLease(context.Background(), adm, chain, "embed")
	if err != nil {
		t.Fatalf("acquireBackfillLease: %v", err)
	}
	defer release()

	if got := rec.acquires; len(got) != 1 || got[0] != "http://first:2" {
		t.Fatalf("blocking pre-acquires = %v, want exactly [http://first:2]", got)
	}
	if len(rec.tries) != 0 {
		t.Fatalf("non-blocking door hit during pre-acquire: %v", rec.tries)
	}
	if guarded.Class != dispatch.ClassBackground {
		t.Fatalf("guarded class = %v, want background (carried through)", guarded.Class)
	}
}

// TestTxGuardHandsPreLeaseThroughExactlyOnce pins the hand-through contract:
// the first acquire on the pre-leased origin returns the HELD lease without
// touching the dispatcher again; a second acquire on the same origin (and
// any other origin) goes through TryAcquire.
func TestTxGuardHandsPreLeaseThroughExactlyOnce(t *testing.T) {
	rec := newMethodRecordingAdmitter(t)
	adm := embedcache.Admission{Admitter: rec, Class: dispatch.ClassBackground}
	chain := []backends.Backend{modelBackend("a", "http://target-a:1"), modelBackend("b", "http://target-b:2")}

	guarded, release, err := acquireBackfillLease(context.Background(), adm, chain, "embed")
	if err != nil {
		t.Fatalf("acquireBackfillLease: %v", err)
	}
	defer release()

	req := func(origin string) dispatch.Request {
		return dispatch.Request{Target: dispatch.Target{Origin: origin}, Class: dispatch.ClassBackground, Role: "embed"}
	}

	// 1st acquire on the pre-leased origin: hand-through, no dispatcher call.
	l1, rc1, err := guarded.Admitter.Acquire(context.Background(), req("http://target-a:1"))
	if err != nil || l1 == nil || rc1 == nil {
		t.Fatalf("hand-through acquire = (%v, %v, %v), want held lease", l1, rc1, err)
	}
	if len(rec.tries) != 0 {
		t.Fatalf("hand-through went through TryAcquire: %v", rec.tries)
	}
	l1.Release()

	// 2nd acquire, same origin: the pre-lease is spent → non-blocking door.
	l2, _, err := guarded.Admitter.Acquire(context.Background(), req("http://target-a:1"))
	if err != nil || l2 == nil {
		t.Fatalf("post-hand-through acquire = (%v, %v), want pass-through lease", l2, err)
	}
	l2.Release()
	if got := rec.tries; len(got) != 1 || got[0] != "http://target-a:1" {
		t.Fatalf("non-blocking door saw %v, want [http://target-a:1]", got)
	}
	if got := rec.acquires; len(got) != 1 {
		t.Fatalf("blocking door called %d times, want 1 (pre-acquire only — never under tx)", len(got))
	}
}

// TestTxGuardMismatchedOriginUsesTryAcquire pins the mechanical rule for a
// pick whose chain starts elsewhere than the peeked block's: the unused
// pre-lease stays with the caller's release, the foreign origin answers via
// TryAcquire — including its busy answer (ErrWouldBlock passes through
// verbatim so the backfill's defer detection sees the sentinel).
func TestTxGuardMismatchedOriginUsesTryAcquire(t *testing.T) {
	rec := newMethodRecordingAdmitter(t)
	adm := embedcache.Admission{Admitter: rec, Class: dispatch.ClassBackground}
	chain := []backends.Backend{modelBackend("a", "http://target-a:1")}

	guarded, release, err := acquireBackfillLease(context.Background(), adm, chain, "embed")
	if err != nil {
		t.Fatalf("acquireBackfillLease: %v", err)
	}
	defer release()

	rec.mu.Lock()
	rec.tryReject = dispatch.ErrWouldBlock
	rec.mu.Unlock()

	_, _, err = guarded.Admitter.Acquire(context.Background(),
		dispatch.Request{Target: dispatch.Target{Origin: "http://elsewhere:9"}, Class: dispatch.ClassBackground, Role: "embed"})
	if !errors.Is(err, dispatch.ErrWouldBlock) {
		t.Fatalf("mismatched-origin acquire = %v, want ErrWouldBlock via TryAcquire", err)
	}
	if got := rec.tries; len(got) != 1 || got[0] != "http://elsewhere:9" {
		t.Fatalf("non-blocking door saw %v, want [http://elsewhere:9]", got)
	}
}

// TestAcquireBackfillLeaseFailsLoud pins the two loud-failure doctrines: a
// zero Admission (I-D1) and an admitter WITHOUT a non-blocking door (Q-I3
// unenforceable) both error before any tx could open — never a silent
// blocking fallback.
func TestAcquireBackfillLeaseFailsLoud(t *testing.T) {
	chain := []backends.Backend{modelBackend("a", "http://target-a:1")}

	if _, _, err := acquireBackfillLease(context.Background(),
		embedcache.Admission{}, chain, "embed"); err == nil {
		t.Fatal("zero admission must fail loud (I-D1)")
	}

	blockingOnly := struct{ dispatch.Admitter }{}
	if _, _, err := acquireBackfillLease(context.Background(),
		embedcache.Admission{Admitter: blockingOnly, Class: dispatch.ClassBackground}, chain, "embed"); err == nil {
		t.Fatal("blocking-only admitter must fail loud — Q-I3 cannot be enforced")
	}
}

// TestAcquireBackfillLeaseNoAttemptableLink pins the acquire-free walk: a
// chain without any model-carrying link yields a guard WITHOUT pre-lease and
// touches no admission door up front.
func TestAcquireBackfillLeaseNoAttemptableLink(t *testing.T) {
	rec := newMethodRecordingAdmitter(t)
	adm := embedcache.Admission{Admitter: rec, Class: dispatch.ClassBackground}
	chain := []backends.Backend{modellessBackend("x", "http://x:1")}

	guarded, release, err := acquireBackfillLease(context.Background(), adm, chain, "embed")
	if err != nil {
		t.Fatalf("acquireBackfillLease: %v", err)
	}
	defer release()
	if len(rec.acquires) != 0 || len(rec.tries) != 0 {
		t.Fatalf("admission doors touched (%v / %v), want none", rec.acquires, rec.tries)
	}
	if guarded.Admitter == nil {
		t.Fatal("guard admission missing")
	}
}
