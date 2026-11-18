package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
)

// Issue #333: the index stage of the verify gate alone, on a migration whose
// verdict of the current watermark is green while one of its _next indexes is
// missing, INVALID or of another definition than the gate builds (a release
// changed a DDL after the verdict, a dropped or crashed build) — the state in
// which the confirm refuses (confirmNextIndexesReady).
//
// Which results it may keep: the confirm swaps on the green verdict of the
// watermark the row carries (status verifying, verify_started_at, bound by
// OnWatermark since issue #332) and re-checks only completeness, in its own
// transaction with the identical watermark predicate. Writes after the scan —
// the arm drains post-watermark blocks and blocks a content change made
// pending again — do not end that verdict; they pass the write path's gates
// and the confirm's completeness re-check. A verdict ends only when the row
// leaves the run of its watermark: resume, pause (operator, model guard, a
// red verdict), abort, or a new watermark, which clears the report. An index
// build writes no row, so it changes no input of the scan stages: the repair
// keeps the verdict, runs no scan stage, stores no new verdict and records
// what it built in verify_report.index_repairs, bound to the run of that
// watermark like the verdicts.
//
// Form: the migration cycle starts it on its own, next to where it starts
// the gate (maybeStartVerifyRunner), so the operator's way out of a refused
// confirm is to confirm again once the index stands; the confirm itself stays
// the short synchronous transaction with lock_timeout it is.

// indexRepairScanStages is the scan_stages value of a repair entry: the
// completeness, integrity, quality and guard sections are those of the verify
// run of the entry's watermark, not run again.
const indexRepairScanStages = "kept from the verify run of this watermark (not run again)"

// verifyIndexRepair is one entry of verify_report.index_repairs.
type verifyIndexRepair struct {
	Result     string    `json:"result"` // green | red
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Watermark is the run whose verdict and scan sections the report keeps.
	Watermark  time.Time          `json:"watermark"`
	ScanStages string             `json:"scan_stages"`
	Detail     string             `json:"detail,omitempty"`
	Indexes    []verifyIndexState `json:"indexes,omitempty"`
}

// maybeStartVerifyRunner starts the verify-stage job the row in verifying
// waits for: the gate while the watermark has no verdict, the index stage
// alone once it has one (runEmbedIndexRepair checks that it is green).
func (s *Scheduler) maybeStartVerifyRunner(ctx context.Context, mig *embedmigration.Migration, cfg *config.Config) {
	if mig.Status != embedmigration.StatusVerifying || mig.VerifyStartedAt == nil {
		return
	}
	if !mig.HasVerifyReport {
		s.maybeStartEmbedVerify(ctx, mig, cfg)
		return
	}
	s.startEmbedVerifyRunner(mig, "embed verify index repair", func() error { return s.runEmbedIndexRepair(ctx, mig) })
}

// runEmbedIndexRepair rebuilds the _next indexes that do not stand, when the
// row waits at the green verdict of the watermark mig carries. While they all
// stand it reads the catalog only (the definition comparison is remembered
// per process) and writes nothing.
func (s *Scheduler) runEmbedIndexRepair(ctx context.Context, mig *embedmigration.Migration) error {
	if mig.VerifyStartedAt == nil {
		return nil
	}
	wm := *mig.VerifyStartedAt
	stand, err := s.verifyNextIndexesStand(ctx)
	if err != nil || stand {
		// An undeterminable comparison builds nothing (issue #312): the
		// confirm refuses with the same reason, the next cycle asks again.
		return err
	}
	wanted := func(ctx context.Context) (bool, error) { return indexRepairWanted(ctx, s.pool, mig.ID, wm) }
	rep := verifyIndexRepair{Result: verifyGreen, StartedAt: time.Now().UTC(), Watermark: wm, ScanStages: indexRepairScanStages}
	built := false
	for _, b := range verifyNextIndexes() {
		// Asked before every index: a row that left the run (resume, a
		// confirm, a new watermark) gets no further build.
		still, err := wanted(ctx)
		if err != nil {
			return err
		}
		if !still {
			slog.Info("scheduler: embed verify index repair ends — the row no longer waits at the green verdict of this watermark",
				"migration_id", mig.ID, "watermark", wm)
			return nil
		}
		action, err := s.verifyEnsureIndex(ctx, mig, b.name, b.ddl, wanted)
		if errors.Is(err, errVerifyRunLeft) {
			slog.Info("scheduler: embed verify index repair ends — the row left the run while it waited for another session's index build",
				"migration_id", mig.ID, "watermark", wm)
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			rep.Result, rep.Detail, rep.FinishedAt = verifyRed, err.Error(), time.Now().UTC()
			return s.finishIndexRepairRed(ctx, mig, &rep)
		}
		if action == indexActionReused {
			continue
		}
		built = true
		rep.Indexes = append(rep.Indexes, verifyIndexState{Name: b.name, Valid: true, Action: action})
	}
	if !built {
		// Another session built what was missing; it records its own.
		return nil
	}
	rep.FinishedAt = time.Now().UTC()
	return s.finishIndexRepairGreen(ctx, mig, &rep)
}

// verifyNextIndexesStand reports whether every _next index stands valid with
// the definition its DDL builds, reading the catalog without the runtime
// index lock — the reuse test of ensureConcurrentIndexValid.
func (s *Scheduler) verifyNextIndexesStand(ctx context.Context) (bool, error) {
	for _, ix := range verifyNextIndexes() {
		st, err := readRuntimeIndex(ctx, s.pool, ix.name)
		if err != nil {
			return false, fmt.Errorf("embed-verify: index repair: read %s: %w", ix.name, err)
		}
		if !st.exists || !st.valid {
			return false, nil
		}
		same, _, _, err := s.runtimeIndexDefinitionSame(ctx, s.pool, ix.name, ix.ddl, st.def)
		if err != nil {
			return false, fmt.Errorf("embed-verify: index repair: %s left as it is: %w", ix.name, err)
		}
		if !same {
			return false, nil
		}
	}
	return true, nil
}

// indexRepairWanted reports whether the row still waits at the green verdict
// of watermark wm: verifying, that watermark, a green report.
func indexRepairWanted(ctx context.Context, db pgxdb.Rower, id string, wm time.Time) (bool, error) {
	var wanted bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM context_embed_migrations
		 WHERE id = $1::uuid AND status = 'verifying' AND verify_started_at = $2
		   AND verify_report->>'result' = 'green')`, id, wm).Scan(&wanted); err != nil {
		return false, fmt.Errorf("embed-verify: row still waits at the green verdict: %w", err)
	}
	return wanted, nil
}

// indexRepairAppendSQL appends one entry to verify_report.index_repairs of the
// row that waits at the green verdict of the entry's watermark ($3); zero rows
// when it no longer does. The verdict and the run's sections stay as they are.
const indexRepairAppendSQL = `UPDATE context_embed_migrations
	SET verify_report = jsonb_set(verify_report, '{index_repairs}',
	        COALESCE(verify_report->'index_repairs', '[]'::jsonb) || jsonb_build_array($2::jsonb))
	WHERE id = $1::uuid AND status = 'verifying' AND verify_started_at = $3
	  AND verify_report->>'result' = 'green'`

// finishIndexRepairGreen records the rebuild on the verdict it repaired.
func (s *Scheduler) finishIndexRepairGreen(ctx context.Context, mig *embedmigration.Migration, rep *verifyIndexRepair) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("embed-verify: marshal index repair: %w", err)
	}
	tag, err := s.pool.Exec(ctx, indexRepairAppendSQL, mig.ID, string(body), rep.Watermark)
	if err != nil {
		return fmt.Errorf("embed-verify: record index repair: %w", err)
	}
	if tag.RowsAffected() == 0 {
		slog.Info("scheduler: embed verify index repair not recorded — the row left the green verdict of this watermark",
			"migration_id", mig.ID, "watermark", rep.Watermark)
		return nil
	}
	slog.Info("scheduler: embed verify index repair done — the green verdict stands, the confirm can swap",
		"migration_id", mig.ID, "watermark", rep.Watermark, "indexes", len(rep.Indexes))
	return nil
}

// finishIndexRepairRed is the red index stage of the gate for the repair: the
// failed attempt is recorded and the row paused with the reason in last_error,
// in one transaction, both bound to the run of the watermark; the green
// verdict's sections stay, and resume re-enters verifying for a whole run.
func (s *Scheduler) finishIndexRepairRed(ctx context.Context, mig *embedmigration.Migration, rep *verifyIndexRepair) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("embed-verify: marshal index repair: %w", err)
	}
	summary := "verify index repair red: index: " + rep.Detail
	if runes := []rune(summary); len(runes) > 500 {
		summary = string(runes[:500])
	}
	err = pgxdb.Write(ctx, s.pool, pgxdb.Stages{
		Begin:  "embed-verify: begin index repair red tx",
		Commit: "embed-verify: commit index repair red",
	}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, indexRepairAppendSQL, mig.ID, string(body), rep.Watermark)
		if err != nil {
			return fmt.Errorf("embed-verify: record red index repair: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return embedmigration.ErrTransitionRaceLost
		}
		terr := embedmigration.Transition(ctx, tx, mig.ID,
			embedmigration.StatusVerifying, embedmigration.StatusPaused,
			embedmigration.WithLastError(summary), embedmigration.OnWatermark(rep.Watermark))
		if terr != nil && !errors.Is(terr, embedmigration.ErrTransitionRaceLost) {
			return fmt.Errorf("embed-verify: index repair pause transition: %w", terr)
		}
		return terr
	})
	if errors.Is(err, embedmigration.ErrTransitionRaceLost) {
		slog.Warn("scheduler: embed verify index repair red discarded — the row left the green verdict of this watermark",
			"migration_id", mig.ID, "watermark", rep.Watermark, "detail", rep.Detail)
		return nil
	}
	if err != nil {
		return err
	}
	slog.Warn("scheduler: embed verify index repair RED — migration paused", "migration_id", mig.ID, "detail", summary)
	return nil
}
