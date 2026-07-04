//go:build integration

// WF T12 Semantik-Fixier-Probe (design/01-type-registry.md §5.4 / D6): the
// Reader-Overlay-wins semantic for GRANTED foreign blocks. A grantee-tenant-admin
// that overrides a _global-excluded type (system-meta → full-pass) pulls granted
// FOREIGN blocks of that type into its OWN retrieval ranking — p_scopes carries
// the granted foreign scope while p_types_visible comes from the grantee overlay,
// and the overlay steers the ranking of foreign blocks too. This test FIXES the
// v1 "Overlay gewinnt" decision so a later flip to monotonic narrowing (the named
// open decision D6) shows up as a deliberate break.
//
//	go test -tags=integration ./internal/rrf/ -run T12 -count=1 -v
package rrf_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

func t12Contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestT12_ReaderOverlayWins_GrantedForeignBlock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := t5Registry(t, ctx, pool)
	emb := t40bEmbedding()
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	const (
		scopeGrantee  = "t12-grantee" // tenant A: the reader
		scopeOwner    = "t12-owner"   // tenant B: grants A a scope
		idForeignMeta = "019f2299-0000-7000-9000-00000000a001"
		idGranteeKnow = "019f2299-0000-7000-9000-00000000a002"
	)

	// B's foreign system-meta block (excluded in the _global base) + a control
	// knowledge block in A's own scope so the query always returns something.
	t40bInsertBlock(t, pool, idForeignMeta, scopeOwner, "system-meta", false, emb, now)
	t40bInsertBlock(t, pool, idGranteeKnow, scopeGrantee, "knowledge", false, emb, now)

	// The grant arm: B's block is reachable to A (p_grant_ids); scope filter is
	// A's own scope only. The TYPE gate (p_types_visible) still decides.
	query := func(visible []string) map[string]bool {
		res, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
			[]string{scopeGrantee}, nil, nil, 10, "", "", visible, nil, nil, nil, nil,
			[]string{idForeignMeta})
		if err != nil {
			t.Fatalf("rrf.Search: %v", err)
		}
		return idSet(res)
	}

	// CONTRAST (no override): the grantee overlay == base, system-meta excluded
	// from VisibleTypes ⇒ the granted foreign system-meta block is HIDDEN.
	baseVisible := reg.SnapshotForTenant(ctx, scopeGrantee).VisibleTypes()
	if t12Contains(baseVisible, "system-meta") {
		t.Fatal("precondition broken: system-meta must be excluded in the base generation")
	}
	if got := query(baseVisible); got[idForeignMeta] {
		t.Errorf("granted foreign system-meta visible WITHOUT a grantee override — contrast broken; got=%v", got)
	} else if !got[idGranteeKnow] {
		t.Errorf("control knowledge block missing — query wiring broken; got=%v", got)
	}

	// Grantee-admin overrides system-meta → full-pass IN ITS OWN SCOPE.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('system-meta', $1, false, false, '{"v":1,"retrieval":{"policy":"full-pass"}}'::jsonb)`,
		scopeGrantee); err != nil {
		t.Fatalf("insert grantee override: %v", err)
	}
	reg.InvalidateTenant(scopeGrantee)

	overVisible := reg.SnapshotForTenant(ctx, scopeGrantee).VisibleTypes()
	if !t12Contains(overVisible, "system-meta") {
		t.Fatalf("grantee overlay did not resolve system-meta→full-pass; visible=%v", overVisible)
	}

	// REGRESSION-FIX (v1 "Overlay gewinnt", D6): the granted foreign system-meta
	// block now APPEARS in the grantee's ranking. RED under reversed precedence
	// (base wins over the tenant row) — system-meta would stay excluded and the
	// block would remain hidden even with the override. The named alternative
	// D6 (monotonic narrowing: forbid a tenant lifting a _global-excluded type)
	// would ALSO make this RED — hence the fixier probe: a switch to narrowing
	// is a deliberate, visible break, never a silent drift.
	if got := query(overVisible); !got[idForeignMeta] {
		t.Errorf("Reader-Overlay does NOT win: granted foreign system-meta hidden despite grantee override (D6 precedence broken); got=%v", got)
	}
}
