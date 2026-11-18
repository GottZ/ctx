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
