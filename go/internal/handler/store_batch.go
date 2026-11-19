// All-or-none block batches (issue #211): the core the MCP `store_batch` tool
// and REST POST /api/store/batch share. Two surfaces, one function: each item
// runs the write gate chain of a single store (runStageWriteGates — the chain
// of /api/store and both arms of the MCP `store` tool), the batch runs its own
// gates over the items as a whole, and the writes land through
// store.UpsertBlocks in ONE transaction. Any refusal — of one item or of the
// batch — refuses the batch before or inside that transaction, names the item
// and the reason, and leaves nothing written: no block row, no write-log row,
// no budget spent.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

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

// storeBatchItem is one item's outcome. Block is the written block, nil for an
// item that changes nothing (UnchangedID names the block it already matches,
// the single store's identical-content no-op). Changed lists what an
// identical-content write rewrites (the /api/store "changed"), Supersedes the
// block it declared to supersede.
type storeBatchItem struct {
	Block       *store.Block
	UnchangedID string
	Changed     []string
	Supersedes  string
	// SensWarning: an explicit sensitivity the upgrade-only upsert did not
	// apply (the /api/store "warnings" entry), "" otherwise.
	SensWarning string
}

// batchItemPrefix names an item in a refusal, as /api/ingest names a chunk.
func batchItemPrefix(i int) string { return fmt.Sprintf("items[%d]: ", i) }

// storeBatchParentMsg refuses parent_id on a batch item: the field hangs a
// block under a parent in a second step after the upsert on /api/store, and a
// batch promises one transaction for everything it writes.
const storeBatchParentMsg = "parent_id is not part of a batch — write a block with a parent through POST /api/store"

// storeBatchPlan is a batch that passed its gates: the items as sent and what
// the gates decided for each (resolved scope, post-detector sensitivity and
// metadata).
type storeBatchPlan struct {
	items []storeRequest
	gated []*stageWriteGateResult
}

// upserts is the store's form of the items, in their order.
func (p *storeBatchPlan) upserts() []store.BatchUpsert {
	batch := make([]store.BatchUpsert, len(p.items))
	for i, it := range p.items {
		res := p.gated[i]
		batch[i] = store.BatchUpsert{
			Category: it.Category, Title: it.Title, Content: it.Content, Tags: it.Tags,
			Metadata: res.Metadata, Scope: res.WriteScope, ScopeExplicit: res.ScopeExplicit,
			Sens: res.Sens, TypeName: it.Type, Supersedes: it.Supersedes,
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
	items []storeRequest, lim storeBatchLimits, reqID string,
) ([]storeBatchItem, *writeReject) {
	plan, rej := gateStoreBatch(ctx, pool, set, ar, items, lim, reqID)
	if rej != nil {
		return nil, rej
	}
	results, err := store.UpsertBlocks(ctx, pool, plan.upserts(), ar.ApiKeyID, set,
		&store.WriteBudget{APIKeyID: ar.ApiKeyID, Limit: lim.RateLimitWrite})
	if err != nil {
		return nil, storeBatchFailureReject(err, reqID)
	}
	out := storeBatchOutcome(results)
	// The blocks and their booking are committed: the afterwork must not stop
	// halfway because the client went away.
	after := context.WithoutCancel(ctx)
	for i, r := range results {
		b := r.Block
		if b == nil {
			continue
		}
		out[i].Supersedes = strings.ToLower(items[i].Supersedes)
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
func storeBatchOutcome(results []store.BatchResult) []storeBatchItem {
	out := make([]storeBatchItem, len(results))
	for i, r := range results {
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
func storeBatchFailureReject(err error, reqID string) *writeReject {
	if rej := storeBatchBudgetReject(err); rej != nil {
		return rej
	}
	if rej := storeBatchRefusal(err); rej != nil {
		return rej
	}
	slog.Error("store batch: upsert error", "error", err, "request_id", reqID)
	return classInternal.reject("Internal server error")
}

// storeBatchRefusal is the refusal of one item inside the transaction, as the
// single store answers it — provenance_protected, the re-type of a comment or
// an issue (constraint), the supersedes classes — named by its position in
// the caller's list. nil when err is no such refusal (a fault).
func storeBatchRefusal(err error) *writeReject {
	var itemErr *store.BatchItemError
	if !errors.As(err, &itemErr) {
		return nil
	}
	rej := provenanceRejectOr(err, retypeBoundReject(err))
	if rej == nil {
		rej = supersedesReject(err)
	}
	if rej == nil {
		return nil
	}
	return rej.prefixed(batchItemPrefix(itemErr.Index))
}

// gateStoreBatch runs every gate of a batch, in this order, each before
// anything is written or booked:
//
//  1. the batch shape — no item, more items than writes.batch_max_items;
//  2. each item in list order: parent_id, then the single store's gate chain
//     (required fields, size caps, sensitivity, the I7 claim gates and the type
//     registry, the credentials detector, the write scope) — the first
//     refusing item answers, with its own class and text behind "items[i]: ";
//  3. two items with one (category, title, scope) identity — a batch writes
//     each block once (constraint).
//
// Whether an item would change nothing (the identical-content no-op of
// /api/store) is not decided here: store.UpsertBlocks decides it on the item's
// row while its transaction holds that row, so no other write can come
// between the decision and the batch's commit. The write budget — one write
// per item that writes, refused whole when they do not fit into what is left
// of query.rate_limit_write in the current window — is checked in the same
// transaction under the key's budget lock, after those decisions and before
// the first write (a staged batch: in the transaction that stages it,
// mcpStageStoreBatch). What the store decides on the row as it is —
// provenance_protected, the re-type of a comment or an issue, the supersedes
// refusals — is decided in the transaction too, which then rolls back and
// names the item.
func gateStoreBatch(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, ar *auth.AuthResult,
	items []storeRequest, lim storeBatchLimits, reqID string,
) (*storeBatchPlan, *writeReject) {
	if rej := storeBatchShapeReject(len(items), lim.MaxItems); rej != nil {
		return nil, rej
	}
	gated := make([]*stageWriteGateResult, len(items))
	for i, it := range items {
		if it.ParentID != "" {
			return nil, classInvalidBody.reject(batchItemPrefix(i) + storeBatchParentMsg)
		}
		// The rate limit is checked once for the whole batch, in its
		// transaction: rateLimitWrite 0 here skips the chain's per-call check.
		res, rej := runStageWriteGates(ctx, pool, set, ar, it, lim.DefaultSens, 0, reqID)
		if rej != nil {
			return nil, rej.prefixed(batchItemPrefix(i))
		}
		gated[i] = res
	}
	if rej := storeBatchDuplicateReject(items, gated); rej != nil {
		return nil, rej
	}
	return &storeBatchPlan{items: items, gated: gated}, nil
}

// storeBatchCanonicalItems is the staged form of a plan: each item as a staged
// op 'store' binds it — the resolved scope, the post-detector metadata and
// sensitivity — so the confirm writes exactly what the gates decided.
func storeBatchCanonicalItems(p *storeBatchPlan) []store.CanonicalWrite {
	items := make([]store.CanonicalWrite, len(p.items))
	for i, it := range p.items {
		res := p.gated[i]
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
// by item: the item's scope against the key's CURRENT writable scopes (D1-M1)
// and the I7 claim gates against the registry of now (confirmClaimReject).
// nil for every other op, and for a batch that still passes. The first
// refusing item answers; the stage token survives, as for a single card.
func confirmBatchReject(ctx context.Context, blocktypes *blocktype.Registry, ar *auth.AuthResult, cw store.CanonicalWrite) *confirmOutcome {
	if cw.Op != store.OpStoreBatch {
		return nil
	}
	if len(cw.Items) == 0 {
		// A card without items was not written by this binary.
		return &confirmOutcome{Kind: confirmMiss}
	}
	writable := writableBlockScopes(ar)
	for i, it := range cw.Items {
		if it.Op != "store" {
			return &confirmOutcome{Kind: confirmMiss}
		}
		if !contains(writable, it.Scope) {
			return &confirmOutcome{Kind: confirmScopeRejected, Op: cw.Op, Scope: it.Scope}
		}
		if rej := confirmClaimReject(ctx, blocktypes, it); rej != nil {
			return &confirmOutcome{Kind: confirmClaimRejected, Op: cw.Op, Reject: rej.prefixed(batchItemPrefix(i))}
		}
	}
	return nil
}

// executeConfirmedStoreBatch runs op 'store_batch' AFTER the consume: the
// card's items through store.UpsertBlocks, the transaction of the direct path
// — an item whose row then already holds what it asks for is left as it is,
// decided on the locked row — then each written block's afterwork. No
// booking — the batch paid for its items at stage time, as every staged op
// does. A refusal the store decides on the row as it is then (provenance, the
// re-type of a comment or an issue, a supersedes target that changed) writes
// nothing and answers behind the prefix that says the token is spent, as a
// confirmed single store does.
func executeConfirmedStoreBatch(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, ar *auth.AuthResult, cw store.CanonicalWrite, pendingID string) confirmOutcome {
	var set *blocktype.Set
	if blocktypes != nil {
		set = blocktypes.SnapshotForRequest(ctx)
	}
	batch := make([]store.BatchUpsert, len(cw.Items))
	for i, it := range cw.Items {
		// ScopeExplicit stays false for the reason executeConfirmedStore gives:
		// the conflict target carries the scope, the assignment moves nothing.
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
	}
	results, err := store.UpsertBlocks(ctx, pool, batch, ar.ApiKeyID, set, nil)
	if err != nil {
		if rej := storeBatchRefusal(err); rej != nil {
			return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op, Reject: rej.prefixed(confirmStoreBatchPrefix)}
		}
		slog.Error("confirm: confirmed batch execute failed", "error", err, "pending_id", pendingID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}
	after := context.WithoutCancel(ctx)
	for _, r := range results {
		if r.Block != nil {
			finishStoredBlock(after, pool, set, r.Block, RequestIDFromContext(ctx))
		}
	}
	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Batch: storeBatchOutcome(results)}
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
// block: the same category and title in the same resolved scope. Each would
// upsert the same row, and which one the block keeps would depend on the order
// the batch writes in.
//
// The same for two items that declare to supersede one block: a block is
// superseded by one block, so the second declaration would be refused inside
// the transaction as already superseded — naming as the head a block of this
// very batch, which the refusal rolls back.
func storeBatchDuplicateReject(items []storeRequest, gated []*stageWriteGateResult) *writeReject {
	type identity struct{ scope, category, title string }
	seen := make(map[identity]int, len(items))
	targets := make(map[string]int, len(items))
	for i, it := range items {
		id := identity{gated[i].WriteScope, it.Category, it.Title}
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

// storeBatchRequest is the body of POST /api/store/batch: the items are
// /api/store bodies.
type storeBatchRequest struct {
	Items []storeRequest `json:"items"`
}

// HandleStoreBatch serves POST /api/store/batch (issue #211): the items of the
// body written all or none through runStoreBatch, the core the MCP store_batch
// tool calls. Like /api/store it is a direct write path for every key, a
// confirm_writes key included (D-E1: the stage-then-confirm dance lives on the
// LLM surfaces). The answer is 200 with one entry per item, each the
// /api/store answer for that item without its "success" — or one refusal for
// the whole batch.
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

	snap := h.cfg.SnapshotForRequest(ctx)
	results, rej := runStoreBatch(ctx, h.pool, h.classifySet(ctx), ar, req.Items, storeBatchLimits{
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
// answers for the same write, without "success".
func storeBatchItemAnswer(it storeBatchItem) map[string]any {
	if it.Block == nil {
		return map[string]any{"action": "noop", "reason": "identical_content", "existing_id": it.UnchangedID}
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
	times := store.ExtractDates(b.Content)
	if err := store.UpdateContentTimes(ctx, pool, b.ID, times); err != nil {
		slog.Error("store batch: content_times update failed", "error", err, "block_id", b.ID, "request_id", reqID)
	}
	if err := store.PopulateTemporal(ctx, pool, b.ID, times, b.CreatedAt); err != nil {
		slog.Error("store batch: temporal populate failed", "error", err, "block_id", b.ID, "request_id", reqID)
	}
}
