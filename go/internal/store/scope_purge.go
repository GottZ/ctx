package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Scope purge (issue #202): the hard delete of block data inside ONE scope,
// narrower than the tenant prune. `manage delete` archives a single block and
// the prune removes a whole tenant; between the two, rows written by mistake
// (a test or import run in a production tenant) stayed for the life of the
// tenant, and so did every block type only they used.
//
// What goes, in this order, per batch of at most pruneBatchSize matched blocks
// and in one transaction per batch:
//
//  1. the batch's blocks are locked (FOR UPDATE), and so is every block that
//     hangs below one of them through parent_id — that FK is ON DELETE CASCADE,
//     so a child goes with its parent whether it matches the filter or not. A
//     child in ANOTHER scope (a scope move keeps parent_id) refuses the run:
//     a purge of one scope never removes a row of another;
//  2. every context_dream_links row that references one of these blocks — the
//     FK is NO ACTION (the ordering the tenant prune documents), and a link
//     inserted by a concurrent dream cycle waits on the row lock of step 1
//     and then fails its FK instead of failing this delete;
//  3. the blocks. Everything else that holds a row per block follows its FK:
//     CASCADE for context_temporal, context_structural_links,
//     context_block_grants, graph_cluster_member, context_embed_failures and
//     context_project_sync_map; SET NULL for context_access_log,
//     context_write_log and context_blobs.context_block_id;
//  4. the targets of removed `supersedes` links that survive are reconciled
//     (ReconcileSupersedesTargets, the step dream-link-resolve runs after a
//     link delete), so no surviving block stays a snapshot of a deleted row.
//
// Without a filter the purge drains the scope's blobs as well, after the
// blocks. A blob carries its own scope, category, tags and time; its link to
// a block (context_block_id) is optional and SET NULL — blobs hang on the
// scope, not on blocks. A filter describes blocks, so a filtered purge leaves
// the blobs and their FK clears the link of those that pointed at a purged
// block. context_sources and context_chat_sessions are not block data and
// stay.
//
// A derivative (a block carrying derived provenance, I7) is never purged: it
// is skipped and counted, with the same predicate DeleteBlock refuses on
// (notDerivedSQL).
//
// Unumkehrbar: without ConfirmBlocks the call is a dry run and only counts.
// With it, the purge counts again and runs only if the number of blocks that
// would go equals the confirmed one; the count fixes a time (AsOf) and the run
// touches no block or blob created after it. A batch that would delete more
// than the confirmed number in total, or reach a child created after AsOf,
// rolls back and ends the run with ErrPurgeConfirmMismatch.
//
// Audit: before the first delete, one context_settings_audit row
// (entity_type 'scope_purge', entity_key and scope = the scope, action
// 'purge') records the filter and the counts, stamped with the caller's actor
// and request id; metadata.tenant_id is the scope's owner, so the tenant-admin
// sees its own purges on GET /api/audit. The audit insert fails ⇒ nothing is
// deleted.

// PurgeFilter narrows a scope purge. Every set field narrows (AND); the zero
// value drains the scope.
type PurgeFilter struct {
	// Tags: the block carries EVERY one of them (tags @> …).
	Tags     []string `json:"tags,omitempty"`
	Category string   `json:"category,omitempty"`
	// CreatedBefore / CreatedAfter are exclusive bounds on created_at.
	CreatedBefore *time.Time `json:"created_before,omitempty"`
	CreatedAfter  *time.Time `json:"created_after,omitempty"`
	// ArchivedOnly: only archived rows (is_archived = true).
	ArchivedOnly bool `json:"archived_only,omitempty"`
}

// empty reports whether the filter drains the whole scope.
func (f PurgeFilter) empty() bool {
	return len(f.Tags) == 0 && f.Category == "" && f.CreatedBefore == nil && f.CreatedAfter == nil && !f.ArchivedOnly
}

// PurgeQuery is one purge request. The reader is part of it and its zero
// value reaches nothing: a server-admin sets AllTenants (any scope that is not
// a '_' system scope); every other caller sets TenantID and reaches only the
// scopes that tenant owns. A scope of another tenant and a scope nobody owns
// answer the same ErrPurgeScopeNotFound — no existence oracle.
type PurgeQuery struct {
	Scope      string
	AllTenants bool
	TenantID   string
	Filter     PurgeFilter
	// ConfirmBlocks nil = dry run. Otherwise the run proceeds only when the
	// fresh count's Blocks equals it.
	ConfirmBlocks *int64
	Stamp         AuditStamp
}

// PurgeCounts is what a purge of the current state would remove.
type PurgeCounts struct {
	// Blocks: rows of context_blocks that go — the matched blocks plus the
	// children that go with them. The confirm binds to this number.
	Blocks int64 `json:"blocks"`
	// CascadedChildren: the part of Blocks that does not match the filter
	// and goes only because a parent goes.
	CascadedChildren int64 `json:"cascaded_children"`
	DreamLinks       int64 `json:"dream_links"`
	// Blobs: the scope's blobs; always 0 for a filtered purge.
	Blobs int64 `json:"blobs"`
	// DerivedSkipped: filter-matching derivatives, kept.
	DerivedSkipped int64 `json:"derived_skipped"`
	// ChildrenOutsideScope: children of matched blocks living in another
	// scope. A run refuses while it is not 0.
	ChildrenOutsideScope int64 `json:"children_outside_scope"`
}

// PurgeDeleted is what a run removed.
type PurgeDeleted struct {
	Blocks     int64 `json:"blocks"`
	DreamLinks int64 `json:"dream_links"`
	Blobs      int64 `json:"blobs"`
}

// PurgeResult answers a purge. Deleted is nil for a dry run and for a run
// refused before its first delete; on an error after it, Deleted holds what
// the committed batches removed. Counted is always the fresh count.
type PurgeResult struct {
	DryRun  bool          `json:"dry_run"`
	AsOf    time.Time     `json:"as_of"`
	Counted PurgeCounts   `json:"counted"`
	Deleted *PurgeDeleted `json:"deleted,omitempty"`
}

var (
	// ErrPurgeScopeNotFound: the scope is not one the reader may purge —
	// unknown, foreign, or no reader named. One answer for all three.
	ErrPurgeScopeNotFound = errors.New("store: purge: scope not found")
	// ErrPurgeScopeInvalid: empty or a '_' system scope.
	ErrPurgeScopeInvalid = errors.New("store: purge: scope must be a non-empty, non-system scope")
	// ErrPurgeConfirmMismatch: ConfirmBlocks differs from the fresh count;
	// nothing was deleted.
	ErrPurgeConfirmMismatch = errors.New("store: purge: confirmed block count differs from the current count")
	// ErrPurgeChildOutsideScope: a block that would go has a child in another
	// scope; nothing (more) was deleted.
	ErrPurgeChildOutsideScope = errors.New("store: purge: a matched block has a child in another scope")
)

// purgeAuditEntity is the context_settings_audit entity_type of a purge.
const purgeAuditEntity = "scope_purge"

// The match predicate is assembled from these constant fragments; caller
// values only ever travel as bind parameters ($1 = scope, $2 = as-of time).
// One statement per filter shape (the auditListSQL pattern) instead of
// `$n IS NULL OR …`, so a cached generic plan never loses the tags/category
// index for a narrow filter.
const (
	purgeMatchBase      = `FROM context_blocks b WHERE b.scope = $1 AND b.created_at <= $2`
	purgeTagsFrag       = "\n   AND b.tags @> $%d::text[]"
	purgeCategoryFrag   = "\n   AND b.category = $%d"
	purgeBeforeFrag     = "\n   AND b.created_at < $%d"
	purgeAfterFrag      = "\n   AND b.created_at > $%d"
	purgeArchivedFrag   = "\n   AND b.is_archived"
	purgeDerivedExpr    = `NOT (` + notDerivedSQL + `)`
	purgeNotDerivedFrag = "\n   AND " + notDerivedSQL
)

// purgeMatch returns the FROM/WHERE of the filter-matching blocks (derivatives
// included) and its arguments.
func purgeMatch(scope string, asOf time.Time, f PurgeFilter) (string, []any) {
	var b strings.Builder
	b.WriteString(purgeMatchBase)
	args := []any{scope, asOf}
	if len(f.Tags) > 0 {
		args = append(args, f.Tags)
		fmt.Fprintf(&b, purgeTagsFrag, len(args))
	}
	if f.Category != "" {
		args = append(args, f.Category)
		fmt.Fprintf(&b, purgeCategoryFrag, len(args))
	}
	if f.CreatedBefore != nil {
		args = append(args, *f.CreatedBefore)
		fmt.Fprintf(&b, purgeBeforeFrag, len(args))
	}
	if f.CreatedAfter != nil {
		args = append(args, *f.CreatedAfter)
		fmt.Fprintf(&b, purgeAfterFrag, len(args))
	}
	if f.ArchivedOnly {
		b.WriteString(purgeArchivedFrag)
	}
	return b.String(), args
}

// purgeCountSQL counts in one statement. kids is the parent_id closure below
// the non-derived matches (UNION: a cycle terminates); gone is what the run
// would delete.
const purgeCountSQL = `WITH RECURSIVE m AS (
  SELECT b.id, ` + purgeDerivedExpr + ` AS derived
  %s
), hit AS (SELECT id FROM m WHERE NOT derived),
kids AS (
  SELECT c.id, c.scope FROM context_blocks c JOIN hit ON c.parent_id = hit.id
  UNION
  SELECT c.id, c.scope FROM context_blocks c JOIN kids k ON c.parent_id = k.id
), gone AS (SELECT id FROM hit UNION SELECT id FROM kids)
SELECT (SELECT count(*) FROM gone),
       (SELECT count(*) FROM gone) - (SELECT count(*) FROM hit),
       (SELECT count(*) FROM context_dream_links l
         WHERE l.source_block_id IN (SELECT id FROM gone) OR l.target_block_id IN (SELECT id FROM gone)),
       (SELECT count(*) FROM m WHERE derived),
       (SELECT count(*) FROM kids WHERE kids.scope <> $1)`

// purgeKidsSQL locks the parent_id closure below one batch and names each
// row's scope and creation time.
const purgeKidsSQL = `WITH RECURSIVE kids AS (
  SELECT c.id FROM context_blocks c WHERE c.parent_id = ANY($1::uuid[])
  UNION
  SELECT c.id FROM context_blocks c JOIN kids k ON c.parent_id = k.id
)
SELECT c.id::text, c.scope, c.created_at FROM context_blocks c WHERE c.id IN (SELECT id FROM kids) FOR UPDATE`

// purgeBlobsSQL drains the scope's blobs created up to the as-of time.
const purgeBlobsSQL = `DELETE FROM context_blobs WHERE ctid IN (
  SELECT ctid FROM context_blobs WHERE scope = $1 AND created_at <= $2 LIMIT $3)`

// PurgeScope counts — and with ConfirmBlocks set, deletes — the block data a
// filter selects in one scope (see the comment above PurgeFilter). On
// ErrPurgeConfirmMismatch and ErrPurgeChildOutsideScope the result carries the
// fresh count.
func PurgeScope(ctx context.Context, pool *pgxpool.Pool, q PurgeQuery) (*PurgeResult, error) {
	tenantID, err := purgeScopeOwner(ctx, pool, q)
	if err != nil {
		return nil, err
	}
	res := &PurgeResult{DryRun: q.ConfirmBlocks == nil}
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&res.AsOf); err != nil {
		return nil, fmt.Errorf("store: purge as-of: %w", err)
	}
	match, args := purgeMatch(q.Scope, res.AsOf, q.Filter)
	c := &res.Counted
	if err := pool.QueryRow(ctx, fmt.Sprintf(purgeCountSQL, match), args...).
		Scan(&c.Blocks, &c.CascadedChildren, &c.DreamLinks, &c.DerivedSkipped, &c.ChildrenOutsideScope); err != nil {
		return nil, fmt.Errorf("store: purge count: %w", err)
	}
	unfiltered := q.Filter.empty()
	if unfiltered {
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blobs WHERE scope = $1 AND created_at <= $2`, q.Scope, res.AsOf).
			Scan(&c.Blobs); err != nil {
			return nil, fmt.Errorf("store: purge count blobs: %w", err)
		}
	}
	if res.DryRun {
		return res, nil
	}
	if *q.ConfirmBlocks != c.Blocks {
		return res, ErrPurgeConfirmMismatch
	}
	if c.ChildrenOutsideScope > 0 {
		return res, ErrPurgeChildOutsideScope
	}
	if c.Blocks == 0 && c.Blobs == 0 {
		res.Deleted = &PurgeDeleted{}
		return res, nil
	}
	if err := writePurgeAudit(ctx, pool, q, tenantID, res); err != nil {
		return nil, err
	}
	del := &PurgeDeleted{}
	pick := `SELECT b.id::text ` + match + purgeNotDerivedFrag + fmt.Sprintf("\n LIMIT $%d FOR UPDATE", len(args)+1)
	pickArgs := append(append([]any{}, args...), pruneBatchSize)
	b := purgeBatch{scope: q.Scope, asOf: res.AsOf, confirmed: c.Blocks, pick: pick, pickArgs: pickArgs}
	for attempt := 0; ; {
		n, err := purgeBlockBatch(ctx, pool, b, del)
		if isPurgeRetryable(err) && attempt < purgeBatchRetries {
			attempt++ // the batch rolled back whole; run it again
			continue
		}
		if err != nil {
			res.Deleted = del // what the committed batches before this one removed
			return res, err
		}
		if n == 0 {
			break
		}
		attempt = 0
	}
	if unfiltered {
		n, err := drainBatched(ctx, pool, purgeBlobsSQL, q.Scope, res.AsOf, pruneBatchSize)
		if err != nil {
			del.Blobs = n
			res.Deleted = del
			return res, fmt.Errorf("store: purge blobs: %w", err)
		}
		del.Blobs = n
	}
	res.Deleted = del
	return res, nil
}

// purgeScopeOwner applies the reader rule of PurgeQuery and returns the
// scope's owning tenant ("" for a scope no tenant owns, reachable by a
// server-admin only).
func purgeScopeOwner(ctx context.Context, pool *pgxpool.Pool, q PurgeQuery) (string, error) {
	if q.Scope == "" || strings.HasPrefix(q.Scope, "_") {
		return "", ErrPurgeScopeInvalid
	}
	if !q.AllTenants && q.TenantID == "" {
		return "", ErrPurgeScopeNotFound
	}
	var owner string
	err := pool.QueryRow(ctx,
		`SELECT tenant_id::text FROM context_tenant_scopes WHERE scope = $1`, q.Scope).Scan(&owner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: purge scope owner: %w", err)
	}
	if !q.AllTenants && owner != q.TenantID {
		return "", ErrPurgeScopeNotFound
	}
	return owner, nil
}

// purgeBatchRetries bounds how often one batch is run again after a deadlock
// or serialization failure. The batch locks blocks before it deletes their
// dream links, the dream cycle takes link rows before the blocks they point
// at, so a dream run on the very rows being purged can close a cycle (40P01);
// PostgreSQL aborts one side, and the purge batch, rolled back whole, is safe
// to repeat. A fixed bound, not a policy: three attempts separate a transient
// collision from a persistent one, which then surfaces as the error.
const purgeBatchRetries = 3

// isPurgeRetryable reports a deadlock (40P01) or serialization failure (40001).
func isPurgeRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// purgeBatch is what every batch of one run shares.
type purgeBatch struct {
	scope     string
	asOf      time.Time
	confirmed int64 // the confirmed Blocks: no run deletes more
	pick      string
	pickArgs  []any
}

// purgeBlockBatch deletes one batch (steps 1-4 of the comment above
// PurgeFilter) in one transaction and returns the number of matched blocks it
// picked; 0 ends the loop. del grows only after the commit. A batch that would
// carry the run past the confirmed count, or that reaches a child created
// after the count, rolls back with ErrPurgeConfirmMismatch: the set changed
// since it was counted (a comment written meanwhile, a row that came to match
// the filter).
func purgeBlockBatch(ctx context.Context, pool *pgxpool.Pool, pb purgeBatch, del *PurgeDeleted) (int, error) {
	picked := 0
	var blocks, links int64
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: purge batch"), func(tx pgx.Tx) error {
		batch, err := collectStrings(ctx, tx, pb.pick, pb.pickArgs...)
		if err != nil {
			return fmt.Errorf("store: purge pick: %w", err)
		}
		picked = len(batch)
		if picked == 0 {
			return nil
		}
		gone := make(map[string]struct{}, len(batch))
		for _, id := range batch {
			gone[id] = struct{}{}
		}
		rows, err := tx.Query(ctx, purgeKidsSQL, batch)
		if err != nil {
			return fmt.Errorf("store: purge children: %w", err)
		}
		for rows.Next() {
			var id, kidScope string
			var kidCreated time.Time
			if err := rows.Scan(&id, &kidScope, &kidCreated); err != nil {
				rows.Close()
				return fmt.Errorf("store: purge children scan: %w", err)
			}
			if kidScope != pb.scope {
				rows.Close()
				return ErrPurgeChildOutsideScope
			}
			if kidCreated.After(pb.asOf) {
				rows.Close()
				return ErrPurgeConfirmMismatch
			}
			gone[id] = struct{}{}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: purge children: %w", err)
		}
		if del.Blocks+int64(len(gone)) > pb.confirmed {
			return ErrPurgeConfirmMismatch
		}
		all := make([]string, 0, len(gone))
		for id := range gone {
			all = append(all, id)
		}

		lrows, err := tx.Query(ctx,
			`DELETE FROM context_dream_links
			  WHERE source_block_id = ANY($1::uuid[]) OR target_block_id = ANY($1::uuid[])
			 RETURNING target_block_id::text, relationship`, all)
		if err != nil {
			return fmt.Errorf("store: purge dream links: %w", err)
		}
		var targets []string
		for lrows.Next() {
			var target, rel string
			if err := lrows.Scan(&target, &rel); err != nil {
				lrows.Close()
				return fmt.Errorf("store: purge dream links scan: %w", err)
			}
			links++
			if _, goes := gone[target]; rel == "supersedes" && !goes {
				targets = append(targets, target)
			}
		}
		lrows.Close()
		if err := lrows.Err(); err != nil {
			return fmt.Errorf("store: purge dream links: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM context_blocks WHERE id = ANY($1::uuid[])`, batch); err != nil {
			return fmt.Errorf("store: purge blocks: %w", err)
		}
		if _, err := ReconcileSupersedesTargets(ctx, tx, targets, ""); err != nil {
			return fmt.Errorf("store: purge supersedes reconcile: %w", err)
		}
		blocks = int64(len(all))
		return nil
	})
	if err == nil {
		del.Blocks += blocks
		del.DreamLinks += links
	}
	return picked, err
}

// collectStrings runs a one-column text query inside tx.
func collectStrings(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// writePurgeAudit records the purge before its first delete (see the comment
// above PurgeFilter).
func writePurgeAudit(ctx context.Context, pool *pgxpool.Pool, q PurgeQuery, tenantID string, res *PurgeResult) error {
	nv, err := json.Marshal(map[string]any{"filter": q.Filter, "counted": res.Counted, "as_of": res.AsOf})
	if err != nil {
		return fmt.Errorf("store: purge audit value: %w", err)
	}
	return pgxdb.Write(ctx, pool, pgxdb.At("store: purge audit"), func(tx pgx.Tx) error {
		if err := stampAudit(ctx, tx, q.Stamp); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO context_settings_audit
			        (entity_type, entity_key, scope, action, new_value, api_key_id, actor_label, metadata)
			 SELECT $1::text, $2::text, $2::varchar, 'purge', $3::jsonb, a.actor, a.actor_label,
			        a.meta || jsonb_strip_nulls(jsonb_build_object('tenant_id', NULLIF($4::text, '')))
			   FROM audit_actor_meta() a`,
			purgeAuditEntity, q.Scope, nv, tenantID); err != nil {
			return fmt.Errorf("store: purge audit: %w", err)
		}
		return nil
	})
}
