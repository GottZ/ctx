//go:build integration

// WF T12 dream-pipeline gate (design/01-type-registry.md §7-T12, §9.6): the
// per-tenant PickBlock scope conjunct + tenant-resolved dream.linkable
// allowlist. Two isolation properties in one container:
//
//   - A's dream.linkable=false override skips A's blocks of that type (the
//     allowlist is tenant-resolved via SnapshotForTenant), while
//
//   - B's block of the SAME type stays dreamable and A's policy never governs
//     it — the pick is bound to the iterating tenant's scopes.
//
//     GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//     go test -tags=integration ./internal/dream/ -run T12 -count=1 -v
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

func insertDreamScopeBlock(t *testing.T, pool *pgxpool.Pool, id, scope, typeName string, quality float64, createdAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	emb := make([]float32, 1024)
	for i := range emb {
		emb[i] = 0.02
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, type_name, quality_score, created_at, updated_at)
		 VALUES ($1::uuid, 'learnings', $2, 'c', $3, $4, $5, $6, $7, $7)`,
		id, "t12-dream-"+id[len(id)-4:], scope, pgvec.NewVector(emb), typeName, quality, createdAt); err != nil {
		t.Fatalf("insert dream block %s: %v", id, err)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestT12_DreamPickTenantIsolation_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	const scopeA, scopeB = "t12-dream-a", "t12-dream-b"
	const idA = "019f3000-0000-7000-9000-00000000a001" // A's reference block
	const idB = "019f3000-0000-7000-9000-00000000b001" // B's reference block

	// A's block is OLDER and LOWER quality → under PickBlock's ORDER BY it would
	// be picked before B's block IF the scope conjunct were missing. That is the
	// RED lever for probe 3.
	insertDreamScopeBlock(t, pool, idA, scopeA, "reference", 0.10, time.Now().Add(-48*time.Hour))
	insertDreamScopeBlock(t, pool, idB, scopeB, "reference", 0.90, time.Now().Add(-1*time.Hour))

	// Tenant-A override: reference is NOT dream-linkable for A.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, config)
		 VALUES ('reference', $1, '{"v":1,"dream":{"linkable":false}}'::jsonb)`, scopeA); err != nil {
		t.Fatalf("insert tenant-A override: %v", err)
	}
	reg.InvalidateTenant(scopeA) // NOTIFY dispatch twin

	setA := reg.SnapshotForTenant(ctx, scopeA)
	setB := reg.SnapshotForTenant(ctx, scopeB)

	// 1. Allowlist is tenant-resolved: reference out for A, in for B.
	if containsStr(setA.DreamLinkableTypes(), "reference") {
		t.Errorf("tenant-A dream.linkable=false override not resolved; linkable=%v", setA.DreamLinkableTypes())
	}
	if !containsStr(setB.DreamLinkableTypes(), "reference") {
		t.Errorf("tenant-B should keep reference dream-linkable; linkable=%v", setB.DreamLinkableTypes())
	}

	// 2. A's cycle (A's allowlist, A's scopes): A's only block is a reference,
	//    excluded by the override ⇒ nothing pickable. Proves the override skips
	//    A's block of that type.
	gotA, err := dream.PickBlock(ctx, pool, setA.DreamLinkableTypes(), []string{scopeA}, dream.PickClaimTTL(nil))
	if err != nil {
		t.Fatalf("PickBlock(A): %v", err)
	}
	if gotA != nil {
		t.Errorf("tenant-A cycle picked %s (scope %s) — reference dreamed despite linkable=false override", gotA.ID, gotA.Scope)
	}

	// 3. B's cycle (B's allowlist, B's scopes): picks B's reference block and
	//    NEVER A's — the scope conjunct binds the pick to the iterating tenant.
	//    RED without the conjunct: A's older+lower-quality block sorts first and
	//    would be picked, so gotB.Scope would be A (foreign-tenant bleed).
	gotB, err := dream.PickBlock(ctx, pool, setB.DreamLinkableTypes(), []string{scopeB}, dream.PickClaimTTL(nil))
	if err != nil {
		t.Fatalf("PickBlock(B): %v", err)
	}
	if gotB == nil {
		t.Fatal("tenant-B cycle picked nothing — B's reference block should be dreamable")
	}
	if gotB.Scope != scopeB || gotB.ID != idB {
		t.Errorf("tenant-B cycle picked id=%s scope=%s, want id=%s scope=%s — scope conjunct missing (A's block bled into B's pick)",
			gotB.ID, gotB.Scope, idB, scopeB)
	}
}
