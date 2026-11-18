package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

// k1Entry asserts the registry shape of one key of the K1 group (issues #208,
// #210, #223, #226): env name, default, hot, tenant-overridable. String-keyed
// on purpose, so the probe runs against a registry that does not know the key
// yet and fails there instead of not compiling.
func k1Entry(t *testing.T, key, env, def string) {
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
	if e.Tenancy != TenancyOverridable {
		t.Errorf("%s tenancy = %q, want %q", key, e.Tenancy, TenancyOverridable)
	}
}

// TestLowConfidenceMaxSourcesKey (#226): the low-confidence cut is a hot,
// tenant-overridable key whose default IS the llm constant, it reaches the
// synthesis settings, and V61 refuses the one value that cannot mean a cut.
func TestLowConfidenceMaxSourcesKey(t *testing.T) {
	k1Entry(t, "query.low_confidence_max_sources", "CTX_LOW_CONFIDENCE_MAX_SOURCES", "2")
	if got := defaultFor("query.low_confidence_max_sources"); got != llm.LowConfidenceMaxSources {
		t.Errorf("registry default %v != llm.LowConfidenceMaxSources %d — the key and the zero-value fallback would disagree",
			got, llm.LowConfidenceMaxSources)
	}

	c := validCfg(t, map[string]string{"query.low_confidence_max_sources": "7"})
	if got := c.SynthesisSettings().LowConfidenceMaxSources; got != 7 {
		t.Errorf("SynthesisSettings().LowConfidenceMaxSources = %d, want 7", got)
	}

	for _, tc := range []struct {
		val  string
		want Severity
	}{
		{"0", SeverityError}, // a prompt with no source is no cut, it is no answer
		{"-1", SeverityError},
		{"1", -1},
		{"2", -1},
		{"20", -1}, // at or above the query limit: no cut
		{"500", -1},
	} {
		c, _ := cfgFrom(t, map[string]string{"query.low_confidence_max_sources": tc.val})
		issues := Validate(c)
		if got := severityFor(issues, "query.low_confidence_max_sources"); got != tc.want {
			t.Errorf("query.low_confidence_max_sources=%s: severity %d, want %d (%v)", tc.val, got, tc.want, issues)
		}
	}
}

// TestMaxSourceRunesKey (#208): the per-source cut is a hot,
// tenant-overridable key whose default IS llm.MaxBlockChars, it reaches the
// synthesis settings, and V62 keeps it between today's cut (the floor every
// head-heavy format was sized against) and the share one prompt can carry.
func TestMaxSourceRunesKey(t *testing.T) {
	k1Entry(t, "query.max_source_runes", "CTX_MAX_SOURCE_RUNES", "1500")
	if got := defaultFor("query.max_source_runes"); got != llm.MaxBlockChars {
		t.Errorf("registry default %v != llm.MaxBlockChars %d", got, llm.MaxBlockChars)
	}

	c := validCfg(t, map[string]string{"query.max_source_runes": "6000"})
	if got := c.SynthesisSettings().MaxSourceRunes; got != 6000 {
		t.Errorf("SynthesisSettings().MaxSourceRunes = %d, want 6000", got)
	}

	for _, tc := range []struct {
		val  string
		want Severity
	}{
		{"0", SeverityError},
		{"-1", SeverityError},
		{"1499", SeverityError}, // below the floor head-heavy formats were sized against
		{"1500", -1},
		{"6000", -1},
		{"30000", -1},            // llm.SourceRunesShare: one source may carry the whole share
		{"30001", SeverityError}, // renders as configured, acts as 30000
	} {
		c, _ := cfgFrom(t, map[string]string{"query.max_source_runes": tc.val})
		issues := Validate(c)
		if got := severityFor(issues, "query.max_source_runes"); got != tc.want {
			t.Errorf("query.max_source_runes=%s: severity %d, want %d (%v)", tc.val, got, tc.want, issues)
		}
	}
}
