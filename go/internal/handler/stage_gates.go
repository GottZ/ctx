package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stageGateReject is a pre-card rejection from runStageWriteGates: the staged
// write would fail at execute time, so it must never reach the ConfirmCard
// (D1-M2 — a card is a promise that the confirmed write will succeed).
// Status carries the direct-path HTTP status for the REST surface; MCP/Chat
// surfaces map Msg into their error result.
type stageGateReject struct {
	Status int
	Msg    string
}

// stageWriteGateResult is the post-gate write intent: exactly the values the
// direct path would hand to store.UpsertBlock. The stage site canonicalizes
// THIS (store.CanonicalWrite) — never the raw request — so the hash binds the
// resolved scope and the post-detector sensitivity.
type stageWriteGateResult struct {
	WriteScope    string
	ScopeExplicit bool
	Sens          store.SensitivityWrite
	Metadata      map[string]any
}

// runStageWriteGates runs EVERY write gate of the direct /api/store path over
// a staged write intent, in the same order (D1-M2 complete): required fields →
// size limits → sensitivity resolution → explicit-type validation → G40
// credentials detector → scope gate → write rate limit. It reuses the exact
// direct-path building blocks (blockSizeLimit, storeSensitivity,
// validateTypeNameAgainstSet, applyWriteDetector, writableBlockScopes,
// store.CheckRateLimit) so stage and execute can never diverge.
//
// It started as the STAGED path's copy of that order, while the direct MCP
// store arm still ran a hand-rolled subset. Gap-C6-a removed the split: the
// direct arm (mcpStoreHandler) calls THIS function too, so REST, MCP-direct
// and MCP-staged share one gate order and one set of rejection messages, and
// a gate added here reaches all three at once.
//
// pool is only touched when rateLimitWrite > 0 (nil pool + limit 0 is a valid
// test wiring). set == nil fails closed on an explicit type (never lets an
// unvalidated name reach the manual-provenance write path).
func runStageWriteGates(
	ctx context.Context,
	pool *pgxpool.Pool,
	set *blocktype.Set,
	ar *auth.AuthResult,
	req storeRequest,
	defaultSens backends.Sensitivity,
	rateLimitWrite int,
	reqID string,
) (*stageWriteGateResult, *stageGateReject) {
	// Required fields.
	if req.Category == "" || req.Title == "" || req.Content == "" {
		return nil, &stageGateReject{http.StatusBadRequest, "Missing required fields: category, title, content"}
	}

	// Size limits.
	if msg := blockSizeLimit(req.Category, req.Title, req.Content); msg != "" {
		return nil, &stageGateReject{http.StatusRequestEntityTooLarge, msg}
	}

	// Sensitivity: request > settings default.
	sens, sensErr := storeSensitivity(defaultSens, req.Sensitivity)
	if sensErr != "" {
		return nil, &stageGateReject{http.StatusBadRequest, sensErr}
	}

	// Explicit type: validate against the policy set BEFORE staging.
	if req.Type != "" {
		if msg := validateTypeNameAgainstSet(set, req.Type); msg != "" {
			return nil, &stageGateReject{http.StatusUnprocessableEntity, msg}
		}
	}

	// G40 credentials detector (upgrade-only; mutates sens + metadata).
	metadata := req.Metadata
	sens, metadata = applyWriteDetector(req.Content, reqID, sens, metadata)

	// Scope gate: same formula as every block write site.
	writeScope := ar.HomeScope
	scopeExplicit := false
	if req.Scope != "" {
		scopeExplicit = true
		if contains(writableBlockScopes(ar), req.Scope) {
			writeScope = req.Scope
		} else {
			return nil, &stageGateReject{http.StatusForbidden, "Cannot write to requested scope"}
		}
	}

	// Write rate limit (0 = disabled). Staging COUNTS as write intent: an LLM
	// stage-storm is exactly the abuse the limit exists for.
	if rateLimitWrite > 0 {
		writeCount, err := store.CheckRateLimit(ctx, pool, ar.ApiKeyID)
		if err != nil {
			slog.Error("stage gates: rate limit check error", "error", err, "request_id", reqID)
			return nil, &stageGateReject{http.StatusInternalServerError, "Internal server error"}
		}
		if writeCount >= rateLimitWrite {
			return nil, &stageGateReject{http.StatusTooManyRequests,
				fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", rateLimitWrite)}
		}
	}

	return &stageWriteGateResult{
		WriteScope:    writeScope,
		ScopeExplicit: scopeExplicit,
		Sens:          sens,
		Metadata:      metadata,
	}, nil
}

// validateTypeNameAgainstSet checks an explicit `type` value against a policy
// set snapshot. Empty msg = registered. Fail-closed on a nil set: an
// unvalidated name must never reach the manual-provenance write path (§5.1(b)).
// Shared by the direct path (validateStoreTypeName) and the stage gates.
func validateTypeNameAgainstSet(set *blocktype.Set, name string) string {
	if set == nil {
		return "type: block-type registry not wired — cannot validate type names"
	}
	if _, ok := set.Resolve(name); !ok {
		return fmt.Sprintf("type: unknown block type %q (see manage type-list)", name)
	}
	return ""
}
