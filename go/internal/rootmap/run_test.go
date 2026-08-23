// Wave W-D unit gates (Cluster-Topic-Map, design/02 §7 "W-D"): the three
// refusals that must hold BEFORE the first query, plus the type-policy
// derivation the coverage section stands on.
//
// A nil *pgxpool.Pool is the instrument here, not a shortcut: every one of
// these paths must decide without touching the database, and a version that
// queries first would panic instead of returning — which is exactly the gate.
package rootmap_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/rootmap"
)

func baseConfig() rootmap.Config {
	return rootmap.Config{
		Enabled:            true,
		BudgetBytes:        15360,
		FooterReserveBytes: 512,
		SmallClusterMax:    2,
	}
}

// TestRunDisabledIsNoOp is gate 1: root_map.enabled=false makes the wave a
// no-op deploy — no block, and above all no QUERY. That is what turns W-D from
// a behaviour change into a deployment that can sit dormant until someone flips
// the flag deliberately.
//
// RED against a version that reads first and checks the flag later: the nil
// pool turns the first read into a panic.
func TestRunDisabledIsNoOp(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = false

	res, err := rootmap.Run(context.Background(), nil, nil, cfg, "private", []string{"private"})
	if err != nil {
		t.Fatalf("disabled run returned an error: %v", err)
	}
	if res.Written || !res.Skipped || res.Reason != "disabled" {
		t.Fatalf("disabled run = %+v, want skipped/disabled and nothing written", res)
	}
	if res.Title != "root-map-private" {
		t.Errorf("title = %q, want root-map-private (the address exists even when the map does not)", res.Title)
	}
}

// TestRunRefusesOversizeBudget is gate 6 (BP-5): a budget above the 50 KB
// public write cap is refused BEFORE the database is touched. The old topic map
// exists at 80.103 characters only because RunDigest calls store.UpsertBlock
// directly and thereby bypasses the cap the HTTP write path enforces; the root
// map refuses instead of inheriting that privilege.
//
// RED against a version without the check: nothing stops the run, and at 60 KB
// the block would be written — unwritable through the public API it shares a
// table with.
func TestRunRefusesOversizeBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*rootmap.Config)
		want string
	}{
		{"over the store cap", func(c *rootmap.Config) { c.BudgetBytes = 60 * 1024 }, "store write cap"},
		{"zero budget", func(c *rootmap.Config) { c.BudgetBytes = 0 }, "must be > 0"},
		{"zero footer reserve", func(c *rootmap.Config) { c.FooterReserveBytes = 0 }, "must be > 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mut(&cfg)
			_, err := rootmap.Run(context.Background(), nil, nil, cfg, "private", []string{"private"})
			if err == nil {
				t.Fatalf("%s was accepted — the check runs after the reads or not at all", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestRunRefusesUnwiredRegistry: without the block-type registry the map cannot
// be classified into system-meta, and an unclassified index block is a
// retrieval candidate — the slot theft the type policy exists to prevent (a
// 15 KB map is a smaller thief than the 80 KB one, but the same kind). Refusing
// to write is the fail-closed direction; writing an unclassifiable block is not.
func TestRunRefusesUnwiredRegistry(t *testing.T) {
	_, err := rootmap.Run(context.Background(), nil, nil, baseConfig(), "private", []string{"private"})
	if err == nil || !strings.Contains(err.Error(), "registry not wired") {
		t.Fatalf("nil registry error = %v, want a loud refusal", err)
	}
}

// TestRunRefusesEmptyHomeScope is BP-4 at its narrowest point: no write scope,
// no write. The live artefact `topic-map-hth` — title says hth, scope says
// work — is what a missing home-scope clamp leaves behind years later.
func TestRunRefusesEmptyHomeScope(t *testing.T) {
	set := blocktype.NewRegistry().Snapshot() // the compiled-in fallback generation
	_, err := rootmap.Run(context.Background(), nil, set, baseConfig(), "", []string{"private"})
	if err == nil || !strings.Contains(err.Error(), "home scope") {
		t.Fatalf("empty home scope error = %v, want a loud refusal", err)
	}
}

// TestTypePolicyDerivation is the E11-02 / A02-2 half of the coverage section:
// checkpoints are OPERATIONAL — outside the map by decision, with a reason —
// while the rest of the non-clustered types are simply outside the cut.
//
// RED against a version without the operational split: `checkpoint` appears in
// ExcludedTypes, the map reports ~83 % of the corpus as a coverage GAP, and the
// next session re-escalates a decision that was settled with evidence.
func TestTypePolicyDerivation(t *testing.T) {
	set := blocktype.NewRegistry().Snapshot()

	opTypes, rationale := rootmap.OperationalTypes(set, "")
	if len(opTypes) != 1 || opTypes[0] != "checkpoint" {
		t.Fatalf("operational types = %v, want [checkpoint]", opTypes)
	}
	if rationale != "hermes-Compaction-Artefakte" {
		t.Errorf("rationale = %q — the coverage line loses its WHY", rationale)
	}

	excluded := rootmap.ExcludedTypes(set)
	for _, name := range excluded {
		if name == "checkpoint" {
			t.Error("checkpoint is listed as a coverage gap — E11-02 says it is a decision, not a gap")
		}
	}
	// system-meta is the instructive member: overview.include is TRUE, but
	// retrieval=excluded keeps it out of the cut. A derivation that only reads
	// overview.include would miss it and the map would claim a wider cut than
	// the rebuild has.
	if !contains(excluded, "system-meta") {
		t.Errorf("excluded types = %v, want system-meta among them (the cut is visible ∩ overview)", excluded)
	}
	for _, want := range []string{"issue", "comment"} {
		if !contains(excluded, want) {
			t.Errorf("excluded types = %v, want %s among them", excluded, want)
		}
	}
	// Determinism: the map's text is byte-compared against the stored one, so
	// every list it prints must have a stable order.
	for i := 1; i < len(excluded); i++ {
		if excluded[i-1] > excluded[i] {
			t.Fatalf("excluded types are not sorted: %v — the map would rewrite itself at random", excluded)
		}
	}
	// Clustered types never appear on either list.
	for _, name := range []string{"knowledge", "reference", "audit-trail"} {
		if contains(excluded, name) || contains(opTypes, name) {
			t.Errorf("%s is inside the Louvain cut and must appear on neither list", name)
		}
	}
}

// TestEmptyNodeCutIsNamed: migration 126 added a SIXTH skip reason after the
// renderer's freeze table was written. It is the freeze most likely to be
// misread as a bug — the node set is the INTERSECTION of the retrieval-visible
// types and overview.include, and an empty intersection selects nothing — so
// the map has to say what happened instead of falling back to the generic
// "übersprungen (empty-node-cut)".
//
// RED against the table without the entry: the map prints the raw enum value.
func TestEmptyNodeCutIsNamed(t *testing.T) {
	in := rootmap.Input{
		Scope:              "private",
		BudgetBytes:        15360,
		FooterReserveBytes: 512,
		SmallClusterMax:    2,
		Freshness:          rootmap.Freshness{SkipReason: "empty-node-cut", ClusterN: 1},
	}
	out, err := rootmap.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(out.Text, "!! Partition eingefroren") {
		t.Fatalf("no freeze block for empty-node-cut:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "übersprungen (empty-node-cut)") {
		t.Errorf("the map prints the raw enum value instead of naming the cause:\n%s", out.Text)
	}
	if !strings.Contains(out.Text, "Knotenschnitt") {
		t.Errorf("freeze clause does not explain the empty node cut:\n%s", out.Text)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
