package config

import (
	"strconv"
	"testing"

	"github.com/GottZ/ctx/internal/guard"
)

// guardKey asserts the registry shape of one guard.* key (#381): env name,
// default, hot, global-only. String-keyed, so the probe runs against a
// registry that does not know the key and fails there instead of not
// compiling.
func guardKey(t *testing.T, key, env, def string) {
	t.Helper()
	e, ok := entryByKey()[key]
	if !ok {
		t.Fatalf("%s is not a registry key", key)
	}
	if e.EnvVar != env {
		t.Errorf("%s env = %q, want %q", key, e.EnvVar, env)
	}
	if e.defRaw != def {
		t.Errorf("%s default = %q, want %q", key, e.defRaw, def)
	}
	if e.Mut != "hot" {
		t.Errorf("%s mut = %q, want hot", key, e.Mut)
	}
	if e.Tenancy != TenancyGlobalOnly {
		t.Errorf("%s tenancy = %q, want %q", key, e.Tenancy, TenancyGlobalOnly)
	}
}

// TestGuardSearchKeys (#381): the guard batch's search window and exact
// limit are hot, global-only, strict keys (defaults 800 and 400) that reach
// the batch through GuardSearch. V66 keeps the window within
// [guard.EfSearchMin, guard.EfSearchMax] — pgvector's default as the floor
// (the window before the key existed), pgvector's maximum as the ceiling (a
// larger value fails the set_config of every batch); a negative value of
// either key is V35's report alone.
func TestGuardSearchKeys(t *testing.T) {
	const key = "guard.ef_search"
	guardKey(t, key, "CTX_GUARD_EF_SEARCH", "800")
	guardKey(t, "guard.exact_max_candidates", "CTX_GUARD_EXACT_MAX_CANDIDATES", "400")
	if got, want := Defaults().GuardSearch(), (guard.Search{EfSearch: 800, ExactMaxCandidates: 400}); got != want {
		t.Errorf("Defaults().GuardSearch() = %+v, want %+v", got, want)
	}

	c := validCfg(t, map[string]string{key: "400", "guard.exact_max_candidates": "0"})
	if got, want := c.GuardSearch(), (guard.Search{EfSearch: 400, ExactMaxCandidates: 0}); got != want {
		t.Errorf("GuardSearch() = %+v, want %+v", got, want)
	}
	c, _ = cfgFrom(t, map[string]string{"guard.exact_max_candidates": "-1"})
	if got := severityFor(Validate(c), "guard.exact_max_candidates"); got != SeverityError {
		t.Errorf("guard.exact_max_candidates=-1: severity %d, want error (V35)", got)
	}

	for _, tc := range []struct {
		val  int
		want Severity
	}{
		{0, SeverityError},
		{guard.EfSearchMin - 1, SeverityError},
		{guard.EfSearchMin, -1},
		{800, -1},
		{guard.EfSearchMax, -1},
		{guard.EfSearchMax + 1, SeverityError},
		{-1, SeverityError}, // V35
	} {
		c, _ := cfgFrom(t, map[string]string{key: strconv.Itoa(tc.val)})
		issues := Validate(c)
		if got := severityFor(issues, key); got != tc.want {
			t.Errorf("%s=%d: severity %d, want %d (%v)", key, tc.val, got, tc.want, issues)
		}
		n := 0
		for _, is := range issues {
			if is.Field == key {
				n++
			}
		}
		if tc.want == SeverityError && n != 1 {
			t.Errorf("%s=%d: %d reports, want exactly 1 (V66 and V35 must not both fire)", key, tc.val, n)
		}
	}
}

// TestWritesBatchMaxItemsCeiling (#483): V67 keeps writes.batch_max_items at
// most WritesBatchMaxItemsCeiling; 0 (batches off) and the default pass, a
// negative value is V35's report alone.
func TestWritesBatchMaxItemsCeiling(t *testing.T) {
	const key = "writes.batch_max_items"
	for _, tc := range []struct {
		val  int
		want Severity
	}{
		{0, -1},
		{100, -1},
		{WritesBatchMaxItemsCeiling, -1},
		{WritesBatchMaxItemsCeiling + 1, SeverityError},
		{5000, SeverityError},
		{-1, SeverityError}, // V35
	} {
		c, _ := cfgFrom(t, map[string]string{key: strconv.Itoa(tc.val)})
		issues := Validate(c)
		if got := severityFor(issues, key); got != tc.want {
			t.Errorf("%s=%d: severity %d, want %d (%v)", key, tc.val, got, tc.want, issues)
		}
		n := 0
		for _, is := range issues {
			if is.Field == key {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s=%d: %d issues, want at most one (%v)", key, tc.val, n, issues)
		}
	}
}

// TestWriteLogRetentionFloor (#277): V69 keeps scheduler.writelog_retention_days
// at 0 (keep every row, the default) or between WriteLogRetentionFloorDays and
// WriteLogRetentionCeilingDays —
// the daily synthesis report reads the last 24 hours of decisions from the
// write log. A negative value is V35's report alone.
func TestWriteLogRetentionFloor(t *testing.T) {
	const key = "scheduler.writelog_retention_days"
	for _, tc := range []struct {
		val  int
		want Severity
	}{
		{0, -1},
		{1, SeverityError},
		{WriteLogRetentionFloorDays - 1, SeverityError},
		{WriteLogRetentionFloorDays, -1},
		{30, -1},
		{WriteLogRetentionCeilingDays, -1},
		{WriteLogRetentionCeilingDays + 1, SeverityError},
		{106752, SeverityError}, // past time.Duration's range in days
		{-1, SeverityError},     // V35
	} {
		c, _ := cfgFrom(t, map[string]string{key: strconv.Itoa(tc.val)})
		issues := Validate(c)
		if got := severityFor(issues, key); got != tc.want {
			t.Errorf("%s=%d: severity %d, want %d (%v)", key, tc.val, got, tc.want, issues)
		}
		n := 0
		for _, is := range issues {
			if is.Field == key {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s=%d: %d issues, want at most one (%v)", key, tc.val, n, issues)
		}
	}
	c, _ := cfgFrom(t, map[string]string{})
	if c.Scheduler.WriteLogRetentionDays != 0 {
		t.Errorf("default %s = %d, want 0 (keep every row: an upgrade deletes nothing)", key, c.Scheduler.WriteLogRetentionDays)
	}
}
