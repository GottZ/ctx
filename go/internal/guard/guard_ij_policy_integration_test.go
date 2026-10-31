//go:build integration

// Integration gates for wave I-J (design/02 §4.7/§5.3, §7-I-J) against a real
// PG18 testcontainer: the issue-type guard policy (guard.mode=flag,
// guard.candidates=same-scope) is resolved from the registry and threaded
// through guard.go. The unified ctx_guard_check generation (M074, wave T7)
// carries the p_same_scope_only + iterative_scan share — this file exercises
// the Go wiring around it. The 100k-filtered-ANN recall probe lives in the
// sibling file guard_ij_recall_integration_test.go.
//
// Shared helpers (t7Insert, readGuardState, unitVec1024, blendedVec1024) come
// from guard_integration_test.go / guard_t7_policy_integration_test.go — same
// guard_test package.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/guard/ -run TestIJGuardPolicy -count=1 -v
package guard_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	// Flag group (scope repo-a): a 0.99 issue duplicate pair.
	ijFlagA = "019f2210-0000-7000-9000-00000000e001"
	ijFlagB = "019f2210-0000-7000-9000-00000000e002"
	// Same-scope group: repo-a issue + an identical foreign repo-b issue.
	ijScopeHome    = "019f2210-0000-7000-9000-00000000e003"
	ijScopeForeign = "019f2210-0000-7000-9000-00000000e004"
	// Knowledge bestand group: cross-scope knowledge pair INSIDE one tenant,
	// plus an identical twin in a SECOND tenant (156 — never a candidate).
	ijKnowA = "019f2210-0000-7000-9000-00000000e005"
	ijKnowB = "019f2210-0000-7000-9000-00000000e006"
	ijKnowC = "019f2210-0000-7000-9000-00000000e007"
)

// ijSeedTenantScopes registers scopes under a fresh tenant (Modell C) so the
// 156 tenant predicate of ctx_guard_check has something to bound to.
func ijSeedTenantScopes(t *testing.T, pool *pgxpool.Pool, slug string, scopes ...string) {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	for _, sc := range scopes {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, sc, tn.ID); err != nil {
			t.Fatalf("register scope %s for %s: %v", sc, slug, err)
		}
	}
}

func TestIJGuardPolicy(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Issue policy from the DB registry (not a test literal): flag mode +
	// same-scope candidates + per-type thresholds 0.97/0.90. Since Welle I-C the
	// issue type ships as a builtin seed (migration 084 §4.1), so the row is
	// already in the DB — resolve it from the registry, do NOT re-insert it (that
	// would collide on uq_block_types_name_scope).
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	set := reg.Snapshot()

	// Sanity: the policy resolved as intended.
	if set.GuardMode("issue") != blocktype.GuardModeFlag {
		t.Fatalf("issue GuardMode = %q, want flag", set.GuardMode("issue"))
	}
	if !set.GuardSameScopeOnly("issue") {
		t.Fatal("issue GuardSameScopeOnly = false, want true")
	}

	t0 := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)

	// Flag group (scope repo-a): A at axis-10, B at cosine 0.99 to A. Both issue.
	t7Insert(t, pool, ijFlagA, "flag-a", "issue", "projects", "repo-a", unitVec1024(10, 1.0), t0)
	t7Insert(t, pool, ijFlagB, "flag-b", "issue", "projects", "repo-a", blendedVec1024(10, 11, 0.99, 0.14107), t0.Add(time.Hour))

	// Same-scope group: home issue in repo-a, an IDENTICAL foreign issue in
	// repo-b (cosine 1.0). Disjoint axis (20) from the flag group.
	t7Insert(t, pool, ijScopeHome, "scope-home", "issue", "projects", "repo-a", unitVec1024(20, 1.0), t0.Add(2*time.Hour))
	t7Insert(t, pool, ijScopeForeign, "scope-foreign", "issue", "projects", "repo-b", unitVec1024(20, 1.0), t0.Add(3*time.Hour))

	// Knowledge bestand group (axis 30): kA and kB are two scopes of ONE
	// tenant (cross-scope inside the tenant, cosine 0.995 — above the 0.98
	// duplicate threshold); kC lives in a SECOND tenant and is kA's EXACT twin
	// (cosine 1.0). Without the 156 tenant predicate kC is kA's nearest
	// neighbour (1.0 > 0.995) — GATE D's matched id turns red and GATE E's
	// clean verdict too.
	ijSeedTenantScopes(t, pool, "ij-tenant-one", "kA", "kB")
	ijSeedTenantScopes(t, pool, "ij-tenant-two", "kC")
	t7Insert(t, pool, ijKnowA, "know-a", "knowledge", "learnings", "kA", unitVec1024(30, 1.0), t0.Add(4*time.Hour))
	t7Insert(t, pool, ijKnowB, "know-b", "knowledge", "learnings", "kB", blendedVec1024(30, 31, 0.995, 0.0999), t0.Add(5*time.Hour))
	t7Insert(t, pool, ijKnowC, "know-c", "knowledge", "learnings", "kC", unitVec1024(30, 1.0), t0.Add(6*time.Hour))

	if _, err := guard.RunGuardBatch(ctx, pool, set, 100); err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}

	// GATE A (flag-only persist, NEVER archive): the 0.99 issue duplicate is
	// flagged (guard_status='possible_duplicate' + guard_matched_id), is_archived
	// stays false. RED against the auto-archive branch (guard.go archive path):
	// without the mode gate, ijFlagA.IsArchived would be true.
	t.Run("FlagOnlyPersist_NeverArchive", func(t *testing.T) {
		a := readGuardState(t, pool, ijFlagA)
		if a.IsArchived {
			t.Errorf("issue duplicate AUTO-ARCHIVED — flag mode must never set is_archived (%+v)", a)
		}
		if a.GuardStatus != "possible_duplicate" {
			t.Errorf("issue guard_status = %q, want possible_duplicate (sim=%.4f)", a.GuardStatus, a.MetaSimilarity)
		}
		if a.MetaMatchedID != ijFlagB {
			t.Errorf("issue guard_matched_id = %q, want %s", a.MetaMatchedID, ijFlagB)
		}
		if a.MetaSimilarity < 0.97 {
			t.Errorf("issue guard_similarity = %.4f, want >=0.97 (near_duplicate)", a.MetaSimilarity)
		}
		// The peer is NOT archived either (flag mode leaves both visible), so
		// when B is processed A is still a candidate → B also flags A.
		b := readGuardState(t, pool, ijFlagB)
		if b.IsArchived {
			t.Errorf("issue peer wrongly archived: %+v", b)
		}
		if b.GuardStatus != "possible_duplicate" || b.MetaMatchedID != ijFlagA {
			t.Errorf("issue peer status=%q matched=%q, want possible_duplicate/%s", b.GuardStatus, b.MetaMatchedID, ijFlagA)
		}
	})

	// GATE B (same-scope, foreign not matched): the home issue's identical twin
	// lives in a foreign scope → excluded by p_same_scope_only=TRUE. Neither
	// guard_matched_id nor a same-scope similarity surfaces the foreign block.
	// RED if checkBlock passed sameScopeOnly=false: matched would be the foreign
	// twin at sim 1.0 (cross-tenant side-channel, §5.3).
	t.Run("SameScopeOnly_ForeignNotMatched", func(t *testing.T) {
		h := readGuardState(t, pool, ijScopeHome)
		if h.MetaMatchedID == ijScopeForeign {
			t.Errorf("foreign-scope twin surfaced as guard_matched_id (sim=%.4f) — same-scope not enforced", h.MetaSimilarity)
		}
		if h.IsArchived {
			t.Errorf("same-scope home issue wrongly archived: %+v", h)
		}
		if h.GuardStatus != "clean" {
			t.Errorf("home issue guard_status = %q, want clean (only twin is foreign-scope)", h.GuardStatus)
		}
		// The foreign block, checked under its own same-scope, is likewise clean
		// and never points back cross-scope.
		f := readGuardState(t, pool, ijScopeForeign)
		if f.MetaMatchedID == ijScopeHome {
			t.Errorf("home block surfaced as foreign issue's guard_matched_id — same-scope not enforced")
		}
	})

	// GATE D (knowledge bestand byte-identical INSIDE a tenant): a knowledge
	// type keeps cross-scope matching (candidates=all default) AND auto-archive
	// (mode=archive default) AND the is_cross_scope flag — bounded to the
	// tenant's own scopes since 156. RED if the I-J change leaked
	// flag/same-scope semantics onto the knowledge line; RED against the
	// pre-156 function because kC (other tenant, cosine 1.0) would be the
	// matched id instead of kB.
	t.Run("KnowledgeBestand_CrossScopeArchive", func(t *testing.T) {
		a := readGuardState(t, pool, ijKnowA)
		if !a.IsArchived {
			t.Errorf("knowledge duplicate NOT archived — bestand auto-archive lost (%+v)", a)
		}
		if a.GuardStatus != "archived_dup" {
			t.Errorf("knowledge guard_status = %q, want archived_dup", a.GuardStatus)
		}
		if a.MetaMatchedID != ijKnowB {
			t.Errorf("knowledge guard_matched_id = %q, want %s (cross-scope match inside the tenant)", a.MetaMatchedID, ijKnowB)
		}
		if !a.MetaIsCrossScope {
			t.Errorf("knowledge guard_is_cross_scope = false, want true (kA vs kB)")
		}
	})

	// GATE E (156, GHSA §2 — never across tenants): kC is kA's exact twin in
	// another tenant. It is neither matched nor archived, and it points at
	// nobody — a tenant-two member reading kC back learns nothing about
	// tenant one. RED against the pre-156 function (measured: GATE D matched
	// kC instead of kB, so kA was archived as a duplicate of another tenant's
	// block; kC, checked after kA had left the candidate set, still carried a
	// foreign block's UUIDv7 — ijFlagA — as guard_matched_id).
	t.Run("CrossTenant_NeverACandidate", func(t *testing.T) {
		c := readGuardState(t, pool, ijKnowC)
		if c.IsArchived {
			t.Errorf("cross-tenant twin archived as a duplicate of another tenant's block: %+v", c)
		}
		if c.GuardStatus != "clean" {
			t.Errorf("cross-tenant twin guard_status = %q, want clean", c.GuardStatus)
		}
		if c.MetaMatchedID != "" {
			t.Errorf("cross-tenant twin guard_matched_id = %q, want none", c.MetaMatchedID)
		}
		if c.MetaSimilarity != 0 {
			t.Errorf("cross-tenant twin guard_similarity = %.4f, want 0 (no candidate)", c.MetaSimilarity)
		}
	})
}
