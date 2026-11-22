package config

import (
	"strings"
	"testing"
)

// TestValidateProjectSyncMaxConcurrent is V68 (issue #491): the forge syncs
// that may run side by side, times the pool connections one of them holds,
// stay below the database pool; a larger count is clamped with a WARN, never
// refused — the key is restart-only and env-only, and a value that booted
// before the rule still boots.
func TestValidateProjectSyncMaxConcurrent(t *testing.T) {
	ceiling := (gatewayDBPoolConns - 1) / forgeSyncConnsPerRun
	for _, tc := range []struct {
		name    string
		n, want int
		warns   bool
	}{
		{"default", Defaults().Project.Sync.MaxConcurrent, Defaults().Project.Sync.MaxConcurrent, false},
		{"the ceiling", ceiling, ceiling, false},
		{"one above the ceiling", ceiling + 1, ceiling, true},
		{"the pool size", gatewayDBPoolConns, ceiling, true},
		{"far above", 500, ceiling, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Project.Sync.MaxConcurrent = tc.n
			mine := issuesOn(Validate(c), "project.sync.max_concurrent")
			if HasErrors(mine) {
				t.Fatalf("Validate with max_concurrent %d refuses it: %+v", tc.n, mine)
			}
			if got := c.Project.Sync.MaxConcurrent; got != tc.want {
				t.Errorf("max_concurrent %d validates to %d, want %d", tc.n, got, tc.want)
			}
			switch {
			case tc.warns && (len(mine) != 1 || mine[0].Severity != SeverityWarn || !strings.Contains(mine[0].Msg, "clamped")):
				t.Errorf("max_concurrent %d: issues %+v, want one WARN that says the value was clamped", tc.n, mine)
			case !tc.warns && len(mine) != 0:
				t.Errorf("max_concurrent %d: issues %+v, want none", tc.n, mine)
			}
			if c.Project.Sync.MaxConcurrent*forgeSyncConnsPerRun >= gatewayDBPoolConns {
				t.Errorf("after Validate %d syncs can hold %d of %d pool connections", c.Project.Sync.MaxConcurrent, c.Project.Sync.MaxConcurrent*forgeSyncConnsPerRun, gatewayDBPoolConns)
			}
		})
	}
}

// TestProjectSyncMaxConcurrentBootsFromTheEnvironment: the boot path (the env
// build plus Validate, what cmd/ctxd aborts on with HasErrors) keeps running
// with a CTX_PROJECT_SYNC_MAX_CONCURRENT above the ceiling and serves the
// clamped value.
func TestProjectSyncMaxConcurrentBootsFromTheEnvironment(t *testing.T) {
	t.Setenv("CTX_PROJECT_SYNC_MAX_CONCURRENT", "25")
	t.Setenv("CONTEXT_DB_PASSWORD", "boot-test")
	c, issues := FromEnv()
	issues = append(issues, Validate(c)...)
	if HasErrors(issues) {
		t.Fatalf("boot with CTX_PROJECT_SYNC_MAX_CONCURRENT=25 stops: %+v", issues)
	}
	if want := (gatewayDBPoolConns - 1) / forgeSyncConnsPerRun; c.Project.Sync.MaxConcurrent != want {
		t.Errorf("the boot serves max_concurrent %d, want the clamp %d", c.Project.Sync.MaxConcurrent, want)
	}
}
