//go:build integration

// Cost gate of issue #342, step (c): a full-text search that names no type,
// with search.leave_out_never_ranked off and on.
//
// The full-text GIN indexes are partial since migration 145: they leave out
// checkpoint and system-meta. Off (the default), such a search binds no type
// filter, the planner cannot prove the index predicate and reads every row's
// tsvector, the never-ranked rows' included. On, store.SearchTypesExclude adds
// the caller's never-ranked types to the exclusion; that list holds both names
// of the deny-list, so searchBlocksSQL declares the index predicate as constant
// text and the planner can use both partial indexes under a custom and under a
// generic plan.
//
// The fixture is the reference instance's shape at 10 000 rows: 57 % checkpoint
// parts interleaved in time with ranked blocks (unrankedSeed), next to a store
// with the same ranked blocks alone. The bound for the search with the key on
// is the same search on the store without the never-ranked rows, scaled by the
// ratio of the two heaps (the matching ranked rows lie on more heap pages
// there), plus a quarter.
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSearchUntypedFullTextLeavesOutNeverRankedCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	base := unrankedShape{ranked: 4300, unranked: 5700, unrankedWidth: 12000, denseRanked: 20, interleaved: true}
	without := base
	without.omitUnranked = true
	with := testdb.SetupSeededTestDB(t, "i342c-cost-with", unrankedSeed(base))
	alone := testdb.SetupSeededTestDB(t, "i342c-cost-without", unrankedSeed(without))
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, with)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()
	heapRatio := float64(unrankedHeapPages(t, with)) / float64(unrankedHeapPages(t, alone))
	t.Logf("heap pages with/without the never-ranked rows: ratio %.2f", heapRatio)

	scopes := []string{"private"}
	// The gate term is in every never-ranked row and in the 20 newest ranked
	// ones; the mid-frequency word is somewhere in about a fifth of the wide
	// rows and in some ranked ones.
	terms := []string{unrankedTerm, unrankedWords[2000]}
	for _, form := range []struct {
		term    string
		compact bool
	}{{terms[0], true}, {terms[1], true}, {terms[0], false}} {
		term := form.term
		off := store.SearchTypesExclude(set, false, term, nil, nil)
		on := store.SearchTypesExclude(set, true, term, nil, nil)
		if off != nil {
			t.Fatalf("key off: exclusion %v, want none", off)
		}
		for _, name := range []string{"checkpoint", "system-meta"} {
			if !strings.Contains(strings.Join(on, ","), name) {
				t.Fatalf("key on: exclusion %v lacks %s", on, name)
			}
		}
		for _, plan := range []string{"custom", "generic"} {
			explain := testdb.ExplainCustom
			if plan == "generic" {
				explain = testdb.ExplainGeneric
			}
			stmt := func(exclude []string) (string, []any) {
				return store.SearchBlocksSQLForTest(store.SearchBlocksParams{
					Query: term, ReadScopes: scopes, Limit: store.SearchDefaultLimit, Compact: form.compact,
					ProbeNext: true, TypesExclude: exclude,
				})
			}
			offSQL, offArgs := stmt(off)
			onSQL, onArgs := stmt(on)
			gotOff := explain(t, with, offSQL, offArgs...)
			gotOn := explain(t, with, onSQL, onArgs...)
			ref := explain(t, alone, onSQL, onArgs...)
			bound := int64(float64(ref.Buffers)*heapRatio*1.25) + 16
			t.Logf("%q compact=%t (%s plan): key off %d buffers (%.1f ms), key on %d (%.1f ms), on the store without never-ranked rows %d, bound %d",
				term, form.compact, plan, gotOff.Buffers, gotOff.ExecMs, gotOn.Buffers, gotOn.ExecMs, ref.Buffers, bound)
			for _, idx := range []string{"idx_context_ts_de", "idx_context_ts_en"} {
				if !strings.Contains(gotOn.Plan, `"Index Name": "`+idx+`"`) {
					t.Errorf("%q (%s plan), key on: the plan does not use %s:\n%s", term, plan, idx, gotOn.Plan)
				}
			}
			if gotOn.Buffers > bound {
				t.Errorf("%q (%s plan), key on: %d buffers, more than %d (%d without the never-ranked rows × heap ratio %.2f × 1.25 + 16): the search reads the never-ranked rows",
					term, plan, gotOn.Buffers, bound, ref.Buffers, heapRatio)
			}
			if gotOn.Buffers*2 > gotOff.Buffers {
				t.Errorf("%q (%s plan): key on %d buffers is not below half of key off %d", term, plan, gotOn.Buffers, gotOff.Buffers)
			}
		}
	}

	// A registry that ranks checkpoint (the type API refuses such a tenant
	// overlay; a row written around it) keeps it in the search: the exclusion
	// then lacks one name of the deny-list, no index premise is declared, and
	// the search costs what it costs with the key off. Logged, not gated:
	// correctness decides that case (never hide a row the caller's registry
	// ranks), and the cost is the one before the key.
	lifted := []string{"catalog", "insight", "system-meta"}
	for _, plan := range []string{"custom", "generic"} {
		explain := testdb.ExplainCustom
		if plan == "generic" {
			explain = testdb.ExplainGeneric
		}
		sql, args := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
			Query: unrankedTerm, ReadScopes: scopes, Limit: store.SearchDefaultLimit, Compact: true,
			ProbeNext: true, TypesExclude: lifted,
		})
		got := explain(t, with, sql, args...)
		t.Logf("checkpoint ranked by the registry (%s plan): %d buffers (%.1f ms)", plan, got.Buffers, got.ExecMs)
	}
}
