// F6-C6 D-W6c: the ONE confirm sequence, shared by every confirm surface.
// D-W5 built it for MCP (mcp_confirm.go), D-W6b copied it for HTTP
// (confirm.go) with an explicit consolidation note — this file redeems that
// note: lookup → payload validation → D1-M1 scope re-check → D1-M3 TOCTOU
// pin re-check (updates, archives) → atomic consume → execute, in exactly one place.
// The surfaces keep their own MESSAGE formatting (a server fault stays opaque
// on both since issue #275) — the core returns a typed outcome
// and never a user-facing string for the cases whose wording differs.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	// confirmTOCTOUGone — D1-M3: the update or archive target vanished from
	// the writable scopes. Rejects on the un-consumed row.
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
	// Outcome.Op says which wording ('store'/'update'/'archive') applies.
	confirmExecErr
	// confirmExecGone — token consumed, update or archive target vanished
	// between the TOCTOU read and the execute (same D1-m2 behaviour sentence).
	confirmExecGone
	// confirmExecRejected — the token IS consumed and the execute was refused
	// for a cause in the payload, not for a server fault: a staged update that
	// would give the block the identity of another active block (issue #227);
	// since #275 also a staged store onto a derivative's identity and a staged
	// blob_store that violates an integrity constraint.
	// Outcome.Reject carries the class — the 409 the direct update answers —
	// with a prefix that says the token is spent. The unique index decides the
	// collision atomically, and only the statement after the consume consults
	// it; a probe before the consume would keep the token for most
	// collisions but not for one that lands between probe and statement, and
	// the remedy (another category or title) is a new payload either way.
	confirmExecRejected
	// confirmExecDrift — the token IS consumed and the execute found the
	// block at another updated_at than the card bound (issue #406): a writer
	// whose transaction was still open when the drift check read the block
	// committed before the statement could write. The statement carries the
	// card's base as a pin (store.UpdateBlockData.ExpectUpdatedAt) and wrote
	// nothing; Outcome.BlockID carries the target id.
	confirmExecDrift
)

// confirmOutcome is the typed result of executeConfirm.
type confirmOutcome struct {
	Kind    confirmKind
	Op      string          // 'store' | 'update' | 'blob_store' | 'blob_link' | 'archive' (from the staged payload once known)
	Block   *store.Block    // on confirmOK for a block op
	Blob    *store.BlobMeta // on confirmOK for op 'blob_store' (W02-8)
	Scope   string          // on confirmScopeRejected
	BlockID string          // on confirmTOCTOUDrift (the update/archive target) / confirmBlockRefGone (the referenced block)
	Err     error           // on confirmInfraErr / confirmExecErr
	Reject  *writeReject    // on confirmClaimRejected / confirmExecRejected
	// Supersedes is the block a confirmed op 'store' declared to supersede
	// (issue #197), "" otherwise.
	Supersedes string
	// Batch is the outcome of a confirmed op 'store_batch' (issue #211), one
	// entry per card item in the card's order: the written block, or the
	// block an item that changed nothing matches.
	Batch []storeBatchItem
	// Item names the card item of a staged batch whose scope a
	// confirmScopeRejected refuses (batchItemPrefix), "" for every other card.
	Item string
}

// confirmIdentityTakenPrefix opens the confirmExecRejected wording of a staged
// update the write refused, on both confirm surfaces: the reason follows
// verbatim from updateFailureReject — an identity collision (#227), a
// derivative (I7/S3) or a re-type of a comment or an issue (#280). Each is decided by the statement on the row as it is then, so
// the token is consumed.
const confirmIdentityTakenPrefix = "confirmed update not executed, the stage token is consumed: "

// confirmSupersedesPrefix opens the confirmExecRejected wording of a staged
// store whose supersedes declaration the execute refused (issue #197): the
// target changed between stage and confirm. The reason follows verbatim from
// supersedesReject (or retypeBoundReject: an explicit type on the conflict row
// of a comment or an issue, #280; or the S3 provenance refusal: the identity
// belongs to a derivative, which the stage gates do not probe). Like #227, the gate
// runs on the statement after the consume — a probe before it would keep the
// token for most changes but not for one that lands in between.
const confirmSupersedesPrefix = "confirmed store not executed, the stage token is consumed: "

// confirmBlobStorePrefix opens the confirmExecRejected wording of a staged
// blob_store whose execute hit an integrity constraint (blobConstraintError).
const confirmBlobStorePrefix = "confirmed blob_store not executed, the stage token is consumed: "

// confirmScopeReject is the D1-M1 re-check: the staged scope against the
// key's CURRENT writable scopes, nil while the key may still write it. A
// staged batch (#211) is checked item by item first, in the card's order, and
// the first item whose scope is no longer writable is named
// (batchItemPrefix), as every other refusal of a batch names its item; the
// row's scope is one of the items' (the smallest in sort order,
// mcpStageStoreBatch). Every item's scope is checked before any item's claims
// (confirmBatchReject), as a single card's scope is before its claims.
//
// Split out of executeConfirm to keep that function under the cyclop budget;
// the placement (before the consume) lives at the call site.
func confirmScopeReject(ar *auth.AuthResult, cw store.CanonicalWrite, rowScope string) *confirmOutcome {
	writable := writableBlockScopes(ar)
	if cw.Op == store.OpStoreBatch {
		for i, it := range cw.Items {
			if !contains(writable, it.Scope) {
				return &confirmOutcome{Kind: confirmScopeRejected, Op: cw.Op, Scope: it.Scope, Item: batchItemPrefix(i)}
			}
		}
	}
	if !contains(writable, rowScope) {
		return &confirmOutcome{Kind: confirmScopeRejected, Op: cw.Op, Scope: rowScope}
	}
	return nil
}

// confirmClaimReject re-runs the I7 claim gates over a staged payload. nil =
// admissible, and nil for the blob ops, which carry no block claim.
//
// Split out of executeConfirm only to keep that function under the cyclop
// budget; the placement (before the consume) and the reasoning live at the
// call site.
//
// The empty parent argument is not an omission: a staged card is built from the
// MCP/chat write shapes, and neither carries a parent — the one surface that
// does (REST /api/store, NZ-3) is a direct write path and never stages. A
// required-parent type therefore stays unconfirmable, which is the same verdict
// the stage itself would have given.
func confirmClaimReject(ctx context.Context, blocktypes *blocktype.Registry, cw store.CanonicalWrite) *writeReject {
	if cw.Op != "store" && cw.Op != "update" {
		return nil
	}
	var set *blocktype.Set
	if blocktypes != nil {
		set = blocktypes.SnapshotForRequest(ctx)
	}
	return claimReject(set, cw.Category, cw.Type, "", updateMetadataClaim(cw.Metadata, cw.MetadataMerge))
}

// confirmDriftReject is the D1-M3 re-check of the by-id block ops ('update',
// and 'archive' since #201): nil = the target is still in a writable scope, at
// the updated_at the card bound. Any other op has no target block and passes.
//
// Split out of executeConfirm only to keep that function under the cyclop
// budget, like confirmClaimReject; the placement (before the consume) and the
// reasoning live at the call site.
func confirmDriftReject(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult, cw store.CanonicalWrite) *confirmOutcome {
	if cw.Op != "update" && cw.Op != store.OpArchive {
		return nil
	}
	// nil type set (V-11): this read is a TOCTOU fingerprint, not a read
	// ANSWER — the block never reaches a consumer, so it carries no framing.
	base, err := store.GetBlock(ctx, pool, nil, cw.ID, writableBlockScopes(ar), nil)
	if err != nil {
		return &confirmOutcome{Kind: confirmInfraErr, Op: cw.Op, Err: err}
	}
	if base == nil {
		return &confirmOutcome{Kind: confirmTOCTOUGone, Op: cw.Op, BlockID: cw.ID}
	}
	if base.UpdatedAt.UTC().Format(time.RFC3339Nano) != cw.BaseUpdatedAt {
		return &confirmOutcome{Kind: confirmTOCTOUDrift, Op: cw.Op, BlockID: cw.ID}
	}
	return nil
}

// executeConfirm runs the complete confirm sequence for one payload hash,
// strictly per-key (D1-M4: lookup and consume are bound to ar.ApiKeyID).
// Every reject before the consume leaves the stage token intact (D1-M1/D1-M3
// pattern); the consume is the atomic replay guard; the execute mirrors the
// direct paths byte-for-byte (store: upsert + classify + temporal; update:
// UpdateBlock + finishBlockUpdate; archive: DeleteBlock).
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
	if out := confirmScopeReject(ar, cw, pw.Scope); out != nil {
		return *out
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

	// D1-M3 TOCTOU guard for staged updates and archives (#201): the confirm
	// executes only against the exact block state the card was rendered for.
	// Also on the un-consumed row — drift rejects, the client re-reads and
	// re-stages.
	if out := confirmDriftReject(ctx, pool, ar, cw); out != nil {
		return *out
	}
	// A staged batch (#211): the claim re-check above, for each of its items
	// (their scopes: confirmScopeReject) — on the un-consumed row, like theirs.
	if out := confirmBatchReject(ctx, blocktypes, cw); out != nil {
		return *out
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
	if cw.Op == store.OpArchive {
		return executeConfirmedArchive(ctx, pool, ar, cw, pw.ID)
	}
	if cw.Op == store.OpStoreBatch {
		return executeConfirmedStoreBatch(ctx, pool, blocktypes, ar, cw, pw.ID)
	}

	if cw.Op == "update" {
		data := updateDataFromCanonical(cw)
		// The drift check above read without a lock; the pin makes the
		// statement itself refuse a row that moved since (issue #406), so a
		// staged content — an edit list resolved at stage time included — never
		// overwrites a change it did not see.
		noteToolBlock(ctx, cw.ID)
		base, err := time.Parse(time.RFC3339Nano, cw.BaseUpdatedAt)
		if err != nil {
			noteToolWrite(ctx, err)
			slog.Error("confirm: staged update base unreadable", "error", err, "pending_id", pw.ID)
			return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
		}
		data.ExpectUpdatedAt = &base
		block, _, err := store.UpdateBlock(ctx, pool, cw.ID, data, writableBlockScopes(ar))
		if errors.Is(err, store.ErrBlockChanged) {
			return confirmOutcome{Kind: confirmExecDrift, Op: cw.Op, BlockID: cw.ID}
		}
		if rej := updateFailureReject(err); rej != nil {
			// The caller's input, not a fault: no ERROR line (#227, #280).
			return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op, Reject: rej.prefixed(confirmIdentityTakenPrefix)}
		}
		if err != nil {
			noteToolWrite(ctx, err)
			logRequestError(ctx, "confirm: confirmed update execute failed", "error", err, "write_outcome", writeOutcome(err), "pending_id", pw.ID)
			return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
		}
		if block == nil {
			// Vanished between the TOCTOU read and the execute — token is
			// consumed (rejected finding D1-m2 behaviour sentence).
			return confirmOutcome{Kind: confirmExecGone, Op: cw.Op, BlockID: cw.ID}
		}
		// Committed: noted before the afterwork, so a fault there leaves the
		// MCP line saying what was written (issue #483).
		noteToolWrite(ctx, nil)
		finishBlockUpdate(ctx, pool, blocktypes, data, block, RequestIDFromContext(ctx))
		return confirmOutcome{Kind: confirmOK, Op: cw.Op, Block: block}
	}

	return executeConfirmedStore(ctx, pool, blocktypes, ar, cw, pw.ID)
}

// executeConfirmedStore runs op 'store' AFTER the consume. Split out of
// executeConfirm to keep that function under the cyclop budget once the
// supersedes refusal (issue #197) and the type-trust re-check (#203) sat in
// it side by side; the order of the refusals is unchanged.
func executeConfirmedStore(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, ar *auth.AuthResult, cw store.CanonicalWrite, pendingID string) confirmOutcome {
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
	var classifySet *blocktype.Set
	if blocktypes != nil {
		classifySet = blocktypes.SnapshotForRequest(ctx)
	}
	block, err := upsertStoreBlock(ctx, pool, classifySet, ar.ApiKeyID, cw.Category, cw.Title, cw.Content, cw.Tags, cw.Metadata,
		cw.Scope, false, sens, cw.Type, cw.Supersedes)
	if rej := supersedesReject(err); rej != nil {
		return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op, Reject: rej.prefixed(confirmSupersedesPrefix)}
	}
	if rej := retypeBoundReject(err); rej != nil {
		return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op, Reject: rej.prefixed(confirmSupersedesPrefix)}
	}
	if rej := provenanceRejectOr(err, nil); rej != nil {
		// The answer the direct store gives (provenance_protected): the caller's
		// payload, not a server fault (issue #275).
		return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op, Reject: rej.prefixed(confirmSupersedesPrefix)}
	}
	if err != nil {
		noteToolWrite(ctx, err)
		logRequestError(ctx, "confirm: confirmed write execute failed", "error", err, "write_outcome", writeOutcome(err), "pending_id", pendingID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}
	noteToolWrite(ctx, nil)
	noteToolBlock(ctx, block.ID)

	// The confirmed write is committed: its afterwork runs detached from the
	// client (issue #453).
	ctx, cancel := followUpContext(ctx)
	defer cancel()
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, classifySet, block.ID, block.Title, block.Metadata); err != nil {
		slog.Warn("confirm: auto-classify failed", "error", err, "block_id", block.ID)
	}
	_, _ = store.RefreshTemporal(ctx, pool, block.ID, block.Content, block.CreatedAt)

	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Block: block, Supersedes: cw.Supersedes}
}

// confirmOpKnown is the op whitelist of the staged-write surface. An op the
// execute cannot run must never reach the consume: a row carrying one is either
// tampered with or written by a version this binary is not, and both collapse
// into the generic miss (no oracle).
func confirmOpKnown(op string) bool {
	switch op {
	case "store", "update", store.OpBlobStore, store.OpBlobLink, store.OpArchive, store.OpStoreBatch:
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
		noteToolBlob(ctx, cw.ID)
		blob, err := store.UpdateBlobBlockRef(ctx, pool, cw.ID, cw.ContextBlockID, writableBlockScopes(ar))
		if err != nil {
			noteToolStatement(ctx, err)
			logRequestError(ctx, "confirm: confirmed blob link execute failed", "error", err, "write_outcome", statementOutcome(err), "pending_id", pendingID)
			return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
		}
		if blob == nil {
			return confirmOutcome{Kind: confirmExecGone, Op: cw.Op}
		}
		noteToolWrite(ctx, nil)
		noteToolBlob(ctx, blob.ID)
		return confirmOutcome{Kind: confirmOK, Op: cw.Op, Blob: blob}
	}

	blob, err := store.UpsertBlob(ctx, pool, cw.Category, cw.Title, cw.Filename, cw.MimeType, cw.Scope, cw.Data, cw.Tags, cw.Metadata, cw.ContextBlockID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23") {
		// An integrity-constraint violation is the request's, answered as the
		// direct blob_store answers it (blobConstraintError), not as a server
		// fault (issue #275).
		status, reason := blobConstraintError(pgErr)
		return confirmOutcome{Kind: confirmExecRejected, Op: cw.Op,
			Reject: classConstraint.rejectStatus(status, reason).prefixed(confirmBlobStorePrefix)}
	}
	if err != nil {
		noteToolStatement(ctx, err)
		logRequestError(ctx, "confirm: confirmed blob write execute failed", "error", err, "write_outcome", statementOutcome(err), "pending_id", pendingID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}
	noteToolWrite(ctx, nil)
	noteToolBlob(ctx, blob.ID)
	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Blob: blob}
}

// executeConfirmedArchive runs op 'archive' (#201) AFTER the consume, over the
// statement the direct archive tool and REST manage delete run: DeleteBlock
// within the key's CURRENT write scopes, the provenance exclusion in its WHERE.
// The drift pin was re-checked before the consume; a block that left the
// writable scopes in between answers nil and lands in confirmExecGone — the
// token is spent, the client re-stages (rejected finding D1-m2). No booking:
// the budget was charged at stage time, as for every staged op.
func executeConfirmedArchive(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult, cw store.CanonicalWrite, pendingID string) confirmOutcome {
	noteToolBlock(ctx, cw.ID)
	block, err := store.DeleteBlock(ctx, pool, cw.ID, writableBlockScopes(ar))
	if err != nil {
		noteToolWrite(ctx, err)
		logRequestError(ctx, "confirm: confirmed archive execute failed", "error", err, "write_outcome", writeOutcome(err), "pending_id", pendingID)
		return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
	}
	if block == nil {
		return confirmOutcome{Kind: confirmExecGone, Op: cw.Op, BlockID: cw.ID}
	}
	noteToolWrite(ctx, nil)
	return confirmOutcome{Kind: confirmOK, Op: cw.Op, Block: block}
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

// scopeRejectMsg is a confirmScopeRejected outcome in the D1-M1 wording, the
// refused item of a staged batch named in front of it: the text both confirm
// surfaces answer.
func (o confirmOutcome) scopeRejectMsg() string {
	return o.Item + confirmScopeRejectMsg(o.Scope)
}

// confirmBlockOpNoun names the staged by-id block op in the wordings both
// confirm surfaces share: an archive (#201) names itself, every other op keeps
// the verbatim D-W6a "update" texts.
func confirmBlockOpNoun(op string) string {
	if op == store.OpArchive {
		return "archive"
	}
	return "update"
}

// confirmTOCTOUGoneMsg / confirmTOCTOUDriftMsg are the D1-M3 reject wordings
// (for op 'update' verbatim the D-W6a texts — both surfaces share them).
func confirmTOCTOUGoneMsg(op string) string {
	return fmt.Sprintf(
		"confirm rejected: the target block no longer exists in a writable scope — the staged %s stays pending until it expires",
		confirmBlockOpNoun(op))
}

// driftMsg is the wording of a drift refusal: before the consume
// (confirmTOCTOUDrift) the stage stays, after it (confirmExecDrift, issue
// #406) the message says the token is spent. Both surfaces answer it with the
// status the drift has always had (409 on REST).
func (o confirmOutcome) driftMsg() string {
	if o.Kind == confirmExecDrift {
		return confirmIdentityTakenPrefix + fmt.Sprintf(
			"block %s changed while this update was confirmed (lost-update protection) — re-read the block and stage the update again", o.BlockID)
	}
	return confirmTOCTOUDriftMsg(o.Op, o.BlockID)
}

func confirmTOCTOUDriftMsg(op, blockID string) string {
	noun := confirmBlockOpNoun(op)
	return fmt.Sprintf(
		"confirm rejected: block %s changed since this %s was staged (lost-update protection) — re-read the block and stage the %s again; the stale stage expires on its own",
		blockID, noun, noun)
}
