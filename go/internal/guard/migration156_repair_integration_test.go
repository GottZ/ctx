//go:build integration

// Migration 156 gates: the data repair that un-archives blocks the duplicate
// guard had archived against a block of ANOTHER tenant (GHSA 2026-10-31 §2).
//
// The suite seeds every pre-156 case into ONE database, applies 156 exactly
// once and reads each fixture back. As in the M121 suite, the guards only
// prove anything in each other's presence: the repair is one UPDATE, repeated
// until a pass returns nothing, and a single row un-archived into an occupied
// slot of a partial unique index over NOT is_archived is a 23505 that rolls
// the whole migration back — in prod, the boot. The file as shipped in 5.29.1/5.29.2 dies on this corpus with
// exactly that 23505; m156ShippedAborts runs it before the amended one.
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18 \
//	  go test -tags=integration ./internal/guard/ -run TestRepair_M156 -count=1 -v
package guard_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m156File = "156_guard_tenant_candidates.sql"

// m156ShippedFile is migration 156 byte for byte as 5.29.1/5.29.2 shipped it
// (migrations.TestAmendedChecksums_Provenance ties it to the amended entry).
const m156ShippedFile = "../../migrations/testdata/156_guard_tenant_candidates.c095c391.sql.txt"

// m156State is everything the repair may touch on one row, plus the audit
// marker that proves the repair (and not something else) touched it.
type m156State struct {
	archived   bool
	guard      string
	repairMark string // metadata->>'guard_repair'; "" when absent
	guardKeys  bool   // metadata ?| m156GuardKeys
}

// m156GuardKeys are the decision keys the guard writes into metadata
// (guard.go applyDecision): the archive branch writes the first five, the
// mark-checked branch the thresholds as well, and a block checked more than
// once carries all seven. A row the repair returns must have lost every one
// of them — guard_checked_at is the key guardPendingWhere reads, so a row
// that kept it would never reach the guard again.
const m156GuardKeys = `array['guard_status', 'guard_checked_at', 'guard_similarity', 'guard_matched_id',
	'guard_is_cross_scope', 'guard_threshold_duplicate', 'guard_threshold_review']`

// m156GuardMeta is the decision metadata of a row the guard archived, all
// seven keys, matched against the given bind parameter.
func m156GuardMeta(matchedParam string) string {
	return `jsonb_build_object('guard_status', 'near_duplicate', 'guard_checked_at', now()::text,
		'guard_similarity', 0.99, 'guard_matched_id', ` + matchedParam + `::text,
		'guard_is_cross_scope', true, 'guard_threshold_duplicate', 0.95,
		'guard_threshold_review', 0.85)`
}

func m156Tenant(t *testing.T, pool *pgxpool.Pool, slug string, scopes ...string) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	for _, sc := range scopes {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, sc, id); err != nil {
			t.Fatalf("register scope %s for %s: %v", sc, slug, err)
		}
	}
}

// m156Live plants a live block (a match target or a slot holder).
func m156Live(t *testing.T, pool *pgxpool.Pool, id, title, scope string) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, type_name, type_source)
		 VALUES ($1::uuid, 'learnings', $2, 'body', $3, 'knowledge', 'auto')
		 RETURNING id::text`, id, title, scope).Scan(&out); err != nil {
		t.Fatalf("seed live %q: %v", title, err)
	}
	return out
}

// m156Archived plants a row the pre-156 guard archived as a duplicate of
// matched, with the decision metadata the guard writes. resolution != ""
// adds the trace guard-resolve leaves when a person archived it.
func m156Archived(t *testing.T, pool *pgxpool.Pool, id, title, scope, matched, resolution string) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks
		   (id, category, title, content, scope, type_name, type_source, is_archived, guard_status, metadata)
		 VALUES ($1::uuid, 'learnings', $2, 'body', $3, 'knowledge', 'auto', true, 'archived_dup',
		         `+m156GuardMeta("$4")+`
		         || CASE WHEN $5 = '' THEN '{}'::jsonb
		                 ELSE jsonb_build_object('guard_resolution', $5::text) END)
		 RETURNING id::text`, id, title, scope, matched, resolution).Scan(&out); err != nil {
		t.Fatalf("seed archived %q: %v", title, err)
	}
	return out
}

// m156Chunk plants a chunk row of source (live, or archived against matched).
func m156Chunk(t *testing.T, pool *pgxpool.Pool, id, title, source string, index int, matched string) string {
	t.Helper()
	archived := matched != ""
	guard := "active"
	if archived {
		guard = "archived_dup"
	}
	var out string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks
		   (id, category, title, content, scope, type_name, type_source,
		    lifecycle_state, source_id, chunk_index, is_archived, guard_status, metadata)
		 VALUES ($1::uuid, 'learnings', $2, 'body', 's156a', 'knowledge', 'auto',
		         'chunk', $3::uuid, $4, $5, $6,
		         CASE WHEN $7 = '' THEN '{}'::jsonb
		              ELSE `+m156GuardMeta("$7")+` END)
		 RETURNING id::text`, id, title, source, index, archived, guard, matched).Scan(&out); err != nil {
		t.Fatalf("seed chunk %q: %v", title, err)
	}
	return out
}

func m156StateOf(t *testing.T, pool *pgxpool.Pool, id string) m156State {
	t.Helper()
	var s m156State
	if err := pool.QueryRow(context.Background(),
		`SELECT is_archived, COALESCE(guard_status, ''), COALESCE(metadata->>'guard_repair', ''),
		        COALESCE(metadata ?| `+m156GuardKeys+`, false)
		   FROM context_blocks WHERE id = $1::uuid`, id).
		Scan(&s.archived, &s.guard, &s.repairMark, &s.guardKeys); err != nil {
		t.Fatalf("read state of %s: %v", id, err)
	}
	return s
}

// m156Snapshot is the idempotency probe: every column the repair writes plus
// updated_at, so a second run that touched even one row cannot come back
// equal.
func m156Snapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, is_archived::text || '|' || COALESCE(guard_status, '<null>') || '|' ||
		        COALESCE(metadata::text, '<null>') || '|' || updated_at::text
		   FROM context_blocks WHERE category = 'learnings' ORDER BY id`)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	snap := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("scan snapshot row: %v", err)
		}
		snap[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return snap
}

// m156ApplyAgain re-executes the EMBEDDED file in its own transaction —
// RunMigrationsUpTo would skip the recorded version and test the runner's
// bookkeeping instead of the statement's idempotency.
func m156ApplyAgain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := migrations.FS.ReadFile(m156File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", m156File, err)
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-apply %s: %v", m156File, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit re-apply: %v", err)
	}
}

// m156ShippedAborts is the negative proof: the shipped revision, run against
// the seeded corpus in a transaction that is rolled back, must die with the
// 23505 on a partial unique index over live rows that the amendment exists
// for. It also pins the file to the checksum the schema contract accepts.
func m156ShippedAborts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	sql, err := os.ReadFile(m156ShippedFile)
	if err != nil {
		t.Fatalf("read shipped 156: %v", err)
	}
	sum := sha256.Sum256(sql)
	if checksum := hex.EncodeToString(sum[:]); !migrations.IsAmendedChecksum(156, checksum) {
		t.Fatalf("%s: sha256 %s is not an amended checksum of 156", m156ShippedFile, checksum)
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, string(sql))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" ||
		(pgErr.ConstraintName != "uq_context_category_title_scope" && pgErr.ConstraintName != "uq_source_chunk") {
		t.Fatalf("shipped 156 on the corpus: err = %v, want 23505 on uq_context_category_title_scope or uq_source_chunk", err)
	}
	t.Logf("shipped 156 on the corpus: %s (%s) on %s", pgErr.Code, pgErr.Message, pgErr.ConstraintName)
}

// TestRepair_M156_Corpus is the main gate: one database, one migration run,
// one fixture per guard.
func TestRepair_M156_Corpus(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 155)

	// Tenant one owns s156a + s156b, tenant two owns s156c; s156legacy is
	// registered to no tenant.
	m156Tenant(t, pool, "m156-one", "s156a", "s156b")
	m156Tenant(t, pool, "m156-two", "s156c")
	foreign := m156Live(t, pool, "01920000-0000-7000-8000-000000156f00", "foreign target", "s156c")
	own := m156Live(t, pool, "01920000-0000-7000-8000-000000156f01", "own target", "s156b")

	// (a) lone row archived against the foreign tenant: comes back.
	lone := m156Archived(t, pool, "01920000-0000-7000-8000-000000156a00", "lone", "s156a", foreign, "")

	// (b) the slot is held by a live row (the title was saved again after
	// the guard archived it). Un-archiving next to it is the 23505 the
	// shipped statement died on.
	slotLive := m156Live(t, pool, "01920000-0000-7000-8000-000000156b00", "slot", "s156a")
	slotArchived := m156Archived(t, pool, "01920000-0000-7000-8000-000000156b01", "slot", "s156a", foreign, "")

	// (c) two archived title twins, no live holder: only the newer (higher
	// UUIDv7) returns.
	twinOld := m156Archived(t, pool, "01920000-0000-7000-8000-000000156c01", "twin", "s156a", foreign, "")
	twinNew := m156Archived(t, pool, "01920000-0000-7000-8000-000000156c02", "twin", "s156a", foreign, "")

	// (d) a person archived it through guard-resolve: the verdict stands.
	// A stale 'keep' from an earlier review does not protect a row the guard
	// archived afterwards.
	human := m156Archived(t, pool, "01920000-0000-7000-8000-000000156d00", "human", "s156a", foreign, "archive")
	staleKeep := m156Archived(t, pool, "01920000-0000-7000-8000-000000156d01", "stale keep", "s156a", foreign, "keep")

	// (e) archived against a block of its OWN tenant: a legitimate verdict,
	// untouched.
	sameTenant := m156Archived(t, pool, "01920000-0000-7000-8000-000000156e00", "same tenant", "s156a", own, "")

	// (f) a scope registered to no tenant, matched in a tenant scope: the
	// guard now gives it same-scope candidates only, so it comes back.
	legacy := m156Archived(t, pool, "01920000-0000-7000-8000-000000156e01", "legacy", "s156legacy", foreign, "")

	// (g) chunk slots (uq_source_chunk): an archived chunk beside a live one
	// stays; of two archived chunk twins only the newer returns. The source
	// row keeps a legacy scope (chk_source_scope); the index does not look at
	// it.
	var source string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('m156.md', 'h', 'private') RETURNING id::text`).
		Scan(&source); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	chunkLive := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a10", "chunk live", source, 0, "")
	chunkSlot := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a11", "chunk slot", source, 0, foreign)
	chunkOld := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a12", "chunk twin old", source, 1, foreign)
	chunkNew := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a13", "chunk twin new", source, 1, foreign)

	// (h) the guard writes guard_matched_id = '' when it found no match
	// (guard.go applyDecision; live data carries such rows). Neither that
	// nor a malformed value may reach a uuid cast — a 22P02 rolls the boot
	// back like a 23505 — and neither names a block, so both stay.
	noMatch := m156Archived(t, pool, "01920000-0000-7000-8000-000000156a20", "no match", "s156a", "", "")
	badMatch := m156Archived(t, pool, "01920000-0000-7000-8000-000000156a21", "bad match", "s156a", "not-a-uuid", "")

	// (i) chunks whose source is gone (fk_blocks_source ON DELETE SET NULL)
	// hold no uq_source_chunk slot — the index treats NULLs as distinct —
	// so two of them at the same chunk_index do not block each other: both
	// come back.
	var goneSource string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('m156-gone.md', 'h', 'private') RETURNING id::text`).
		Scan(&goneSource); err != nil {
		t.Fatalf("seed gone source: %v", err)
	}
	orphanOld := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a30", "orphan old", goneSource, 0, foreign)
	orphanNew := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a31", "orphan new", goneSource, 0, foreign)
	if _, err := pool.Exec(ctx, `DELETE FROM context_sources WHERE id = $1::uuid`, goneSource); err != nil {
		t.Fatalf("delete gone source: %v", err)
	}

	// (j) the newest row of a title slot is a chunk whose chunk slot the live
	// chunk of (g) holds: it stays, and it must not keep the older row of
	// the same title, whose chunk slot is free, from coming back.
	crossTitleFree := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a40", "cross title", source, 2, foreign)
	crossTitleHeld := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a41", "cross title", source, 0, foreign)

	// (k) the same across the other index: the newest row of a chunk slot
	// carries the title the live row of (b) holds; the older row of that
	// chunk slot has a free title and comes back.
	crossChunkFree := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a50", "cross chunk", source, 3, foreign)
	crossChunkHeld := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a51", "slot", source, 3, foreign)

	// (l) a shadow only a second pass lifts: "chain z" (newest) takes chunk
	// slot 4 from the newer "chain" row, which therefore stays — but in that
	// same pass it still outranks the older "chain" row (chunk slot 5).
	chainOld := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a60", "chain", source, 5, foreign)
	chainNew := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a61", "chain", source, 4, foreign)
	chainZ := m156Chunk(t, pool, "01920000-0000-7000-8000-000000156a62", "chain z", source, 4, foreign)

	m156ShippedAborts(t, pool)

	pre := m156Snapshot(t, pool)
	if err := store.RunMigrationsUpTo(ctx, pool, 156); err != nil {
		t.Fatalf("apply migration 156: %v", err)
	}
	post := m156Snapshot(t, pool)

	back := m156State{archived: false, guard: "active", repairMark: "M156", guardKeys: false}
	stays := m156State{archived: true, guard: "archived_dup", repairMark: "", guardKeys: true}
	live := m156State{archived: false, guard: "active", repairMark: "", guardKeys: false}

	for _, c := range []struct {
		label string
		id    string
		want  m156State
	}{
		{"(a) lone", lone, back},
		{"(b) slot holder", slotLive, live},
		{"(b) archived beside a live twin", slotArchived, stays},
		{"(c) older twin", twinOld, stays},
		{"(c) newer twin", twinNew, back},
		{"(d) archived by a person", human, stays},
		{"(d) stale keep", staleKeep, back},
		{"(e) same-tenant duplicate", sameTenant, stays},
		{"(f) scope without tenant", legacy, back},
		{"(g) live chunk", chunkLive, live},
		{"(g) archived chunk beside a live one", chunkSlot, stays},
		{"(g) older chunk twin", chunkOld, stays},
		{"(g) newer chunk twin", chunkNew, back},
		{"(h) empty guard_matched_id", noMatch, stays},
		{"(h) malformed guard_matched_id", badMatch, stays},
		{"(i) older orphaned chunk", orphanOld, back},
		{"(i) newer orphaned chunk", orphanNew, back},
		{"(j) newest of the title, chunk slot held", crossTitleHeld, stays},
		{"(j) older of the title, chunk slot free", crossTitleFree, back},
		{"(k) newest of the chunk slot, title held", crossChunkHeld, stays},
		{"(k) older of the chunk slot, title free", crossChunkFree, back},
		{"(l) chunk slot taken in the same pass", chainNew, stays},
		{"(l) the row that took it", chainZ, back},
		{"(l) older row, second pass", chainOld, back},
		{"foreign target", foreign, live},
		{"own target", own, live},
	} {
		if got := m156StateOf(t, pool, c.id); got != c.want {
			t.Errorf("%s = %+v, want %+v", c.label, got, c.want)
		}
		// A row the guards hold back stays exactly as it was: metadata
		// byte for byte, updated_at included.
		if c.want == stays && post[c.id] != pre[c.id] {
			t.Errorf("%s was held back but changed:\n before %s\n after  %s", c.label, pre[c.id], post[c.id])
		}
	}

	// Idempotency: the embedded statement again must not touch a single row.
	before := m156Snapshot(t, pool)
	m156ApplyAgain(t, pool)
	after := m156Snapshot(t, pool)
	if len(before) != len(after) {
		t.Fatalf("second run changed the row count: %d -> %d", len(before), len(after))
	}
	for id, s := range before {
		if after[id] != s {
			t.Errorf("second run touched %s:\n before %s\n after  %s", id, s, after[id])
		}
	}
}
