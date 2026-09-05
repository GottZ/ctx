// distill_retention.go — the retention consumer of the distiller arm
// (design/02 §6.2, finding N-13 of reports/bau/c3-3-re-pilot.md §10, wave C5-B).
//
// Migration 135 laid the two indexes this file drives over and named the wave
// that would build it (135:48-54, "Die Retention selbst kommt mit Welle
// W03-11; diese Migration legt nur die Indizes, über die sie fahren wird").
// The wave landed the tables and the config keys, not the consumer: since then
// distill.retention_days and distill.seen_retention_days have been DECLARED and
// UNREAD — the settings surface renders a horizon, the registry description
// promises "days the janitor keeps", and nothing ever deleted a row. The E-6
// full backfill puts the ledger at 20 000 rows upward, which is why A.7 (d)
// makes this the backfill's precondition rather than tidiness.
//
// THE TWO TABLES ARE NOT THE SAME KIND OF ROW, and the difference is what
// decides the two delete rules.
//
// distill_seen is a LEDGER OF SIGHTINGS. A row says "this content was already
// paid for" and nothing about the arm's position in a source. Deleting one is
// therefore a COST statement and only a cost statement — and it is one exactly
// when the same content comes back, which migration 135 names as the dominant
// cost item rather than an edge case (135:222-227: the same repeated tool
// output, batched and paid for again in EVERY generation). The horizon that
// makes such a delete safe is the sliding one distill_select.go promises, and
// this wave had to make that promise true before it could delete on it — see
// distillTouchSeen and the red evidence in the wave report.
//
// distill_run IS THE WATERMARK. There is no state row and no settings key
// beside it: where the arm stands in a source is COALESCE(max(watermark_to), 0)
// over that source's non-running journal rows (135:29-42, distill.go:1405-1416,
// "Es gibt keine zweite Zustandsquelle"). A purge that empties a source's
// journal does not free storage — it RESETS THE ARM to the beginning of that
// source, and the material below the old watermark gets read, batched and paid
// for again. Measured on this tree with the naive delete: one deleted row took
// the watermark from 1788063650102010 to 0.
//
// So the journal rule keeps, per source_key and regardless of age, the two rows
// the arm still reads:
//
//   - the WATERMARK CARRIER — the non-running row with the greatest
//     watermark_to. Keeping it keeps max(watermark_to), which IS the watermark.
//   - the NEWEST row of any outcome, because distillSameAnswer reads exactly
//     that row (distill.go:1366-1373) to decide whether a diagnostic row would
//     repeat what the journal already says. Losing it would let the janitor
//     change what the arm WRITES, and a retention sweep may change what is
//     stored and nothing else.
//   - every budget-trip row still inside distill.spend_backoff, because there
//     is a THIRD journal reader: distillResting derives the active back-off
//     from exactly those rows (distill_spend.go). A retention horizon shorter
//     than the back-off would otherwise lift an active rest and change what
//     the arm SPENDS — review C5-B condition 1 measured that lift
//     (distillResting true -> false across one janitor run) before this
//     clause existed. Nothing couples the two keys in validation, so the
//     clause, not the operator, carries the invariant.
//
// Running rows are exempt from the sweep entirely. Their watermark_to is not in
// the derivation yet, but the startup sweep turns them into killed rows WITHOUT
// discarding the watermark (135:24-27), so deleting one would discard durable
// progress that was about to count. They cannot accumulate: a run row is closed
// within its tick, and every process start sweeps the leftovers
// (distillStartupSweep), so an exempt row that survives the horizon is a
// diagnostic anomaly an operator should see rather than a leak.
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// runDistillRetention is the ninth line of the 6h janitor bundle (§6.2,
// 135:50-53, which names runRecallRetention as the pattern). Both horizons read
// their 0 as the no-op the registry documents (config.go:2148-2153, kept
// forever) and config.Validate refuses the negative that would render as a
// configured window while acting as an off-switch (validate.go:528-529), so the
// guard below is the second deck rather than the only one.
//
// The two purges are INDEPENDENT on purpose. Every neighbour in the bundle
// returns on its first error because it sweeps ONE table; this line sweeps two
// that age on different clocks, and a journal purge that fails must not cost
// the ledger its sweep — the ledger is the half that grows without bound.
func (s *Scheduler) runDistillRetention(ctx context.Context) {
	defer guardPanic("distill retention")
	cfg := s.cfg.Snapshot() //nolint:forbidigo // MT 06 background: distiller retention is a server-global janitor policy over two process-wide bookkeeping tables, not tenant-scoped.
	runDays, seenDays := cfg.Distill.RetentionDays, cfg.Distill.SeenRetentionDays

	runs, err := s.distillPurgeRuns(ctx, runDays, cfg.Distill.SpendBackoff)
	if err != nil {
		slog.Warn("scheduler: distiller journal retention failed", "error", err)
	}
	seen, err := s.distillPurgeSeen(ctx, seenDays)
	if err != nil {
		slog.Warn("scheduler: distiller dedup retention failed", "error", err)
	}
	// One line per run, naming BOTH tables separately — the observability half
	// of the wave (a bundle line that reports a single total cannot say which
	// of two horizons is the one that moved). Silent when there was nothing to
	// do, like every neighbour.
	if runs > 0 || seen > 0 {
		slog.Info("scheduler: distiller bookkeeping purged",
			"run_rows", runs, "retention_days", runDays,
			"seen_rows", seen, "seen_retention_days", seenDays)
	}
}

// distillPurgeSeen trims the cross-run dedup ledger to
// distill.seen_retention_days over last_seen — the axis idx_distill_seen_age
// was created for (135:218-220, "Der Retention-Purge fährt über die Zeitachse,
// nicht über den PK").
//
// The delete is unconditional beyond the horizon, and it may be: last_seen is
// slid forward by every sighting (distillTouchSeen), so a row that reaches the
// horizon is a hash whose content has not come back inside the whole window —
// exactly the row the key's own description calls useless.
func (s *Scheduler) distillPurgeSeen(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM distill_seen
		 WHERE last_seen < now() - make_interval(days => $1)`, days)
	if err != nil {
		return 0, fmt.Errorf("distill: dedup ledger retention delete: %w", err)
	}
	return tag.RowsAffected(), nil
}

// distillPurgeRuns trims the run journal to distill.retention_days over
// started_at, keeping the two rows per source_key the arm still reads (see the
// file header for which two and why).
//
// The two EXISTS clauses and the trip clause ARE those three statements, one
// each. The EXISTS pair needs no aggregate and no window: a row may go when a
// STRICTLY BETTER watermark carrier and a STRICTLY NEWER row of the same
// source both still stand. Both orders are total — the row constructors break
// every tie on run_id, which is the primary key — so exactly one row is
// unbeatable per order, and that row is the one that survives. A source whose
// whole journal is older than the horizon therefore keeps one or two rows,
// never zero. The trip clause shields what distillResting still reads: a
// budget-trip row younger than the back-off window stays regardless of the
// horizon. A backoff of zero or less builds an empty window and leaves the
// clause inert, consistent with that half's documented off-switch.
//
// The cost is a self-join over the source's own rows rather than a plain time
// scan, and the migration sized that deliberately: it declined a full
// started_at index because the journal is "einige tausend Zeilen/Jahr je
// Quelle" and a second complete time axis would be write load without a reader
// (135:194-199). idx_distill_run_source (source_key, watermark_to DESC) serves
// the first EXISTS; the wave report carries the measured plan.
func (s *Scheduler) distillPurgeRuns(ctx context.Context, days int, backoff time.Duration) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM distill_run d
		 WHERE d.started_at < now() - make_interval(days => $1)
		   AND d.outcome <> $2
		   AND NOT (d.outcome = $3
		            AND d.started_at > now() - make_interval(secs => $4))
		   AND EXISTS (
		           SELECT 1 FROM distill_run w
		            WHERE w.source_key = d.source_key
		              AND w.outcome <> $2
		              AND (w.watermark_to, w.started_at, w.run_id)
		                > (d.watermark_to, d.started_at, d.run_id))
		   AND EXISTS (
		           SELECT 1 FROM distill_run n
		            WHERE n.source_key = d.source_key
		              AND (n.started_at, n.run_id) > (d.started_at, d.run_id))`,
		days, distillOutcomeRunning, distillOutcomeBudgetTripped, backoff.Seconds())
	if err != nil {
		return 0, fmt.Errorf("distill: run journal retention delete: %w", err)
	}
	return tag.RowsAffected(), nil
}
