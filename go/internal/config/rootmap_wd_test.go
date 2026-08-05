// Wave W-D (Cluster-Topic-Map, design/02 §4.8 + §7 "W-D"): the root_map.*
// namespace is declared ONCE, completely, and — for the three super_* knobs —
// without a consumer. The C0 precedent one file over is the model, and the
// reason is the same: a namespace that grows wave by wave forces a compose edit
// per wave, and the documented legacy failure of this very repo is a knob the
// container cannot receive (three of five graph_overview.* env vars).
//
//	(i)   TestRootMapRegistryContract     — all 9 keys, key/env/default/mut/tenancy
//	(ii)  TestRootMapComposeDeclaresEvery — every key reaches the container
//	(iii) TestRootMapDefaults             — the shipped generation is OFF and sane
package config

import (
	"strings"
	"testing"
)

// rootMapKeys is the §4.8 contract, written out. A literal table on purpose
// (selectorKeys / clusterKeys precedent): a typo'd env var or a silently
// changed default must fail HERE, against the design doc, instead of being
// re-derived from the code it is supposed to guard.
var rootMapKeys = []struct {
	key, env, def, mut, tenancy string
	strict                      bool
}{
	{"root_map.enabled", "CTX_ROOT_MAP_ENABLED", "false", "hot", TenancyGlobalOnly, false},
	{"root_map.budget_bytes", "CTX_ROOT_MAP_BUDGET_BYTES", "15360", "hot", TenancyGlobalOnly, true},
	{"root_map.small_cluster_max", "CTX_ROOT_MAP_SMALL_CLUSTER_MAX", "2", "hot", TenancyGlobalOnly, false},
	{"root_map.footer_reserve_bytes", "CTX_ROOT_MAP_FOOTER_RESERVE_BYTES", "512", "hot", TenancyGlobalOnly, false},
	{"root_map.count_timeout", "CTX_ROOT_MAP_COUNT_TIMEOUT", "5", "hot", TenancyGlobalOnly, false},
	{"root_map.label_budget", "CTX_ROOT_MAP_LABEL_BUDGET", "0", "hot", TenancyGlobalOnly, false},
	{"root_map.super_enabled", "CTX_ROOT_MAP_SUPER_ENABLED", "false", "hot", TenancyGlobalOnly, false},
	{"root_map.super_min_resolution", "CTX_ROOT_MAP_SUPER_MIN_RESOLUTION", "0.2", "hot", TenancyGlobalOnly, false},
	{"root_map.super_max_nodes", "CTX_ROOT_MAP_SUPER_MAX_NODES", "20000", "hot", TenancyGlobalOnly, true},
}

// TestRootMapRegistryContract is gate (i). The second half — nothing outside
// the table carries the prefix — is what pins "W-D owns the namespace": a later
// wave sneaking a tenth key in fails here, not in review.
func TestRootMapRegistryContract(t *testing.T) {
	byKey := map[string]entry{}
	for _, e := range registry() {
		byKey[e.Key] = e
	}
	for _, want := range rootMapKeys {
		e, ok := byKey[want.key]
		if !ok {
			t.Errorf("%s: key missing from the registry", want.key)
			continue
		}
		if e.EnvVar != want.env {
			t.Errorf("%s: env = %q, want %q", want.key, e.EnvVar, want.env)
		}
		if e.defRaw != want.def {
			t.Errorf("%s: default = %q, want %q", want.key, e.defRaw, want.def)
		}
		if e.Mut != want.mut {
			t.Errorf("%s: mut = %q, want %q (every map knob is a runtime flip)", want.key, e.Mut, want.mut)
		}
		if e.Tenancy != want.tenancy {
			t.Errorf("%s: tenancy = %q, want %q (the map is ONE background job over a shared artefact)",
				want.key, e.Tenancy, want.tenancy)
		}
		if e.Strict != want.strict {
			t.Errorf("%s: parse-strict = %v, want %v (size ceilings on a persisted artefact abort the boot)",
				want.key, e.Strict, want.strict)
		}
		if !IsGlobalOnly(want.key) {
			t.Errorf("%s: IsGlobalOnly = false — a tenant could re-tune a process-wide job", want.key)
		}
	}

	declared := map[string]bool{}
	for _, want := range rootMapKeys {
		declared[want.key] = true
	}
	for _, e := range registry() {
		if strings.HasPrefix(e.Key, "root_map.") && !declared[e.Key] {
			t.Errorf("%s: root_map.* key outside the W-D table (K6: one declaring wave)", e.Key)
		}
	}
	if got := len(rootMapKeys); got != 9 {
		t.Errorf("root_map.* contract has %d keys, expected 9 (change it with intent)", got)
	}
}

// TestRootMapComposeDeclaresEveryKey is gate (ii): a knob the container cannot
// receive is not a knob. Deliberately restricted to root_map.*: max_nodes and
// rebuild_timeout are the documented legacy gap of the graph_overview.* block
// and belong to the axis-01 compose sweep (K14) — claiming them here would
// collide with a parallel strand for no gain.
func TestRootMapComposeDeclaresEveryKey(t *testing.T) {
	declared := ctxServiceEnvNames(t)
	for _, want := range rootMapKeys {
		if !declared[want.env] {
			t.Errorf("%s: %s missing from the ctx service environment: block — the key is unreachable through compose",
				want.key, want.env)
		}
	}
}

// TestRootMapDefaults is gate (iii): the shipped generation writes no map, and
// the two numbers that would silently break the artefact if misread — the byte
// budget and the bare-seconds duration — are what they claim to be.
func TestRootMapDefaults(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	if len(issues) != 0 {
		t.Fatalf("default generation: unexpected issues %v", issues)
	}
	if c.RootMap.Enabled {
		t.Error("root_map.enabled = true, want false — W-D ships as a no-op deploy")
	}
	if c.RootMap.SuperEnabled {
		t.Error("root_map.super_enabled = true, want false — its consumer (W-F) does not exist yet")
	}
	if got := c.RootMap.BudgetBytes; got != 15360 {
		t.Errorf("root_map.budget_bytes = %d, want 15360", got)
	}
	if got := c.RootMap.BudgetBytes; got > 50*1024 {
		t.Errorf("root_map.budget_bytes = %d exceeds the 50 KB store write cap", got)
	}
	if got := c.RootMap.FooterReserveBytes; got >= c.RootMap.BudgetBytes {
		t.Errorf("root_map.footer_reserve_bytes = %d >= budget %d — no room for a single topic line",
			got, c.RootMap.BudgetBytes)
	}
	// Bare-seconds convention (parseDurationSeconds): reading 5 as nanoseconds
	// would turn the coverage cap into an instant, always-timing-out count.
	if got := c.RootMap.CountTimeout; got.Seconds() != 5 {
		t.Errorf("root_map.count_timeout = %v, want 5s (bare seconds)", got)
	}
	if got := c.RootMap.LabelBudget; got != 0 {
		t.Errorf("root_map.label_budget = %d, want 0 (= the rendered row budget)", got)
	}
}
