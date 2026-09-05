// F6-C6 D-W6c: the ONE confirm sequence, shared by every confirm surface.
// D-W5 built it for MCP (mcp_confirm.go), D-W6b copied it for HTTP
// (confirm.go) with an explicit consolidation note — this file redeems that
// note: lookup → payload validation → D1-M1 scope re-check → D1-M3 TOCTOU
// pin re-check (updates) → atomic consume → execute, in exactly one place.
// The surfaces keep their own MESSAGE formatting (MCP surfaces raw errors,
// HTTP launders them to "internal error") — the core returns a typed outcome
// and never a user-facing string for the cases whose wording differs.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// confirmKind classifies the outcome of one confirm attempt.
type confirmKind int

const (
	// confirmOK — the staged write executed; Outcome.Block/Op are set.
	confirmOK confirmKind = iota
	// confirmMiss — the four indistinguishable miss cases (unknown hash,
	// expired, consumed, foreign key) plus a tampered row (op/scope mismatch).
	confirmMiss
	// confirmScopeRejected — D1-M1: the staged scope is no longer writable.
	// Rejects on the UN-consumed row; Outcome.Scope carries the scope.
	confirmScopeRejected
	// confirmTOCTOUGone — D1-M3: the update target vanished from the writable
	// scopes. Rejects on the un-consumed row.
	confirmTOCTOUGone
	// confirmTOCTOUDrift — D1-M3: the block changed since staging
	// (lost-update protection). Rejects on the un-consumed row;
	// Outcome.BlockID carries the target id.
	confirmTOCTOUDrift
	// confirmBlockRefGone — W02-10 A1: the referenced context_block_id is no
	// longer visible in the key's read scopes (archived, deleted, or a read
	// right shrunk since staging). Rejects on the UN-consumed row, like its
	// D1-M1/D1-M3 neighbours; Outcome.BlockID carries the referenced block.
	confirmBlockRefGone
	// confirmClaimRejected — W01-2a Nachbesserung: the staged payload claims a
	// derived type, a reserved category or the provenance key. Rejects on the
	// UN-consumed row like its D1-M1/D1-M3 neighbours; Outcome.Reject carries
	// the class, so the surfaces answer the same status and the same code they
	// would have answered at stage time.
	confirmClaimRejected
	// confirmUnreadable — the server-held payload failed to unmarshal.
	confirmUnreadable
	// confirmInfraErr — lookup/TOCTOU-read/consume infrastructure error
	// BEFORE the token was consumed; Outcome.Err is set.
	confirmInfraErr
	// confirmExecErr — the token IS consumed and the execute failed
	// (rejected finding D1-m2: the client re-stages); Outcome.Err set,
	// Outcome.Op says which wording ('store'/'update') applies.
	confirmExecErr
	// confirmExecGone — token consumed, update target vanished between the
	// TOCTOU read and the execute (same D1-m2 behaviour sentence).
	confirmExecGone
)

// confirmOutcome is the typed result of executeConfirm.
type confirmOutcome struct {
	Kind    confirmKind
	Op      string          // 'store' | 'update' | 'blob_store' | 'blob_link' (from the staged payload once known)
	Block   *store.Block    // on confirmOK for a block op
	Blob    *store.BlobMeta // on confirmOK for op 'blob_store' (W02-8)
	Scope   string          // on confirmScopeRejected
	BlockID string          // on confirmTOCTOUDrift (the update target) / confirmBlockRefGone (the referenced block)
	Err     error           // on confirmInfraErr / confirmExecErr
	Reject  *writeReject    // on confirmClaimRejected
}

// confirmClaimReject re-runs the I7 claim gates over a staged payload. nil =
// admissible, and nil for the blob ops, which carry no block claim.
//
// Split out of executeConfirm only to keep that function under the cyclop
// budget; the placement (before the consume) and the reasoning live at the
// call site.
func confirmClaimReject(ctx context.Context, blocktypes *blocktype.Registry, cw store.CanonicalWrite) *writeReject {
	if cw.Op != "store" && cw.Op != "update" {
		return nil
	}
	var set *blocktype.Set
	if blocktypes != nil {
		set = blocktypes.SnapshotForRequest(ctx)
	}
	return claimReject(set, cw.Category, cw.Type, cw.Metadata)
}

// executeConfirm runs the complete confirm sequence for one payload hash,
// strictly per-key (D1-M4: lookup and consume are bound to ar.ApiKeyID).
// Every reject before the consume leaves the stage token intact (D1-M1/D1-M3
// pattern); the consume is the atomic replay guard; the execute mirrors the
// direct paths byte-for-byte (store: upsert + classify + temporal; update:
// UpdateBlock + finishBlockUpdate).
func executeConfirm(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, ar *auth.AuthResult, payloadHash string) confirmOutcome {
	pw, err := store.LookupPendingWrite(ctx, pool, ar.ApiKeyID, payloadHash)
	if errors.Is(err, store.ErrPendingWriteNotFound) {
		return confirmOutcome{Kind: confirmMiss}
	}
	if err != nil {
		return confirmOutcome{Kind: confirmInfraErr, Err: err}
	}

	var cw store.CanonicalWrite
	if err := json.Unmarshal(pw.Payload, &cw); err != nil {
		slog.Error("confirm: staged payload unmarshal failed", "error", err, "pending_id", pw.ID)
		return confirmOutcome{Kind: confirmUnreadable}
	}
	if !confirmOpKnown(cw.Op) || cw.Scope != pw.Scope {
		// A row/payload scope or op mismatch would mean the stage row was
		// tampered with — indistinguishable from a miss on purpose.
		return confirmOutcome{Kind: confirmMiss}
	}

	// D1-M1: re-validate against the CURRENT key rights, on the un-consumed
	// row — a shrunk right rejects without burning the stage token.
	if !contains(writableBlockScopes(ar), pw.Scope) {
		return confirmOutcome{Kind: confirmScopeRejected, Op: cw.Op, Scope: pw.Scope}
	}

	// W02-10 A1, re-validated exactly like D1-M1 above and on the same
	// un-consumed row: a staged write may carry a reference to a context block,
	// and the right to SEE that block can shrink between staging and confirm.
	// The card promised an edge to a block this key could read; if it no longer
	// can, the edge must not be written, and the stage survives to expire.
	if cw.ContextBlockID != "" {
		visible, err := store.BlockVisible(ctx, pool, cw.ContextBlockID, ar.ReadScopes)
		if err != nil {
			return confirmOutcome{Kind: confirmInfraErr, Op: cw.Op, Err: err}
		}
		if !visible {
			return confirmOutcome{Kind: confirmBlockRefGone, Op: cw.Op, BlockID: cw.ContextBlockID}
		}
	}

	// I7 claim gates, re-validated on the un-consumed row (W01-2a
	// Nachbesserung, review finding #4 — major). The gates ran at STAGE time
	// only, and a card outlives its staging: `writes.confirm_ttl` defaults to
	// 600 s and is documented as "0 = never expires", so every card issued
	// before this wave deployed would have executed after it — a block in a
	// reserved category, a block with type_source='manual' on a derived type,
	// the full B14 outcome the wave exists to prevent. The same holds whenever
	// the reservation list itself moves (D-02's distill.category
	// reconciliation): old cards must not execute under the old list.
	//
	// Placed with D1-M1/D1-M3, i.e. BEFORE the consume: a claim rejection is
	// the server refusing, not the client spending — the token survives, and a
	// client that fixes the payload re-stages instead of losing the write.
	//
	// For op 'update' the empty-value convention does the field selection: a
	// field the card does not carry is "" / nil and claimReject reads that as
	// "not part of this write". The blob ops carry no block claim at all.
	if rej := confirmClaimReject(ctx, blocktypes, cw); rej != nil {
		return confirmOutcome{Kind: confirmClaimRejected, Op: cw.Op, Reject: rej}
	}

	// D1-M3 TOCTOU guard for staged updates: the confirm executes only
	// against the exact block state the card was rendered for. Also on the
	// un-consumed row — drift rejects, the client re-reads and re-stages.
	if cw.Op == "update" {
		// nil type set (V-11): this read is a TOCTOU fingerprint, not a read
		// ANSWER — the block never reaches a consumer, so it carries no framing.
		base, err := store.GetBlock(ctx, pool, nil, cw.ID, writableBlockScopes(ar), nil)
		if err != nil {
			return confirmOutcome{Kind: confirmInfraErr, Op: cw.Op, Err: err}
		}
		if base == nil {
			return confirmOutcome{Kind: confirmTOCTOUGone, Op: cw.Op, BlockID: cw.ID}
		}
		if base.UpdatedAt.UTC().Format(time.RFC3339Nano) != cw.BaseUpdatedAt {
			return confirmOutcome{Kind: confirmTOCTOUDrift, Op: cw.Op, BlockID: cw.ID}
		}
	}

	// Atomic consume (exactly once) — a racing double-confirm loses here and
	// gets the generic miss.
	if _, err := store.ConsumePendingWrite(ctx, pool, ar.ApiKeyID, payloadHash); err != nil {
		if errors.Is(err, store.ErrPendingWriteNotFound) {
			return confirmOutcome{Kind: confirmMiss}
		}
		return confirmOutcome{Kind: confirmInfraErr, Op: cw.Op, Err: err}
	}

	if cw.Op == store.OpBlobStore || cw.Op == store.OpBlobLink {
		return executeConfirmedBlobOp(ctx, pool, ar, cw, pw.ID)
	}

	if cw.Op == "update" {
		data := updateDataFromCanonical(cw)
		block, needsReEmbed, err := store.UpdateBlock(ctx, pool, cw.ID, data, writableBlockScopes(ar))
		if err != nil {
			slog.Error("confirm: confirmed update execute failed", "error", err, "pending_id", pw.ID)
			return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
		}
		if block == nil {
			// Vanished between the TOCTOU read and the execute — token is
			// consumed (rejected finding D1-m2 behaviour sentence).
			return confirmOutcome{Kind: confirmExecGone, Op: cw.Op, BlockID: cw.ID}
		}
		finishBlockUpdate(ctx, pool, blocktypes, data, block, needsReEmbed, RequestIDFromContext(ctx))
		return confirmOutcome{Kind: confirmOK, Op: cw.Op, Block: block}
	}

	// op 'store': execute over the SAME path as the direct store tools
	// (upsert + classify + temporal). cw.Scope is the scope the stage gates
	// RESOLVED — the key's home scope, or, since E-M4, the explicit scope the
	// staging call named and passed the gate with — re-validated above against
	// the key's current rights.
	//
	// scopeExplicit stays false, and E-M4 does not change that: the flag only
	// adds `scope = EXCLUDED.scope` to the ON CONFLICT UPDATE, whose conflict
	// target is (category, title, scope) — the conflicting row therefore
	// already carries this scope, and the assignment can move nothing. A
	// staged write lands in cw.Scope either way.
	sens := store.SensitivityWrite{
		Value:    backends.Sensitivity(cw.Sensitivity),
		Manual:   cw.SensitivityManual,
		Detector: cw.SensitivityDetect,
	}
	block, err := store.UpsertBlock(ctx, pool, cw.Category, cw.Title, cw.Content, cw.Tags, cw.Metadata, cw.Scope, false, sens, cw.Type)
	if err != nil {
		slog.Error("confirm: confirmed write execute failed", "error", err, "pending_id", pw.ID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}

	var classifySet *blocktype.Set
	if blocktypes != nil {
		classifySet = blocktypes.SnapshotForRequest(ctx)
	}
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, classifySet, block.ID, block.Title, block.Metadata); err != nil {
		slog.Warn("confirm: auto-classify failed", "error", err, "block_id", block.ID)
	}
	times := store.ExtractDates(block.Content)
	_ = store.UpdateContentTimes(ctx, pool, block.ID, times)
	_ = store.PopulateTemporal(ctx, pool, block.ID, times, block.CreatedAt)

	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Block: block}
}

// confirmOpKnown is the op whitelist of the staged-write surface. An op the
// execute cannot run must never reach the consume: a row carrying one is either
// tampered with or written by a version this binary is not, and both collapse
// into the generic miss (no oracle).
func confirmOpKnown(op string) bool {
	switch op {
	case "store", "update", store.OpBlobStore, store.OpBlobLink:
		return true
	default:
		return false
	}
}

// executeConfirmedBlobOp runs the two blob ops AFTER the consume.
//
// No booking on either arm — the budget was charged at stage time
// (mcpStageBlobStore / mcpStageBlobLink), exactly as it is for a staged block
// write. No classify and no temporal enrichment either: a blob is not a
// retrieval source. The scope was re-validated by the caller through the same
// writableBlockScopes formula a direct write runs, so a right shrunk after
// staging rejects there as it does for a block.
//
// op 'blob_link' (W02-10) re-runs the DIRECT path's statement, scope filter
// included: cw.ID is the blob, cw.ContextBlockID the block whose visibility was
// re-checked before the consume. A blob that left the writable scopes in the
// meantime answers nil and lands in confirmExecGone — the token is spent, the
// client re-stages (rejected finding D1-m2).
func executeConfirmedBlobOp(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult, cw store.CanonicalWrite, pendingID string) confirmOutcome {
	if cw.Op == store.OpBlobLink {
		blob, err := store.UpdateBlobBlockRef(ctx, pool, cw.ID, cw.ContextBlockID, writableBlockScopes(ar))
		if err != nil {
			slog.Error("confirm: confirmed blob link execute failed", "error", err, "pending_id", pendingID)
			return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
		}
		if blob == nil {
			return confirmOutcome{Kind: confirmExecGone, Op: cw.Op}
		}
		return confirmOutcome{Kind: confirmOK, Op: cw.Op, Blob: blob}
	}

	blob, err := store.UpsertBlob(ctx, pool, cw.Category, cw.Title, cw.Filename, cw.MimeType, cw.Scope, cw.Data, cw.Tags, cw.Metadata, cw.ContextBlockID)
	if err != nil {
		slog.Error("confirm: confirmed blob write execute failed", "error", err, "pending_id", pendingID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}
	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Blob: blob}
}

// confirmBlockRefGoneMsg is the W02-10 A1 reject wording, shared by both
// confirm surfaces. It names neither the block nor whether it ever existed —
// the same reason blobBlockRefNotFoundMsg does not.
const confirmBlockRefGoneMsg = "confirm rejected: the referenced context_block_id is no longer visible for this key — the staged write stays pending until it expires"

// confirmScopeRejectMsg is the D1-M1 reject wording — identical on every
// surface (MCP text and HTTP body agree since D-W5/D-W6b).
func confirmScopeRejectMsg(scope string) string {
	return fmt.Sprintf("confirm rejected: scope %q is no longer writable for this key — the staged write stays pending until it expires", scope)
}

// confirmTOCTOUGoneMsg / confirmTOCTOUDriftMsg are the D1-M3 reject wordings
// (verbatim the D-W6a texts — both surfaces share them).
const confirmTOCTOUGoneMsg = "confirm rejected: the target block no longer exists in a writable scope — the staged update stays pending until it expires"

func confirmTOCTOUDriftMsg(blockID string) string {
	return fmt.Sprintf(
		"confirm rejected: block %s changed since this update was staged (lost-update protection) — re-read the block and stage the update again; the stale stage expires on its own",
		blockID)
}
