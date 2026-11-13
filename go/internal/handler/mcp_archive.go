// Issue #201: the MCP archive tool. Before it, an MCP-only client could rename
// and re-categorise a block it wrote but never retract it — the block kept
// coming back in query, search and recent. Semantics are REST manage delete
// (context_manage.go handleDelete), statement for statement: resolve the id
// within writableBlockScopes with no grant arm (a block grant is read-only and
// must never widen a write path), then store.DeleteBlock, whose WHERE carries
// the provenance exclusion — a derivative answers provenance_protected here as
// it does there. No unarchive: the archived block stays readable by full id
// through `get` with include_archived (get_archived.go).
//
// A confirm_writes key gets the archive STAGED (op 'archive'), not refused like
// guard_resolve archive: the update tool's staging carries a by-id write on an
// existing block already — target id, its scope, the drift pin
// (CanonicalWrite.BaseUpdatedAt) and the confirm core's re-check — and an
// archive is that same shape with no fields. guard_resolve stays refused
// because it is a batch with per-id outcomes, which one staged card cannot pin.
package handler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type archiveInput struct {
	ID string `json:"id" jsonschema:"block UUID (or unambiguous prefix) — must live in a scope this key may write"`
}

func mcpArchiveHandler(cfg MCPConfig) mcp.ToolHandlerFor[archiveInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input archiveInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return errResult("id is required"), nil, nil
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		if ar.ConfirmWrites {
			return mcpStageArchive(ctx, cfg, ar, input)
		}

		resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
		if miss != "" {
			return errResult(miss), nil, nil
		}
		block, err := store.DeleteBlock(ctx, cfg.Pool, resolvedID, writableBlockScopes(ar))
		if err != nil {
			// I7/S3: the target is a derivative — the 403 class REST answers.
			if rej := provenanceRejectOr(err, nil); rej != nil {
				return errResultReject(rej), nil, nil
			}
			slog.Error("mcp: archive error", "error", err, "block_id", resolvedID, "request_id", RequestIDFromContext(ctx))
			return classInternal.errResult("archive failed: internal error"), nil, nil
		}
		if block == nil {
			// A full id passes the resolve unchecked: an already archived or a
			// foreign block lands here and answers what an unknown id answers.
			return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Archived: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
		}, nil, nil
	}
}

// mcpStageArchive is the stage branch for confirm_writes keys. It runs every
// check the execute would fail on BEFORE a card exists (D1-M2: a card promises
// a write the confirm can execute): the write rate limit, the resolve within the
// writable scopes, and the provenance verdict DeleteBlock would answer. The
// card binds the block's scope and its updated_at; executeConfirm re-checks
// both on the un-consumed row.
func mcpStageArchive(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, input archiveInput) (*mcp.CallToolResult, any, error) {
	reqID := RequestIDFromContext(ctx)
	var ttl time.Duration
	rateLimit := 0
	if cfg.Cfg != nil {
		snap := cfg.Cfg.SnapshotForRequest(ctx)
		ttl = snap.Writes.ConfirmTTL
		rateLimit = snap.Query.RateLimitWrite
	}
	if rateLimit > 0 {
		writeCount, err := store.CheckRateLimit(ctx, cfg.Pool, ar.ApiKeyID)
		if err != nil {
			slog.Error("mcp: stage archive rate limit check error", "error", err, "request_id", reqID)
			return classInternal.errResult("stage failed: internal error"), nil, nil
		}
		if writeCount >= rateLimit {
			return classRateLimit.errResult(fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", rateLimit)), nil, nil
		}
	}

	resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
	if miss != "" {
		return errResult(miss), nil, nil
	}
	// Read through the SAME write-eligible filter the execute uses; nil type
	// set (V-11): a fingerprint, not a read answer.
	block, err := store.GetBlock(ctx, cfg.Pool, nil, resolvedID, writableBlockScopes(ar), nil)
	if err != nil {
		slog.Error("mcp: stage archive base read failed", "error", err, "block_id", resolvedID, "request_id", reqID)
		return classInternal.errResult("stage failed: internal error"), nil, nil
	}
	if block == nil {
		return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
	}
	// The key DeleteBlock's WHERE tests (metadata ? 'provenance'); GetBlock
	// strips only guard_matched_id from metadata, so presence here is the same
	// predicate.
	if _, derivative := block.Metadata[derived.MetadataKey]; derivative {
		return errResultReject(provenanceRejectOr(store.ErrProvenanceProtected, nil)), nil, nil
	}

	cw := store.CanonicalWrite{
		Op:            store.OpArchive,
		ID:            resolvedID,
		Scope:         block.Scope,
		BaseUpdatedAt: block.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged archive failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: cannot canonicalize payload"), nil, nil
	}
	pw, err := store.StagePendingWrite(ctx, cfg.Pool, ar.ApiKeyID, block.Scope, store.OpArchive, "mcp", canonical, hash, ttl)
	if err != nil {
		slog.Error("mcp: stage pending archive failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: could not persist the staged write"), nil, nil
	}
	// Gap-C6-a: charge the write budget at INTENT, as the store and blob stage
	// arms do — executeConfirm books nothing. Logged, never fatal.
	if err := store.LogAccess(ctx, cfg.Pool, ar.ApiKeyID, "", "write"); err != nil {
		slog.Error("mcp: staged archive log error", "error", err, "request_id", reqID)
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	// IsError=true and UNCODED (D3-C3): the write has not happened, and a
	// client that cannot confirm must not read the card as success.
	return errResult(fmt.Sprintf(
		"STAGED — NOT archived yet. This key requires write confirmation (confirm_writes).\n"+
			"target block: %s (%s)\n"+
			"payload_hash: %s\n"+
			"expires: %s\n"+
			"To execute this archive, call the 'confirm' tool with this payload_hash. "+
			"The server holds the authoritative payload; confirming cannot alter it. "+
			"If the block changes before you confirm, the confirm is rejected and you must re-stage.",
		block.Title, resolvedID, hash, expiry)), nil, nil
}
