package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

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

// extraWriteGates is the WIRING probe's seam into this chain: gates that run
// after the seven production ones, on every surface that calls
// runStageWriteGates. It is nil in every production build — the only assignment
// lives in the probe test (mcp_store_gatechain_integration_test.go), which
// restores it through t.Cleanup.
//
// It exists for the same reason handleBlobManage takes its action table as a
// parameter (blob.go:294-300): a surface that copies the seven gates by hand
// and a surface that CALLS this function answer the same bytes for every one of
// them, so no assertion over those seven can tell the two wirings apart. A
// probe gate hung here can: it reaches REST, MCP-direct and MCP-staged if and
// only if all three really run this chain — which is what keeps the claim of
// the doc comment below ("a gate added here reaches every call site at once")
// checkable after the fact instead of merely stated. Three arms, not four: the
// probe drives the surfaces a client can compare byte-for-byte; the chain's
// fourth call site is named below.
//
// A gate sees the REQUEST and nothing else: not the resolved sensitivity, not
// the post-detector metadata, not the resolved scope. That is deliberate — the
// seam can neither reorder nor overturn a verdict of the seven, and it is
// useless as a place to grow production logic.
var extraWriteGates []func(req storeRequest) *writeReject

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
// and MCP-staged share one gate order and one set of rejection messages.
//
// FOUR call sites reach it, and a gate added here reaches every call site at
// once: REST /api/store (context_store.go), the MCP store tool for BOTH its arms
// (mcp.go), the confirm of a staged card (mcp_confirm.go) and the chat stage
// runner (chat_stage.go) — five surfaces on four call sites. The doc comments
// of this chain counted three of them until NZ-3 point 9, because the wiring
// probe drives three; the chat arm was the uncounted fourth caller (T02-11
// Übergabe 5). What is true of it is that it reaches the chain with no scope,
// type or sensitivity input of its own (chat_stage.go), so every gate that reads
// one of those fields is inert there — inert, not absent.
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
	if rej := claimReject(set, req.Category, req.Type, req.ParentID, req.Metadata); rej != nil {
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

	// Probe seam, last and empty in production (see extraWriteGates).
	for _, gate := range extraWriteGates {
		if rej := gate(req); rej != nil {
			return nil, rej
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
// nil = admissible. It has exactly one caller — claimReject below — and every
// write surface reaches it through that one function, so "may this caller claim
// this type" is decided in a single place and no write surface can grow its own
// answer.
//
// Three refusals, in this order (the third since C2-8):
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
//
//  2. the registry membership check (WF T10), fail-closed on a nil set: an
//     unvalidated name must never reach the manual-provenance write path
//     (§5.1(b)).
//
//  3. BA14/C2-8 (design D-02 §3.1, §5.1 BA14): the resolved policy's
//     write.internal_only. Same verdict CLASS as (1) — 422 reserved_type — and
//     that is deliberate: the two checks answer the same question ("is this
//     type the client's to claim?") from two authorities, and splitting the
//     code would ask a client to branch on WHICH authority refused, a
//     distinction with no consequence on its side. Only the message differs,
//     which is exactly what rejectClass.reject is for.
//
//     It runs AFTER the membership check, not before, and that order is what
//     makes it fail-closed without a fail-open predicate: an unknown name is
//     already gone at (2), so the policy read below is always a policy that
//     actually resolved. Reading a not-ok Resolve here would hand back the zero
//     Policy — write.internal_only=false — and answer "claimable" for a name the
//     registry never heard of.
//
//     (1) still stands in front of it and is not redundant: the derived name
//     list is compiled in, so it refuses insight/catalog even when the registry
//     row is dropped, renamed or served from a degraded snapshot (bruchpfad
//     B15) — the state in which the row-carried flag below is exactly what is
//     missing.
func validateTypeNameAgainstSet(set *blocktype.Set, name string) *writeReject {
	if derived.IsDerivedType(name) {
		return classReservedType.reject(fmt.Sprintf(
			"type: %q belongs to the derived layer and is assigned by the server, not claimed by a client", name))
	}
	if set == nil {
		return classUnknownType.reject("type: block-type registry not wired — cannot validate type names")
	}
	pol, ok := set.Resolve(name)
	if !ok {
		return classUnknownType.reject(fmt.Sprintf("type: unknown block type %q (see manage type-list)", name))
	}
	if pol.Write.InternalOnly {
		return classReservedType.reject(fmt.Sprintf(
			"type: %q is an internal write target (write.internal_only) — its blocks are written by the server, not claimed by a client", name))
	}
	return nil
}

// parentRequiredReject is the fourth claim gate: a type whose registry policy
// says parent.mode=required may not be claimed by a write that carries no
// parent. nil = admissible.
//
// It is the MECHANISM behind a policy that had none (design/02 §8 E02-4).
// blocktype.Set.ParentMode resolved the value and no production path read it,
// so a registry row could promise orphan prevention that no write delivered:
// REST /api/store, both MCP store arms, manage-update and the confirm of a
// staged card all take a client-named `type` out of the same client JSON. A
// block of a required-parent type written through one of them without a parent
// is an orphan by construction — while the registry had accepted the very
// configuration that forbids it. On the shipped registry the affected type is
// `comment`, which is not write.internal_only: `{"type":"comment"}` on
// /api/store bought a comment block with parent_id NULL.
//
// parentID is what THIS WRITE carries, not what the row has — empty means "this
// write names no parent". Since NZ-3 exactly one surface can fill it:
// storeRequest.parent_id on REST /api/store, which hands the id to
// store.PutBlockParent after the upsert (context_store.go). There the gate is a
// CONDITION; everywhere else it stays the total refusal T02-11 built, because
// those shapes carry no parent field at all and pass "" for that reason. Two
// other callers of claimReject — the chat stage runner and /api/ingest — pass
// no type either, so the gate is inert on them by construction rather than by
// omission, exactly as validateTypeNameAgainstSet already is. The MCP update
// tool passes its `type` since issue #203 and no parent, so there the gate is
// the total refusal it is on manage-update.
//
// The refusal names the type's own DOMAIN path, and that is not the same offer
// as parent_id: for `comment` the domain path is store.InsertCommentBlock behind
// the issue verbs (manage issue-comment-create, POST
// /api/project/{id}/issues/{block_id}/comments, the MCP issue_comment tool),
// which takes parent_id as a mandatory argument, derives title, local sequence
// and scope FROM the parent, and refuses an empty parent itself
// (store.ErrCommentParentRequired). /api/store with parent_id hangs a block
// under a parent; it does not become that domain path. That path runs neither
// this gate nor this chain and is unchanged, down to the bytes of its first
// refusal.
//
// WHY IN claimReject AND NOT IN runStageWriteGates: the claim gates are the
// surfaces' shared answer to "what may a client assert about a block it writes",
// and manage-update asserts a type from the same JSON without being able to set
// a parent either. Hanging the gate on the store chain alone would have made
// orphan prevention a property of the VERB a client picks — the exact failure
// updateClaimReject (context_manage.go) was written to close. It also inherits
// the confirm-time re-check (confirm_core.go): a card staged before the gate
// existed is refused at confirm instead of executing under the old rules.
//
// KNOWN CONSEQUENCE, deliberate: manage-update and the MCP update tool cannot
// re-assert a required-parent type on a block that already HAS a parent either
// — the gate sees the claim, not the row. No legitimate write is lost by that,
// because no update payload can give a block the parent the type demands; the
// case is a no-op re-assertion on an existing comment.
//
// It runs AFTER validateTypeNameAgainstSet, and that order is load-bearing:
// ParentMode falls back to ParentModeNone for names it does not know, so a
// parent gate placed ahead of the membership check would answer "admissible"
// for a typo and hand the verdict to the next gate — and a nil set would reach
// s.policies through a nil receiver. Past the membership check the set is
// non-nil and the name resolved.
func parentRequiredReject(set *blocktype.Set, name, parentID string) *writeReject {
	if set == nil {
		// Unreachable from claimReject (the membership check above fails closed
		// with unknown_type on a nil set); here so the function is total.
		return nil
	}
	if set.ParentMode(name) != blocktype.ParentModeRequired {
		return nil
	}
	if parentID != "" {
		// The mode is satisfied by the CLAIM; whether the named parent exists, is
		// visible and shares the scope is decided at the write, by
		// store.ParentLinkable and store.PutBlockParent — a registry gate must not
		// hold a second, weaker copy of that verdict.
		return nil
	}
	// The prose is unchanged since T02-11 and byte-identical on every arm (the
	// arm-parity probe in mcp_store_gatechain_integration_test.go asserts the
	// EQUALITY between arms, not only the class). On the one arm that grew a
	// parent field, "this write surface carries no parent" now reads as "this
	// write carries none"; the accurate remedy for that arm — name parent_id — is
	// published in docs/api.md rather than re-worded into a message four other
	// arms share.
	return classParentRequired.reject(fmt.Sprintf(
		"type: %q requires a parent (parent.mode=required) — this write surface carries no parent, "+
			"so a block of that type is created through its own domain path, not claimed here", name))
}

// claimReject runs the four gates that decide what a client may CLAIM about a
// block it writes: the category it occupies (I7/S2), the provenance key it
// carries in its own metadata (I7/S3, second half), the type it names (I7/S1
// plus the WF T10 registry check) and the parent that type's policy demands
// (T02-11). nil = admissible.
//
// ONE function and ONE order — category, metadata, type, parent — called at the
// same position by every write surface, because the gates only add up to an
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
//
// parentID follows the same empty-value convention as the other three: "" means
// "this write names no parent", which is the literal truth on every surface but
// REST /api/store. Passing it explicitly is what keeps that fact at the call
// site instead of inside the gate.
func claimReject(set *blocktype.Set, category, typeName, parentID string, metadata map[string]any) *writeReject {
	if rej := reservedCategoryReject(category); rej != nil {
		return rej
	}
	if rej := reservedMetadataReject(metadata); rej != nil {
		return rej
	}
	if typeName == "" {
		return nil
	}
	if rej := validateTypeNameAgainstSet(set, typeName); rej != nil {
		return rej
	}
	return parentRequiredReject(set, typeName, parentID)
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

// blockIdentityTakenMsg is the one wording of the issue #227 refusal. REST
// manage update, the MCP update tool and both confirm surfaces carry it
// verbatim; a client may match on it as well as on the 409 / "constraint".
const blockIdentityTakenMsg = "another active block in this scope already has this category and title"

// identityTakenReject maps store.BlockIdentityTakenError onto the write
// surfaces' verdict vocabulary: 409 of the constraint class, the class every
// surface that answers a uniqueness collision already uses (blobConstraintError,
// the model-alias name). nil = err is not that refusal. It is the single place
// that binds the error to a status and a wording, so the update surfaces cannot
// answer the same collision two ways.
//
// The holder's id is named when the store found it. That discloses nothing the
// caller cannot read: the store reads the holder within the caller's write
// scopes, every write scope is also a read scope of the same key
// (writableBlockScopes), and GetBlock returns an active block of a read scope
// to `get` whatever its type, sensitivity or provenance.
func identityTakenReject(err error) *writeReject {
	var taken *store.BlockIdentityTakenError
	if !errors.As(err, &taken) {
		return nil
	}
	msg := blockIdentityTakenMsg
	if taken.ConflictID != "" {
		msg += " (block " + taken.ConflictID + ")"
	}
	return classConstraint.rejectStatus(http.StatusConflict, msg+" — choose another category or title")
}
