package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
)

// Achse 04 W04-4 (design/04-reembed-migration.md §4.3): the re-embed
// MIGRATION scheduler arm — migrateOneEmbedding, structurally the sibling of
// backfillOneEmbedding (scheduler.go), with the §4.3 seam-table deviations:
// dual-column store (StoreEmbeddingNext), model_map key "embed_next" on the
// RoleEmbed chain, per-cycle Model-Guard, persistent peek cursor, batch
// counters (one UPDATE per cycle, §6.3), migration-scoped failure memos and
// runtime partial index (CONCURRENTLY + INVALID recovery, §3.3).

// embedMigrateInterval is the arm's ticker cadence in Run()'s select
// (design §4.3 Takt: "eigener Ticker-Fall"). Throughput is governed by
// embed_migration.batch_per_cycle (hot config) and, structurally, by the
// BACKGROUND admission class — the tick merely bounds the idle latency
// between two batches, so a fixed constant (guardInterval convention)
// suffices; nothing to tune per deployment.
const embedMigrateInterval = 15 * time.Second

// migrationPendingIndexName / DDL: the runtime partial index carrying the
// migration worker's peek (design §3.3 Migrations-Backfill row, E-04-5:
// runtime DDL, NOT a permanent index — outside a migration it would be an
// empty-set index still taxing every embedding write). CONCURRENTLY is
// mandatory: a plain CREATE INDEX holds SHARE over a full-table scan and a
// BACKGROUND arm must never cause a user-visible write outage at 10M rows.
// CIC runs outside any tx (PG requirement — pool.Exec sends it as a single
// autocommit statement, before the worker's cycle tx opens).
const (
	migrationPendingIndexName = "idx_embedding_next_pending"
	migrationPendingIndexDDL  = `CREATE INDEX CONCURRENTLY IF NOT EXISTS ` + migrationPendingIndexName + `
		ON context_blocks (created_at)
		WHERE embedding IS NOT NULL AND embedding_next IS NULL AND NOT is_archived`
)

// migratePendingWhere is the migration pending predicate (design §3.3,
// defined once): blocks that carry an old-space vector, no new-space vector
// yet, and are live. It matches migrationPendingIndexDDL's WHERE clause
// byte-for-byte in semantics — the planner needs that congruence to serve
// the peek from the partial index.
const migratePendingWhere = `embedding IS NOT NULL AND embedding_next IS NULL AND NOT is_archived`

// migratePeekSQL builds the lock-free peek (backfillOneEmbedding sibling).
// POSITIONAL CONTRACT: $1 = migration id (store.
// EmbedMigrationFailureExcludedPredicate), $2 = cursor (only when
// withCursor). The cursor clause is emitted conditionally instead of an
// `($2 IS NULL OR created_at > $2)` OR-form: the OR defeats the index range
// condition — at 10M the peek MUST start its index scan AT the cursor
// (design §4.3 "Peek-Kosten O(Batch), nicht O(#Skips)").
func migratePeekSQL(withCursor bool) string {
	sql := `SELECT id, sensitivity, scope, created_at FROM context_blocks
		WHERE ` + migratePendingWhere + store.EmbedMigrationFailureExcludedPredicate
	if withCursor {
		sql += `
		AND created_at > $2`
	}
	return sql + `
		ORDER BY created_at ASC
		LIMIT 1`
}

// migratePickSQL is the in-tx pick: identical predicate + FOR UPDATE SKIP
// LOCKED (Welle-49 tx-wrap doctrine — the row lock holds over the wire call
// so a concurrent picker lands on a distinct row). Same positional contract
// as migratePeekSQL.
func migratePickSQL(withCursor bool) string {
	sql := `SELECT id, title, content, sensitivity, scope, created_at FROM context_blocks
		WHERE ` + migratePendingWhere + store.EmbedMigrationFailureExcludedPredicate
	if withCursor {
		sql += `
		AND created_at > $2`
	}
	return sql + `
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`
}

// migrateOutcome classifies one migrateOneEmbedding round for the cycle's
// batch accounting (§6.3) and cursor movement (§4.3: the cursor advances
// with EVERY pick — success, skip AND failure; a failed block's retry
// happens after the wrap-around, its memo backoff keeps it excluded until
// then).
type migrateOutcome int

const (
	// migrateOutcomeMigrated: embedding_next stored; migrated_count++.
	migrateOutcomeMigrated migrateOutcome = iota
	// migrateOutcomeSkipped: oversize / sensitivity_ineligible memo with
	// next_attempt_at='infinity'; skipped_count++ (design §4.4).
	migrateOutcomeSkipped
	// migrateOutcomeFailed: transient wire failure memoized with backoff;
	// failed_count++.
	migrateOutcomeFailed
	// migrateOutcomeQueueEnd: no pending block past the cursor — the cycle
	// ends and the cursor wraps to NULL for the next pass (§4.3).
	migrateOutcomeQueueEnd
	// migrateOutcomeDeferred: busy follow-up target under the held tx
	// (Q-I3) or a lost pick race — end the cycle WITHOUT advancing the
	// cursor; a later cycle retries the same spot.
	migrateOutcomeDeferred
)

// migrateResult carries one round's outcome plus the picked block's
// created_at (the cursor high-water, §4.3).
type migrateResult struct {
	kind     migrateOutcome
	pickedAt time.Time
}

// errMigrationServedModelMismatch is the per-block fail-closed net behind
// the per-cycle Model-Guard (§4.2): the backend that ACTUALLY served the
// embed resolved a model other than to_model under the embed_next key —
// storing it would poison the new space with truthfully-labeled foreign
// vectors that only the verify gate would catch. The cycle treats it like a
// guard failure: pause with last_error, never store.
var errMigrationServedModelMismatch = errors.New("embed-migrate: served backend resolved a foreign model under embed_next")

// runEmbedMigration is the ticker entry (Run()'s select): panic isolation +
// interactive-demand defer (runDigest pattern — the whole cycle yields to
// interactive load, design §4.3 Takt) + per-cycle config/router derivation,
// then the actual cycle. Global scope: the vector space is global and
// scope-free (§5 Bruchpfad 9) — the migration is never a per-tenant
// iteration.
func (s *Scheduler) runEmbedMigration(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in embed migration", "error", r, "stack", string(debug.Stack()))
		}
	}()

	if demand := s.interactiveDemand(); demand > 0 {
		slog.Debug("scheduler: embed migration deferred, interactive demand", "count", demand)
		return
	}

	cfg := s.cfg.SnapshotForTenant(ctx, store.GlobalScope)
	router := s.newRouter(cfg, store.GlobalScope)
	if err := s.runEmbedMigrationCycle(ctx, router, cfg); err != nil {
		slog.Error("scheduler: embed migration cycle error", "error", err)
	}
}

// runEmbedMigrationCycle is one worker cycle: status read (§4.1 — active in
// running AND verifying, the drain semantics; paused/pending idle; terminal
// or absent drops the runtime index), runtime index ensure (CIC + INVALID
// recovery, §3.3), Model-Guard (§4.2, before EVERY cycle), then up to
// batch_per_cycle migrateOneEmbedding rounds and ONE counter/cursor UPDATE
// (§6.3). Split from runEmbedMigration so tests drive it with an injected
// router/config (backfillOneEmbedding test convention).
func (s *Scheduler) runEmbedMigrationCycle(ctx context.Context, router *dream.Router, cfg *config.Config) error {
	mig, err := embedmigration.Active(ctx, s.pool)
	if err != nil {
		return err
	}
	if mig == nil {
		// No active migration (none ever, or the last one went terminal):
		// the runtime index must not survive into normal operation
		// (design §3.3 "Terminal-Status → Index droppen"; E-04-5).
		return s.dropMigrationPendingIndex(ctx)
	}

	switch mig.Status {
	case embedmigration.StatusRunning, embedmigration.StatusVerifying:
		// verifying is "running + Gate-Läufe", never a work stop (§4.1
		// drain semantics — the verify phase at 10M is long and organic
		// writes never stop).
	default:
		// pending/paused: idle. The index intentionally STAYS — a pause is
		// a resume candidate, and rebuilding a 10M-row index per
		// pause/resume round-trip would be pure churn.
		return nil
	}

	// Model-Guard, fail-closed, VOR JEDEM Zyklus (§4.2, §5 Bruchpfad 2):
	// ModelFor("embed_next") silently falls back to "default"/Model when
	// the key is missing — the OLD model — and the migration would relabel
	// old-space vectors as migrated (RED-proven by the guard test against
	// the guard-less intermediate build). Per cycle, not per start, so an
	// operator editing context_backends MID-migration is caught too. The
	// guard resolves the RESOLVED string on the chain head — failover
	// chains where several backends serve to_model stay allowed (§4.2).
	// A fully empty SensPublic chain (pool outage/eject) also pauses here:
	// fail-closed, and it keeps the per-block empty-chain branch below
	// unambiguous (there it can only mean the BLOCK's sensitivity).
	if resolved, ok := migrationModelGuard(router, mig); !ok {
		s.pauseMigrationModelGuard(ctx, mig,
			fmt.Sprintf("model guard: resolved %q, expected %q", resolved, mig.ToModel))
		return nil
	}

	if err := s.ensureMigrationPendingIndex(ctx); err != nil {
		return fmt.Errorf("embed-migrate: ensure pending index: %w", err)
	}

	batch := cfg.EmbedMigration.BatchPerCycle
	if batch <= 0 {
		return nil // explicit opt-out (config doc), arm inert
	}

	// W04-5: kick the verify run when the row sits in verifying WITHOUT a
	// verdict for the current watermark (§4.7). Fire-and-forget goroutine —
	// the arm keeps draining below while the gate runs (drain semantics
	// §4.1); the atomic start guard makes repeated ticks harmless.
	if mig.Status == embedmigration.StatusVerifying && !mig.HasVerifyReport && mig.VerifyStartedAt != nil {
		s.maybeStartEmbedVerify(ctx, mig, cfg)
	}

	b := s.runEmbedMigrationBatch(ctx, router, cfg, mig, batch)
	cycleErr := b.err

	// Flush the batch delta even when the loop broke on an error — work
	// that HAPPENED is counted (§6.3: the counters are bookkeeping of past
	// writes, not a progress promise).
	if b.dirty || b.migrated+b.failed+b.skipped > 0 {
		if err := embedmigration.ApplyCycleDelta(ctx, s.pool, mig.ID, b.migrated, b.failed, b.skipped, b.cursor); err != nil {
			if cycleErr == nil {
				cycleErr = err
			} else {
				slog.Error("scheduler: embed migration delta flush failed after cycle error", "error", err)
			}
		}
	}

	// W04-5 automatic running → verifying (§4.1/§4.7): probe only on a
	// clean QueueEnd cycle — that is the sole moment the pending set can be
	// empty, and it keeps the probe off the hot path (early in a 10M
	// migration every cycle is a full batch, no QueueEnd, no count query;
	// once drained, the pending set — and with it this count — is small).
	if cycleErr == nil && mig.Status == embedmigration.StatusRunning && b.sawQueueEnd {
		if err := s.maybeEnterVerifying(ctx, mig); err != nil {
			cycleErr = err
		}
	}
	return cycleErr
}

// migrateBatchState is one cycle's batch bookkeeping (counter deltas, the
// in-memory cursor high-water, wrap/queue-end signals) — carried out of the
// loop in one piece so the cycle body stays a readable pipeline.
type migrateBatchState struct {
	migrated, failed, skipped int64
	cursor                    *time.Time
	dirty                     bool
	sawQueueEnd               bool
	err                       error
}

// runEmbedMigrationBatch runs up to batch migrateOneEmbedding rounds and
// accumulates their outcomes (§4.3 cursor doctrine: the cursor advances
// with every pick — success, skip AND failure — and wraps to NULL at queue
// end; §6.3: deltas land in ONE update, done by the caller).
func (s *Scheduler) runEmbedMigrationBatch(ctx context.Context, router *dream.Router, cfg *config.Config, mig *embedmigration.Migration, batch int) migrateBatchState {
	b := migrateBatchState{cursor: mig.CursorCreatedAt}

	for i := 0; i < batch; i++ {
		// Mid-cycle demand defer (wie Guard/Digest, §4.3): an interactive
		// burst arriving between two blocks ends the batch early — the
		// per-attempt BACKGROUND admission already lets single queries
		// overtake, this stops the arm from queueing MORE background work
		// behind them.
		if s.interactiveDemand() > 0 {
			break
		}

		out, err := s.migrateOneEmbedding(ctx, router, cfg, mig, b.cursor)
		if err != nil {
			if errors.Is(err, errMigrationServedModelMismatch) {
				s.pauseMigrationModelGuard(ctx, mig, err.Error())
				break
			}
			b.err = err
			break
		}

		advance := func(at time.Time) {
			c := at
			b.cursor = &c
			b.dirty = true
		}
		switch out.kind {
		case migrateOutcomeMigrated:
			b.migrated++
			advance(out.pickedAt)
		case migrateOutcomeSkipped:
			b.skipped++
			advance(out.pickedAt)
		case migrateOutcomeFailed:
			b.failed++
			advance(out.pickedAt)
		case migrateOutcomeQueueEnd:
			b.sawQueueEnd = true
			if b.cursor != nil {
				b.cursor = nil // wrap-around: next pass starts from the top (§4.3)
				b.dirty = true
			}
		case migrateOutcomeDeferred:
			// End the cycle without advancing — a later cycle retries.
		}
		if out.kind == migrateOutcomeQueueEnd || out.kind == migrateOutcomeDeferred {
			break
		}
	}
	return b
}

// maybeEnterVerifying counts the CURRENT pending set (the §4.7 Stufe-1
// predicate WITHOUT the watermark clause: live blocks that carry an
// old-space vector, no _next vector, and no infinity memo of this
// migration) and, at zero, performs the running → verifying CAS with a
// fresh watermark and a cleared verify_report (§4.7: the watermark scopes
// every completeness statement of verify AND confirm; clearing the report
// re-arms the verify runner). Blocks with a FINITE backoff memo still count
// as pending — only declared skips (infinity) are excepted, so the arm
// never enters verifying while retryable failures are outstanding. A block
// created between count and CAS is covered by the watermark scoping: it is
// younger than verify_started_at, outside the verify scope, and the arm
// keeps draining it in verifying (§4.1).
func (s *Scheduler) maybeEnterVerifying(ctx context.Context, mig *embedmigration.Migration) error {
	var pending int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE `+verifyCompletenessWhere(false),
		mig.ID).Scan(&pending); err != nil {
		return fmt.Errorf("embed-migrate: verifying-entry pending count: %w", err)
	}
	if pending != 0 {
		return nil
	}
	err := embedmigration.Transition(ctx, s.pool, mig.ID,
		embedmigration.StatusRunning, embedmigration.StatusVerifying,
		embedmigration.WithVerifyStartedAt(), embedmigration.WithVerifyReportCleared())
	if errors.Is(err, embedmigration.ErrTransitionRaceLost) {
		// Operator pause/abort won the race — their transition stands, the
		// next cycle re-reads the row (§4.1: never a silent double move).
		slog.Info("scheduler: embed migration verifying-entry lost CAS race", "migration_id", mig.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("embed-migrate: enter verifying: %w", err)
	}
	slog.Info("scheduler: embed migration drained — entering verifying", "migration_id", mig.ID)
	return nil
}

// migrationModelGuard resolves the RoleEmbed chain at the weakest
// sensitivity (SensPublic — the broadest chain; the per-block FloorSens
// re-resolve stays binding for every actual pick) and checks the chain
// head's embed_next resolution against to_model (§4.2). Returns the
// resolved string for the last_error message and whether the guard passes.
func migrationModelGuard(router *dream.Router, mig *embedmigration.Migration) (string, bool) {
	chain, _, err := router.EmbedChain(router.FloorSens(backends.SensPublic, store.GlobalScope))
	if err != nil || len(chain) == 0 {
		return "", false
	}
	resolved := chain[0].ModelFor(embedmigration.ModelMapKeyEmbedNext).Model
	return resolved, resolved == mig.ToModel
}

// pauseMigrationModelGuard is the fail-closed CAS `running|verifying →
// paused` with last_error (§4.2). A lost CAS race is logged, not escalated:
// it means another transition (operator pause/abort) already moved the row
// — the guard's goal ("no wrong-space write") is met either way, the worker
// simply stops.
func (s *Scheduler) pauseMigrationModelGuard(ctx context.Context, mig *embedmigration.Migration, msg string) {
	slog.Warn("scheduler: embed migration model guard tripped — pausing", "migration_id", mig.ID, "detail", msg)
	if err := embedmigration.Transition(ctx, s.pool, mig.ID, mig.Status, embedmigration.StatusPaused,
		embedmigration.WithLastError(msg)); err != nil {
		slog.Error("scheduler: embed migration pause transition failed", "migration_id", mig.ID, "error", err)
	}
}

// migrateOneEmbedding processes ONE pending block of the active migration —
// the structural sibling of backfillOneEmbedding with the design §4.3 seam
// deviations. Doctrine inherited 1:1: lock-free peek → lease BEFORE tx
// (MW9/Q-I3, acquireBackfillLease) → tx pick FOR UPDATE SKIP LOCKED →
// per-block chain re-resolve with FloorSens (sensitivity gate stays binding
// per block, §5 Bruchpfad 3) → EmbedChain with pool=nil (document embeds
// never enter the query cache) → store in the SAME tx → commit. Deviations:
// migration pending predicate from the persistent cursor, model resolution
// via model_map key embed_next, failure memos migration-scoped, skips with
// infinity semantics, StoreEmbeddingNext instead of StoreEmbedding,
// pipeline label embed-migrate.
func (s *Scheduler) migrateOneEmbedding(ctx context.Context, router *dream.Router, cfg *config.Config, mig *embedmigration.Migration, cursor *time.Time) (migrateResult, error) {
	deferred := migrateResult{kind: migrateOutcomeDeferred}

	args := []any{mig.ID}
	if cursor != nil {
		args = append(args, *cursor)
	}

	var peekID, peekSens, peekScope string
	var peekAt time.Time
	err := s.pool.QueryRow(ctx, migratePeekSQL(cursor != nil), args...).
		Scan(&peekID, &peekSens, &peekScope, &peekAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return migrateResult{kind: migrateOutcomeQueueEnd}, nil
	}
	if err != nil {
		return deferred, fmt.Errorf("embed-migrate: peek: %w", err)
	}

	// Lease BEFORE tx (MW9): resolve the peeked block's chain and block on
	// its background admission while no tx is open. A chain error here is
	// NOT the skip path yet — the pick below re-resolves for the actually
	// picked block and writes the memo inside the held tx; without a chain
	// there is no wire call, so proceeding lease-less is safe.
	guarded := embedcache.Admission{}
	release := func() {}
	peekChain, _, peekChainErr := router.EmbedChain(router.FloorSens(backends.Sensitivity(peekSens), peekScope))
	if peekChainErr == nil {
		guarded, release, err = acquireBackfillLease(ctx, router.EmbedAdmit(), peekChain, embedmigration.ModelMapKeyEmbedNext)
		if err != nil {
			return deferred, fmt.Errorf("embed-migrate: admission: %w", err)
		}
	}
	// Idempotent (B1): settles the pre-lease on every path where EmbedChain
	// never claims it (lost pick, skip memo, pre-wire error).
	defer release()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return deferred, fmt.Errorf("embed-migrate: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var blockID, title, content, sens, scope string
	var pickedAt time.Time
	err = tx.QueryRow(ctx, migratePickSQL(cursor != nil), args...).
		Scan(&blockID, &title, &content, &sens, &scope, &pickedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The peeked block vanished under us (concurrent ClearEmbedding /
		// archive / lock) — defer, don't advance.
		return deferred, nil
	}
	if err != nil {
		return deferred, fmt.Errorf("embed-migrate: pick: %w", err)
	}

	// Chain re-resolve for THE picked block with its floor-adjusted
	// sensitivity (identical to backfill — the gate table binds the chain
	// to the block, never the peek). An empty chain is the
	// sensitivity_ineligible skip (§4.4/§5 Bruchpfad 3): memo with
	// infinity, NEVER escalation across the trust border. The pool-outage
	// look-alike ("no backend at all") cannot reach this branch in
	// production: the per-cycle Model-Guard resolves the SensPublic chain
	// first and pauses the migration when even that is empty — so an empty
	// chain HERE is genuinely the block's sensitivity.
	required := router.FloorSens(backends.Sensitivity(sens), scope)
	chain, _, chainErr := router.EmbedChain(required)
	if chainErr != nil {
		msg := store.NormalizeEmbedError(store.EmbedFailureSensitivityIneligible,
			fmt.Sprintf("no eligible embed backend for sensitivity %q", required))
		if ferr := store.RecordEmbedFailureForMigration(ctx, tx, blockID, mig.ID,
			store.EmbedFailureSensitivityIneligible, msg,
			cfg.EmbedMigration.BackoffBase, cfg.EmbedMigration.BackoffCap); ferr != nil {
			return deferred, fmt.Errorf("embed-migrate: sensitivity memo: %w", ferr)
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return deferred, fmt.Errorf("embed-migrate: commit sensitivity memo: %w", cerr)
		}
		slog.Warn("scheduler: embed migration skip — no eligible backend for block sensitivity, memoized",
			"block_id", blockID, "sensitivity", string(required))
		return migrateResult{kind: migrateOutcomeSkipped, pickedAt: pickedAt}, nil
	}

	embedText := title + "\n\n" + content

	// Oversize-Gate VOR dem Wire-Call (§4.4): conservative len/4 estimate,
	// skip WITHOUT spending a wire call, memo with infinity. MaxTokens<=0
	// disables (explicit opt-out).
	if maxTokens := cfg.EmbedMigration.MaxTokens; maxTokens > 0 {
		if estimated := len(embedText) / 4; estimated > maxTokens {
			msg := store.NormalizeEmbedError(store.EmbedFailureOversize,
				store.OversizeEstimateMessage(estimated, maxTokens))
			if ferr := store.RecordEmbedFailureForMigration(ctx, tx, blockID, mig.ID,
				store.EmbedFailureOversize, msg,
				cfg.EmbedMigration.BackoffBase, cfg.EmbedMigration.BackoffCap); ferr != nil {
				return deferred, fmt.Errorf("embed-migrate: oversize memo: %w", ferr)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return deferred, fmt.Errorf("embed-migrate: commit oversize memo: %w", cerr)
			}
			slog.Warn("scheduler: embed migration oversize pre-check — skipped, memoized",
				"block_id", blockID, "estimated_tokens", estimated, "max_tokens", maxTokens)
			return migrateResult{kind: migrateOutcomeSkipped, pickedAt: pickedAt}, nil
		}
	}

	// pool=nil: document embeddings never enter the query cache (§4.3).
	// Role parameter is the model_map KEY embed_next — the chain itself was
	// resolved over RoleEmbed above (§4.2 Rollensemantik).
	vec, served, attempts, wired, err := embedcache.EmbedChain(
		ctx, nil, chain, embedmigration.ModelMapKeyEmbedNext, embedText, embed.PrefixDocument,
		embedcache.ReportFunc(router.Report), guarded)
	if wired {
		// "" → NULL: background, no request-bound caller (T35b).
		llm.LogEmbedWire(s.pool, "embed-migrate", embedmigration.ModelMapKeyEmbedNext, required, served, attempts,
			[]string{blockID}, err, "", guarded.Class)
	} else if err != nil {
		llm.RecordRejection(s.pool, "embed-migrate", err, guarded.Class)
	}
	if err != nil {
		return s.migrateWireFailure(ctx, tx, cfg, mig, blockID, pickedAt, err)
	}

	servedModel := served.ModelFor(embedmigration.ModelMapKeyEmbedNext).Model
	if servedModel != mig.ToModel {
		// Per-block net behind the per-cycle guard: a FAILOVER link that
		// resolves a foreign model under embed_next (the cycle guard only
		// pins the chain head). Never store — the cycle pauses the
		// migration with this message (fail-closed, §4.2).
		return deferred, fmt.Errorf("%w: model guard: resolved %q, expected %q (serving backend %s)",
			errMigrationServedModelMismatch, servedModel, mig.ToModel, served.Name)
	}

	// StoreEmbeddingNext within the pick tx (execQuerier). The serving
	// pair (embedding/embed_model) is untouched — the live space stays
	// byte-identical until the cutover (§4.5 option c).
	if err := store.StoreEmbeddingNext(ctx, tx, blockID, servedModel, vec); err != nil {
		return deferred, fmt.Errorf("embed-migrate: store: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return deferred, fmt.Errorf("embed-migrate: commit: %w", err)
	}

	slog.Info("scheduler: embedding migrated", "block_id", blockID, "model", servedModel)
	return migrateResult{kind: migrateOutcomeMigrated, pickedAt: pickedAt}, nil
}

// migrateWireFailure handles a failed EmbedChain walk under the held pick
// tx: Q-I3 defer on a busy follow-up target, otherwise Fehler-Memo statt
// Blind-Retry (§4.4) — the memo lands in the SAME tx that holds the row
// lock and the tx is COMMITTED (never rolled back) so it persists although
// the embed failed. exceed_context_size 400s classify as oversize →
// infinity park + skipped outcome (the estimate gate's net); everything
// else is a transient wire failure → exponential backoff + failed outcome.
func (s *Scheduler) migrateWireFailure(ctx context.Context, tx pgx.Tx, cfg *config.Config, mig *embedmigration.Migration, blockID string, pickedAt time.Time, wireErr error) (migrateResult, error) {
	deferred := migrateResult{kind: migrateOutcomeDeferred}
	if errors.Is(wireErr, dispatch.ErrWouldBlock) {
		// Mechanical Q-I3 defer: busy follow-up target under the held tx —
		// never wait here, retry in a later cycle.
		slog.Info("scheduler: embed migration deferred — follow-up embed target busy under held tx (Q-I3)",
			"block_id", blockID)
		return deferred, nil
	}
	class, normalized := store.ClassifyEmbedError(wireErr)
	if ferr := store.RecordEmbedFailureForMigration(ctx, tx, blockID, mig.ID, class, normalized,
		cfg.EmbedMigration.BackoffBase, cfg.EmbedMigration.BackoffCap); ferr != nil {
		return deferred, fmt.Errorf("embed-migrate: embed: %w (memo write also failed: %w)", wireErr, ferr)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return deferred, fmt.Errorf("embed-migrate: commit failure memo: %w", cerr)
	}
	slog.Warn("scheduler: embed migration embed failed — memoized",
		"block_id", blockID, "class", class, "error", wireErr)
	if class == store.EmbedFailureOversize {
		return migrateResult{kind: migrateOutcomeSkipped, pickedAt: pickedAt}, nil
	}
	return migrateResult{kind: migrateOutcomeFailed, pickedAt: pickedAt}, nil
}

// ensureConcurrentIndexValid is the shared CIC lifecycle helper (design
// §3.3, reused by the W04-5 verify gate for the HNSW + sister indexes): a
// VALID index of that name is reused as-is (reused=true — the §4.7
// idempotent re-run doctrine: an existing valid idx_embedding_next_hnsw is
// never rebuilt), an INVALID leftover of a crashed/canceled CIC is dropped
// CONCURRENTLY and rebuilt, a missing index is created via ddl (which MUST
// be a CREATE INDEX CONCURRENTLY IF NOT EXISTS statement; pool.Exec sends
// it as a single autocommit statement outside any tx — PG requirement).
func (s *Scheduler) ensureConcurrentIndexValid(ctx context.Context, name, ddl string) (reused bool, err error) {
	var valid bool
	err = s.pool.QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = $1`, name).Scan(&valid)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to create
	case err != nil:
		return false, fmt.Errorf("embed-migrate: index validity probe (%s): %w", name, err)
	case valid:
		return true, nil
	default:
		slog.Warn("scheduler: embed migration index INVALID — dropping for rebuild", "index", name)
		if _, err := s.pool.Exec(ctx,
			`DROP INDEX CONCURRENTLY IF EXISTS `+name); err != nil {
			return false, fmt.Errorf("embed-migrate: drop invalid index (%s): %w", name, err)
		}
	}
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return false, fmt.Errorf("embed-migrate: create index (%s): %w", name, err)
	}
	slog.Info("scheduler: embed migration index ready", "index", name)
	return false, nil
}

// ensureMigrationPendingIndex creates the runtime partial index if missing
// and recovers from an INVALID leftover (design §3.3: a crashed/canceled
// CIC leaves an INVALID index — detect via pg_index.indisvalid, DROP
// CONCURRENTLY, rebuild; §5 Bruchpfad 7: the index state is part of the
// deterministically checkable recovery state, no dedicated recovery code).
func (s *Scheduler) ensureMigrationPendingIndex(ctx context.Context) error {
	_, err := s.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL)
	return err
}

// dropMigrationPendingIndex removes the runtime index once no migration is
// active (terminal cleanup, §3.3). Existence-probe first so the common case
// (no migration ever — years of normal operation) costs one catalog lookup
// per tick, never a DDL statement.
func (s *Scheduler) dropMigrationPendingIndex(ctx context.Context) error {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'i')`,
		migrationPendingIndexName).Scan(&exists); err != nil {
		return fmt.Errorf("embed-migrate: index existence probe: %w", err)
	}
	if !exists {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`DROP INDEX CONCURRENTLY IF EXISTS `+migrationPendingIndexName); err != nil {
		return fmt.Errorf("embed-migrate: drop pending index: %w", err)
	}
	slog.Info("scheduler: embed migration pending index dropped (no active migration)",
		"index", migrationPendingIndexName)
	return nil
}
