//go:build integration

// Interleavings of the index stage alone (issue #333) with a second session:
// another ctxd's build under the runtime index lock of issue #332, an
// operator resume, a confirm, a stop mid-build, a failed build.
package events

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestEmbedIndexRepairSessions_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// Two instances repair at once: A's repair builds (held), B's cycle
	// runs its repair. B neither drops nor builds beside A's build: it
	// waits, asks again, reuses the finished index and records nothing it
	// did not build; A records its build once.
	t.Run("repair_waits_for_the_repair_of_another_instance", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}

		release := holdBuilds(t, pool)
		chA := make(chan ensureResult, 1)
		go func() { chA <- ensureResult{err: sA.runEmbedIndexRepair(ctx, mig)} }()
		pidA := buildingPID(t, pool, "idx_embedding_pending_next")
		oidA := indexOIDT(t, pool, "idx_embedding_pending_next")
		doneB := make(chan struct{})
		go func() {
			defer close(doneB)
			runVerifyCycle(t, pool, sB, verifyTestCfg())
		}()
		// B's definition probes run on an empty copy of the table inside a
		// rolled-back transaction (issue #312); they are no DDL on
		// context_blocks.
		var seen []string
		for _, q := range watchIndexDDL(t, pool, pidA, 3*time.Second, doneB) {
			if !strings.Contains(q, "ctx_index_probe_") {
				seen = append(seen, q)
			}
		}
		release()
		rA := waitEnsure(t, chA, "A")
		select {
		case <-doneB:
		case <-time.After(90 * time.Second):
			t.Fatalf("B's cycle did not end within 90s")
		}
		if len(seen) > 0 {
			t.Errorf("B ran index DDL while A's build was in progress: %q", seen)
		}
		if rA.err != nil || indexOIDT(t, pool, "idx_embedding_pending_next") != oidA {
			t.Errorf("A: %+v; oid %d → %d: A's build was replaced", rA, oidA, indexOIDT(t, pool, "idx_embedding_pending_next"))
		}
		e := repairEntries(t, readVerifyRow(t, pool, migID).Report)
		if len(e) != 1 || len(e[0].Indexes) != 1 || e[0].Indexes[0].Action != indexActionCreated || !e[0].Watermark.Equal(wm) {
			t.Errorf("index_repairs %+v, want A's one build (B built nothing)", e)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
	})

	// A's repair of watermark W1 builds (held); the operator resumes, the
	// drained arm re-enters verifying with W2, and instance B's gate runs
	// for W2 and waits at the index stage. A's build ends: A records nothing
	// on the run of W2, B's gate reuses the index and lands the verdict of
	// W2, which is a whole run.
	t.Run("repair_of_an_earlier_watermark_beside_the_gate_of_a_new_one", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		release := holdBuilds(t, pool)
		chA := make(chan error, 1)
		go func() { chA <- sA.runEmbedIndexRepair(ctx, mig) }()
		buildingPID(t, pool, "idx_embedding_pending_next")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusRunning, embedmigration.StatusVerifying,
			embedmigration.WithVerifyStartedAt(), embedmigration.WithVerifyReportCleared()); err != nil {
			t.Fatalf("re-enter verifying: %v", err)
		}
		doneB := make(chan struct{})
		go func() {
			defer close(doneB)
			runVerifyCycle(t, pool, sB, verifyTestCfg())
		}()
		time.Sleep(time.Second) // B's gate reaches the index stage and waits
		release()
		select {
		case err := <-chA:
			if err != nil {
				t.Errorf("A: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("A's repair did not end within 60s")
		}
		select {
		case <-doneB:
		case <-time.After(90 * time.Second):
			t.Fatalf("B's cycle did not end within 90s")
		}
		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" || st.VerifyStartedAt == nil || !st.VerifyStartedAt.After(wm) {
			t.Fatalf("row: status=%q watermark=%v, want verifying after %v", st.Status, st.VerifyStartedAt, wm)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != verifyGreen || !rep.Watermark.Equal(*st.VerifyStartedAt) || rep.Integrity.RowsChecked != 3 {
			t.Errorf("verdict of W2 %s, want a whole green run of %v", st.Report, st.VerifyStartedAt)
		}
		if e := repairEntries(t, st.Report); len(e) != 0 {
			t.Errorf("A's repair of %v was recorded on the run of %v: %+v", wm, st.VerifyStartedAt, e)
		}
		requireIndexOfDDL(t, pool, "idx_embedding_pending_next")
	})

	// The repair builds the first of two indexes (held); meanwhile the
	// operator resumes the row. After that build the repair builds no
	// further index for a row that left the run, and records nothing.
	t.Run("repair_stops_between_indexes_when_the_row_leaves", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_dream_pending_next")
		dropIndexT(t, pool, "idx_guard_pending_type_next")
		sB, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		release := holdBuilds(t, pool)
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(ctx, mig) }()
		buildingPID(t, pool, "idx_dream_pending_next")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		release()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("repair: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("repair did not end within 60s")
		}
		st := readVerifyRow(t, pool, migID)
		if st.Status != "running" || st.LastErr != nil || len(repairEntries(t, st.Report)) != 0 {
			t.Errorf("row status=%q last_error=%v report=%s, want running, untouched", st.Status, st.LastErr, st.Report)
		}
		if indexOIDT(t, pool, "idx_guard_pending_type_next") != 0 {
			t.Errorf("the repair built idx_guard_pending_type_next after the row had left the run")
		}
	})

	// The server ends the session of the repair's build (pg_terminate_backend,
	// as a database restart does): nothing is recorded, nothing paused, the
	// green verdict stands; the next cycle drops the INVALID leftover and
	// builds again. Found by the review: the build error paused the row, and
	// resume then cost the whole gate.
	t.Run("server_ends_the_build_session_next_cycle_rebuilds", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sB, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		release := holdBuilds(t, pool)
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(ctx, mig) }()
		pid := buildingPID(t, pool, "idx_embedding_pending_next")
		if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("terminate: %v", err)
		}
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Fatalf("repair did not end within 60s")
		}
		release()
		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" || st.LastErr != nil || !st.VerifyStartedAt.Equal(wm) || len(repairEntries(t, st.Report)) != 0 {
			t.Fatalf("after the ended session: status=%q last_error=%v watermark=%v report=%s, want verifying at %v, untouched",
				st.Status, st.LastErr, st.VerifyStartedAt, st.Report, wm)
		}
		if ix := indexStateT(t, pool, "idx_embedding_pending_next"); !ix.exists || ix.valid {
			t.Fatalf("after the ended session: exists=%t valid=%t, want an INVALID leftover", ix.exists, ix.valid)
		}
		measuredCycles(t, pool, 1)
		requireIndexOfDDL(t, pool, "idx_embedding_pending_next")
		e := repairEntries(t, readVerifyRow(t, pool, migID).Report)
		if len(e) != 1 || len(e[0].Indexes) != 1 || e[0].Indexes[0].Action != indexActionRebuiltInvalid {
			t.Errorf("index_repairs %+v, want one rebuilt_invalid", e)
		}
	})

	// The repair of W1 builds (held); the row is resumed, re-enters verifying
	// with W2 and holds the green verdict of W2 (another instance's gate)
	// before the W1 build ends. The W1 repair records nothing on the verdict
	// of W2. From the review: without the watermark clauses this record
	// landed on W2.
	t.Run("repair_of_an_earlier_watermark_records_nothing_on_a_newer_green_verdict", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sB, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		release := holdBuilds(t, pool)
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(ctx, mig) }()
		buildingPID(t, pool, "idx_embedding_pending_next")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusRunning, embedmigration.StatusVerifying,
			embedmigration.WithVerifyStartedAt(), embedmigration.WithVerifyReportCleared()); err != nil {
			t.Fatalf("re-enter verifying: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_migrations SET verify_report = jsonb_build_object('result', 'green', 'watermark', verify_started_at)
			 WHERE id = $1::uuid`, migID); err != nil {
			t.Fatalf("green verdict of W2: %v", err)
		}
		release()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("repair: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("repair did not end within 60s")
		}
		st := readVerifyRow(t, pool, migID)
		if e := repairEntries(t, st.Report); len(e) != 0 || st.Status != "verifying" || st.VerifyStartedAt.Equal(wm) {
			t.Errorf("row status=%q watermark=%v (W1 %v), index_repairs %+v: want the verdict of W2 without a record of W1", st.Status, st.VerifyStartedAt, wm, e)
		}
	})

	// The row leaves the run while the repair builds its last index (held):
	// the build finishes, and its record does not land on the row, which no
	// longer waits at the green verdict.
	t.Run("row_left_during_the_last_build_gets_no_record", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sB, _ := probeScheduler(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		release := holdBuilds(t, pool)
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(ctx, mig) }()
		buildingPID(t, pool, "idx_embedding_pending_next")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		release()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("repair: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("repair did not end within 60s")
		}
		requireIndexOfDDL(t, pool, "idx_embedding_pending_next")
		st := readVerifyRow(t, pool, migID)
		if st.Status != "running" || st.LastErr != nil || len(repairEntries(t, st.Report)) != 0 {
			t.Errorf("row status=%q last_error=%v report=%s, want running without a repair record", st.Status, st.LastErr, st.Report)
		}
	})

	// The repair waits for another session's build; meanwhile the operator
	// resumes the row (verifying → running). The repair ends without
	// building, without a record and without touching the status.
	t.Run("waiting_repair_ends_on_resume", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		dropIndexT(t, pool, "idx_dream_pending_next")
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB, _ := probeScheduler(t, pool)
		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, "idx_embedding_pending_next", ddlOfManaged("idx_embedding_pending_next"))
		buildingPID(t, pool, "idx_embedding_pending_next")
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(ctx, mig) }()
		time.Sleep(time.Second)
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		release()
		waitEnsure(t, chA, "A")
		var runErr error
		select {
		case runErr = <-runDone:
		case <-time.After(90 * time.Second):
			t.Fatalf("B's repair did not end within 90s")
		}
		st := readVerifyRow(t, pool, migID)
		if runErr != nil || st.Status != "running" || st.LastErr != nil || len(repairEntries(t, st.Report)) != 0 {
			t.Errorf("B: %v; row status=%q last_error=%v report=%s, want running, untouched", runErr, st.Status, st.LastErr, st.Report)
		}
		if indexOIDT(t, pool, "idx_dream_pending_next") != 0 {
			t.Errorf("B built idx_dream_pending_next for a row that had left the run")
		}
	})

	// The repair builds (held); a confirm meanwhile refuses and changes
	// nothing; once the build is done the next confirm swaps.
	t.Run("confirm_during_the_repair_build_refuses", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sB, _ := probeScheduler(t, pool)
		release := holdBuilds(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(ctx, mig) }()
		buildingPID(t, pool, "idx_embedding_pending_next")
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, cerr := ConfirmEmbedMigration(cctx, pool, backends.NewPool(pool, nil), migID)
		cancel()
		if cerr == nil {
			t.Fatalf("confirm swapped while the repair built a _next index")
		}
		if st := readVerifyRow(t, pool, migID); st.Status != "verifying" || !st.VerifyStartedAt.Equal(wm) {
			t.Fatalf("after the refused confirm: status=%q watermark=%v", st.Status, st.VerifyStartedAt)
		}
		release()
		select {
		case err := <-runDone:
			if err != nil {
				t.Fatalf("repair: %v", err)
			}
		case <-time.After(90 * time.Second):
			t.Fatalf("repair did not end within 90s")
		}
		if e := repairEntries(t, readVerifyRow(t, pool, migID).Report); len(e) != 1 {
			t.Errorf("index_repairs %+v, want one", e)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm after the repair: %v", err)
		}
	})

	// The process stops mid-rebuild (its context is canceled, as an orderly
	// ctxd stop does): the build leaves an INVALID index, nothing is stored,
	// the row stays verifying; the next cycle drops the leftover and builds.
	t.Run("stop_mid_rebuild_next_cycle_cleans_up", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		sB, _ := probeScheduler(t, pool)
		release := holdBuilds(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		rctx, cancel := context.WithCancel(ctx)
		runDone := make(chan error, 1)
		go func() { runDone <- sB.runEmbedIndexRepair(rctx, mig) }()
		buildingPID(t, pool, "idx_embedding_pending_next")
		cancel()
		select {
		case <-runDone:
		case <-time.After(60 * time.Second):
			t.Fatalf("canceled repair did not end within 60s")
		}
		// The canceled build ends server-side with its lock while the holder
		// still blocks it (the cancel request reaches the server on its own
		// connection); then the holder goes.
		waitNoAdvisoryLocks(t, pool, 20*time.Second)
		release()
		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" || st.LastErr != nil || len(repairEntries(t, st.Report)) != 0 {
			t.Fatalf("after the stop: status=%q last_error=%v report=%s, want verifying, untouched", st.Status, st.LastErr, st.Report)
		}
		if ix := indexStateT(t, pool, "idx_embedding_pending_next"); !ix.exists || ix.valid {
			t.Fatalf("after the stop: exists=%t valid=%t, want an INVALID leftover", ix.exists, ix.valid)
		}
		measuredCycles(t, pool, 1)
		requireIndexOfDDL(t, pool, "idx_embedding_pending_next")
		st = readVerifyRow(t, pool, migID)
		e := repairEntries(t, st.Report)
		if len(e) != 1 || len(e[0].Indexes) != 1 || e[0].Indexes[0].Action != indexActionRebuiltInvalid || !st.VerifyStartedAt.Equal(wm) {
			t.Errorf("index_repairs %+v at watermark %v, want one rebuilt_invalid at %v", e, st.VerifyStartedAt, wm)
		}
	})

	// A rebuild that fails turns the index stage red like the gate's: the
	// row is paused with the index in last_error, the failed attempt is
	// recorded, the verdict sections stay. The failure here is the gate's
	// read-back: an index of that name on another table of the schema makes
	// CREATE INDEX … IF NOT EXISTS skip, and no index stands on
	// context_blocks.
	t.Run("failed_rebuild_pauses_like_the_gate", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_embedding_pending_next")
		for _, sql := range []string{
			`CREATE TABLE repair_name_taker (x int)`,
			`CREATE INDEX idx_embedding_pending_next ON repair_name_taker (x)`,
		} {
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		before := readVerifyRow(t, pool, migID)
		measuredCycles(t, pool, 1)
		st := readVerifyRow(t, pool, migID)
		if st.Status != "paused" || st.LastErr == nil || !strings.Contains(*st.LastErr, "idx_embedding_pending_next") {
			t.Fatalf("after the failed rebuild: status=%q last_error=%v, want paused naming the index", st.Status, st.LastErr)
		}
		e := repairEntries(t, st.Report)
		if len(e) != 1 || e[0].Result != verifyRed || !e[0].Watermark.Equal(wm) {
			t.Errorf("index_repairs %+v, want one red entry of %v", e, wm)
		}
		b, a := reportSections(t, before.Report), reportSections(t, st.Report)
		for _, k := range runSections {
			if string(b[k]) != string(a[k]) {
				t.Errorf("report section %q changed: %s → %s", k, b[k], a[k])
			}
		}
	})
}
