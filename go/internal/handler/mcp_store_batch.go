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
	"written in one transaction, and a refusal of any item (or of the batch) writes nothing and names the " +
	"item as items[i]. Each item takes the store tool's arguments and upserts on (category, title, scope). " +
	"At most writes.batch_max_items items (default 100); each written item counts as one write of the key's " +
	"write budget. Example: {\"items\": [{\"category\": \"decisions\", \"title\": \"Cache layer\", \"content\": " +
	"\"We cache …\"}, {\"category\": \"decisions\", \"title\": \"Decision index\", \"content\": \"- Cache layer\"}]}"

func registerStoreBatchTool(server *mcp.Server, cfg MCPConfig) {
	mcp.AddTool(server, &mcp.Tool{
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
// would refuse on today's rows: one declaring item as a staged store checks
// it (store.CheckSupersedesDeclarable, no write); two or more through a
// rolled-back run of the whole batch (store.CheckUpsertBlocks), because two
// declarations can refuse each other — one target named twice, two blocks
// declaring each other — which a check of one at a time does not see. nil =
// admissible; otherwise the tool's answer.
func stageBatchSupersedesCheck(ctx context.Context, cfg MCPConfig, set *blocktype.Set, ar *auth.AuthResult,
	plan *storeBatchPlan, reqID string,
) *mcp.CallToolResult {
	var declaring []int
	for _, i := range plan.writes {
		if plan.items[i].Supersedes != "" {
			declaring = append(declaring, i)
		}
	}
	var err error
	switch len(declaring) {
	case 0:
		return nil
	case 1:
		i := declaring[0]
		it := plan.items[i]
		err = store.CheckSupersedesDeclarable(ctx, cfg.Pool, it.Category, it.Title, plan.gated[i].WriteScope,
			store.SupersedesDeclaration{TargetID: it.Supersedes, APIKeyID: ar.ApiKeyID, Types: set})
		if rej := supersedesReject(err); rej != nil {
			return errResultReject(rej.prefixed(batchItemPrefix(i)))
		}
	default:
		err = store.CheckUpsertBlocks(ctx, cfg.Pool, plan.upserts(), ar.ApiKeyID, set)
		if rej := storeBatchRefusal(err, plan.writes); rej != nil {
			return errResultReject(rej)
		}
	}
	if err != nil {
		slog.Error("mcp: stage batch supersedes check failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: cannot check supersedes")
	}
	return nil
}

// renderConfirmedStoreBatch is the confirm tool's answer to a staged batch:
// the card's items in their order, in the confirm tool's wording.
func renderConfirmedStoreBatch(blocks []*store.Block) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Confirmed and stored %d blocks in one transaction:\n", len(blocks))
	for i, b := range blocks {
		fmt.Fprintf(&sb, "%s%s (id: %s, category: %s)\n", batchItemPrefix(i), b.Title, b.ID, b.Category)
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

// mcpStageStoreBatch stages a batch for a confirm_writes key: the gates the
// direct path runs (gateStoreBatch), the supersedes declarations checked
// (stageBatchSupersedesCheck), then ONE card whose hash binds every item. The
// card holds the items that write; an unchanged item is answered at once, as
// the store tool answers it. The budget is charged at stage time, one write
// intent per staged item in the transaction that stages the card, and the
// confirm books nothing — the rule of every staged op.
func mcpStageStoreBatch(ctx context.Context, cfg MCPConfig, set *blocktype.Set, ar *auth.AuthResult,
	items []storeRequest, lim storeBatchLimits, ttl time.Duration, reqID string,
) (*mcp.CallToolResult, any, error) {
	plan, rej := gateStoreBatch(ctx, cfg.Pool, set, ar, items, lim, reqID)
	if rej != nil {
		return errResultReject(rej), nil, nil
	}
	if len(plan.writes) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{textContent(renderStoreBatch(plan.out))}}, nil, nil
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
		if err := store.ReserveWriteIntents(ctx, tx, budget, len(plan.writes)); err != nil {
			return err
		}
		var err error
		pw, err = store.StagePendingWrite(ctx, tx, ar.ApiKeyID, cw.Scope, store.OpStoreBatch, "mcp", canonical, hash, ttl)
		return err
	})
	if rej := storeBatchBudgetReject(err); rej != nil {
		return errResultReject(rej), nil, nil
	}
	if err != nil {
		slog.Error("mcp: stage pending batch failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: could not persist the staged write"), nil, nil
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"+
		"%d of %d items staged as one batch:", len(plan.writes), len(items))
	for _, i := range plan.writes {
		sb.WriteString(" " + strings.TrimSuffix(batchItemPrefix(i), ": "))
	}
	sb.WriteString("\n")
	for i, it := range plan.out {
		if it.UnchangedID != "" {
			sb.WriteString(batchItemPrefix(i) + storeBatchItemLine(it) + " — not part of the batch\n")
		}
	}
	fmt.Fprintf(&sb, "payload_hash: %s\nexpires: %s\n"+
		"To execute this exact batch, call the 'confirm' tool with this payload_hash: every staged item is "+
		"written in one transaction, or none is. The server holds the authoritative payload; confirming cannot alter it.",
		hash, expiry)
	// IsError=true and uncoded, as every STAGED answer (D3-C3, Gap-C6-c).
	return errResult(sb.String()), nil, nil
}
