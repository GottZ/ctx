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
	Items []storeInput `json:"items" jsonschema:"the blocks to write, each with the arguments of the store tool (category, title, content required); all of them are written in one transaction, or none is"`
}

// storeBatchToolDescription is the tool's description, example included: the
// one place a model reads how the call is shaped before it makes one.
const storeBatchToolDescription = "Save or update several knowledge blocks at once, all or none: every item is " +
	"written in one transaction, and a refusal of any item or of the batch writes nothing; an item's refusal " +
	"names the item as items[i]. Each item takes the store tool's arguments and upserts on (category, title, scope). " +
	"At most writes.batch_max_items items (default 100); each written item counts as one write of the key's " +
	"write budget. Example: {\"items\": [{\"category\": \"decisions\", \"title\": \"Cache layer\", \"content\": " +
	"\"We cache …\"}, {\"category\": \"decisions\", \"title\": \"Decision index\", \"content\": \"- Cache layer\"}]}"

func registerStoreBatchTool(server *mcp.Server, cfg MCPConfig) {
	addMCPTool(server, &mcp.Tool{
		Name:        "store_batch",
		Description: storeBatchToolDescription,
	}, mcpStoreBatchHandler(cfg))
}

// storeRequest maps the store tool's arguments onto the request the write
// gates read (the mapping mcpStoreHandler spells out at its own call site).
func (in storeInput) storeRequest() storeRequest {
	return storeRequest{
		Category:    in.Category,
		Title:       in.Title,
		Content:     in.Content,
		Tags:        in.Tags,
		Metadata:    in.Metadata,
		Sensitivity: in.Sensitivity,
		Type:        in.Type,
		Scope:       in.Scope,
		Supersedes:  in.Supersedes,
	}
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
		items := make([]storeRequest, len(input.Items))
		for i, it := range input.Items {
			items[i] = it.storeRequest()
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
	written := 0
	for _, it := range results {
		if it.Block != nil {
			written++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Batch written in one transaction: %d stored, %d unchanged.\n", written, len(results)-written)
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

// stageBatchSupersedesCheck checks the supersedes declarations of a batch
// before it is staged, so a card does not promise a declaration its confirm
// would refuse on today's rows: a batch of one item that declares is checked
// as a staged store (store.CheckSupersedesDeclarable, no write); any other
// batch with a declaration through a rolled-back run of the whole batch
// (store.CheckUpsertBlocks), because its items can refuse a declaration
// together — one target named twice, two blocks declaring each other, an item
// written before the declaring one that retypes its target (review of #389)
// — which a check of one declaration against today's rows does not see. nil =
// admissible; otherwise the tool's answer.
func stageBatchSupersedesCheck(ctx context.Context, cfg MCPConfig, set *blocktype.Set, ar *auth.AuthResult,
	plan *storeBatchPlan, reqID string,
) *mcp.CallToolResult {
	declares := slices.ContainsFunc(plan.items, func(it storeRequest) bool { return it.Supersedes != "" })
	var err error
	switch {
	case !declares:
		return nil
	case len(plan.items) == 1:
		it := plan.items[0]
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
			for i, it := range plan.items {
				if it.Supersedes != "" {
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
	items []storeRequest, lim storeBatchLimits, ttl time.Duration, reqID string,
) (*mcp.CallToolResult, any, error) {
	plan, rej := gateStoreBatch(ctx, cfg.Pool, set, ar, items, lim, reqID)
	if rej != nil {
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

	cardItems := storeBatchCanonicalItems(plan)
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
