// Forge sync-state store layer (workflow I-F, design/02-issue-workflow.md §4.5;
// migration 080). DB-CRUD for the project register's sync-state columns
// (sync_status/sync_enabled/last_error/backoff_until/last_sync_at/token_secret/
// sync_cursor), the per-run history (context_project_sync_runs, 079) and a
// read-only count over the issue↔block mapping (context_project_sync_map, 080).
//
// I-F WRITES the sync STATE (cursor, backoff, run rows) and the token REF; it
// does NOT write context_project_sync_map rows — those need a block_id, which
// only the Pull-APPLY step (Welle I-G) mints (§3.5, migration 080 header).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncRunRow is one context_project_sync_runs row (079) — the counting
// substrate for run-state and the diagnosis history behind forge-sync-status.
type SyncRunRow struct {
	ID         string          `json:"id"`
	ProjectID  string          `json:"project_id"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Status     string          `json:"status"` // running | done | error | interrupted
	Error      *string         `json:"error,omitempty"`
	Stats      json.RawMessage `json:"stats"`
}

const syncRunSelect = `id, project_id::text, started_at, finished_at, status, error, stats`

func scanSyncRun(row pgx.Row) (*SyncRunRow, error) {
	r := &SyncRunRow{}
	err := row.Scan(&r.ID, &r.ProjectID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Error, &r.Stats)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan sync run: %w", err)
	}
	return r, nil
}

// StartSyncRun inserts a fresh running row and returns it. The row is the
// durable trace of a run; the in-memory run-state (forge.SyncManager) is the
// single-flight gate (§4.4/§4.5.5). status defaults to 'running' (079).
func StartSyncRun(ctx context.Context, pool *pgxpool.Pool, projectID string) (*SyncRunRow, error) {
	return scanSyncRun(pool.QueryRow(ctx,
		`INSERT INTO context_project_sync_runs (project_id) VALUES ($1::uuid)
		 RETURNING `+syncRunSelect, projectID))
}

// FinishSyncRun closes a run: finished_at=now, status (done|error|interrupted),
// error text (empty = NULL) and the Achse-02 stats blob (fetched/prs_skipped/…).
func FinishSyncRun(ctx context.Context, pool *pgxpool.Pool, runID, status, errMsg string, stats json.RawMessage) error {
	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	if len(stats) == 0 {
		stats = json.RawMessage(`{}`)
	}
	_, err := pool.Exec(ctx,
		`UPDATE context_project_sync_runs
		    SET finished_at = now(), status = $2, error = $3, stats = $4::jsonb
		  WHERE id = $1::uuid`, runID, status, errArg, stats)
	if err != nil {
		return fmt.Errorf("store: finish sync run: %w", err)
	}
	return nil
}

// LatestSyncRun returns the newest run for a project, or (nil, nil) if none.
func LatestSyncRun(ctx context.Context, pool *pgxpool.Pool, projectID string) (*SyncRunRow, error) {
	return scanSyncRun(pool.QueryRow(ctx,
		`SELECT `+syncRunSelect+` FROM context_project_sync_runs
		  WHERE project_id = $1::uuid ORDER BY started_at DESC LIMIT 1`, projectID))
}

// SyncStatePatch carries the mutable sync-state columns; a nil field is left
// unchanged (COALESCE). ClearBackoff/ClearError/ClearLastError are explicit
// resets (a nil pointer can not express "set NULL", so a successful run needs a
// way to clear a prior backoff/error).
type SyncStatePatch struct {
	SyncStatus   *string
	SyncEnabled  *bool
	LastError    *string
	ClearError   bool
	BackoffUntil *time.Time
	ClearBackoff bool
	SetLastSync  bool // stamp last_sync_at = now()
}

// SetProjectSyncState updates the sync-state columns fail-safe (a NULL id is a
// no-op error). The found=false tenant gate (§4.5.5/S13) uses this to set
// sync_enabled=false + last_error atomically.
func SetProjectSyncState(ctx context.Context, pool *pgxpool.Pool, id string, p SyncStatePatch) error {
	if id == "" {
		return fmt.Errorf("store: sync state: id required")
	}
	var errArg any
	if p.ClearError {
		errArg = nil
	} else if p.LastError != nil {
		errArg = *p.LastError
	}
	var backoffArg any
	if p.ClearBackoff {
		backoffArg = nil
	} else if p.BackoffUntil != nil {
		backoffArg = *p.BackoffUntil
	}
	// COALESCE keeps unset scalar fields; error/backoff are set explicitly when
	// their Clear/value flags fire, else left as-is via the $flag guards.
	_, err := pool.Exec(ctx,
		`UPDATE context_projects SET
		    sync_status   = COALESCE($2, sync_status),
		    sync_enabled  = COALESCE($3, sync_enabled),
		    last_error    = CASE WHEN $4 THEN $5 ELSE last_error END,
		    backoff_until = CASE WHEN $6 THEN $7 ELSE backoff_until END,
		    last_sync_at  = CASE WHEN $8 THEN now() ELSE last_sync_at END
		  WHERE id = $1::uuid`,
		id, p.SyncStatus, p.SyncEnabled,
		p.ClearError || p.LastError != nil, errArg,
		p.ClearBackoff || p.BackoffUntil != nil, backoffArg,
		p.SetLastSync)
	if err != nil {
		return fmt.Errorf("store: set sync state: %w", err)
	}
	return nil
}

// SetProjectSyncCursor persists the forge-side progress cursor (ETag/since;
// Achse-02 contract, 079 sync_cursor JSONB). Per-page persistence for 10k+
// resumability rides this call. cursor must be a JSON object.
func SetProjectSyncCursor(ctx context.Context, pool *pgxpool.Pool, id string, cursor json.RawMessage) error {
	if len(cursor) == 0 {
		cursor = json.RawMessage(`{}`)
	}
	_, err := pool.Exec(ctx,
		`UPDATE context_projects SET sync_cursor = $2::jsonb WHERE id = $1::uuid`, id, string(cursor))
	if err != nil {
		return fmt.Errorf("store: set sync cursor: %w", err)
	}
	return nil
}

// MergeProjectSyncCursor merges patch into sync_cursor (jsonb `||`), preserving
// the fetch keys (etag/since) other callers manage. The backoff-attempt counter
// (backoff_n) rides this call so the offline-first resilience state (§4.5.3) does
// not clobber the fetch cursor.
func MergeProjectSyncCursor(ctx context.Context, pool *pgxpool.Pool, id string, patch json.RawMessage) error {
	if len(patch) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx,
		`UPDATE context_projects SET sync_cursor = sync_cursor || $2::jsonb WHERE id = $1::uuid`,
		id, string(patch))
	if err != nil {
		return fmt.Errorf("store: merge sync cursor: %w", err)
	}
	return nil
}

// SetProjectToken records the NAME of the sealed PAT secret on the register
// (never the PAT — the plaintext lives ONLY in context_secrets, §5.4). Runs
// TX-bound so the seal (PutSecret) and the ref update commit atomically.
func SetProjectToken(ctx context.Context, tx pgx.Tx, id, secretName string) error {
	_, err := tx.Exec(ctx,
		`UPDATE context_projects SET token_secret = $2 WHERE id = $1::uuid`, id, secretName)
	if err != nil {
		return fmt.Errorf("store: set project token ref: %w", err)
	}
	return nil
}

// ConflictCount returns the number of mapping rows currently flagged conflict
// for a project (forge-sync-status surface, §4.5.2). Read-only; the mapping
// rows themselves are written by I-G apply.
func ConflictCount(ctx context.Context, pool *pgxpool.Pool, projectID string) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_project_sync_map WHERE project_id = $1::uuid AND conflict`,
		projectID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: conflict count: %w", err)
	}
	return n, nil
}
