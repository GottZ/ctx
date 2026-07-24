package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Achse 04 W04-6 (design/04-reembed-migration.md §4.8/§4.9/§4.10): the
// cutover MECHANICS — confirm (verifying → done), rollback (done →
// rolled_back) and cleanup — as callable functions. The HTTP/CLI surface
// on top (operator confirm per E-04-2) is W04-7; nothing here is wired to
// a route or a scheduler tick, these are explicit operator actions.
//
// Choreography contract (§4.8, review-critical): the confirm executes the
// model_map flip ITSELF as a data write inside the swap tx and reloads the
// backend pool synchronously right after commit — the residual mixed-space
// window is the sub-second reload latency, and the Nachlauf-Sweep over the
// M109 provenance column (embed_model != to_model) is the net under it.
// ctx_rrf / ctx_guard_check are never touched: expression indexes and
// function bodies bind to column ATTNUMS, so the column RENAME re-points
// them to the new space with zero function generations (the e2e gate pins
// pg_proc before/after).

// cutoverLockTimeout bounds the ACCESS-EXCLUSIVE wait of the swap tx's
// RENAMEs (§4.8): the cutover must never queue behind a long reader and
// thereby block every follow-up query — on timeout the WHOLE tx aborts, the
// statemachine stays `verifying`, and a retry is simply a new confirm call.
const cutoverLockTimeout = "5s"

// Named refusal errors — each is a §-anchored fail-closed gate, surfaced
// verbatim by the W04-7 operator surface.
var (
	ErrCutoverMigrationNotFound   = errors.New("embed-cutover: migration id not found")
	ErrConfirmNotVerifying        = errors.New("embed-cutover: confirm: migration is not in status verifying")
	ErrConfirmVerifyReportMissing = errors.New("embed-cutover: confirm: no verify_report stored — run the verify gate first (§4.7)")
	ErrConfirmVerifyNotGreen      = errors.New("embed-cutover: confirm: verify_report result is not green")
	ErrConfirmWatermarkMissing    = errors.New("embed-cutover: confirm: verify_started_at watermark missing")
	// ErrConfirmNoFlipTarget: no RoleEmbed backend carries the embed_next
	// model_map key — there is no intended target configuration to flip to
	// (§4.8: "Confirm prüft die INTENDIERTE Ziel-Konfiguration").
	ErrConfirmNoFlipTarget = errors.New("embed-cutover: confirm: no RoleEmbed backend carries an embed_next model_map key — nothing to flip")
	// ErrConfirmEmbedKeyNotFlippable: a flip-target backend has no explicit
	// 'embed' model_map row (it resolves via the default fallback) or the
	// row is not a parseable string/object — not "auffindbar und eindeutig
	// updatebar" (§4.8), so the confirm refuses instead of inventing one.
	ErrConfirmEmbedKeyNotFlippable = errors.New("embed-cutover: confirm: a flip-target backend's 'embed' model_map row is not findable/uniquely updatable")
	// ErrConfirmEmbedNextForeign: a flip-target's embed_next resolves to a
	// model other than to_model — flipping it would relabel a foreign space
	// as the serving space (§4.2 model-guard class, at confirm time).
	ErrConfirmEmbedNextForeign  = errors.New("embed-cutover: confirm: a RoleEmbed backend's embed_next key resolves to a foreign model")
	ErrConfirmNextIndexNotReady = errors.New("embed-cutover: confirm: a _next index is missing or INVALID — the verify gate builds them (§4.7 Stufe 3)")
	// ErrConfirmIncomplete: the in-tx completeness re-check (IDENTICAL
	// watermark+memo predicate as §4.7 Stufe 1 — §5 Bruchpfad 4) found
	// pending blocks. The tx rolled back, the row stays verifying.
	ErrConfirmIncomplete = errors.New("embed-cutover: confirm: completeness re-check found pending blocks before the watermark — tx rolled back, migration stays verifying")

	ErrRollbackReasonRequired = errors.New("embed-cutover: rollback: rollback_reason is required (§4.10) — refused before any write")
	ErrRollbackNotDone        = errors.New("embed-cutover: rollback: migration is not in status done")
	// ErrRollbackActiveMigration: a later, non-terminal migration exists —
	// rolling back underneath it would collide with its embedding_next use.
	ErrRollbackActiveMigration     = errors.New("embed-cutover: rollback: another non-terminal migration exists")
	ErrRollbackOldResourcesMissing = errors.New("embed-cutover: rollback: _old resources missing — no rollback anchor (after cleanup the way back is a new migration in the opposite direction, §4.10)")
	// ErrRollbackNextDataPresent: the post-confirm fresh embedding_next pair
	// carries data (a follow-up migration wrote into it) — dropping it for
	// the inverse rename would silently destroy that work.
	ErrRollbackNextDataPresent = errors.New("embed-cutover: rollback: the fresh embedding_next pair carries data — purge or abort that work first")

	ErrCleanupNotDone              = errors.New("embed-cutover: cleanup: migration is not in status done")
	ErrCleanupOldResourcesMissing  = errors.New("embed-cutover: cleanup: _old resources missing (already cleaned up, or dropped out-of-band)")
	ErrCutoverBackendPoolRequired  = errors.New("embed-cutover: backend pool is required — the synchronous post-commit reload is part of the mechanics, not optional")
	ErrConfirmFlipRowCountMismatch = errors.New("embed-cutover: confirm: model_map flip touched fewer backends than validated — tx rolled back")
)

// EmbedCutoverResult is the confirm's operator payload (§4.8: "Rückgabewert
// des Confirm: visibility_loss + Nach-Watermark-Transiente + Sweep-Count";
// W04-7 renders it).
type EmbedCutoverResult struct {
	MigrationID string
	FromModel   string
	ToModel     string
	// VisibilityLoss is the §4.7-Stufe-1 number out of the green
	// verify_report: blocks that lost their serving vector at the swap
	// (skips with an old-space vector + archived rows with a vector). A
	// DECLARED permanent loss, shown, never silently absorbed.
	VisibilityLoss int64
	// PostWatermarkPending counts blocks whose serving column is NULL after
	// the swap and that carry no infinity park — the transient the regular
	// backfill (capped Pfad A, Pfad B) converges (§4.8 Restmengen).
	PostWatermarkPending int64
	// SweepCleared counts rows the Nachlauf-Sweep nulled (serving vector
	// labeled != to_model — reload-latency window writes; expected 0..single
	// digits).
	SweepCleared int64
	// MemosCopied counts infinity migration memos re-homed as backfill
	// memos (migration_id NULL, next_attempt_at infinity) so parked blocks
	// never enter a post-swap wire-retry loop (§4.8).
	MemosCopied int64
	// FlippedBackends names the context_backends rows whose model_map was
	// flipped in the swap tx (audit trail for the operator output).
	FlippedBackends []string
}

// EmbedRollbackResult mirrors EmbedCutoverResult for the §4.10 mirror tx.
type EmbedRollbackResult struct {
	MigrationID string
	FromModel   string
	ToModel     string
	// SweepCleared counts rows the INVERSE sweep nulled (serving vector
	// labeled != from_model after the rename back).
	SweepCleared    int64
	FlippedBackends []string
}

// cutoverColumnRenames is the §4.8 (3) column choreography IN ORDER (the
// serving column must vacate its name before _next can take it). The
// inverse (rollback §4.10) walks it backwards with sides swapped.
var cutoverColumnRenames = [][2]string{
	{"embedding", "embedding_old"},
	{"embedding_next", "embedding"},
	{"embed_model", "embed_model_old"},
	{"embed_model_next", "embed_model"},
}

// cutoverIndexRenames is §4.8 (3)+(3b): the HNSW pair plus the three
// partial-index pairs of the Attnum class. Without (3b) the old exemplars'
// predicates would silently follow embedding_old while the Go queries mean
// the new column by NAME — backfill peek, dream queue scan and guard
// pending scan would all seq-scan at 10M (the e2e gate pins the EXPLAINs).
var cutoverIndexRenames = [][2]string{
	{"idx_embedding_hnsw", "idx_embedding_hnsw_old"},
	{verifyNextHNSWIndexName, "idx_embedding_hnsw"},
	{"idx_embedding_pending", "idx_embedding_pending_old"},
	{"idx_embedding_pending_next", "idx_embedding_pending"},
	{"idx_dream_pending", "idx_dream_pending_old"},
	{"idx_dream_pending_next", "idx_dream_pending"},
	{"idx_guard_pending", "idx_guard_pending_old"},
	{"idx_guard_pending_next", "idx_guard_pending"},
}

// cutoverOldIndexNames / cutoverOldColumnNames are the rollback anchor
// (§4.8 "Alt-Bestand") — the exact objects `cleanup` drops BY NAME and the
// rollback preflight requires to exist (fail-closed).
var (
	cutoverOldIndexNames  = []string{"idx_embedding_hnsw_old", "idx_embedding_pending_old", "idx_dream_pending_old", "idx_guard_pending_old"}
	cutoverOldColumnNames = []string{"embedding_old", "embed_model_old"}
)

// cutoverRow is the migration row slice the three operator actions need.
type cutoverRow struct {
	status         embedmigration.Status
	fromModel      string
	toModel        string
	watermark      *time.Time
	verifyResult   *string
	visibilityLoss int64
}

func loadCutoverRow(ctx context.Context, q embedmigration.Querier, migrationID string) (*cutoverRow, error) {
	r := &cutoverRow{}
	var status string
	err := q.QueryRow(ctx,
		`SELECT status, from_model, to_model, verify_started_at,
		        verify_report->>'result',
		        COALESCE((verify_report->'completeness'->>'visibility_loss')::bigint, 0)
		 FROM context_embed_migrations WHERE id = $1::uuid`, migrationID).
		Scan(&status, &r.fromModel, &r.toModel, &r.watermark, &r.verifyResult, &r.visibilityLoss)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCutoverMigrationNotFound, migrationID)
	}
	if err != nil {
		return nil, fmt.Errorf("embed-cutover: load migration %s: %w", migrationID, err)
	}
	r.status = embedmigration.Status(status)
	return r, nil
}

// embedFlipTargets returns the names of every RoleEmbed backend carrying an
// embed_next model_map key, after validating that each is flippable: the
// embed_next value resolves to toModel AND an explicit 'embed' row exists
// (string or object — the only shapes ParseModelMap admits). The §4.8
// precondition in code: refuse when the intended target configuration is
// not findable/uniquely updatable, instead of demanding it be pre-flipped.
func embedFlipTargets(ctx context.Context, q embedmigration.Querier, toModel string) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT name, model_map FROM context_backends
		 WHERE $1 = ANY(roles) AND model_map ? $2
		 ORDER BY name`,
		backends.RoleEmbed, embedmigration.ModelMapKeyEmbedNext)
	if err != nil {
		return nil, fmt.Errorf("embed-cutover: flip target scan: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("embed-cutover: flip target row: %w", err)
		}
		mm, err := backends.ParseModelMap(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: backend %q: %w", ErrConfirmEmbedKeyNotFlippable, name, err)
		}
		if spec := mm[embedmigration.ModelMapKeyEmbedNext]; spec.Model != toModel {
			return nil, fmt.Errorf("%w: backend %q resolves %q, expected %q",
				ErrConfirmEmbedNextForeign, name, spec.Model, toModel)
		}
		if _, ok := mm[backends.RoleEmbed]; !ok {
			return nil, fmt.Errorf("%w: backend %q has no explicit 'embed' row (default-fallback rows cannot be flipped in place)",
				ErrConfirmEmbedKeyNotFlippable, name)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embed-cutover: flip target iterate: %w", err)
	}
	return names, nil
}

// flipEmbedModelMapSQL rewrites model_map on the validated targets inside
// the swap tx (§4.8 (4)): 'embed' → to_model (preserving an object row's
// params), 'embed_next' removed. The string/object CASE mirrors
// ParseModelMap's two admitted shapes. The UPDATE also fires the
// trg_backends_notify trigger at commit — the listener reload is the second
// net under the synchronous in-process Reload.
const flipEmbedModelMapSQL = `
	UPDATE context_backends
	   SET model_map = (model_map - 'embed_next') || jsonb_build_object('embed',
	           CASE WHEN jsonb_typeof(model_map->'embed') = 'object'
	                THEN jsonb_set(model_map->'embed', '{model}', to_jsonb($2::text))
	                ELSE to_jsonb($2::text) END),
	       updated_at = now()
	 WHERE name = ANY($1)`

// unflipEmbedModelMapSQL is the §4.10 mirror: 'embed' → from_model ($2),
// 'embed_next' re-armed with to_model ($3).
const unflipEmbedModelMapSQL = `
	UPDATE context_backends
	   SET model_map = model_map
	           || jsonb_build_object('embed',
	              CASE WHEN jsonb_typeof(model_map->'embed') = 'object'
	                   THEN jsonb_set(model_map->'embed', '{model}', to_jsonb($2::text))
	                   ELSE to_jsonb($2::text) END)
	           || jsonb_build_object('embed_next', jsonb_build_object('model', $3::text)),
	       updated_at = now()
	 WHERE name = ANY($1)`

// ConfirmEmbedMigration is the operator cutover (`verifying → done`,
// E-04-2): ONE transaction per the §4.8 choreography — lock_timeout, cache
// flush FIRST (DELETE, not TRUNCATE: row deletes keep the cache MVCC-
// readable and the later ACCESS-EXCLUSIVE window stays pure catalog writes,
// §4.9), completeness re-check under the lock with the IDENTICAL
// watermark+memo predicate as the verify gate (§5 Bruchpfad 4), the column/
// index RENAMEs including the (3b) partial-index class, the model_map flip
// as a data write, and the status CAS. After commit, synchronously:
// backend-pool reload (in-process — ctxd is a monolith), Nachlauf-Sweep
// over the provenance column, and the infinity-memo re-homing.
//
// The swap tx additionally re-adds a FRESH, empty embedding_next/
// embed_model_next pair (documented deviation from the §4.8 SQL sketch,
// with reason): the RENAME consumes the _next names, but ClearEmbeddingTx —
// on the hot content-update path — and the create/purge validation address
// embedding_next BY NAME; without the re-add every `manage update` after
// the cutover would 42703. The §3.2b invariant ("Dual-Spalten-Paar
// permanent angelegt") therefore holds at every instant; catalog-only ADD
// COLUMN of a NULL column costs no table rewrite. The rollback drops the
// fresh pair again (after proving it empty) before the inverse renames.
//
// On lock_timeout the whole tx aborts, status stays verifying, and a retry
// is a new ConfirmEmbedMigration call.
func ConfirmEmbedMigration(ctx context.Context, db *pgxpool.Pool, bpool *backends.Pool, migrationID string) (*EmbedCutoverResult, error) {
	if bpool == nil {
		return nil, ErrCutoverBackendPoolRequired
	}
	row, targets, err := confirmPreflight(ctx, db, migrationID)
	if err != nil {
		return nil, err
	}
	if err := runConfirmSwapTx(ctx, db, migrationID, row, targets); err != nil {
		return nil, err
	}

	res := &EmbedCutoverResult{
		MigrationID: migrationID, FromModel: row.fromModel, ToModel: row.toModel,
		VisibilityLoss: row.visibilityLoss, FlippedBackends: targets,
	}
	// (6) Post-commit, synchronous, each independent (a failure in one
	// never skips the others — they are separate convergence nets; errors
	// are joined and surfaced, the cutover itself is already committed).
	postErr := confirmPostCommit(ctx, db, bpool, migrationID, row, res)
	slog.Info("embed-cutover: CONFIRMED — serving space swapped",
		"migration_id", migrationID, "to_model", row.toModel,
		"visibility_loss", res.VisibilityLoss, "post_watermark_pending", res.PostWatermarkPending,
		"sweep_cleared", res.SweepCleared, "memos_copied", res.MemosCopied,
		"flipped_backends", strings.Join(targets, ","))
	return res, postErr
}

// confirmPreflight runs every §4.8 precondition BEFORE the swap tx opens:
// statemachine position, green verify_report, watermark, flippable target
// configuration, _next index readiness. Read-only — a refusal here has
// touched nothing.
func confirmPreflight(ctx context.Context, db *pgxpool.Pool, migrationID string) (*cutoverRow, []string, error) {
	row, err := loadCutoverRow(ctx, db, migrationID)
	if err != nil {
		return nil, nil, err
	}
	if row.status != embedmigration.StatusVerifying {
		return nil, nil, fmt.Errorf("%w (status %q)", ErrConfirmNotVerifying, row.status)
	}
	if row.verifyResult == nil {
		return nil, nil, ErrConfirmVerifyReportMissing
	}
	if *row.verifyResult != verifyGreen {
		return nil, nil, fmt.Errorf("%w (result %q)", ErrConfirmVerifyNotGreen, *row.verifyResult)
	}
	if row.watermark == nil {
		return nil, nil, ErrConfirmWatermarkMissing
	}

	targets, err := embedFlipTargets(ctx, db, row.toModel)
	if err != nil {
		return nil, nil, err
	}
	if len(targets) == 0 {
		return nil, nil, ErrConfirmNoFlipTarget
	}

	// All four _next indexes must exist VALID before their canonical
	// rename — renaming an INVALID CIC leftover into the canonical name
	// would be a silent planner hole behind a green-looking cutover.
	for _, name := range append([]string{verifyNextHNSWIndexName}, sisterIndexNames()...) {
		valid, exists, err := indexValidity(ctx, db, name)
		if err != nil {
			return nil, nil, err
		}
		if !exists || !valid {
			return nil, nil, fmt.Errorf("%w: %s (exists=%t, valid=%t)", ErrConfirmNextIndexNotReady, name, exists, valid)
		}
	}
	return row, targets, nil
}

// runConfirmSwapTx is the ONE §4.8 transaction (steps 1-5 of the sketch).
func runConfirmSwapTx(ctx context.Context, db *pgxpool.Pool, migrationID string, row *cutoverRow, targets []string) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("embed-cutover: begin swap tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+cutoverLockTimeout+`'`); err != nil {
		return fmt.Errorf("embed-cutover: set lock_timeout: %w", err)
	}
	// (1) Cache flush FIRST (§4.9): the up-to-50k row deletes must not run
	// under the later ACCESS-EXCLUSIVE lock on context_blocks — locks are
	// taken at first touch, so ordering IS the lock-window control.
	if _, err := tx.Exec(ctx, `DELETE FROM context_embed_cache`); err != nil {
		return fmt.Errorf("embed-cutover: cache flush: %w", err)
	}
	// (2) Completeness re-check under the tx — the IDENTICAL predicate as
	// §4.7 Stufe 1 (watermark scoping keeps this reachable under write
	// traffic — no livelock; the memo exception keeps declared skips from
	// blocking forever). >0 → the deferred rollback undoes the flush too.
	var pending int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE `+verifyCompletenessWhere(true),
		migrationID, *row.watermark).Scan(&pending); err != nil {
		return fmt.Errorf("embed-cutover: completeness re-check: %w", err)
	}
	if pending > 0 {
		return fmt.Errorf("%w (%d pending)", ErrConfirmIncomplete, pending)
	}
	// (3)+(3b) Catalog swap.
	for _, r := range cutoverColumnRenames {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE context_blocks RENAME COLUMN %s TO %s`, r[0], r[1])); err != nil {
			return fmt.Errorf("embed-cutover: rename column %s -> %s: %w", r[0], r[1], err)
		}
	}
	for _, r := range cutoverIndexRenames {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`ALTER INDEX %s RENAME TO %s`, r[0], r[1])); err != nil {
			return fmt.Errorf("embed-cutover: rename index %s -> %s: %w", r[0], r[1], err)
		}
	}
	// Fresh dual pair (documented deviation, see function comment).
	if _, err := tx.Exec(ctx,
		`ALTER TABLE context_blocks ADD COLUMN embedding_next vector(1024),
		                            ADD COLUMN embed_model_next TEXT`); err != nil {
		return fmt.Errorf("embed-cutover: re-add fresh _next pair: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`ALTER TABLE context_blocks ALTER COLUMN embedding_next SET STORAGE EXTERNAL`); err != nil {
		return fmt.Errorf("embed-cutover: fresh _next storage: %w", err)
	}
	// (4) model_map flip as a data write IN the tx (§4.8 choreography
	// decision — no operator-paced mixed-space window).
	tag, err := tx.Exec(ctx, flipEmbedModelMapSQL, targets, row.toModel)
	if err != nil {
		return fmt.Errorf("embed-cutover: model_map flip: %w", err)
	}
	if tag.RowsAffected() != int64(len(targets)) {
		return fmt.Errorf("%w (%d of %d)", ErrConfirmFlipRowCountMismatch, tag.RowsAffected(), len(targets))
	}
	// (5) Statemachine CAS — 0 rows = a concurrent transition won; the
	// deferred rollback aborts the whole cutover (§4.8).
	if err := embedmigration.Transition(ctx, tx, migrationID,
		embedmigration.StatusVerifying, embedmigration.StatusDone,
		embedmigration.WithFinishedAt()); err != nil {
		return fmt.Errorf("embed-cutover: confirm CAS: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("embed-cutover: commit swap tx: %w", err)
	}

	return nil
}

// confirmPostCommit runs the §4.8 step-(6) convergence nets after the swap
// tx committed: synchronous in-process pool reload, Nachlauf-Sweep,
// infinity-memo re-homing and the post-swap transient count. Each step is
// independent — errors are joined, never short-circuited: the cutover is
// already committed and every remaining net still narrows the window.
func confirmPostCommit(ctx context.Context, db *pgxpool.Pool, bpool *backends.Pool, migrationID string, row *cutoverRow, res *EmbedCutoverResult) error {
	var postErrs []error
	var err error
	if err = bpool.Reload(ctx); err != nil {
		postErrs = append(postErrs, fmt.Errorf("embed-cutover: post-commit pool reload (listener NOTIFY is the fallback net): %w", err))
	}
	if res.SweepCleared, err = sweepForeignServingVectors(ctx, db, row.toModel); err != nil {
		postErrs = append(postErrs, err)
	}
	if res.MemosCopied, err = copyInfinityMemosToBackfill(ctx, db, migrationID); err != nil {
		postErrs = append(postErrs, err)
	}
	if res.PostWatermarkPending, err = countPostSwapPending(ctx, db); err != nil {
		postErrs = append(postErrs, err)
	}
	return errors.Join(postErrs...)
}

// RollbackEmbedMigration is the §4.10 mirror tx (`done → rolled_back`, the
// ONE documented terminal exception): fail-closed preflight on every _old
// resource, then ONE tx — cache flush first, fresh-_next-pair drop, inverse
// renames (columns, HNSW, partial class), model_map un-flip, CAS with the
// MANDATORY rollback_reason — then synchronous reload + inverse sweep
// (serving vectors labeled != from_model are cleared).
func RollbackEmbedMigration(ctx context.Context, db *pgxpool.Pool, bpool *backends.Pool, migrationID, reason string) (*EmbedRollbackResult, error) {
	if bpool == nil {
		return nil, ErrCutoverBackendPoolRequired
	}
	// Reason gate BEFORE any read or write (§4.10: "leerer Grund = Fehler
	// vor jedem Write") — the Transition CAS would also enforce it, but by
	// then the cache flush would already sit in the tx.
	if strings.TrimSpace(reason) == "" {
		return nil, ErrRollbackReasonRequired
	}
	row, targets, err := rollbackPreflight(ctx, db, migrationID)
	if err != nil {
		return nil, err
	}
	if err := runRollbackSwapTx(ctx, db, migrationID, reason, row, targets); err != nil {
		return nil, err
	}

	res := &EmbedRollbackResult{
		MigrationID: migrationID, FromModel: row.fromModel, ToModel: row.toModel,
		FlippedBackends: targets,
	}
	var postErrs []error
	if err := bpool.Reload(ctx); err != nil {
		postErrs = append(postErrs, fmt.Errorf("embed-cutover: post-rollback pool reload (listener NOTIFY is the fallback net): %w", err))
	}
	if res.SweepCleared, err = sweepForeignServingVectors(ctx, db, row.fromModel); err != nil {
		postErrs = append(postErrs, err)
	}
	slog.Warn("embed-cutover: ROLLED BACK — serving space restored",
		"migration_id", migrationID, "from_model", row.fromModel,
		"reason", reason, "sweep_cleared", res.SweepCleared)
	return res, errors.Join(postErrs...)
}

// rollbackPreflight runs every §4.10 fail-closed precondition read-only:
// statemachine position, no concurrent non-terminal migration, ALL _old
// anchor resources present, the fresh _next pair empty, and the un-flip
// target set. A refusal here has touched nothing.
func rollbackPreflight(ctx context.Context, db *pgxpool.Pool, migrationID string) (*cutoverRow, []string, error) {
	row, err := loadCutoverRow(ctx, db, migrationID)
	if err != nil {
		return nil, nil, err
	}
	if row.status != embedmigration.StatusDone {
		return nil, nil, fmt.Errorf("%w (status %q)", ErrRollbackNotDone, row.status)
	}
	active, err := embedmigration.Active(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	if active != nil {
		return nil, nil, fmt.Errorf("%w (id %s, status %s)", ErrRollbackActiveMigration, active.ID, active.Status)
	}
	missing, err := missingOldResources(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrRollbackOldResourcesMissing, strings.Join(missing, ", "))
	}
	// The fresh _next pair the confirm re-added must be EMPTY — data in it
	// belongs to someone (a follow-up migration) and is never silently
	// destroyed by the drop in the swap tx.
	nextExists, err := columnExists(ctx, db, "embedding_next")
	if err != nil {
		return nil, nil, err
	}
	if nextExists {
		var hasData bool
		if err := db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM context_blocks WHERE embedding_next IS NOT NULL OR embed_model_next IS NOT NULL)`,
		).Scan(&hasData); err != nil {
			return nil, nil, fmt.Errorf("embed-cutover: fresh _next data probe: %w", err)
		}
		if hasData {
			return nil, nil, ErrRollbackNextDataPresent
		}
	}
	// Un-flip targets: RoleEmbed backends whose explicit 'embed' row serves
	// to_model (the post-confirm state). May legitimately be empty — the
	// operator may have re-pointed backends since; the rollback of the DATA
	// side must not be hostage to the config side (only the _old resources
	// are the hard gate, §4.10).
	targets, err := embedUnflipTargets(ctx, db, row.toModel)
	if err != nil {
		return nil, nil, err
	}
	return row, targets, nil
}

// runRollbackSwapTx is the §4.10 mirror transaction of runConfirmSwapTx.
func runRollbackSwapTx(ctx context.Context, db *pgxpool.Pool, migrationID, reason string, row *cutoverRow, targets []string) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("embed-cutover: begin rollback tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+cutoverLockTimeout+`'`); err != nil {
		return fmt.Errorf("embed-cutover: set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM context_embed_cache`); err != nil {
		return fmt.Errorf("embed-cutover: rollback cache flush: %w", err)
	}
	// Vacate the _next names for the inverse renames (pair proven empty).
	if _, err := tx.Exec(ctx,
		`ALTER TABLE context_blocks DROP COLUMN IF EXISTS embedding_next,
		                            DROP COLUMN IF EXISTS embed_model_next`); err != nil {
		return fmt.Errorf("embed-cutover: drop fresh _next pair: %w", err)
	}
	for i := len(cutoverColumnRenames) - 1; i >= 0; i-- {
		r := cutoverColumnRenames[i]
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE context_blocks RENAME COLUMN %s TO %s`, r[1], r[0])); err != nil {
			return fmt.Errorf("embed-cutover: rename column %s -> %s: %w", r[1], r[0], err)
		}
	}
	for i := len(cutoverIndexRenames) - 1; i >= 0; i-- {
		r := cutoverIndexRenames[i]
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`ALTER INDEX %s RENAME TO %s`, r[1], r[0])); err != nil {
			return fmt.Errorf("embed-cutover: rename index %s -> %s: %w", r[1], r[0], err)
		}
	}
	if len(targets) > 0 {
		if _, err := tx.Exec(ctx, unflipEmbedModelMapSQL, targets, row.fromModel, row.toModel); err != nil {
			return fmt.Errorf("embed-cutover: model_map un-flip: %w", err)
		}
	}
	if err := embedmigration.Transition(ctx, tx, migrationID,
		embedmigration.StatusDone, embedmigration.StatusRolledBack,
		embedmigration.WithRollbackReason(reason)); err != nil {
		return fmt.Errorf("embed-cutover: rollback CAS: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("embed-cutover: commit rollback tx: %w", err)
	}
	return nil
}

// CleanupEmbedMigration drops the rollback anchor (§4.8 Alt-Bestand): the
// four _old indexes BY NAME, then the two _old columns. Operator-explicit,
// only from status done (E-04-2: cleanup destroys the instant-rollback
// path — after this, RollbackEmbedMigration refuses via its _old preflight
// and the only way back is a NEW migration in the opposite direction,
// §4.10). Storage returns via vacuum. The fresh embedding_next pair the
// confirm re-added stays — it IS the §3.2b permanent dual pair for the
// next migration.
func CleanupEmbedMigration(ctx context.Context, db *pgxpool.Pool, migrationID string) error {
	row, err := loadCutoverRow(ctx, db, migrationID)
	if err != nil {
		return err
	}
	if row.status != embedmigration.StatusDone {
		return fmt.Errorf("%w (status %q)", ErrCleanupNotDone, row.status)
	}
	missing, err := missingOldResources(ctx, db)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrCleanupOldResourcesMissing, strings.Join(missing, ", "))
	}

	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("embed-cutover: begin cleanup tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+cutoverLockTimeout+`'`); err != nil {
		return fmt.Errorf("embed-cutover: set lock_timeout: %w", err)
	}
	// Named drops first (documented, never the silent DROP-COLUMN cascade
	// — §4.8: "die _old-Exemplare fallen DOKUMENTIERT, nicht still").
	for _, name := range cutoverOldIndexNames {
		if _, err := tx.Exec(ctx, `DROP INDEX `+name); err != nil {
			return fmt.Errorf("embed-cutover: drop index %s: %w", name, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`ALTER TABLE context_blocks DROP COLUMN embedding_old, DROP COLUMN embed_model_old`); err != nil {
		return fmt.Errorf("embed-cutover: drop _old columns: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("embed-cutover: commit cleanup tx: %w", err)
	}
	slog.Info("embed-cutover: cleanup done — rollback anchor dropped, storage returns via vacuum",
		"migration_id", migrationID,
		"dropped_indexes", strings.Join(cutoverOldIndexNames, ","),
		"dropped_columns", strings.Join(cutoverOldColumnNames, ","))
	return nil
}

// sweepForeignServingVectors is the Nachlauf-Sweep (§4.8 / §5 Bruchpfad 6):
// clear every live serving vector whose M109 provenance label is not
// wantModel — reload-latency window writes and in-flight straddlers that
// materialized as writes. Full ClearEmbedding semantics, set-based (both
// pairs nulled + the backfill memos of the cleared blocks deleted) so the
// sweep stays one statement even in a pathological window.
func sweepForeignServingVectors(ctx context.Context, db *pgxpool.Pool, wantModel string) (int64, error) {
	rows, err := db.Query(ctx,
		`UPDATE context_blocks
		 SET embedding = NULL, embed_model = NULL,
		     embedding_next = NULL, embed_model_next = NULL
		 WHERE embedding IS NOT NULL AND embed_model IS DISTINCT FROM $1 AND NOT is_archived
		 RETURNING id::text`, wantModel)
	if err != nil {
		return 0, fmt.Errorf("embed-cutover: sweep: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("embed-cutover: sweep row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("embed-cutover: sweep iterate: %w", err)
	}
	if len(ids) > 0 {
		if _, err := db.Exec(ctx,
			`DELETE FROM context_embed_failures WHERE migration_id IS NULL AND block_id::text = ANY($1)`,
			ids); err != nil {
			return int64(len(ids)), fmt.Errorf("embed-cutover: sweep memo delete: %w", err)
		}
		slog.Info("embed-cutover: sweep cleared foreign-labeled serving vectors",
			"count", len(ids), "want_model", wantModel)
	}
	return int64(len(ids)), nil
}

// copyInfinityMemosToBackfill re-homes this migration's infinity memos
// (oversize / sensitivity_ineligible parks) as backfill memos —
// migration_id NULL, next_attempt_at infinity (§4.8 Restmengen): after the
// swap those blocks match the regular pending predicate (serving column
// NULL), and WITHOUT a backfill-scoped park Pfad A/B would wire-retry them
// forever against the same structural limit. COPY, not move: the
// migration-scoped originals stay as that migration's bookkeeping (§5
// Bruchpfad 10 keeps the copied rows deliberately persistent + visible).
func copyInfinityMemosToBackfill(ctx context.Context, db *pgxpool.Pool, migrationID string) (int64, error) {
	tag, err := db.Exec(ctx,
		`INSERT INTO context_embed_failures
		     (block_id, migration_id, attempts, last_error, last_class, next_attempt_at, first_seen)
		 SELECT block_id, NULL, attempts, last_error, last_class, 'infinity', first_seen
		   FROM context_embed_failures
		  WHERE migration_id = $1::uuid AND next_attempt_at = 'infinity'
		 ON CONFLICT (block_id) WHERE migration_id IS NULL
		 DO UPDATE SET last_error      = EXCLUDED.last_error,
		               last_class      = EXCLUDED.last_class,
		               next_attempt_at = 'infinity'`,
		migrationID)
	if err != nil {
		return 0, fmt.Errorf("embed-cutover: memo copy: %w", err)
	}
	return tag.RowsAffected(), nil
}

// countPostSwapPending quantifies the Nach-Watermark-Transiente (§4.8
// Restmengen): live blocks whose serving column is NULL after the swap and
// that are not permanently parked — exactly the set the regular backfill
// (capped Pfad A + Pfad B) converges with to_model.
func countPostSwapPending(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	var n int64
	err := db.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks cb
		 WHERE cb.embedding IS NULL AND NOT cb.is_archived
		   AND NOT EXISTS (SELECT 1 FROM context_embed_failures f
		                   WHERE f.block_id = cb.id AND f.migration_id IS NULL
		                     AND f.next_attempt_at = 'infinity')`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("embed-cutover: post-swap pending count: %w", err)
	}
	return n, nil
}

// embedUnflipTargets selects the rollback's model_map targets: RoleEmbed
// backends whose EXPLICIT 'embed' row serves toModel (the post-confirm
// state). Backends resolving toModel only via the default fallback carry no
// updatable row and are deliberately left alone (mirror of the confirm's
// "eindeutig updatebar" rule).
func embedUnflipTargets(ctx context.Context, q embedmigration.Querier, toModel string) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT name, model_map FROM context_backends
		 WHERE $1 = ANY(roles) AND model_map ? $1
		 ORDER BY name`,
		backends.RoleEmbed)
	if err != nil {
		return nil, fmt.Errorf("embed-cutover: un-flip target scan: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("embed-cutover: un-flip target row: %w", err)
		}
		mm, err := backends.ParseModelMap(raw)
		if err != nil {
			// Unparseable model_map on the way BACK: skip, never block the
			// rollback of the data side on a config row the confirm did not
			// write (the operator edited it since; W22: the hard gate is
			// the _old resources, not the config mirror).
			slog.Warn("embed-cutover: un-flip skipping unparseable model_map", "backend", name, "error", err)
			continue
		}
		if spec, ok := mm[backends.RoleEmbed]; ok && spec.Model == toModel {
			names = append(names, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embed-cutover: un-flip target iterate: %w", err)
	}
	return names, nil
}

// missingOldResources reports which rollback-anchor objects are absent —
// shared preflight of rollback (refuse: no anchor) and cleanup (refuse:
// nothing/partial to drop; a partial state is operator territory, never a
// best-effort drop).
func missingOldResources(ctx context.Context, q embedmigration.Querier) ([]string, error) {
	var missing []string
	for _, col := range cutoverOldColumnNames {
		ok, err := columnExists(ctx, q, col)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, "column "+col)
		}
	}
	for _, idx := range cutoverOldIndexNames {
		_, exists, err := indexValidity(ctx, q, idx)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, "index "+idx)
		}
	}
	return missing, nil
}

// columnExists probes a live (non-dropped) context_blocks column.
func columnExists(ctx context.Context, q embedmigration.Querier, column string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_attribute
		 WHERE attrelid = 'context_blocks'::regclass AND attname = $1
		   AND attnum > 0 AND NOT attisdropped)`, column).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("embed-cutover: column probe (%s): %w", column, err)
	}
	return ok, nil
}

// indexValidity reports existence and pg_index.indisvalid for an index name.
func indexValidity(ctx context.Context, q embedmigration.Querier, name string) (valid, exists bool, err error) {
	err = q.QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = $1`, name).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("embed-cutover: index probe (%s): %w", name, err)
	}
	return valid, true, nil
}

// sisterIndexNames lists the three sister partial indexes (W04-5 builds
// them, the swap tx renames them canonically).
func sisterIndexNames() []string {
	names := make([]string, 0, len(verifySisterIndexes))
	for _, s := range verifySisterIndexes {
		names = append(names, s.name)
	}
	return names
}
