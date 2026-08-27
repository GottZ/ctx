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

	// Scope and type both ride into the gates here, and for the same reason: a
	// card must never promise a write the confirm cannot execute. The scope
	// was ALREADY refused in mcpStoreHandler if it lies outside
	// writableBlockScopes (E-M4 — the gate runs ahead of the stage branch, so
	// no card exists for a foreign scope); running it again through the chain
	// is what puts the RESOLVED scope into res.WriteScope, which the canonical
	// payload below binds and the confirm re-validates (D1-M1).
	res, rej := runStageWriteGates(ctx, cfg.Pool, set, ar, storeRequest{
		Category:    input.Category,
		Title:       input.Title,
		Content:     input.Content,
		Tags:        input.Tags,
		Metadata:    input.Metadata,
		Sensitivity: input.Sensitivity,
		Type:        input.Type,
		Scope:       input.Scope,
	}, defaultSens, rateLimit, reqID)
	if rej != nil {
		return errResultReject(rej), nil, nil
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
		// omitempty: a type-less store canonicalizes to the exact pre-N-26
		// bytes, so a hash a client already holds stays confirmable across
		// the upgrade.
		Type: input.Type,
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("mcp: canonicalize staged write failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: cannot canonicalize payload"), nil, nil
	}

	pw, err := store.StagePendingWrite(ctx, cfg.Pool, ar.ApiKeyID, res.WriteScope, "store", "mcp", canonical, hash, ttl)
	if err != nil {
		slog.Error("mcp: stage pending write failed", "error", err, "request_id", reqID)
		return classInternal.errResult("stage failed: could not persist the staged write"), nil, nil
	}

	// Gap-C6-a: book the write INTENT. runStageWriteGates CHECKED the budget
	// above, but nothing ever SPENT it — store.CheckRateLimit counts
	// context_access_log rows with action='write', and no staged write booked
	// one, so a purely staging key sat at writeCount 0 forever and the limit
	// could not bite the exact abuse it exists for (an LLM stage-storm).
	//
	// block_id stays NULL: at stage time no block exists, and an unconfirmed
	// stage may never produce one. Charging at INTENT time — not at confirm —
	// is the deliberate semantics: the budget guards the flood of proposals,
	// and executeConfirm therefore books nothing (a confirmed write is already
	// paid for). Booked only AFTER a successful stage, so a failed stage costs
	// nothing; a re-armed duplicate stage IS a fresh call and is charged like
	// one. Logged, never fatal: losing an audit row must not turn a persisted
	// stage into an error result.
	if err := store.LogAccess(ctx, cfg.Pool, ar.ApiKeyID, "", "write"); err != nil {
		slog.Error("mcp: staged write log error", "error", err, "request_id", reqID)
	}

	expiry := "never (writes.confirm_ttl = 0)"
	if pw.ExpiresAt != nil {
		expiry = fmt.Sprintf("%s (in %s)", pw.ExpiresAt.UTC().Format(time.RFC3339), time.Until(*pw.ExpiresAt).Round(time.Second))
	}
	// IsError=true is deliberate (D3-C3), not a failure signal: the write has
	// NOT happened, and a client that cannot confirm must surface that. And
	// therefore deliberately UNCODED (Gap-C6-c): the gates PASSED, so this is
	// no rejection — the absence of a code is how a client tells a staged
	// write from a refused one.
	return errResult(fmt.Sprintf(
		"STAGED — NOT saved yet. This key requires write confirmation (confirm_writes).\n"+
			"payload_hash: %s\n"+
			"expires: %s\n"+
			"To execute this exact write, call the 'confirm' tool with this payload_hash. "+
			"The server holds the authoritative payload; confirming cannot alter it.",
		hash, expiry)), nil, nil
}

// mcpConfirmHandler executes a previously staged write, selected by payload
// hash, strictly per-key (D1-M4). Since D-W6c the sequence itself lives in
// confirm_core.go (shared with POST /api/confirm) — this handler only maps
// the typed outcome onto the exact MCP wordings (raw errors surface here;
// the HTTP surface launders them).
func mcpConfirmHandler(cfg MCPConfig) mcp.ToolHandlerFor[confirmInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input confirmInput) (*mcp.CallToolResult, any, error) {
		if input.PayloadHash == "" {
			return errResult("payload_hash is required"), nil, nil
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}

		out := executeConfirm(ctx, cfg.Pool, cfg.Blocktypes, ar, input.PayloadHash)
		switch out.Kind {
		case confirmOK:
			if out.Op == store.OpBlobLink {
				// The edge is what this op wrote, so it is what the answer
				// names — a link that reported only the blob would be
				// indistinguishable from a no-op.
				linked := ""
				if out.Blob.ContextBlockID != nil {
					linked = *out.Blob.ContextBlockID
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{textContent(fmt.Sprintf("Confirmed and linked blob: %s (id: %s, context_block_id: %s)",
						out.Blob.Title, out.Blob.ID, linked))},
				}, nil, nil
			}
			if out.Op == store.OpBlobStore {
				return &mcp.CallToolResult{
					Content: []mcp.Content{textContent(fmt.Sprintf("Confirmed and stored blob: %s (id: %s, category: %s, %d bytes)",
						out.Blob.Title, out.Blob.ID, out.Blob.Category, out.Blob.FileSize))},
				}, nil, nil
			}
			verb := "stored"
			if out.Op == "update" {
				verb = "updated"
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("Confirmed and %s: %s (id: %s, category: %s)", verb, out.Block.Title, out.Block.ID, out.Block.Category))},
			}, nil, nil
		case confirmMiss:
			return errResult(confirmNotFoundMsg), nil, nil
		case confirmScopeRejected:
			return errResult(confirmScopeRejectMsg(out.Scope)), nil, nil
		case confirmClaimRejected:
			// Same code the stage gates carry on this surface
			// (structuredContent.code), same prose. The card survives.
			return errResultReject(out.Reject), nil, nil
		case confirmTOCTOUGone:
			return errResult(confirmTOCTOUGoneMsg), nil, nil
		case confirmTOCTOUDrift:
			return errResult(confirmTOCTOUDriftMsg(out.BlockID)), nil, nil
		case confirmBlockRefGone:
			return errResult(confirmBlockRefGoneMsg), nil, nil
		case confirmUnreadable:
			return errResult("confirm failed: staged payload unreadable"), nil, nil
		case confirmExecErr:
			if out.Op == "update" {
				return errResult(fmt.Sprintf("confirmed update failed to execute: %v — the stage token is consumed; re-stage the update", out.Err)), nil, nil
			}
			return errResult(fmt.Sprintf("confirmed write failed to execute: %v — the stage token is consumed; re-stage the write", out.Err)), nil, nil
		case confirmExecGone:
			if out.Op == store.OpBlobLink {
				return errResult("confirmed blob_link failed to execute: blob no longer accessible — the stage token is consumed; re-stage the link"), nil, nil
			}
			return errResult("confirmed update failed to execute: block no longer accessible — the stage token is consumed; re-stage the update"), nil, nil
		default: // confirmInfraErr
			return errResult(fmt.Sprintf("confirm failed: %v", out.Err)), nil, nil
		}
	}
}
