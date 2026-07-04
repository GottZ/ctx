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

// ── I-G Pull-APPLY mapping writes (design/02 §3.5/§3.6/§4.5.2) ────────────────
// I-F ships the mapping TABLE and reads it (ConflictCount); I-G MINTS the rows:
// every write below carries a block_id (NOT NULL, 080), so a mapping row is
// structurally inseparable from a real block — the ownership boundary in the 080
// header. base_hash is the sha256 canonical projection at last successful sync
// (§3.6, W16 — never a timestamp); forge_updated_at is telemetry ONLY, never a
// direction input. Every write is Tx-bound (the apply couples block + mapping in
// one Tx so a half-written mapping can never exist).

// SyncMap is one context_project_sync_map row as the 3-way apply needs it: the
// stored base_hash + the conflict flag drive the direction decision (§4.5.2).
// BaseFields is the canonical projection JSON at the last base write — the push
// field-diff (Welle I-H, §4.5.2) reads it to send only CHANGED fields; nil on a
// legacy row (the push then falls back to a full-projection push). It is stored
// under metadata.base_fields, never a direction input (like base_hash it is a
// SNAPSHOT, not a timestamp — W16).
type SyncMap struct {
	ProjectID      string
	EntityKind     string
	ForgeID        int64
	BlockID        string
	BaseHash       string
	BaseFields     json.RawMessage
	Conflict       bool
	ForgeUpdatedAt *time.Time
}

// GetSyncMapsByForge batch-loads the mapping rows for a page of forge ids of one
// kind, keyed forge_id→row — ONE query per page, no N+1 on the direction lookup
// (§6, 10k+ issues/repo). forge_id 0 (local-only, I-H push) is never a fetch id,
// so ctx-only mappings are naturally untouched by a pull. A forge id with no
// mapping is simply absent from the result (⇒ the pull-create case, §4.5.2).
func GetSyncMapsByForge(ctx context.Context, pool *pgxpool.Pool, projectID, kind string, forgeIDs []int64) (map[int64]SyncMap, error) {
	out := make(map[int64]SyncMap, len(forgeIDs))
	if len(forgeIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT entity_kind, forge_id, block_id::text, base_hash, conflict, forge_updated_at,
		        metadata->'base_fields'
		   FROM context_project_sync_map
		  WHERE project_id = $1::uuid AND entity_kind = $2 AND forge_id = ANY($3::bigint[])`,
		projectID, kind, forgeIDs)
	if err != nil {
		return nil, fmt.Errorf("store: get sync maps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m := SyncMap{ProjectID: projectID}
		if err := rows.Scan(&m.EntityKind, &m.ForgeID, &m.BlockID, &m.BaseHash, &m.Conflict, &m.ForgeUpdatedAt, &m.BaseFields); err != nil {
			return nil, fmt.Errorf("store: scan sync map: %w", err)
		}
		out[m.ForgeID] = m
	}
	return out, rows.Err()
}

// InsertSyncMap writes the mapping row of a pull-create (§4.5.2 creation case):
// (project, kind, forge_id) → block_id with base_hash = the projection just
// pulled. Tx-bound (couples to the block insert). ON CONFLICT DO NOTHING keeps a
// re-applied create idempotent (a concurrent create of the same forge id is a
// no-op, not a 23505 abort of the run).
func InsertSyncMap(ctx context.Context, tx pgx.Tx, m SyncMap) error {
	meta := baseFieldsMeta(m.BaseFields)
	_, err := tx.Exec(ctx,
		`INSERT INTO context_project_sync_map
		   (project_id, entity_kind, forge_id, block_id, base_hash, forge_updated_at, metadata)
		 VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7::jsonb)
		 ON CONFLICT (project_id, entity_kind, forge_id, block_id) DO NOTHING`,
		m.ProjectID, m.EntityKind, m.ForgeID, m.BlockID, m.BaseHash, m.ForgeUpdatedAt, meta)
	if err != nil {
		return fmt.Errorf("store: insert sync map: %w", err)
	}
	return nil
}

// baseFieldsMeta wraps a canonical projection snapshot in the mapping-metadata
// shape {"base_fields": <json>}; a nil snapshot yields '{}' (legacy row, no
// snapshot — the push falls back to a full-projection push).
func baseFieldsMeta(fields json.RawMessage) string {
	if len(fields) == 0 {
		return `{}`
	}
	obj, err := json.Marshal(map[string]json.RawMessage{"base_fields": fields})
	if err != nil {
		return `{}`
	}
	return string(obj)
}

// UpdateSyncMapBase rewrites base_hash after a pull-apply (base := forgeH) or a
// convergence (base := ctxH == forgeH). It CLEARS any conflict flag — a resolved
// direction ends the conflict. Keyed on block_id (the per-block unique index).
// Tx-bound so it commits with the block update (or, for convergence, alone: no
// block write, only the base advance, §4.5.2).
func UpdateSyncMapBase(ctx context.Context, tx pgx.Tx, blockID, baseHash string, baseFields json.RawMessage, forgeUpdatedAt *time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE context_project_sync_map
		    SET base_hash = $2, forge_updated_at = $3, synced_at = now(),
		        conflict = false, conflict_at = NULL,
		        metadata = metadata || $4::jsonb
		  WHERE block_id = $1::uuid`,
		blockID, baseHash, forgeUpdatedAt, baseFieldsMeta(baseFields))
	if err != nil {
		return fmt.Errorf("store: update sync map base: %w", err)
	}
	return nil
}

// FlagSyncMapConflict marks a both-ahead mapping (§4.5.2 last row): conflict=true
// + conflict_at, and ZERO writes to the block in either direction. conflict_at is
// preserved on a re-flag (COALESCE) so the divergence timestamp is the FIRST
// detection, not the latest run. Keyed on block_id.
func FlagSyncMapConflict(ctx context.Context, tx pgx.Tx, blockID string, at time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE context_project_sync_map
		    SET conflict = true, conflict_at = COALESCE(conflict_at, $2), synced_at = now()
		  WHERE block_id = $1::uuid`,
		blockID, at)
	if err != nil {
		return fmt.Errorf("store: flag sync map conflict: %w", err)
	}
	return nil
}

// ── W11 sync-trigger substrate (design/03 §4.4/§3.1) ────────────────────────.

// CountSyncRunsSince counts the runs a project STARTED inside the trailing
// window — the per-PROJECT rate substrate for project.sync.rate_limit (§4.4).
// It counts by project_id, NOT api_key_id (the I6 CheckRateLimit dimension), so
// N agent keys of one repo share ONE budget and cannot each fire 6 syncs/h at the
// same GitHub token. It also returns retryAfter: how long until the OLDEST run in
// the window ages out (so the 429 caller gets an honest wait, not a fixed guess);
// 0 when no run is in the window. Index: idx_sync_runs_project (project_id,
// started_at DESC), 079.
func CountSyncRunsSince(ctx context.Context, pool *pgxpool.Pool, projectID string, window time.Duration) (count int, retryAfter time.Duration, err error) {
	var secs float64
	err = pool.QueryRow(ctx,
		`SELECT count(*),
		        COALESCE(EXTRACT(EPOCH FROM (min(started_at) + make_interval(secs => $2::double precision) - now())), 0)
		   FROM context_project_sync_runs
		  WHERE project_id = $1::uuid
		    AND started_at > now() - make_interval(secs => $2::double precision)`,
		projectID, window.Seconds()).Scan(&count, &secs)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count sync runs: %w", err)
	}
	if secs < 0 {
		secs = 0
	}
	return count, time.Duration(secs * float64(time.Second)), nil
}

// ListSyncRuns returns the newest N runs for a project (the diagnosis history
// behind `ctx project issues sync --status` / GET .../sync). limit is clamped to
// [1,50]. Empty slice (never nil-panic) when the project has no runs.
func ListSyncRuns(ctx context.Context, pool *pgxpool.Pool, projectID string, limit int) ([]SyncRunRow, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := pool.Query(ctx,
		`SELECT `+syncRunSelect+` FROM context_project_sync_runs
		  WHERE project_id = $1::uuid ORDER BY started_at DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list sync runs: %w", err)
	}
	defer rows.Close()
	out := make([]SyncRunRow, 0, limit)
	for rows.Next() {
		r := SyncRunRow{}
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Error, &r.Stats); err != nil {
			return nil, fmt.Errorf("store: scan sync run row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NormalizeInterruptedSyncs is the boot-time crash recovery (design/03 §3.1): the
// in-memory run-state (forge.SyncManager) does NOT survive a process restart, so a
// project left at sync_status='running' by a crash is a lie — normalise it to
// 'error' + last_error='interrupted', and close every open run row (status=
// 'running') as 'interrupted'. Idempotent (a clean boot matches 0 rows). Called
// once after migrations, before the router serves. Returns (projects, runs)
// normalised for the boot log.
func NormalizeInterruptedSyncs(ctx context.Context, pool *pgxpool.Pool) (projects int, runs int, err error) {
	pt, err := pool.Exec(ctx,
		`UPDATE context_projects
		    SET sync_status = 'error', last_error = 'interrupted'
		  WHERE sync_status = 'running'`)
	if err != nil {
		return 0, 0, fmt.Errorf("store: normalize interrupted projects: %w", err)
	}
	rt, err := pool.Exec(ctx,
		`UPDATE context_project_sync_runs
		    SET status = 'interrupted', finished_at = now(), error = COALESCE(error, 'interrupted')
		  WHERE status = 'running'`)
	if err != nil {
		return int(pt.RowsAffected()), 0, fmt.Errorf("store: normalize interrupted runs: %w", err)
	}
	return int(pt.RowsAffected()), int(rt.RowsAffected()), nil
}
