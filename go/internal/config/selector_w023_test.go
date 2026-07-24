// W02-3 gates G3/G4/G5 on the config side (Evokoa-Clean-Room
// design/02-strategy-selektor.md §3.4, §7 "W02-3"): the SelectorConfig group
// is pinned key-by-key (key/env/default/mut/tenancy), the mirror SelectorRRF()
// carries the group into the rrf policy struct, the default generation is OFF
// (the shipped state), and a settings flip of retrieval.selector.enabled is
// live from the next snapshot on — no restart.
package config

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/rrf"
)

// selectorKeys is the §3.4 contract, written out. It is deliberately a literal
// table and not derived from the struct: a typo'd env var or a silently changed
// default must fail HERE, against the design doc, not be re-derived from the
// code it is supposed to guard.
var selectorKeys = []struct {
	key, env, def, mut, tenancy string
}{
	{"retrieval.selector.enabled", "CTX_RETRIEVAL_SELECTOR_ENABLED", "false", "hot", TenancyGlobalOnly},
	{"retrieval.selector.exact_max", "CTX_RETRIEVAL_SELECTOR_EXACT_MAX", "4096", "hot", TenancyGlobalOnly},
	{"retrieval.selector.grey_max", "CTX_RETRIEVAL_SELECTOR_GREY_MAX", "65536", "hot", TenancyGlobalOnly},
	{"retrieval.selector.grey_scan_tuples", "CTX_RETRIEVAL_SELECTOR_GREY_SCAN_TUPLES", "60000", "hot", TenancyGlobalOnly},
	{"retrieval.selector.stats_ttl", "CTX_RETRIEVAL_SELECTOR_STATS_TTL", "60", "hot", TenancyGlobalOnly},
}

// TestSelectorRegistryContract (G5 companion): every one of the five keys is
// registered exactly as §3.4 specifies — and global-only. TestRegistryTenancySet
// pins the complement (the 55-key tenant-overridable allowlist stays untouched);
// this test pins the positive side per key, so a later "just make enabled
// tenant-tunable" edit trips two independent tests.
func TestSelectorRegistryContract(t *testing.T) {
	byKey := map[string]entry{}
	for _, e := range registry() {
		byKey[e.Key] = e
	}
	for _, want := range selectorKeys {
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
			t.Errorf("%s: mut = %q, want %q (a settings flip must not need a restart)", want.key, e.Mut, want.mut)
		}
		if e.Tenancy != want.tenancy {
			t.Errorf("%s: tenancy = %q, want %q (buffer touch of the SHARED database, §3.4)", want.key, e.Tenancy, want.tenancy)
		}
		if !IsGlobalOnly(want.key) {
			t.Errorf("%s: IsGlobalOnly = false — a tenant could override it", want.key)
		}
	}
}

// TestSelectorRRFDefaultsAreOff (G4): the SHIPPED generation is off. The
// mirror hands rrf a policy with Enabled=false, which is rrf's Ist path
// (Decision{ann, disabled}, no probe roundtrip — pinned on the rrf side by
// TestSelectorG1_ZeroPolicyIsIstPath and TestSelectorDisabledWithThresholds).
// The thresholds ride along at their §3.4 defaults so that arming the master
// gate is a pure data flip, not a re-tuning exercise.
func TestSelectorRRFDefaultsAreOff(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	if len(issues) != 0 {
		t.Fatalf("default generation: unexpected issues %v", issues)
	}
	got := c.SelectorRRF()
	want := rrf.SelectorPolicy{
		Enabled:        false,
		ExactMax:       4096,
		GreyMax:        65536,
		GreyScanTuples: 60000,
		StatsTTL:       60 * time.Second,
	}
	if got != want {
		t.Errorf("SelectorRRF() = %+v, want %+v", got, want)
	}
	if c.Source("retrieval.selector.enabled") != "default" {
		t.Errorf("enabled source = %q, want \"default\"", c.Source("retrieval.selector.enabled"))
	}
}

// TestSelectorStatsTTLIsBareSeconds pins the house duration convention
// (§3.4 Konventions-Hinweis, parseDurationSeconds): the raw value is a bare
// integer of SECONDS — "60s" is not a legal value here, and reading 60 as
// nanoseconds would silently disarm the pg_stats cache in W02-4.
func TestSelectorStatsTTLIsBareSeconds(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{"retrieval.selector.stats_ttl": "300"})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues %v", issues)
	}
	if got := c.Selector.StatsTTL; got != 300*time.Second {
		t.Errorf("stats_ttl = %v, want 5m0s (bare seconds)", got)
	}
}

// TestSelectorHotFlipViaStore (G3): the master gate flips at RUNTIME.
// false -> true -> false through Store.Replace (the F2 settings-write path);
// every snapshot taken afterwards carries the new policy, the previously
// published generation stays intact, and no restart is involved. Same
// copy-on-write idiom as TestStoreCopyOnWrite.
func TestSelectorHotFlipViaStore(t *testing.T) {
	base, issues := cfgFrom(t, map[string]string{})
	if len(issues) != 0 {
		t.Fatalf("base generation: unexpected issues %v", issues)
	}
	store := NewStore(base)

	if store.SnapshotForRequest(t.Context()).SelectorRRF().Enabled {
		t.Fatal("shipped generation must be OFF")
	}

	on := *store.Snapshot()
	on.Selector.Enabled = true
	if err := store.Replace(&on); err != nil {
		t.Fatalf("Replace(on): %v", err)
	}
	if p := store.SnapshotForRequest(t.Context()).SelectorRRF(); !p.Enabled {
		t.Error("after the flip the request snapshot must carry Enabled=true")
	}
	if base.Selector.Enabled {
		t.Error("the previously published generation was mutated")
	}

	off := *store.Snapshot()
	off.Selector.Enabled = false
	if err := store.Replace(&off); err != nil {
		t.Fatalf("Replace(off): %v", err)
	}
	if p := store.SnapshotForRequest(t.Context()).SelectorRRF(); p.Enabled {
		t.Error("the flip back to false must be live too")
	}
}
