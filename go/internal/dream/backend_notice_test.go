package dream

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// syncBuf is a log sink for the notice test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestBackendNoticeOneLinePerChangeOfTheSharedView pins the state the notice
// keeps (issue #495): that of the shared pool. Tenant "acme" dreams on a
// private row, tenant "beta" has no backend of its own and the shared rows
// serve no dream role. The round robin alternates the two: "beta" skips,
// "acme" serves — the WARN comes once, and "acme"'s serving cycles neither
// announce a recovery nor re-arm the WARN for "beta"'s next turn. A shared
// row ends it with one INFO; a shared row that only exists disabled gets the
// WARN that names it instead of the seed hint. The WARN describes the shared
// pool: it names no tenant, and a tenant's own disabled row does not turn the
// seed hint into the disabled one. Nothing asks Chain (no "backends: empty
// chain" line).
func TestBackendNoticeOneLinePerChangeOfTheSharedView(t *testing.T) {
	logs := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	betaDisabled := tenantDreamRow("beta-dream", "beta", 100)
	betaDisabled.Enabled = false
	acmeOnly := []backends.Backend{tenantDreamRow("acme-dream", "acme", 100), betaDisabled}
	shared := tenantDreamRow("shared-dream", backends.GlobalScope, 100)
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest(acmeOnly)
	acme := &Router{Pool: p, Tenant: "acme"}
	beta := &Router{Pool: p, Tenant: "beta"}
	var n BackendNotice

	count := func(sub string) int { return strings.Count(logs.String(), sub) }
	const (
		warnMissing  = `level=WARN msg="dream: no backend of role dream`
		warnDisabled = `level=WARN msg="dream: every backend of role dream is disabled`
		infoServes   = `level=INFO msg="dream: a backend of role dream serves again`
	)

	for i := 0; i < 5; i++ {
		if n.Skip(acme) {
			t.Fatalf("turn %d: acme skipped, but its private row serves", i)
		}
		if !n.Skip(beta) {
			t.Fatalf("turn %d: beta did not skip without a backend", i)
		}
	}
	if w, s := count(warnMissing), count(infoServes); w != 1 || s != 0 {
		t.Fatalf("after 5 turns of the round robin: %d WARN, %d INFO, want 1 and 0 — one line per change of state", w, s)
	}
	// beta's own row exists, disabled: the shared pool still has no row of
	// the role, so the WARN is the seed hint, not the disabled one.
	if !strings.Contains(logs.String(), "ctx backends seed") || count(warnDisabled) != 0 {
		t.Errorf("the WARN must name the shared pool's remedy (ctx backends seed):\n%s", logs.String())
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "tenant_scope=") {
			t.Errorf("the WARN describes the shared pool and names no tenant: %s", line)
		}
	}

	p.SeedSnapshotForTest(append([]backends.Backend{shared}, acmeOnly...))
	for i := 0; i < 3; i++ {
		if n.Skip(beta) || n.Skip(acme) {
			t.Fatalf("turn %d: a skip with a shared dream row", i)
		}
	}
	if s := count(infoServes); s != 1 {
		t.Errorf("a shared row serves: %d INFO, want 1", s)
	}

	disabled := shared
	disabled.Enabled = false
	p.SeedSnapshotForTest([]backends.Backend{disabled})
	for i := 0; i < 3; i++ {
		if !n.Skip(beta) {
			t.Fatalf("turn %d: beta did not skip with the shared row disabled", i)
		}
	}
	if w, d := count(warnMissing), count(warnDisabled); w != 1 || d != 1 {
		t.Errorf("shared row disabled: %d missing-WARN, %d disabled-WARN, want 1 and 1", w, d)
	}
	if c := count("backends: empty chain"); c != 0 {
		t.Errorf("%d \"backends: empty chain\" line(s) — the notice asked Chain", c)
	}
}
