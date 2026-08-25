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
	Op      string          // 'store' | 'update' | 'blob_store' (from the staged payload once known)
	Block   *store.Block    // on confirmOK for a block op
	Blob    *store.BlobMeta // on confirmOK for op 'blob_store' (W02-8)
	Scope   string          // on confirmScopeRejected
	BlockID string          // on confirmTOCTOUDrift (the update target)
	Err     error           // on confirmInfraErr / confirmExecErr
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
	if (cw.Op != "store" && cw.Op != "update" && cw.Op != store.OpBlobStore) || cw.Scope != pw.Scope {
		// A row/payload scope or op mismatch would mean the stage row was
		// tampered with — indistinguishable from a miss on purpose.
		return confirmOutcome{Kind: confirmMiss}
	}

	// D1-M1: re-validate against the CURRENT key rights, on the un-consumed
	// row — a shrunk right rejects without burning the stage token.
	if !contains(writableBlockScopes(ar), pw.Scope) {
		return confirmOutcome{Kind: confirmScopeRejected, Op: cw.Op, Scope: pw.Scope}
	}

	// D1-M3 TOCTOU guard for staged updates: the confirm executes only
	// against the exact block state the card was rendered for. Also on the
	// un-consumed row — drift rejects, the client re-reads and re-stages.
	if cw.Op == "update" {
		base, err := store.GetBlock(ctx, pool, cw.ID, writableBlockScopes(ar), nil)
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

	// op 'blob_store' (W02-8): the upsert, and nothing else. No booking — the
	// budget was charged at stage time (mcpStageBlobStore), exactly as it is
	// for a staged block write; no classify, no temporal enrichment, because a
	// blob is not a retrieval source. The scope was re-validated above through
	// the same writableBlockScopes formula, so a right shrunk after staging
	// rejects here as it does for a block.
	if cw.Op == store.OpBlobStore {
		blob, err := store.UpsertBlob(ctx, pool, cw.Category, cw.Title, cw.Filename, cw.MimeType, cw.Scope, cw.Data, cw.Tags, cw.Metadata)
		if err != nil {
			slog.Error("confirm: confirmed blob write execute failed", "error", err, "pending_id", pw.ID)
			return confirmOutcome{Kind: confirmExecErr, Op: cw.Op, Err: err}
		}
		return confirmOutcome{Kind: confirmOK, Op: cw.Op, Blob: blob}
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
		finishBlockUpdate(ctx, pool, blocktypes, data, block, needsReEmbed)
		return confirmOutcome{Kind: confirmOK, Op: cw.Op, Block: block}
	}

	// op 'store': execute over the SAME path as the direct store tools
	// (upsert + classify + temporal). scopeExplicit=false mirrors the staging
	// surfaces (no scope input; cw.Scope is the resolved home_scope from
	// stage time, re-validated above).
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
