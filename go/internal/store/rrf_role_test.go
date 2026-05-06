//go:build integration

package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/testdb"
)

// insertRoleTestBlock writes a context_blocks row with shared embedding but a
// per-id unique title — the unique constraint uq_context_category_title_scope
// would reject identical titles. The trigram channel gates similarity > 0.05,
// so the variant suffix in titles still keeps trigram-rank identical across
// blocks for our zzqqxx-miss query.
func insertRoleTestBlock(t *testing.T, pool *pgxpool.Pool, id string, embedding []float32, ts time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec := pgvec.NewVector(embedding)
	title := fmt.Sprintf("rrf-role-test-title-%s", id[len(id)-1:])
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, content_times)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, ARRAY[$7]::timestamptz[])`,
		id, "test_role", title, "rrf-role-test-content",
		"private", vec, ts,
	)
	if err != nil {
		t.Fatalf("insert role-test block %s: %v", id, err)
	}
}

// TestCtxRrf_BlockRole_BehaviourMatchesContract verifies M038 (Welle 40 HOLD):
// ctx_rrf is block_role-aware. system-meta is hard-excluded; knowledge,
// audit-trail and reference all full-pass with role_factor=1.0.
//
// Background: M036 (Welle 40 it.2) introduced damping=0.3 for audit-trail.
// Iter-3 + Iter-4 bench (damping 0.3 / 0.5) both regressed -7pp identically
// (5 NEG flips at explicit-audit-trail-target queries). M038 reverts damping
// to 1.0 (no-op) while keeping the block_role schema for forwards-compat.
// Folge-Welle 41+ shall implement query-aware damping.
//
// Four test blocks share identical embedding/content_times — so mass_factor
// is constant (=1.0 for all) and the only differentiating signal is whether
// the block is hard-excluded (system-meta) or retained.
//
// Contract:
//
//	A: block_role='knowledge'    → role_factor = 1.0 (full-pass)
//	B: block_role='audit-trail'  → role_factor = 1.0 (M038 HOLD: no damping)
//	C: block_role='reference'    → role_factor = 1.0 (full-pass)
//	D: block_role='system-meta'  → hard-excluded (must NOT appear in result)
//
// Expected:
//
//	D not in scores map.
//	A, B, C all in scores map.
//	B / max(A, C) ≈ 1.0 within 0.1 (no damping in HOLD state).
func TestCtxRrf_BlockRole_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Single shared embedding (1024 floats, all 0.1 — unit-direction-like).
	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}

	// All four blocks: identical title and content, distinct ids. UUID prefix
	// 019d0040 differentiates from rrf_mass_test.go (019d0030).
	const (
		idA = "019d0040-0000-7000-9000-00000000000a"
		idB = "019d0040-0000-7000-9000-00000000000b"
		idC = "019d0040-0000-7000-9000-00000000000c"
		idD = "019d0040-0000-7000-9000-00000000000d"
	)

	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	insertRoleTestBlock(t, pool, idA, embedding, now)
	insertRoleTestBlock(t, pool, idB, embedding, now)
	insertRoleTestBlock(t, pool, idC, embedding, now)
	insertRoleTestBlock(t, pool, idD, embedding, now)

	// Differentiate block_role per block. M035 default is 'knowledge', so A
	// could be left untouched — explicit UPDATE for symmetry and read-clarity.
	for _, p := range []struct {
		id   string
		role string
	}{
		{idA, "knowledge"},
		{idB, "audit-trail"},
		{idC, "reference"},
		{idD, "system-meta"},
	} {
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET block_role = $1 WHERE id = $2::uuid`,
			p.role, p.id,
		); err != nil {
			t.Fatalf("set block_role=%s on %s: %v", p.role, p.id, err)
		}
	}

	// Run ctx_rrf with limit=10 (we only inserted 4 blocks). Use the same
	// FTS-miss query as rrf_mass_test.go ("zzqqxx") so all signal goes
	// through the semantic channel — identical embedding, identical title+
	// content guarantees the four blocks are semantic-rank-ties modulo
	// per-block ROW_NUMBER ordering.
	hv := pgvec.NewHalfVector(embedding)
	rows, err := pool.Query(ctx,
		`SELECT id::text, rrf_score FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9)`,
		hv, "zzqqxx", "zzqqxx", []string{"private"}, nil, nil, 10, nil, nil,
	)
	if err != nil {
		t.Fatalf("query ctx_rrf: %v", err)
	}
	scores := map[string]float64{}
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			rows.Close()
			t.Fatalf("scan rrf row: %v", err)
		}
		scores[id] = score
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}

	// Contract 1: D (system-meta) must be hard-excluded.
	if _, ok := scores[idD]; ok {
		t.Errorf("system-meta block D leaked into result: scores=%v", scores)
	}

	// Contract 2: A, B, C must all appear.
	for _, id := range []string{idA, idB, idC} {
		if _, ok := scores[id]; !ok {
			t.Fatalf("ctx_rrf missing block %s in result; got=%v", id, scores)
		}
	}

	// Contract 3: M038 HOLD — no damping. B(audit-trail) must be in the same
	// score-band as A(knowledge) and C(reference). With identical mass_factor
	// and role_factor=1.0 across all retained blocks, the only differentiator
	// is the per-block ROW_NUMBER tie-break in the semantic CTE (ranks 1..3).
	// B/max(A,C) ≈ 1.0 within 0.1 — the 0.1 tolerance covers the worst-case
	// rank-1-vs-rank-3 differential (~ 1/61 vs 1/63 = 0.969).
	maxAC := math.Max(scores[idA], scores[idC])
	const expectedRatio = 1.0
	gotRatio := scores[idB] / maxAC
	if math.Abs(gotRatio-expectedRatio) > 0.1 {
		t.Errorf("M038 HOLD: ratio B/max(A,C) drifted: got=%g, want≈%g (no damping); A=%g, B=%g, C=%g",
			gotRatio, expectedRatio, scores[idA], scores[idB], scores[idC])
	}
}
