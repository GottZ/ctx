// /api/project/{id}/sync — the manual forge-sync TRIGGER + status poll (workflow
// W11, design/03-workflow-api-cli.md §4.2/§4.4/§4.6; decision E6=a). It is a thin
// REST shell around the Achse-02 sync engine (forge.SyncManager); the deferred
// periodic scheduler loop (I-F) stays deferred — W11 is the on-demand trigger.
//
//	POST /api/project/{id}/sync   start a run   write-scope gate + per-project rate limit
//	GET  /api/project/{id}/sync   status poll   member (scope-read)
//
// TIER (E6=a, "wer Issues schreiben darf, darf syncen"): the START is gated on the
// per-project WRITE scope (writableBlockScopes ∋ project.scope), NOT tenant-admin
// (that is the manage-action transport). A scope the caller cannot even READ ⇒ 404
// uniform (no existence oracle); readable-but-not-writable ⇒ 403 (the caller
// already knows the project exists, so 403 leaks nothing). This mirrors the W7
// write surface exactly (project_issues_write.go resolveWriteScope).
//
// RUN-STATE (§4.4): the engine holds a PER-PROJECT single-flight (double-start of
// the SAME project ⇒ 409) UNDER a process-global concurrency semaphore (project.
// sync.max_concurrent, default 3 — a full semaphore ⇒ 409 + retry_after_s, never a
// daemon-wide serialise). The rate limit (project.sync.rate_limit, default 6/h) is
// counted PER PROJECT over context_project_sync_runs (§3.1) — N agent keys of one
// repo SHARE the budget (not per api_key_id like the I6 write throttle) ⇒ 429.
package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// syncRateWindow is the fixed counting window for project.sync.rate_limit
	// ("6/h pro Projekt", §4.4): the config key carries the COUNT, the window is the
	// hour. Per-project via context_project_sync_runs + idx_sync_runs_project (§3.1).
	syncRateWindow = time.Hour
	// syncSaturatedRetryAfterS is the retry hint (seconds) on a full concurrency
	// semaphore (§4.4, 409). A running sync is expected to progress; a small honest
	// constant beats a precise value the process cannot compute for a live run.
	syncSaturatedRetryAfterS = 5
)

// SyncController is the narrow run-engine surface the sync endpoint consumes: a
// slice of *forge.SyncManager (StartSync + Status). SetToken stays manage-only —
// the REST sync path never seals a credential.
type SyncController interface {
	StartSync(ctx context.Context, project store.ProjectRow, dryRun bool) (forge.SyncStatus, error)
	Status(projectID string) forge.SyncStatus
}

// ProjectSyncHandler serves /api/project/{id}/sync. cfg is optional (nil = the
// rate limit is disabled, test wiring); forge is the run engine (nil ⇒ 503).
type ProjectSyncHandler struct {
	pool  *pgxpool.Pool
	forge SyncController
	cfg   ConfigStore
}

// NewProjectSyncHandler wires the pool, the run engine and the runtime config.
func NewProjectSyncHandler(pool *pgxpool.Pool, fc SyncController, cfg ConfigStore) *ProjectSyncHandler {
	return &ProjectSyncHandler{pool: pool, forge: fc, cfg: cfg}
}

// MountProjectSync mounts POST+GET /api/project/{id}/sync under ONE RequireMember
// group (a missing gate is a missing route, §5.1). RequireMember ADMITS; the POST
// handler then runs the per-project write-scope gate (E6=a), the GET handler the
// scope-read gate — the K-T1 pairing (the mount admits, the handler scopes).
func MountProjectSync(r chi.Router, h *ProjectSyncHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireMember)
		r.Post("/api/project/{id}/sync", h.HandleStart)
		r.Get("/api/project/{id}/sync", h.HandleStatus)
	})
}

// HandleStart implements POST /api/project/{id}/sync: write-scope gate → per-project
// rate limit → engine start, with the run-state error vocabulary mapped to HTTP.
func (h *ProjectSyncHandler) HandleStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.forge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "sync engine not enabled"})
		return
	}
	row, ok := h.resolveWritableProject(w, r)
	if !ok {
		return
	}
	// Per-project rate limit (§4.4): count sync_runs in the trailing window. Counted
	// by project_id (NOT api_key_id) so N keys of one repo share ONE budget. Fail-
	// closed: a count error is a 500, never a silently-unmetered start.
	if h.cfg != nil {
		if limit := h.cfg.SnapshotForRequest(ctx).Project.Sync.RateLimit; limit > 0 {
			count, retryAfter, err := store.CountSyncRunsSince(ctx, h.pool, row.ID, syncRateWindow)
			if err != nil {
				slog.Error("project-sync: rate count", "error", err, "request_id", RequestIDFromContext(ctx))
				internalProjectError(w, ctx, "project-sync: rate count", err)
				return
			}
			if count >= limit {
				ras := int(retryAfter.Seconds())
				if ras < 1 {
					ras = 1
				}
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"success": false, "error": "project sync rate limit exceeded", "retry_after_s": ras,
				})
				return
			}
		}
	}
	dryRun := r.URL.Query().Get("dry_run") == "true"
	st, err := h.forge.StartSync(ctx, *row, dryRun)
	if err != nil {
		switch {
		case errors.Is(err, forge.ErrSyncRunning):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "sync already running for this project"})
		case errors.Is(err, forge.ErrSyncSaturated):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "sync concurrency limit reached — retry shortly", "retry_after_s": syncSaturatedRetryAfterS})
		case errors.Is(err, forge.ErrTenantSuspended):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "owning tenant is suspended"})
		case errors.Is(err, forge.ErrNoTenant):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": "scope has no owning tenant — sync disabled"})
		case errors.Is(err, forge.ErrIssuePolicy):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		default:
			internalProjectError(w, ctx, "project-sync: start", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "run": st})
}

// HandleStatus implements GET /api/project/{id}/sync: the in-memory run-state
// merged with the DB history (last run, recent runs, open conflict count) and the
// register's display columns. Member scope-read (no write needed).
func (h *ProjectSyncHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		internalProjectError(w, ctx, "project-sync: status load", err)
		return
	}
	if row == nil || ar == nil || !slices.Contains(ar.ReadScopes, row.Scope) {
		projectNotFound(w) // 404 uniform (no existence oracle)
		return
	}
	last, err := store.LatestSyncRun(ctx, h.pool, row.ID)
	if err != nil {
		internalProjectError(w, ctx, "project-sync: latest run", err)
		return
	}
	recent, err := store.ListSyncRuns(ctx, h.pool, row.ID, 10)
	if err != nil {
		internalProjectError(w, ctx, "project-sync: list runs", err)
		return
	}
	conflicts, err := store.ConflictCount(ctx, h.pool, row.ID)
	if err != nil {
		internalProjectError(w, ctx, "project-sync: conflict count", err)
		return
	}
	var run forge.SyncStatus
	if h.forge != nil {
		run = h.forge.Status(row.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"project_id":    row.ID,
		"sync_status":   row.SyncStatus,
		"sync_enabled":  row.SyncEnabled,
		"push_enabled":  row.PushEnabled,
		"token_set":     row.TokenSecret != nil,
		"last_sync_at":  row.LastSyncAt,
		"backoff_until": row.BackoffUntil,
		"last_error":    row.LastError,
		"conflicts":     conflicts,
		"run":           run,
		"last_run":      last,
		"recent_runs":   recent,
	})
}

// resolveWritableProject loads {id} and enforces the E6=a write-scope gate: a
// scope the caller cannot READ ⇒ 404 uniform (no oracle); a readable-but-not-
// writable scope ⇒ 403 (the write-authorization boundary, leaks nothing new). ok
// == false means a response was already written.
func (h *ProjectSyncHandler) resolveWritableProject(w http.ResponseWriter, r *http.Request) (*store.ProjectRow, bool) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		internalProjectError(w, ctx, "project-sync: write scope load", err)
		return nil, false
	}
	if row == nil || ar == nil || !slices.Contains(ar.ReadScopes, row.Scope) {
		projectNotFound(w)
		return nil, false
	}
	if !slices.Contains(writableBlockScopes(ar), row.Scope) {
		writeScopeForbidden(w)
		return nil, false
	}
	return row, true
}
