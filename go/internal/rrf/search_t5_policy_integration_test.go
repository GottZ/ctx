//go:build integration

// WF T5 gates (design/01 §3.5, §5.3, §7-T5) against a real PG18
// testcontainer: the M073 ctx_rrf generation — type visibility as an
// ALLOWLIST parameter (fail-closed), damping as parallel arrays sourced from
// the block-type registry, CHECK-drop, single signature.
//
// RED states proven before M073 was written (captured in the T5 build log,
// scratch run against commit bd62847 / migrations ≤072):
//   - rogue INSERT: SQLSTATE 23514 (context_blocks_block_role_check) — the
//     CHECK, not the registry, was the only rogue barrier;
//   - after a manual CHECK drop, the rogue block WAS VISIBLE in the 071
//     ctx_rrf (exclude semantics = fail-open). Under M073 both invert:
//     the INSERT succeeds, the block is invisible.
//
//	go test -tags=integration ./internal/rrf/ -run TestT5 -count=1 -v
package rrf_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// pgvNewHalfVec builds the halfvec bind value for direct ctx_rrf calls.
func pgvNewHalfVec(emb []float32) pgvec.HalfVector { return pgvec.NewHalfVector(emb) }

// t5Registry boots a registry off the migrated test DB — every allowlist and
// damping array in these tests is DB-sourced (M072 seeds + live edits),
// never the compiled-in builtin set.
func t5Registry(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blocktype.Registry {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg
}

// TestT5_CheckDropAndRogueFailClosed is THE core probe of the axis (§5.1a):
// the M035 CHECK is gone (rogue INSERT possible and WANTED — fail-closed
// moved to the read path), and a rogue-typed block is INVISIBLE in ctx_rrf
// under allowlist semantics — including via the grant arm (§5.3).
func TestT5_CheckDropAndRogueFailClosed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := t5Registry(t, ctx, pool)
	visible := reg.Snapshot().VisibleTypes()
	emb := t40bEmbedding()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	// CHECK gone: pg_constraint no longer carries the M035 enum.
	var checkCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conname = 'context_blocks_block_role_check'`).Scan(&checkCount); err != nil {
		t.Fatalf("pg_constraint: %v", err)
	}
	if checkCount != 0 {
		t.Fatalf("context_blocks_block_role_check still exists (%d) — M073 CHECK-drop missing", checkCount)
	}

	// Rogue INSERT past Go now SUCCEEDS (pre-073: SQLSTATE 23514).
	const (
		idKnow  = "019f2205-0000-7000-9000-00000000a001"
		idRogue = "019f2205-0000-7000-9000-00000000a002"
	)
	t40bInsertBlock(t, pool, idKnow, "private", "knowledge", false, emb, now)
	t40bInsertBlock(t, pool, idRogue, "private", "rogue", false, emb, now) // t.Fatal on error

	// Allowlist semantics: rogue invisible, knowledge control visible.
	res, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", visible, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search: %v", err)
	}
	got := idSet(res)
	if got[idRogue] {
		t.Errorf("FAIL-OPEN: rogue-typed block visible under allowlist semantics; got=%v", got)
	}
	if !got[idKnow] {
		t.Errorf("knowledge control block missing; got=%v", got)
	}

	// Grant-arm bracket (§5.3): a GRANTED rogue block stays invisible — the
	// type conjuncts stand BEFORE the (scope OR grant) parens. RED if the
	// conjunct is refactored behind the bracket.
	const idForeignRogue = "019f2205-0000-7000-9000-00000000a003"
	t40bInsertBlock(t, pool, idForeignRogue, "t5-foreign", "rogue", false, emb, now)
	res, _, err = rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", visible, nil, nil, nil, nil,
		[]string{idForeignRogue}, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (grant): %v", err)
	}
	if idSet(res)[idForeignRogue] {
		t.Error("LEAK: granted rogue-typed block visible — type conjunct behind the grant bracket?")
	}

	// Orphan sweep observability (§5.1a): the reload after the INSERT warns
	// with the rogue type name (query path has no emitter by design).
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// (WARN presence is pinned in blocktype's orphan-sweep test; here we only
	// assert the reload path stays green with orphans present.)

	// RETURNS carries type_name (M073): the scanned TypeName is set.
	for _, r := range res {
		if r.ID == idKnow && r.TypeName != "knowledge" {
			t.Errorf("TypeName = %q, want knowledge (M073 RETURNS column)", r.TypeName)
		}
	}
}

// TestT5_AllowlistFailClosedHard pins both layers of the empty-allowlist
// reject: Go errors loudly (wiring-bug surface), SQL returns 0 rows on
// NULL/empty (hard fail-closed, §3.5 invariant 1).
func TestT5_AllowlistFailClosedHard(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()
	now := time.Date(2026, 7, 2, 13, 0, 0, 0, time.UTC)
	const idKnow = "019f2206-0000-7000-9000-00000000a001"
	t40bInsertBlock(t, pool, idKnow, "private", "knowledge", false, emb, now)

	// Go layer: empty allowlist = error, not empty result.
	if _, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", nil, nil, nil, nil, nil, nil, rrf.SelectorPolicy{}); err == nil {
		t.Error("empty visibleTypes accepted by rrf.Search — fail-closed guard missing")
	}
	// Mismatched damping arrays = error.
	if _, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", []string{"knowledge"},
		[]string{"audit-trail"}, nil, nil, nil, nil, rrf.SelectorPolicy{}); err == nil {
		t.Error("damped types/factors length mismatch accepted")
	}

	// SQL layer: NULL and empty-array allowlists yield 0 rows.
	for _, arrExpr := range []string{"NULL::text[]", "ARRAY[]::text[]"} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ctx_rrf($1, 'zzqqxx', 'zzqqxx', ARRAY['private'],
			         p_types_visible => `+arrExpr+`)`,
			pgvNewHalfVec(emb)).Scan(&n); err != nil {
			t.Fatalf("ctx_rrf(%s): %v", arrExpr, err)
		}
		if n != 0 {
			t.Errorf("ctx_rrf with p_types_visible=%s returned %d rows, want 0 (hard fail-closed)", arrExpr, n)
		}
	}
}

// TestT5_DampingPolicyFromDB covers the §7-T5 behaviour pair AND the
// test-level DB-sourcing probe:
//
//  1. generic query ⇒ audit-trail damped (seed 0.3) — knowledge ranks first
//     although audit-trail is the better semantic match;
//  2. intent query ("session handover") ⇒ lift — audit-trail ranks first;
//  3. SQL-UPDATE damping_factor 0.3→1.0 + registry Reload ⇒ the SAME generic
//     query now ranks audit-trail first WITHOUT a process restart — RED if
//     the arrays were sourced from the compiled-in builtin set (seeds and
//     builtins are golden-identical, so ONLY a live edit exposes that drift).
func TestT5_DampingPolicyFromDB(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := t5Registry(t, ctx, pool)
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)

	// audit-trail block = EXACT embedding match (rank 1 semantic),
	// knowledge block = slightly perturbed (rank 2). Titles/content carry no
	// FTS/trigram signal for either probe query — the semantic channel + the
	// damping factor decide the order deterministically.
	embQ := t40bEmbedding()
	embKnow := t40bEmbedding()
	for i := 0; i < 64; i++ {
		embKnow[i] = 0.05
	}
	const (
		idAudit = "019f2207-0000-7000-9000-00000000a001"
		idKnow  = "019f2207-0000-7000-9000-00000000a002"
	)
	t40bInsertBlock(t, pool, idAudit, "private", "audit-trail", false, embQ, now)
	t40bInsertBlock(t, pool, idKnow, "private", "knowledge", false, embKnow, now)

	rank := func(query string) []string {
		set := reg.Snapshot()
		damped, factors := set.DampedTypesFor(query)
		res, _, err := rrf.Search(ctx, pool, embQ, query, query,
			[]string{"private"}, nil, nil, 10, "", "", set.VisibleTypes(), damped, factors, nil, nil, nil, rrf.SelectorPolicy{})
		if err != nil {
			t.Fatalf("rrf.Search(%q): %v", query, err)
		}
		ids := make([]string, len(res))
		for i, r := range res {
			ids[i] = r.ID
		}
		return ids
	}

	// 1. Generic query: audit-trail damped 0.3 ⇒ knowledge first.
	order := rank("zzqqxx")
	if len(order) < 2 || order[0] != idKnow {
		t.Fatalf("generic query order = %v, want knowledge first (audit damped 0.3)", order)
	}

	// 2. Intent query: lift ⇒ the better semantic match (audit) first.
	order = rank("session handover zzqqxx")
	if len(order) < 2 || order[0] != idAudit {
		t.Fatalf("intent query order = %v, want audit-trail first (lift)", order)
	}

	// 3. DB-sourcing: live damping edit + Reload flips the generic order —
	//    no process restart, no new Registry instance.
	if _, err := pool.Exec(ctx,
		`UPDATE context_block_types
		    SET config = jsonb_set(config, '{retrieval,damping_factor}', '1.0')
		  WHERE name = 'audit-trail' AND scope = '_global'`); err != nil {
		t.Fatalf("damping update: %v", err)
	}
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("reload: %v", err)
	}
	order = rank("zzqqxx")
	if len(order) < 2 || order[0] != idAudit {
		t.Fatalf("post-edit generic order = %v, want audit-trail first (damping 1.0 from DB) — retrieval not DB-sourced?", order)
	}
}

// TestT5_SingleSignatureAndIdempotency: exactly ONE ctx_rrf overload exists
// (the 048-pattern DROPs removed the 071 generation — a leftover overload
// would 42725-ambiguate positional callers), and the migration file body is
// idempotent (2nd raw execution).
func TestT5_SingleSignatureAndIdempotency(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	countSignatures := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_proc WHERE proname = 'ctx_rrf'`).Scan(&n); err != nil {
			t.Fatalf("pg_proc: %v", err)
		}
		return n
	}
	if n := countSignatures(); n != 1 {
		t.Fatalf("ctx_rrf signatures = %d, want exactly 1", n)
	}

	// Signature carries the policy parameters (spot pin: p_types_visible).
	var args string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_function_identity_arguments(oid) FROM pg_proc WHERE proname = 'ctx_rrf'`).Scan(&args); err != nil {
		t.Fatalf("identity args: %v", err)
	}
	for _, want := range []string{"p_types_visible", "p_damped_types", "p_damped_factors", "p_types_exclude"} {
		if !strings.Contains(args, want) {
			t.Errorf("ctx_rrf signature misses %s (args: %s)", want, args)
		}
	}
	if strings.Contains(args, "p_audit_trail_factor") {
		t.Errorf("p_audit_trail_factor survived M073 (args: %s)", args)
	}

	// Idempotency: raw re-execution of the file body (runner skips by
	// version; the claim is about DROP IF EXISTS / CREATE OR REPLACE /
	// ON CONFLICT in the body). Since Gen 15 (M112, W02-1) the newest
	// generation lives in 112: 073's DROP list predates the 18-param
	// signature, so a raw 073 re-run on a migrated DB resurrects the Gen-14
	// overload NEXT TO Gen 15. The single-signature pin therefore asserts
	// after re-applying the current-generation file in chain order — the
	// state every fresh DB converges on. Sentinel intention unchanged:
	// each generation file re-executes cleanly, and the chain ends single-
	// signature (M112 owns its own idempotency double-run in the W02-1 G1
	// gate, gen15_w021_integration_test.go).
	for _, file := range []string{"073_rrf_policy_params.sql", "112_rrf_gen15_dual_arm.sql"} {
		sqlBytes, err := migrations.Section(file)
		if err != nil {
			t.Fatalf("read embedded migration %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("second run of %s failed (not idempotent): %v", file, err)
		}
	}
	if n := countSignatures(); n != 1 {
		t.Fatalf("ctx_rrf signatures after double-run = %d, want 1", n)
	}
}

// TestT5_ExplainSemanticChannelIndex shows the §6.2 index story at
// testcontainer scale: the semantic channel's predicate shape still rides
// the HNSW index (idx_embedding_hnsw) with the allowlist conjunct in place.
// The planner prefers a seq scan on a near-empty table, so the probe forces
// the choice off (enable_seqscan=off) — this pins INDEX USABILITY (the
// conjunct does not break the index path); the p95-regression budget on the
// live corpus + 1M synthetic set is the integrator's EXPLAIN ANALYZE run.
func TestT5_ExplainSemanticChannelIndex(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()
	now := time.Date(2026, 7, 2, 15, 0, 0, 0, time.UTC)
	for i, id := range []string{
		"019f2208-0000-7000-9000-00000000a001",
		"019f2208-0000-7000-9000-00000000a002",
		"019f2208-0000-7000-9000-00000000a003",
	} {
		e := t40bEmbedding()
		e[0] = float32(i) * 0.01
		t40bInsertBlock(t, pool, id, "private", "knowledge", false, e, now)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Three-row tables never make an ANN index the cheapest plan — force the
	// choice: no seq scan, no explicit sort. What remains for the
	// ORDER-BY-distance shape is the HNSW path (or nothing, if the new
	// allowlist conjunct broke index usability — the red case of this probe).
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_sort = off`); err != nil {
		t.Fatalf("set: %v", err)
	}
	rows, err := tx.Query(ctx, `
		EXPLAIN (ANALYZE, COSTS OFF)
		SELECT cb.id FROM context_blocks cb
		WHERE NOT cb.is_archived
		  AND cb.type_name = ANY(ARRAY['audit-trail','knowledge','reference'])
		  AND cb.scope = ANY(ARRAY['private'])
		ORDER BY cb.embedding::halfvec(1024) <=> $1
		LIMIT 75`, pgvNewHalfVec(emb))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, line)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_embedding_hnsw") {
		t.Errorf("semantic channel shape does not use idx_embedding_hnsw:\n%s", joined)
	}
	t.Logf("semantic channel plan:\n%s", joined)
}
