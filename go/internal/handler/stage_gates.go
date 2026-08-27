package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
//
// Gap-C6-c: every verdict is built from a rejectClass (errcode.go), so it
// carries the same machine code the REST write handlers emit for that class.
func runStageWriteGates(
	ctx context.Context,
	pool *pgxpool.Pool,
	set *blocktype.Set,
	ar *auth.AuthResult,
	req storeRequest,
	defaultSens backends.Sensitivity,
	rateLimitWrite int,
	reqID string,
) (*stageWriteGateResult, *writeReject) {
	// Required fields.
	if req.Category == "" || req.Title == "" || req.Content == "" {
		return nil, classMissingFields.reject("Missing required fields: category, title, content")
	}

	// Size limits.
	if msg := blockSizeLimit(req.Category, req.Title, req.Content); msg != "" {
		return nil, classSizeCap.reject(msg)
	}

	// Sensitivity: request > settings default.
	sens, sensErr := storeSensitivity(defaultSens, req.Sensitivity)
	if sensErr != "" {
		return nil, classInvalidSensitivity.reject(sensErr)
	}

	// I7 claim gates (S1 + S2 + S3-metadata) and the WF T10 registry check,
	// BEFORE staging: a flagged key must not even get a CARD for a type, a
	// namespace or a provenance claim it may not make — the same reasoning the
	// scope gate's placement carries. req.Metadata is the RAW client metadata:
	// the detector below adds its own key afterwards, and gating the
	// post-detector map would gate the server's own annotation.
	if rej := claimReject(set, req.Category, req.Type, req.Metadata); rej != nil {
		return nil, rej
	}

	// G40 credentials detector (upgrade-only; mutates sens + metadata).
	metadata := req.Metadata
	sens, metadata = applyWriteDetector(req.Content, reqID, sens, metadata)

	// Scope gate: same formula as every block write site — literally the same
	// function since E-M4 (resolveWriteScope), which is what lets the MCP
	// store tool's own `scope` field reach REST's verdict without a copy.
	writeScope, scopeExplicit, scopeRej := resolveWriteScope(ar, req.Scope)
	if scopeRej != nil {
		return nil, scopeRej
	}

	// Write rate limit (0 = disabled). Staging COUNTS as write intent: an LLM
	// stage-storm is exactly the abuse the limit exists for.
	if rateLimitWrite > 0 {
		writeCount, err := store.CheckRateLimit(ctx, pool, ar.ApiKeyID)
		if err != nil {
			slog.Error("stage gates: rate limit check error", "error", err, "request_id", reqID)
			return nil, classInternal.reject("Internal server error")
		}
		if writeCount >= rateLimitWrite {
			return nil, classRateLimit.reject(
				fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", rateLimitWrite))
		}
	}

	return &stageWriteGateResult{
		WriteScope:    writeScope,
		ScopeExplicit: scopeExplicit,
		Sens:          sens,
		Metadata:      metadata,
	}, nil
}

// validateTypeNameAgainstSet checks an explicit `type` value a CLIENT asserted.
// nil = admissible. Shared by the direct REST path (validateStoreTypeName), the
// stage gates and, since W01-2a, the manage-update path
// (ManageHandler.validateBlockTypeName) — the one place where "may this caller
// claim this type" is decided, so a fourth write surface cannot grow a fifth
// answer.
//
// Two refusals, in this order:
//
//  1. I7/S1 (design D-01 §4.3.1, §5.2 B14): a type of the DERIVED layer is
//     never client-claimable. It is checked FIRST, before the registry is even
//     consulted, and that order is load-bearing: the check hangs on
//     derived.StratumOf — a compiled-in string mapping — while the registry is
//     a table, and type_name carries no FK (bruchpfad B15, registry.go
//     sweepOrphans only warns). If the row is dropped or renamed, S1 must still
//     refuse the name. Claiming a derived type buys type_source='manual'
//     (the classifier never touches the block again), guard.check=false,
//     guard.candidate=false, untrusted=false and the optics of a proven
//     derivative — none of which a client may hand itself.
//  2. the registry membership check (WF T10), fail-closed on a nil set: an
//     unvalidated name must never reach the manual-provenance write path
//     (§5.1(b)).
func validateTypeNameAgainstSet(set *blocktype.Set, name string) *writeReject {
	if derived.IsDerivedType(name) {
		return classReservedType.reject(fmt.Sprintf(
			"type: %q belongs to the derived layer and is assigned by the server, not claimed by a client", name))
	}
	if set == nil {
		return classUnknownType.reject("type: block-type registry not wired — cannot validate type names")
	}
	if _, ok := set.Resolve(name); !ok {
		return classUnknownType.reject(fmt.Sprintf("type: unknown block type %q (see manage type-list)", name))
	}
	return nil
}

// claimReject runs the three gates that decide what a client may CLAIM about a
// block it writes: the category it occupies (I7/S2), the provenance key it
// carries in its own metadata (I7/S3, second half) and the type it names
// (I7/S1 plus the WF T10 registry check). nil = admissible.
//
// ONE function and ONE order — category, metadata, type — called at the same
// position by every write surface, because the gates only add up to an
// invariant when their order and their verdicts are identical everywhere: a
// surface that ran them in its own order would answer the same payload with a
// different rejection, and that difference is what a probing client measures.
// The claim: REST /api/store, the MCP-direct and MCP-staged chain, the chat
// stage runner, /api/ingest, manage-update, the MCP update tool AND the confirm
// core all reach the invariant through this function; the empty-value
// convention (category "" / type "" / nil metadata = "not part of this write")
// is what lets the by-id update surfaces share it with the creating ones.
//
// It was two gates and two orders until the W01-2a Nachbesserung: the
// manage-update copy checked type before category and answered 422 where
// /api/store answered 403 for the identical payload (review finding #6) — the
// exact property this comment claimed.
func claimReject(set *blocktype.Set, category, typeName string, metadata map[string]any) *writeReject {
	if rej := reservedCategoryReject(category); rej != nil {
		return rej
	}
	if rej := reservedMetadataReject(metadata); rej != nil {
		return rej
	}
	if typeName == "" {
		return nil
	}
	return validateTypeNameAgainstSet(set, typeName)
}

// reservedMetadataReject is the second half of I7/S3 (design D-01 §4.3.1 read
// as a whole: "die Ebene eines Blocks vergibt das System, nie der Client").
// nil = admissible.
//
// The first half refuses a client write that would OVERWRITE a block carrying
// provenance. Without this half that guard is a lever the attacker operates:
// the provenance key was client-writable, the system writers' identities are
// fully predictable ("index"/"topic-map-<scope>", "index"/"root-map-<scope>"),
// and one planted block therefore locked digest and rootmap out of that scope
// permanently — `digest.go:147` turns the refusal into `return err` and the
// whole run dies. Cost O(1) for the attacker, O(corpus) for the operator
// (review finding #3).
//
// A REFUSAL, not a strip: derived.StripReserved exists for the WRITER side of
// the contract, where dropping a key the model invented is right. Here the
// caller asserted something about the block's standing in the derivation
// order, and answering 200 to that assertion while discarding it would tell
// the client a block is a derivative when it is not. Fail-closed and visible.
//
// The system path is untouched by construction: the arms write through
// store.UpsertBlock directly and never pass a handler gate — the same seam S1
// and S2 leave open, documented at store/blocks.go's S3 guard.
func reservedMetadataReject(metadata map[string]any) *writeReject {
	if !derived.HasProvenance(metadata) {
		return nil
	}
	return classReservedMetadata.reject(fmt.Sprintf(
		"metadata: %q is the derived layer's provenance key — it is written by the server, not by a client",
		derived.MetadataKey))
}

// reservedCategoryReject is I7/S2 (design D-01 §4.3.1): the categories the
// derived arms write into are not client-writable. nil = admissible.
//
// The list is code-owned in internal/derived, and every client write surface
// asks THIS function rather than the list, so the refusal wording and the code
// cannot drift per surface. S1 alone would not do: a client does not have to
// name the type to occupy the identity a derivative upserts on.
func reservedCategoryReject(category string) *writeReject {
	if !derived.IsReservedCategory(category) {
		return nil
	}
	return classReservedCategory.reject(fmt.Sprintf(
		"category: %q is reserved for the derived layer and is not client-writable", category))
}

// provenanceRejectOr maps a store write error onto the write surfaces' verdict
// vocabulary: the S3 sentinel becomes 403 provenance_protected, everything else
// stays the caller's generic verdict. It is the single place that binds
// store.ErrProvenanceProtected to a status, so a surface cannot answer the same
// refusal with 500.
func provenanceRejectOr(err error, fallback *writeReject) *writeReject {
	if errors.Is(err, store.ErrProvenanceProtected) {
		return classProvenanceProtected.reject(
			"block carries derived provenance — a client write cannot replace its content or metadata")
	}
	return fallback
}
