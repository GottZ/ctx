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

// TestCtxRrf_BlockRole_BehaviourMatchesContract verifies M036 (Welle 40):
// ctx_rrf is block_role-aware. system-meta is hard-excluded, audit-trail is
// score-damped by 0.3, knowledge and reference both pass through unchanged.
//
// Four test blocks share identical embedding/content_times — so mass_factor
// is constant (=1.0 for all) and role_factor is the differentiating signal
// modulo per-block ROW_NUMBER tie-break in the semantic CTE (ranks 1..3 for
// the three retrievable blocks, exact assignment is non-deterministic).
//
// Contract:
//
//	A: block_role='knowledge'    → role_factor = 1.0 (full-pass)
//	B: block_role='audit-trail'  → role_factor = 0.3 (damped)
//	C: block_role='reference'    → role_factor = 1.0 (full-pass)
//	D: block_role='system-meta'  → hard-excluded (must NOT appear in result)
//
// Expected:
//
//	D not in scores map.
//	A, B, C all in scores map.
//	B < min(A, C) by a wide margin (audit-trail damped well below knowledge/
//	    reference even at the worst-case ROW_NUMBER assignment).
//	B / max(A, C) ≈ 0.3 within 0.05 (damping factor 0.3 confirmed; using max
//	    for the denominator gives a conservative ratio that is independent of
//	    the per-block rank assignment).
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

	// Contract 3: B (audit-trail) is damped below both A (knowledge) and C
	// (reference). Use min(A, C) as the conservative ceiling — even at the
	// worst-case ROW_NUMBER tie-break (B got rank 1, A/C got rank 3), the
	// 0.3 damping must still pull B below the higher-ranked role=1.0 block.
	minAC := math.Min(scores[idA], scores[idC])
	if scores[idB] >= minAC {
		t.Errorf("audit-trail not damped: B(audit-trail)=%g must be < min(A,C)=%g (A=%g, C=%g)",
			scores[idB], minAC, scores[idA], scores[idC])
	}

	// Contract 4: damping factor ≈ 0.3 within 0.05. B/max(A,C) is the
	// conservative ratio (max as denominator gives smallest ratio, which
	// corresponds to the rank-1 case for A or C). The expected damping is
	// 0.3 directly when ranks are equal; rank-tie-break shifts it slightly.
	maxAC := math.Max(scores[idA], scores[idC])
	const expectedRatio = 0.3
	gotRatio := scores[idB] / maxAC
	if math.Abs(gotRatio-expectedRatio) > 0.05 {
		t.Errorf("ratio B/max(A,C) drifted: got=%g, want≈%g (damping=0.3); A=%g, B=%g, C=%g",
			gotRatio, expectedRatio, scores[idA], scores[idB], scores[idC])
	}
}
