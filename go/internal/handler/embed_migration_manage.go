// Evokoa-Clean-Room design/04 §7 W04-7: the server-admin control surface of the
// re-embed migration statemachine. All actions ride /api/manage as the
// embed-migration-* family (tierServerAdmin, S9-pinned) and are thin transports
// over the already-built mechanics: embedmigration.Create/Transition/Purge
// (statemachine + create validation, W04-3) and events.Confirm/Rollback/Cleanup
// EmbedMigration (cutover/rollback/cleanup transactions, W04-6). The arm itself
// (migrateOneEmbedding, W04-4) and the verify gate (W04-5) run on their own — the
// operator only steers status transitions here.
//
// Metadaten-Seite (§5 Bruchpfad 9): the RICH status view — verify_report WITH
// block-IDs over ALL scopes, both infinity sets, and the on-demand exact index
// count — is reachable ONLY through this admin-gated endpoint, NEVER through
// /api/status (whose embed_migration field is the slim, block-ID-free frame,
// status.go). context_embed_failures is likewise server-admin-only (§5 Bruchpfad
// 10): the embed-migration-failures action is the only read path.
//
// Mechanik-Fehler werden 1:1 als benannter Fehlertext durchgereicht: a
// fail-closed refusal (ErrActiveMigrationExists, ErrConfirmIncomplete, the model
// guard, …) is operator information, not an internal error to be masked.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/events"
	"github.com/jackc/pgx/v5"
)

// embedMigrationDiskCheckPath is the volume the create pre-flight disk check
// statfs's (design §6.1). ctxd runs as its own container that does NOT bind-mount
// the DB data volume (docker-compose.yml: only n8n-db-1 mounts ./db), so a
// truthful cross-container reading of the exact PGDATA free space is not
// available from this process. On the single-host RAID-0 deployment the ctx
// container's own root filesystem sits on the SAME physical array as the DB bind
// mount, so "/" is a real, same-order lower-bound guard — exactly what this
// fail-closed "refuse to start blind" pre-flight needs (it is a safety floor, not
// exact accounting). The cross-container caveat is documented in the operator
// runbook (docs/operations.md).
const embedMigrationDiskCheckPath = "/"

// embedMigrationData is the req.Data payload shared by the embed-migration-*
// actions (only the fields an action reads are populated). Kept as one struct so
// the dispatcher parses once; unknown fields are ignored (forward-compatible).
type embedMigrationData struct {
	FromModel     string `json:"from_model"`
	ToModel       string `json:"to_model"`
	ToBackend     string `json:"to_backend"`
	ReuseExisting bool   `json:"reuse_existing"`
	Reason        string `json:"reason"`
	Exact         bool   `json:"exact"`
	Limit         int    `json:"limit"`
}

// embedMigrationView is the RICH status wire shape (manage-only). It carries
// verify_report VERBATIM (block-IDs over all scopes, §5 Bruchpfad 9) and both
// infinity sets — the block-ID-free slim frame is the /api/status embed_migration
// field (status.go). pending is the §6.3 arithmetic (total − migrated − failed −
// skipped, NEVER a count(*) on context_blocks); pending_exact is the on-demand
// index count (§6.3 "--exact"), null unless requested.
type embedMigrationView struct {
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	FromModel         string          `json:"from_model"`
	ToModel           string          `json:"to_model"`
	ToBackend         string          `json:"to_backend"`
	Mode              string          `json:"mode"`
	TotalBlocks       int64           `json:"total_blocks"`
	MigratedCount     int64           `json:"migrated_count"`
	FailedCount       int64           `json:"failed_count"`
	SkippedCount      int64           `json:"skipped_count"`
	Pending           int64           `json:"pending"`
	PendingExact      *int64          `json:"pending_exact"`
	InfinityMigration int64           `json:"infinity_migration"`
	InfinityBackfill  int64           `json:"infinity_backfill"`
	CursorCreatedAt   *time.Time      `json:"cursor_created_at"`
	VerifyStartedAt   *time.Time      `json:"verify_started_at"`
	VerifyReport      json.RawMessage `json:"verify_report"`
	LastError         *string         `json:"last_error"`
	AbortReason       *string         `json:"abort_reason"`
	RollbackReason    *string         `json:"rollback_reason"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at"`
	FinishedAt        *time.Time      `json:"finished_at"`
}

// embedFailureRow is one context_embed_failures row in the admin-only failures
// list (§5 Bruchpfad 10). last_error is ALREADY normalized in the worker
// (store.NormalizeEmbedError — class + HTTP status + ≤500 chars, never embed
// text), so surfacing it here leaks no content; the table stays admin-only for
// the block-ID/topology dimension.
type embedFailureRow struct {
	BlockID     string  `json:"block_id"`
	MigrationID *string `json:"migration_id"`
	Attempts    int     `json:"attempts"`
	LastError   string  `json:"last_error"`
	LastClass   string  `json:"last_class"`
	// NextAttemptAt is rendered as text (::text in SQL): the parked-forever
	// memos carry the literal 'infinity', which pgx cannot scan into time.Time
	// — a string keeps both the timestamp and the 'infinity' sentinel readable.
	NextAttemptAt string    `json:"next_attempt_at"`
	FirstSeen     time.Time `json:"first_seen"`
}

// dispatchEmbedMigrationAction fans the embed-migration-* family out (split from
// HandleManage's switch for the cyclomatic budget — mirrors dispatchBackendAction
// / dispatchAPIKeyAction). All arms are server-admin-gated upstream (actionTier).
func (h *ManageHandler) dispatchEmbedMigrationAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "embed-migration-create":
		h.handleEmbedMigrationCreate(w, r, req)
	case "embed-migration-status":
		h.handleEmbedMigrationStatus(w, r, req)
	case "embed-migration-pause":
		h.handleEmbedMigrationTransition(w, r, req, embedmigration.StatusRunning, embedmigration.StatusPaused)
	case "embed-migration-resume":
		// resume/start is →running from the CURRENT status: valid from both
		// pending (the initial start — the worker is idle in pending, §4.1, so
		// the operator moves it) AND paused (resume). A fixed from would only
		// cover one; reading current + IsAllowedTransition covers both and
		// surfaces a wrong-state attempt verbatim.
		h.handleEmbedMigrationResume(w, r, req)
	case "embed-migration-abort":
		h.handleEmbedMigrationAbort(w, r, req)
	case "embed-migration-confirm":
		h.handleEmbedMigrationConfirm(w, r, req)
	case "embed-migration-rollback":
		h.handleEmbedMigrationRollback(w, r, req)
	case "embed-migration-cleanup":
		h.handleEmbedMigrationCleanup(w, r, req)
	case "embed-migration-purge":
		h.handleEmbedMigrationPurge(w, r)
	case "embed-migration-failures":
		h.handleEmbedMigrationFailures(w, r, req)
	}
}

// parseEmbedMigrationData unmarshals req.Data into embedMigrationData (empty on
// absent data — every field is optional per action).
func parseEmbedMigrationData(req manageRequest) embedMigrationData {
	var d embedMigrationData
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &d)
	}
	return d
}

// embedMigrationMechErr maps a mechanics-layer error onto an HTTP status and
// surfaces its text VERBATIM (design §7: "Fehler der Mechanik-Schicht 1:1 als
// benannte Fehlertexte durchreichen"). Validation/precondition refusals are 400,
// the single-active race is 409 (conflict), not-found is 404; anything else is a
// genuine 500. errors.As on *embedmigration.DiskEstimate keeps the actual byte
// numbers in the returned text.
func embedMigrationMechErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, embedmigration.ErrActiveMigrationExists):
		status = http.StatusConflict
	case errors.Is(err, events.ErrCutoverMigrationNotFound):
		status = http.StatusNotFound
	case errors.Is(err, embedmigration.ErrModelsIdentical),
		errors.Is(err, embedmigration.ErrFromModelUnknown),
		errors.Is(err, embedmigration.ErrToModelUnknown),
		errors.Is(err, embedmigration.ErrDimsMismatch),
		errors.Is(err, embedmigration.ErrBackendNotFound),
		errors.Is(err, embedmigration.ErrBackendNotLocal),
		errors.Is(err, embedmigration.ErrBackendNotGlobalScoped),
		errors.Is(err, embedmigration.ErrBackendMissingEmbedNext),
		errors.Is(err, embedmigration.ErrRestEmbeddingNextData),
		errors.Is(err, embedmigration.ErrReuseModelMismatch),
		errors.Is(err, embedmigration.ErrReuseAfterRollback),
		errors.Is(err, embedmigration.ErrReuseRequiresAborted),
		errors.Is(err, embedmigration.ErrDiskCheckFailed),
		errors.Is(err, embedmigration.ErrDiskInsufficient),
		errors.Is(err, embedmigration.ErrTransitionNotAllowed),
		errors.Is(err, embedmigration.ErrReasonRequired),
		errors.Is(err, embedmigration.ErrTransitionRaceLost),
		errors.Is(err, embedmigration.ErrPurgeActiveMigration),
		errors.Is(err, events.ErrConfirmNotVerifying),
		errors.Is(err, events.ErrConfirmVerifyReportMissing),
		errors.Is(err, events.ErrConfirmVerifyNotGreen),
		errors.Is(err, events.ErrConfirmWatermarkMissing),
		errors.Is(err, events.ErrConfirmNoFlipTarget),
		errors.Is(err, events.ErrConfirmEmbedKeyNotFlippable),
		errors.Is(err, events.ErrConfirmEmbedNextForeign),
		errors.Is(err, events.ErrConfirmNextIndexNotReady),
		errors.Is(err, events.ErrConfirmIncomplete),
		errors.Is(err, events.ErrConfirmFlipRowCountMismatch),
		errors.Is(err, events.ErrRollbackReasonRequired),
		errors.Is(err, events.ErrRollbackNotDone),
		errors.Is(err, events.ErrRollbackActiveMigration),
		errors.Is(err, events.ErrRollbackOldResourcesMissing),
		errors.Is(err, events.ErrRollbackNextDataPresent),
		errors.Is(err, events.ErrCleanupNotDone),
		errors.Is(err, events.ErrCleanupOldResourcesMissing),
		errors.Is(err, events.ErrCutoverBackendPoolRequired):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"success": false, "error": err.Error()})
}

// handleEmbedMigrationCreate validates + inserts a new migration row
// (embedmigration.Create) with a REAL statfs disk pre-flight. On success it
// returns the new id; every validation refusal is surfaced verbatim.
func (h *ManageHandler) handleEmbedMigrationCreate(w http.ResponseWriter, r *http.Request, req manageRequest) {
	d := parseEmbedMigrationData(req)
	id, err := embedmigration.Create(r.Context(), h.pool, embedmigration.CreateParams{
		FromModel:     d.FromModel,
		ToModel:       d.ToModel,
		ToBackend:     d.ToBackend,
		ReuseExisting: d.ReuseExisting,
	}, embedmigration.StatfsChecker(embedMigrationDiskCheckPath))
	if err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
}

// resolveMigrationID returns req.ID when set, else the single active migration's
// id (idx_embed_migration_single_active guarantees at most one). ok=false with a
// 404 already written when neither is available — the caller returns.
func (h *ManageHandler) resolveMigrationID(w http.ResponseWriter, ctx context.Context, req manageRequest) (string, bool) {
	if req.ID != "" {
		return req.ID, true
	}
	m, err := embedmigration.Active(ctx, h.pool)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return "", false
	}
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "embed-migration: no active migration and no id given",
		})
		return "", false
	}
	return m.ID, true
}

// handleEmbedMigrationStatus reads the target row (req.ID, else the active one)
// and renders the rich view: arithmetic pending (§6.3), both infinity sets,
// verify_report WITH block-IDs, and — only on data.exact — the true index count.
// With no id and no active migration it returns {migration:null} (not an error:
// "there is nothing running" is a valid, queryable state).
func (h *ManageHandler) handleEmbedMigrationStatus(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	d := parseEmbedMigrationData(req)

	id := req.ID
	if id == "" {
		m, err := embedmigration.Active(ctx, h.pool)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
			return
		}
		if m == nil {
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "migration": nil})
			return
		}
		id = m.ID
	}

	v, err := h.loadEmbedMigrationView(ctx, id, d.Exact)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "embed-migration: migration id not found",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "migration": v})
}

// loadEmbedMigrationView reads one context_embed_migrations row into the rich
// view. pending is derived arithmetically (§6.3, clamped at 0 — ClearEmbedding
// re-pending drift can only push it low, never below zero on the display); the
// exact index count runs ONLY when requested (a scan the Verify gate normally
// owns). The infinity counts are two indexed count(*) over context_embed_failures.
func (h *ManageHandler) loadEmbedMigrationView(ctx context.Context, id string, exact bool) (*embedMigrationView, error) {
	v := &embedMigrationView{}
	err := h.pool.QueryRow(ctx,
		`SELECT id::text, status, from_model, to_model, to_backend, mode,
		        total_blocks, migrated_count, failed_count, skipped_count,
		        cursor_created_at, verify_started_at, verify_report,
		        last_error, abort_reason, rollback_reason,
		        created_at, started_at, finished_at
		 FROM context_embed_migrations WHERE id = $1::uuid`, id,
	).Scan(&v.ID, &v.Status, &v.FromModel, &v.ToModel, &v.ToBackend, &v.Mode,
		&v.TotalBlocks, &v.MigratedCount, &v.FailedCount, &v.SkippedCount,
		&v.CursorCreatedAt, &v.VerifyStartedAt, &v.VerifyReport,
		&v.LastError, &v.AbortReason, &v.RollbackReason,
		&v.CreatedAt, &v.StartedAt, &v.FinishedAt)
	if err != nil {
		return nil, err
	}
	v.Pending = arithmeticPending(v.TotalBlocks, v.MigratedCount, v.FailedCount, v.SkippedCount)

	// Both infinity sets (§4.4): the migration-scoped parked set and the
	// backfill-scoped one (Pfad A/B oversize memos live independently of any
	// migration). Two single-index count(*) — cheap enough for a manual read.
	_ = h.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_embed_failures
		 WHERE migration_id = $1::uuid AND next_attempt_at = 'infinity'`, id,
	).Scan(&v.InfinityMigration)
	_ = h.pool.QueryRow(ctx,
		`SELECT count(*) FROM context_embed_failures
		 WHERE migration_id IS NULL AND next_attempt_at = 'infinity'`,
	).Scan(&v.InfinityBackfill)

	if exact {
		var n int64
		if err := h.pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks cb
			 WHERE cb.embedding IS NOT NULL AND cb.embedding_next IS NULL AND NOT cb.is_archived
			   AND NOT EXISTS (SELECT 1 FROM context_embed_failures f
			                   WHERE f.block_id = cb.id AND f.migration_id = $1::uuid
			                     AND f.next_attempt_at = 'infinity')`, id,
		).Scan(&n); err == nil {
			v.PendingExact = &n
		}
	}
	return v, nil
}

// arithmeticPending is the §6.3 hot-row-free pending: total − migrated − failed −
// skipped, floored at 0. NEVER a count(*) on context_blocks — the whole point of
// §6.3 is that the status surface must not scan the pending partial index (which
// holds ~10M entries for most of a migration).
func arithmeticPending(total, migrated, failed, skipped int64) int64 {
	p := total - migrated - failed - skipped
	if p < 0 {
		return 0
	}
	return p
}

// handleEmbedMigrationTransition drives the fixed-from CAS transitions pause
// (running→paused) and resume (paused→running). Transition's CAS surfaces a lost
// race (status already moved) verbatim — fail-closed, no silent no-op.
func (h *ManageHandler) handleEmbedMigrationTransition(w http.ResponseWriter, r *http.Request, req manageRequest, from, to embedmigration.Status) {
	ctx := r.Context()
	id, ok := h.resolveMigrationID(w, ctx, req)
	if !ok {
		return
	}
	if err := embedmigration.Transition(ctx, h.pool, id, from, to); err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "status": string(to)})
}

// handleEmbedMigrationResume transitions the active migration to running from
// its CURRENT status — the start (pending→running) and the resume
// (paused→running) share one action. Reads current, then CAS current→running;
// IsAllowedTransition rejects any other current status verbatim
// (ErrTransitionNotAllowed). started_at is stamped on the pending→running edge.
func (h *ManageHandler) handleEmbedMigrationResume(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	id, ok := h.resolveMigrationID(w, ctx, req)
	if !ok {
		return
	}
	cur, ok := h.loadMigrationStatus(w, ctx, id)
	if !ok {
		return
	}
	var opts []embedmigration.TransitionOption
	if cur == embedmigration.StatusPending {
		opts = append(opts, embedmigration.WithStartedAt())
	}
	if err := embedmigration.Transition(ctx, h.pool, id, cur, embedmigration.StatusRunning, opts...); err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "status": string(embedmigration.StatusRunning)})
}

// loadMigrationStatus reads one migration row's current status. ok=false with the
// HTTP error already written when the row is missing or the read fails.
func (h *ManageHandler) loadMigrationStatus(w http.ResponseWriter, ctx context.Context, id string) (embedmigration.Status, bool) {
	var cur string
	if err := h.pool.QueryRow(ctx,
		`SELECT status FROM context_embed_migrations WHERE id = $1::uuid`, id,
	).Scan(&cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "embed-migration: migration id not found"})
			return "", false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return "", false
	}
	return embedmigration.Status(cur), true
}

// handleEmbedMigrationAbort transitions the active migration to aborted from its
// CURRENT status (allowed from pending/running/paused/verifying). abort_reason is
// mandatory (WithAbortReason → ErrReasonRequired when empty, surfaced verbatim).
func (h *ManageHandler) handleEmbedMigrationAbort(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	d := parseEmbedMigrationData(req)
	id, ok := h.resolveMigrationID(w, ctx, req)
	if !ok {
		return
	}
	cur, ok := h.loadMigrationStatus(w, ctx, id)
	if !ok {
		return
	}
	if err := embedmigration.Transition(ctx, h.pool, id, cur, embedmigration.StatusAborted,
		embedmigration.WithAbortReason(d.Reason)); err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "status": string(embedmigration.StatusAborted)})
}

// handleEmbedMigrationConfirm runs the cutover (events.ConfirmEmbedMigration) and
// returns the operator payload: visibility_loss, post-watermark transient, sweep
// count, memos re-homed, flipped backends (§4.8 Restmengen).
func (h *ManageHandler) handleEmbedMigrationConfirm(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	id, ok := h.resolveMigrationID(w, ctx, req)
	if !ok {
		return
	}
	res, err := events.ConfirmEmbedMigration(ctx, h.pool, h.backendPool, id)
	if err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":                true,
		"id":                     res.MigrationID,
		"from_model":             res.FromModel,
		"to_model":               res.ToModel,
		"visibility_loss":        res.VisibilityLoss,
		"post_watermark_pending": res.PostWatermarkPending,
		"sweep_cleared":          res.SweepCleared,
		"memos_copied":           res.MemosCopied,
		"flipped_backends":       res.FlippedBackends,
	})
}

// handleEmbedMigrationRollback runs the post-cutover rollback
// (events.RollbackEmbedMigration) — rollback_reason mandatory, surfaced verbatim
// when absent (ErrRollbackReasonRequired, refused before any write).
func (h *ManageHandler) handleEmbedMigrationRollback(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	d := parseEmbedMigrationData(req)
	id, ok := h.resolveMigrationID(w, ctx, req)
	if !ok {
		return
	}
	res, err := events.RollbackEmbedMigration(ctx, h.pool, h.backendPool, id, d.Reason)
	if err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":          true,
		"id":               res.MigrationID,
		"from_model":       res.FromModel,
		"to_model":         res.ToModel,
		"sweep_cleared":    res.SweepCleared,
		"flipped_backends": res.FlippedBackends,
	})
}

// handleEmbedMigrationCleanup drops the _old rollback anchor
// (events.CleanupEmbedMigration) — operator-explicit, after which the way back is
// a new migration in the opposite direction (§4.10).
func (h *ManageHandler) handleEmbedMigrationCleanup(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ctx := r.Context()
	id, ok := h.resolveMigrationID(w, ctx, req)
	if !ok {
		return
	}
	if err := events.CleanupEmbedMigration(ctx, h.pool, id); err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "cleaned": true})
}

// handleEmbedMigrationPurge nulls leftover embedding_next data
// (embedmigration.Purge) — fail-closed while any non-terminal migration exists.
func (h *ManageHandler) handleEmbedMigrationPurge(w http.ResponseWriter, r *http.Request) {
	cleared, err := embedmigration.Purge(r.Context(), h.pool, 0)
	if err != nil {
		embedMigrationMechErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "cleared": cleared})
}

// embedFailuresDefaultLimit / embedFailuresMaxLimit bound the admin-only failures
// list read (§5 Bruchpfad 10).
const (
	embedFailuresDefaultLimit = 50
	embedFailuresMaxLimit     = 500
)

// handleEmbedMigrationFailures reads context_embed_failures (server-admin-only,
// §5 Bruchpfad 10) newest-parked-first. last_error is already normalized in the
// worker, so no content leaks; the table stays admin-only for its block-ID
// dimension. Optional data.migration_id-free — the list spans both regimes.
func (h *ManageHandler) handleEmbedMigrationFailures(w http.ResponseWriter, r *http.Request, req manageRequest) {
	d := parseEmbedMigrationData(req)
	limit := d.Limit
	if limit <= 0 {
		limit = embedFailuresDefaultLimit
	}
	if limit > embedFailuresMaxLimit {
		limit = embedFailuresMaxLimit
	}
	rows, err := h.pool.Query(r.Context(),
		`SELECT block_id::text, migration_id::text, attempts, last_error, last_class,
		        next_attempt_at::text, first_seen
		 FROM context_embed_failures
		 ORDER BY next_attempt_at DESC, first_seen DESC
		 LIMIT $1`, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	defer rows.Close()

	out := []embedFailureRow{}
	for rows.Next() {
		var fr embedFailureRow
		if err := rows.Scan(&fr.BlockID, &fr.MigrationID, &fr.Attempts, &fr.LastError,
			&fr.LastClass, &fr.NextAttemptAt, &fr.FirstSeen); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
			return
		}
		out = append(out, fr)
	}
	if rows.Err() != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": rows.Err().Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "failures": out, "count": len(out)})
}
