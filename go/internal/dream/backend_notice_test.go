package dream

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// syncBuf is a log sink for the notice tests.
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

// The lines of dream.BackendNotice, as the text handler writes them.
const (
	noticeWarnShared   = `level=WARN msg="dream: no backend of role dream —`
	noticeWarnDisabled = `level=WARN msg="dream: every backend of role dream is disabled`
	noticeWarnOwn      = `level=WARN msg="dream: no backend of role dream of this tenant serves`
	noticeInfoShared   = `level=INFO msg="dream: a backend of role dream serves again`
	noticeInfoOwn      = `level=INFO msg="dream: a backend of role dream of this tenant serves again`
)

// captureNotice routes slog into a buffer at DEBUG for one test.
func captureNotice(t *testing.T) (*syncBuf, func(sub string) int) {
	t.Helper()
	logs := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs, func(sub string) int { return strings.Count(logs.String(), sub) }
}

// TestBackendNoticeReportsEveryOutageOfATenantsOwnBackend (issue #495, review
// of the linked PR): a deployment whose shared view carries no dream row on
// purpose; tenant c dreams on a row of its own. Its row goes down, comes back,
// goes down again. Each change is a line — WARN, INFO, WARN — and the WARN
// names the tenant and `ctx backends`, not the seed: nothing is missing from
// the shared pool. With the state kept for the shared view alone, the first
// outage gave the seed hint, the recovery no INFO, and the second outage no
// line above DEBUG.
func TestBackendNoticeReportsEveryOutageOfATenantsOwnBackend(t *testing.T) {
	logs, count := captureNotice(t)
	up := tenantDreamRow("c-dream", "c", 100)
	down := up
	down.Enabled = false
	p := backends.NewPool(nil, nil)
	c := &Router{Pool: p, Tenant: "c"}
	var n BackendNotice

	for i, step := range []struct {
		row  backends.Backend
		skip bool
	}{{up, false}, {down, true}, {down, true}, {up, false}, {down, true}, {down, true}} {
		p.SeedSnapshotForTest([]backends.Backend{step.row})
		if got := n.Skip(c); got != step.skip {
			t.Fatalf("step %d: Skip = %v, want %v", i, got, step.skip)
		}
	}
	if w, i := count(noticeWarnOwn), count(noticeInfoOwn); w != 2 || i != 1 {
		t.Errorf("outage, recovery, outage: %d WARN and %d INFO for the tenant, want 2 and 1:\n%s", w, i, logs.String())
	}
	if count("ctx backends seed") != 0 || count(noticeWarnShared)+count(noticeInfoShared) != 0 {
		t.Errorf("a tenant's own backend went down; the lines speak of the shared pool:\n%s", logs.String())
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && (!strings.Contains(line, "tenant_scope=c") || !strings.Contains(line, "`ctx backends`")) {
			t.Errorf("the tenant's WARN must name it and `ctx backends`: %s", line)
		}
	}
}

// TestBackendNoticeTenantsWithoutABackendShareOneLine: tenants without a
// dream row of their own skip for one reason, the empty shared view. In the
// round robin of five turns over three of them (the default tenant among
// them) and a fourth that dreams on its own row, the WARN — the seed hint —
// comes once and the INFO once, when the last of them served again. A shared
// row that only exists disabled gets the WARN that names it instead.
func TestBackendNoticeTenantsWithoutABackendShareOneLine(t *testing.T) {
	logs, count := captureNotice(t)
	acme := tenantDreamRow("acme-dream", "acme", 100)
	shared := tenantDreamRow("shared-dream", backends.GlobalScope, 100)
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{acme})
	without := []*Router{{Pool: p, Tenant: backends.GlobalScope}, {Pool: p, Tenant: "alpha"}, {Pool: p, Tenant: "beta"}}
	withOwn := &Router{Pool: p, Tenant: "acme"}
	var n BackendNotice

	for turn := 0; turn < 5; turn++ {
		for _, r := range without {
			if !n.Skip(r) {
				t.Fatalf("turn %d: %s did not skip without a backend", turn, r.Tenant)
			}
		}
		if n.Skip(withOwn) {
			t.Fatalf("turn %d: acme skipped, but its own row serves", turn)
		}
	}
	if w, i, d := count(noticeWarnShared), count(noticeInfoShared), count("level=DEBUG"); w != 1 || i != 0 || d != 14 {
		t.Fatalf("5 turns over 3 tenants without a backend: %d WARN, %d INFO, %d DEBUG, want 1, 0, 14:\n%s", w, i, d, logs.String())
	}
	if count("ctx backends seed") != 1 || count("tenant_scope=") != 14 {
		t.Errorf("the shared WARN gives the seed hint and names no tenant:\n%s", logs.String())
	}

	p.SeedSnapshotForTest([]backends.Backend{shared, acme})
	for i, r := range without {
		if n.Skip(r) {
			t.Fatalf("%s skipped with a shared dream row", r.Tenant)
		}
		if want := 0; i < len(without)-1 && count(noticeInfoShared) != want {
			t.Errorf("INFO after %d of %d tenants served — the group is not empty yet", i+1, len(without))
		}
	}
	if i := count(noticeInfoShared); i != 1 {
		t.Errorf("the shared row serves all of them: %d INFO, want 1", i)
	}

	disabled := shared
	disabled.Enabled = false
	p.SeedSnapshotForTest([]backends.Backend{disabled, acme})
	for turn := 0; turn < 3; turn++ {
		for _, r := range without {
			n.Skip(r)
		}
	}
	if w, d := count(noticeWarnShared), count(noticeWarnDisabled); w != 1 || d != 1 {
		t.Errorf("shared row disabled: %d seed-WARN, %d disabled-WARN, want 1 and 1", w, d)
	}
	// The shared rows are the default tenant's view, not rows of its own.
	if o := count(noticeWarnOwn); o != 0 {
		t.Errorf("%d tenant WARN(s) — a shared row counted as a tenant's own", o)
	}
	if c := count("backends: empty chain"); c != 0 {
		t.Errorf("%d \"backends: empty chain\" line(s) — the notice asked Chain", c)
	}
}

// TestBackendNoticeForgetsTenantsNoLongerListed: the set holds the tenants
// whose cycles skip, and Retain drops those that left the background tenant
// list. A deleted tenant therefore neither stays in it (memory) nor keeps
// the group of tenants without a backend from being empty: the next tenant
// that skips for the shared reason is a change of state and gets the WARN.
func TestBackendNoticeForgetsTenantsNoLongerListed(t *testing.T) {
	_, count := captureNotice(t)
	down := tenantDreamRow("z-dream", "z", 100)
	down.Enabled = false
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{down})
	var n BackendNotice

	n.Skip(&Router{Pool: p, Tenant: "x"}) // shared reason
	n.Skip(&Router{Pool: p, Tenant: "z"}) // its own row is down
	n.Retain([]string{"y", backends.GlobalScope})
	if len(n.skipped) != 0 || n.shared != 0 {
		t.Fatalf("after Retain without x and z: %d entries, %d in the shared group, want none", len(n.skipped), n.shared)
	}
	n.Skip(&Router{Pool: p, Tenant: "y"})
	if w := count(noticeWarnShared); w != 2 {
		t.Errorf("%d shared WARN, want 2 — y skipping after x was forgotten is a change of state", w)
	}
	if i := count(noticeInfoShared) + count(noticeInfoOwn); i != 0 {
		t.Errorf("forgetting a tenant logged %d INFO — no backend served", i)
	}
}

// TestBackendNoticeConcurrentWorkers: sixteen workers share one notice, as
// the dream workers of a scheduler do. Per phase every worker calls Skip 500
// times over three tenants without a backend and one with its own row; each
// change of state is one line, whichever worker sees it first.
func TestBackendNoticeConcurrentWorkers(t *testing.T) {
	_, count := captureNotice(t)
	own := tenantDreamRow("c-dream", "c", 100)
	ownDown := own
	ownDown.Enabled = false
	shared := tenantDreamRow("shared-dream", backends.GlobalScope, 100)
	p := backends.NewPool(nil, nil)
	tenants := []string{backends.GlobalScope, "alpha", "beta", "c"}
	var n BackendNotice
	run := func(wantSkip bool) {
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					r := &Router{Pool: p, Tenant: tenants[(g+i)%len(tenants)]}
					if n.Skip(r) != wantSkip {
						t.Errorf("tenant %s: Skip != %v", r.Tenant, wantSkip)
						return
					}
				}
			}(g)
		}
		wg.Wait()
	}
	for round := 1; round <= 5; round++ {
		p.SeedSnapshotForTest([]backends.Backend{ownDown})
		run(true)
		if s, o := count(noticeWarnShared), count(noticeWarnOwn); s != round || o != round {
			t.Fatalf("round %d down: %d shared WARN, %d own WARN, want %d each", round, s, o, round)
		}
		p.SeedSnapshotForTest([]backends.Backend{shared, own})
		run(false)
		if s, o := count(noticeInfoShared), count(noticeInfoOwn); s != round || o != round {
			t.Fatalf("round %d up: %d shared INFO, %d own INFO, want %d each", round, s, o, round)
		}
	}
}
