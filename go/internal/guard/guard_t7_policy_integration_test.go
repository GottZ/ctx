//go:build integration

// Integration gates for wave T7 (design/01-type-registry.md §7-T7 + the
// axis-02 K1 share, design/02 §4.7/I-J) against a real PG18 testcontainer:
// ctx_guard_check is policy-parametrised (M074), the batch predicate reads
// GuardCheckTypes, the thresholds come from GuardThresholds.
//
// RED states proven before T7 was written (scratch probe on the post-T6
// tree, t7red_scratch_integration_test.go — deleted before this commit,
// output in the wave return): the 0.98 literal auto-archived a 0.985 pair
// whose type ordered threshold_duplicate=0.995; a guard.candidate=false
// type WAS matched as guard_matched_id AND was itself guard-checked; the
// legacy 1-arg ctx_guard_check call SUCCEEDED. The topic-map
// (category='index') control was green on both sides (mechanism rest).
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/guard/ -run TestT7GuardPolicy -count=1 -v
package guard_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	t7StrictA  = "019f2208-0000-7000-9000-00000000d001"
	t7StrictB  = "019f2208-0000-7000-9000-00000000d002"
	t7Anchor   = "019f2208-0000-7000-9000-00000000d003"
	t7NoCand   = "019f2208-0000-7000-9000-00000000d004"
	t7Index    = "019f2208-0000-7000-9000-00000000d005"
	t7ScopeA   = "019f2208-0000-7000-9000-00000000d006"
	t7ScopeB   = "019f2208-0000-7000-9000-00000000d007"
)

func t7Insert(t *testing.T, pool *pgxpool.Pool, id, title, typeName, category, scope string, emb []float32, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, lifecycle_state, type_name, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, 'body', $4, $5, 'knowledge', $6, $7, $7)`,
		id, category, title, scope, pgvec.NewVector(emb), typeName, at); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func TestT7GuardPolicy(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Per-type policy rows; the guard reads them via the registry (Reload
	// merges over the builtins — DB-sourced policy, not a test literal).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_block_types (name, scope, builtin, is_default, config) VALUES
		('wf-strict', '_global', false, false,
		 '{"v":1,"guard":{"check":true,"candidate":true,"threshold_duplicate":0.995,"threshold_review":0.92}}'::jsonb),
		('wf-nocand', '_global', false, false,
		 '{"v":1,"guard":{"check":false,"candidate":false}}'::jsonb)`); err != nil {
		t.Fatalf("insert registry rows: %v", err)
	}
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	set := reg.Snapshot()

	t0 := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	// wf-strict pair at cosine ~0.985 — above the old 0.98 literal, below the
	// ordered per-type duplicate threshold 0.995.
	t7Insert(t, pool, t7StrictA, "strict-a", "wf-strict", "learnings", "private", unitVec1024(0, 1.0), t0)
	t7Insert(t, pool, t7StrictB, "strict-b", "wf-strict", "learnings", "private", blendedVec1024(0, 1, 0.985, 0.17255), t0.Add(time.Hour))
	// knowledge anchor whose ONLY near neighbour is candidate-excluded (~0.99).
	t7Insert(t, pool, t7Anchor, "anchor", "knowledge", "learnings", "private", unitVec1024(2, 1.0), t0.Add(2*time.Hour))
	t7Insert(t, pool, t7NoCand, "nocand", "wf-nocand", "learnings", "private", blendedVec1024(2, 3, 0.99, 0.14107), t0.Add(3*time.Hour))
	// topic-map shape: category='index' stays outside the batch (mechanism).
	t7Insert(t, pool, t7Index, "topic-map-private", "system-meta", "index", "private", unitVec1024(4, 1.0), t0.Add(4*time.Hour))

	if _, err := guard.RunGuardBatch(ctx, pool, set, 10); err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}

	// GATE 1 (per-type threshold): the 0.985 pair is needs_review under
	// threshold_duplicate=0.995 — RED pre-M074 (0.98 literal archived it).
	t.Run("PerTypeThreshold", func(t *testing.T) {
		sA := readGuardState(t, pool, t7StrictA)
		if sA.IsArchived {
			t.Errorf("wf-strict 0.985-pair auto-archived — per-type threshold not applied")
		}
		if sA.GuardStatus != "needs_review" {
			t.Errorf("wf-strict pair status = %q, want needs_review (sim=%.4f, threshold_duplicate=0.995)",
				sA.GuardStatus, sA.MetaSimilarity)
		}
		// The persisted metadata documents the RESOLVED policy thresholds.
		var dup float64
		if err := pool.QueryRow(ctx,
			`SELECT (metadata->>'guard_threshold_duplicate')::float8 FROM context_blocks WHERE id = $1::uuid`,
			t7StrictA).Scan(&dup); err != nil {
			t.Fatalf("read threshold metadata: %v", err)
		}
		if dup != 0.995 {
			t.Errorf("persisted guard_threshold_duplicate = %v, want 0.995 (per-type value, not literal)", dup)
		}
	})

	// GATE 2 (candidate allowlist): guard.candidate=false never appears as
	// guard_matched_id; guard.check=false is never itself checked. RED
	// pre-T7 on both arms (type-blind candidate set + type-blind pick).
	t.Run("CandidateAndCheckAllowlists", func(t *testing.T) {
		sAnchor := readGuardState(t, pool, t7Anchor)
		if sAnchor.MetaMatchedID == t7NoCand {
			t.Errorf("candidate-excluded type matched as guard_matched_id (sim=%.4f)", sAnchor.MetaSimilarity)
		}
		if sAnchor.GuardStatus != "clean" {
			t.Errorf("anchor status = %q, want clean (only neighbour is candidate-excluded)", sAnchor.GuardStatus)
		}
		if s := readGuardState(t, pool, t7NoCand); s.MetaCheckedSet {
			t.Errorf("guard.check=false type was guard-checked (checked_at=%q)", s.MetaCheckedAt)
		}
	})

	// GATE 3 (topic-map mechanism rest): category='index' stays unchecked —
	// green on both sides of T7 (the category conjunct survives as code).
	t.Run("TopicMapStaysUnchecked", func(t *testing.T) {
		if s := readGuardState(t, pool, t7Index); s.MetaCheckedSet {
			t.Errorf("topic-map (category='index') was guard-checked")
		}
	})

	// GATE 4 (fail-closed call semantics): the legacy 1-arg call is a loud
	// 42883 (RED pre-M074: it succeeded); a NULL candidate array yields 0
	// candidates — never silently-all.
	t.Run("LegacyCall42883_NullCandidatesZero", func(t *testing.T) {
		var dec string
		err := pool.QueryRow(ctx, `SELECT decision FROM ctx_guard_check($1::uuid)`, t7Anchor).Scan(&dec)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42883" {
			t.Errorf("1-arg ctx_guard_check: err=%v, want SQLSTATE 42883", err)
		}
		// NULL candidates: the anchor's 0.99 neighbour exists, but the NULL
		// allowlist admits ZERO candidates ⇒ clean, no match.
		var matched *string
		if err := pool.QueryRow(ctx,
			`SELECT decision, matched_id::text FROM ctx_guard_check($1::uuid, 0.98::real, 0.92::real, NULL::text[], false)`,
			t7Anchor).Scan(&dec, &matched); err != nil {
			t.Fatalf("NULL-candidates call: %v", err)
		}
		if dec != "clean" || matched != nil {
			t.Errorf("NULL candidates: decision=%q matched=%v, want clean/none (fail-closed)", dec, matched)
		}
	})

	// GATE 5 (single signature): exactly ONE ctx_guard_check overload exists
	// (the 42725 hazard class of CREATE OR REPLACE with changed parameters).
	t.Run("ExactlyOneSignature", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_proc WHERE proname = 'ctx_guard_check'`).Scan(&n); err != nil {
			t.Fatalf("pg_proc count: %v", err)
		}
		if n != 1 {
			t.Errorf("ctx_guard_check signatures = %d, want exactly 1", n)
		}
	})

	// GATE 6 (K1 share, p_same_scope_only): TRUE restricts candidates to the
	// checked block's scope. Fixture: identical embeddings in 'private' and
	// 'work'. FALSE (knowledge line) matches cross-scope; TRUE yields clean.
	// (The 100k-filtered-ANN recall probe under iterative_scan is the I-J
	// gate of Achse 02 — here the branch semantics are pinned.)
	t.Run("SameScopeOnlyBranch", func(t *testing.T) {
		t7Insert(t, pool, t7ScopeA, "scoped-a", "knowledge", "learnings", "private", unitVec1024(5, 1.0), t0.Add(5*time.Hour))
		t7Insert(t, pool, t7ScopeB, "scoped-b", "knowledge", "learnings", "work", unitVec1024(5, 1.0), t0.Add(6*time.Hour))
		var dec string
		var matched *string
		if err := pool.QueryRow(ctx,
			`SELECT decision, matched_id::text FROM ctx_guard_check($1::uuid, 0.98::real, 0.92::real, $2::text[], false)`,
			t7ScopeA, []string{"knowledge"}).Scan(&dec, &matched); err != nil {
			t.Fatalf("cross-scope call: %v", err)
		}
		if matched == nil || *matched != t7ScopeB {
			t.Errorf("same_scope_only=false: matched=%v, want %s (bestand cross-scope semantics)", matched, t7ScopeB)
		}
		if err := pool.QueryRow(ctx,
			`SELECT decision, matched_id::text FROM ctx_guard_check($1::uuid, 0.98::real, 0.92::real, $2::text[], true)`,
			t7ScopeA, []string{"knowledge"}).Scan(&dec, &matched); err != nil {
			t.Fatalf("same-scope call: %v", err)
		}
		// The identical-embedding twin sits in 'work': under same-scope it is
		// excluded, so the top-1 falls back to an unrelated private knowledge
		// block at sim≈0 ⇒ decision clean, and the foreign block NEVER
		// surfaces as matched_id.
		if dec != "clean" {
			t.Errorf("same_scope_only=true: decision=%q, want clean (identical twin lives in a foreign scope)", dec)
		}
		if matched != nil && *matched == t7ScopeB {
			t.Errorf("same_scope_only=true: foreign-scope block surfaced as matched_id")
		}
	})

	// GATE 7 (idempotency): re-applying the raw 074 file is a no-op —
	// DROP IF EXISTS both signatures + CREATE + IF NOT EXISTS + ON CONFLICT.
	t.Run("Migration074Idempotent", func(t *testing.T) {
		raw, err := migrations.Section("074_guard_check_type_policy.sql")
		if err != nil {
			t.Fatalf("read 074 from embed FS: %v", err)
		}
		for i := 0; i < 2; i++ {
			if _, err := pool.Exec(ctx, string(raw)); err != nil {
				t.Fatalf("re-apply 074 (round %d): %v", i+1, err)
			}
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_proc WHERE proname = 'ctx_guard_check'`).Scan(&n); err != nil {
			t.Fatalf("pg_proc count: %v", err)
		}
		if n != 1 {
			t.Errorf("after 2x re-apply: ctx_guard_check signatures = %d, want 1", n)
		}
	})

	// GATE 8 (batch fail-closed wiring): a nil policy set errors loudly.
	t.Run("NilSetFailsClosed", func(t *testing.T) {
		if _, err := guard.RunGuardBatch(ctx, pool, nil, 10); err == nil ||
			!strings.Contains(err.Error(), "policy set") {
			t.Errorf("nil set: err=%v, want loud policy-set error", err)
		}
	})
}
