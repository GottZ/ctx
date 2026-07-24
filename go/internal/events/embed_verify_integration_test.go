//go:build integration

// Gates for the Evokoa-Clean-Room-Plan Achse 04 W04-5 (design/04 §4.6/§4.7,
// §7 wave row W04-5): the verify gate of the re-embed migration.
//
//   - G-Rot (three prepared defects, EACH INDIVIDUALLY red): (a) a null
//     vector in embedding_next (norm 0), (b) a row with embed_model_next !=
//     to_model, (c) a missing rest block before the watermark (embedding_next
//     NULL, no memo). Each run must end verify_report=red naming the defect
//     class AND status=paused (never auto-abort). RED evidence procedure:
//     each detector was individually neutered in an intermediate build and
//     its test shown failing (gate stayed green/verifying) before the
//     detector landed — see the wave report.
//   - G-Grün (memo exception): a prepared skip block (infinity memo of THIS
//     migration) keeps the gate GREEN and visibility_loss counts it; the
//     in-test negative probe shows the §4.7 predicate WITHOUT the NOT-EXISTS
//     exception would count the skip (gate permanently red — the review
//     finding the exception exists for).
//   - G-Grün (clean): clean bestand → green report with all five sections
//     filled, status stays verifying, all four indexes valid in the catalog.
//   - Plus: automatic running → verifying at watermark-pending 0 (blocks
//     with finite backoff memos still hold the transition), verify re-run
//     idempotency across paused→resume→verifying with HNSW index REUSE
//     (relfilenode-pinned, never a rebuild).
//
// Run: go test -tags=integration ./internal/events/ -run TestEmbedVerify_Integration -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

// verifyTestCfg is migCfg plus the W04-5 verify knobs (small deterministic
// sample sizes; fixtures stay far below every cap, so full coverage holds).
func verifyTestCfg() *config.Config {
	c := migCfg(8, 0)
	c.EmbedMigration.VerifySampleN = 100
	c.EmbedMigration.VerifyOverlapK = 3
	c.EmbedMigration.VerifyOverlapSamples = 4
	return c
}

// verifyUnitVec builds a deterministic unit-norm vector per seed — distinct
// seeds give distinct directions, so top-k neighborhoods are well-defined.
func verifyUnitVec(seed uint64) []float32 {
	rng := rand.New(rand.NewPCG(seed, 42))
	v := make([]float32, embed.TargetDims)
	var sum float64
	for i := range v {
		v[i] = float32(rng.Float64()*2 - 1)
		sum += float64(v[i]) * float64(v[i])
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
	return v
}

// seedMigratedBlock inserts a block that already carries BOTH space vectors
// (the verify universe: migrated before the watermark).
func seedMigratedBlock(t *testing.T, pool *pgxpool.Pool, title string, age time.Duration, oldVec, nextVec []float32, nextModel string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity,
		        embedding, embed_model, embedding_next, embed_model_next, created_at, updated_at)
		 VALUES ('learnings', $1, 'verify content', 'shared', 'internal',
		        $2, $3, $4, $5, now() - $6::interval, now())
		 RETURNING id::text`,
		title, pgvec.NewVector(oldVec), migFromModel, pgvec.NewVector(nextVec), nextModel,
		age.String()).Scan(&id); err != nil {
		t.Fatalf("seed migrated block %s: %v", title, err)
	}
	return id
}

// seedCleanMigrated seeds n fully-clean migrated blocks whose old and new
// vectors are IDENTICAL per block — the two spaces then share every
// neighborhood, so the degraded Overlap@k must report exactly 1.0.
func seedCleanMigrated(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		v := verifyUnitVec(uint64(i + 1))
		ids = append(ids, seedMigratedBlock(t, pool,
			"clean-"+string(rune('a'+i)), time.Duration(n-i)*time.Hour, v, v, migToModel))
	}
	return ids
}

type verifyRowState struct {
	Status          string
	LastErr         *string
	VerifyStartedAt *time.Time
	Report          []byte
}

func readVerifyRow(t *testing.T, pool *pgxpool.Pool, id string) verifyRowState {
	t.Helper()
	var st verifyRowState
	if err := pool.QueryRow(context.Background(),
		`SELECT status, last_error, verify_started_at, verify_report
		 FROM context_embed_migrations WHERE id = $1::uuid`, id).
		Scan(&st.Status, &st.LastErr, &st.VerifyStartedAt, &st.Report); err != nil {
		t.Fatalf("read verify row: %v", err)
	}
	return st
}

func parseReport(t *testing.T, raw []byte) verifyReport {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("verify_report is NULL, expected a stored report")
	}
	var rep verifyReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("unmarshal verify_report: %v (raw: %s)", err, raw)
	}
	return rep
}

// runVerifyCycle drives one migration cycle with a fresh fake backend
// armed with the embed_next key (the per-cycle model guard runs before
// everything, including the verify trigger) and a synchronous verify
// runner.
func runVerifyCycle(t *testing.T, pool *pgxpool.Pool, s *Scheduler, cfg *config.Config) {
	t.Helper()
	srv := newMigrateEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	if err := s.runEmbedMigrationCycle(context.Background(), backfillRouter(bpool, d), cfg); err != nil {
		t.Fatalf("cycle: %v", err)
	}
}

func TestEmbedVerify_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	s.embedVerifySync = true // deterministic: the trigger runs the gate inline

	// red_null_vector is G-Rot (a): one migrated block carries an all-zero
	// embedding_next (norm 0 — outside the [0.99,1.01] gate). The verify
	// must go red on the integrity section and CAS verifying → paused.
	t.Run("red_null_vector", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 2)
		badID := seedMigratedBlock(t, pool, "null-vector", 30*time.Minute,
			verifyUnitVec(90), make([]float32, embed.TargetDims), migToModel)

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		if st.Status != "paused" {
			t.Errorf("status = %q, want paused (red verify must pause, never abort)", st.Status)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != "red" {
			t.Errorf("report result = %q, want red", rep.Result)
		}
		if rep.Integrity.Result != "red" || rep.Integrity.NormViolations != 1 {
			t.Errorf("integrity = (%q, norm=%d), want (red, 1)", rep.Integrity.Result, rep.Integrity.NormViolations)
		}
		found := false
		for _, o := range rep.Integrity.Offenders {
			if o.BlockID == badID && o.Kind == "norm" {
				found = true
			}
		}
		if !found {
			t.Errorf("offender list %v does not name the null-vector block %s", rep.Integrity.Offenders, badID)
		}
		if st.LastErr == nil || !containsAll(*st.LastErr, "verify red", "norm") {
			t.Errorf("last_error = %v, want a verify-red summary naming the norm defect", st.LastErr)
		}
		if rep.Index.Result != "skipped" {
			t.Errorf("index section = %q, want skipped (no CIC over a defective bestand)", rep.Index.Result)
		}
	})

	// red_wrong_model is G-Rot (b): embed_model_next carries the OLD model
	// string on one row — the relabeling class the per-block guard exists
	// for; the verify is the second net and must catch it in the pass.
	t.Run("red_wrong_model", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 2)
		v := verifyUnitVec(91)
		badID := seedMigratedBlock(t, pool, "wrong-model", 30*time.Minute, v, v, migFromModel)

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		if st.Status != "paused" {
			t.Errorf("status = %q, want paused", st.Status)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != "red" || rep.Integrity.ModelViolations != 1 {
			t.Errorf("report = (%q, model=%d), want (red, 1)", rep.Result, rep.Integrity.ModelViolations)
		}
		found := false
		for _, o := range rep.Integrity.Offenders {
			if o.BlockID == badID && o.Kind == "model" {
				found = true
			}
		}
		if !found {
			t.Errorf("offender list %v does not name the wrong-model block %s", rep.Integrity.Offenders, badID)
		}
		if st.LastErr == nil || !containsAll(*st.LastErr, "verify red", "embed_model_next") {
			t.Errorf("last_error = %v, want a verify-red summary naming the model defect", st.LastErr)
		}
	})

	// red_missing_block is G-Rot (c): a live block before the watermark
	// with an old-space vector, NO _next vector and NO memo — the §4.7
	// Stufe-1 count must be non-zero and the gate red.
	t.Run("red_missing_block", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 2)
		missingID := seedMigratableBlock(t, pool, "missing-rest", "unmigrated content", "internal", time.Hour)

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		if st.Status != "paused" {
			t.Errorf("status = %q, want paused", st.Status)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != "red" || rep.Completeness.Result != "red" || rep.Completeness.MissingBlocks != 1 {
			t.Errorf("completeness = (%q, missing=%d), want (red, 1)", rep.Completeness.Result, rep.Completeness.MissingBlocks)
		}
		found := false
		for _, id := range rep.Completeness.MissingSample {
			if id == missingID {
				found = true
			}
		}
		if !found {
			t.Errorf("missing_sample %v does not name the rest block %s", rep.Completeness.MissingSample, missingID)
		}
		if st.LastErr == nil || !containsAll(*st.LastErr, "verify red", "completeness") {
			t.Errorf("last_error = %v, want a verify-red summary naming the completeness defect", st.LastErr)
		}
	})

	// green_memo_exception is the G-Grün memo probe: a declared skip
	// (infinity memo of THIS migration) must NOT hold the gate red — and
	// the in-test negative probe shows that WITHOUT the NOT-EXISTS
	// exception the very same bestand counts 1 pending (permanently red,
	// the review finding).
	t.Run("green_memo_exception", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		skipID := seedMigratableBlock(t, pool, "skip-oversize", "huge content", "internal", 2*time.Hour)
		if err := store.RecordEmbedFailureForMigration(ctx, pool, skipID, migID,
			store.EmbedFailureOversize, "oversize: pre-wire estimate", time.Minute, time.Hour); err != nil {
			t.Fatalf("seed skip memo: %v", err)
		}

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" {
			t.Fatalf("status = %q, want verifying (green verdict keeps the row)", st.Status)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != "green" {
			t.Fatalf("report result = %q, want green (declared skip must not hold the gate red)", rep.Result)
		}
		if rep.Completeness.SkipCount != 1 || len(rep.Completeness.Skips) != 1 ||
			rep.Completeness.Skips[0].BlockID != skipID || rep.Completeness.Skips[0].Class != "oversize" {
			t.Errorf("skip list = (count=%d, %v), want the oversize skip %s named",
				rep.Completeness.SkipCount, rep.Completeness.Skips, skipID)
		}
		if rep.Completeness.VisibilityLossSkips != 1 || rep.Completeness.VisibilityLoss < 1 {
			t.Errorf("visibility_loss (skips) = %d/%d, want the skip with old-space vector counted",
				rep.Completeness.VisibilityLossSkips, rep.Completeness.VisibilityLoss)
		}

		// Negative probe (review finding): the same predicate WITHOUT the
		// NOT-EXISTS memo exception counts the skip — the gate would be
		// permanently red on any declared skip.
		var withoutException int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks
			 WHERE created_at < $1
			   AND embedding_next IS NULL AND embedding IS NOT NULL AND NOT is_archived`,
			*st.VerifyStartedAt).Scan(&withoutException); err != nil {
			t.Fatalf("negative probe: %v", err)
		}
		if withoutException != 1 {
			t.Errorf("predicate without memo exception counts %d, want 1 — the exception is load-bearing", withoutException)
		}
	})

	// green_clean_five_sections is G-Grün (sauber): clean bestand → green
	// report with all five sections filled, all four indexes valid, status
	// stays verifying; identical per-block vectors in both spaces pin the
	// degraded Overlap@k at exactly 1.0; an archived row with an old-space
	// vector lands in visibility_loss_archived.
	t.Run("green_clean_five_sections", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 5)
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity,
			        embedding, embed_model, is_archived, created_at, updated_at)
			 VALUES ('learnings', 'archived-with-vector', 'c', 'shared', 'internal',
			        $1, $2, true, now() - interval '3 hours', now())`,
			pgvec.NewVector(verifyUnitVec(77)), migFromModel); err != nil {
			t.Fatalf("seed archived block: %v", err)
		}

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" {
			t.Fatalf("status = %q, want verifying (green never advances the statemachine — confirm is W04-6)", st.Status)
		}
		rep := parseReport(t, st.Report)
		t.Logf("green verify_report: %s", st.Report)
		if rep.Result != "green" {
			t.Fatalf("report result = %q, want green", rep.Result)
		}
		// All five sections carry a verdict.
		if rep.Completeness.Result != "green" || rep.Integrity.Result != "green" ||
			rep.Index.Result != "green" || rep.Quality.Result != "informative" ||
			rep.Guard.Result != "informative" {
			t.Errorf("sections = (%q,%q,%q,%q,%q), want (green,green,green,informative,informative)",
				rep.Completeness.Result, rep.Integrity.Result, rep.Index.Result,
				rep.Quality.Result, rep.Guard.Result)
		}
		if rep.Integrity.RowsChecked != 5 {
			t.Errorf("integrity rows_checked = %d, want 5", rep.Integrity.RowsChecked)
		}
		if rep.Completeness.VisibilityLossArchived != 1 {
			t.Errorf("visibility_loss_archived = %d, want 1", rep.Completeness.VisibilityLossArchived)
		}
		if rep.Quality.MeanOverlap != 1.0 || rep.Quality.MinOverlap != 1.0 {
			t.Errorf("overlap mean/min = %v/%v, want 1.0/1.0 (identical vectors in both spaces)",
				rep.Quality.MeanOverlap, rep.Quality.MinOverlap)
		}
		if rep.Guard.Pairs == 0 || rep.Guard.ThresholdDuplicate <= 0 {
			t.Errorf("guard section = %+v, want pairs > 0 and thresholds filled", rep.Guard)
		}
		if rep.Guard.AboveDuplicate != 0 {
			t.Errorf("guard above_duplicate = %d, want 0 (random unit vectors are non-duplicates)", rep.Guard.AboveDuplicate)
		}

		// All four indexes valid in the catalog.
		var valid int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			 WHERE c.relname = ANY($1) AND i.indisvalid`,
			[]string{verifyNextHNSWIndexName, "idx_embedding_pending_next",
				"idx_dream_pending_next", "idx_guard_pending_next"}).Scan(&valid); err != nil {
			t.Fatalf("index catalog probe: %v", err)
		}
		if valid != 4 {
			t.Errorf("valid verify indexes = %d, want 4", valid)
		}
	})

	// auto_transition_running_to_verifying pins task 1: the arm CAS-moves
	// running → verifying (with watermark, cleared report) exactly when the
	// watermark-less §4.7 pending count is 0 — a block under a FINITE
	// backoff memo still holds the transition; a declared infinity skip
	// does not.
	t.Run("auto_transition_running_to_verifying", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		seedCleanMigrated(t, pool, 2)
		holdID := seedMigratableBlock(t, pool, "finite-hold", "content", "internal", time.Hour)
		if err := store.RecordEmbedFailureForMigration(ctx, pool, holdID, migID,
			store.EmbedFailureWire, "wire: transient", time.Hour, 24*time.Hour); err != nil {
			t.Fatalf("seed finite memo: %v", err)
		}

		// Cycle 1: the finite-backoff block is excluded from the pick but
		// COUNTS as pending — no transition.
		runVerifyCycle(t, pool, s, verifyTestCfg())
		st := readVerifyRow(t, pool, migID)
		if st.Status != "running" {
			t.Fatalf("status = %q, want running (finite memo must hold the transition)", st.Status)
		}

		// Flip the memo to a declared skip (infinity) — now the pending set
		// is empty and the arm must enter verifying.
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_failures SET next_attempt_at = 'infinity', last_class = 'oversize'
			 WHERE block_id = $1`, holdID); err != nil {
			t.Fatalf("flip memo to infinity: %v", err)
		}
		runVerifyCycle(t, pool, s, verifyTestCfg())
		st = readVerifyRow(t, pool, migID)
		if st.Status != "verifying" {
			t.Fatalf("status = %q, want verifying (pending 0 → auto transition)", st.Status)
		}
		if st.VerifyStartedAt == nil {
			t.Errorf("verify_started_at is NULL after transition — watermark must be set in the CAS")
		}
		if len(st.Report) != 0 {
			t.Errorf("verify_report set immediately after transition, want NULL (verify runs on the NEXT cycle)")
		}

		// Cycle 3: verifying without report → the gate runs (green: the
		// skip is declared, the clean blocks pass).
		runVerifyCycle(t, pool, s, verifyTestCfg())
		st = readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if st.Status != "verifying" || rep.Result != "green" {
			t.Errorf("after gate: status=%q result=%q, want verifying/green", st.Status, rep.Result)
		}
	})

	// verify_rerun_reuses_index pins §4.7 idempotency: green verify → no
	// re-run while the report stands; paused→resume→drain re-enters
	// verifying with a FRESH watermark and cleared report; the re-run goes
	// green again and REUSES the existing valid HNSW (relfilenode-pinned —
	// a rebuild would allocate a new one).
	t.Run("verify_rerun_reuses_index", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)

		runVerifyCycle(t, pool, s, verifyTestCfg())
		st := readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if st.Status != "verifying" || rep.Result != "green" {
			t.Fatalf("precondition: first verify not green (status=%q result=%q)", st.Status, rep.Result)
		}
		var nodeBefore int64
		if err := pool.QueryRow(ctx,
			`SELECT relfilenode::bigint FROM pg_class WHERE relname = $1`, verifyNextHNSWIndexName).Scan(&nodeBefore); err != nil {
			t.Fatalf("relfilenode before: %v", err)
		}

		// A further cycle must NOT re-run the gate (report present) — the
		// report bytes stay identical.
		reportBefore := string(st.Report)
		runVerifyCycle(t, pool, s, verifyTestCfg())
		st = readVerifyRow(t, pool, migID)
		if string(st.Report) != reportBefore {
			t.Errorf("verify_report changed on a cycle with a standing verdict — gate must not re-run")
		}

		// Operator round-trip: pause → resume. The drained arm re-enters
		// verifying with a fresh watermark and a CLEARED report.
		if err := embedmigration.Transition(ctx, pool, migID,
			embedmigration.StatusVerifying, embedmigration.StatusPaused); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		if err := embedmigration.Transition(ctx, pool, migID,
			embedmigration.StatusPaused, embedmigration.StatusRunning); err != nil {
			t.Fatalf("operator resume: %v", err)
		}
		runVerifyCycle(t, pool, s, verifyTestCfg())
		st = readVerifyRow(t, pool, migID)
		if st.Status != "verifying" {
			t.Fatalf("status = %q, want verifying (re-entry after resume+drain)", st.Status)
		}
		if len(st.Report) != 0 {
			t.Fatalf("verify_report not cleared on re-entry — WithVerifyReportCleared missing from the CAS")
		}

		// Re-run goes green and REUSES the index.
		runVerifyCycle(t, pool, s, verifyTestCfg())
		st = readVerifyRow(t, pool, migID)
		rep = parseReport(t, st.Report)
		if rep.Result != "green" {
			t.Fatalf("re-run result = %q, want green", rep.Result)
		}
		var nodeAfter int64
		if err := pool.QueryRow(ctx,
			`SELECT relfilenode::bigint FROM pg_class WHERE relname = $1`, verifyNextHNSWIndexName).Scan(&nodeAfter); err != nil {
			t.Fatalf("relfilenode after: %v", err)
		}
		if nodeBefore != nodeAfter {
			t.Errorf("HNSW relfilenode changed (%d → %d) — re-run must reuse the valid index, never rebuild", nodeBefore, nodeAfter)
		}
		reused := false
		for _, ix := range rep.Index.Indexes {
			if ix.Name == verifyNextHNSWIndexName && ix.Reused {
				reused = true
			}
		}
		if !reused {
			t.Errorf("index section %+v does not flag the HNSW as reused", rep.Index.Indexes)
		}
	})
}

// containsAll reports whether s contains every needle.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
