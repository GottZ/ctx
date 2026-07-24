//go:build integration

// Evokoa-Clean-Room design/03 §3.3/§7 W03-6, Migration 115 (HNSW
// ef_construction reconcile, E-03-2 = 128). Four testcontainer gates:
//
//   - G1: the historic Session-3 live drift (chain stops at the pre-115
//     Migrations-Wahrheit of ef_construction=64, index then rebuilt
//     out-of-band to 128 exactly the way Prod really got there) is
//     correctly detected as EXACTLY one definition_drift against a
//     manifest reconstructed from that same pre-115 state — then
//     Migration 115 proves a true no-op on top of it (the real Prod case:
//     reloptions already say 128, nothing to rebuild).
//   - G2: a fresh chain through 115 builds ef_construction=128 directly
//     (RED: a fresh chain stopping at 114 still builds 64).
//   - G3: a table whose planner believes it is large (reltuples>=500000,
//     i.e. it WAS analyzed) gets a RAISE WARNING instead of an inline
//     rebuild.
//   - G4: the more realistic restore path — a table that actually holds
//     >500000 rows but was NEVER analyzed (reltuples=-1, the pg_restore
//     sentinel) — is caught by the bounded-count guard instead of
//     slipping through the disproven COALESCE(reltuples,0)<500000 guard,
//     which would have triggered an hours-class inline rebuild here.
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/schemacontract/ -run TestMigration115 -v
package schemacontract

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const hnswIndexName = "idx_embedding_hnsw"

// manifestFromLiveSnapshot converts a LiveSnapshot into a Manifest with the
// same shape TestGenerateManifest (generate_test.go, genmanifest build tag)
// produces. Duplicated here rather than shared because the two files carry
// different build tags (genmanifest vs integration) and neither should
// need the other's tag to compile. Used only to reconstruct "what the
// manifest would have said as of migration 114" WITHOUT depending on the
// currently embedded manifest.json — whose ef_construction value moves
// across this very wave (W03-6 regenerates it from 64 to 128), so a test
// pinned to Embedded() could not stay meaningful on both sides of that
// regeneration.
func manifestFromLiveSnapshot(live LiveSnapshot, migMax int) Manifest {
	extensions := make(map[string]ExtSpec, len(live.Extensions))
	for name, ext := range live.Extensions {
		extensions[name] = ExtSpec{MinVersion: ext.Version}
	}
	return Manifest{
		ManifestVersion:  1,
		GeneratedAgainst: GeneratedAgainst{MigrationMax: migMax, PGMajor: live.PGMajor},
		Extensions:       extensions,
		Tables:           live.Tables,
		Indexes:          live.Indexes,
		Functions:        live.Functions,
		Triggers:         live.Triggers,
		Rules:            live.Rules,
		Hypertables:      live.Hypertables,
	}
}

// hnswRelfilenode reads idx_embedding_hnsw's current physical storage file
// identifier as text (avoids any pgx OID type-mapping assumption). ANY
// physical rebuild — DROP+CREATE or a plain REINDEX — allocates a fresh
// file and changes this value; a true no-op leaves it byte-identical. This
// is the strongest available proof "no rebuild happened" short of parsing
// server logs, and is the relfilenode-Vergleich the wave brief calls for.
func hnswRelfilenode(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var filenode string
	err := pool.QueryRow(ctx,
		`SELECT relfilenode::text FROM pg_class WHERE relname = $1`, hnswIndexName,
	).Scan(&filenode)
	if err != nil {
		t.Fatalf("hnswRelfilenode: %v", err)
	}
	return filenode
}

// hnswEfConstruction reads the live ef_construction reloption directly
// (bypassing Introspect/Diff entirely) — the most direct possible probe for
// G2/G3/G4, which are about the migration's SQL guard behavior, not the
// contract package's own classification logic (that's G1's job).
func hnswEfConstruction(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var opts []string
	err := pool.QueryRow(ctx,
		`SELECT c.reloptions FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relname = $1`, hnswIndexName,
	).Scan(&opts)
	if err != nil {
		t.Fatalf("hnswEfConstruction: query reloptions: %v", err)
	}
	for _, o := range opts {
		if v, ok := strings.CutPrefix(o, "ef_construction="); ok {
			return v
		}
	}
	t.Fatalf("hnswEfConstruction: ef_construction not present in reloptions %v", opts)
	return ""
}

// TestMigration115_HistoricDrift_ThenNoop is Gate G1. Part 1 (RED): a chain
// stopping at 114 (the pre-115 Migrations-Wahrheit, ef_construction=64
// since 001_initial.sql:250-252) with the index then rebuilt out-of-band to
// 128 — exactly how Session 3 really produced Prod's live state, per
// design/03 §2 N7 — must diff to EXACTLY one definition_drift against a
// manifest reconstructed from the pre-rebuild snapshot. Part 2 (the no-op
// proof): applying Migration 115 on top of that state changes NOTHING —
// reloptions and relfilenode both survive byte-identical, proving the
// reloptions-guard short-circuits before any DROP+CREATE.
func TestMigration115_HistoricDrift_ThenNoop(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 114)

	// Snapshot BEFORE the manual rebuild — this is byte-for-byte what a
	// manifest generated against the chain up to 114 would have declared
	// (Introspect is the same code TestGenerateManifest runs against a
	// fresh 001..114 chain).
	liveAt114, err := Introspect(ctx, pool)
	if err != nil {
		t.Fatalf("Introspect at chain=114: %v", err)
	}
	oldManifest := manifestFromLiveSnapshot(liveAt114, 114)
	if got := liveAt114.Indexes[hnswIndexName].RelOptions["ef_construction"]; got != "64" {
		t.Fatalf("setup assumption violated: chain through 114 has ef_construction=%s, want 64 (baseline for this gate no longer holds)", got)
	}

	// Simulate the historic out-of-band rebuild (Session 3): NOT through a
	// migration, exactly the Prod case the Migrations-Kette never recorded.
	if _, err := pool.Exec(ctx, `DROP INDEX `+hnswIndexName); err != nil {
		t.Fatalf("simulate manual rebuild: drop: %v", err)
	}
	if _, err := pool.Exec(ctx, `
CREATE INDEX `+hnswIndexName+`
    ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 128)`); err != nil {
		t.Fatalf("simulate manual rebuild: create: %v", err)
	}

	liveAfterManualRebuild, err := Introspect(ctx, pool)
	if err != nil {
		t.Fatalf("Introspect after manual rebuild: %v", err)
	}

	// --- RED: the drift must be visible, and be EXACTLY one finding. ---
	drifts := Diff(oldManifest, liveAfterManualRebuild)
	t.Logf("RED (G1): Diff(manifest-at-114, live-after-manual-128-rebuild) = %+v", drifts)
	if len(drifts) != 1 {
		t.Fatalf("want exactly 1 drift, got %d: %+v", len(drifts), drifts)
	}
	d := drifts[0]
	if d.Object != "index:"+hnswIndexName {
		t.Errorf("drift object = %q, want index:%s", d.Object, hnswIndexName)
	}
	if d.Class != ClassDefinitionDrift || d.Severity != SeverityParam {
		t.Errorf("drift class/severity = %s/%s, want definition_drift/param", d.Class, d.Severity)
	}
	if d.Detail != "reloptions ef_construction: manifest=64 live=128" {
		t.Errorf("drift detail = %q, want %q", d.Detail, "reloptions ef_construction: manifest=64 live=128")
	}

	// --- No-op proof: apply Migration 115 on top of the already-128 state. ---
	filenodeBefore := hnswRelfilenode(ctx, t, pool)
	optsBefore := liveAfterManualRebuild.Indexes[hnswIndexName].RelOptions

	if err := store.RunMigrationsUpTo(ctx, pool, 115); err != nil {
		t.Fatalf("applying migration 115: %v", err)
	}

	filenodeAfter := hnswRelfilenode(ctx, t, pool)
	liveAfter115, err := Introspect(ctx, pool)
	if err != nil {
		t.Fatalf("Introspect after migration 115: %v", err)
	}
	optsAfter := liveAfter115.Indexes[hnswIndexName].RelOptions

	t.Logf("GREEN (G1 no-op proof): relfilenode before=%s after=%s (identical=%v); reloptions before=%v after=%v",
		filenodeBefore, filenodeAfter, filenodeBefore == filenodeAfter, optsBefore, optsAfter)

	if filenodeBefore != filenodeAfter {
		t.Errorf("relfilenode changed (%s -> %s) — migration 115 physically rebuilt an index that was already at ef_construction=128; the reloptions-guard did not short-circuit", filenodeBefore, filenodeAfter)
	}
	if optsAfter["ef_construction"] != "128" || optsAfter["m"] != "16" {
		t.Errorf("reloptions after migration 115 = %v, want ef_construction=128 m=16 unchanged", optsAfter)
	}
}

// TestMigration115_FreshDB_Builds128 is Gate G2. RED: a fresh chain
// stopping at 114 still builds ef_construction=64 (001_initial.sql is
// untouched — 115 is the only migration that changes this). GREEN: a fresh
// chain through 115 builds ef_construction=128 directly, inline, on an
// empty table (bounded count = 0 rows, well under the 500k guard).
func TestMigration115_FreshDB_Builds128(t *testing.T) {
	ctx := context.Background()

	poolPre := testdb.SetupTestDBUpTo(t, 114)
	efPre := hnswEfConstruction(ctx, t, poolPre)
	t.Logf("RED (G2): fresh chain through 114 builds ef_construction=%s", efPre)
	if efPre != "64" {
		t.Fatalf("baseline assumption violated: chain through 114 built ef_construction=%s, want 64", efPre)
	}

	poolPost := testdb.SetupTestDBUpTo(t, 115)
	efPost := hnswEfConstruction(ctx, t, poolPost)
	t.Logf("GREEN (G2): fresh chain through 115 builds ef_construction=%s", efPost)
	if efPost != "128" {
		t.Errorf("chain through 115 built ef_construction=%s, want 128", efPost)
	}
}

// TestMigration115_LargeAnalyzedTable_WarnsInsteadOfRebuild is Gate G3: a
// table the planner believes holds >=500000 rows (a real ANALYZE outcome,
// simulated here via a direct pg_class fake — cheaper than actually
// populating and analyzing 500k rows, and G4 below already proves the real
// bulk-data path) must produce a RAISE WARNING, never an inline rebuild —
// reloptions and relfilenode both stay at the pre-115 (64) state.
func TestMigration115_LargeAnalyzedTable_WarnsInsteadOfRebuild(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 114)

	if _, err := pool.Exec(ctx, `UPDATE pg_class SET reltuples = 600000 WHERE relname = 'context_blocks'`); err != nil {
		t.Fatalf("faking reltuples: %v", err)
	}

	filenodeBefore := hnswRelfilenode(ctx, t, pool)
	efBefore := hnswEfConstruction(ctx, t, pool)

	if err := store.RunMigrationsUpTo(ctx, pool, 115); err != nil {
		t.Fatalf("applying migration 115: %v", err)
	}

	filenodeAfter := hnswRelfilenode(ctx, t, pool)
	efAfter := hnswEfConstruction(ctx, t, pool)
	t.Logf("G3: reltuples=600000 (fake, analyzed) -> ef_construction before=%s after=%s, relfilenode before=%s after=%s",
		efBefore, efAfter, filenodeBefore, filenodeAfter)

	if efAfter != "64" {
		t.Errorf("ef_construction after migration 115 = %s, want 64 (guard should WARN, not rebuild, at reltuples=600000)", efAfter)
	}
	if filenodeAfter != filenodeBefore {
		t.Errorf("relfilenode changed (%s -> %s) — migration 115 rebuilt the index despite reltuples=600000 exceeding the 500k guard", filenodeBefore, filenodeAfter)
	}
}

// TestMigration115_RestoreSimulation_UnanalyzedBoundedCountWarns is Gate
// G4 — the realistic Restore path and the ROT probe against the OLD,
// disproven guard design/03 §3.3 replaced: reltuples < 0 means "never
// analyzed" (the pg_restore sentinel, PG14+ default for a table that has
// never run ANALYZE), which the naive COALESCE(reltuples,0) < 500000 guard
// would have read as "definitely small" and triggered an hours-class
// inline rebuild on a real 10M-row restore. The real guard treats
// reltuples<0 as "unknown" and falls back to a bounded count capped at
// LIMIT 500000 — this test populates the table with genuinely more than
// 500000 rows (so the bounded count truly saturates at the cap) WITHOUT
// ever running ANALYZE, then forces reltuples=-1 to make the "never
// analyzed" state explicit and independent of autovacuum timing.
func TestMigration115_RestoreSimulation_UnanalyzedBoundedCountWarns(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 114)

	// Autovacuum must not sneak an ANALYZE in between the bulk insert and
	// the migration — that would silently turn this back into the
	// analyzed-large-table case G3 already covers, not the restore case
	// this gate targets.
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks SET (autovacuum_enabled = false)`); err != nil {
		t.Fatalf("disabling autovacuum: %v", err)
	}

	// >500000 REAL rows, no embedding (HNSW's aminsert skips NULLs, so this
	// stays fast and never touches the index), never analyzed. Loaded with
	// session_replication_role=replica (skips trg_block_write's per-row
	// pg_notify, 004_notify_triggers.sql) — this is not just a speed fix
	// (500010 individual pg_notify calls turned the naive version of this
	// insert into a 10-minute statement and blew the test's timeout budget)
	// but a MORE realistic restore simulation: pg_restore's own data-load
	// path runs with triggers disabled the same way (`--disable-triggers` /
	// session_replication_role=replica), so a real restore never fires this
	// trigger 10M times either.
	const rows = 500010
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin bulk-insert tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("set session_replication_role: %v", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO context_blocks (category, title, content)
SELECT 'restore_sim', 'restore-sim row ' || g, 'restore simulation content ' || g
  FROM generate_series(1, $1) AS g`, rows); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit bulk-insert tx: %v", err)
	}

	var liveCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks`).Scan(&liveCount); err != nil {
		t.Fatalf("counting real rows: %v", err)
	}
	if liveCount < rows {
		t.Fatalf("bulk insert assumption violated: only %d rows present, want >= %d", liveCount, rows)
	}

	// The pg_restore sentinel: never-analyzed. Forced explicitly rather
	// than relying on autovacuum timing, per the wave brief.
	if _, err := pool.Exec(ctx, `UPDATE pg_class SET reltuples = -1 WHERE relname = 'context_blocks'`); err != nil {
		t.Fatalf("forcing reltuples=-1: %v", err)
	}
	var reltuplesBefore float64
	if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE relname = 'context_blocks'`).Scan(&reltuplesBefore); err != nil {
		t.Fatalf("reading reltuples: %v", err)
	}
	t.Logf("ROT-Setup (G4): real rows=%d, pg_class.reltuples=%.0f (restore sentinel: never analyzed)", liveCount, reltuplesBefore)
	if reltuplesBefore >= 0 {
		t.Fatalf("reltuples=%.0f, want <0 (restore sentinel not in place — this would exercise G3's analyzed path, not G4's)", reltuplesBefore)
	}

	filenodeBefore := hnswRelfilenode(ctx, t, pool)
	efBefore := hnswEfConstruction(ctx, t, pool)

	if err := store.RunMigrationsUpTo(ctx, pool, 115); err != nil {
		t.Fatalf("applying migration 115: %v", err)
	}

	filenodeAfter := hnswRelfilenode(ctx, t, pool)
	efAfter := hnswEfConstruction(ctx, t, pool)
	t.Logf("GREEN (G4): ef_construction before=%s after=%s, relfilenode before=%s after=%s — bounded count (LIMIT 500000) correctly treated the unanalyzed, >500k-row table as too large to rebuild inline",
		efBefore, efAfter, filenodeBefore, filenodeAfter)

	if efAfter != "64" {
		t.Errorf("ef_construction after migration 115 = %s, want 64 (guard should WARN via the bounded-count fallback, not rebuild, on an unanalyzed >500k-row table)", efAfter)
	}
	if filenodeAfter != filenodeBefore {
		t.Errorf("relfilenode changed (%s -> %s) — migration 115 rebuilt the index on an unanalyzed >500k-row table; the OLD naive COALESCE(reltuples,0)<500000 guard would have done exactly this (an hours-class inline rebuild on a real 10M restore)", filenodeBefore, filenodeAfter)
	}
}
