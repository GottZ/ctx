//go:build integration

// Integration test for Multi-Tenant wave T40a (Achse 07, design/07 §4/§5.5/§7):
// the BILLIGE block-level "Abruf-only-OR". A single block stays in its scope but
// becomes additively READABLE for a grantee tenant via the resolved grant set
// flowing as a bound uuid[] into the visibility OR-arm (VisibilityPredicate plus
// the three blocks.go inline paths). The pausability invariant: an EMPTY grant
// set is a byte-identical no-op to the scope-only state.
//
// Setup: owner scope C (block B lives here), grantee tenant A (home_scope A).
// The grantee reads with readScopes=["A"]; WITHOUT a grant B is invisible (404),
// WITH grants=[B.id] B is visible — through GetBlock / SearchBlocks / EgoGraph.
//
// G-Gates (RED before T40a, GREEN after):
//   - G1 Abruf: grantee finds B with grants, misses it without.
//   - G2 Revocation: DELETE the grant → GrantedBlockIDs empty → B invisible again.
//   - G3 Pflicht-Klammer: a grant on an ARCHIVED block must NOT surface (the
//     NOT is_archived term in front of the parentheses wins) — on the inline
//     GetBlock path AND on the VisibilityPredicate EgoGraph path.
//   - G5 empty-scope (konservativ gepinnt): empty readScopes + grants → the
//     RequireScopes guard still errors (fail-closed), NOT the block.
//   - byte-identical: with grants=nil the in-scope/out-of-scope behaviour is the
//     pre-T40a behaviour (the no-op proof).
//
// pgCode, defaultTenantID, seedBlock are declared elsewhere in store_test
// (tenants_hybrid / scope_generalize) and reused — NOT redeclared.
//
//	go test -tags=integration ./internal/store/ -run TestBlockGrantsT40a -count=1 -v
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// t40Tenant registers one tenant and returns its UUID.
func t40Tenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	return id
}

// t40MapScope maps one scope to a tenant (context_tenant_scopes, model C).
func t40MapScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("map scope %s: %v", scope, err)
	}
}

// t40Block seeds one block in an EXPLICIT scope (seedBlock hardcodes 'private')
// and returns its id. archived=true lets the G3 probe seed a granted-but-archived
// block.
func t40Block(t *testing.T, pool *pgxpool.Pool, scope, title string, archived bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, is_archived)
		 VALUES ('learnings', $1, 'content of ' || $1, $2, $3) RETURNING id::text`,
		title, scope, archived).Scan(&id); err != nil {
		t.Fatalf("seed block %s (scope %s): %v", title, scope, err)
	}
	return id
}

// t40Grant inserts a row-level read grant (block B → grantee tenant).
func t40Grant(t *testing.T, pool *pgxpool.Pool, blockID, granteeTenant string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_block_grants (block_id, grantee_tenant) VALUES ($1::uuid, $2::uuid)`,
		blockID, granteeTenant); err != nil {
		t.Fatalf("insert grant block=%s grantee=%s: %v", blockID, granteeTenant, err)
	}
}

func previewIDs(ps []store.BlockPreview) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func TestBlockGrantsT40a_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Owner tenant owns scope C; grantee tenant A owns scope A.
	const scopeC = "t40-c" // owner scope (block B lives here)
	const scopeA = "t40-a" // grantee scope (A's home_scope)
	ownerTenant := t40Tenant(t, pool, "t40-owner")
	granteeTenant := t40Tenant(t, pool, "t40-grantee")
	t40MapScope(t, pool, scopeC, ownerTenant)
	t40MapScope(t, pool, scopeA, granteeTenant)

	blockB := t40Block(t, pool, scopeC, "t40-shared-block-B", false)
	granteeScopes := []string{scopeA}

	// G1 (Abruf): GetBlock + SearchBlocks, with and without grants.
	t.Run("G1_GetBlock_visible_with_grant", func(t *testing.T) {
		grants, err := store.GrantedBlockIDs(ctx, pool, granteeTenant)
		if err != nil {
			t.Fatalf("GrantedBlockIDs (pre-grant): %v", err)
		}
		// Pre-grant: B is NOT granted yet → invisible to A.
		got, err := store.GetBlock(ctx, pool, blockB, granteeScopes, grants)
		if err != nil {
			t.Fatalf("GetBlock pre-grant: %v", err)
		}
		if got != nil {
			t.Fatalf("GetBlock pre-grant returned block %s, want nil (A has no grant + B not in scope A)", got.ID)
		}

		// Grant B to A, re-resolve, GetBlock must now see it.
		t40Grant(t, pool, blockB, granteeTenant)
		grants, err = store.GrantedBlockIDs(ctx, pool, granteeTenant)
		if err != nil {
			t.Fatalf("GrantedBlockIDs (post-grant): %v", err)
		}
		if !slices.Contains(grants, blockB) {
			t.Fatalf("GrantedBlockIDs = %v, want it to contain %s", grants, blockB)
		}
		got, err = store.GetBlock(ctx, pool, blockB, granteeScopes, grants)
		if err != nil {
			t.Fatalf("GetBlock post-grant: %v", err)
		}
		if got == nil || got.ID != blockB {
			t.Fatalf("GetBlock post-grant = %v, want block %s (visible via grant OR-arm)", got, blockB)
		}
	})

	t.Run("G1_SearchBlocks_visible_with_grant", func(t *testing.T) {
		grants, err := store.GrantedBlockIDs(ctx, pool, granteeTenant)
		if err != nil {
			t.Fatalf("GrantedBlockIDs: %v", err)
		}
		// With grant: B appears in A's browse (empty query).
		withGrant, err := store.SearchBlocks(ctx, pool, "", granteeScopes, "", nil, 50, true, nil, grants)
		if err != nil {
			t.Fatalf("SearchBlocks with grant: %v", err)
		}
		if !slices.Contains(previewIDs(withGrant), blockB) {
			t.Fatalf("SearchBlocks(grants=[B]) results %v, want it to contain %s", previewIDs(withGrant), blockB)
		}
		// Without grant (empty set): B is gone.
		noGrant, err := store.SearchBlocks(ctx, pool, "", granteeScopes, "", nil, 50, true, nil, []string{})
		if err != nil {
			t.Fatalf("SearchBlocks no grant: %v", err)
		}
		if slices.Contains(previewIDs(noGrant), blockB) {
			t.Fatalf("SearchBlocks(grants=[]) leaked %s into scope A (no-op OR-arm broken)", blockB)
		}
	})

	// G3 (Pflicht-Klammer): a grant on an ARCHIVED block must NOT surface.
	t.Run("G3_archived_granted_block_stays_invisible", func(t *testing.T) {
		archB := t40Block(t, pool, scopeC, "t40-archived-granted", true)
		t40Grant(t, pool, archB, granteeTenant)
		grants, err := store.GrantedBlockIDs(ctx, pool, granteeTenant)
		if err != nil {
			t.Fatalf("GrantedBlockIDs: %v", err)
		}
		if !slices.Contains(grants, archB) {
			t.Fatalf("archived block %s not in grant set %v (setup bug)", archB, grants)
		}
		// GetBlock inline path: NOT is_archived stands BEFORE the (scope OR id)
		// parentheses, so the archived granted block must NOT come back.
		got, err := store.GetBlock(ctx, pool, archB, granteeScopes, grants)
		if err != nil {
			t.Fatalf("GetBlock archived granted: %v", err)
		}
		if got != nil {
			t.Fatalf("GetBlock returned ARCHIVED granted block %s — the mandatory parentheses leaked (G3)", got.ID)
		}
		// SearchBlocks browse path: same guarantee.
		res, err := store.SearchBlocks(ctx, pool, "", granteeScopes, "", nil, 50, true, nil, grants)
		if err != nil {
			t.Fatalf("SearchBlocks archived granted: %v", err)
		}
		if slices.Contains(previewIDs(res), archB) {
			t.Fatalf("SearchBlocks leaked ARCHIVED granted block %s (mandatory parentheses broken)", archB)
		}
	})

	// G2 (Revocation): DELETE the grant → GrantedBlockIDs empty for B → invisible
	// in every path. Use a fresh tenant/block so the deletion does not race the
	// other sub-tests' grant on the shared granteeTenant.
	t.Run("G2_revocation_makes_block_invisible", func(t *testing.T) {
		revTenant := t40Tenant(t, pool, "t40-rev-grantee")
		const revScope = "t40-rev"
		t40MapScope(t, pool, revScope, revTenant)
		revBlock := t40Block(t, pool, scopeC, "t40-rev-block", false)
		t40Grant(t, pool, revBlock, revTenant)

		grants, err := store.GrantedBlockIDs(ctx, pool, revTenant)
		if err != nil || !slices.Contains(grants, revBlock) {
			t.Fatalf("pre-revoke GrantedBlockIDs=%v err=%v, want it to contain %s", grants, err, revBlock)
		}
		got, err := store.GetBlock(ctx, pool, revBlock, []string{revScope}, grants)
		if err != nil || got == nil {
			t.Fatalf("pre-revoke GetBlock = (%v,%v), want visible", got, err)
		}

		// Revoke.
		if _, err := pool.Exec(ctx, `DELETE FROM context_block_grants WHERE block_id=$1::uuid AND grantee_tenant=$2::uuid`, revBlock, revTenant); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		grants, err = store.GrantedBlockIDs(ctx, pool, revTenant)
		if err != nil {
			t.Fatalf("post-revoke GrantedBlockIDs: %v", err)
		}
		if slices.Contains(grants, revBlock) {
			t.Fatalf("post-revoke GrantedBlockIDs still has %s, want it gone", revBlock)
		}
		got, err = store.GetBlock(ctx, pool, revBlock, []string{revScope}, grants)
		if err != nil {
			t.Fatalf("post-revoke GetBlock: %v", err)
		}
		if got != nil {
			t.Fatalf("post-revoke GetBlock returned %s, want nil (revocation immediate)", got.ID)
		}
	})

	// G5 (empty-scope, konservativ gepinnt): the RequireScopes guard fires even
	// with a non-empty grant set — fail-closed, NOT the block.
	t.Run("G5_empty_scope_still_fails_closed_with_grant", func(t *testing.T) {
		grants := []string{blockB}
		if _, err := store.GetBlock(ctx, pool, blockB, []string{}, grants); !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("GetBlock(empty scopes, grants=[B]) err = %v, want store.ErrNoScopes (G5 conservative pin)", err)
		}
		if _, err := store.SearchBlocks(ctx, pool, "", []string{}, "", nil, 50, true, nil, grants); !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("SearchBlocks(empty scopes, grants=[B]) err = %v, want store.ErrNoScopes (G5)", err)
		}
	})

	// byte-identical no-op: with grants=nil an in-scope block is visible and an
	// out-of-scope block is not — exactly the pre-T40a scope-only behaviour.
	t.Run("ByteIdentical_nil_grants_is_scope_only", func(t *testing.T) {
		ownerOwnBlock := t40Block(t, pool, scopeC, "t40-owner-own", false)
		// Owner (scope C) sees its own block with nil grants.
		got, err := store.GetBlock(ctx, pool, ownerOwnBlock, []string{scopeC}, nil)
		if err != nil || got == nil || got.ID != ownerOwnBlock {
			t.Fatalf("GetBlock(scope C, nil grants) = (%v,%v), want the in-scope block %s", got, err, ownerOwnBlock)
		}
		// Grantee (scope A) does NOT see C's block with nil grants (no-op OR-arm).
		got, err = store.GetBlock(ctx, pool, ownerOwnBlock, granteeScopes, nil)
		if err != nil {
			t.Fatalf("GetBlock(scope A, nil grants): %v", err)
		}
		if got != nil {
			t.Fatalf("GetBlock(scope A, nil grants) returned %s, want nil (nil grants must be a scope-only no-op)", got.ID)
		}
	})

	// GrantedBlockIDs contract: empty/whitespace tenant → non-nil empty slice.
	t.Run("GrantedBlockIDs_empty_tenant_is_nonnil_empty", func(t *testing.T) {
		got, err := store.GrantedBlockIDs(ctx, pool, "")
		if err != nil {
			t.Fatalf("GrantedBlockIDs(\"\"): %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("GrantedBlockIDs(\"\") = %v (nil=%t), want non-nil empty slice", got, got == nil)
		}
	})
}

// TestBlockGrantsT40a_EgoGraph_Integration pins the VisibilityPredicate path
// (EgoGraph): a granted block becomes a VISIBLE focus, and the mandatory
// parentheses keep an archived granted block out of the focus hydrate.
func TestBlockGrantsT40a_EgoGraph_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scopeC = "t40g-c"
	const scopeA = "t40g-a"
	owner := t40Tenant(t, pool, "t40g-owner")
	grantee := t40Tenant(t, pool, "t40g-grantee")
	t40MapScope(t, pool, scopeC, owner)
	t40MapScope(t, pool, scopeA, grantee)

	focus := t40Block(t, pool, scopeC, "t40g-focus", false)
	granteeScopes := []string{scopeA}

	// Without a grant the foreign-scope focus is ErrNotVisible (404, no oracle).
	if _, err := store.EgoGraph(ctx, pool, store.EgoParams{Focus: focus, Hops: 1, Limit: 10}, granteeScopes, nil); !errors.Is(err, store.ErrNotVisible) {
		t.Fatalf("EgoGraph(foreign focus, no grant) err = %v, want store.ErrNotVisible", err)
	}

	// Grant B → grantee: the focus hydrate's VisibilityPredicate OR-arm makes it
	// visible.
	t40Grant(t, pool, focus, grantee)
	grants, err := store.GrantedBlockIDs(ctx, pool, grantee)
	if err != nil {
		t.Fatalf("GrantedBlockIDs: %v", err)
	}
	res, err := store.EgoGraph(ctx, pool, store.EgoParams{Focus: focus, Hops: 1, Limit: 10}, granteeScopes, grants)
	if err != nil {
		t.Fatalf("EgoGraph(granted focus) err = %v, want visible focus", err)
	}
	if res == nil || res.Focus != focus {
		t.Fatalf("EgoGraph(granted focus) focus = %v, want %s", res, focus)
	}

	// Archived granted focus: the NOT is_archived term before the parentheses
	// keeps it a 404 (mandatory parentheses on the VisibilityPredicate path).
	archFocus := t40Block(t, pool, scopeC, "t40g-archived-focus", true)
	t40Grant(t, pool, archFocus, grantee)
	grants, err = store.GrantedBlockIDs(ctx, pool, grantee)
	if err != nil {
		t.Fatalf("GrantedBlockIDs (arch): %v", err)
	}
	if _, err := store.EgoGraph(ctx, pool, store.EgoParams{Focus: archFocus, Hops: 1, Limit: 10}, granteeScopes, grants); !errors.Is(err, store.ErrNotVisible) {
		t.Fatalf("EgoGraph(archived granted focus) err = %v, want store.ErrNotVisible (mandatory parentheses)", err)
	}
}
