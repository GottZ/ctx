//go:build integration

// Stufe-2-Welle beta15 (migration fold). 113_baseline.sql replaces the
// individual migration files 001-113; these gates are the proof that the two
// paths a database can take into v5 still end in the same place:
//
//   - G1 fresh install: applying the folded SQL section by section, each in
//     its own transaction the way the runner used to, and applying it as one
//     baseline file in one transaction, build the same schema and the same
//     _migrations content. That is the transaction-shape question the fold
//     actually raises; it is NOT a proof that the sections still say what the
//     deleted files said. That proof is the checksum in migrations.Folded(),
//     computed from the files before they were removed and unreachable from
//     the baseline (migrations.TestFoldedChainIntegrity), plus the one-time
//     pg_dump comparison against the real pre-fold chain recorded in the
//     wave's commit message.
//   - G2 upgrade from the last v4.x release: a database that already carries
//     001-132 applies ONLY migration 133 — the baseline is skipped, nothing
//     is re-applied, seeded and operator data survive (RED: take version 113
//     out of _migrations and the runner refuses the database instead of
//     rebuilding over it).
//   - G3 repeatability: the runner path is a no-op on both kinds of database,
//     and the baseline is atomic — a failure inside it leaves nothing behind,
//     which is what makes a fresh install recoverable by booting again.
//
// The pre-fold chain is not gone: migrations.Section() hands back each folded
// file byte for byte out of the baseline, so these tests reconstruct a real
// v4.x database from the same artefact they are testing rather than from a
// copy of the old tree that would rot.
package schemacontract

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// lastV4Migration is the highest version the last v4.x release (v4.38.0)
// ships — the state every supported upgrade to v5 starts from. 133 is the
// v5-only migration that has to be the ONLY thing that runs on such a
// database.
const lastV4Migration = 132

// migrationRow is one _migrations row, without the wall-clock column.
type migrationRow struct {
	filename string
	checksum *string
}

// replayPreFold rebuilds a database the way the pre-fold chain built it:
// every folded migration applied from its own section, in version order,
// each in its own transaction exactly like the runner did, then the version
// ledger a v4.x runner plus BackfillChecksums left behind, then the ordinary
// embedded files up to upTo. The result is indistinguishable from a database
// that was migrated by a v4.x binary.
func replayPreFold(ctx context.Context, t *testing.T, pool *pgxpool.Pool, upTo int) {
	t.Helper()

	// Creates _migrations (the folded sections from 031 onwards record
	// themselves into it) and applies nothing else.
	if err := store.RunMigrationsUpTo(ctx, pool, 0); err != nil {
		t.Fatalf("prepare _migrations: %v", err)
	}

	for _, f := range migrations.Folded() {
		if f.Version > upTo {
			continue
		}
		body, err := migrations.Section(f.Filename)
		if err != nil {
			t.Fatalf("section %s: %v", f.Filename, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin for %s: %v", f.Filename, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply %s: %v", f.Filename, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO _migrations (version, filename, checksum) VALUES ($1, $2, $3)
			 ON CONFLICT (version) DO UPDATE SET filename = EXCLUDED.filename, checksum = EXCLUDED.checksum`,
			f.Version, f.Filename, f.Checksum,
		); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("record %s: %v", f.Filename, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %s: %v", f.Filename, err)
		}
	}

	if err := store.RunMigrationsUpTo(ctx, pool, upTo); err != nil {
		t.Fatalf("apply embedded migrations up to %d: %v", upTo, err)
	}
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("backfill checksums: %v", err)
	}
}

// readMigrationRows reads _migrations as a comparable map.
func readMigrationRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[int]migrationRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT version, filename, checksum FROM _migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	defer rows.Close()
	out := map[int]migrationRow{}
	for rows.Next() {
		var v int
		var r migrationRow
		if err := rows.Scan(&v, &r.filename, &r.checksum); err != nil {
			t.Fatalf("scan _migrations: %v", err)
		}
		out[v] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate _migrations: %v", err)
	}
	return out
}

// TestBaselineFoldEquivalence — G1.
func TestBaselineFoldEquivalence(t *testing.T) {
	ctx := context.Background()

	preFold := testdb.SetupTestDBUpTo(t, 0)
	replayPreFold(ctx, t, preFold, math.MaxInt)

	folded := testdb.SetupTestDB(t) // the baseline chain, boot sequence and all

	oldSnap, err := Introspect(ctx, preFold)
	if err != nil {
		t.Fatalf("introspect pre-fold database: %v", err)
	}
	newSnap, err := Introspect(ctx, folded)
	if err != nil {
		t.Fatalf("introspect folded database: %v", err)
	}

	// Both directions: Diff reports what the live side is missing relative to
	// the manifest, so a one-way comparison could not see an object the
	// baseline adds on top.
	if d := Diff(manifestFromLiveSnapshot(oldSnap, 133), newSnap); len(d) != 0 {
		for _, x := range d {
			t.Errorf("baseline chain drifts from pre-fold chain: %s %s — %s", x.Class, x.Object, x.Detail)
		}
	}
	if d := Diff(manifestFromLiveSnapshot(newSnap, 133), oldSnap); len(d) != 0 {
		for _, x := range d {
			t.Errorf("pre-fold chain drifts from baseline chain: %s %s — %s", x.Class, x.Object, x.Detail)
		}
	}

	// _migrations has to match too, or a later binary would disagree with
	// itself about which migrations a fresh install has seen.
	oldRows := readMigrationRows(ctx, t, preFold)
	newRows := readMigrationRows(ctx, t, folded)
	if len(oldRows) != len(newRows) {
		t.Errorf("_migrations rows: pre-fold %d, folded %d", len(oldRows), len(newRows))
	}
	for v, want := range oldRows {
		got, ok := newRows[v]
		if !ok {
			t.Errorf("version %d recorded by the pre-fold chain, missing after the fold", v)
			continue
		}
		if got.filename != want.filename {
			t.Errorf("version %d filename: pre-fold %q, folded %q", v, want.filename, got.filename)
		}
		switch {
		case got.checksum == nil && want.checksum == nil:
		case got.checksum == nil || want.checksum == nil:
			t.Errorf("version %d checksum: one side NULL (pre-fold %v, folded %v)", v, want.checksum, got.checksum)
		case *got.checksum != *want.checksum:
			t.Errorf("version %d checksum: pre-fold %s, folded %s", v, *want.checksum, *got.checksum)
		}
	}
	for v := range newRows {
		if _, ok := oldRows[v]; !ok {
			t.Errorf("version %d recorded after the fold, unknown to the pre-fold chain", v)
		}
	}

	// RED probe: the comparison above is only worth something if it can fail.
	// One index short on the folded side has to show up as drift.
	t.Run("MutatedBaselineIsDetected", func(t *testing.T) {
		victim := testdb.SetupTestDB(t)
		if _, err := victim.Exec(ctx, `DROP INDEX idx_context_ts_de`); err != nil {
			t.Fatalf("drop index for negative probe: %v", err)
		}
		mutated, err := Introspect(ctx, victim)
		if err != nil {
			t.Fatalf("introspect mutated database: %v", err)
		}
		d := Diff(manifestFromLiveSnapshot(oldSnap, 133), mutated)
		if len(d) == 0 {
			t.Fatal("dropping an index the fold must carry produced no drift — the equivalence gate cannot fail")
		}
	})
}

// TestBaselineUpgradeFromLastV4 — G2.
func TestBaselineUpgradeFromLastV4(t *testing.T) {
	ctx := context.Background()

	pool := testdb.SetupTestDBUpTo(t, 0)
	replayPreFold(ctx, t, pool, lastV4Migration)

	// Sentinel 1: a seeded row removed by hand. Migration 109 seeds it with
	// ON CONFLICT DO NOTHING, so a re-run of the baseline body would put it
	// back — its continued absence is direct evidence the body did not run.
	if _, err := pool.Exec(ctx, `DELETE FROM context_embed_models WHERE model_key = 'qwen3-embedding-8b'`); err != nil {
		t.Fatalf("remove seeded embed model: %v", err)
	}

	// Sentinel 2: migration 044 drops and re-adds context_blocks.ts_de, so a
	// re-run would move the column's attribute number. Nothing else in the
	// chain touches it.
	attnum := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT attnum FROM pg_attribute WHERE attrelid = 'context_blocks'::regclass AND attname = 'ts_de' AND NOT attisdropped`,
		).Scan(&n); err != nil {
			t.Fatalf("read ts_de attnum: %v", err)
		}
		return n
	}
	attBefore := attnum()

	// Operator data the upgrade must not touch, and one row migration 133
	// is supposed to remove.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope) VALUES ('reference', 'beta15 upgrade probe', 'survives the hop', '_global')`,
	); err != nil {
		t.Fatalf("seed operator block: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_settings (key, value, scope) VALUES ('chat.host', '"http://example.invalid"'::jsonb, '_global')`,
	); err != nil {
		t.Fatalf("seed retired settings row: %v", err)
	}

	before := readMigrationRows(ctx, t, pool)
	if _, ok := before[lastV4Migration]; !ok {
		t.Fatalf("replayed database does not carry version %d", lastV4Migration)
	}
	if _, ok := before[133]; ok {
		t.Fatal("replayed database already carries version 133")
	}

	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("upgrade run: %v", err)
	}
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("upgrade backfill: %v", err)
	}

	after := readMigrationRows(ctx, t, pool)
	if len(after) != len(before)+1 {
		t.Errorf("_migrations rows after the hop = %d, want %d (exactly migration 133)", len(after), len(before)+1)
	}
	if _, ok := after[133]; !ok {
		t.Error("migration 133 did not run")
	}
	for v, want := range before {
		got, ok := after[v]
		if !ok {
			t.Errorf("version %d disappeared across the hop", v)
			continue
		}
		if got.filename != want.filename {
			t.Errorf("version %d filename rewritten across the hop: %q -> %q", v, want.filename, got.filename)
		}
	}

	var seeded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_models WHERE model_key = 'qwen3-embedding-8b'`).Scan(&seeded); err != nil {
		t.Fatalf("re-read seeded embed model: %v", err)
	}
	if seeded != 0 {
		t.Error("a deleted seed row came back — the baseline body re-ran on a database that already had it")
	}
	if got := attnum(); got != attBefore {
		t.Errorf("context_blocks.ts_de attnum moved %d -> %d — migration 044's section re-ran", attBefore, got)
	}

	var blocks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title = 'beta15 upgrade probe'`).Scan(&blocks); err != nil {
		t.Fatalf("re-read operator block: %v", err)
	}
	if blocks != 1 {
		t.Errorf("operator block count after the hop = %d, want 1", blocks)
	}

	var retired int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_settings WHERE key = 'chat.host'`).Scan(&retired); err != nil {
		t.Fatalf("re-read retired settings row: %v", err)
	}
	if retired != 0 {
		t.Errorf("retired settings row count after the hop = %d, want 0 (migration 133)", retired)
	}

	// RED probe: the skip is keyed on version 113 and nothing else. Take that
	// row away and the same database is no longer recognizable as post-fold —
	// the runner must refuse it with the hop instruction rather than rebuild
	// on top of a live schema.
	t.Run("DatabaseBelowTheFoldIsRefused", func(t *testing.T) {
		victim := testdb.SetupTestDBUpTo(t, 0)
		replayPreFold(ctx, t, victim, lastV4Migration)
		if _, err := victim.Exec(ctx, `DELETE FROM _migrations WHERE version >= $1`, migrations.BaselineVersion); err != nil {
			t.Fatalf("remove fold-line bookkeeping: %v", err)
		}
		err := store.RunMigrations(ctx, victim)
		if err == nil {
			t.Fatal("a database without the fold line was accepted — the baseline would have been applied over a live schema")
		}
		if !errors.Is(err, store.ErrPreBaselineDatabase) {
			t.Errorf("error = %v, want the pre-baseline refusal", err)
		}
	})
}

// TestBaselineAtomicAndRepeatable — G3.
//
// Whole-chain re-application was never a property of this project and is not
// one now: measured against the pre-fold chain, 001, 003, 009, 012, 019, 020,
// 024, 025, 026, 027 and more fail outright when re-run on a fully migrated
// database (later migrations drop the columns they name). What carried that
// weight before and carries it now is the runner: it applies a version once
// and never again. The baseline adds the second half of the guarantee — it is
// a single file, so it is a single transaction, so a fresh install can only
// ever see it whole or not at all.
func TestBaselineAtomicAndRepeatable(t *testing.T) {
	ctx := context.Background()

	// (1) Fresh path: the boot sequence run twice changes nothing.
	fresh := testdb.SetupTestDB(t)
	before, err := Introspect(ctx, fresh)
	if err != nil {
		t.Fatalf("introspect fresh database: %v", err)
	}
	beforeRows := readMigrationRows(ctx, t, fresh)
	if err := store.RunMigrations(ctx, fresh); err != nil {
		t.Fatalf("second RunMigrations on a fresh database: %v", err)
	}
	if err := store.BackfillChecksums(ctx, fresh); err != nil {
		t.Fatalf("second BackfillChecksums on a fresh database: %v", err)
	}
	after, err := Introspect(ctx, fresh)
	if err != nil {
		t.Fatalf("introspect fresh database again: %v", err)
	}
	if d := Diff(manifestFromLiveSnapshot(before, 133), after); len(d) != 0 {
		for _, x := range d {
			t.Errorf("second boot changed the schema: %s %s — %s", x.Class, x.Object, x.Detail)
		}
	}
	if got := len(readMigrationRows(ctx, t, fresh)); got != len(beforeRows) {
		t.Errorf("_migrations rows after a second boot = %d, want %d", got, len(beforeRows))
	}

	// (2) Upgrade path: same, on a database that came through v4.38.0.
	upgraded := testdb.SetupTestDBUpTo(t, 0)
	replayPreFold(ctx, t, upgraded, lastV4Migration)
	if err := store.RunMigrations(ctx, upgraded); err != nil {
		t.Fatalf("upgrade run: %v", err)
	}
	rowsOnce := readMigrationRows(ctx, t, upgraded)
	if err := store.RunMigrations(ctx, upgraded); err != nil {
		t.Fatalf("second upgrade run: %v", err)
	}
	if got := len(readMigrationRows(ctx, t, upgraded)); got != len(rowsOnce) {
		t.Errorf("_migrations rows after a second upgrade run = %d, want %d", got, len(rowsOnce))
	}

	// (3) Atomicity: the baseline plus one failing statement, in the shape the
	// runner uses — one transaction per file. Nothing may survive.
	empty := testdb.SetupTestDBUpTo(t, 0)
	body, err := migrations.FS.ReadFile(migrations.BaselineFile)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	tx, err := empty.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, string(body)+"\nSELECT 1/0;\n"); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("the deliberately failing statement did not fail — the atomicity probe proves nothing")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var leftovers int
	if err := empty.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name LIKE 'context_%'`,
	).Scan(&leftovers); err != nil {
		t.Fatalf("count leftover tables: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d context_* tables survived a failed baseline — the fresh-install path is not atomic", leftovers)
	}
}

// TestBaselineFoldMigrationIntegrity — the schema contract's migration-integrity
// half. Before the fold it compared _migrations against one embedded file per
// version; now versions 001-113 are checked against migrations.Folded()
// instead. The check has to stay clean on BOTH kinds of database and it has to
// stay able to see a tampered or missing folded row — a check that tolerates
// the fold by tolerating everything below it would have quietly given up its
// oldest 112 versions.
func TestBaselineFoldMigrationIntegrity(t *testing.T) {
	ctx := context.Background()

	integrityDrifts := func(pool *pgxpool.Pool) []Drift {
		t.Helper()
		report, err := Check(ctx, pool)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		var out []Drift
		for _, d := range report.Drifts {
			if d.Class == ClassMigrationIntegrity {
				out = append(out, d)
			}
		}
		return out
	}

	t.Run("FreshInstallIsClean", func(t *testing.T) {
		if d := integrityDrifts(testdb.SetupTestDB(t)); len(d) != 0 {
			t.Errorf("fresh install: %d migration-integrity drifts, want 0: %+v", len(d), d)
		}
	})

	t.Run("UpgradedFromLastV4IsClean", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 0)
		replayPreFold(ctx, t, pool, lastV4Migration)
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("upgrade run: %v", err)
		}
		if err := store.BackfillChecksums(ctx, pool); err != nil {
			t.Fatalf("upgrade backfill: %v", err)
		}
		if d := integrityDrifts(pool); len(d) != 0 {
			t.Errorf("upgraded database: %d migration-integrity drifts, want 0: %+v", len(d), d)
		}
	})

	// The runner's gap handling — a missing lower version is re-applied even
	// though a higher one is recorded — used to be pinned on migration 077.
	// 077 is folded now, so the property moves here, onto a version that
	// still ships as its own file and has nineteen applied versions above it.
	t.Run("RunnerStillClosesGapsAboveTheFold", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		if _, err := pool.Exec(ctx, `DELETE FROM _migrations WHERE version = 114`); err != nil {
			t.Fatalf("delete version 114: %v", err)
		}
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("re-run after gap: %v", err)
		}
		var back, still133 bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = 114)`).Scan(&back); err != nil {
			t.Fatalf("re-record probe: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = 133)`).Scan(&still133); err != nil {
			t.Fatalf("133 probe: %v", err)
		}
		if !back {
			t.Error("version 114 was not re-applied — gap handling above the fold line is broken")
		}
		if !still133 {
			t.Error("version 133 vanished after the re-run")
		}
	})

	t.Run("TamperedFoldedRowIsSeen", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		if _, err := pool.Exec(ctx,
			`UPDATE _migrations SET checksum = repeat('0', 64) WHERE version = 51`); err != nil {
			t.Fatalf("tamper with version 51: %v", err)
		}
		d := integrityDrifts(pool)
		if len(d) != 1 {
			t.Fatalf("tampered folded row: %d migration-integrity drifts, want 1: %+v", len(d), d)
		}
		if d[0].Object != "_migrations:51" {
			t.Errorf("drift object = %s, want _migrations:51", d[0].Object)
		}
	})

	t.Run("MissingFoldedRowIsSeen", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		if _, err := pool.Exec(ctx, `DELETE FROM _migrations WHERE version = 51`); err != nil {
			t.Fatalf("delete version 51: %v", err)
		}
		d := integrityDrifts(pool)
		if len(d) != 1 {
			t.Fatalf("missing folded row: %d migration-integrity drifts, want 1: %+v", len(d), d)
		}
		if d[0].Object != "_migrations:51" {
			t.Errorf("drift object = %s, want _migrations:51", d[0].Object)
		}
	})
}
