// F6-C6 D-W5: the MCP stage-then-confirm dance. A key flagged with
// confirm_writes (090) gets its MCP store calls STAGED (server-held canonical
// payload, 089) instead of executed; only the confirm tool — selecting by
// payload hash, per-key — executes the write. Keys without the flag keep the
// direct path byte-for-byte (fail-open, D-E2); REST/CLI stay direct entirely
// (D-E1 scope boundary — gating LLM writes is the harness's job, this flag is
// the per-principal distrust tool for harnesses without one).
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type confirmInput struct {
	PayloadHash string `json:"payload_hash" jsonschema:"the payload_hash returned by a staged store or update call — confirming executes that exact server-held write"`
}

// confirmNotFoundMsg is the ONE generic miss answer (D1-M4): unknown hash,
// expired stage, already-consumed stage and a stage owned by another key are
// indistinguishable on purpose — the confirm surface must not be an oracle
// over other principals' staged writes.
const confirmNotFoundMsg = "no confirmable staged write for this payload_hash (unknown, expired, already confirmed, or staged under a different key)"

// mcpStageStore is the D-W5 branch for flagged keys: run EVERY direct-path
// write gate first (D1-M2 — the staged card is a promise that the confirmed
// write will succeed), canonicalize the POST-GATE result (never the raw
// input — the hash must bind the resolved scope and post-detector
// sensitivity), and stage it. The response is IsError=true BY DESIGN (D3-C3):
// a non-confirm-capable legacy client must never read "staged" as success and
// silently lose the write after TTL.
func mcpStageStore(ctx context.Context, cfg MCPConfig, ar *auth.AuthResult, input storeInput) (*mcp.CallToolResult, any, error) {
	reqID := RequestIDFromContext(ctx)

	var set *blocktype.Set
	if cfg.Blocktypes != nil {
		set = cfg.Blocktypes.SnapshotForRequest(ctx)
	}
	var defaultSens backends.Sensitivity
	var rateLimit int
	var ttl time.Duration
	if cfg.Cfg != nil {
		snap := cfg.Cfg.SnapshotForRequest(ctx)
		defaultSens = snap.Pool.DefaultBlockSensitivity
		rateLimit = snap.Query.RateLimitWrite
		ttl = snap.Writes.ConfirmTTL
	}

	// The MCP store tool carries no scope/type field (decision D4) — the
	// storeRequest mapping leaves both empty: scope resolves to home_scope
	// (scopeExplicit=false at execute time), type stays auto-classify.
	res, rej := runStageWriteGates(ctx, cfg.Pool, set, ar, storeRequest{
		Category:    input.Category,
		Title:       input.Title,
		Content:     input.Content,
		Tags:        input.Tags,
		Metadata:    input.Metadata,
		Sensitivity: input.Sensitivity,
	}, defaultSens, rateLimit, reqID)
	if rej != nil {
		return errResult(rej.Msg), nil, nil
	}

	cw := store.CanonicalWrite{
		Op:                "store",
		Scope:             res.WriteScope,
		Category:          input.Category,
		Title:             input.Title,
		Content:           input.Content,
		Tags:              input.Tags,
		Metadata:          res.Metadata,
		Sensitivity:       string(res.Sens.Value),
		SensitivityManual: res.Sens.Manual,
		SensitivityDetect: res.Sens.Detector,
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged write failed", "error", err, "request_id", reqID)
		return errResult("stage failed: cannot canonicalize payload"), nil, nil
	}

	pw, err := store.StagePendingWrite(ctx, cfg.Pool, ar.ApiKeyID, res.WriteScope, "store", "mcp", canonical, hash, ttl)
	if err != nil {
		slog.Error("mcp: stage pending write failed", "error", err, "request_id", reqID)
		return errResult("stage failed: could not persist the staged write"), nil, nil
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	// IsError=true is deliberate (D3-C3), not a failure signal: the write has
	// NOT happened, and a client that cannot confirm must surface that.
	return errResult(fmt.Sprintf(
		"STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"+
			"payload_hash: %s\n"+
			"expires: %s\n"+
			"To execute this exact write, call the 'confirm' tool with this payload_hash. "+
			"The server holds the authoritative payload; confirming cannot alter it.",
		hash, expiry)), nil, nil
}

// mcpConfirmHandler executes a previously staged write, selected by payload
// hash, strictly per-key (D1-M4: consume is bound to the caller's api_key_id).
// Before executing it re-validates writableBlockScopes (D1-M1 — write rights
// may have shrunk between stage and confirm); that rejection happens on a
// LOOKUP, not a consume, so the stage survives a transient rights problem.
// The consume itself is one atomic statement (fail-closed): replay, expiry,
// foreign key and unknown hash all land in the same generic miss.
func mcpConfirmHandler(cfg MCPConfig) mcp.ToolHandlerFor[confirmInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input confirmInput) (*mcp.CallToolResult, any, error) {
		if input.PayloadHash == "" {
			return errResult("payload_hash is required"), nil, nil
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}

		pw, err := store.LookupPendingWrite(ctx, cfg.Pool, ar.ApiKeyID, input.PayloadHash)
		if errors.Is(err, store.ErrPendingWriteNotFound) {
			return errResult(confirmNotFoundMsg), nil, nil
		}
		if err != nil {
			return errResult(fmt.Sprintf("confirm failed: %v", err)), nil, nil
		}

		var cw store.CanonicalWrite
		if err := json.Unmarshal(pw.Payload, &cw); err != nil {
			slog.Error("mcp: staged payload unmarshal failed", "error", err, "pending_id", pw.ID)
			return errResult("confirm failed: staged payload unreadable"), nil, nil
		}
		if (cw.Op != "store" && cw.Op != "update") || cw.Scope != pw.Scope {
			// A scope mismatch between row and payload would mean the stage
			// row was tampered with — reject.
			return errResult(confirmNotFoundMsg), nil, nil
		}

		// D1-M1: re-validate the write scope against the CURRENT key rights.
		// This runs on the un-consumed row: a shrunk right rejects without
		// burning the stage token.
		if !contains(writableBlockScopes(ar), pw.Scope) {
			return errResult(fmt.Sprintf("confirm rejected: scope %q is no longer writable for this key — the staged write stays pending until it expires", pw.Scope)), nil, nil
		}

		// TOCTOU guard for staged updates (D1-M3): the confirm executes only
		// against the exact block state the card was rendered for. Runs on
		// the un-consumed row (like D1-M1) — a drift rejects without burning
		// the token, the client re-reads and re-stages.
		if cw.Op == "update" {
			base, err := store.GetBlock(ctx, cfg.Pool, cw.ID, writableBlockScopes(ar), nil)
			if err != nil {
				return errResult(fmt.Sprintf("confirm failed: %v", err)), nil, nil
			}
			if base == nil {
				return errResult("confirm rejected: the target block no longer exists in a writable scope — the staged update stays pending until it expires"), nil, nil
			}
			if base.UpdatedAt.UTC().Format(time.RFC3339Nano) != cw.BaseUpdatedAt {
				return errResult(fmt.Sprintf(
					"confirm rejected: block %s changed since this update was staged (lost-update protection) — re-read the block and stage the update again; the stale stage expires on its own",
					cw.ID)), nil, nil
			}
		}

		// Atomic consume (exactly once). A racing double-confirm loses here
		// and gets the generic miss.
		if _, err := store.ConsumePendingWrite(ctx, cfg.Pool, ar.ApiKeyID, input.PayloadHash); err != nil {
			if errors.Is(err, store.ErrPendingWriteNotFound) {
				return errResult(confirmNotFoundMsg), nil, nil
			}
			return errResult(fmt.Sprintf("confirm failed: %v", err)), nil, nil
		}

		// op 'update' (D-W6a): execute over the SAME path as the direct
		// update tool (UpdateBlock + re-classify/temporal/re-embed).
		// scopeExplicit does not apply here — the update form never changes
		// the scope (no scope field, D4-analog), so there is nothing to mark
		// explicit; UpdateBlock filters by writableBlockScopes instead.
		if cw.Op == "update" {
			data := updateDataFromCanonical(cw)
			block, needsReEmbed, err := store.UpdateBlock(ctx, cfg.Pool, cw.ID, data, writableBlockScopes(ar))
			if err != nil {
				slog.Error("mcp: confirmed update execute failed", "error", err, "pending_id", pw.ID)
				return errResult(fmt.Sprintf("confirmed update failed to execute: %v — the stage token is consumed; re-stage the update", err)), nil, nil
			}
			if block == nil {
				// Vanished between the TOCTOU read and the execute — token is
				// consumed (rejected finding D1-m2 behaviour sentence).
				return errResult("confirmed update failed to execute: block no longer accessible — the stage token is consumed; re-stage the update"), nil, nil
			}
			finishBlockUpdate(ctx, cfg, data, block, needsReEmbed)
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("Confirmed and updated: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
			}, nil, nil
		}

		// op 'store': execute over the SAME path as the direct store tool
		// (upsert + classify + temporal). scopeExplicit=false mirrors the MCP
		// direct path (the store tool has no scope input; cw.Scope is the
		// resolved home_scope from stage time, re-validated above).
		sens := store.SensitivityWrite{
			Value:    backends.Sensitivity(cw.Sensitivity),
			Manual:   cw.SensitivityManual,
			Detector: cw.SensitivityDetect,
		}
		block, err := store.UpsertBlock(ctx, cfg.Pool, cw.Category, cw.Title, cw.Content, cw.Tags, cw.Metadata, cw.Scope, false, sens, cw.Type)
		if err != nil {
			// The token is consumed but the write failed — fail-closed and
			// rare (rejected finding D1-m2): the client re-stages.
			slog.Error("mcp: confirmed write execute failed", "error", err, "pending_id", pw.ID)
			return errResult(fmt.Sprintf("confirmed write failed to execute: %v — the stage token is consumed; re-stage the write", err)), nil, nil
		}

		var classifySet *blocktype.Set
		if cfg.Blocktypes != nil {
			classifySet = cfg.Blocktypes.SnapshotForRequest(ctx)
		}
		if _, err := store.ClassifyBlockAfterUpsert(ctx, cfg.Pool, classifySet, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("mcp: auto-classify failed", "error", err, "block_id", block.ID)
		}

		times := store.ExtractDates(block.Content)
		_ = store.UpdateContentTimes(ctx, cfg.Pool, block.ID, times)
		_ = store.PopulateTemporal(ctx, cfg.Pool, block.ID, times, block.CreatedAt)

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Confirmed and stored: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
		}, nil, nil
	}
}
