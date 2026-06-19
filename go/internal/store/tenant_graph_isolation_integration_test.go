//go:build integration

// Multi-Tenant wave T16a (02-V3, isolation arm): end-to-end MT graph isolation.
//
// The four single-path scope-leak negative probes ALREADY EXIST from the F3/F5
// build and feed the switch point a HAND-BUILT flat scope list:
//
//   - §5.2.1 bridge / §5.2.2 induced / §5.2.3 degree / §5.2.4 oracle:
//     graph_integration_test.go::TestEgoGraph_ScopeFixture
//   - §5.2 bridge-edge / §5.5 size:  overview_integration_test.go::TestGraphOverview_ScopeNegativeProbes
//   - §5.4 supersedes-straggler:     handler/query_sensitivity_test.go::TestAnnotateSensitivities_LookupMissFailsClosed
//   - empty-scope guard (T07):       failclosed_scope_integration_test.go::TestReadPathsFailClosedOnEmptyScopes_Integration
//
// What no test covered until now is the COMPOSITION that V3 is actually about
// (design/02 §530 "RRF/GraphExpand/EgoGraph/Overview unter Multi-Tenant",
// §548 "Mechanik erbt V2"): the read_scopes that ctx_auth RESOLVES for a REAL
// tenant (context_tenants + context_tenant_scopes + tenant-bound key, migration
// 060), fed to the graph read paths, isolate that tenant. The existing probes
// stop at a hand-built []string; the auth probes stop at ar.ReadScopes without
// ever feeding a graph path. This pins the seam between them:
//
//	tenant key → auth.Authenticate (ctx_auth) → ar.ReadScopes
//	           → EgoGraph / GraphOverview → foreign tenant's blocks invisible
//
// including the PROMOTION case (a dream_link whose link.scope IS visible but
// whose target block.scope is NOT — the gate keys on context_blocks.scope,
// visibility.go:20-22) and the SUSPEND→{} path (the 060 sentinel emits
// read_scopes='{}', composed with the T07 RequireScopes guard → fail-closed,
// not full access).
//
// No product-code delta (design/02 §537). The mutation proofs (a hop→link.scope,
// b scope_t drop, c node-scope drop, e guard drop) were run on file copies under
// .gocache/verify-t16a/ — see the commit body. Reuses gInsertBlock/gNodeIDs/
// gEgoParams from graph_integration_test.go (same package store_test).
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Two tenants, model C: A owns t16-a, B owns t16-b, both read t16-shared via
// allowed_scopes. t16-b is A's blind spot and vice versa.
const (
	t16ScopeA      = "t16-a"
	t16ScopeB      = "t16-b"
	t16ScopeShared = "t16-shared"

	t16BA    = "019e1600-0000-7000-9000-00000000000a" // t16-a   (A-private)
	t16BB    = "019e1600-0000-7000-9000-00000000000b" // t16-b   (B-private)
	t16BS1   = "019e1600-0000-7000-9000-000000000051" // shared  (ego focus)
	t16BS2   = "019e1600-0000-7000-9000-000000000052" // shared  (only reachable via BB)
	t16BProm = "019e1600-0000-7000-9000-0000000000a0" // t16-b   (promotion target)
)

// t16Tenant registers one tenant (slug+status) and returns its UUID.
func t16Tenant(t *testing.T, pool *pgxpool.Pool, slug, status string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name, status) VALUES ($1,$2,$3) RETURNING id::text`,
		slug, slug, status).Scan(&id); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	return id
}

// t16MapScope maps one scope to a tenant (context_tenant_scopes, model C).
func t16MapScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`,
		scope, tenantID); err != nil {
		t.Fatalf("map scope %s: %v", scope, err)
	}
}

// t16Key mints a tenant-bound key and returns the plaintext for auth.Authenticate.
// It creates against the default tenant and then pins tenant_id with an explicit
// UPDATE, so a nil allowed_scopes keeps the {shared} default-tenant inheritance
// these isolation probes rely on (T06's CreateApiKey would hand a foreign tenant
// {} instead — deliberately not what is exercised here).
func t16Key(t *testing.T, pool *pgxpool.Pool, label, home string, allowed []string, tenantID string) string {
	t.Helper()
	key, plaintext, err := store.CreateApiKey(context.Background(), pool, label, home, allowed, "")
	if err != nil {
		t.Fatalf("create key %s: %v", label, err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_api_keys SET tenant_id = $1::uuid WHERE id = $2::uuid`, tenantID, key.ID); err != nil {
		t.Fatalf("pin key %s to tenant: %v", label, err)
	}
	return plaintext
}

// t16Link seeds one dream link with an EXPLICIT link scope — the promotion lever
// (gInsertLink hardcodes 'shared', ovInsLink 'private'; neither lets a link.scope
// diverge from its target block.scope).
func t16Link(t *testing.T, pool *pgxpool.Pool, src, dst, rel string, conf float64, linkScope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid,$2::uuid,$3,$4,$4,$5,5)`,
		src, dst, rel, conf, linkScope); err != nil {
		t.Fatalf("insert link %s→%s (scope %s): %v", src, dst, linkScope, err)
	}
}

func TestTenantGraphIsolation_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// --- tenants + scope map (model C: data tables carry NO tenant_id) ---
	tenantA := t16Tenant(t, pool, "t16-tenant-a", "active")
	tenantB := t16Tenant(t, pool, "t16-tenant-b", "active")
	t16MapScope(t, pool, t16ScopeA, tenantA)
	t16MapScope(t, pool, t16ScopeB, tenantB)
	t16MapScope(t, pool, t16ScopeShared, tenantA) // ownership is irrelevant to read_scopes here (key allowed_scopes carries it)

	// --- tenant-bound keys; ctx_auth (060) RESOLVES read_scopes from these ---
	keyA := t16Key(t, pool, "t16-key-a", t16ScopeA, []string{t16ScopeShared}, tenantA)
	keyB := t16Key(t, pool, "t16-key-b", t16ScopeB, []string{t16ScopeShared}, tenantB)

	arA, err := auth.Authenticate(ctx, pool, keyA)
	if err != nil {
		t.Fatalf("authenticate A: %v", err)
	}
	arB, err := auth.Authenticate(ctx, pool, keyB)
	if err != nil {
		t.Fatalf("authenticate B: %v", err)
	}
	// Sanity: the resolution gives each tenant its own + shared, never the other's.
	// (Exact ordering/positionality is pinned by the auth ctx_auth probes; here we
	// guard only the membership the isolation assertions below rely on.)
	if !arA.IsValid || !slices.Contains(arA.ReadScopes, t16ScopeA) || !slices.Contains(arA.ReadScopes, t16ScopeShared) || slices.Contains(arA.ReadScopes, t16ScopeB) {
		t.Fatalf("tenant A read_scopes = %v, want {t16-a, t16-shared} without t16-b", arA.ReadScopes)
	}
	if !arB.IsValid || !slices.Contains(arB.ReadScopes, t16ScopeB) || !slices.Contains(arB.ReadScopes, t16ScopeShared) || slices.Contains(arB.ReadScopes, t16ScopeA) {
		t.Fatalf("tenant B read_scopes = %v, want {t16-b, t16-shared} without t16-a", arB.ReadScopes)
	}

	// --- corpus (one connected component spanning both tenants) ---
	gInsertBlock(t, pool, t16BA, t16ScopeA, "graphtest")      // A-private
	gInsertBlock(t, pool, t16BB, t16ScopeB, "graphtest")      // B-private
	gInsertBlock(t, pool, t16BS1, t16ScopeShared, "graphtest") // shared focus
	gInsertBlock(t, pool, t16BS2, t16ScopeShared, "graphtest") // shared, only via BB
	gInsertBlock(t, pool, t16BProm, t16ScopeB, "graphtest")    // B-private, promotion target

	t16Link(t, pool, t16BS1, t16BA, "topical", 0.9, t16ScopeShared)  // A-visible neighbor (positive control)
	t16Link(t, pool, t16BS1, t16BB, "topical", 0.9, t16ScopeShared)  // bridge entry into B
	t16Link(t, pool, t16BB, t16BS2, "topical", 0.9, t16ScopeShared)  // BS2 ONLY reachable via BB
	t16Link(t, pool, t16BS1, t16BProm, "topical", 0.9, t16ScopeA)    // PROMOTION: link.scope A-visible, block.scope B-invisible

	// PROBE A (hop + promotion): tenant A's ctx_auth-resolved scopes deliver only
	// the shared focus and A's own neighbor — never B's private bridge node, never
	// the node reachable only THROUGH it, never the promotion target (gated on the
	// BLOCK scope, not the A-visible link scope).
	t.Run("EgoGraph_TenantA_HopAndPromotionIsolation", func(t *testing.T) {
		res, err := store.EgoGraph(ctx, pool, gEgoParams(t16BS1), arA.ReadScopes)
		if err != nil {
			t.Fatalf("EgoGraph(BS1) as A: %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[t16BA]; !ok {
			t.Error("tenant A lost its own visible neighbor BA")
		}
		if _, ok := nodes[t16BB]; ok {
			t.Error("LEAK: B-private BB delivered to tenant A")
		}
		if _, ok := nodes[t16BS2]; ok {
			t.Error("LEAK: BS2 is reachable only via B's private BB — must not reach A (bridge hop)")
		}
		if _, ok := nodes[t16BProm]; ok {
			t.Error("LEAK: promotion — link.scope=t16-a is A-visible but block.scope=t16-b must gate BProm out")
		}
		if len(res.Nodes) != 2 {
			t.Errorf("tenant A ego(BS1) = %d nodes, want exactly 2 (BS1, BA)", len(res.Nodes))
		}
	})

	// PROBE B (per-tenant symmetry): the SAME corpus, seen through tenant B's
	// resolved scopes, yields B's own view — BB and the BS2 behind it, the
	// promotion target (t16-b, B-visible) — but never A's private BA. Proves the
	// isolation is keyed on the resolved scope set, not on a hardcoded list.
	t.Run("EgoGraph_TenantB_SeesOwnNotForeign", func(t *testing.T) {
		res, err := store.EgoGraph(ctx, pool, gEgoParams(t16BS1), arB.ReadScopes)
		if err != nil {
			t.Fatalf("EgoGraph(BS1) as B: %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[t16BB]; !ok {
			t.Error("tenant B lost its own BB")
		}
		if _, ok := nodes[t16BS2]; !ok {
			t.Error("tenant B should reach BS2 via its visible BB")
		}
		if _, ok := nodes[t16BProm]; !ok {
			t.Error("tenant B should see BProm (t16-b, B-visible)")
		}
		if _, ok := nodes[t16BA]; ok {
			t.Error("LEAK: A-private BA delivered to tenant B")
		}
	})

	// PROBE C (overview, end-to-end): the scope-partitioned supergraph aggregated
	// under each tenant's RESOLVED scopes is scope-pure — a node's scope_mix can
	// only contain that tenant's read_scopes, never the other tenant's scope.
	t.Run("GraphOverview_ScopePureViaResolvedScopes", func(t *testing.T) {
		if _, err := overview.Rebuild(ctx, pool, 1.0); err != nil {
			t.Fatalf("overview rebuild: %v", err)
		}
		params := store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}

		rA, err := store.GraphOverview(ctx, pool, params, arA.ReadScopes)
		if err != nil {
			t.Fatalf("GraphOverview as A: %v", err)
		}
		if len(rA.Nodes) == 0 {
			t.Error("tenant A overview empty — A owns visible members, expected ≥1 cluster")
		}
		for _, n := range rA.Nodes {
			for _, s := range n.ScopeMix {
				if s != t16ScopeA && s != t16ScopeShared {
					t.Errorf("LEAK: tenant A overview scope_mix contains %q (only {t16-a,t16-shared} are A's)", s)
				}
			}
		}

		rB, err := store.GraphOverview(ctx, pool, params, arB.ReadScopes)
		if err != nil {
			t.Fatalf("GraphOverview as B: %v", err)
		}
		for _, n := range rB.Nodes {
			for _, s := range n.ScopeMix {
				if s != t16ScopeB && s != t16ScopeShared {
					t.Errorf("LEAK: tenant B overview scope_mix contains %q (only {t16-b,t16-shared} are B's)", s)
				}
			}
		}
	})

	// PROBE D (suspend → fail-closed, end-to-end): a suspended tenant's key
	// authenticates to the 060 sentinel (is_valid=false, read_scopes='{}'). That
	// EMPTY set, fed to the graph read paths, hits the T07 RequireScopes guard and
	// returns ErrNoScopes — NOT a silent 0-row read, NOT full access. This is the
	// 060 suspend-gate composed with the T07 guard; the raw empty-scopes guard
	// itself is pinned separately (failclosed_scope_integration_test.go) — here we
	// prove the SUSPEND path actually produces that empty set in the live call.
	t.Run("SuspendedTenant_FailsClosedAcrossGraphPaths", func(t *testing.T) {
		tenantS := t16Tenant(t, pool, "t16-tenant-susp", "suspended")
		t16MapScope(t, pool, "t16-susp", tenantS)
		keyS := t16Key(t, pool, "t16-key-susp", "t16-susp", nil, tenantS)

		arS, err := auth.Authenticate(ctx, pool, keyS)
		if err != nil {
			t.Fatalf("authenticate suspended: %v", err)
		}
		if arS.IsValid {
			t.Fatal("suspended-tenant key authenticated valid (060 status gate)")
		}
		if len(arS.ReadScopes) != 0 {
			t.Fatalf("suspended read_scopes = %v, want empty (060 __UNAUTHORIZED__ sentinel)", arS.ReadScopes)
		}
		if _, err := store.EgoGraph(ctx, pool, gEgoParams(t16BS1), arS.ReadScopes); !errors.Is(err, store.ErrNoScopes) {
			t.Errorf("EgoGraph(suspended scopes) err = %v, want store.ErrNoScopes (fail-closed)", err)
		}
		if _, err := store.GraphOverview(ctx, pool, store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}, arS.ReadScopes); !errors.Is(err, store.ErrNoScopes) {
			t.Errorf("GraphOverview(suspended scopes) err = %v, want store.ErrNoScopes (fail-closed)", err)
		}
	})
}
