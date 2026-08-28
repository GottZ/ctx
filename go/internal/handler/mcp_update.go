// F6-C6 D-W6a: the MCP update tool. Additive — every existing tool is
// untouched (D3-m1). Semantics mirror the REST manage-update path
// (context_manage.go handleUpdate): resolve within writableBlockScopes
// (home_scope ∪ shared-if-allowed, v4.0.1), size limits, then
// store.UpdateBlock + the same re-classify/temporal/re-embed afterwork.
//
// A key WITHOUT confirm_writes updates directly; a flagged key gets the
// update STAGED (op 'update') with a TOCTOU pin (D1-M3): the block's
// updated_at at stage time is hash-bound (CanonicalWrite.BaseUpdatedAt), and
// the confirm rejects — without consuming the token — when the block changed
// in between. The update tool carries NO scope field and no sensitivity
// field: those stay REST-only, because on an EXISTING block both are guard
// flows — a scope move sweeps the link tables (GD5/K8), a sensitivity
// downgrade needs the confirm — and neither is a property of the write this
// tool performs. E-M4 (2026-08-25) gave the store and blob_store tools an
// optional `scope`, and deliberately not this one: there the field NAMES where
// a new row goes, here it would MOVE an existing one. The type axis parted
// ways in N-26 for the same shape of reason: the store tool takes an explicit
// `type` (registry-validated, manual provenance), the update tool does not —
// re-typing an existing block runs through REST manage-update.
package handler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type updateInput struct {
	ID       string         `json:"id" jsonschema:"block UUID (or unambiguous prefix) — must live in a scope this key may write"`
	Category *string        `json:"category,omitempty" jsonschema:"new category (omit = unchanged)"`
	Title    *string        `json:"title,omitempty" jsonschema:"new title (omit = unchanged)"`
	Content  *string        `json:"content,omitempty" jsonschema:"new content, max 50KB (omit = unchanged)"`
	Tags     []string       `json:"tags,omitempty" jsonschema:"replacement tag set ([] clears all tags, omit = unchanged)"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"replacement metadata object ({} clears, omit = unchanged)"`
}

// updateFieldsOf extracts the authoritative field list + UpdateBlockData from
// the tool input. The list (not value presence) is what the staged form
// hash-binds — "clear tags" and "leave tags" must never collide (D-W6a).
func updateFieldsOf(in updateInput) (store.UpdateBlockData, []string) {
	var data store.UpdateBlockData
	fields := make([]string, 0, 5)
	if in.Category != nil {
		data.Category = in.Category
		fields = append(fields, "category")
	}
	if in.Title != nil {
		data.Title = in.Title
		fields = append(fields, "title")
	}
	if in.Content != nil {
		data.Content = in.Content
		fields = append(fields, "content")
	}
	if in.Tags != nil {
		data.Tags = in.Tags
		fields = append(fields, "tags")
	}
	if in.Metadata != nil {
		data.Metadata = in.Metadata
		fields = append(fields, "metadata")
	}
	return data, fields
}

// updateDataFromCanonical rebuilds UpdateBlockData from a staged update
// payload. Driven by UpdateFields ONLY: a listed field with an empty value is
// a clear (omitempty dropped the value from the canonical JSON), an unlisted
// field stays untouched.
func updateDataFromCanonical(cw store.CanonicalWrite) store.UpdateBlockData {
	var data store.UpdateBlockData
	for _, f := range cw.UpdateFields {
		switch f {
		case "category":
			v := cw.Category
			data.Category = &v
		case "title":
			v := cw.Title
			data.Title = &v
		case "content":
			v := cw.Content
			data.Content = &v
		case "tags":
			t := cw.Tags
			if t == nil {
				t = []string{}
			}
			data.Tags = t
		case "metadata":
			m := cw.Metadata
			if m == nil {
				m = map[string]any{}
			}
			data.Metadata = m
		}
	}
	return data
}

// finishBlockUpdate runs the REST-parity afterwork of a block update
// (context_manage.go handleUpdate): re-classify on title/metadata change
// (T4: promotes auto-typed blocks only), temporal re-extraction on content
// change, embedding reset when content/title changed. All non-fatal.
// Surface-neutral since D-W6c (pool+registry, not MCPConfig): the MCP direct
// path AND the shared confirm core (confirm_core.go) call it.
func finishBlockUpdate(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, data store.UpdateBlockData, block *store.Block, needsReEmbed bool) {
	if blocktypes != nil && (data.Title != nil || data.Metadata != nil) {
		set := blocktypes.SnapshotForRequest(ctx)
		if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("update afterwork: re-classify failed", "error", err, "block_id", block.ID)
		}
	}
	if data.Content != nil {
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(ctx, pool, block.ID, times); err != nil {
			slog.Error("update afterwork: content_times update failed", "error", err, "block_id", block.ID)
		}
		if err := store.PopulateTemporal(ctx, pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("update afterwork: temporal populate failed", "error", err, "block_id", block.ID)
		}
	}
	if needsReEmbed {
		if err := store.ClearEmbedding(ctx, pool, block.ID); err != nil {
			slog.Error("update afterwork: clear embedding failed", "error", err, "block_id", block.ID)
		}
	}
}

// resolveUpdateTarget resolves the tool's id argument within the caller's
// write-eligible scopes (REST parity: ResolveBlockID over writableBlockScopes,
// grants nil — a block grant is read-only and must never widen a write path).
// Returns "" plus a user-facing message on any miss.
func resolveUpdateTarget(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, id string) (string, string) {
	resolvedID, matches, err := store.ResolveBlockID(ctx, cfg.Pool, id, writableBlockScopes(ar), nil)
	if err != nil {
		if len(matches) > 0 {
			return "", fmt.Sprintf("ambiguous id prefix (%d matches) — re-specify with a longer prefix or the full id", len(matches))
		}
		return "", fmt.Sprintf("cannot resolve block id: %v", err)
	}
	if resolvedID == "" {
		return "", "Block not found (or not in a writable scope of this key)"
	}
	return resolvedID, ""
}

func mcpUpdateHandler(cfg MCPConfig) mcp.ToolHandlerFor[updateInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input updateInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return errResult("id is required"), nil, nil
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}

		data, fields := updateFieldsOf(input)
		if len(fields) == 0 {
			return errResult("nothing to update — provide at least one of category, title, content, tags, metadata"), nil, nil
		}
		// Size limits — same gate as the direct store path (REST parity).
		if msg := blockSizeLimit(strOrEmpty(data.Category), strOrEmpty(data.Title), strOrEmpty(data.Content)); msg != "" {
			return errResult(msg), nil, nil
		}
		// I7 claim gates (design D-01 §4.3.1): MOVING a block into a reserved
		// category occupies it exactly as creating one there does, and a
		// metadata replacement can plant the provenance key just as a create
		// can. The tool carries no `type`, so that arm of claimReject is inert
		// here. Ahead of the stage branch, so a flagged key gets no card either.
		if rej := claimReject(nil, strOrEmpty(data.Category), "", data.Metadata); rej != nil {
			return errResultReject(rej), nil, nil
		}

		if ar.ConfirmWrites {
			return mcpStageUpdate(ctx, cfg, ar, input, fields)
		}

		resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
		if miss != "" {
			return errResult(miss), nil, nil
		}
		block, needsReEmbed, err := store.UpdateBlock(ctx, cfg.Pool, resolvedID, data, writableBlockScopes(ar))
		if err != nil {
			// I7/S3: the target is a derivative — 403 class, not a 500.
			if rej := provenanceRejectOr(err, nil); rej != nil {
				return errResultReject(rej), nil, nil
			}
			slog.Error("mcp: update error", "error", err, "block_id", resolvedID)
			return errResult("update failed: internal error"), nil, nil
		}
		if block == nil {
			return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
		}
		finishBlockUpdate(ctx, cfg.Pool, cfg.Blocktypes, data, block, needsReEmbed)
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Updated: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
		}, nil, nil
	}
}

// mcpStageUpdate is the D-W6a stage branch for flagged keys. Deliberately NO
// credentials detector here: the update EXECUTE path (store.UpdateBlock, REST
// parity) applies none either, and the staged card must promise exactly what
// the confirm will do (D1-M2 — gate parity between stage and execute). The
// write rate limit counts staging as write intent (same reasoning as
// runStageWriteGates).
func mcpStageUpdate(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, input updateInput, fields []string) (*mcp.CallToolResult, any, error) {
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
			slog.Error("mcp: stage update rate limit check error", "error", err)
			return errResult("stage failed: internal error"), nil, nil
		}
		if writeCount >= rateLimit {
			return errResult(fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", rateLimit)), nil, nil
		}
	}

	resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
	if miss != "" {
		return errResult(miss), nil, nil
	}
	// The TOCTOU pin reads the block through the SAME write-eligible filter
	// the execute will use — and captures updated_at as the base fingerprint.
	// nil type set (V-11): a TOCTOU fingerprint, not a read answer.
	block, err := store.GetBlock(ctx, cfg.Pool, nil, resolvedID, writableBlockScopes(ar), nil)
	if err != nil {
		slog.Error("mcp: stage update base read failed", "error", err, "block_id", resolvedID)
		return errResult("stage failed: internal error"), nil, nil
	}
	if block == nil {
		return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
	}

	cw := store.CanonicalWrite{
		Op:            "update",
		ID:            resolvedID,
		Scope:         block.Scope,
		Category:      strOrEmpty(input.Category),
		Title:         strOrEmpty(input.Title),
		Content:       strOrEmpty(input.Content),
		Tags:          input.Tags,
		Metadata:      input.Metadata,
		UpdateFields:  fields,
		BaseUpdatedAt: block.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged update failed", "error", err)
		return errResult("stage failed: cannot canonicalize payload"), nil, nil
	}
	pw, err := store.StagePendingWrite(ctx, cfg.Pool, ar.ApiKeyID, block.Scope, "update", "mcp", canonical, hash, ttl)
	if err != nil {
		slog.Error("mcp: stage pending update failed", "error", err)
		return errResult("stage failed: could not persist the staged write"), nil, nil
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	// IsError=true is deliberate (D3-C3) — see mcpStageStore.
	return errResult(fmt.Sprintf(
		"STAGED — NOT updated yet. This key requires write confirmation (confirm_writes).\n"+
			"target block: %s (%s)\n"+
			"payload_hash: %s\n"+
			"expires: %s\n"+
			"To execute this exact update, call the 'confirm' tool with this payload_hash. "+
			"The server holds the authoritative payload; confirming cannot alter it. "+
			"If the block changes before you confirm, the confirm is rejected and you must re-stage.",
		block.Title, resolvedID, hash, expiry)), nil, nil
}
