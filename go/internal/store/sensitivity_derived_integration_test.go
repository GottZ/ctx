//go:build integration

// Integration gate for wave W01-4 / V-W9 — `sensitivity_source = 'derived'`
// (migration 144). Three questions, each probed in both directions:
//
//  1. THE CONSTRAINT. Before 144 an INSERT carrying 'derived' fails with
//     SQLSTATE 23514 against context_blocks_sensitivity_source_check; after
//     144 it lands, the four established values keep working, and an
//     invented value still fails — the widened CHECK is enforced, not gone.
//
//  2. THE LOCK FOOTPRINT (design/01 §3.5). The migration runner executes the
//     WHOLE FILE in ONE transaction (store/migrations.go:132-156), so every
//     lock the file takes is held until commit. The gate does not measure
//     RUNTIME — at today's corpus every scan is milliseconds and would look
//     green while hiding the trap. It measures the LOCKS the transaction
//     holds, plus the catalog fact that proves no heap scan happened:
//     convalidated = false. The negative probe runs the same file WITH a
//     VALIDATE appended and shows convalidated flipping to true while the
//     ACCESS EXCLUSIVE lock is still held — the minutes-long outage §3.5
//     forbids at 1M+ blocks.
//
//  3. THE SWEEP (design/05 §7 V-W9, F-6). Making the value legal is not
//     enough on its own: a derived block that later turns out to carry a key
//     must still be reachable by the G40 pattern sweep — the second line of
//     defence for EXISTING rows (the write-time detector, V-W8, only sees
//     writes). The sweep predicate is an EXCLUSION list
//     (`sensitivity_source <> 'manual'`, store/sensitivity.go:246/:284), so
//     'derived' is covered the moment the constraint allows it. That is a
//     property of the predicate, not luck, so it is probed against a mutated
//     predicate that loses it. The G41 LLM audit is pinned in the OTHER
//     direction: its inclusion list (`= 'default'`, :124/:166/:180) must NOT
//     grow — a folded classification is not an unclassified one.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestMigration144 -count=1 -v
//	go test -tags=integration ./internal/store/ -run TestDerivedSweep -count=1 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// migration143 is the highest landed migration before this wave — the state
// the red half of gate 1 is measured against.
const (
	migration143  = 143
	sourceCheck   = "context_blocks_sensitivity_source_check"
	derivedSource = "derived"
)

// w014Cred carries an AWS access key id shape (AKIA + 16 base32 chars), the
// first-matching structured rule in sensitivity.Scan, so the expected Kind is
// stable regardless of the surrounding prose. Synthetic: the 16 payload chars
// are a constant run, never a real credential. Same fixture shape as
// write_detector_integration_test.go's vw8Cred.
var w014Cred = "handover note: AKIA" + strings.Repeat("Q", 16) + " was pasted into the transcript"

// insertWithSource attempts one INSERT with an explicit sensitivity_source and
// returns the raw error, so callers can assert on SQLSTATE rather than on a
// substring of a message.
func insertWithSource(t *testing.T, pool *pgxpool.Pool, title, source string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, sensitivity_source)
		 VALUES ('learnings', $1, 'content of ' || $1, 'private', 'internal', $2)`,
		title, source)
	return err
}

// checkViolation unwraps a pgconn error and reports SQLSTATE + constraint
// name, or ok=false when err is nil or not a Postgres error.
func checkViolation(err error) (sqlstate, constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName, true
	}
	return "", "", false
}

// TestMigration144DerivedSensitivitySource_Integration is gate 1 + gate 2's
// catalog half: the constraint before and after, and the four established
// values.
func TestMigration144DerivedSensitivitySource_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	t.Run("red at 143: derived violates the CHECK", func(t *testing.T) {
		pre := testdb.SetupTestDBUpTo(t, migration143)
		err := insertWithSource(t, pre, "w014-derived-pre", derivedSource)
		if err == nil {
			t.Fatal("INSERT with sensitivity_source='derived' succeeded at migration 143 — " +
				"the red half of this gate never was red, so the green half proves nothing")
		}
		code, constraint, ok := checkViolation(err)
		if !ok {
			t.Fatalf("INSERT failed with a non-Postgres error: %v", err)
		}
		if code != "23514" {
			t.Errorf("SQLSTATE = %q (constraint %q), want 23514 (check_violation): %v", code, constraint, err)
		}
		if constraint != sourceCheck {
			t.Errorf("constraint = %q, want %s", constraint, sourceCheck)
		}
	})

	pool := testdb.SetupTestDB(t)

	t.Run("green at 144: derived is a legal source", func(t *testing.T) {
		if err := insertWithSource(t, pool, "w014-derived-post", derivedSource); err != nil {
			t.Fatalf("INSERT with sensitivity_source='derived' rejected after 144: %v", err)
		}
	})

	t.Run("the four established values keep working", func(t *testing.T) {
		for _, source := range []string{"default", "llm-audit", "pattern", "manual"} {
			if err := insertWithSource(t, pool, "w014-keep-"+source, source); err != nil {
				t.Errorf("INSERT with sensitivity_source=%q rejected after 144: %v", source, err)
			}
		}
	})

	t.Run("the CHECK still rejects an invented value", func(t *testing.T) {
		// NOT VALID applies only to PRE-EXISTING rows; every INSERT and UPDATE
		// is fully enforced. Without this probe "green" could equally mean
		// "the constraint was dropped and never re-added".
		err := insertWithSource(t, pool, "w014-invented", "folded-by-hand")
		code, constraint, ok := checkViolation(err)
		if !ok {
			t.Fatalf("INSERT with an invented source did not fail with a Postgres error: %v", err)
		}
		if code != "23514" || constraint != sourceCheck {
			t.Errorf("SQLSTATE = %q constraint = %q, want 23514 / %s", code, constraint, sourceCheck)
		}
	})

	t.Run("the constraint is NOT VALID in the catalog", func(t *testing.T) {
		// convalidated=false is the catalog-visible proof that no heap scan
		// ran: ADD … NOT VALID writes two catalog rows and touches no heap.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var validated bool
		if err := pool.QueryRow(ctx,
			`SELECT convalidated FROM pg_constraint
			  WHERE conrelid = 'context_blocks'::regclass AND conname = $1`, sourceCheck).Scan(&validated); err != nil {
			t.Fatalf("read pg_constraint: %v", err)
		}
		if validated {
			t.Error("convalidated = true — something validated the constraint, i.e. scanned the whole table " +
				"under the migration transaction's ACCESS EXCLUSIVE lock (design/01 §3.5)")
		}
	})
}

// accessExclusiveRelations lists the relations the CURRENT transaction holds
// an ACCESS EXCLUSIVE lock on, as regclass names. Ordinary catalog writes
// (pg_constraint, pg_depend, …) take RowExclusiveLock and therefore never
// appear here — only a table-level rewrite/DDL target does.
func accessExclusiveRelations(t *testing.T, tx pgx.Tx) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT relation::regclass::text FROM pg_locks
		  WHERE pid = pg_backend_pid() AND locktype = 'relation'
		    AND mode = 'AccessExclusiveLock' AND granted
		  ORDER BY 1`)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatalf("scan pg_locks: %v", err)
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_locks rows: %v", err)
	}
	return out
}

// convalidatedInTx reads the constraint's catalog state from INSIDE the open
// transaction (the ADD is not committed yet, so a second session cannot see
// it).
func convalidatedInTx(t *testing.T, tx pgx.Tx) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var validated bool
	if err := tx.QueryRow(ctx,
		`SELECT convalidated FROM pg_constraint
		  WHERE conrelid = 'context_blocks'::regclass AND conname = $1`, sourceCheck).Scan(&validated); err != nil {
		t.Fatalf("read pg_constraint in tx: %v", err)
	}
	return validated
}

// TestMigration144LockFootprint_Integration is gate 3, the lock gate: it runs
// migration 144 the way the runner runs it — the whole file, one transaction —
// and inspects what that transaction HOLDS, not how long it took.
func TestMigration144LockFootprint_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDBUpTo(t, migration143)
	ctx := context.Background()

	body, err := migrations.Section("144_sensitivity_source_derived.sql")
	if err != nil {
		t.Fatalf("read migration 144 from migrations.FS: %v", err)
	}

	t.Run("the real file: ACCESS EXCLUSIVE on context_blocks only, and NOT VALID", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("execute migration 144 in one transaction: %v", err)
		}

		locked := accessExclusiveRelations(t, tx)
		if len(locked) != 1 || locked[0] != "context_blocks" {
			t.Errorf("transaction holds ACCESS EXCLUSIVE on %v, want exactly [context_blocks] — "+
				"every extra relation is a table the migration blocks for readers AND writers until commit", locked)
		}
		if convalidatedInTx(t, tx) {
			t.Error("convalidated = true inside the migration transaction — a heap scan ran while " +
				"ACCESS EXCLUSIVE was held (design/01 §3.5: minutes of total outage at 1M+ blocks)")
		}
	})

	t.Run("negative probe: the same file WITH VALIDATE scans under the same lock", func(t *testing.T) {
		// This is the variant §3.5 rejects. It must be VISIBLY different, or
		// the assertions above are decoration: the lock is still held (locks
		// live until commit), but convalidated flips to true — the catalog
		// fact that a full heap scan happened inside the ACCESS EXCLUSIVE
		// window.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		withValidate := string(body) + "\nALTER TABLE context_blocks VALIDATE CONSTRAINT " + sourceCheck + ";\n"
		if _, err := tx.Exec(ctx, withValidate); err != nil {
			t.Fatalf("execute the VALIDATE variant: %v", err)
		}
		if !convalidatedInTx(t, tx) {
			t.Fatal("the VALIDATE variant left convalidated = false — the probe cannot distinguish " +
				"the two shapes and therefore proves nothing about the real file")
		}
		if locked := accessExclusiveRelations(t, tx); len(locked) != 1 || locked[0] != "context_blocks" {
			t.Errorf("VALIDATE variant holds ACCESS EXCLUSIVE on %v — expected it to still hold "+
				"context_blocks, which is exactly why the scan is unaffordable", locked)
		}
	})
}

// derivedSweepSeed inserts one block with an explicit sensitivity + source and
// the credential-shaped content, and returns its id.
func derivedSweepSeed(t *testing.T, pool *pgxpool.Pool, scope, title, sens, source, content string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, sensitivity_source)
		 VALUES ('learnings', $1, $2, $3, $4, $5) RETURNING id`,
		title, content, scope, sens, source).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return id
}

func derivedSweepState(t *testing.T, pool *pgxpool.Pool, id string) (sens, source string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx,
		`SELECT sensitivity, sensitivity_source FROM context_blocks WHERE id = $1`, id).Scan(&sens, &source); err != nil {
		t.Fatalf("read block %s: %v", id, err)
	}
	return sens, source
}

func derivedSweepContains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func derivedSweepPick(t *testing.T, pool *pgxpool.Pool, scope string) []string {
	t.Helper()
	blocks, err := store.PickClassifyCandidates(context.Background(), pool, scope, "", 500)
	if err != nil {
		t.Fatalf("pick classify candidates: %v", err)
	}
	ids := make([]string, len(blocks))
	for i, b := range blocks {
		ids[i] = b.ID
	}
	return ids
}

// TestDerivedSweepCoverage_Integration is gate 4, the one D-05 calls the more
// important half: a 'derived' row that carries a key is raised to credentials
// by the G40 pattern sweep — pick, scan, verdict, exactly as
// events/classify.go:160-194 drives it.
func TestDerivedSweepCoverage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	derivedID := derivedSweepSeed(t, pool, "private", "w014-sweep-derived", "internal", derivedSource, w014Cred)
	manualID := derivedSweepSeed(t, pool, "private", "w014-sweep-manual", "internal", "manual", w014Cred)
	defaultID := derivedSweepSeed(t, pool, "private", "w014-sweep-default", "internal", "default", w014Cred)

	t.Run("the derived row is a sweep candidate", func(t *testing.T) {
		picked := derivedSweepPick(t, pool, "private")
		if !derivedSweepContains(picked, derivedID) {
			t.Fatal("the derived block is missing from the candidate set — a folded block that " +
				"later turns out to carry a key would be uncorrectable (design/05 F-6)")
		}
		if derivedSweepContains(picked, manualID) {
			t.Error("the manual block entered the candidate set — manual stays untouchable (design 03 §2.3d)")
		}
		if !derivedSweepContains(picked, defaultID) {
			t.Error("the default block left the candidate set — the wave changed sweep behaviour it must not change")
		}
	})

	t.Run("the scan hits and the verdict raises it to credentials", func(t *testing.T) {
		m, hit := sensitivity.Scan(w014Cred)
		if !hit {
			t.Fatal("sensitivity.Scan reported no hit on the key fixture — the probe would be vacuous")
		}
		applied, err := store.ApplyPatternVerdict(ctx, pool, derivedID, "private", m.Kind, m.Reason)
		if err != nil {
			t.Fatalf("apply pattern verdict on a derived row: %v", err)
		}
		if !applied {
			t.Fatal("the verdict did not apply to the derived row")
		}
		sens, source := derivedSweepState(t, pool, derivedID)
		if sens != "credentials" || source != "pattern" {
			t.Fatalf("post-verdict state = %s/%s, want credentials/pattern", sens, source)
		}
	})

	t.Run("negative probe: an inclusion-list predicate loses the derived row", func(t *testing.T) {
		// The coverage above is a property of the EXCLUSION predicate
		// (`sensitivity_source <> 'manual'`, store/sensitivity.go:246). This
		// probe runs the same pick with the audit's inclusion predicate
		// (`= 'default'`, :124) instead and shows the derived row dropping
		// out — so the assertion above tests the predicate, not the fixture.
		fresh := derivedSweepSeed(t, pool, "private", "w014-sweep-derived-2", "internal", derivedSource, w014Cred)
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var n int
		if err := pool.QueryRow(qctx,
			`SELECT count(*) FROM context_blocks
			  WHERE scope = 'private' AND NOT is_archived
			    AND sensitivity <> 'credentials' AND sensitivity_source = 'default'
			    AND id = $1`, fresh).Scan(&n); err != nil {
			t.Fatalf("mutated pick: %v", err)
		}
		if n != 0 {
			t.Fatalf("the mutated predicate still matched the derived row (%d) — the probe proves nothing", n)
		}
		// And the real predicate does match it, in the same breath.
		if !derivedSweepContains(derivedSweepPick(t, pool, "private"), fresh) {
			t.Fatal("the real predicate lost the derived row")
		}
	})

	t.Run("manual keeps its veto against the pattern verdict", func(t *testing.T) {
		applied, err := store.ApplyPatternVerdict(ctx, pool, manualID, "private", "aws-key", "AWS access key id pattern")
		if err != nil {
			t.Fatalf("apply against manual: %v", err)
		}
		if applied {
			t.Fatal("the verdict applied to a manual row — the source guard is gone")
		}
		if sens, source := derivedSweepState(t, pool, manualID); sens != "internal" || source != "manual" {
			t.Fatalf("manual row changed: %s/%s", sens, source)
		}
	})

	t.Run("the G41 LLM audit does NOT adopt derived rows", func(t *testing.T) {
		// The audit's inclusion list stays at 'default' on purpose: a folded
		// classification is a decision with provenance, not an unclassified
		// row. §3.5 names this as the reason 'derived' exists instead of
		// reusing 'manual' — 'manual' would ALSO leave the pattern sweep,
		// which is the outcome F-6 calls uncorrectable.
		audited := derivedSweepSeed(t, pool, "private", "w014-audit-derived", "internal", derivedSource, "an ordinary note")
		picks, err := store.PickAuditBlocks(ctx, pool, "private", 0, 500, false)
		if err != nil {
			t.Fatalf("pick audit blocks: %v", err)
		}
		for _, b := range picks {
			if b.ID == audited {
				t.Fatal("a derived row entered the G41 audit pick set — the audit would overwrite a folded classification")
			}
		}
		applied, err := store.ApplyAuditVerdict(ctx, pool, audited, backends.SensPublic, md5Of(t, pool, audited))
		if err != nil {
			t.Fatalf("apply audit verdict: %v", err)
		}
		if applied {
			t.Fatal("the audit verdict applied to a derived row")
		}
		if sens, source := derivedSweepState(t, pool, audited); sens != "internal" || source != derivedSource {
			t.Fatalf("derived row changed under the audit: %s/%s", sens, source)
		}
	})

	t.Run("AuditProgress reports the new source class", func(t *testing.T) {
		pending, bySource, err := store.AuditProgress(ctx, pool, "private")
		if err != nil {
			t.Fatalf("audit progress: %v", err)
		}
		if bySource[derivedSource] == 0 {
			t.Errorf("bySource carries no 'derived' entry (%v) — the status surface would hide the new class", bySource)
		}
		if pending != bySource["default"] {
			t.Errorf("pending = %d, want the 'default' count %d — derived must not be counted as unclassified",
				pending, bySource["default"])
		}
	})
}

// md5Of reads the content digest ApplyAuditVerdict binds its verdict to.
func md5Of(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sum string
	if err := pool.QueryRow(ctx, `SELECT md5(content) FROM context_blocks WHERE id = $1`, id).Scan(&sum); err != nil {
		t.Fatalf("read content md5: %v", err)
	}
	return sum
}
