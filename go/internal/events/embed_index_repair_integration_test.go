//go:build integration

// Gates of issue #333: a re-embed migration whose verify verdict of the
// current watermark is green and one of whose _next indexes is missing,
// INVALID or of another definition than the verify gate builds gets that
// index rebuilt by the migration cycle alone — the index stage only, without
// the completeness/integrity scan stages and without a new verdict — and the
// confirm then swaps. Every case in which the green verdict of the watermark
// no longer stands runs no shortcut. The interleavings with a second session
// (another ctxd) use the runtime index lock of issue #332.
//
// Scan cost is read from PostgreSQL itself: the TOAST block accesses of
// context_blocks (pg_statio_user_tables) across a cycle. The scan stages
// detoast both vector columns of every migrated row; the sister index builds
// read neither. A backend flushes its table statistics when it exits, so a
// measured cycle runs on its own pool, which is closed before the read.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

var repairProbeSeq atomic.Int64

// probeScheduler is a scheduler on its own pool whose sessions carry a
// unique application_name; closeProbe ends them and waits until the server
// has seen every one of them exit (their statistics are flushed then).
func probeScheduler(t *testing.T, pool *pgxpool.Pool) (*Scheduler, func()) {
	t.Helper()
	app := fmt.Sprintf("ctx-repair-probe-%d", repairProbeSeq.Add(1))
	cfg := pool.Config()
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("probe pool: %v", err)
	}
	s := newIndexTestScheduler(p)
	s.embedVerifySync = true
	closed := false
	closeProbe := func() {
		if closed {
			return
		}
		closed = true
		p.Close()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var n int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, app).Scan(&n); err != nil {
				t.Fatalf("probe sessions: %v", err)
			}
			if n == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("probe sessions of %s still alive after 20s", app)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Cleanup(closeProbe)
	return s, closeProbe
}

// toastBlocks is the number of TOAST block accesses (read + hit) on
// context_blocks so far.
func toastBlocks(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(toast_blks_read, 0) + COALESCE(toast_blks_hit, 0)
		 FROM pg_statio_user_tables WHERE relname = 'context_blocks'`).Scan(&n); err != nil {
		t.Fatalf("toast blocks: %v", err)
	}
	return n
}

// measuredCycles runs n migration cycles on a fresh probe scheduler and
// returns the TOAST block accesses on context_blocks they caused.
func measuredCycles(t *testing.T, pool *pgxpool.Pool, n int) int64 {
	t.Helper()
	s, closeProbe := probeScheduler(t, pool)
	before := toastBlocks(t, pool)
	for i := 0; i < n; i++ {
		runVerifyCycle(t, pool, s, verifyTestCfg())
	}
	closeProbe()
	return toastBlocks(t, pool) - before
}

// verifiedGreen drives a real verify run to a green verdict: a verifying
// row, clean migrated blocks before the watermark, a flippable backend for
// the confirm, one cycle with the gate inline. It returns the migration id,
// the watermark and the TOAST block accesses of that full run.
func verifiedGreen(t *testing.T, pool *pgxpool.Pool) (string, time.Time, int64) {
	t.Helper()
	seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
	migID := seedMigrationRow(t, pool, "verifying")
	seedCleanMigrated(t, pool, 3)
	reads := measuredCycles(t, pool, 1)
	st := readVerifyRow(t, pool, migID)
	if st.Status != "verifying" || st.VerifyStartedAt == nil || parseReport(t, st.Report).Result != verifyGreen {
		t.Fatalf("fixture: status=%q report=%s, want verifying with a green verdict", st.Status, st.Report)
	}
	return migID, *st.VerifyStartedAt, reads
}

// runSections are the parts of verify_report the verify run of a watermark
// wrote; an index repair leaves every one of them as it is.
var runSections = []string{"result", "started_at", "finished_at", "watermark", "to_model",
	"completeness", "integrity", "index", "quality", "guard"}

func reportSections(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("report sections: %v (raw %s)", err, raw)
	}
	return m
}

// repairEntry is one verify_report.index_repairs entry as stored.
type repairEntry struct {
	Result     string             `json:"result"`
	Watermark  time.Time          `json:"watermark"`
	ScanStages string             `json:"scan_stages"`
	Detail     string             `json:"detail"`
	Indexes    []verifyIndexState `json:"indexes"`
}

func repairEntries(t *testing.T, raw []byte) []repairEntry {
	t.Helper()
	m := reportSections(t, raw)
	r, ok := m["index_repairs"]
	if !ok {
		return nil
	}
	var out []repairEntry
	if err := json.Unmarshal(r, &out); err != nil {
		t.Fatalf("index_repairs: %v (raw %s)", err, r)
	}
	return out
}

// requireIndexOfDDL fails unless name stands valid on context_blocks with the
// definition the verify gate builds.
func requireIndexOfDDL(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	st := indexStateT(t, pool, name)
	if !st.exists || !st.valid {
		t.Fatalf("%s: exists=%t valid=%t, want a valid index", name, st.exists, st.valid)
	}
	live, expected, same, err := runtimeIndexMatches(context.Background(), pool, name, ddlOfManaged(name), st.def)
	if err != nil || !same {
		t.Fatalf("%s: definition %q, want %q (err %v)", name, live, expected, err)
	}
}

func dropIndexT(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP INDEX `+name); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
}

func invalidateIndexT(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE pg_index SET indisvalid = false WHERE indexrelid = $1::regclass`, name); err != nil {
		t.Fatalf("invalidate %s: %v", name, err)
	}
}

// repairBreaks are the three states of a _next index the confirm refuses.
var repairBreaks = []struct {
	label   string
	indexes []string
	action  string
	breakIt func(t *testing.T, pool *pgxpool.Pool)
}{
	{"definition_mismatch", []string{"idx_embedding_pending_scope_sens_next", "idx_embedding_pending_sens_next"},
		indexActionRebuiltDefinition, seedPreMigration175PickSisters},
	{"invalid", []string{"idx_dream_pending_next"}, indexActionRebuiltInvalid,
		func(t *testing.T, pool *pgxpool.Pool) { invalidateIndexT(t, pool, "idx_dream_pending_next") }},
	// The release of migration 182 (issue #360) while a migration waits at
	// its green verdict: the guard twin of the older release no longer has
	// the definition the gate builds.
	{"definition_mismatch_182", []string{"idx_guard_pending_next"},
		indexActionRebuiltDefinition, seedPreMigration182GuardSister},
	{"missing", []string{"idx_guard_pending_next"}, indexActionCreated,
		func(t *testing.T, pool *pgxpool.Pool) { dropIndexT(t, pool, "idx_guard_pending_next") }},
}

func TestEmbedIndexRepair_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// The case of the issue, once per refused state: the green verdict of
	// watermark W stands, a _next index does not. One cycle rebuilds that
	// index alone — no scan stage (no TOAST access), the verdict and every
	// section of the run of W unchanged, the rebuild recorded with W — and
	// the confirm swaps. Before issue #333 the cycle left the index as it
	// was and the confirm kept refusing; the way out was resume, a new
	// watermark and the whole gate.
	for _, c := range repairBreaks {
		t.Run("green_verdict_"+c.label+"_rebuilt_alone", func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			migID, wm, fullReads := verifiedGreen(t, pool)
			before := readVerifyRow(t, pool, migID)
			c.breakIt(t, pool)
			if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); !errors.Is(err, ErrConfirmNextIndexNotReady) {
				t.Fatalf("precondition: confirm = %v, want ErrConfirmNextIndexNotReady", err)
			}

			reads := measuredCycles(t, pool, 1)
			after := readVerifyRow(t, pool, migID)
			t.Logf("TOAST blocks: full verify run %d, cycle with the broken index %d", fullReads, reads)

			for _, ix := range c.indexes {
				requireIndexOfDDL(t, pool, ix)
			}
			if after.Status != "verifying" || after.VerifyStartedAt == nil || !after.VerifyStartedAt.Equal(wm) {
				t.Fatalf("row after the cycle: status=%q watermark=%v, want verifying at %v", after.Status, after.VerifyStartedAt, wm)
			}
			if reads != 0 {
				t.Errorf("the cycle read %d TOAST blocks of context_blocks — a scan stage ran (the full run read %d)", reads, fullReads)
			}
			b, a := reportSections(t, before.Report), reportSections(t, after.Report)
			for _, k := range runSections {
				if string(b[k]) != string(a[k]) {
					t.Errorf("report section %q changed: %s → %s", k, b[k], a[k])
				}
			}
			entries := repairEntries(t, after.Report)
			if len(entries) != 1 {
				t.Fatalf("index_repairs = %+v, want one entry", entries)
			}
			e := entries[0]
			if e.Result != verifyGreen || !e.Watermark.Equal(wm) || e.ScanStages == "" {
				t.Errorf("repair entry %+v, want green, watermark %v, scan_stages naming the run they are kept from", e, wm)
			}
			got := map[string]string{}
			for _, ix := range e.Indexes {
				got[ix.Name] = ix.Action
			}
			for _, ix := range c.indexes {
				if got[ix] != c.action {
					t.Errorf("repair entry: %s action %q, want %q (entry %+v)", ix, got[ix], c.action, e.Indexes)
				}
			}
			for name, action := range got {
				if action != c.action {
					t.Errorf("repair entry names %s (%s), an index it did not build", name, action)
				}
			}
			if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
				t.Fatalf("confirm after the repair: %v", err)
			}
		})
	}

	// A cycle whose _next indexes all stand does nothing beyond reading the
	// catalog: no DDL, no report write.
	t.Run("standing_indexes_are_left_alone", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		before := readVerifyRow(t, pool, migID)
		oids := map[string]int64{}
		for _, ix := range verifyNextIndexes() {
			oids[ix.name] = indexOIDT(t, pool, ix.name)
		}
		if reads := measuredCycles(t, pool, 2); reads != 0 {
			t.Errorf("cycles read %d TOAST blocks", reads)
		}
		for _, ix := range verifyNextIndexes() {
			if got := indexOIDT(t, pool, ix.name); got != oids[ix.name] {
				t.Errorf("%s oid %d → %d", ix.name, oids[ix.name], got)
			}
		}
		if after := readVerifyRow(t, pool, migID); string(after.Report) != string(before.Report) {
			t.Errorf("report changed: %s → %s", before.Report, after.Report)
		}
	})

	// The invalidation cases: the scan results of a run hold for its
	// watermark as long as the row waits at it with the green verdict. Where
	// that no longer holds, no cycle rebuilds the index alone.
	t.Run("paused_by_the_operator_no_shortcut", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_guard_pending_next")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusPaused); err != nil {
			t.Fatalf("pause: %v", err)
		}
		measuredCycles(t, pool, 1)
		if indexOIDT(t, pool, "idx_guard_pending_next") != 0 {
			t.Errorf("a paused migration got its index rebuilt")
		}
		if e := repairEntries(t, readVerifyRow(t, pool, migID).Report); len(e) != 0 {
			t.Errorf("index_repairs on a paused row: %+v", e)
		}
	})

	t.Run("model_guard_pause_no_shortcut", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_guard_pending_next")
		s, _ := probeScheduler(t, pool)
		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		foreign := embedNextPoolRow("embed-a", srv.srv.URL, 100)
		foreign.ModelMap = map[string]backends.ModelSpec{"default": {Model: migFromModel}}
		bpool.SeedSnapshotForTest([]backends.Backend{foreign})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), verifyTestCfg()); err != nil {
			t.Fatalf("cycle: %v", err)
		}
		st := readVerifyRow(t, pool, migID)
		if st.Status != "paused" {
			t.Fatalf("status %q, want paused by the model guard", st.Status)
		}
		if indexOIDT(t, pool, "idx_guard_pending_next") != 0 || len(repairEntries(t, st.Report)) != 0 {
			t.Errorf("the model guard's cycle rebuilt the index (oid %d, report %s)", indexOIDT(t, pool, "idx_guard_pending_next"), st.Report)
		}
	})

	t.Run("red_verdict_no_shortcut", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_guard_pending_next")
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_migrations SET verify_report = jsonb_set(verify_report, '{result}', '"red"') WHERE id = $1::uuid`,
			migID); err != nil {
			t.Fatalf("stamp red: %v", err)
		}
		measuredCycles(t, pool, 1)
		if indexOIDT(t, pool, "idx_guard_pending_next") != 0 {
			t.Errorf("an index was rebuilt on a red verdict")
		}
		if e := repairEntries(t, readVerifyRow(t, pool, migID).Report); len(e) != 0 {
			t.Errorf("index_repairs on a red verdict: %+v", e)
		}
	})

	// resume (verifying → running) leaves the run of W: the drained arm
	// re-enters verifying with a new watermark and the whole gate runs, the
	// scan stages included; the gate's index stage builds the index.
	t.Run("resumed_row_runs_the_whole_gate", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_guard_pending_next")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		reads := measuredCycles(t, pool, 2)
		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" || st.VerifyStartedAt == nil || !st.VerifyStartedAt.After(wm) {
			t.Fatalf("after resume and two cycles: status=%q watermark=%v, want verifying after %v", st.Status, st.VerifyStartedAt, wm)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != verifyGreen || len(repairEntries(t, st.Report)) != 0 {
			t.Errorf("report %s, want the green verdict of a new run without index_repairs", st.Report)
		}
		if reads == 0 || rep.Integrity.RowsChecked != 3 {
			t.Errorf("TOAST blocks %d, integrity rows %d: want the scan stages of a whole run", reads, rep.Integrity.RowsChecked)
		}
		requireIndexOfDDL(t, pool, "idx_guard_pending_next")
	})

	// A row in verifying without a verdict (a new watermark) runs the gate,
	// never the index stage alone.
	t.Run("no_verdict_runs_the_whole_gate", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_guard_pending_next")
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_migrations SET verify_report = NULL, verify_started_at = now() WHERE id = $1::uuid`, migID); err != nil {
			t.Fatalf("new watermark: %v", err)
		}
		reads := measuredCycles(t, pool, 1)
		st := readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if rep.Result != verifyGreen || reads == 0 || len(repairEntries(t, st.Report)) != 0 {
			t.Errorf("result %q, TOAST blocks %d, report %s: want a whole green run", rep.Result, reads, st.Report)
		}
		requireIndexOfDDL(t, pool, "idx_guard_pending_next")
	})

	// A content change of a block before the watermark after the green
	// verdict: the verdict stands (the confirm re-checks completeness in its
	// transaction, as before issue #333), the cycle rebuilds the index and
	// the arm migrates the block again; until then the confirm refuses.
	t.Run("write_after_the_verdict_is_drained_before_the_swap", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, "idx_guard_pending_next")
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM context_blocks ORDER BY created_at LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("pick block: %v", err)
		}
		changed := "changed after the verdict"
		if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: &changed}, []string{"shared"}); err != nil {
			t.Fatalf("content change: %v", err)
		}
		var pending bool
		if err := pool.QueryRow(ctx, `SELECT embedding_next IS NULL FROM context_blocks WHERE id = $1::uuid`, id).Scan(&pending); err != nil || !pending {
			t.Fatalf("precondition: the content change left embedding_next (null=%t, err %v)", pending, err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err == nil {
			t.Fatalf("confirm swapped with a missing index and a pending block")
		}
		measuredCycles(t, pool, 1)
		requireIndexOfDDL(t, pool, "idx_guard_pending_next")
		st := readVerifyRow(t, pool, migID)
		if !st.VerifyStartedAt.Equal(wm) || len(repairEntries(t, st.Report)) != 1 {
			t.Errorf("watermark %v (want %v), report %s: want the verdict of W with one repair", st.VerifyStartedAt, wm, st.Report)
		}
		var next *string
		if err := pool.QueryRow(ctx, `SELECT embed_model_next FROM context_blocks WHERE id = $1::uuid`, id).Scan(&next); err != nil {
			t.Fatalf("read block: %v", err)
		}
		if next == nil || *next != migToModel {
			t.Fatalf("block changed after the verdict not migrated again (embed_model_next %v)", next)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
	})

	// The refusal of the confirm names the way out that exists now.
	t.Run("confirm_refusal_names_the_repair", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		seedPreMigration175PickSisters(t, pool)
		_, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
		if err == nil || !strings.Contains(err.Error(), "the migration cycle rebuilds it") || strings.Contains(err.Error(), "resume the migration") {
			t.Errorf("refusal %v, want it to name the rebuild by the migration cycle instead of resume", err)
		}
		dropIndexT(t, pool, "idx_guard_pending_next")
		_, err = ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
		if err == nil || !strings.Contains(err.Error(), "the migration cycle rebuilds it") {
			t.Errorf("refusal of a missing index %v, want it to name the rebuild by the migration cycle", err)
		}
	})
}

