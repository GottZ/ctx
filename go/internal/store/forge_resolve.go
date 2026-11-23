// forge_resolve.go — the operator's way out of a conflict flag and of a forge
// stamp that went back (issue #509). A flag the pull or the push set on a
// mapping (context_project_sync_map.conflict) kept the mapping out of the
// push for good where block and forge did not come level again, and no
// surface listed or resolved it; the documented recovery of a forge stamp
// that went back was two hand-written UPDATEs. These are the database
// primitives; package forge decides with the block's projection and runs
// them under the sync's locks.
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

// ForgeSeen is the forge's version a pull saw when it flagged a mapping, or
// saw later on the flagged mapping (a newer remote edit, issue #509): its
// canonical projection, the projection's hash, and the forge's updated_at of
// it. It is kept in the mapping's metadata.forge_seen while the flag stands
// and dropped by every base write, so it tells the resolve surface what the
// forge held as far as the database knows — not what it holds since. It is
// valid for a resolution only while At equals the row's forge_updated_at
// (forgeSeenValid): a push advance or a stamp reset since leaves it stale.
type ForgeSeen struct {
	Fields json.RawMessage `json:"fields"`
	Hash   string          `json:"hash"`
	At     *time.Time      `json:"at"`
}

// FlaggedSyncMap is one flagged mapping row with the block it maps: what the
// resolve surface lists and decides on.
type FlaggedSyncMap struct {
	ProjectID      string
	EntityKind     string
	ForgeID        int64
	BlockID        string
	BaseHash       string
	BaseFields     json.RawMessage
	Conflict       bool
	ConflictAt     *time.Time
	ForgeUpdatedAt *time.Time
	Seen           *ForgeSeen
	Block          Block // Title, Content, Metadata, WorkflowStatus, UpdatedAt, Scope
}

// SeenValid answers whether the row's forge snapshot describes the newest
// forge observation the row records (Seen.At == ForgeUpdatedAt).
func (m FlaggedSyncMap) SeenValid() bool {
	if m.Seen == nil || m.Seen.At == nil || m.ForgeUpdatedAt == nil || m.Seen.Hash == "" || len(m.Seen.Fields) == 0 || string(m.Seen.Fields) == "null" {
		return false
	}
	return m.Seen.At.Truncate(time.Microsecond).Equal(m.ForgeUpdatedAt.Truncate(time.Microsecond))
}

const flaggedCols = `m.project_id::text, m.entity_kind, m.forge_id, m.block_id::text, m.base_hash,
	m.metadata->'base_fields', m.conflict, m.conflict_at, m.forge_updated_at, m.metadata->'forge_seen',
	b.title, b.content, b.metadata, COALESCE(b.workflow_status, ''), b.updated_at, b.scope`

func scanFlagged(row pgx.Row) (FlaggedSyncMap, error) {
	var m FlaggedSyncMap
	var seen []byte
	if err := row.Scan(&m.ProjectID, &m.EntityKind, &m.ForgeID, &m.BlockID, &m.BaseHash,
		&m.BaseFields, &m.Conflict, &m.ConflictAt, &m.ForgeUpdatedAt, &seen,
		&m.Block.Title, &m.Block.Content, &m.Block.Metadata, &m.Block.WorkflowStatus, &m.Block.UpdatedAt, &m.Block.Scope); err != nil {
		return m, err
	}
	m.Block.ID = m.BlockID
	if len(seen) > 0 && string(seen) != "null" {
		var s ForgeSeen
		if err := json.Unmarshal(seen, &s); err == nil {
			m.Seen = &s
		}
	}
	return m, nil
}

// ListFlaggedSyncMaps returns up to limit flagged mappings of a project whose
// block is live in scope, in block id order after afterBlockID ("" for the
// first page). The partial index idx_sync_map_conflict (project_id) WHERE
// conflict holds only the flagged rows, so a page reads the project's
// flagged rows and sorts them, not the project's mappings: its cost grows
// with the count of flagged rows of the project, not with the mappings
// (a keyset index on (project_id, block_id) WHERE conflict would need a
// migration).
func ListFlaggedSyncMaps(ctx context.Context, pool *pgxpool.Pool, projectID, scope, afterBlockID string, limit int) ([]FlaggedSyncMap, error) {
	var after any
	if afterBlockID != "" {
		after = afterBlockID
	}
	rows, err := pool.Query(ctx, `
		SELECT `+flaggedCols+`
		  FROM context_project_sync_map m
		  JOIN context_blocks b ON b.id = m.block_id
		 WHERE m.project_id = $1::uuid AND m.conflict
		   AND ($2::uuid IS NULL OR m.block_id > $2::uuid)
		   AND b.scope = $3 AND NOT b.is_archived
		 ORDER BY m.block_id
		 LIMIT $4`, projectID, after, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list flagged sync maps: %w", err)
	}
	defer rows.Close()
	out := make([]FlaggedSyncMap, 0, limit)
	for rows.Next() {
		m, err := scanFlagged(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan flagged sync map: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LockSyncMapOfProject reads the mapping row of blockID inside the caller's
// transaction, FOR NO KEY UPDATE, when it belongs to projectID; the block
// row is locked already (forge.LockPulledBlock — block before mapping, the
// order of every writer of both). ok=false: no mapping of this project.
func LockSyncMapOfProject(ctx context.Context, tx pgx.Tx, projectID, blockID string) (FlaggedSyncMap, bool, error) {
	m, err := scanFlagged(tx.QueryRow(ctx, `
		SELECT `+flaggedCols+`
		  FROM context_project_sync_map m
		  JOIN context_blocks b ON b.id = m.block_id
		 WHERE m.block_id = $1::uuid AND m.project_id = $2::uuid
		   FOR NO KEY UPDATE OF m`, blockID, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FlaggedSyncMap{}, false, nil
	}
	if err != nil {
		return FlaggedSyncMap{}, false, fmt.Errorf("store: lock sync map of project: %w", err)
	}
	return m, true, nil
}

// SyncMapResolution is what a resolution writes on a flagged mapping: the
// base (hash and snapshot; a nil snapshot drops base_fields) and synced_at
// (nil: -infinity, the row is a push candidate whatever its block's
// updated_at). PullGate sets synced_at to 'infinity' instead: the candidate
// filter (b.updated_at > m.synced_at) is false for every local write, so the
// push — of this version and of one before it alike — leaves the mapping
// alone until a pull decided over the entity and wrote synced_at (a pull, a
// convergence, a flag, or OpenPullGate where the forge holds the base).
type SyncMapResolution struct {
	BaseHash   string
	BaseFields json.RawMessage
	SyncedAt   *time.Time
	PullGate   bool
}

// ResolveSyncMapConflict writes a resolution on the flagged mapping of
// blockID and clears the flag: conflict, conflict_at and the forge
// snapshot go, forge_updated_at stays (the newest forge observation, which
// orders the next pull's fetch). The caller holds block and mapping locked.
// ok=false: the row is not flagged (nothing written).
func ResolveSyncMapConflict(ctx context.Context, tx pgx.Tx, blockID string, r SyncMapResolution) (bool, error) {
	var fields any
	if len(r.BaseFields) > 0 {
		fields = string(r.BaseFields)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE context_project_sync_map
		   SET base_hash = $2,
		       metadata = (CASE WHEN $3::jsonb IS NULL THEN metadata - 'base_fields'
		                        ELSE metadata || jsonb_build_object('base_fields', $3::jsonb) END) - 'forge_seen',
		       synced_at = CASE WHEN $5 THEN 'infinity'::timestamptz ELSE COALESCE($4::timestamptz, '-infinity'::timestamptz) END,
		       conflict = false, conflict_at = NULL
		 WHERE block_id = $1::uuid AND conflict`, blockID, r.BaseHash, fields, r.SyncedAt, r.PullGate)
	if err != nil {
		return false, fmt.Errorf("store: resolve sync map conflict: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordForgeObservation records on the locked mapping row of blockID a
// forge version a pull saw and did not apply (issue #509): forge_updated_at
// becomes the later of the row's and at, and, with seen, the snapshot of a
// flagged row is replaced. A later fetch older than this observation is
// then skipped (forge.olderOrUnordered) — a convergence another instance
// read before the version was seen no longer clears the flag over it.
func RecordForgeObservation(ctx context.Context, tx pgx.Tx, blockID string, at *time.Time, seen *ForgeSeen) error {
	seenJSON, err := forgeSeenJSON(seen)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE context_project_sync_map
		   SET forge_updated_at = GREATEST(forge_updated_at, $2::timestamptz),
		       metadata = CASE WHEN $3::jsonb IS NULL THEN metadata
		                       ELSE metadata || jsonb_build_object('forge_seen', $3::jsonb) END
		 WHERE block_id = $1::uuid`, blockID, at, seenJSON); err != nil {
		return fmt.Errorf("store: record forge observation: %w", err)
	}
	return nil
}

// OpenPullGate ends the push gate of a remote resolution (SyncMapResolution
// .PullGate) on the locked mapping row of blockID: a pull found the forge at
// the base, so the forge holds no version the push could send over unseen,
// and synced_at becomes -infinity — the block's edit since the resolution is
// a push candidate. A row without the gate is left as it is.
func OpenPullGate(ctx context.Context, tx pgx.Tx, blockID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE context_project_sync_map
		   SET synced_at = '-infinity'::timestamptz
		 WHERE block_id = $1::uuid AND synced_at = 'infinity'::timestamptz`, blockID); err != nil {
		return fmt.Errorf("store: open pull gate: %w", err)
	}
	return nil
}

// forgeSeenJSON is the metadata.forge_seen value of seen, nil for none.
func forgeSeenJSON(seen *ForgeSeen) (any, error) {
	if seen == nil {
		return nil, nil
	}
	b, err := json.Marshal(seen)
	if err != nil {
		return nil, fmt.Errorf("store: forge snapshot: %w", err)
	}
	return string(b), nil
}

// ResetForgeStamps sets forge_updated_at to NULL — on the mapping of blockID
// when it belongs to projectID and its block is live in scope, on every
// mapping of the project when blockID is "" — and drops their forge
// snapshots: the way out of a forge stamp that went back behind the recorded
// one (docs/architecture.md, forge sync), whose fetches every pull skipped
// as older. found=false: blockID names no such mapping (an archived block, a
// block out of the project's scope — the 404 a resolution gives there;
// nothing written). It returns the rows it reset and the entity kinds among
// them (the legs whose fetch cursor the caller holds). Tx-bound; the caller
// holds the project row, the mapping rows come after it.
func ResetForgeStamps(ctx context.Context, tx pgx.Tx, projectID, scope, blockID string) (found bool, n int, kinds []string, err error) {
	var block any
	if blockID != "" {
		var one int
		err := tx.QueryRow(ctx, `
			SELECT 1 FROM context_project_sync_map m
			  JOIN context_blocks b ON b.id = m.block_id
			 WHERE m.project_id = $1::uuid AND m.block_id = $2::uuid AND b.scope = $3 AND NOT b.is_archived
			   FOR NO KEY UPDATE OF m`, projectID, blockID, scope).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, nil, nil
		}
		if err != nil {
			return false, 0, nil, fmt.Errorf("store: reset forge stamps: %w", err)
		}
		block = blockID
	}
	rows, err := tx.Query(ctx, `
		UPDATE context_project_sync_map
		   SET forge_updated_at = NULL, metadata = metadata - 'forge_seen'
		 WHERE project_id = $1::uuid AND ($2::uuid IS NULL OR block_id = $2::uuid)
		RETURNING entity_kind`, projectID, block)
	if err != nil {
		return false, 0, nil, fmt.Errorf("store: reset forge stamps: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return false, 0, nil, fmt.Errorf("store: reset forge stamps: %w", err)
		}
		n++
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	return true, n, kinds, rows.Err()
}

// LockProjectForCursor locks the project row FOR NO KEY UPDATE inside the
// caller's transaction, before the block and mapping rows it takes next (a
// project delete takes the project row and then its mappings, so the project
// row comes first here too), and answers whether it exists.
func LockProjectForCursor(ctx context.Context, tx pgx.Tx, projectID string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM context_projects WHERE id = $1::uuid FOR NO KEY UPDATE`, projectID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: lock project: %w", err)
	}
	return true, nil
}

// HoldFetchCursor makes the next run of a project's fetch leg (leg =
// "issues" | "comments") list every entity updated at or after at: a hold
// {at, n} goes into sync_cursor.holds.<leg> and stays until a run that read
// it finishes the leg (CommitFetchCursor); since becomes no later than at
// and the ETag goes, for a run that does not know holds. A run lists from
// hold.at without an ETag whatever since and ETag its leg carries
// (forge.legStart), so the hold also outlives a cursor commit that replaces
// the leg object — the release before holds existed merges {leg: {etag,
// since}} into sync_cursor (issue #509, rolling deploy). A run that started
// before the hold commits its cursor no further than hold.at and without
// an ETag. A second hold keeps the earlier at and takes a new nonce. A zero
// at lists the whole leg. The caller holds the project row
// (LockProjectForCursor).
func HoldFetchCursor(ctx context.Context, tx pgx.Tx, projectID, leg string, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE context_projects p
		   SET sync_cursor = COALESCE(p.sync_cursor, '{}'::jsonb)
		         || jsonb_build_object($2::text,
		              COALESCE(p.sync_cursor->$2, '{}'::jsonb) || jsonb_build_object(
		                'etag', '',
		                'since', `+utcJSON(`LEAST(COALESCE((p.sync_cursor->$2->>'since')::timestamptz, '0001-01-01T00:00:00Z'::timestamptz), $3::timestamptz)`)+`))
		         || jsonb_build_object('holds',
		              COALESCE(p.sync_cursor->'holds', '{}'::jsonb) || jsonb_build_object($2::text, jsonb_build_object(
		                'at', `+utcJSON(`LEAST(COALESCE((p.sync_cursor->'holds'->$2->>'at')::timestamptz, $3::timestamptz), $3::timestamptz)`)+`,
		                'n', gen_random_uuid()::text)))
		 WHERE p.id = $1::uuid`, projectID, leg, at.UTC()); err != nil {
		return fmt.Errorf("store: hold fetch cursor: %w", err)
	}
	return nil
}

// utcJSON renders a timestamptz SQL expression as a JSON string in UTC with
// microseconds and a Z — the form encoding/json reads into a time.Time
// whatever the session's TimeZone (to_jsonb renders the session's offset,
// and for year 1 a local mean time offset with seconds that RFC 3339 does
// not carry).
func utcJSON(expr string) string {
	return `to_jsonb(to_char((` + expr + `) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))`
}

// CommitFetchCursor saves a fetch leg's cursor after a page it applied (leg
// = "issues" | "comments"): etag and since as the leg computed them, unless
// sync_cursor.holds.<leg> carries a hold this run did not read (readHold,
// the hold's nonce in the cursor the run started from; "" for none) — then
// since goes no further than hold.at and no ETag is saved. A run that read
// the hold saves its own cursor (it listed from hold.at or before) and, with
// last (its leg read the last page), drops the hold; every other hold stays
// (HoldFetchCursor). top (a JSON object, nil for none) is merged into
// sync_cursor beside the legs — the comment leg's held orphans. The other
// keys of the cursor survive.
func CommitFetchCursor(ctx context.Context, pool *pgxpool.Pool, projectID, leg, etag string, since time.Time, readHold string, last bool, top json.RawMessage) error {
	if len(top) == 0 {
		top = json.RawMessage(`{}`)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE context_projects p
		   SET sync_cursor = COALESCE(p.sync_cursor, '{}'::jsonb)
		         || jsonb_build_object($2::text, CASE
		              WHEN p.sync_cursor->'holds'->$2 IS NULL OR p.sync_cursor->'holds'->$2->>'n' = $5::text
		                THEN jsonb_build_object('etag', $3::text, 'since', `+utcJSON(`$4::timestamptz`)+`)
		              ELSE jsonb_build_object('etag', '',
		                     'since', `+utcJSON(`LEAST($4::timestamptz, (p.sync_cursor->'holds'->$2->>'at')::timestamptz)`)+`)
		            END)
		         || CASE WHEN p.sync_cursor->'holds'->$2->>'n' = $5::text AND $6
		              THEN jsonb_build_object('holds', (p.sync_cursor->'holds') - $2::text)
		              ELSE '{}'::jsonb END
		         || $7::jsonb
		 WHERE p.id = $1::uuid`, projectID, leg, etag, since.UTC(), readHold, last, string(top)); err != nil {
		return fmt.Errorf("store: commit fetch cursor: %w", err)
	}
	return nil
}

// MaxMappedForgeID is the highest forge id of a kind the project maps (0 for
// none) — a backward scan of the mapping's primary key (project_id,
// entity_kind, forge_id, …).
func MaxMappedForgeID(ctx context.Context, pool *pgxpool.Pool, projectID, kind string) (int64, error) {
	var mx int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(forge_id), 0) FROM context_project_sync_map WHERE project_id = $1::uuid AND entity_kind = $2`,
		projectID, kind).Scan(&mx); err != nil {
		return 0, fmt.Errorf("store: max mapped forge id: %w", err)
	}
	return mx, nil
}

// ForgeSyncAuditEntity is the context_settings_audit entity_type of a
// resolution or a stamp reset (issue #509).
const ForgeSyncAuditEntity = "forge_sync"

// WriteForgeSyncAudit records a resolution or a stamp reset in the caller's
// transaction: entity_key the block (or the project, for a reset of all its
// mappings), the project's scope, action, the mapping before and after, the
// acting key and request (stamp), and the owning tenant in
// metadata.tenant_id, so GET /api/audit serves it to that tenant's admins.
func WriteForgeSyncAudit(ctx context.Context, tx pgx.Tx, stamp AuditStamp, tenantID, scope, key, action string, before, after any) error {
	if err := stampAudit(ctx, tx, stamp); err != nil {
		return err
	}
	ov, err := json.Marshal(before)
	if err != nil {
		return fmt.Errorf("store: forge sync audit: %w", err)
	}
	nv, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("store: forge sync audit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO context_settings_audit
		       (entity_type, entity_key, scope, action, old_value, new_value, api_key_id, actor_label, metadata)
		SELECT $1::text, $2::text, $3::varchar, $4::text, $5::jsonb, $6::jsonb, a.actor, a.actor_label,
		       a.meta || jsonb_strip_nulls(jsonb_build_object('tenant_id', NULLIF($7::text, '')))
		  FROM audit_actor_meta() a`,
		ForgeSyncAuditEntity, key, scope, action, string(ov), string(nv), tenantID); err != nil {
		return fmt.Errorf("store: forge sync audit: %w", err)
	}
	return nil
}
