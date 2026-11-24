// All-or-none block batches (issue #211): the core the MCP `store_batch` tool
// and REST POST /api/store/batch share. Two surfaces, one function: each item
// runs the write gate chain of its single write — a store item the chain of a
// single store (runStageWriteGates — the chain of /api/store and both arms of
// the MCP `store` tool), an update item the one of a single update
// (gateUpdateItem) — the batch runs its own gates over the items as a whole,
// and the writes land through store.UpsertBlocks in ONE transaction. Any
// refusal — of one item or of the batch — refuses the batch before or inside
// that transaction and leaves nothing written: no block row, no parent link,
// no write-log row, no budget spent. A refusal of one item names the item
// (batchItemPrefix); one of the batch as a whole — its shape, its write
// budget — names none.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// storeBatchLimits is what one batch reads from the request's config
// snapshot: the per-item sensitivity default, the per-key write budget and the
// batch cap (writes.batch_max_items).
type storeBatchLimits struct {
	DefaultSens    backends.Sensitivity
	RateLimitWrite int
	MaxItems       int
}

// storeBatchEntry is one item of a batch as its surface decoded it: a store
// item (Store, with the earlier item it hangs under, ParentItem) or an update
// item (Update) — exactly one of the two. The form is told by the item's `id`:
// an item that names a block by id updates it, every other item is a store
// item, so a batch written before update items existed is read as it always
// was. Misplaced is the refusal of a field the item's form does not take,
// "" for none.
type storeBatchEntry struct {
	Store      *storeRequest
	ParentItem *int
	Update     *storeBatchUpdate
	Misplaced  string
}

// storeBatchUpdate is an update item: the id as sent, the patch, and the
// fields it names (the staged card binds them as UpdateFields).
type storeBatchUpdate struct {
	ID     string
	Data   store.UpdateBlockData
	Fields []string
}

// The refusals of a field an item's form does not take.
const (
	storeItemUpdateFieldsMsg = "edits and metadata_merge belong to an update item — name the block by id"
	mcpUpdateItemFieldsMsg   = "sensitivity, scope and supersedes are not part of an update item on this surface (the update tool's form)"
	restUpdateItemFieldsMsg  = "supersedes, parent_id and parent_item are not part of an update item"
	parentBothMsg            = "parent_id and parent_item are mutually exclusive — name an existing parent by id or an earlier item of this batch"
	parentItemMsg            = "parent_item must name an earlier item of this batch"
	nothingToUpdateMsg       = "nothing to update — provide at least one of category, title, content, tags, metadata, type"
	updateNotFoundMsg        = "Block not found (or not in a writable scope of this key)"
)

// storeBatchItem is one item's outcome. Block is the written block, nil for an
// item that changes nothing (UnchangedID names the block it already matches,
// the single store's identical-content no-op, or the block an update item
// would leave as it is). Changed lists what an identical-content write
// rewrites (the /api/store "changed"), Supersedes the block it declared to
// supersede. Update marks an update item.
type storeBatchItem struct {
	Block       *store.Block
	UnchangedID string
	Changed     []string
	Supersedes  string
	Update      bool
	// SensWarning: an explicit sensitivity the upgrade-only upsert did not
	// apply (the /api/store "warnings" entry), "" otherwise.
	SensWarning string
}

// batchItemPrefix names an item in a refusal, as /api/ingest names a chunk.
func batchItemPrefix(i int) string { return fmt.Sprintf("items[%d]: ", i) }

// storeBatchPlan is a batch that passed its gates: the items as sent and what
// the gates decided for each — for a store item the resolved scope,
// post-detector sensitivity and metadata (gated), for an update item the
// block id it resolved to (resolved) — and the key's writable scopes.
type storeBatchPlan struct {
	items    []storeBatchEntry
	gated    []*stageWriteGateResult
	resolved []string
	writable []string
}

// upserts is the store's form of the items, in their order.
func (p *storeBatchPlan) upserts() []store.BatchUpsert {
	batch := make([]store.BatchUpsert, len(p.items))
	for i, e := range p.items {
		if e.Update != nil {
			batch[i] = store.BatchUpsert{Update: &store.BatchUpdate{ID: p.resolved[i], Data: e.Update.Data, WriteScopes: p.writable}}
			continue
		}
		it, res := e.Store, p.gated[i]
		batch[i] = store.BatchUpsert{
			Category: it.Category, Title: it.Title, Content: it.Content, Tags: it.Tags,
			Metadata: res.Metadata, Scope: res.WriteScope, ScopeExplicit: res.ScopeExplicit,
			Sens: res.Sens, TypeName: it.Type, Supersedes: it.Supersedes,
			ParentID: it.ParentID, ParentItem: e.ParentItem,
		}
	}
	return batch
}

// runStoreBatch is the shared core of the direct path: the gates, then the one
// transaction — every item locked, the no-op items decided on their locked
// rows, the budget checked and booked for the items that write — then the
// afterwork of each written block. A non-nil reject means nothing was written
// and nothing was booked.
func runStoreBatch(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult,
	items []storeBatchEntry, lim storeBatchLimits, reqID string,
) ([]storeBatchItem, *writeReject) {
	plan, rej := gateStoreBatch(ctx, pool, set, ar, items, lim, reqID)
	if rej != nil {
		return nil, rej
	}
	results, err := store.UpsertBlocks(ctx, pool, plan.upserts(), ar.ApiKeyID, set,
		&store.WriteBudget{APIKeyID: ar.ApiKeyID, Limit: lim.RateLimitWrite})
	if err != nil {
		return nil, storeBatchFailureReject(ctx, err, reqID)
	}
	out := storeBatchOutcome(results, items)
	if batchWrote(out) {
		noteToolWrite(ctx, nil)
	}
	// The blocks and their booking are committed: the afterwork must not stop
	// halfway because the client went away, and one bound ends all of it
	// (followUpContext, issue #453) — not one per block, so a shutdown's
	// drain waits for a whole batch no longer than followUpTimeout. Each
	// block's afterwork is non-fatal and its own: a fault in one is logged and
	// leaves the others, and the booking (made in the transaction) as it is.
	after, cancel := followUpContext(ctx)
	defer cancel()
	for i, r := range results {
		b := r.Block
		if b == nil {
			continue
		}
		if u := items[i].Update; u != nil {
			finishBlockUpdateSet(after, pool, set, u.Data, b, reqID)
			continue
		}
		out[i].Supersedes = strings.ToLower(items[i].Store.Supersedes)
		if res := plan.gated[i]; res.Sens.Manual && b.Sensitivity != string(res.Sens.Value) {
			out[i].SensWarning = fmt.Sprintf(
				"sensitivity %s not applied: existing block is %s — downgrades need manage update with confirm_sensitivity_downgrade",
				res.Sens.Value, b.Sensitivity)
		}
		finishStoredBlock(after, pool, set, b, reqID)
	}
	return out, nil
}

// storeBatchOutcome is each item's outcome as the store decided it: the
// written block and what an identical-content write rewrote, or the block an
// unchanged item matches.
func storeBatchOutcome(results []store.BatchResult, items []storeBatchEntry) []storeBatchItem {
	out := make([]storeBatchItem, len(results))
	for i, r := range results {
		out[i].Update = items[i].Update != nil
		if r.Block == nil {
			out[i].UnchangedID = r.Match.ID
			continue
		}
		out[i].Block = r.Block
		out[i].Changed = r.Match.Changed()
	}
	return out
}

// storeBatchFailureReject renders a failed store.UpsertBlocks on the direct
// path: the budget refusal, an item's refusal (storeBatchRefusal), else the
// opaque 500 with the cause in the log.
func storeBatchFailureReject(ctx context.Context, err error, reqID string) *writeReject {
	if rej := storeBatchBudgetReject(err); rej != nil {
		return rej
	}
	if rej := storeBatchRefusal(err); rej != nil {
		return rej
	}
	noteToolWrite(ctx, err)
	logRequestError(ctx, "store batch: upsert error", "error", err, "write_outcome", writeOutcome(err), "request_id", reqID)
	return classInternal.reject("Internal server error")
}

// storeBatchRefusal is the refusal of one item inside the transaction, as its
// single write answers it — provenance_protected, the re-type of a comment or
// an issue (constraint), the supersedes classes, an update's identity
// collision and edit mismatch, an update target that is gone (unknown_block),
// a parent it cannot hang under (unknown_parent), two items on one block
// (constraint) — named by its position in the caller's list. nil when err is
// no such refusal (a fault).
func storeBatchRefusal(err error) *writeReject {
	var itemErr *store.BatchItemError
	if !errors.As(err, &itemErr) {
		return nil
	}
	rej := batchItemReject(err)
	if rej == nil {
		return nil
	}
	return rej.prefixed(batchItemPrefix(itemErr.Index))
}

// batchItemReject maps the refusal of one item onto the class its single
// write answers it with; nil for a fault.
func batchItemReject(err error) *writeReject {
	if rej := provenanceRejectOr(err, retypeBoundReject(err)); rej != nil {
		return rej
	}
	if rej := supersedesReject(err); rej != nil {
		return rej
	}
	if rej := updateFailureReject(err); rej != nil {
		return rej
	}
	var same *store.BatchSameBlockError
	switch {
	case errors.Is(err, store.ErrBlockNotFound):
		return classUnknownBlock.reject(updateNotFoundMsg)
	case errors.Is(err, store.ErrLinkScopeViolation):
		return classUnknownParent.reject(parentNotLinkableMsg)
	case errors.Is(err, store.ErrBatchParentItem):
		return classInvalidBody.reject(parentItemMsg)
	case errors.As(err, &same) && same.Identity:
		return classConstraint.reject(fmt.Sprintf("same category, title and scope as items[%d] — a batch writes each block once", same.First))
	case errors.As(err, &same):
		return classConstraint.reject(fmt.Sprintf("same block as items[%d] — a batch writes each block once", same.First))
	}
	return nil
}

// gateStoreBatch runs every gate of a batch, in this order, each before
// anything is written or booked:
//
//  1. the batch shape — no item, more items than writes.batch_max_items;
//  2. each item in list order: a field its form does not take, then
//     - a store item: its parent fields (both named, or a parent_item that is
//     not an earlier item), then the single store's gate chain (required
//     fields, size caps, sensitivity, the I7 claim gates and the type
//     registry — a parent named either way satisfies a required-parent type
//     — the credentials detector, the write scope);
//     - an update item: the single update's gate chain (gateUpdateItem);
//     the first refusing item answers, with its own class and text behind
//     "items[i]: ";
//  3. two store items with one (category, title, scope) identity, two update
//     items of one block — a batch writes each block once (constraint).
//
// Whether an item would change nothing (the identical-content no-op of
// /api/store, an update whose every field already holds) is not decided here:
// store.UpsertBlocks decides it on the item's row while its transaction holds
// that row, so no other write can come between the decision and the batch's
// commit. The write budget — one write per item that writes, refused whole
// when they do not fit into what is left of query.rate_limit_write in the
// current window — is checked in the same transaction under the key's budget
// lock, after those decisions and before the first write (a staged batch: in
// the transaction that stages it, mcpStageStoreBatch). What the store decides
// on the row as it is — provenance_protected, the re-type of a comment or an
// issue, the supersedes refusals, an update's identity collision and edits,
// the parent and its cycle, a store item and an update item on one block — is
// decided in the transaction too, which then rolls back and names the item.
func gateStoreBatch(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult,
	items []storeBatchEntry, lim storeBatchLimits, reqID string,
) (*storeBatchPlan, *writeReject) {
	if rej := storeBatchShapeReject(len(items), lim.MaxItems); rej != nil {
		return nil, rej
	}
	plan := &storeBatchPlan{items: items, gated: make([]*stageWriteGateResult, len(items)),
		resolved: make([]string, len(items)), writable: writableBlockScopes(ar)}
	for i, e := range items {
		if e.Misplaced != "" {
			return nil, classInvalidBody.reject(batchItemPrefix(i) + e.Misplaced)
		}
		if e.Update != nil {
			id, rej := gateUpdateItem(ctx, pool, set, ar, e.Update, reqID)
			if rej != nil {
				return nil, rej.prefixed(batchItemPrefix(i))
			}
			plan.resolved[i] = id
			continue
		}
		res, rej := gateStoreItem(ctx, pool, set, ar, i, e, lim, reqID)
		if rej != nil {
			return nil, rej.prefixed(batchItemPrefix(i))
		}
		plan.gated[i] = res
	}
	if rej := storeBatchDuplicateReject(plan); rej != nil {
		return nil, rej
	}
	return plan, nil
}

// gateStoreItem is the gate chain of the store item at index i: its parent
// fields, then the single store's chain. A parent_item satisfies the claim
// gate of a required-parent type as a parent_id does (the parent is named;
// whether it can carry the block is decided in the transaction).
func gateStoreItem(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult,
	i int, e storeBatchEntry, lim storeBatchLimits, reqID string,
) (*stageWriteGateResult, *writeReject) {
	req := *e.Store
	if e.ParentItem != nil {
		if req.ParentID != "" {
			return nil, classInvalidBody.reject(parentBothMsg)
		}
		if *e.ParentItem < 0 || *e.ParentItem >= i {
			return nil, classInvalidBody.reject(parentItemMsg)
		}
		req.ParentID = batchItemPrefix(*e.ParentItem)
	}
	// The rate limit is checked once for the whole batch, in its
	// transaction: rateLimitWrite 0 here skips the chain's per-call check.
	return runStageWriteGates(ctx, pool, set, ar, req, lim.DefaultSens, 0, reqID)
}

// gateUpdateItem is the gate chain of a single update over one update item,
// for both batch surfaces, in the order the single update surfaces run it:
// the patch's shape (a field named at all, CheckUpdatePatch's combinations,
// the size caps), the I7 claim gates (manage update's verdict,
// updateClaimRejectSet), the scope a move names (writableBlockScopes), the id
// within the key's writable scopes (resolveUpdateTargetIn), and a sensitivity
// downgrade's confirm (sensitivityGuardReject). The MCP form carries no scope
// and no sensitivity, so its two gates are inert there. Returns the resolved
// block id. Each refusal carries a class; the single update surfaces answer
// the same texts uncoded (errcode.go SCOPE).
func gateUpdateItem(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult, u *storeBatchUpdate, reqID string) (string, *writeReject) {
	if len(u.Fields) == 0 {
		return "", classMissingFields.reject(nothingToUpdateMsg)
	}
	if err := store.CheckUpdatePatch(u.Data); err != nil {
		return "", classInvalidBody.reject(err.Error())
	}
	if msg := blockSizeLimit(strOrEmpty(u.Data.Category), strOrEmpty(u.Data.Title), strOrEmpty(u.Data.Content)); msg != "" {
		return "", classSizeCap.reject(msg)
	}
	if rej := updateClaimRejectSet(set, u.Data); rej != nil {
		return "", rej
	}
	if u.Data.Scope != nil && !contains(writableBlockScopes(ar), *u.Data.Scope) {
		return "", classScopeDenied.reject("Cannot set scope to requested value")
	}
	id, miss := resolveUpdateTargetIn(ctx, pool, ar, u.ID)
	if miss != "" {
		return "", classUnknownBlock.reject(miss)
	}
	if rej := sensitivityGuardReject(ctx, pool, ar, reqID, id, &u.Data); rej != nil {
		return "", rej
	}
	return id, nil
}

// storeBatchCanonicalItems is the staged form of the store items of a plan:
// each as a staged op 'store' binds it — the resolved scope, the
// post-detector metadata and sensitivity — so the confirm writes exactly what
// the gates decided. An update item's entry is the caller's (its card form is
// read from the block at stage time, mcpStageStoreBatch); its slot stays
// zero here.
func storeBatchCanonicalItems(p *storeBatchPlan) []store.CanonicalWrite {
	items := make([]store.CanonicalWrite, len(p.items))
	for i, e := range p.items {
		if e.Update != nil {
			continue
		}
		it, res := e.Store, p.gated[i]
		items[i] = store.CanonicalWrite{
			Op:                "store",
			Scope:             res.WriteScope,
			Category:          it.Category,
			Title:             it.Title,
			Content:           it.Content,
			Tags:              it.Tags,
			Metadata:          res.Metadata,
			Sensitivity:       string(res.Sens.Value),
			SensitivityManual: res.Sens.Manual,
			SensitivityDetect: res.Sens.Detector,
			Type:              it.Type,
			Supersedes:        it.Supersedes,
		}
	}
	return items
}

// confirmStoreBatchPrefix opens the confirmExecRejected wording of a staged
// batch the write refused after the consume, on both confirm surfaces.
const confirmStoreBatchPrefix = "confirmed store_batch not executed, nothing of it is written, the stage token is consumed: "

// confirmBatchReject re-validates a staged batch on the un-consumed row, item
// by item: the I7 claim gates against the registry of now
// (confirmClaimReject), and for an update item the drift pin its single card
// carries (confirmDriftReject: the block still in a writable scope, at the
// updated_at the card bound) — a drift in ANY item refuses the whole confirm.
// Each item's scope was re-checked against the key's CURRENT writable scopes
// before, with the row's (D1-M1, confirmScopeReject). nil for every other op,
// and for a batch that still passes. The first refusing item answers, named;
// the stage token survives, as for a single card.
func confirmBatchReject(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, ar *auth.AuthResult, cw store.CanonicalWrite) *confirmOutcome {
	if cw.Op != store.OpStoreBatch {
		return nil
	}
	if len(cw.Items) == 0 {
		// A card without items was not written by this binary.
		return &confirmOutcome{Kind: confirmMiss}
	}
	for i, it := range cw.Items {
		if it.Op != "store" && it.Op != "update" {
			return &confirmOutcome{Kind: confirmMiss}
		}
		if rej := confirmClaimReject(ctx, blocktypes, it); rej != nil {
			return &confirmOutcome{Kind: confirmClaimRejected, Op: cw.Op, Reject: rej.prefixed(batchItemPrefix(i))}
		}
	}
	for i, it := range cw.Items {
		if out := confirmDriftReject(ctx, pool, ar, it); out != nil {
			out.Op, out.Item = cw.Op, batchItemPrefix(i)
			return out
		}
	}
	return nil
}

// executeConfirmedStoreBatch runs op 'store_batch' AFTER the consume: the
// card's items through store.UpsertBlocks, the transaction of the direct path
// — an item whose row then already holds what it asks for is left as it is,
// decided on the locked row; an update item pinned to the updated_at its card
// bound, so a block that moved after the drift check refuses the whole batch
// (store.ErrBlockChanged) — then each written block's afterwork. No booking —
// the batch paid for its items at stage time, as every staged op does. A
// refusal the store decides on the row as it is then (provenance, the re-type
// of a comment or an issue, a supersedes target that changed, an update's
// identity collision) writes nothing and answers behind the prefix that says
// the token is spent, as a confirmed single write does.
func executeConfirmedStoreBatch(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, ar *auth.AuthResult, cw store.CanonicalWrite, pendingID string) confirmOutcome {
	var set *blocktype.Set
	if blocktypes != nil {
		set = blocktypes.SnapshotForRequest(ctx)
	}
	batch, entries, err := confirmedBatchItems(cw, writableBlockScopes(ar))
	if err != nil {
		noteToolWrite(ctx, err)
		slog.Error("confirm: staged update base unreadable", "error", err, "pending_id", pendingID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}
	noteToolCount(ctx, len(batch))
	results, err := store.UpsertBlocks(ctx, pool, batch, ar.ApiKeyID, set, nil)
	if err != nil {
		return confirmedBatchFailure(ctx, cw, err, pendingID)
	}
	out := storeBatchOutcome(results, entries)
	if batchWrote(out) {
		noteToolWrite(ctx, nil)
	}
	// Committed: one bound for the whole afterwork, as on the direct path.
	after, cancel := followUpContext(ctx)
	defer cancel()
	for i, r := range results {
		if r.Block == nil {
			continue
		}
		if u := entries[i].Update; u != nil {
			finishBlockUpdateSet(after, pool, set, u.Data, r.Block, RequestIDFromContext(ctx))
			continue
		}
		finishStoredBlock(after, pool, set, r.Block, RequestIDFromContext(ctx))
	}
	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Batch: out}
}

// confirmedBatchItems is the store's form of a card's items: a store item as
// executeConfirmedStore writes one — ScopeExplicit stays false for the reason
// it gives: the conflict target carries the scope, the assignment moves
// nothing — and an update item as executeConfirm writes one, rebuilt from its
// UpdateFields and pinned to its BaseUpdatedAt. entries mark the update items
// and carry their patches for the afterwork.
func confirmedBatchItems(cw store.CanonicalWrite, writable []string) ([]store.BatchUpsert, []storeBatchEntry, error) {
	batch := make([]store.BatchUpsert, len(cw.Items))
	entries := make([]storeBatchEntry, len(cw.Items))
	for i, it := range cw.Items {
		if it.Op == "update" {
			data := updateDataFromCanonical(it)
			base, err := parseStagedBase(it.BaseUpdatedAt)
			if err != nil {
				return nil, nil, err
			}
			data.ExpectUpdatedAt = &base
			batch[i] = store.BatchUpsert{Update: &store.BatchUpdate{ID: it.ID, Data: data, WriteScopes: writable}}
			entries[i].Update = &storeBatchUpdate{ID: it.ID, Data: data, Fields: it.UpdateFields}
			continue
		}
		batch[i] = store.BatchUpsert{
			Category: it.Category, Title: it.Title, Content: it.Content, Tags: it.Tags, Metadata: it.Metadata,
			Scope: it.Scope,
			Sens: store.SensitivityWrite{
				Value:    backends.Sensitivity(it.Sensitivity),
				Manual:   it.SensitivityManual,
				Detector: it.SensitivityDetect,
			},
			TypeName: it.Type, Supersedes: it.Supersedes,
		}
		entries[i].Store = &storeRequest{Supersedes: it.Supersedes}
	}
	return batch, entries, nil
}

// confirmedBatchFailure renders a confirmed batch the store refused or failed
// after the consume: a block of an update item that moved since the drift
// check (confirmExecDrift) or left the writable scopes (confirmExecGone),
// named by its item; any other refusal behind the token-spent prefix; a fault
// as the opaque execute error.
func confirmedBatchFailure(ctx context.Context, cw store.CanonicalWrite, err error, pendingID string) confirmOutcome {
	var itemErr *store.BatchItemError
	if errors.As(err, &itemErr) {
		item, id := batchItemPrefix(itemErr.Index), cw.Items[itemErr.Index].ID
		switch {
		case errors.Is(err, store.ErrBlockChanged):
			return confirmOutcome{Kind: confirmExecDrift, Op: cw.Op, BlockID: id, Item: item}
		case errors.Is(err, store.ErrBlockNotFound):
			return confirmOutcome{Kind: confirmExecGone, Op: cw.Op, BlockID: id, Item: item}
		}
	}
	if rej := storeBatchRefusal(err); rej != nil {
		return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op, Reject: rej.prefixed(confirmStoreBatchPrefix)}
	}
	noteToolWrite(ctx, err)
	logRequestError(ctx, "confirm: confirmed batch execute failed", "error", err, "write_outcome", writeOutcome(err), "pending_id", pendingID)
	return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
}

// storeBatchShapeReject refuses a batch without items or above the cap.
func storeBatchShapeReject(n, maxItems int) *writeReject {
	if n == 0 {
		return classMissingFields.reject("items: a batch needs at least one item")
	}
	if n > maxItems {
		return classSizeCap.reject(fmt.Sprintf(
			"items: %d items exceed the batch cap of %d (writes.batch_max_items; 0 switches batches off)", n, maxItems))
	}
	return nil
}

// storeBatchDuplicateReject refuses the second of two items that address one
// block: two store items with the same category and title in the same
// resolved scope, two update items whose ids resolve to one block. Each would
// write the same row, and which one the block keeps would depend on the order
// the batch writes in. (A store item and an update item on one block, and an
// update moved onto a store item's identity, meet only on the rows — the
// store refuses them in the transaction, store.BatchSameBlockError.)
//
// The same for two items that declare to supersede one block: a block is
// superseded by one block, so the second declaration would be refused inside
// the transaction as already superseded — naming as the head a block of this
// very batch, which the refusal rolls back.
func storeBatchDuplicateReject(p *storeBatchPlan) *writeReject {
	type identity struct{ scope, category, title string }
	seen := make(map[identity]int, len(p.items))
	ids := make(map[string]int, len(p.items))
	targets := make(map[string]int, len(p.items))
	for i, e := range p.items {
		if e.Update != nil {
			id := strings.ToLower(p.resolved[i])
			if first, dup := ids[id]; dup {
				return classConstraint.reject(fmt.Sprintf(
					"%ssame block as items[%d] — a batch writes each block once", batchItemPrefix(i), first))
			}
			ids[id] = i
			continue
		}
		it := e.Store
		id := identity{p.gated[i].WriteScope, it.Category, it.Title}
		if first, dup := seen[id]; dup {
			return classConstraint.reject(fmt.Sprintf(
				"%ssame category, title and scope as items[%d] — a batch writes each block once", batchItemPrefix(i), first))
		}
		seen[id] = i
		if it.Supersedes == "" {
			continue
		}
		target := strings.ToLower(it.Supersedes)
		if first, dup := targets[target]; dup {
			return classSupersedesConflict.reject(fmt.Sprintf(
				"%ssupersedes the same block as items[%d] — a block is superseded by one block", batchItemPrefix(i), first))
		}
		targets[target] = i
	}
	return nil
}

// storeBatchBudgetReject renders the budget refusal of a batch
// (*store.WriteBudgetError), nil for any other error. The prose opens with the
// single store's sentence, so a client that reads that one reads this one too.
func storeBatchBudgetReject(err error) *writeReject {
	var over *store.WriteBudgetError
	if !errors.As(err, &over) {
		return nil
	}
	return classRateLimit.reject(fmt.Sprintf(
		"Rate limit exceeded: max %d writes per 60 seconds — this batch writes %d blocks and %d writes of the window are used",
		over.Limit, over.Writes, over.Used))
}

// storeBatchRequest is the body of POST /api/store/batch: each item an
// /api/store body (a store item) or, with an `id`, a manage-update `data`
// object plus that id (an update item). The items are decoded one by one
// (decodeStoreBatchItem), so the form of each is told by its own keys.
type storeBatchRequest struct {
	Items []json.RawMessage `json:"items"`
}

// restStoreItem is a store item of the REST batch: an /api/store body plus
// the earlier item it hangs under; edits and metadata_merge are read only to
// refuse them (they belong to an update item).
type restStoreItem struct {
	storeRequest
	ParentItem    *int            `json:"parent_item"`
	Edits         json.RawMessage `json:"edits"`
	MetadataMerge json.RawMessage `json:"metadata_merge"`
}

// restUpdateItem is an update item of the REST batch: the id and manage
// update's `data` (store.UpdateBlockData: scope moves and sensitivity
// included); supersedes and the parent fields are read only to refuse them.
type restUpdateItem struct {
	ID string `json:"id"`
	store.UpdateBlockData
	Supersedes json.RawMessage `json:"supersedes"`
	ParentID   json.RawMessage `json:"parent_id"`
	ParentItem json.RawMessage `json:"parent_item"`
}

// decodeStoreBatchItem decodes one REST batch item: an item with a non-null
// `id` is an update item, every other one a store item, decoded exactly as
// /api/store decodes its body.
func decodeStoreBatchItem(raw json.RawMessage) (storeBatchEntry, error) {
	var probe struct {
		ID *string `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return storeBatchEntry{}, err
	}
	if probe.ID == nil {
		var it restStoreItem
		if err := json.Unmarshal(raw, &it); err != nil {
			return storeBatchEntry{}, err
		}
		e := storeBatchEntry{Store: &it.storeRequest, ParentItem: it.ParentItem}
		if it.Edits != nil || it.MetadataMerge != nil {
			e.Misplaced = storeItemUpdateFieldsMsg
		}
		return e, nil
	}
	var it restUpdateItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return storeBatchEntry{}, err
	}
	e := storeBatchEntry{Update: &storeBatchUpdate{ID: it.ID, Data: it.UpdateBlockData, Fields: updateDataFields(it.UpdateBlockData)}}
	if it.Supersedes != nil || it.ParentID != nil || it.ParentItem != nil {
		e.Misplaced = restUpdateItemFieldsMsg
	}
	return e, nil
}

// updateDataFields names the fields a manage-update patch sets, in the
// order of store.UpdateBlockData.
func updateDataFields(d store.UpdateBlockData) []string {
	var fields []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"category", d.Category != nil}, {"title", d.Title != nil}, {"content", d.Content != nil},
		{"tags", d.Tags != nil}, {"metadata", d.Metadata != nil}, {"scope", d.Scope != nil},
		{"sensitivity", d.Sensitivity != nil}, {"type", d.Type != nil}, {"edits", d.Edits != nil},
		{"metadata_merge", d.MetadataMerge != nil},
	} {
		if f.set {
			fields = append(fields, f.name)
		}
	}
	return fields
}

// parseStagedBase reads the BaseUpdatedAt a staged update binds.
func parseStagedBase(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// HandleStoreBatch serves POST /api/store/batch (issue #211): the items of the
// body written all or none through runStoreBatch, the core the MCP store_batch
// tool calls. Like /api/store it is a direct write path for every key, a
// confirm_writes key included (D-E1: the stage-then-confirm dance lives on the
// LLM surfaces). The answer is 200 with one entry per item, each the
// /api/store answer for a store item, the manage-update answer for an update
// item, without its "success" — or one refusal for the whole batch.
func (h *StoreHandler) HandleStoreBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	ar := AuthResultFromContext(ctx)
	if ar == nil || !ar.IsValid {
		writeJSONReject(w, classUnauthorized.reject("unauthorized"))
		return
	}
	var req storeBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// A body over the route's 1 MB cap is refused as too large, not as
		// malformed (the /api/ingest answer, #323): a batch reaches the cap
		// far more easily than one block does.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeHouseTooLarge(w, r, tooLarge.Limit)
			return
		}
		slog.Warn("store batch: invalid request body", "error", err, "request_id", reqID)
		writeJSONReject(w, classInvalidBody.reject("Invalid request body"))
		return
	}
	items := make([]storeBatchEntry, len(req.Items))
	for i, raw := range req.Items {
		e, err := decodeStoreBatchItem(raw)
		if err != nil {
			slog.Warn("store batch: invalid request body", "error", err, "item", i, "request_id", reqID)
			writeJSONReject(w, classInvalidBody.reject("Invalid request body"))
			return
		}
		items[i] = e
	}

	snap := h.cfg.SnapshotForRequest(ctx)
	results, rej := runStoreBatch(ctx, h.pool, h.classifySet(ctx), ar, items, storeBatchLimits{
		DefaultSens:    snap.Pool.DefaultBlockSensitivity,
		RateLimitWrite: snap.Query.RateLimitWrite,
		MaxItems:       snap.Writes.BatchMaxItems,
	}, reqID)
	if rej != nil {
		h.rejectLog.record(ctx, ar.ApiKeyID, "", "", rej)
		writeJSONReject(w, rej)
		return
	}
	answers := make([]map[string]any, len(results))
	for i, it := range results {
		answers[i] = storeBatchItemAnswer(it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "items": answers})
}

// storeBatchItemAnswer is one item of the REST answer: the keys /api/store
// answers for the same write (manage update's for an update item), without
// "success".
func storeBatchItemAnswer(it storeBatchItem) map[string]any {
	if it.Block == nil {
		return map[string]any{"action": "noop", "reason": "identical_content", "existing_id": it.UnchangedID}
	}
	if it.Update {
		return map[string]any{"action": "update", "block": it.Block}
	}
	answer := map[string]any{"block": it.Block}
	if it.Supersedes != "" {
		answer["supersedes"] = it.Supersedes
	}
	if it.Changed != nil {
		answer["action"] = "updated"
		answer["reason"] = "identical_content"
		answer["changed"] = it.Changed
	}
	if it.SensWarning != "" {
		answer["warnings"] = []string{it.SensWarning}
	}
	return answer
}

// finishStoredBlock is the afterwork of one written block, the single store's:
// classify and temporal enrichment, after the commit and non-fatal, as on
// /api/store and the store tool. The write booking is the caller's: the direct
// path books per block, a confirmed batch paid at stage time.
func finishStoredBlock(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, b *store.Block, reqID string) {
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, b.ID, b.Title, b.Metadata); err != nil {
		slog.Warn("store batch: auto-classify failed", "error", err, "block_id", b.ID, "request_id", reqID)
	}
	if _, err := store.RefreshBlockTemporal(ctx, pool, b); err != nil {
		logRequestError(ctx, "store batch: temporal refresh failed", "error", err, "block_id", b.ID, "request_id", reqID)
	}
}

// batchWrote reports whether a batch wrote a block: a batch whose items all
// matched what is stored committed nothing (the write tool's line says none).
func batchWrote(items []storeBatchItem) bool {
	for _, it := range items {
		if it.Block != nil {
			return true
		}
	}
	return false
}
