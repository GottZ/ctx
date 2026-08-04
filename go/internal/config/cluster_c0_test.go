// Wave C0 (Cluster-Topic-Map, design/03 §4.9 + §7 "C0"): the cluster.*
// namespace is declared ONCE, completely, and without a consumer. K6 of the
// masterplan makes C0 the ONLY cluster.*-declaring wave — every later wave
// (C2/C3/C4/C6/C7/C8/C9) wires ITS fields and adds no key. These gates are
// what makes that claim checkable:
//
//   (i)  TestClusterRegistryContract    — all 21 keys, key/env/default/mut/tenancy
//   (ii) TestClusterComposeDeclaresEvery — every key reaches the container
//
// Gate (iii) of the wave brief ("/api/query golden byte-identical") needs no
// test of its own: C0 has no consumer, so the whole `-short` suite IS the
// golden — a behaviour change would trip it.
package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// clusterKeys is the §4.9 contract, written out. Deliberately a literal table
// and NOT derived from the struct (same reason as selectorKeys): a typo'd env
// var or a silently changed default must fail HERE, against the design doc,
// not be re-derived from the code it is supposed to guard.
//
// The three decided defaults carry their decision marker: size_damping=true is
// UD-04-03 (DECISIONS.md, 2. Rückgabe — Ziel-Scale-Semantik ab Tag 1),
// enabled=false is the pausability invariant (the boost is armed only after C4
// + eval-A/B), inject_max=0 is the C9 no-op default.
var clusterKeys = []struct {
	key, env, def, mut, tenancy string
}{
	// ClusterConfig — ranking knobs, tenant-overridable (a tenant tuning its
	// own query augmentation touches only its own queries).
	{"cluster.enabled", "CTX_CLUSTER_ENABLED", "false", "hot", TenancyOverridable},
	{"cluster.seed_count", "CTX_CLUSTER_SEED_COUNT", "10", "hot", TenancyOverridable},
	{"cluster.top_clusters", "CTX_CLUSTER_TOP_CLUSTERS", "2", "hot", TenancyOverridable},
	{"cluster.min_share", "CTX_CLUSTER_MIN_SHARE", "0.25", "hot", TenancyOverridable},
	{"cluster.boost_weight", "CTX_CLUSTER_BOOST_WEIGHT", "0.12", "hot", TenancyOverridable},
	{"cluster.size_damping", "CTX_CLUSTER_SIZE_DAMPING", "true", "hot", TenancyOverridable},
	{"cluster.centroid_enabled", "CTX_CLUSTER_CENTROID_ENABLED", "false", "hot", TenancyOverridable},
	{"cluster.centroid_weight", "CTX_CLUSTER_CENTROID_WEIGHT", "0.5", "hot", TenancyOverridable},
	{"cluster.centroid_top_k", "CTX_CLUSTER_CENTROID_TOP_K", "3", "hot", TenancyOverridable},
	{"cluster.inject_max", "CTX_CLUSTER_INJECT_MAX", "0", "hot", TenancyOverridable},

	// ClusterOpsConfig — wire contracts + shared-artefact operation, global-only.
	// A wire contract must not diverge per tenant, and the centroid build is one
	// process-wide background run over a SHARED artefact (analogy: the whole of
	// GraphOverviewConfig is global-only).
	{"cluster.max_staleness", "CTX_CLUSTER_MAX_STALENESS", "86400", "hot", TenancyGlobalOnly},
	{"cluster.ego_annotate", "CTX_CLUSTER_EGO_ANNOTATE", "false", "hot", TenancyGlobalOnly},
	{"cluster.ego_annotate_max_nodes", "CTX_CLUSTER_EGO_ANNOTATE_MAX_NODES", "500", "hot", TenancyGlobalOnly},
	{"cluster.facet_enabled", "CTX_CLUSTER_FACET_ENABLED", "false", "hot", TenancyGlobalOnly},
	{"cluster.route_enabled", "CTX_CLUSTER_ROUTE_ENABLED", "false", "hot", TenancyGlobalOnly},
	{"cluster.centroid_build", "CTX_CLUSTER_CENTROID_BUILD", "false", "hot", TenancyGlobalOnly},
	{"cluster.centroid_timeout", "CTX_CLUSTER_CENTROID_TIMEOUT", "300", "hot", TenancyGlobalOnly},
	{"cluster.centroid_batch", "CTX_CLUSTER_CENTROID_BATCH", "500", "hot", TenancyGlobalOnly},
	{"cluster.centroid_work_mem", "CTX_CLUSTER_CENTROID_WORK_MEM", "256MB", "hot", TenancyGlobalOnly},
	{"cluster.centroid_ann_threshold", "CTX_CLUSTER_CENTROID_ANN_THRESHOLD", "50000", "hot", TenancyGlobalOnly},
	{"cluster.centroid_ef_search", "CTX_CLUSTER_CENTROID_EF_SEARCH", "100", "hot", TenancyGlobalOnly},
}

// TestClusterRegistryContract is gate (i): every one of the 21 keys is
// registered exactly as §4.9 specifies, and the namespace contains NOTHING
// else — the second half is what pins K6 ("C0 is the only cluster.*-declaring
// wave"): a later wave sneaking a 22nd key in fails here, not in review.
func TestClusterRegistryContract(t *testing.T) {
	byKey := map[string]entry{}
	for _, e := range registry() {
		byKey[e.Key] = e
	}
	for _, want := range clusterKeys {
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
			t.Errorf("%s: mut = %q, want %q (every cluster knob is a runtime flip)", want.key, e.Mut, want.mut)
		}
		if e.Tenancy != want.tenancy {
			t.Errorf("%s: tenancy = %q, want %q (§4.9 ranking/ops cut)", want.key, e.Tenancy, want.tenancy)
		}
		if want.tenancy == TenancyGlobalOnly && !IsGlobalOnly(want.key) {
			t.Errorf("%s: IsGlobalOnly = false — a tenant could override a wire contract", want.key)
		}
	}

	declared := map[string]bool{}
	for _, want := range clusterKeys {
		declared[want.key] = true
	}
	for _, e := range registry() {
		if strings.HasPrefix(e.Key, "cluster.") && !declared[e.Key] {
			t.Errorf("%s: cluster.* key outside the C0 table (K6: C0 owns the namespace)", e.Key)
		}
	}
	if got := len(clusterKeys); got != 21 {
		t.Errorf("cluster.* contract has %d keys, expected 21 (change it with intent)", got)
	}
}

// TestClusterDefaultsAreOffAndDecided pins the SHIPPED generation: the whole
// consumption surface is dark (enabled/ego_annotate/facet/route/centroid_* all
// false, inject_max 0) while the ONE decided non-default default —
// size_damping=true, UD-04-03 — is armed. Whoever flips a gate flips it here
// first, visibly.
func TestClusterDefaultsAreOffAndDecided(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	if len(issues) != 0 {
		t.Fatalf("default generation: unexpected issues %v", issues)
	}
	for name, got := range map[string]bool{
		"cluster.enabled":          c.Cluster.Enabled,
		"cluster.centroid_enabled": c.Cluster.CentroidEnabled,
		"cluster.ego_annotate":     c.ClusterOps.EgoAnnotate,
		"cluster.facet_enabled":    c.ClusterOps.FacetEnabled,
		"cluster.route_enabled":    c.ClusterOps.RouteEnabled,
		"cluster.centroid_build":   c.ClusterOps.CentroidBuild,
	} {
		if got {
			t.Errorf("%s = true, want false — the shipped consumption surface is dark", name)
		}
	}
	if c.Cluster.InjectMax != 0 {
		t.Errorf("cluster.inject_max = %d, want 0 (C9 arms it after the eval measurement)", c.Cluster.InjectMax)
	}
	if !c.Cluster.SizeDamping {
		t.Error("cluster.size_damping = false, want true (UD-04-03: Ziel-Scale-Semantik ab Tag 1)")
	}
	// Bare-seconds convention (parseDurationSeconds): reading 86400 as
	// nanoseconds would silently disarm the C4 staleness gate.
	if got := c.ClusterOps.MaxStaleness; got.Hours() != 24 {
		t.Errorf("cluster.max_staleness = %v, want 24h (bare seconds)", got)
	}
	if got := c.ClusterOps.CentroidTimeout; got.Minutes() != 5 {
		t.Errorf("cluster.centroid_timeout = %v, want 5m (bare seconds)", got)
	}
}

// TestClusterComposeDeclaresEveryKey is gate (ii): a knob that the container
// cannot receive is not a knob. The documented legacy class is exactly this
// failure — docker-compose.yml declares only 3 of the 5 graph_overview.* env
// vars, so max_nodes/rebuild_timeout are unreachable through compose. Those
// two stay untouched here (K14: they belong to W-D); this gate holds the NEW
// namespace to the standard the old one missed.
func TestClusterComposeDeclaresEveryKey(t *testing.T) {
	declared := ctxServiceEnvNames(t)
	for _, want := range clusterKeys {
		if !declared[want.env] {
			t.Errorf("%s: %s missing from the ctx service environment: block — the key is unreachable through compose",
				want.key, want.env)
		}
	}
}

// ctxServiceEnvNames returns the env var names declared in the `ctx` service's
// `environment:` block of the repo-root docker-compose.yml. Line-scanned on
// purpose: pulling in a YAML dependency for one block would promote an
// indirect module to a direct one, and the block is a flat map of scalars.
func ctxServiceEnvNames(t *testing.T) map[string]bool {
	t.Helper()
	const path = "../../../docker-compose.yml" // go/internal/config -> repo root
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v (the compose gate needs the repo root checkout)", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	names := map[string]bool{}
	inCtx, inEnv := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2: // a service name
			inCtx, inEnv = trimmed == "ctx:", false
		case indent == 4 && inCtx: // a service-level key
			inEnv = trimmed == "environment:"
		case indent >= 6 && inCtx && inEnv:
			if name, _, ok := strings.Cut(trimmed, ":"); ok {
				names[strings.TrimSpace(name)] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if len(names) == 0 {
		t.Fatalf("no environment names found in the ctx service block of %s — the scanner lost the block", path)
	}
	return names
}
