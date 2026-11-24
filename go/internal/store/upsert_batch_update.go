// Update items and parents of a block batch (issue #211): the lock pass of an
// update item, its write through UpdateBlock's transaction half, the checks
// that keep two items off one block, and the parent links a batch writes after
// its items.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readBatchUpdateTargets reads, without a lock, the identity each update
// item's block has now: the scope whose supersedes lock the batch takes for it
// and the key the lock pass orders it by. ONE statement for every update item
// (primary-key probes through = ANY). An id that names no live block of the
// item's write scopes — a malformed id included — refuses the batch with
// ErrBlockNotFound, the single update's miss; a derivative is found here and
// refused by the lock pass, as UpdateBlock's WHERE refuses it.
func readBatchUpdateTargets(ctx context.Context, tx pgx.Tx, items []BatchUpsert) ([]batchIdentity, error) {
	current := make([]batchIdentity, len(items))
	var ids, scopes []string
	for _, it := range items {
		if it.Update == nil {
			continue
		}
		if parsed, err := uuid.Parse(it.Update.ID); err == nil && IsFullUUID(it.Update.ID) {
			ids = append(ids, parsed.String())
		}
		for _, s := range it.Update.WriteScopes {
			if !slices.Contains(scopes, s) {
				scopes = append(scopes, s)
			}
		}
	}
	if scopes == nil {
		return current, nil
	}
	found := make(map[string]batchIdentity, len(ids))
	if len(ids) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT id::text, scope, category, title FROM context_blocks
			  WHERE id = ANY($1::uuid[]) AND scope = ANY($2::text[]) AND NOT is_archived`, ids, scopes)
		if err != nil {
			return nil, fmt.Errorf("store: upsert block batch: read update targets: %w", err)
		}
		for rows.Next() {
			var id string
			var ident batchIdentity
			if err := rows.Scan(&id, &ident.scope, &ident.category, &ident.title); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: upsert block batch: read update targets: %w", err)
			}
			found[id] = ident
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: upsert block batch: read update targets: %w", err)
		}
	}
	for i, it := range items {
		if it.Update == nil {
			continue
		}
		parsed, err := uuid.Parse(it.Update.ID)
		ident, ok := found[parsed.String()]
		if err != nil || !ok || !slices.Contains(it.Update.WriteScopes, ident.scope) {
			return nil, &BatchItemError{Index: i, Err: ErrBlockNotFound}
		}
		current[i] = ident
	}
	return current, nil
}

// changedSQL is the boolean expression that tells whether the update would
// change the row it reads: any assignment of its SET list whose value differs
// from the column — updated_at aside, which every update sets. The
// expressions are the statement's own, over the statement's arguments, so the
// decision cannot drift from the write it predicts.
func (u *blockUpdate) changedSQL() string {
	var sb []byte
	sb = append(sb, "NOT ("...)
	for i, a := range u.assign {
		if i > 0 {
			sb = append(sb, " AND "...)
		}
		sb = append(sb, "("+a.expr+") IS NOT DISTINCT FROM "+a.col...)
	}
	sb = append(sb, ")"...)
	return string(sb)
}

// pinSQL is the ExpectUpdatedAt condition of the statement, "" without a pin.
func (u *blockUpdate) pinSQL() string {
	if u.pinIdx == 0 {
		return ""
	}
	return fmt.Sprintf(" AND updated_at = $%d", u.pinIdx)
}

// lockBatchUpdate is the lock pass of an update item: its row FOR NO KEY
// UPDATE — the lock the UPDATE itself takes — by id, within the write scopes,
// live, not a derivative, at the pinned updated_at of a confirmed card, and in
// a scope whose supersedes lock the batch holds (held: the scope it read for
// the item, and the one an earlier run found the block in). In the same statement the row is compared with what the update
// writes (changedSQL): an update that changes nothing is the item's no-op.
// Edits resolve on the content of the held row first (the content read with
// the lock, the comparison a statement after it). A scope move then locks the
// targets of the block's supersedes links, in id order, so the reconcile after
// its sweep waits for no row lock (lockSupersedesSource's discipline).
func lockBatchUpdate(ctx context.Context, tx pgx.Tx, u *blockUpdate, held []string) (batchHold, error) {
	var rowID, content string
	var ident batchIdentity
	var changed bool
	var err error
	if u.edits == nil {
		args := append(slices.Clone(u.args), held)
		err = tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT id::text, scope, category, title, %s FROM context_blocks
			  WHERE id = $%d AND scope = ANY($%d::text[]) AND NOT is_archived AND `+notDerivedSQL+`%s AND scope = ANY($%d::text[])
			  FOR NO KEY UPDATE`,
			u.changedSQL(), u.idIdx, u.scopeIdx, u.pinSQL(), len(args)), args...,
		).Scan(&rowID, &ident.scope, &ident.category, &ident.title, &changed)
	} else {
		args := []any{u.id, u.writeScopes, held}
		pin := ""
		if u.data.ExpectUpdatedAt != nil {
			args = append(args, *u.data.ExpectUpdatedAt)
			pin = " AND updated_at = $4"
		}
		err = tx.QueryRow(ctx,
			`SELECT id::text, scope, category, title, content FROM context_blocks
			  WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived AND `+notDerivedSQL+pin+` AND scope = ANY($3::text[])
			  FOR NO KEY UPDATE`, args...,
		).Scan(&rowID, &ident.scope, &ident.category, &ident.title, &content)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return batchHold{}, batchUpdateMiss(ctx, tx, u, held)
	}
	if err != nil {
		return batchHold{}, fmt.Errorf("store: upsert block batch: lock update target: %w", err)
	}
	if u.edits != nil {
		edited, times, editErr := applyEditsAndDates(content, u.edits.edits)
		if editErr != nil {
			return batchHold{}, editErr
		}
		u.args[u.contentArg], u.args[u.contentArg+1] = edited, times
		if err := tx.QueryRow(ctx, fmt.Sprintf(
			`SELECT %s FROM context_blocks WHERE id = $%d AND scope = ANY($%d::text[])%s`,
			u.changedSQL(), u.idIdx, u.scopeIdx, u.pinSQL()), u.args...).Scan(&changed); err != nil {
			return batchHold{}, fmt.Errorf("store: upsert block batch: compare update target: %w", err)
		}
	}
	h := batchHold{rowID: rowID, hadRow: true, identity: ident, from: ident}
	if u.data.Scope != nil {
		h.identity.scope = *u.data.Scope
	}
	if u.data.Category != nil {
		h.identity.category = *u.data.Category
	}
	if u.data.Title != nil {
		h.identity.title = *u.data.Title
	}
	if !changed {
		h.match = IdenticalContent{ID: rowID, TagsEqual: true, MetadataEqual: true, SensitivityEqual: true, TypeEqual: true}
		return h, nil
	}
	if u.moveScopes != nil {
		if h.targets, err = lockLateSupersedesTargets(ctx, tx, []string{rowID}, []string{ident.scope}, []string{rowID}); err != nil {
			return batchHold{}, err
		}
	}
	return h, nil
}

// batchUpdateMiss is the reason an update item's lock found no row, read in
// the batch's transaction: no live block of the write scopes
// (ErrBlockNotFound), a derivative (ErrProvenanceProtected, UpdateBlock's
// answer), a block another transaction moved out of the scopes the batch
// locked for it (a scopesToLockError naming the scope it is in now: the batch
// runs again and locks that scope too, in order), or a confirmed card's block
// at another updated_at (ErrBlockChanged).
func batchUpdateMiss(ctx context.Context, tx pgx.Tx, u *blockUpdate, held []string) error {
	var scope string
	var notDerived bool
	var updatedAt time.Time
	err := tx.QueryRow(ctx,
		`SELECT scope, `+notDerivedSQL+`, updated_at FROM context_blocks
		  WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived`,
		u.id, u.writeScopes).Scan(&scope, &notDerived, &updatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrBlockNotFound
	case err != nil:
		return fmt.Errorf("store: upsert block batch: update target: %w", err)
	case !notDerived:
		return ErrProvenanceProtected
	case !slices.Contains(held, scope):
		return &scopesToLockError{scopes: []string{scope}}
	case u.data.ExpectUpdatedAt != nil && !updatedAt.Equal(*u.data.ExpectUpdatedAt):
		return ErrBlockChanged
	}
	return ErrBlockNotFound
}

// writeBatchUpdate writes an update item whose row the lock pass holds,
// through UpdateBlock's transaction half (execHeld): the edits were resolved
// by the lock pass, the supersedes targets of a scope move locked there.
func writeBatchUpdate(ctx context.Context, tx pgx.Tx, u *blockUpdate, h batchHold) (*Block, error) {
	b := &Block{}
	swept, _, err := u.execHeld(ctx, tx, b, updateHold{targets: h.targets})
	if errors.Is(err, errUpdateMiss) {
		return nil, ErrBlockNotFound
	}
	if err != nil {
		return nil, err
	}
	if swept > 0 {
		slog.Info("store: scope move swept divergent links", "block_id", u.id, "new_scope", b.Scope, "links_deleted", swept)
	}
	return b, nil
}

// batchIdentityClash refuses the later of two items whose blocks meet on one
// (category, title, scope):
//
//   - two items whose blocks would hold it after the batch — a store item's
//     identity, an update item's identity after its rename or move. The unique
//     index would refuse the second write only if the first were written
//     before it; in the other order the store item's upsert would write into
//     the block the update just moved there;
//   - an update item that renames or moves its block onto the identity another
//     update item's block leaves (a chain or a swap of titles). Whether the
//     unique index lets such a batch through would depend on the order the
//     batch writes in — the identities' sort order, not the client's — so it
//     is refused whatever that order is.
//
// It also refuses a supersedes declaration whose target an update item of the
// batch moves to another scope: the move's link sweep would end the very
// declaration the batch reports (supersedes holds within one scope), so the
// two items would write one block's supersedes state against each other.
func batchIdentityClash(items []BatchUpsert, holds []batchHold) error {
	final := make(map[batchIdentity]int, len(items))
	for i := range items {
		id := holds[i].identity
		if first, dup := final[id]; dup {
			return &BatchItemError{Index: i, Err: &BatchSameBlockError{First: first, Identity: true}}
		}
		final[id] = i
	}
	moved := make(map[string]int, len(items))
	for k, it := range items {
		from := holds[k].from
		if it.Update == nil || holds[k].identity == from {
			continue
		}
		if i, taken := final[from]; taken && i != k {
			return &BatchItemError{Index: max(i, k), Err: &BatchSameBlockError{First: min(i, k), Identity: true}}
		}
		if holds[k].identity.scope != from.scope {
			moved[holds[k].rowID] = k
		}
	}
	for i, h := range holds {
		if k, ok := moved[h.target.id]; ok && h.target.id != "" {
			return &BatchItemError{Index: max(i, k), Err: &BatchSameBlockError{First: min(i, k)}}
		}
	}
	return nil
}

// linkBatchParents hangs each item that names a parent under it, in list
// order, after every item is written: a ParentItem names the block the earlier
// item wrote (or, for an item that changed nothing, the block it matched).
// The link is the one of /api/store (LinkBlockParent), so a refusal is its
// ErrLinkScopeViolation, named by the item.
func linkBatchParents(ctx context.Context, tx pgx.Tx, items []BatchUpsert, results []BatchResult) error {
	for i, it := range items {
		if it.Update != nil || (it.ParentID == "" && it.ParentItem == nil) {
			continue
		}
		parent := it.ParentID
		if it.ParentItem != nil {
			r := results[*it.ParentItem]
			parent = r.Match.ID
			if r.Block != nil {
				parent = r.Block.ID
			}
		}
		if err := LinkBlockParent(ctx, tx, results[i].Block.ID, parent, it.Scope); err != nil {
			return &BatchItemError{Index: i, Err: err}
		}
	}
	return nil
}

// batchIdentityHolder names the holder of the identity an update item of a
// refused batch asked for (*BlockIdentityTakenError.ConflictID), as
// UpdateBlock names it: read after the rollback, within the item's write
// scopes (blockIdentityHolder).
func batchIdentityHolder(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, err error) {
	var itemErr *BatchItemError
	var taken *BlockIdentityTakenError
	if !errors.As(err, &itemErr) || !errors.As(err, &taken) || taken.ConflictID != "" {
		return
	}
	if up := items[itemErr.Index].Update; up != nil {
		taken.ConflictID = blockIdentityHolder(ctx, pool, up.ID, up.Data, up.WriteScopes, taken.refusedKey)
	}
}
