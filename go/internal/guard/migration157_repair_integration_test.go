//go:build integration

// Migration 157 gates: live blocks whose guard decision names a match the
// tenant-bounded guard (156) could not have found are handed back to the
// guard (§F-§2). 156 repaired only the rows the old guard had archived.
//
// The corpus is written the way production wrote it: tenants and scopes
// through store.CreateTenant/AssignTenantScope, the guard decisions by
// RunGuardBatch under the pre-156 ctx_guard_check (the database starts at
// 155), then 156 and 157 run as on an upgrade. Edge rows the guard does not
// write by itself (empty, malformed, dangling match, person-archived) are
// planted directly. After 157 the guard runs again and must pick up exactly
// the returned rows it still checks (lifecycle knowledge) — and not the rows
// a person decided on through guard-resolve, whose verdict 157 keeps.
//
//	GOTMPDIR=/compose/n8n/.gotmp/tmp GOCACHE=/compose/n8n/.gocache \
//	  go test -tags=integration ./internal/guard/ -run TestRepair_M157 -count=1 -v
package guard_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m157File = "157_guard_leftovers_repair.sql"

// m157StalePredicate is the selection of 157 as a count: live rows whose
// existing match the 156 candidate predicate excludes. Zero after 157, and
// zero again after the guard re-checked the returned rows.
const m157StalePredicate = `
	SELECT count(*) FROM context_blocks b
	 WHERE NOT b.is_archived AND b.metadata ? 'guard_matched_id'
	   AND EXISTS (
	         SELECT 1 FROM context_blocks m
	           LEFT JOIN context_tenant_scopes mts ON mts.scope = m.scope
	           LEFT JOIN context_tenant_scopes bts ON bts.scope = b.scope
	          WHERE m.id = CASE WHEN pg_input_is_valid(b.metadata->>'guard_matched_id', 'uuid')
	                            THEN (b.metadata->>'guard_matched_id')::uuid END
	            AND m.scope <> b.scope
	            AND (bts.tenant_id IS NULL OR mts.tenant_id IS DISTINCT FROM bts.tenant_id))`

// m157Target plants a match target the guard treats as already checked, so
// the batch decides only the rows under test and targets never match each
// other.
func m157Target(t *testing.T, pool *pgxpool.Pool, id, title, scope string, axis int, at time.Time) string {
	t.Helper()
	t7Insert(t, pool, id, title, "knowledge", "learnings", scope, unitVec1024(axis, 1.0), at)
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET metadata = jsonb_build_object('guard_checked_at', now()::text) WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("mark target %s checked: %v", title, err)
	}
	return id
}

// m157Planted is a live row with hand-written guard metadata (the cases the
// guard does not produce on its own). No embedding: never pending.
func m157Planted(t *testing.T, pool *pgxpool.Pool, id, title, scope, matched, extra string) string {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name, type_source, guard_status, metadata)
		 VALUES ($1::uuid, 'learnings', $2, 'body', $3, 'knowledge', 'auto', 'needs_review',
		         jsonb_build_object('guard_status', 'needs_review', 'guard_checked_at', now()::text,
		                            'guard_similarity', 0.95, 'guard_matched_id', $4::text,
		                            'guard_is_cross_scope', true) || $5::jsonb)`,
		id, title, scope, matched, extra); err != nil {
		t.Fatalf("plant %s: %v", title, err)
	}
	return id
}

// m157KeySet lists the guard_* metadata keys of a row, sorted, comma-joined.
func m157KeySet(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var keys string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(k, ',' ORDER BY k), '')
		   FROM context_blocks b, jsonb_object_keys(b.metadata) k
		  WHERE b.id = $1::uuid AND k LIKE 'guard\_%'`, id).Scan(&keys); err != nil {
		t.Fatalf("read guard keys of %s: %v", id, err)
	}
	return keys
}

func m157Matched(t *testing.T, pool *pgxpool.Pool, id string) (status, matched string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(guard_status, ''), COALESCE(metadata->>'guard_matched_id', '') FROM context_blocks WHERE id = $1::uuid`, id).
		Scan(&status, &matched); err != nil {
		t.Fatalf("read match of %s: %v", id, err)
	}
	return status, matched
}

func TestRepair_M157_Corpus(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 155)

	// Tenant B owns s157b. private/work/shared belong to the default tenant
	// (baseline); s157legacy and s157legacy2 are registered to no tenant.
	tb, err := store.CreateTenant(ctx, pool, "m157-b", "m157-b")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, tb.ID, "s157b", nil); err != nil {
		t.Fatalf("assign scope: %v", err)
	}

	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	review := func(axis int) []float32 { return blendedVec1024(axis, axis+1, 0.96, 0.28) } // cosine 0.96: needs_review

	foreign := m157Target(t, pool, "01920000-0000-7000-8000-000000157f00", "foreign target", "s157b", 0, at(0))
	foreignLow := m157Target(t, pool, "01920000-0000-7000-8000-000000157f01", "foreign low target", "s157b", 20, at(0))
	own := m157Target(t, pool, "01920000-0000-7000-8000-000000157f02", "own target", "work", 10, at(0))
	ownPrivate := m157Target(t, pool, "01920000-0000-7000-8000-000000157f03", "own private target", "private", 30, at(0))
	legacySame := m157Target(t, pool, "01920000-0000-7000-8000-000000157f04", "legacy same target", "s157legacy", 40, at(0))
	unregistered := m157Target(t, pool, "01920000-0000-7000-8000-000000157f05", "unregistered target", "s157legacy2", 50, at(0))
	foreignKept := m157Target(t, pool, "01920000-0000-7000-8000-000000157f06", "foreign kept target", "s157b", 60, at(0))

	// Decided by the pre-156 guard (candidates across every tenant):
	// (a) needs_review in private against tenant B's block — the case 156
	// left live.
	fixture := "01920000-0000-7000-8000-000000157a00"
	t7Insert(t, pool, fixture, "fixture", "knowledge", "learnings", "private", review(0), at(1))
	// (b) clean, but the nearest neighbour it recorded is tenant B's block.
	cleanForeign := "01920000-0000-7000-8000-000000157a01"
	t7Insert(t, pool, cleanForeign, "clean foreign", "knowledge", "learnings", "private", blendedVec1024(20, 21, 0.5, 0.866), at(2))
	// (c) control: needs_review against a block of its own tenant (work).
	control := "01920000-0000-7000-8000-000000157a02"
	t7Insert(t, pool, control, "control", "knowledge", "learnings", "private", review(10), at(3))
	// (d) own scope registered to no tenant, match in private: the guard
	// gives such a scope same-scope candidates only now.
	legacy := "01920000-0000-7000-8000-000000157a03"
	t7Insert(t, pool, legacy, "legacy", "knowledge", "learnings", "s157legacy", review(30), at(4))
	// (e) control: unregistered scope, match in the same scope.
	legacyControl := "01920000-0000-7000-8000-000000157a04"
	t7Insert(t, pool, legacyControl, "legacy control", "knowledge", "learnings", "s157legacy", review(40), at(5))
	// (f) match in a scope registered to no tenant, own scope private: no
	// tenant owns the match, so the current guard cannot return it either.
	// The 156 repair predicate (inner join on the match's tenant) skips it.
	unregMatch := "01920000-0000-7000-8000-000000157a05"
	t7Insert(t, pool, unregMatch, "unregistered match", "knowledge", "learnings", "private", review(50), at(6))
	// (g) needs_review against tenant B, then kept by a person through
	// guard-resolve: the verdict must survive 157.
	kept := "01920000-0000-7000-8000-000000157a06"
	t7Insert(t, pool, kept, "kept", "knowledge", "learnings", "private", review(60), at(7))

	if n, err := guard.RunGuardBatch(ctx, pool, gitSet(), 50); err != nil || n != 7 {
		t.Fatalf("pre-156 guard batch: processed %d, err %v; want 7", n, err)
	}
	if _, err := store.GuardResolve(ctx, pool, kept, "keep", []string{"private"}); err != nil {
		t.Fatalf("guard-resolve keep: %v", err)
	}
	for _, c := range []struct {
		label, id, status, matched string
	}{
		{"(a) fixture", fixture, "needs_review", foreign},
		{"(b) clean foreign", cleanForeign, "clean", foreignLow},
		{"(c) control", control, "needs_review", own},
		{"(d) legacy", legacy, "needs_review", ownPrivate},
		{"(e) legacy control", legacyControl, "needs_review", legacySame},
		{"(f) unregistered match", unregMatch, "needs_review", unregistered},
		{"(g) kept", kept, "active", foreignKept},
	} {
		if s, m := m157Matched(t, pool, c.id); s != c.status || m != c.matched {
			t.Fatalf("corpus %s: guard wrote %s/%s, want %s/%s", c.label, s, m, c.status, c.matched)
		}
	}

	// Planted edge rows: an archived row a person archived against tenant B
	// (156 holds it back; 157 never touches archived rows), an empty and a
	// malformed match, a match that no longer exists.
	var person string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name, type_source, is_archived, guard_status, metadata)
		 VALUES ('01920000-0000-7000-8000-000000157b00', 'learnings', 'person archived', 'body', 'private', 'knowledge', 'auto',
		         true, 'archived_dup', jsonb_build_object('guard_status', 'near_duplicate', 'guard_checked_at', now()::text,
		             'guard_similarity', 0.99, 'guard_matched_id', $1::text, 'guard_is_cross_scope', true,
		             'guard_resolution', 'archive'))
		 RETURNING id::text`, foreign).Scan(&person); err != nil {
		t.Fatalf("plant person archived: %v", err)
	}
	noMatch := m157Planted(t, pool, "01920000-0000-7000-8000-000000157b01", "no match", "private", "", `{}`)
	badMatch := m157Planted(t, pool, "01920000-0000-7000-8000-000000157b02", "bad match", "private", "not-a-uuid", `{}`)
	gone := m157Planted(t, pool, "01920000-0000-7000-8000-000000157b03", "gone match", "private", "01920000-0000-7000-8000-000000157dea", `{}`)
	// Decided against tenant B while it was knowledge, canonical since: 157
	// clears the decision, the guard does not take the row up again.
	canonical := m157Planted(t, pool, "01920000-0000-7000-8000-000000157b04", "canonical foreign", "private", foreign, `{}`)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET lifecycle_state = 'canonical' WHERE id = $1::uuid`, canonical); err != nil {
		t.Fatalf("make canonical: %v", err)
	}
	// A live row carrying a person's archive verdict next to a foreign match
	// (the 5.29.1/5.29.2 revision of 156 un-archived such rows): any trace of
	// a person takes the narrow branch, not only keep.
	traced := m157Planted(t, pool, "01920000-0000-7000-8000-000000157b05", "archive trace", "private", foreign,
		`{"guard_resolution": "archive", "guard_resolved_at": "2026-10-01T00:00:00Z"}`)

	// 156 as shipped today: the live rows keep their foreign decisions.
	if err := store.RunMigrationsUpTo(ctx, pool, 156); err != nil {
		t.Fatalf("apply migration 156: %v", err)
	}
	if s, m := m157Matched(t, pool, fixture); s != "needs_review" || m != foreign {
		t.Fatalf("after 156 the fixture is %s/%s; the corpus no longer shows the leftover", s, m)
	}
	var stale int
	if err := pool.QueryRow(ctx, m157StalePredicate).Scan(&stale); err != nil || stale != 7 {
		t.Fatalf("stale rows before 157 = %d (err %v), want 7", stale, err)
	}

	pre := m156Snapshot(t, pool)
	if err := store.RunMigrationsUpTo(ctx, pool, 157); err != nil {
		t.Fatalf("apply migration 157: %v", err)
	}
	post := m156Snapshot(t, pool)

	back := m156State{archived: false, guard: "active", repairMark: "M157", guardKeys: false}
	for _, c := range []struct {
		label string
		id    string
		want  *m156State // nil: byte-identical to before 157
	}{
		{"(a) fixture", fixture, &back},
		{"(b) clean foreign", cleanForeign, &back},
		{"(c) control", control, nil},
		{"(d) legacy", legacy, &back},
		{"(e) legacy control", legacyControl, nil},
		{"(f) unregistered match", unregMatch, &back},
		{"person archived", person, nil},
		{"empty guard_matched_id", noMatch, nil},
		{"malformed guard_matched_id", badMatch, nil},
		{"match gone", gone, nil},
		{"canonical, decided as knowledge", canonical, &back},
		{"foreign target", foreign, nil},
		{"foreign low target", foreignLow, nil},
		{"own target", own, nil},
		{"own private target", ownPrivate, nil},
		{"legacy same target", legacySame, nil},
		{"unregistered target", unregistered, nil},
	} {
		if c.want == nil {
			if post[c.id] != pre[c.id] {
				t.Errorf("%s changed:\n before %s\n after  %s", c.label, pre[c.id], post[c.id])
			}
			continue
		}
		if got := m156StateOf(t, pool, c.id); got != *c.want {
			t.Errorf("%s = %+v, want %+v", c.label, got, *c.want)
		}
		if got := m157KeySet(t, pool, c.id); got != "guard_repair" {
			t.Errorf("%s keeps guard keys %q, want only guard_repair", c.label, got)
		}
	}

	// A person's verdict stands: only the keys that point at the foreign
	// block go. guard_checked_at stays, so the row is not pending again.
	personVerdict := []struct {
		label, id, status, keys, resolution string
	}{
		{"(g) kept", kept, "active",
			"guard_checked_at,guard_repair,guard_resolution,guard_resolved_at,guard_status,guard_threshold_duplicate,guard_threshold_review", "keep"},
		{"archive trace", traced, "needs_review",
			"guard_checked_at,guard_repair,guard_resolution,guard_resolved_at,guard_status", "archive"},
	}
	checkVerdicts := func(when string) {
		t.Helper()
		for _, c := range personVerdict {
			var status, resolution, mark string
			if err := pool.QueryRow(ctx,
				`SELECT COALESCE(guard_status, ''), COALESCE(metadata->>'guard_resolution', ''), COALESCE(metadata->>'guard_repair', '')
				   FROM context_blocks WHERE id = $1::uuid AND NOT is_archived`, c.id).Scan(&status, &resolution, &mark); err != nil {
				t.Fatalf("%s %s: %v", when, c.label, err)
			}
			if status != c.status || resolution != c.resolution || mark != "M157" {
				t.Errorf("%s %s: guard_status %q, guard_resolution %q, guard_repair %q; want %q, %q, M157",
					when, c.label, status, resolution, mark, c.status, c.resolution)
			}
			if got := m157KeySet(t, pool, c.id); got != c.keys {
				t.Errorf("%s %s: guard keys\n got  %s\n want %s", when, c.label, got, c.keys)
			}
		}
	}
	checkVerdicts("after 157:")
	if err := pool.QueryRow(ctx, m157StalePredicate).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale rows after 157 = %d (err %v), want 0", stale, err)
	}

	// Idempotency: the embedded statement again changes nothing.
	sql, err := migrations.FS.ReadFile(m157File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", m157File, err)
	}
	tag, err := pool.Exec(ctx, string(sql))
	if err != nil {
		t.Fatalf("re-apply %s: %v", m157File, err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("second run of %s updated %d rows, want 0", m157File, tag.RowsAffected())
	}
	again := m156Snapshot(t, pool)
	for id, s := range post {
		if again[id] != s {
			t.Errorf("second run touched %s:\n before %s\n after  %s", id, s, again[id])
		}
	}

	// The returned knowledge rows are pending again: the next guard batch
	// takes exactly those four, under the tenant-bounded function, and no
	// live row is left with a match the guard could not have found. The
	// canonical row is not pending and stays without a decision; the kept
	// row (knowledge, with an embedding) is not pending either and keeps its
	// verdict.
	if n, err := guard.RunGuardBatch(ctx, pool, gitSet(), 50); err != nil || n != 4 {
		t.Fatalf("guard batch after 157: processed %d, err %v; want the 4 returned knowledge rows", n, err)
	}
	checkVerdicts("after the re-check:")
	if got := m156StateOf(t, pool, canonical); got != back {
		t.Errorf("canonical row after the batch = %+v, want %+v", got, back)
	}
	for _, id := range []string{fixture, cleanForeign, legacy, unregMatch} {
		if s, m := m157Matched(t, pool, id); s == "active" || m == foreign || m == foreignLow {
			t.Errorf("re-check of %s: guard_status %q, match %q", id, s, m)
		}
	}
	if err := pool.QueryRow(ctx, m157StalePredicate).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale rows after the re-check = %d (err %v), want 0", stale, err)
	}
}
