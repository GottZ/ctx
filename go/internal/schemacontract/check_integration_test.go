//go:build integration

package schemacontract

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestCheck_FreshDB_NoDrift is the live cousin of the pure nullbeweis test
// (diff_test.go): running the FULL Check pipeline (Introspect + Diff +
// migration-integrity + GUC probes) against a freshly migrated testcontainer
// must come back clean. It is the strongest available proof that
// manifest.json actually matches what store.RunMigrations produces — the
// pure test only proves Diff's classification logic is internally
// consistent, not that Introspect's SQL agrees with the checked-in file.
func TestCheck_FreshDB_NoDrift(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	report, err := Check(ctx, pool)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Status != StatusOK {
		t.Errorf("Status = %s, want ok — drifts: %+v", report.Status, report.Drifts)
	}
	if len(report.Drifts) != 0 {
		t.Errorf("want 0 drifts against a fresh DB, got %d: %+v", len(report.Drifts), report.Drifts)
	}
	if report.ExcludedSnapshotTables != 0 {
		t.Errorf("ExcludedSnapshotTables = %d, want 0 on a fresh DB with no hand-added tables", report.ExcludedSnapshotTables)
	}
	m := Embedded()
	if report.LiveMax != m.GeneratedAgainst.MigrationMax {
		t.Errorf("LiveMax = %d, want %d (manifest max) — testdb should apply the full chain", report.LiveMax, m.GeneratedAgainst.MigrationMax)
	}
}

// TestCheck_DefinitionDrift_FunctionBodyPatch: a psql-Exec CREATE OR REPLACE
// of a declared contract function (design/03 §7 W03-2 Gate 2, bullet 1)
// must surface as definition_drift/param — a MODIFIED body is not the same
// threat class as an UNDECLARED function (that's unknown_active_object/
// breaking, proven in diff_test.go).
func TestCheck_DefinitionDrift_FunctionBodyPatch(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const patched = `
CREATE OR REPLACE FUNCTION notify_block_write() RETURNS trigger AS $$
BEGIN
    -- tampered body (W03-2 Gate 2 probe): drops the payload, changes the channel.
    PERFORM pg_notify('ctx_tampered_channel', 'x');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;`
	if _, err := pool.Exec(ctx, patched); err != nil {
		t.Fatalf("patching notify_block_write: %v", err)
	}

	report, err := Check(ctx, pool)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Status != StatusDrift {
		t.Fatalf("Status = %s, want drift", report.Status)
	}
	found := findDrift(report.Drifts, "function:notify_block_write()")
	if found == nil {
		t.Fatalf("no drift for function:notify_block_write() — drifts: %+v", report.Drifts)
	}
	if found.Class != ClassDefinitionDrift || found.Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want definition_drift/param", found.Class, found.Severity)
	}
}

// TestCheck_MigrationIntegrity_ChecksumTamper: an UPDATE against an already
// applied _migrations row (design/03 §7 W03-2 Gate 2, bullet 2 — "getamperter
// checksum") must surface as migration_integrity/breaking. This is the
// scenario the checksum column exists specifically to catch: an already
// -applied SQL file edited after the fact, invisible to everything before
// W03-1/W03-2 (design/03 §1.3).
func TestCheck_MigrationIntegrity_ChecksumTamper(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tampered := strings.Repeat("d", 64)
	if _, err := pool.Exec(ctx, `UPDATE _migrations SET checksum = $1 WHERE version = 1`, tampered); err != nil {
		t.Fatalf("tampering checksum: %v", err)
	}

	report, err := Check(ctx, pool)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Status != StatusDrift {
		t.Fatalf("Status = %s, want drift", report.Status)
	}
	found := findDrift(report.Drifts, "_migrations:1")
	if found == nil {
		t.Fatalf("no drift for _migrations:1 — drifts: %+v", report.Drifts)
	}
	if found.Class != ClassMigrationIntegrity || found.Severity != SeverityBreaking {
		t.Errorf("got class=%s severity=%s, want migration_integrity/breaking", found.Class, found.Severity)
	}
}

// TestCheck_HandAddedObjects_ClassificationAndSnapshotExceptionScope covers
// design/03 §7 W03-2 Gate 2 bullets 3-5 in one container: a hand-added
// table (unknown_object/param), a hand-added trigger on a contract table
// (unknown_active_object/breaking — the rot-probe: a pauschale
// param-classification would put this at param, tearing the assertion
// below), and the *_snapshot_* exception's precise scope (a snapshot-named
// TABLE is excluded and counted; a snapshot-named TRIGGER and a
// snapshot-named FUNCTION are still reported at full severity, because the
// exception is drawn around relkind='r' tables only — design/03 §4.4).
func TestCheck_HandAddedObjects_ClassificationAndSnapshotExceptionScope(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	ddls := []string{
		`CREATE TABLE hand_added_probe_table (id int)`,
		`CREATE TABLE context_dream_links_snapshot_20260423_prev5 (id int)`,
		`CREATE TRIGGER trg_evil_audit AFTER INSERT ON context_settings FOR EACH ROW EXECUTE FUNCTION notify_block_write()`,
		`CREATE TRIGGER trg_audit_snapshot_probe AFTER INSERT ON context_blocks FOR EACH ROW EXECUTE FUNCTION notify_block_write()`,
		`CREATE FUNCTION restore_snapshot_helper() RETURNS void AS $$ BEGIN END; $$ LANGUAGE plpgsql`,
	}
	for _, ddl := range ddls {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("DDL %q: %v", ddl, err)
		}
	}

	report, err := Check(ctx, pool)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Status != StatusDrift {
		t.Fatalf("Status = %s, want drift", report.Status)
	}

	// (3) hand-added table -> unknown_object / param.
	handTable := findDrift(report.Drifts, "table:hand_added_probe_table")
	if handTable == nil {
		t.Fatalf("no drift for table:hand_added_probe_table — drifts: %+v", report.Drifts)
	}
	if handTable.Class != ClassUnknownObject || handTable.Severity != SeverityParam {
		t.Errorf("hand table: got class=%s severity=%s, want unknown_object/param", handTable.Class, handTable.Severity)
	}

	// snapshot-named TABLE must be excluded from the drift list entirely...
	if excl := findDrift(report.Drifts, "table:context_dream_links_snapshot_20260423_prev5"); excl != nil {
		t.Errorf("snapshot-named table must be excluded, found drift: %+v", excl)
	}
	// ...but counted, never silently (design/03 §4.4: "Drift bleibt laut,
	// schließt die eigenen Blind Spots ein").
	if report.ExcludedSnapshotTables != 1 {
		t.Errorf("ExcludedSnapshotTables = %d, want 1", report.ExcludedSnapshotTables)
	}

	// (4) hand-added trigger on a contract table -> unknown_active_object /
	// breaking. ROT-PROBE: if the classifier routed unknown triggers through
	// the same pauschale param path as unknown tables, Severity would read
	// "param" here and this assertion tears.
	handTrigger := findDrift(report.Drifts, "trigger:context_settings.trg_evil_audit")
	if handTrigger == nil {
		t.Fatalf("no drift for trigger:context_settings.trg_evil_audit — drifts: %+v", report.Drifts)
	}
	if handTrigger.Class != ClassUnknownActiveObject || handTrigger.Severity != SeverityBreaking {
		t.Errorf("hand trigger: got class=%s severity=%s, want unknown_active_object/breaking (a pauschale param-classification would tear this)",
			handTrigger.Class, handTrigger.Severity)
	}

	// (5) snapshot-NAMED trigger/function are still reported at full
	// severity — the exception's scope stops at relkind='r' tables.
	snapTrigger := findDrift(report.Drifts, "trigger:context_blocks.trg_audit_snapshot_probe")
	if snapTrigger == nil {
		t.Fatalf("snapshot-named trigger must still be reported — drifts: %+v", report.Drifts)
	}
	if snapTrigger.Class != ClassUnknownActiveObject || snapTrigger.Severity != SeverityBreaking {
		t.Errorf("snapshot-named trigger: got class=%s severity=%s, want unknown_active_object/breaking", snapTrigger.Class, snapTrigger.Severity)
	}
	snapFunc := findDrift(report.Drifts, "function:restore_snapshot_helper()")
	if snapFunc == nil {
		t.Fatalf("snapshot-named function must still be reported — drifts: %+v", report.Drifts)
	}
	if snapFunc.Class != ClassUnknownActiveObject || snapFunc.Severity != SeverityBreaking {
		t.Errorf("snapshot-named function: got class=%s severity=%s, want unknown_active_object/breaking", snapFunc.Class, snapFunc.Severity)
	}
}

// TestGUCProbe_FailOpen_NegativeProbe is design/03 §7 W03-2 Gate 2's last
// bullet: it first documents the RED alternative design (a naive set_config
// probe on a session that never loaded pgvector reports false-ok for a
// made-up GUC — live-proven behavior, design/03 §2), then proves the real
// three-step probe (library-load first) correctly detects the same made-up
// GUC as guc_probe_failed.
func TestGUCProbe_FailOpen_NegativeProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	bogus := GucProbe{Name: "hnsw.completely_bogus_guc_w032", ProbeValue: "x"}

	// --- RED: the naive design, without a library-load first ---
	// A brand-new physical connection (never touched by the pool) makes the
	// "fresh session" property real rather than assumed — reusing a pool
	// connection risks it having already loaded pgvector from an earlier
	// query in this same test process, which would silently invalidate the
	// negative probe.
	rawConn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("opening fresh raw connection: %v", err)
	}
	defer rawConn.Close(ctx)

	tx, err := rawConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin naive-probe tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// No `SELECT '[0]'::vector` here — this is the documented flaw.
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, bogus.Name, bogus.ProbeValue); err != nil {
		t.Fatalf("RED design assumption violated: naive set_config on a fresh session errored (%v) — "+
			"if pgvector now validates hnsw.* prefixes without a prior vector op, the fail-open behavior "+
			"this test documents no longer holds and the design/03 §2 finding needs re-verification", err)
	}
	// The naive design would now report "ok" — it has no way to see that
	// hnsw.completely_bogus_guc_w032 isn't a real GUC, because pg_settings
	// was never asked and set_config on an as-yet-unreserved prefix always
	// succeeds as a placeholder. This is the fail-open the real probe fixes.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback naive-probe tx: %v", err)
	}

	// --- GREEN: the real probe (library-load first) ---
	drifts, err := checkGUCProbes(ctx, pool, []GucProbe{bogus})
	if err != nil {
		t.Fatalf("checkGUCProbes: %v", err)
	}
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassGucProbeFailed || drifts[0].Severity != SeverityBreaking {
		t.Errorf("got class=%s severity=%s, want guc_probe_failed/breaking", drifts[0].Class, drifts[0].Severity)
	}

	// Sanity: the SAME probe shape against the real, manifest-declared GUC
	// must NOT fail — proves the real probe isn't just failing everything.
	realDrifts, err := checkGUCProbes(ctx, pool, []GucProbe{{Name: "hnsw.iterative_scan", ProbeValue: "relaxed_order"}})
	if err != nil {
		t.Fatalf("checkGUCProbes (real GUC): %v", err)
	}
	if len(realDrifts) != 0 {
		t.Errorf("real GUC probe should not fail: %+v", realDrifts)
	}
}

func findDrift(drifts []Drift, object string) *Drift {
	for i := range drifts {
		if drifts[i].Object == object {
			return &drifts[i]
		}
	}
	return nil
}
