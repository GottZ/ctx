//go:build integration

// W-A DB gates (internal package: persist and the meta SQL are unexported).
// Proves the migration-123 line: graph_overview_meta remembers every rebuild
// ATTEMPT, not only the successful ones — last_attempt_at/skip_reason/
// candidate_n, computed_at nullable AND without DEFAULT, the skip_reason enum
// wide enough for the outcome that dominates at target scale (timeout), and a
// global success run that no longer wipes a skip-only row.
package overview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const mig123File = "123_overview_meta_liveness.sql"

// columnShape reads nullability + default of one column straight from the
// catalog — the live \d finding of design/02 §3.1 as an assertion.
func columnShape(t *testing.T, pool *pgxpool.Pool, table, column string) (nullable bool, def *string) {
	t.Helper()
	var isNullable string
	if err := pool.QueryRow(context.Background(), `
		SELECT is_nullable, column_default
		  FROM information_schema.columns
		 WHERE table_name = $1 AND column_name = $2`,
		table, column).Scan(&isNullable, &def); err != nil {
		t.Fatalf("column shape %s.%s: %v", table, column, err)
	}
	return isNullable == "YES", def
}

// applyMigrationFile executes one embedded migration file in its own tx —
// exactly what store.RunMigrations does per file.
func applyMigrationFile(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ctx := context.Background()
	sql, err := migrations.Section(name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestMeta123_Schema is the schema half of the W-A gate (design/02 §3.1 +
// §7 gates 3/11). RED against HEAD (migration chain through 122): the three
// columns do not exist and computed_at is `not null | now()`.
func TestMeta123_Schema(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("three attempt columns exist", func(t *testing.T) {
		for _, col := range []string{"last_attempt_at", "skip_reason", "candidate_n"} {
			var n int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM information_schema.columns
				 WHERE table_name = 'graph_overview_meta' AND column_name = $1`, col).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("graph_overview_meta.%s missing — migration 123 not applied", col)
			}
		}
	})

	// The load-bearing half of the finding: DROP NOT NULL alone would leave
	// DEFAULT now() in place (057_graph_overview.sql:69), and a skip upsert
	// that does not name the column would silently claim freshness.
	t.Run("computed_at is nullable AND has no DEFAULT", func(t *testing.T) {
		nullable, def := columnShape(t, pool, "graph_overview_meta", "computed_at")
		if !nullable {
			t.Fatal("computed_at still NOT NULL — a never-built partition cannot be stamped")
		}
		if def != nil {
			t.Fatalf("computed_at still carries DEFAULT %q — a skip upsert would claim freshness (§3.1 second finding)", *def)
		}
	})

	// The enum must cover all FIVE exits of rebuildOverviewOnce, not just the
	// two observable today: at 1M+ 'timeout' is the regular case, and a narrow
	// CHECK would lock out exactly the outcome the column exists for.
	t.Run("skip_reason enum covers all five exits", func(t *testing.T) {
		for _, reason := range []string{"node-cap", "advisory-lock", "timeout", "error", "disabled", "registry-unwired"} {
			if _, err := pool.Exec(ctx, `
				INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, skip_reason)
				VALUES ($1, NULL, now(), $2)
				ON CONFLICT (scope) DO UPDATE SET skip_reason = EXCLUDED.skip_reason`,
				"enum-probe", reason); err != nil {
				t.Fatalf("skip_reason %q rejected by CHECK: %v", reason, err)
			}
		}
		// Negative probe: the CHECK is a real constraint, not decoration.
		_, err := pool.Exec(ctx, `UPDATE graph_overview_meta SET skip_reason = 'nonsense' WHERE scope = 'enum-probe'`)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("unknown skip_reason accepted (err=%v) — CHECK missing", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM graph_overview_meta WHERE scope = 'enum-probe'`); err != nil {
			t.Fatal(err)
		}
	})
}

// TestMeta123_BackfillFromPre123 replays the migration against a database
// frozen at 122 with a real pre-123 row: every existing row describes a
// SUCCESSFUL run (only persist could write it), so last_attempt_at =
// computed_at and candidate_n = node_n are the historically correct values.
func TestMeta123_BackfillFromPre123(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 122)
	ctx := context.Background()

	legacyAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_overview_meta (scope, computed_at, modularity, cluster_n, node_n, edge_n, resolution)
		VALUES ('private', $1, 0.87, 59, 1190, 220, 1.0)`, legacyAt); err != nil {
		t.Fatalf("pre-123 row: %v", err)
	}

	// Pre-condition = the red state: DEFAULT now() is live at 122.
	if nullable, def := columnShape(t, pool, "graph_overview_meta", "computed_at"); nullable || def == nil {
		t.Fatalf("pre-123 precondition broken: computed_at nullable=%v default=%v, want NOT NULL + DEFAULT", nullable, def)
	}

	applyMigrationFile(t, pool, mig123File)

	var lastAttempt time.Time
	var candidateN int
	var skipReason *string
	if err := pool.QueryRow(ctx, `
		SELECT last_attempt_at, candidate_n, skip_reason
		  FROM graph_overview_meta WHERE scope = 'private'`).Scan(&lastAttempt, &candidateN, &skipReason); err != nil {
		t.Fatalf("backfilled row: %v", err)
	}
	if !lastAttempt.Equal(legacyAt) {
		t.Fatalf("last_attempt_at = %v, want the preserved computed_at %v", lastAttempt, legacyAt)
	}
	if candidateN != 1190 {
		t.Fatalf("candidate_n = %d, want node_n 1190 (a success run clusters every candidate)", candidateN)
	}
	if skipReason != nil {
		t.Fatalf("skip_reason = %q, want NULL (the row describes a success)", *skipReason)
	}

	// Idempotent: the file re-runs cleanly (DROP CONSTRAINT IF EXISTS before
	// ADD, ADD COLUMN IF NOT EXISTS, UPDATE … WHERE … IS NULL).
	applyMigrationFile(t, pool, mig123File)
}

// TestMeta123_LockTimeout is gate 11: the runner holds a whole migration file
// in ONE transaction and sets no lock deckel of its own, so an ALTER waiting
// behind a long persist tx (cluster.go:69-73 measures 465 s at 400k nodes)
// would stall every reader and writer behind it in the Postgres lock queue.
// With SET LOCAL lock_timeout the file fails LOUDLY (55P03) and the next boot
// retries — all statements are idempotent.
//
// Negative probe in the same test: the identical body with the lock_timeout
// line stripped does NOT get 55P03 — it waits until the caller's context
// deadline. That is the stall this line exists to prevent.
func TestMeta123_LockTimeout(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	raw, err := migrations.FS.ReadFile(mig123File)
	if err != nil {
		t.Fatalf("read embedded %s: %v", mig123File, err)
	}
	body := string(raw)
	if !strings.Contains(body, "SET LOCAL lock_timeout") {
		t.Fatalf("%s carries no SET LOCAL lock_timeout — gate 11 has nothing to prove", mig123File)
	}

	// Blocker: an ACCESS EXCLUSIVE lock held open for the whole probe.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `LOCK TABLE graph_overview_meta IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	run := func(sql string, timeout time.Duration) error {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		tx, err := pool.Begin(rctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, execErr := tx.Exec(rctx, sql)
		return execErr
	}

	err = run(body, 30*time.Second)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("migration under a held lock returned %v, want SQLSTATE 55P03 (lock_timeout)", err)
	}

	// Red probe: strip the deckel, the same body stalls into the deadline.
	var stripped strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "SET LOCAL lock_timeout") {
			continue
		}
		stripped.WriteString(line + "\n")
	}
	err = run(stripped.String(), 5*time.Second)
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		t.Fatal("stripped body still returned 55P03 — the negative probe proves nothing")
	}
	if err == nil {
		t.Fatal("stripped body completed under a held ACCESS EXCLUSIVE lock — blocker broken")
	}
}

// metaRow is the W-A view of one partition's meta row.
type metaRow struct {
	computedAt  *time.Time
	lastAttempt *time.Time
	skipReason  *string
	candidateN  *int
}

func readMeta(t *testing.T, pool *pgxpool.Pool, scope string) metaRow {
	t.Helper()
	var m metaRow
	if err := pool.QueryRow(context.Background(), `
		SELECT computed_at, last_attempt_at, skip_reason, candidate_n
		  FROM graph_overview_meta WHERE scope = $1`, scope).Scan(
		&m.computedAt, &m.lastAttempt, &m.skipReason, &m.candidateN); err != nil {
		t.Fatalf("meta row %q: %v", scope, err)
	}
	return m
}

// TestMetaLiveness_SuccessWritesPerScopeCandidates covers §7 gates 5 and 9:
// the candidate tally reaches the meta row PER SCOPE (never one run total),
// and a successful run clears any recorded cap (skip_reason NULL,
// last_attempt_at = computed_at).
func TestMetaLiveness_SuccessWritesPerScopeCandidates(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	types := []string{"knowledge"}

	// Two-scope fixture with DELIBERATELY unequal sizes: A = 5 candidates,
	// B = 50. A scalar pass-through would write 55 into BOTH rows — the
	// difference channel on foreign corpus size (BP-1) this gate rules out.
	// (Sizes chosen small enough to keep the fixture cheap; the asymmetry is
	// what discriminates, not the magnitude.)
	for i := 0; i < 5; i++ {
		insBlockB3(t, pool, uuidSeq("a", i), "private", fmt.Sprintf("A-%d", i))
	}
	for i := 0; i < 50; i++ {
		insBlockB3(t, pool, uuidSeq("b", i), "work", fmt.Sprintf("B-%d", i))
	}
	insLinkB3(t, pool, uuidSeq("a", 0), uuidSeq("a", 1), 0.9)
	insLinkB3(t, pool, uuidSeq("b", 0), uuidSeq("b", 1), 0.9)

	stats, err := Rebuild(ctx, pool, Options{Resolution: 1.0, VisibleTypes: types, OverviewTypes: types})
	if err != nil {
		t.Fatalf("global rebuild: %v", err)
	}
	if stats.Skipped {
		t.Fatalf("rebuild skipped (%s) — fixture broken", stats.SkipReason)
	}
	if stats.CandidateCount["private"] != 5 || stats.CandidateCount["work"] != 50 {
		t.Fatalf("Stats.CandidateCount = %v, want private:5 work:50 (per-scope, not a run total of 55)", stats.CandidateCount)
	}

	for scope, want := range map[string]int{"private": 5, "work": 50} {
		m := readMeta(t, pool, scope)
		if m.candidateN == nil || *m.candidateN != want {
			t.Fatalf("scope %q candidate_n = %v, want %d — a run total (55) here is the BP-1 leak", scope, m.candidateN, want)
		}
		if m.skipReason != nil {
			t.Fatalf("scope %q skip_reason = %q after a SUCCESS — a good run must clear the cap", scope, *m.skipReason)
		}
		if m.computedAt == nil || m.lastAttempt == nil || !m.lastAttempt.Equal(*m.computedAt) {
			t.Fatalf("scope %q computed_at=%v last_attempt_at=%v, want equal (same tx now())", scope, m.computedAt, m.lastAttempt)
		}
	}
}

// TestMetaLiveness_AdvisoryLockKeepsCandidateN is §7 gate 6: the advisory-lock
// skip is born inside persist with no candidate map of its own. An
// unconditional `SET candidate_n = EXCLUDED…` would overwrite a correct value
// with 0 — and candidate_n is the DENOMINATOR of the coverage figure.
func TestMetaLiveness_AdvisoryLockKeepsCandidateN(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Success first: a real candidate_n on the row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, skip_reason, candidate_n, node_n)
		VALUES ('private', now() - interval '1 hour', now() - interval '1 hour', NULL, 4711, 4711)`); err != nil {
		t.Fatal(err)
	}

	// The advisory-lock skip carries no candidates (nil map).
	if err := StampAttempt(ctx, pool, []string{"private"}, "advisory-lock", nil, time.Now()); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	m := readMeta(t, pool, "private")
	if m.candidateN == nil || *m.candidateN != 4711 {
		t.Fatalf("candidate_n = %v after a candidate-less skip, want the preserved 4711", m.candidateN)
	}
	if m.skipReason == nil || *m.skipReason != "advisory-lock" {
		t.Fatalf("skip_reason = %v, want advisory-lock", m.skipReason)
	}
	if m.computedAt == nil {
		t.Fatal("skip cleared computed_at — a skip makes a partition OLD, not invalid")
	}
}

// TestMetaLiveness_GlobalSuccessKeepsSkipOnlyRow is §7 gate 8: a skip-only row
// (the "fresh deploy above the cap" case — computed_at NULL, no cluster rows)
// must survive a later GLOBAL success run. RED against the unconditional
// `DELETE FROM graph_overview_meta` (cluster.go:649-652): metaWriteGlobalSQL
// only re-creates scopes present in graph_cluster_node, so the skip memory
// would vanish without a trace.
func TestMetaLiveness_GlobalSuccessKeepsSkipOnlyRow(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	types := []string{"knowledge"}

	const (
		P1 = "019d0000-0000-7000-9000-0000000008a1"
		P2 = "019d0000-0000-7000-9000-0000000008a2"
	)
	insBlockB3(t, pool, P1, "private", "G-P1")
	insBlockB3(t, pool, P2, "private", "G-P2")
	insLinkB3(t, pool, P1, P2, 0.9)

	// A partition that only ever recorded an ATTEMPT — no blocks, no cluster
	// rows, no computed_at.
	if err := StampAttempt(ctx, pool, []string{"over-cap"}, "node-cap",
		map[string]int{"over-cap": 1204331}, time.Now()); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	if _, err := Rebuild(ctx, pool, Options{Resolution: 1.0, VisibleTypes: types, OverviewTypes: types}); err != nil {
		t.Fatalf("global rebuild: %v", err)
	}

	m := readMeta(t, pool, "over-cap") // Fatal if the row is gone
	if m.skipReason == nil || *m.skipReason != "node-cap" {
		t.Fatalf("skip-only row skip_reason = %v after a global success, want node-cap", m.skipReason)
	}
	if m.computedAt != nil {
		t.Fatalf("skip-only row grew a computed_at (%v) — a foreign success must not claim this partition", m.computedAt)
	}
	if m.candidateN == nil || *m.candidateN != 1204331 {
		t.Fatalf("skip-only row candidate_n = %v, want the recorded 1204331", m.candidateN)
	}
	// …and the partition that DID rebuild is fresh, so the teardown restriction
	// did not turn into "never delete anything".
	if p := readMeta(t, pool, "private"); p.computedAt == nil || p.skipReason != nil {
		t.Fatalf("private row computed_at=%v skip_reason=%v, want fresh + clear", p.computedAt, p.skipReason)
	}
}

// uuidSeq builds a deterministic fixture uuid per (partition letter, index) —
// the B3 helpers take the id, and the two partitions must not collide.
func uuidSeq(prefix string, i int) string {
	return fmt.Sprintf("019d0000-0000-7000-9000-00000000%s%03x", prefix, i)
}
