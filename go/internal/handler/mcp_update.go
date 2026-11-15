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
// a new row goes, here it would MOVE an existing one.
//
// The type IS a field since issue #203. Re-typing has no flow of its own
// behind it — REST manage-update runs the claim gates and the store sets two
// columns — and the store tool already re-typed an existing row through its
// upsert branch, at the price of resending the whole block under an exactly
// reproduced category and title. The field runs the gates REST runs, through
// the same function (claimReject, against the request's registry snapshot),
// and the store writes it with type_source='manual'. One rule is this tool's
// own (untrustedLiftReject): a re-type may not take the untrusted marking off
// a block, at stage time, at confirm time and on the direct path.
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
	Type     string         `json:"type,omitempty" jsonschema:"new block type name from the registry (e.g. reference); written as a manual choice the title classifier no longer overrides; a type that would drop the untrusted marking of the block's current type is refused (omit = unchanged)"`
}

// updateFieldsOf extracts the authoritative field list + UpdateBlockData from
// the tool input. The list (not value presence) is what the staged form
// hash-binds — "clear tags" and "leave tags" must never collide (D-W6a).
func updateFieldsOf(in updateInput) (store.UpdateBlockData, []string) {
	var data store.UpdateBlockData
	fields := make([]string, 0, 6)
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
	// "" is "not part of this write", the empty-value convention claimReject
	// and the store tool's own `type` share: there is no clear for a type.
	if in.Type != "" {
		t := in.Type
		data.Type = &t
		fields = append(fields, "type")
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
		case "type":
			v := cw.Type
			data.Type = &v
		}
	}
	return data
}

// untrustedLiftReject is the update tool's one type rule beyond the claim
// gates (issue #203): a re-type may not take the untrusted marking off a
// block. nil = admissible.
//
// retrieval.untrusted marks a type whose content a third party shapes (tool
// output, transcripts); recall frames such a block as data, not as knowledge
// or instruction. A re-type from such a type to one without the marking would
// end that framing for text the block still holds — the same direction as a
// sensitivity downgrade, which REST lets through only behind an explicit
// confirm flag. This tool has no such flag and its confirm_writes payload no
// field for one, so it refuses: the class is reserved_type (the remedy is the
// one that code names — drop the claim), the message says which marking and
// why. A move between two untrusted types keeps the marking, a move onto an
// untrusted type adds it; both pass.
//
// current is the block's type as stored, next the claimed one; both resolve
// against the caller's registry snapshot through Set.IsUntrusted, the
// predicate recall itself frames with (store.untrustedOf), so a type the
// caller's snapshot does not mark untrusted is not treated as marked here
// either. A nil set is unreachable — claimReject refuses a named type on an
// unwired registry first — and answers nil so the function is total.
func untrustedLiftReject(set *blocktype.Set, current, next string) *writeReject {
	if next == "" || set == nil || !set.IsUntrusted(current) || set.IsUntrusted(next) {
		return nil
	}
	return classReservedType.reject(fmt.Sprintf(
		"type: the block's current type %q marks its content as untrusted (retrieval.untrusted) and %q does not — "+
			"the update tool does not take that marking off a block", current, next))
}

// mcpUpdateTrustGate runs untrustedLiftReject on the direct path: it reads the
// block's current type through the same write-eligible filter the update will
// use. nil = proceed, otherwise the tool's answer. Called only for an update
// that names a type, so an update without one does no extra read.
//
// The read and the update are two statements, the window REST's sensitivity
// guard (applySensitivityGuard) has between its read and the same update: a
// concurrent writer that re-types the block in between is not seen.
func mcpUpdateTrustGate(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, set *blocktype.Set, id, next string) *mcp.CallToolResult {
	// nil type set (V-11): a gate input, not a read answer.
	base, err := store.GetBlock(ctx, cfg.Pool, nil, id, writableBlockScopes(ar), nil)
	if err != nil {
		slog.Error("mcp: update type base read failed", "error", err, "block_id", id)
		return errResult("update failed: internal error")
	}
	if base == nil {
		return errResult("Block not found (or not in a writable scope of this key)")
	}
	if rej := untrustedLiftReject(set, base.TypeName, next); rej != nil {
		return errResultReject(rej)
	}
	return nil
}

// finishBlockUpdate runs the afterwork of a block update: re-classify on
// title/metadata change (WF T4, design/01 §4.5 seam 5 — safe because
// ClassifyBlockAfterUpsert only writes type_source='auto' blocks, and
// deliberately match-only: a block whose new title stops matching keeps its
// current type, the hook promotes and never demotes), temporal re-extraction
// on content change (PopulateTemporal always runs: createdAt is the
// meta-anchor even without content times). All non-fatal; reqID stamps every
// line with the request it belongs to. The embedding reset on a changed title
// or content is not afterwork: store.UpdateBlock clears the vector in the
// update's own transaction.
// Surface-neutral since D-W6c (pool+registry, not MCPConfig): the REST manage
// path (context_manage.go handleUpdate), the MCP direct path AND the shared
// confirm core (confirm_core.go) call it.
func finishBlockUpdate(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, data store.UpdateBlockData, block *store.Block, reqID string) {
	if blocktypes != nil && (data.Title != nil || data.Metadata != nil) {
		set := blocktypes.SnapshotForRequest(ctx)
		if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("update afterwork: re-classify failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}
	if data.Content != nil {
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(ctx, pool, block.ID, times); err != nil {
			slog.Error("update afterwork: content_times update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
		if err := store.PopulateTemporal(ctx, pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("update afterwork: temporal populate failed", "error", err, "block_id", block.ID, "request_id", reqID)
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
			return errResult("nothing to update — provide at least one of category, title, content, tags, metadata, type"), nil, nil
		}
		// Size limits — same gate as the direct store path (REST parity).
		if msg := blockSizeLimit(strOrEmpty(data.Category), strOrEmpty(data.Title), strOrEmpty(data.Content)); msg != "" {
			return errResult(msg), nil, nil
		}
		// One registry snapshot per call, for the claim gates and the untrusted
		// rule — resolved only when the call names a type, so an update without
		// one resolves nothing it did not resolve before.
		var set *blocktype.Set
		if input.Type != "" {
			set = cfg.mcpTypeSnapshot(ctx)
		}
		// I7 claim gates (design D-01 §4.3.1): MOVING a block into a reserved
		// category occupies it exactly as creating one there does, and a
		// metadata replacement can plant the provenance key just as a create
		// can. The type arm (issue #203) is REST manage-update's verdict
		// (updateClaimReject): registry membership against the request's
		// snapshot, fail-closed on an unwired registry, the derived and
		// internal-only refusals, and the parent gate, which refuses a
		// required-parent type outright because this tool carries no parent.
		// Ahead of the stage branch, so a flagged key gets no card either.
		if rej := claimReject(set, strOrEmpty(data.Category), input.Type, "", data.Metadata); rej != nil {
			return errResultReject(rej), nil, nil
		}

		if ar.ConfirmWrites {
			return mcpStageUpdate(ctx, cfg, ar, set, input, fields)
		}

		resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
		if miss != "" {
			return errResult(miss), nil, nil
		}
		if input.Type != "" {
			if r := mcpUpdateTrustGate(ctx, cfg, ar, set, resolvedID, input.Type); r != nil {
				return r, nil, nil
			}
		}
		block, _, err := store.UpdateBlock(ctx, cfg.Pool, resolvedID, data, writableBlockScopes(ar))
		if err != nil {
			// I7/S3: the target is a derivative — 403 class, not a 500.
			// #227: the update would take another active block's identity —
			// the 409 class, the same verdict REST manage update answers.
			if rej := provenanceRejectOr(err, identityTakenReject(err)); rej != nil {
				return errResultReject(rej), nil, nil
			}
			slog.Error("mcp: update error", "error", err, "block_id", resolvedID)
			return errResult("update failed: internal error"), nil, nil
		}
		if block == nil {
			return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
		}
		finishBlockUpdate(ctx, cfg.Pool, cfg.Blocktypes, data, block, RequestIDFromContext(ctx))
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
func mcpStageUpdate(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, set *blocktype.Set, input updateInput, fields []string) (*mcp.CallToolResult, any, error) {
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
	// The untrusted rule over the type the pin just read: a card for a re-type
	// the confirm would refuse is never issued (D1-M2). The confirm runs it
	// again (confirmTypeTrustReject) against the registry of that moment.
	if rej := untrustedLiftReject(set, block.TypeName, input.Type); rej != nil {
		return errResultReject(rej), nil, nil
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
		Type:          input.Type,
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
