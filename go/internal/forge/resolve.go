// resolve.go — the way out of a conflict flag, and of a forge stamp that went
// back (issue #509). A flag the pull set where block and forge both moved
// apart from the base, or the push set where it could not tell what the
// forge holds, kept the mapping out of the push until block and forge came
// level again; nothing listed it beyond a count in forge-sync-status, and a
// later local edit was not pushed. The documented recovery of a forge stamp
// that went back (every pull skipped the entity as older) was two UPDATEs by
// hand. The Resolver lists the flagged mappings of a project with both
// sides, resolves one in either direction under the sync's locks, and runs
// the stamp reset; package handler serves it as manage actions and the CLI
// calls those.
package forge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The two directions a flag resolves to.
const (
	// ResolveLocal keeps the block's version: the next push sends it over the
	// forge's.
	ResolveLocal = "local"
	// ResolveRemote takes the forge's version: the next pull writes it over
	// the block.
	ResolveRemote = "remote"
)

// Conflict list bounds: the default and the largest page.
const (
	conflictPageDefault = 50
	conflictPageMax     = 200
)

// Sentinel errors the manage handler maps onto statuses.
var (
	// ErrConflictNotFound: the block has no mapping in this project (or is
	// gone, archived, or out of the project's scope) — 404.
	ErrConflictNotFound = errors.New("forge: no mapping of this project for that block")
	// ErrPushInFlight: another session holds the entity's push lock — a push
	// of it may be on the wire now — 409, retry.
	ErrPushInFlight = errors.New("forge: a push of this entity is in flight")
	// ErrFlagChanged: the flag the caller listed is not the one the row
	// carries now (re-flagged, or the forge side moved) — 409.
	ErrFlagChanged = errors.New("forge: the flag changed since it was listed")
	// ErrResolveDirection: the direction is neither local nor remote — 400.
	ErrResolveDirection = errors.New("forge: direction must be local or remote")
	// ErrLocalTruncated: the local version of an issue whose body was
	// truncated at the cap differs in its body from the version the push
	// would diff it against (or there is none to diff against) — 409. The
	// push never sends a truncated body: it would flag the mapping again
	// (the data-loss guard), or, against an unknown base, send every field
	// but the body and leave the forge's body different without a flag.
	ErrLocalTruncated = errors.New("forge: the issue's body is truncated at the cap and differs from the forge's; the push never sends a truncated body — take the remote version")
)

// Resolver serves the resolve surface. It holds the pool and the registry
// snapshot, for the block's projection under the scope's workflow — the one
// the pull and the push compare (issueTypePolicy.ctxIssueBase).
type Resolver struct {
	pool     *pgxpool.Pool
	snapshot SnapshotFunc
}

// NewResolver builds the resolve surface.
func NewResolver(pool *pgxpool.Pool, snapshot SnapshotFunc) *Resolver {
	return &Resolver{pool: pool, snapshot: snapshot}
}

// ConflictSide is one side of a flagged mapping: the canonical projection
// (§3.6) and its hash.
type ConflictSide struct {
	Hash   string          `json:"hash"`
	Fields json.RawMessage `json:"fields,omitempty"`
}

// ForgeSide is what the database knows about the forge's side of a flagged
// mapping without a request to the forge: the newest forge stamp the row
// records (UpdatedAt) and the version the flagging pull — or a later pull
// that saw the flagged entity again — fetched (Seen, with its stamp). Seen
// is current only while its stamp equals UpdatedAt (SeenCurrent); a push
// advance or a stamp reset since leaves the forge side unknown. Whether the
// forge changed after that observation the list cannot know.
type ForgeSide struct {
	UpdatedAt   *time.Time    `json:"updated_at"`
	Seen        *ConflictSide `json:"seen"`
	SeenAt      *time.Time    `json:"seen_at,omitempty"`
	SeenCurrent bool          `json:"seen_current"`
}

// Conflict is one flagged mapping on the wire. Flag is the token a
// resolution may pass as expect: it names the flag (its base, its time and
// the forge stamp), so a resolution decided on a listing is refused once
// the row was flagged again or saw a newer forge version. Cell names the
// matrix cell of block, base and the forge version seen (diverged,
// forge_ahead, ctx_ahead, converged, level), or forge_unknown without a
// current snapshot.
type Conflict struct {
	BlockID    string       `json:"block_id"`
	EntityKind string       `json:"entity_kind"`
	ForgeID    int64        `json:"forge_id"`
	Title      string       `json:"title"`
	Flag       string       `json:"flag"`
	ConflictAt *time.Time   `json:"conflict_at"`
	Cell       string       `json:"cell"`
	Block      ConflictSide `json:"block"`
	BlockAt    time.Time    `json:"block_updated_at"`
	Base       ConflictSide `json:"base"`
	Forge      ForgeSide    `json:"forge"`
}

// ResolveResult is what a resolution did. Changed=false: the mapping was not
// flagged (resolved already, or a pull cleared the flag), nothing written.
// Basis (local): forge_seen — the push diffs the block against the forge
// version the flag recorded; base — no current snapshot, the push diffs
// against the row's base; unknown — the base is unknown and the push sends
// every pushed field. HeldSince (remote): the
// fetch leg (Leg) lists every entity updated at or after it on its next run.
type ResolveResult struct {
	BlockID   string     `json:"block_id"`
	Direction string     `json:"direction"`
	Changed   bool       `json:"changed"`
	Basis     string     `json:"basis,omitempty"`
	Leg       string     `json:"leg,omitempty"`
	HeldSince *time.Time `json:"held_since,omitempty"`
	Flag      string     `json:"flag,omitempty"`
}

// ResetResult is what a stamp reset did: the mappings whose forge stamp it
// set to NULL, and the fetch legs it holds to a full listing.
type ResetResult struct {
	Reset int      `json:"reset"`
	Legs  []string `json:"legs"`
}

// flagToken names a flag: its base, the time it was set and the forge stamp
// the row records.
func flagToken(m store.FlaggedSyncMap) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%s", m.BaseHash, stampText(m.ConflictAt), stampText(m.ForgeUpdatedAt))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func stampText(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

// legOf is the fetch leg of an entity kind.
func legOf(kind string) string {
	if kind == entityKindComment {
		return "comments"
	}
	return "issues"
}

// projectSide is the block's projection as the pull and the push compare it
// with the row's base snapshot.
func projectSide(pol issueTypePolicy, kind string, b *store.Block, baseFields json.RawMessage) ([]byte, string) {
	if kind == entityKindComment {
		return CtxCommentBase(b)
	}
	return pol.ctxIssueBase(b, baseFields)
}

// sameBody answers whether two issue projections carry the same body (false
// when the basis has no snapshot).
func sameBody(basis, ctx json.RawMessage) bool {
	b, ok := parseIssueProjection(basis)
	c, ok2 := parseIssueProjection(ctx)
	return ok && ok2 && b.Body == c.Body
}

// cellName names the matrix cell of block, base and forge.
func cellName(d decision) string {
	switch d {
	case decNoop:
		return "level"
	case decPull:
		return "forge_ahead"
	case decCtxAhead:
		return "ctx_ahead"
	case decConverge:
		return "converged"
	}
	return "diverged"
}

// ListConflicts returns a page of the project's flagged mappings, in block
// id order after after ("" for the first page), and the cursor of the next
// page ("" for none). limit ≤ 0 is the default, above the maximum the
// maximum.
func (r *Resolver) ListConflicts(ctx context.Context, project store.ProjectRow, after string, limit int) ([]Conflict, string, error) {
	if limit <= 0 {
		limit = conflictPageDefault
	}
	if limit > conflictPageMax {
		limit = conflictPageMax
	}
	rows, err := store.ListFlaggedSyncMaps(ctx, r.pool, project.ID, project.Scope, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].BlockID
	}
	pol := resolveIssuePolicy(r.snapshot(ctx, project.Scope))
	out := make([]Conflict, 0, len(rows))
	for i := range rows {
		m := rows[i]
		fields, h := projectSide(pol, m.EntityKind, &m.Block, m.BaseFields)
		c := Conflict{
			BlockID: m.BlockID, EntityKind: m.EntityKind, ForgeID: m.ForgeID, Title: m.Block.Title,
			Flag: flagToken(m), ConflictAt: m.ConflictAt, Cell: "forge_unknown",
			Block: ConflictSide{Hash: h, Fields: fields}, BlockAt: m.Block.UpdatedAt,
			Base:  ConflictSide{Hash: m.BaseHash, Fields: m.BaseFields},
			Forge: ForgeSide{UpdatedAt: m.ForgeUpdatedAt, SeenCurrent: m.SeenValid()},
		}
		if m.Seen != nil {
			c.Forge.Seen = &ConflictSide{Hash: m.Seen.Hash, Fields: m.Seen.Fields}
			c.Forge.SeenAt = m.Seen.At
		}
		if c.Forge.SeenCurrent {
			c.Cell = cellName(decide(m.BaseHash, h, m.Seen.Hash))
		}
		out = append(out, c)
	}
	return out, next, nil
}

// ResolveConflict resolves the flag of blockID's mapping in direction:
//
//   - local: the base becomes the forge version the flag recorded
//     (metadata.forge_seen, while current) and synced_at -infinity, so the
//     next push diffs the block against what the forge held and sends every
//     field the block holds otherwise; a pull that lists that same version
//     reads ctx-ahead and leaves it to the push, a newer remote edit is a
//     conflict again. Without a current snapshot (a flag a push advance
//     kept, which drops it; the push's data-loss guard; a flag set before
//     snapshots were kept) the base stays: where the forge holds the base
//     (the flag a push advance kept over a local edit) the push sends the
//     block's changes and a pull of that version reads ctx-ahead; where it
//     does not, a pull that lists the entity flags it again, now with a
//     snapshot, and a push before that sends the fields the block changed
//     — the forge keeps its changes in the others, and the pull after the
//     push brings them to the block. An unknown base (the advance could
//     not tell what the forge holds) stays unknown: the push sends every
//     pushed field, but the PATCH that left it unknown moved the forge's
//     stamp, so the next run lists the entity before the push and flags it
//     again — now with a snapshot, which a second local resolution diffs
//     against. Refused for an issue whose truncated body differs from the
//     basis (ErrLocalTruncated).
//   - remote: the base becomes the block's own projection, the fetch leg
//     of the entity is held at the row's forge stamp (store.HoldFetchCursor;
//     a full listing without one), and the push leaves the mapping alone
//     until a pull decided over the entity (store.SyncMapResolution
//     .PullGate). The next pull lists the entity — its forge stamp is at or
//     after the recorded one — and, the block holding the base, pulls the
//     forge's version over it; a local edit since the resolution has moved
//     the block off that base, so the pull flags the mapping (both sides
//     moved) instead of either side winning unseen. A push before that pull
//     sends nothing: the base is a version the forge never held, so its
//     diff would have sent the edit over the forge's version, the one the
//     resolution chose. Where the pull finds the forge at the base, the
//     gate opens and the push sends the block's edit.
//
// Both clear the flag, its time and the forge snapshot, keep the row's
// forge stamp (it orders the next pull's fetch) and record who resolved
// which way when (store.WriteForgeSyncAudit). The resolution runs under the
// locks of the sync, in their order: the entity's push lock (tried — a push
// on the wire is not waited for, ErrPushInFlight), then, for remote, the
// project row (the cursor), then the block and the mapping row, FOR NO KEY
// UPDATE, the order of the pull and of the push advance. A pull or a push
// advance of the entity therefore runs wholly before or after it. expect,
// when set, must name the row's flag (Conflict.Flag), else ErrFlagChanged.
// A mapping that is not flagged is no error: Changed=false, nothing written
// (a repeated resolution is idempotent).
func (r *Resolver) ResolveConflict(ctx context.Context, project store.ProjectRow, blockID, direction, expect string, stamp store.AuditStamp) (ResolveResult, error) {
	if direction != ResolveLocal && direction != ResolveRemote {
		return ResolveResult{}, ErrResolveDirection
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return ResolveResult{}, fmt.Errorf("forge: resolve: acquire a connection: %w", err)
	}
	locked, err := store.TryPushLock(ctx, conn, blockID)
	switch {
	case err != nil:
		closePushConn(ctx, conn)
		return ResolveResult{}, err
	case !locked:
		conn.Release()
		return ResolveResult{}, ErrPushInFlight
	}
	defer unlockPushEntity(ctx, conn, blockID)

	var res ResolveResult
	err = pgxdb.Write(ctx, conn.Conn(), pgxdb.Stages{Begin: "forge: resolve begin", Commit: "forge: resolve commit"}, func(tx pgx.Tx) error {
		res = ResolveResult{BlockID: blockID, Direction: direction}
		return r.resolveLocked(ctx, tx, project, blockID, direction, expect, stamp, &res)
	})
	return res, err
}

// resolveLocked is ResolveConflict's transaction (see there).
func (r *Resolver) resolveLocked(ctx context.Context, tx pgx.Tx, project store.ProjectRow, blockID, direction, expect string,
	stamp store.AuditStamp, res *ResolveResult) error {

	if direction == ResolveRemote {
		ok, err := store.LockProjectForCursor(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrConflictNotFound
		}
	}
	cur, err := store.LockPulledBlock(ctx, tx, blockID, []string{project.Scope}, nil)
	if errors.Is(err, store.ErrIssueNotFound) {
		return ErrConflictNotFound
	}
	if err != nil {
		return err
	}
	m, ok, err := store.LockSyncMapOfProject(ctx, tx, project.ID, blockID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrConflictNotFound
	}
	if !m.Conflict {
		return nil
	}
	if expect != "" && expect != flagToken(m) {
		res.Flag = flagToken(m)
		return ErrFlagChanged
	}
	pol := resolveIssuePolicy(r.snapshot(ctx, project.Scope))
	ctxFields, ctxH := projectSide(pol, m.EntityKind, cur, m.BaseFields)
	var w store.SyncMapResolution
	switch direction {
	case ResolveLocal:
		switch {
		case m.SeenValid():
			w = store.SyncMapResolution{BaseHash: m.Seen.Hash, BaseFields: m.Seen.Fields}
			res.Basis = "forge_seen"
		case m.BaseHash == store.UnknownBaseHash:
			w = store.SyncMapResolution{BaseHash: m.BaseHash}
			res.Basis = "unknown"
		default:
			w = store.SyncMapResolution{BaseHash: m.BaseHash, BaseFields: m.BaseFields}
			res.Basis = "base"
		}
		if m.EntityKind == entityKindIssue && metaBool(cur.Metadata, "truncated") && !sameBody(w.BaseFields, ctxFields) {
			return ErrLocalTruncated
		}
	case ResolveRemote:
		w = store.SyncMapResolution{BaseHash: ctxH, BaseFields: ctxFields, PullGate: true}
		var at time.Time
		if m.ForgeUpdatedAt != nil {
			at = *m.ForgeUpdatedAt
		}
		res.Leg, res.HeldSince = legOf(m.EntityKind), &at
		if err := store.HoldFetchCursor(ctx, tx, project.ID, res.Leg, at); err != nil {
			return err
		}
	}
	changed, err := store.ResolveSyncMapConflict(ctx, tx, blockID, w)
	if err != nil || !changed {
		return err
	}
	res.Changed = true
	return store.WriteForgeSyncAudit(ctx, tx, stamp, project.TenantID, project.Scope, blockID, "resolve-"+direction,
		map[string]any{"project_id": project.ID, "entity_kind": m.EntityKind, "forge_id": m.ForgeID,
			"base_hash": m.BaseHash, "conflict_at": m.ConflictAt, "forge_updated_at": m.ForgeUpdatedAt, "block_hash": ctxH},
		map[string]any{"base_hash": w.BaseHash, "basis": res.Basis, "leg": res.Leg, "held_since": res.HeldSince})
}

// ResetStamps runs the way out of a forge stamp that went back behind the
// one a mapping records (docs/architecture.md, forge sync): the forge stamp
// of blockID's mapping — of every mapping of the project when blockID is ""
// — becomes NULL, so the apply decides on content again, and the fetch leg
// of each reset kind is held to a full listing (store.HoldFetchCursor at
// the zero time), so the next run lists the entity although its stamp lies
// behind the saved since; a run already under way cannot save a cursor past
// it. The project row is locked first, then the mapping rows. Both steps
// commit together, with the audit row. Only mappings with a stamp or a
// snapshot to drop count; a reset that changes none holds no leg and writes
// no audit row — a repeat after the held listing ran is a no-op. A block
// that is archived or out of the project's scope is ErrConflictNotFound.
func (r *Resolver) ResetStamps(ctx context.Context, project store.ProjectRow, blockID string, stamp store.AuditStamp) (ResetResult, error) {
	var res ResetResult
	err := pgxdb.Write(ctx, r.pool, pgxdb.Stages{Begin: "forge: stamp reset begin", Commit: "forge: stamp reset commit"}, func(tx pgx.Tx) error {
		res = ResetResult{Legs: []string{}}
		ok, err := store.LockProjectForCursor(ctx, tx, project.ID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrConflictNotFound
		}
		found, n, kinds, err := store.ResetForgeStamps(ctx, tx, project.ID, project.Scope, blockID)
		if err != nil {
			return err
		}
		if !found {
			return ErrConflictNotFound
		}
		if n == 0 {
			return nil // nothing to reset: no hold, no audit row
		}
		res.Reset = n
		for _, k := range kinds {
			leg := legOf(k)
			if err := store.HoldFetchCursor(ctx, tx, project.ID, leg, time.Time{}); err != nil {
				return err
			}
			res.Legs = append(res.Legs, leg)
		}
		slices.Sort(res.Legs)
		key := blockID
		if key == "" {
			key = project.ID
		}
		return store.WriteForgeSyncAudit(ctx, tx, stamp, project.TenantID, project.Scope, key, "stamp-reset",
			map[string]any{"project_id": project.ID, "block_id": blockID},
			map[string]any{"reset": res.Reset, "legs": res.Legs})
	})
	return res, err
}
