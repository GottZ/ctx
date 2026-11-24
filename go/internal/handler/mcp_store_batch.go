// The MCP store_batch tool (issue #211): several store writes, all or none.
// The arguments of each item are the store tool's own; the batch runs through
// the core REST POST /api/store/batch runs (store_batch.go). A confirm_writes
// key gets the whole batch STAGED as one card — one payload_hash over every
// item — and `confirm` executes it in one transaction.
package handler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

type storeBatchInput struct {
	Items []storeBatchItemInput `json:"items" jsonschema:"the writes, all in one transaction or none: a store item (category, title, content required) or an update item (id plus the fields to change)"`
}

// storeBatchItemInput is one item of the tool: the store tool's arguments (a
// store item) or, with an id, the update tool's (an update item, issue #211).
// One object for both, the form told by `id` (storeBatchEntry), so a batch
// written before update items existed is read as it always was. category,
// title and content are therefore not required by the schema; a store item
// without them is refused by the store chain, named by its index.
type storeBatchItemInput struct {
	ID            string            `json:"id,omitempty" jsonschema:"update item: the block id (or unambiguous prefix); omit for a store item"`
	Category      *string           `json:"category,omitempty" jsonschema:"block category"`
	Title         *string           `json:"title,omitempty" jsonschema:"block title (a store item upserts on category+title+scope)"`
	Content       *string           `json:"content,omitempty" jsonschema:"block content (max 50KB)"`
	Tags          []string          `json:"tags,omitempty" jsonschema:"tags (an update item replaces the set; [] clears)"`
	Metadata      map[string]any    `json:"metadata,omitempty" jsonschema:"metadata object; put the source of the knowledge into metadata.source"`
	Sensitivity   string            `json:"sensitivity,omitempty" jsonschema:"store item: credentials|personal|internal|public"`
	Type          string            `json:"type,omitempty" jsonschema:"explicit block type name from the registry"`
	Scope         string            `json:"scope,omitempty" jsonschema:"store item: target scope (default: the key's home scope)"`
	Supersedes    string            `json:"supersedes,omitempty" jsonschema:"store item: id of a block in the same scope this block replaces"`
	Edits         []updateEditInput `json:"edits,omitempty" jsonschema:"update item: text edits of the content instead of a whole content, all or none"`
	MetadataMerge map[string]any    `json:"metadata_merge,omitempty" jsonschema:"update item: keys merged into the stored metadata (null removes) instead of replacing it"`
}

// storeBatchToolDescription is the tool's description, example included: the
// one place a model reads how the call is shaped before it makes one.
const storeBatchToolDescription = "Save or update several knowledge blocks at once, all or none: every item is " +
	"written in one transaction, and a refusal of any item or of the batch writes nothing; an item's refusal " +
	"names the item as items[i]. A store item takes the store tool's arguments and upserts on (category, title, scope); " +
	"an item with an id takes the update tool's arguments and updates that block. " +
	"At most writes.batch_max_items items (default 100); each written item counts as one write of the key's " +
	"write budget. Example: {\"items\": [{\"category\": \"decisions\", \"title\": \"Cache layer\", \"content\": " +
	"\"We cache …\"}, {\"id\": \"<id of the decision index>\", \"edits\": [{\"old\": \"- Auth\", \"new\": \"- Auth\\n- Cache layer\"}]}]}"

func registerStoreBatchTool(server *mcp.Server, cfg MCPConfig) {
	addMCPTool(server, &mcp.Tool{
		Name:        "store_batch",
		Description: storeBatchToolDescription,
	}, mcpStoreBatchHandler(cfg))
}

// entry maps one item of the tool onto the batch core's form: a store item
// onto the request the write gates read (the mapping mcpStoreHandler spells out
// at its own call site), an update item onto the update tool's patch
// (updateFieldsOf). A field the item's form does not take is refused by name:
// the update tool carries no sensitivity, scope or supersedes, a store item no
// edits or metadata_merge.
func (in storeBatchItemInput) entry() storeBatchEntry {
	if in.ID == "" {
		e := storeBatchEntry{Store: &storeRequest{
			Category:    strOrEmpty(in.Category),
			Title:       strOrEmpty(in.Title),
			Content:     strOrEmpty(in.Content),
			Tags:        in.Tags,
			Metadata:    in.Metadata,
			Sensitivity: in.Sensitivity,
			Type:        in.Type,
			Scope:       in.Scope,
			Supersedes:  in.Supersedes,
		}}
		if in.Edits != nil || in.MetadataMerge != nil {
			e.Misplaced = storeItemUpdateFieldsMsg
		}
		return e
	}
	data, fields := updateFieldsOf(updateInput{ID: in.ID, Category: in.Category, Title: in.Title, Content: in.Content,
		Tags: in.Tags, Metadata: in.Metadata, Type: in.Type, Edits: in.Edits, MetadataMerge: in.MetadataMerge})
	e := storeBatchEntry{Update: &storeBatchUpdate{ID: in.ID, Data: data, Fields: fields}}
	if in.Sensitivity != "" || in.Scope != "" || in.Supersedes != "" {
		e.Misplaced = mcpUpdateItemFieldsMsg
	}
	return e
}

func mcpStoreBatchHandler(cfg MCPConfig) mcp.ToolHandlerFor[storeBatchInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input storeBatchInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return classUnauthorized.errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		var lim storeBatchLimits
		var ttl time.Duration
		if cfg.Cfg != nil {
			snap := cfg.Cfg.SnapshotForRequest(ctx)
			lim = storeBatchLimits{
				DefaultSens:    snap.Pool.DefaultBlockSensitivity,
				RateLimitWrite: snap.Query.RateLimitWrite,
				MaxItems:       snap.Writes.BatchMaxItems,
			}
			ttl = snap.Writes.ConfirmTTL
		}
		set := cfg.mcpTypeSnapshot(ctx)
		items := make([]storeBatchEntry, len(input.Items))
		for i, it := range input.Items {
			items[i] = it.entry()
		}
		reqID := RequestIDFromContext(ctx)
		noteToolCount(ctx, len(input.Items))

		if ar.ConfirmWrites {
			return mcpStageStoreBatch(ctx, cfg, set, ar, items, lim, ttl, reqID)
		}
		results, rej := runStoreBatch(ctx, cfg.Pool, set, ar, items, lim, reqID)
		if rej != nil {
			return errResultReject(rej), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{textContent(renderStoreBatch(results))}}, nil, nil
	}
}

// renderStoreBatch is the tool's answer: a summary line, then one line per
// item in the store tool's own wording.
func renderStoreBatch(results []storeBatchItem) string {
	stored, updated, updates := 0, 0, false
	for _, it := range results {
		updates = updates || it.Update
		switch {
		case it.Block == nil:
		case it.Update:
			updated++
		default:
			stored++
		}
	}
	var sb strings.Builder
	if updates {
		fmt.Fprintf(&sb, "Batch written in one transaction: %d stored, %d updated, %d unchanged.\n",
			stored, updated, len(results)-stored-updated)
	} else {
		fmt.Fprintf(&sb, "Batch written in one transaction: %d stored, %d unchanged.\n", stored, len(results)-stored)
	}
	for i, it := range results {
		sb.WriteString(batchItemPrefix(i))
		sb.WriteString(storeBatchItemLine(it))
		sb.WriteString("\n")
		if it.SensWarning != "" {
			sb.WriteString("    warning: " + it.SensWarning + "\n")
		}
	}
	return sb.String()
}

// stageBatchSupersedesCheck checks a batch before it is staged, so a card does
// not promise what its confirm would refuse on today's rows: a batch of one
// item that declares a supersedes is checked as a staged store
// (store.CheckSupersedesDeclarable, no write); any other batch with a
// declaration or an update item through a rolled-back run of the whole batch
// (store.CheckUpsertBlocks), because its items can refuse each other — one
// target named twice, two blocks declaring each other, an item written before
// the declaring one that retypes its target (review of #389), a store item and
// an update item on one block, a chain of renames, an edit that no longer
// matches (review of #211) — which a check of one item at a time does not see.
// nil = admissible; otherwise the tool's answer.
func stageBatchSupersedesCheck(ctx context.Context, cfg MCPConfig, set *blocktype.Set, ar *auth.AuthResult,
	plan *storeBatchPlan, reqID string,
) *mcp.CallToolResult {
	declares := slices.ContainsFunc(plan.items, func(e storeBatchEntry) bool { return e.Store != nil && e.Store.Supersedes != "" })
	updates := slices.ContainsFunc(plan.items, func(e storeBatchEntry) bool { return e.Update != nil })
	var err error
	switch {
	case !declares && !updates:
		return nil
	case len(plan.items) == 1 && declares:
		it := plan.items[0].Store
		err = store.CheckSupersedesDeclarable(ctx, cfg.Pool, it.Category, it.Title, plan.gated[0].WriteScope,
			store.SupersedesDeclaration{TargetID: it.Supersedes, APIKeyID: ar.ApiKeyID, Types: set})
		if rej := supersedesReject(err); rej != nil {
			return errResultReject(rej.prefixed(batchItemPrefix(0)))
		}
	default:
		err = store.CheckUpsertBlocks(ctx, cfg.Pool, plan.upserts(), ar.ApiKeyID, set)
		if rej := storeBatchRefusal(err); rej != nil {
			return errResultReject(rej)
		}
		if rej := writeConflictReject(err); rej != nil {
			return errResultReject(rej) // the stage check lost to concurrent writes (issue #547)
		}
	}
	if err != nil {
		logRequestError(ctx, "mcp: stage batch supersedes check failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: cannot check supersedes")
	}
	return nil
}

// renderConfirmedStoreBatch is the confirm tool's answer to a staged batch:
// the card's items in their order, in the confirm tool's wording — an item
// whose block already held what it asks for in the store tool's.
func renderConfirmedStoreBatch(items []storeBatchItem) string {
	written := 0
	for _, it := range items {
		if it.Block != nil {
			written++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Confirmed and stored %d blocks in one transaction", written)
	if unchanged := len(items) - written; unchanged > 0 {
		fmt.Fprintf(&sb, ", %d unchanged", unchanged)
	}
	sb.WriteString(":\n")
	for i, it := range items {
		if b := it.Block; b != nil {
			fmt.Fprintf(&sb, "%s%s (id: %s, category: %s)\n", batchItemPrefix(i), b.Title, b.ID, b.Category)
			continue
		}
		sb.WriteString(batchItemPrefix(i) + storeBatchItemLine(it) + "\n")
	}
	return sb.String()
}

// storeBatchItemLine is one item in the store tool's wording.
func storeBatchItemLine(it storeBatchItem) string {
	if it.Block == nil {
		return fmt.Sprintf("No change needed — identical content already exists (id: %s)", it.UnchangedID)
	}
	if it.Update {
		return fmt.Sprintf("Updated: %s (id: %s, category: %s)", it.Block.Title, it.Block.ID, it.Block.Category)
	}
	return fmt.Sprintf("Stored: %s (id: %s, category: %s%s)", it.Block.Title, it.Block.ID, it.Block.Category,
		supersedesSuffix(it.Supersedes))
}

// stageBatchUnchanged is the stage-time look at each item's block (the store
// tool's identical-content check, IdenticalContentMatch, on the values the
// gates decided): per item the id of the block that already holds what the
// item asks for, "" for an item that would write — and a declaring item
// always would. Every item is read in ONE read-only transaction on one
// snapshot (REPEATABLE READ), so the items it finds unchanged were unchanged
// at the same moment: read one by one, each read on a snapshot of its own, a
// batch committed between two reads could make an earlier item match before
// it and a later one after it, and the all-unchanged answer would name a
// state the store never held (review of #389). Nothing is locked — the single
// store's stage reads its block the same way, at one moment: the reads decide
// only whether a card is staged at all and what the stage answer says; the
// confirm decides each item again on its locked row.
func stageBatchUnchanged(ctx context.Context, cfg MCPConfig, plan *storeBatchPlan, reqID string) ([]string, *writeReject) {
	ids := make([]string, len(plan.items))
	err := pgxdb.WriteOpts(ctx, cfg.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		pgxdb.At("mcp: stage batch identical-content check"), func(tx pgx.Tx) error {
			for i, e := range plan.items {
				it := e.Store
				if it == nil || it.Supersedes != "" {
					// An update item, and a declaring item, would write.
					continue
				}
				res := plan.gated[i]
				match, err := store.IdenticalContentMatch(ctx, tx, it.Content, res.WriteScope, it.Category, it.Title,
					it.Tags, res.Metadata, res.Sens, it.Type)
				if err != nil {
					return fmt.Errorf("items[%d]: %w", i, err)
				}
				if match.Unchanged() {
					ids[i] = match.ID
				}
			}
			return nil
		})
	if err != nil {
		logRequestError(ctx, "mcp: stage batch hash noop check error", "error", err, "request_id", reqID)
		return nil, classInternal.reject("Internal server error")
	}
	return ids, nil
}

// stageBatchUpdateCards fills the card form of each update item of a plan
// (cards; the store items' are filled), as the update tool's stage builds its
// card (mcpStageUpdate): the block read within the key's writable scopes, its
// updated_at bound as the drift pin, a derivative refused (no card for a write
// the confirm cannot execute, D1-M2), edits resolved against the content the
// pin names and bound as that content, a metadata_merge kept a merge. A
// refusal names the item.
func stageBatchUpdateCards(ctx context.Context, cfg MCPConfig, plan *storeBatchPlan, cards []store.CanonicalWrite, reqID string) *writeReject {
	for i, e := range plan.items {
		u := e.Update
		if u == nil {
			continue
		}
		id := plan.resolved[i]
		// nil type set (V-11): a TOCTOU fingerprint, not a read answer.
		block, err := store.GetBlock(ctx, cfg.Pool, nil, id, plan.writable, nil)
		if err != nil {
			logRequestError(ctx, "mcp: stage batch update base read failed", "error", err, "block_id", id, "request_id", reqID)
			return classInternal.reject("stage failed: internal error")
		}
		if block == nil {
			return classUnknownBlock.reject(updateNotFoundMsg).prefixed(batchItemPrefix(i))
		}
		if rej := derivativeTargetReject(block); rej != nil {
			return rej.prefixed(batchItemPrefix(i))
		}
		content, fields := strOrEmpty(u.Data.Content), u.Fields
		if u.Data.Edits != nil {
			edited, err := store.ApplyContentEdits(block.Content, u.Data.Edits)
			if rej := contentEditReject(err); rej != nil {
				return rej.prefixed(batchItemPrefix(i))
			}
			if err != nil {
				slog.Error("mcp: stage batch update edits failed", "error", err, "block_id", id, "request_id", reqID)
				return classInternal.reject("stage failed: internal error")
			}
			content, fields = edited, stagedEditFields(fields)
		}
		cards[i] = store.CanonicalWrite{
			Op:            "update",
			ID:            id,
			Scope:         block.Scope,
			Category:      strOrEmpty(u.Data.Category),
			Title:         strOrEmpty(u.Data.Title),
			Content:       content,
			Tags:          u.Data.Tags,
			Metadata:      u.Data.Metadata,
			Type:          strOrEmpty(u.Data.Type),
			UpdateFields:  fields,
			BaseUpdatedAt: block.UpdatedAt.UTC().Format(time.RFC3339Nano),
			MetadataMerge: u.Data.MetadataMerge,
		}
	}
	return nil
}

// mcpStageStoreBatch stages a batch for a confirm_writes key: the gates the
// direct path runs (gateStoreBatch), the supersedes declarations checked
// (stageBatchSupersedesCheck), then ONE card whose hash binds every item. A
// batch of which every item would change nothing right now stages nothing and
// costs nothing; it is answered at once, as the direct path answers it.
// Otherwise the card holds EVERY item, the ones unchanged at stage time
// included: a card without them would let the confirm write the rest of the
// batch around a block another writer changed in between, so the confirm
// decides each item on its locked row (executeConfirmedStoreBatch). The
// budget is charged at stage time, one write intent per item of the card, in
// the transaction that stages it, and the confirm books nothing — the rule of
// every staged op; an item the confirm then finds unchanged was charged
// without being written.
func mcpStageStoreBatch(ctx context.Context, cfg MCPConfig, set *blocktype.Set, ar *auth.AuthResult,
	items []storeBatchEntry, lim storeBatchLimits, ttl time.Duration, reqID string,
) (*mcp.CallToolResult, any, error) {
	plan, rej := gateStoreBatch(ctx, cfg.Pool, set, ar, items, lim, reqID)
	if rej != nil {
		return errResultReject(rej), nil, nil
	}
	cardItems := storeBatchCanonicalItems(plan)
	if rej := stageBatchUpdateCards(ctx, cfg, plan, cardItems, reqID); rej != nil {
		return errResultReject(rej), nil, nil
	}
	unchanged, rej := stageBatchUnchanged(ctx, cfg, plan, reqID)
	if rej != nil {
		return errResultReject(rej), nil, nil
	}
	if !slices.Contains(unchanged, "") {
		out := make([]storeBatchItem, len(items))
		for i, id := range unchanged {
			out[i].UnchangedID = id
		}
		return &mcp.CallToolResult{Content: []mcp.Content{textContent(renderStoreBatch(out))}}, nil, nil
	}
	if res := stageBatchSupersedesCheck(ctx, cfg, set, ar, plan, reqID); res != nil {
		return res, nil, nil
	}

	scopes := make([]string, len(cardItems))
	for k, it := range cardItems {
		scopes[k] = it.Scope
	}
	// The row binds one scope: the first of the items' scopes (store.OpStoreBatch);
	// the confirm re-validates each item's own.
	cw := store.CanonicalWrite{Op: store.OpStoreBatch, Scope: slices.Min(scopes), Items: cardItems}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged batch failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: cannot canonicalize payload"), nil, nil
	}
	// The card and its charge commit together, under the key's budget lock:
	// one write intent per staged item, refused whole when they do not fit.
	var pw *store.PendingWrite
	err = pgxdb.Write(ctx, cfg.Pool, pgxdb.At("mcp: stage block batch"), func(tx pgx.Tx) error {
		budget := store.WriteBudget{APIKeyID: ar.ApiKeyID, Limit: lim.RateLimitWrite}
		if err := store.ReserveWriteIntents(ctx, tx, budget, len(cardItems)); err != nil {
			return err
		}
		var err error
		pw, err = store.StagePendingWrite(ctx, tx, ar.ApiKeyID, cw.Scope, store.OpStoreBatch, "mcp", canonical, hash, ttl)
		return err
	})
	if rej := storeBatchBudgetReject(err); rej != nil {
		return errResultReject(rej), nil, nil
	}
	noteToolStaged(ctx, err, true)
	if err != nil {
		logRequestError(ctx, "mcp: stage pending batch failed", "error", err, "write_outcome", writeOutcome(err), "request_id", reqID)
		return classInternal.errResult("stage failed: could not persist the staged write"), nil, nil
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"+
		"%d items staged as one batch.\n", len(cardItems))
	for i, id := range unchanged {
		if id != "" {
			fmt.Fprintf(&sb, "%sidentical content exists now (id: %s) — the confirm leaves it as it is if it still is\n",
				batchItemPrefix(i), id)
		}
	}
	fmt.Fprintf(&sb, "payload_hash: %s\nexpires: %s\n"+
		"To execute this exact batch, call the 'confirm' tool with this payload_hash: every staged item is "+
		"written in one transaction, or none is. The server holds the authoritative payload; confirming cannot alter it.",
		hash, expiry)
	// IsError=true and uncoded, as every STAGED answer (D3-C3, Gap-C6-c).
	return errResult(sb.String()), nil, nil
}
