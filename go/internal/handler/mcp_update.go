// F6-C6 D-W6a: the MCP update tool. Additive — every existing tool is
// untouched (D3-m1). Semantics mirror the REST manage-update path
// (context_manage.go handleUpdate): resolve within writableBlockScopes
// (home_scope ∪ shared-if-allowed, v4.0.1), size limits, then
// store.UpdateBlock (which invalidates the vector of a changed title or
// content in its own transaction) + the same re-classify/temporal afterwork.
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
// and the store writes it with type_source='manual'.
//
// Since issue #280 the three re-type paths behave alike, and the rules that
// read the block's CURRENT type live in the statement that writes it
// (trg_block_retype, migration 174) rather than in a read ahead of it: a
// re-type out of a type marked untrusted is accepted and records that type on
// the block (untrusted_origin), and a comment or an issue keeps its type
// (store.ErrRetypeBound). #203 refused the first at stage,
// confirm and direct path (reserved_type); the owner's decision on #280 is
// that the corpus takes such a block up and the record keeps where its text
// came from.
package handler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type updateInput struct {
	ID       string         `json:"id" jsonschema:"block UUID (or unambiguous prefix) — must live in a scope this key may write"`
	Category *string        `json:"category,omitempty" jsonschema:"new category (omit = unchanged)"`
	Title    *string        `json:"title,omitempty" jsonschema:"new title (omit = unchanged)"`
	Content  *string        `json:"content,omitempty" jsonschema:"new content, max 50KB (omit = unchanged)"`
	Tags     []string       `json:"tags,omitempty" jsonschema:"replacement tag set ([] clears all tags, omit = unchanged)"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"replacement metadata object ({} clears, omit = unchanged); keep the source of the knowledge in metadata.source"`
	Type     string         `json:"type,omitempty" jsonschema:"new block type name from the registry (e.g. reference); written as a manual choice the title classifier no longer overrides; leaving a type marked untrusted records that type on the block as untrusted_origin (omit = unchanged)"`
	// Issue #406: a change of part of a block without sending the whole of it.
	Edits         []updateEditInput `json:"edits,omitempty" jsonschema:"text edits of the content instead of a whole new content (not together with content): applied in order to the block's current content, all or none; each old must occur exactly once (or at least once with replace_all), the answer names an edit that does not match"`
	MetadataMerge map[string]any    `json:"metadata_merge,omitempty" jsonschema:"keys merged into the stored metadata instead of replacing it (not together with metadata): a value sets the key, null removes it, every other key stays"`
}

// updateEditInput is one edit of the update tool (store.ContentEdit on the
// wire). Old and new are both required by the exported schema, so a call
// without new is refused by the SDK instead of deleting the text.
type updateEditInput struct {
	Old        string `json:"old" jsonschema:"the exact text to replace (bytes of UTF-8, case-sensitive, no normalisation); must not be empty"`
	New        string `json:"new" jsonschema:"the replacement text; an empty string deletes old"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence of old instead of requiring exactly one"`
}

// updateFieldsOf extracts the authoritative field list + UpdateBlockData from
// the tool input. The list (not value presence) is what the staged form
// hash-binds — "clear tags" and "leave tags" must never collide (D-W6a).
func updateFieldsOf(in updateInput) (store.UpdateBlockData, []string) {
	var data store.UpdateBlockData
	fields := make([]string, 0, 8)
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
	if in.Edits != nil {
		data.Edits = make([]store.ContentEdit, len(in.Edits))
		for i, e := range in.Edits {
			repl := e.New
			data.Edits[i] = store.ContentEdit{Old: e.Old, New: &repl, ReplaceAll: e.ReplaceAll}
		}
		fields = append(fields, "edits")
	}
	if in.MetadataMerge != nil {
		data.MetadataMerge = in.MetadataMerge
		fields = append(fields, "metadata_merge")
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
		case "metadata_merge":
			m := cw.MetadataMerge
			if m == nil {
				m = map[string]any{}
			}
			data.MetadataMerge = m
		}
	}
	return data
}

// finishBlockUpdate runs the afterwork of a block update: re-classify on
// title/metadata change (WF T4, design/01 §4.5 seam 5 — safe because
// ClassifyBlockAfterUpsert only writes type_source='auto' blocks, and
// deliberately match-only: a block whose new title stops matching keeps its
// current type, the hook promotes and never demotes; nothing is written once a
// later write replaced the title or a metadata key the rules read — issue
// #426), the temporal rows on content change (store.RefreshBlockTemporal,
// from the dates the update computed for content_times — issue #434; the rows
// always get createdAt as the meta-anchor, even without content times, and
// nothing is written once a later write replaced the content — issue #414). All non-fatal; reqID stamps every
// line with the request it belongs to. The embedding reset on a changed title
// or content is not afterwork: store.UpdateBlock clears the vector in the
// update's own transaction.
// Surface-neutral since D-W6c (pool+registry, not MCPConfig): the REST manage
// path (context_manage.go handleUpdate), the MCP direct path AND the shared
// confirm core (confirm_core.go) call it.
//
// Issue #406: a metadata_merge changes metadata as a replacement does, and
// edits change the content as a new content does — each runs the same
// afterwork.
func finishBlockUpdate(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, data store.UpdateBlockData, block *store.Block, reqID string) {
	// The update is committed: the afterwork runs detached from the client
	// (issue #453).
	ctx, cancel := followUpContext(ctx)
	defer cancel()
	var set *blocktype.Set
	if blocktypes != nil && (data.Title != nil || data.Metadata != nil || data.MetadataMerge != nil) {
		set = blocktypes.SnapshotForRequest(ctx)
	}
	finishBlockUpdateSet(ctx, pool, set, data, block, reqID)
}

// finishBlockUpdateSet is finishBlockUpdate on a registry snapshot the caller
// holds and in a context it already detached: the update items of a block
// batch (issue #211) run it in the batch's one afterwork bound. A nil set
// skips the re-classify, as a nil registry does.
func finishBlockUpdateSet(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, data store.UpdateBlockData, block *store.Block, reqID string) {
	if set != nil && (data.Title != nil || data.Metadata != nil || data.MetadataMerge != nil) {
		if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("update afterwork: re-classify failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}
	if data.Content != nil || data.Edits != nil {
		if _, err := store.RefreshBlockTemporal(ctx, pool, block); err != nil {
			logRequestError(ctx, "update afterwork: temporal refresh failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}
}

// resolveUpdateTarget resolves the tool's id argument within the caller's
// write-eligible scopes (REST parity: ResolveBlockID over writableBlockScopes,
// grants nil — a block grant is read-only and must never widen a write path).
// Returns "" plus a user-facing message on any miss.
func resolveUpdateTarget(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, id string) (string, string) {
	return resolveUpdateTargetIn(ctx, cfg.Pool, ar, id)
}

// resolveUpdateTargetIn is resolveUpdateTarget on a pool: the update items of a
// block batch resolve their ids through it (issue #211).
func resolveUpdateTargetIn(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult, id string) (string, string) {
	resolvedID, matches, err := store.ResolveBlockID(ctx, pool, id, writableBlockScopes(ar), nil)
	if err != nil {
		if len(matches) > 0 {
			return "", fmt.Sprintf("ambiguous id prefix (%d matches) — re-specify with a longer prefix or the full id", len(matches))
		}
		if errors.Is(err, store.ErrIDPrefixTooShort) || errors.Is(err, store.ErrNoScopes) {
			return "", fmt.Sprintf("cannot resolve block id: %v", err) // an answer about the id
		}
		// A fault of the resolve: the cause in the line, not in the answer
		// (issue #555), as resolveBatchUpdateTarget answers it.
		logRequestError(ctx, "mcp: resolve block id error", "error", err, "request_id", RequestIDFromContext(ctx))
		return "", "cannot resolve block id: internal error"
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
		// Issue #406: the combinations an update cannot mean, REST parity.
		if err := store.CheckUpdatePatch(data); err != nil {
			return errResult(err.Error()), nil, nil
		}
		// Size limits — same gate as the direct store path (REST parity).
		if msg := blockSizeLimit(strOrEmpty(data.Category), strOrEmpty(data.Title), strOrEmpty(data.Content)); msg != "" {
			return errResult(msg), nil, nil
		}
		// One registry snapshot per call, for the claim gates — resolved only
		// when the call names a type, so an update without one resolves nothing
		// it did not resolve before.
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
		if rej := claimReject(set, strOrEmpty(data.Category), input.Type, "", updateMetadataClaim(data.Metadata, data.MetadataMerge)); rej != nil {
			return errResultReject(rej), nil, nil
		}

		if ar.ConfirmWrites {
			return mcpStageUpdate(ctx, cfg, ar, input, fields)
		}

		resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
		if miss != "" {
			return errResult(miss), nil, nil
		}
		noteToolBlock(ctx, resolvedID)
		// Issue #519: the update is a write of the key's budget (update_budget.go).
		block, _, err := store.UpdateBlockBooked(ctx, cfg.Pool, resolvedID, data, writableBlockScopes(ar), updateWriteBudget(ctx, cfg.Cfg, ar))
		if err != nil {
			// I7/S3: the target is a derivative — 403 class, not a 500.
			// #227: the update would take another active block's identity —
			// the 409 class, the same verdict REST manage update answers.
			// #280: a re-type of a comment or an issue. #519: the budget.
			if rej := cmp.Or(updateBudgetReject(err), updateFailureReject(err)); rej != nil {
				return errResultReject(rej), nil, nil
			}
			noteToolWrite(ctx, err)
			logRequestError(ctx, "mcp: update error", "error", err, "block_id", resolvedID, "write_outcome", writeOutcome(err))
			return errResult("update failed: internal error"), nil, nil
		}
		if block == nil {
			return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
		}
		noteToolWrite(ctx, nil)
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
//
// Edits (issue #406) are resolved here, against the block the card pins: the
// stage reads content and updated_at in one statement, so the content the
// edits produce belongs to exactly the base the confirm re-checks. The card
// binds that content (op 'update' with field "content"), not the edits — the
// confirm writes what the card names, or refuses because the block moved.
// An edit that does not match gets no card (D1-M2). A metadata_merge stays a
// merge (CanonicalWrite.MetadataMerge says why).
func mcpStageUpdate(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, input updateInput, fields []string) (*mcp.CallToolResult, any, error) {
	var ttl time.Duration
	if cfg.Cfg != nil {
		ttl = cfg.Cfg.SnapshotForRequest(ctx).Writes.ConfirmTTL
	}
	// The write budget is counted and spent with the stage, in its
	// transaction (stageWithIntent, issue #519).

	resolvedID, miss := resolveUpdateTarget(ctx, cfg, ar, input.ID)
	if miss != "" {
		return errResult(miss), nil, nil
	}
	// The TOCTOU pin reads the block through the SAME write-eligible filter
	// the execute will use — and captures updated_at as the base fingerprint.
	// nil type set (V-11): a TOCTOU fingerprint, not a read answer.
	block, err := store.GetBlock(ctx, cfg.Pool, nil, resolvedID, writableBlockScopes(ar), nil)
	if err != nil {
		logRequestError(ctx, "mcp: stage update base read failed", "error", err, "block_id", resolvedID)
		return errResult("stage failed: internal error"), nil, nil
	}
	if block == nil {
		return errResult("Block not found (or not in a writable scope of this key)"), nil, nil
	}
	// A derivative is never updated by a client (UpdateBlock's WHERE, I7/S3):
	// no card for a write the confirm cannot execute (D1-M2), as the archive
	// stage refuses it (issue #275).
	if rej := derivativeTargetReject(block); rej != nil {
		return errResultReject(rej), nil, nil
	}

	content := strOrEmpty(input.Content)
	if input.Edits != nil {
		data, _ := updateFieldsOf(input)
		edited, err := store.ApplyContentEdits(block.Content, data.Edits)
		if rej := contentEditReject(err); rej != nil {
			return errResultReject(rej), nil, nil
		}
		if err != nil {
			slog.Error("mcp: stage update edits failed", "error", err, "block_id", resolvedID)
			return errResult("stage failed: internal error"), nil, nil
		}
		content = edited
		fields = stagedEditFields(fields)
	}

	cw := store.CanonicalWrite{
		Op:            "update",
		ID:            resolvedID,
		Scope:         block.Scope,
		Category:      strOrEmpty(input.Category),
		Title:         strOrEmpty(input.Title),
		Content:       content,
		Tags:          input.Tags,
		Metadata:      input.Metadata,
		Type:          input.Type,
		UpdateFields:  fields,
		BaseUpdatedAt: block.UpdatedAt.UTC().Format(time.RFC3339Nano),
		MetadataMerge: input.MetadataMerge,
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged update failed", "error", err)
		return errResult("stage failed: cannot canonicalize payload"), nil, nil
	}
	noteToolBlock(ctx, resolvedID)
	// Issue #519: the stage pays its write intent in its transaction.
	pw, err := stageWithIntent(ctx, cfg.Pool, updateWriteBudget(ctx, cfg.Cfg, ar), func(q pgxdb.Rower) (*store.PendingWrite, error) {
		return store.StagePendingWrite(ctx, q, ar.ApiKeyID, block.Scope, "update", "mcp", canonical, hash, ttl)
	})
	noteToolStaged(ctx, err, false)
	if rej := updateBudgetReject(err); rej != nil {
		return errResultReject(rej), nil, nil
	}
	if rej := requestKeyGoneReject(err); rej != nil {
		return errResultReject(rej), nil, nil // the key was deleted while the request ran (issue #555)
	}
	if err != nil {
		logRequestError(ctx, "mcp: stage pending update failed", "error", err, "write_outcome", statementOutcome(err))
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

// stagedEditFields is the field list of a staged update whose edits were
// resolved at stage time: "edits" becomes "content", the field the card binds
// and updateDataFromCanonical rebuilds. The other fields keep their places.
func stagedEditFields(fields []string) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		if f == "edits" {
			f = "content"
		}
		out[i] = f
	}
	return out
}
