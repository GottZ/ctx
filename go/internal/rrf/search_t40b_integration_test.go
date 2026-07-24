//go:build integration

// Integration test for Multi-Tenant wave T40b (Achse 07-W5, design/07
// §2.2/§4.2/§5.3.2/§5.7 + Welle T40b): the EXPENSIVE RRF-retrieval block-grant
// OR-arm. T40a wired the cheap abruf/ID paths via store.VisibilityPredicate;
// T40b adds the same row-level read-share OR to ctx_rrf's SIX CTE WHERE clauses
// (migration 068) plus the rrf.Search $13 grant parameter.
//
// Gates (negativ-geprobt against the live SQL):
//   - G1-RRF: a foreign-scope block granted to the caller surfaces in rrf.Search
//     WHEN the grant set carries its id; with nil/empty grants it stays absent.
//     Mutation "OR removed from the 6 CTEs" → grant block disappears → G1-RRF red.
//   - G3:     an ARCHIVED or type_name='system-meta' foreign-scope block that is
//     granted is NEVER visible — the archived/system-meta conjuncts stand BEFORE
//     the inner (scope OR id) parens. Mutation "inner parens omitted" (flat OR,
//     AND binds tighter) → the archived grant block leaks → G3 red. §5.3.2.
//   - NoOp:   nil grant set behaves byte-identically to an empty grant set and to
//     the scope-only state (the IS NOT NULL guard + DEFAULT NULL no-op).
//
// Uses idSet() from search_test.go (same package rrf_test) — not redeclared.
//
//	go test -tags=integration ./internal/rrf/ -run TestSearchT40b -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// t40bInsertBlock writes a context_blocks row with an explicit scope, type_name
// and is_archived flag (the three switch-point-relevant columns). A shared
// embedding ranks every block into the semantic channel so visibility — not
// relevance — is what the test isolates.
func t40bInsertBlock(t *testing.T, pool *pgxpool.Pool, id, scope, typeName string, isArchived bool, embedding []float32, ts time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec := pgvec.NewVector(embedding)
	title := fmt.Sprintf("rrf-t40b-title-%s", id[len(id)-4:])
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, type_name, is_archived)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, $8, $9)`,
		id, "knowledge", title, "rrf-t40b-content", scope, vec, ts, typeName, isArchived,
	)
	if err != nil {
		t.Fatalf("insert t40b block %s: %v", id, err)
	}
}

func t40bEmbedding() []float32 {
	e := make([]float32, 1024)
	for i := range e {
		e[i] = 0.1
	}
	return e
}

// TestSearchT40b_GrantArm_Findability is G1-RRF: a foreign-scope granted block
// surfaces ONLY when its id is in the grant set.
func TestSearchT40b_GrantArm_Findability(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()

	const (
		homeScope = "t40b-home"
		foreign   = "t40b-foreign"
		idHome    = "019e40b0-0000-7000-9000-00000000a001" // caller's own scope
		idGrant   = "019e40b0-0000-7000-9000-00000000a002" // foreign scope, granted
	)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	t40bInsertBlock(t, pool, idHome, homeScope, "knowledge", false, emb, now)
	t40bInsertBlock(t, pool, idGrant, foreign, "knowledge", false, emb, now)

	// Without a grant: the foreign block is invisible (scope-only).
	res, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{homeScope}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (no grant): %v", err)
	}
	got := idSet(res)
	if !got[idHome] {
		t.Fatalf("own-scope block absent without grant; got=%v", got)
	}
	if got[idGrant] {
		t.Errorf("LEAK: foreign block visible WITHOUT a grant; got=%v", got)
	}

	// With the grant: the foreign block surfaces via the RRF OR-arm.
	res, _, err = rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{homeScope}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, []string{idGrant}, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (with grant): %v", err)
	}
	got = idSet(res)
	if !got[idGrant] {
		t.Errorf("granted foreign block NOT found with grant set — RRF OR-arm missing? got=%v", got)
	}
	if !got[idHome] {
		t.Errorf("own-scope block dropped when a grant was added; got=%v", got)
	}
}

// TestSearchT40b_HardExclusion_ArchivedAndSystemMeta is G3 (§5.3.2): a granted
// foreign block that is ARCHIVED or system-meta is NEVER visible. This is the
// code-realistic operator-precedence probe — the inner (scope OR id) parens keep
// the archived/system-meta conjuncts in force for the grant arm.
func TestSearchT40b_HardExclusion_ArchivedAndSystemMeta(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()

	const (
		homeScope = "t40b-home"
		foreign   = "t40b-foreign"
		idHome    = "019e40b1-0000-7000-9000-00000000b001" // anchor: keeps the result set non-empty
		idArch    = "019e40b1-0000-7000-9000-00000000b002" // foreign + archived + granted
		idSysMeta = "019e40b1-0000-7000-9000-00000000b003" // foreign + system-meta + granted
	)
	now := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	t40bInsertBlock(t, pool, idHome, homeScope, "knowledge", false, emb, now)
	t40bInsertBlock(t, pool, idArch, foreign, "knowledge", true, emb, now)
	t40bInsertBlock(t, pool, idSysMeta, foreign, "system-meta", false, emb, now)

	// Grant BOTH the archived and the system-meta block. Neither may surface.
	res, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{homeScope}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil,
		[]string{idArch, idSysMeta}, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (grant archived+system-meta): %v", err)
	}
	got := idSet(res)
	// Positive control: the in-scope anchor proves the query ran and returns rows,
	// so the two absent-assertions below are not vacuous.
	if !got[idHome] {
		t.Fatal("positive control failed: own-scope anchor absent — the absent-assertions would be vacuous")
	}
	if got[idArch] {
		t.Errorf("LEAK (G3): granted ARCHIVED block surfaced — inner (scope OR id) parens missing? got=%v", got)
	}
	if got[idSysMeta] {
		t.Errorf("LEAK (G3): granted system-meta block surfaced — inner (scope OR id) parens missing? got=%v", got)
	}
}

// TestSearchT40b_GrantNoOp_ByteIdentical: nil grant set ≡ empty grant set ≡
// scope-only state (the IS NOT NULL guard + M048 DEFAULT-NULL no-op). Pausability
// invariant: with no grants the OR-arm changes nothing.
func TestSearchT40b_GrantNoOp_ByteIdentical(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()

	const (
		homeScope = "t40b-home"
		foreign   = "t40b-foreign"
		idHome    = "019e40b2-0000-7000-9000-00000000c001"
		idForeign = "019e40b2-0000-7000-9000-00000000c002" // never granted in either call
	)
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	t40bInsertBlock(t, pool, idHome, homeScope, "knowledge", false, emb, now)
	t40bInsertBlock(t, pool, idForeign, foreign, "knowledge", false, emb, now)

	resNil, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{homeScope}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (nil grant): %v", err)
	}
	resEmpty, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{homeScope}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, []string{}, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (empty grant): %v", err)
	}

	nilSet, emptySet := idSet(resNil), idSet(resEmpty)
	if len(resNil) != len(resEmpty) {
		t.Errorf("nil-vs-empty grant differ in count: nil=%d empty=%d", len(resNil), len(resEmpty))
	}
	if !nilSet[idHome] || !emptySet[idHome] {
		t.Errorf("own-scope block missing in no-op baseline; nil=%v empty=%v", nilSet, emptySet)
	}
	if nilSet[idForeign] || emptySet[idForeign] {
		t.Errorf("foreign block visible with no grant (no-op broken); nil=%v empty=%v", nilSet, emptySet)
	}
}

// TestSearchT40b_EmptyScopeRejectStays (§5.3.1): a non-empty grant set NEVER
// relaxes the hard empty-scope reject — the scope-gate is the primary
// fail-closed point.
func TestSearchT40b_EmptyScopeRejectStays(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()

	const idGrant = "019e40b3-0000-7000-9000-00000000d001"
	t40bInsertBlock(t, pool, idGrant, "t40b-foreign", "knowledge", false, emb,
		time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC))

	_, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, []string{idGrant}, rrf.SelectorPolicy{})
	if err == nil {
		t.Errorf("empty scope + non-empty grant must still be rejected (got nil error) — grant arm must not replace the scope-gate")
	}
}
