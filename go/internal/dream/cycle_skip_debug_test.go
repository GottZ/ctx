package dream

// Issue #504: step 0 of RunDreamCycle — the guard for every caller other than
// the scheduler loop, which asks BackendNotice first — skips a cycle without a
// serving dream backend before it touches the database, and says so at DEBUG
// only: the operator hears about a missing backend once per change of state
// from BackendNotice (issue #495), not once per cycle from here.

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

func TestRunDreamCycleSkipsWithoutABackendAtDebug(t *testing.T) {
	disabled := tenantDreamRow("c-dream", "c", 100)
	disabled.Enabled = false
	for _, tc := range []struct {
		name string
		rows []backends.Backend
	}{
		{"no dream row at all", nil},
		{"the tenant's own dream row disabled", []backends.Backend{disabled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, count := captureNotice(t)
			p := backends.NewPool(nil, nil)
			p.SeedSnapshotForTest(tc.rows)
			r := &Router{Pool: p, Tenant: "c"}
			// A nil database pool: a cycle that got past step 0 would panic
			// at its first statement.
			n, err := RunDreamCycle(context.Background(), nil, r, llm.Options{}, BackoffConfig{}, []string{"c"}, NoThrottle)
			if n != 0 || err != nil {
				t.Fatalf("RunDreamCycle = (%d, %v), want (0, nil): the cycle is skipped", n, err)
			}
			const line = `level=DEBUG msg="dream: no eligible dream backend — cycle skipped before pick" tenant_scope=c`
			if got := count(line); got != 1 {
				t.Errorf("step 0 logged %q %d times, want once:\n%s", line, got, logs.String())
			}
			for _, level := range []string{"level=INFO", "level=WARN", "level=ERROR"} {
				if strings.Contains(logs.String(), level) {
					t.Errorf("step 0 logged above DEBUG:\n%s", logs.String())
				}
			}
		})
	}
}
