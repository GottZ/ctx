// ingest.go — Ingest endpoint for bulk block import.
// Part of ctx by GottZ and Damien Moon — The memory your LLM pretends to have.
//
// POST /api/ingest accepts up to 200 chunks per request.
// Each chunk is validated, hash-checked, upserted, embedded, and temporally enriched.
// Write rate limit: own action bucket "ingest", one row per accepted request,
// budgeted against query.rate_limit_write (§9e).
//
// Source: https://github.com/GottZ/ctx
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// maxIngestChunks is the maximum number of chunks per ingest request.
	maxIngestChunks = 200

	// maxContentSize is the maximum content size per chunk (50 KB).
	maxContentSize = 50 * 1024

	// maxCategoryLen is the maximum category length in bytes (len).
	maxCategoryLen = 100

	// maxTitleLen is the maximum title length in bytes (len).
	maxTitleLen = 500
)

// ingestBucket is the access-log action the ingest write rate limit counts.
// One row per accepted REQUEST, not per chunk: a batch carries up to
// maxIngestChunks chunks, and the per-chunk "write" rows feed the /api/store
// budget already — counting chunks here would cut bulk vault ingestion to
// rate_limit_write chunks a minute.
const ingestBucket = "ingest"

// IngestHandler handles POST /api/ingest.
type IngestHandler struct {
	pool       *pgxpool.Pool
	cfg        ConfigStore // nil = no write rate limit (test wiring)
	blocktypes *blocktype.Registry
}

// NewIngestHandler creates a new IngestHandler. The write rate limit comes
// from cfg per request (tenant-overridable query.rate_limit_write); a nil cfg
// disables it. blocktypes feeds the auto-classify hook after a block-mode
// upsert, the same registry /api/store classifies with.
func NewIngestHandler(pool *pgxpool.Pool, cfg ConfigStore, blocktypes *blocktype.Registry) *IngestHandler {
	return &IngestHandler{pool: pool, cfg: cfg, blocktypes: blocktypes}
}

// IngestRequest is the JSON body for POST /api/ingest.
type IngestRequest struct {
	Source string `json:"source"`
	// TargetScope is the scope every chunk of the request is written to
	// (#322), resolved by resolveWriteScope, the gate of POST /api/store and
	// the MCP store tool: absent or empty means the key's home scope, a scope
	// outside the key's writable scopes refuses the whole request (403
	// scope_denied) before a chunk is written.
	//
	// It is not called `scope` because that name is taken on the wire: every
	// `ctx ingest` before #322 sends `scope`, "private" unless the user chose
	// another, and this handler never read it. Honouring that field would
	// refuse the runs of every such CLI whose key has another home scope, or
	// move them into 'private' where the key may write it, so `scope` stays
	// unread (docs/api.md).
	TargetScope string        `json:"target_scope,omitempty"`
	Chunks      []IngestChunk `json:"chunks"`
}

// IngestChunk represents a single chunk to ingest.
type IngestChunk struct {
	Category   string         `json:"category"`
	Title      string         `json:"title"`
	Content    string         `json:"content"`
	Tags       []string       `json:"tags"`
	Metadata   map[string]any `json:"metadata"`
	SourceID   string         `json:"source_id,omitempty"`
	ChunkIndex int            `json:"chunk_index,omitempty"`
}

// IngestResult holds the outcome for a single chunk.
type IngestResult struct {
	Index  int    `json:"index"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// IngestResponse is the JSON response for POST /api/ingest.
type IngestResponse struct {
	Success bool `json:"success"`
	// Scope is the scope the request wrote to (#322): the resolved
	// target_scope, the key's home scope without one. A client reports this,
	// not the scope it asked for; its absence marks a server that ignores
	// target_scope.
	Scope    string         `json:"scope"`
	Total    int            `json:"total"`
	Inserted int            `json:"inserted"`
	Updated  int            `json:"updated"`
	Skipped  int            `json:"skipped"`
	Failed   int            `json:"failed"`
	Results  []IngestResult `json:"results"`
}

// validateIngestRequest validates the ingest request and returns a list of errors.
// Returns nil/empty slice if valid.
func validateIngestRequest(req *IngestRequest) []string {
	var errs []string

	if len(req.Chunks) == 0 {
		errs = append(errs, "chunks is required and must not be empty")
		return errs
	}

	if len(req.Chunks) > maxIngestChunks {
		errs = append(errs, fmt.Sprintf("chunks exceeds maximum of %d", maxIngestChunks))
		return errs
	}

	for i, c := range req.Chunks {
		prefix := fmt.Sprintf("chunk[%d]", i)

		if strings.TrimSpace(c.Category) == "" {
			errs = append(errs, fmt.Sprintf("%s: category is required", prefix))
		} else if len(c.Category) > maxCategoryLen {
			errs = append(errs, fmt.Sprintf("%s: category exceeds %d bytes", prefix, maxCategoryLen))
		}

		if strings.TrimSpace(c.Title) == "" {
			errs = append(errs, fmt.Sprintf("%s: title is required", prefix))
		} else if len(c.Title) > maxTitleLen {
			errs = append(errs, fmt.Sprintf("%s: title exceeds %d bytes", prefix, maxTitleLen))
		}

		if strings.TrimSpace(c.Content) == "" {
			errs = append(errs, fmt.Sprintf("%s: content is required", prefix))
		} else if len(c.Content) > maxContentSize {
			errs = append(errs, fmt.Sprintf("%s: content exceeds 50KB", prefix))
		}
	}

	return errs
}

// HandleIngest processes bulk ingest requests.
func (h *IngestHandler) HandleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Parse body.
	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// A body over the route's cap (handler.MaxBodySize in server.go) is
		// refused as too large, as decodeIssueBody refuses it, not as
		// malformed: a client could not tell the two apart (#323).
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			slog.Warn("ingest: request body over the cap", "limit", tooLarge.Limit, "request_id", reqID)
			writeHouseTooLarge(w, r, tooLarge.Limit)
			return
		}
		slog.Warn("ingest: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Validate.
	if errs := validateIngestRequest(&req); len(errs) > 0 {
		// Use 400 for missing/invalid fields, 413 for size violations.
		status := http.StatusBadRequest
		for _, e := range errs {
			if strings.Contains(e, "exceeds maximum of") {
				status = http.StatusRequestEntityTooLarge
				break
			}
		}
		writeJSON(w, status, map[string]any{
			"success": false, "error": strings.Join(errs, "; "),
		})
		return
	}

	// The write scope (#322), through the scope gate of POST /api/store. It
	// runs before every step that reads the scope: the provenance probe below
	// asks the resolved scope, and asked about a scope the key may not write
	// it would tell whether a derived block exists there. It runs before the
	// write budget too, so a refused scope costs none, as on /api/store.
	writeScope, _, scopeRej := resolveWriteScope(authResult, req.TargetScope)
	if scopeRej != nil {
		writeJSONReject(w, scopeRej)
		return
	}

	// The I7 gates for the batch (design D-01 §4.3.1). Ingest is the write
	// surface with TWO write paths — store.UpsertBlock in block mode and
	// store.InsertChunk in chunk mode, which never touches UpsertBlock at all —
	// so both the claim gates and the S3 identity check run HERE, on the
	// REQUEST, ahead of either.
	//
	// The whole batch is refused rather than the offending chunk: this endpoint
	// reports per-chunk outcomes with a 200, and a partial success would leave
	// the reserved namespace occupied (or a derivative overwritten) while
	// telling the caller the request succeeded. Fail-closed on the batch is the
	// only answer that keeps the reservation true — and it is what makes the
	// documented contract (403 for every I7 refusal, docs/api.md) hold on this
	// endpoint too. The chunk-level S3 fallback in processChunk stays as the
	// backstop for the race between this sweep and the write.
	//
	// Scale: one indexed EXISTS per chunk, bounded by maxIngestChunks = 200
	// (store.ProvenanceClaimed: one descent of idx_context_derived_identity
	// since migration 187), alongside the per-chunk hash check this endpoint
	// already runs.
	// A chunk whose value carries a NUL character fails on its own, before
	// any statement (issue #519; the NUL guard leaves the chunks to this
	// handler), and the others are written. The claim check reads the chunk
	// without a statement and still refuses the whole request for it; only
	// the probe, which would send its values to the database, is left out.
	nulChunks := ingestNULChunks(req.Chunks)
	for i, chunk := range req.Chunks {
		if rej := claimReject(nil, chunk.Category, "", "", chunk.Metadata); rej != nil {
			writeJSONReject(w, rej.prefixed(fmt.Sprintf("chunk[%d]: ", i)))
			return
		}
		if nulChunks[i] != "" {
			continue
		}
		claimed, err := store.ProvenanceClaimed(ctx, h.pool, chunk.Category, chunk.Title, writeScope)
		if err != nil {
			logRequestError(ctx, "ingest: provenance claim probe failed", "error", err, "index", i, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
			return
		}
		if claimed {
			writeJSONReject(w, classProvenanceProtected.reject(fmt.Sprintf(
				"chunk[%d]: block carries derived provenance — a client write cannot replace its content or metadata", i)))
			return
		}
	}

	// Write rate limit (§9e): before this the route had none, so one key could
	// push maxIngestChunks writes per request without bound. Checked after the
	// batch gates so a refused batch costs no budget; the bucket row is written
	// synchronously so concurrent requests see it.
	if h.ingestRateLimited(w, ctx, authResult.ApiKeyID) {
		return
	}

	// One block-type snapshot for the whole batch (#70), resolved after the
	// rate limit so a refused request costs none. Resolved per chunk, a batch
	// of maxIngestChunks would reach the registry up to 200 times: on its miss
	// path (a failed tenant-set build is not cached and logs a WARN) that is
	// 200 rebuild attempts and 200 WARN lines, and a reload landing mid-batch
	// would classify one request against two rule generations.
	classifySet := h.classifySet(ctx)

	// Process each chunk.
	resp := IngestResponse{
		Scope:   writeScope,
		Total:   len(req.Chunks),
		Results: make([]IngestResult, 0, len(req.Chunks)),
	}

	for i, chunk := range req.Chunks {
		result := IngestResult{Index: i, Status: "failed", Error: nulChunks[i] + " " + nulFieldMessage}
		if nulChunks[i] == "" {
			result = h.processChunk(ctx, reqID, chunk, i, writeScope, authResult.ApiKeyID, classifySet)
		}
		resp.Results = append(resp.Results, result)

		switch result.Status {
		case "inserted":
			resp.Inserted++
		case "updated":
			resp.Updated++
		case "skipped":
			resp.Skipped++
		case "failed":
			resp.Failed++
		}
	}

	resp.Success = resp.Failed == 0

	writeJSON(w, http.StatusOK, resp)
}

// processChunk handles a single chunk: hash check, upsert/insert, embed, temporal enrichment.
// classifySet is the request's block-type snapshot, resolved once in HandleIngest.
func (h *IngestHandler) processChunk(ctx context.Context, reqID string, chunk IngestChunk, index int, scope, apiKeyID string, classifySet *blocktype.Set) IngestResult {
	// 1. Identical-content check for both write modes. Block mode predicts
	// UpsertBlock's (category, title, scope) conflict and metadata merge.
	// Chunk mode writes by (source_id, chunk_index), but the same content
	// identity check prevents a no-op rewrite and a cross-source collision on
	// uq_context_category_title_scope, which has no lifecycle filter.
	// IdenticalContentMatch merges metadata while InsertChunk replaces it, so
	// request metadata that is a subset of the row's metadata can answer
	// skipped although InsertChunk would remove the unnamed keys.
	match, matchErr := store.IdenticalContentMatch(ctx, h.pool, chunk.Content, scope, chunk.Category, chunk.Title,
		chunk.Tags, chunk.Metadata, store.SensitivityWrite{}, "")
	if matchErr != nil {
		logRequestError(ctx, "ingest: identical content match error",
			"error", matchErr, "index", index, "request_id", reqID)
		return IngestResult{Index: index, Status: "failed", Error: "hash check failed"}
	}
	if match.Unchanged() {
		return IngestResult{Index: index, ID: match.ID, Status: "skipped"}
	}
	if chunk.SourceID != "" && match.ID != "" {
		// InsertChunk can only write its own (source_id, chunk_index) slot.
		// A matching row outside that slot holds the content identity, so the
		// insert would hit uq_context_category_title_scope instead of landing.
		// The predicate is InsertChunk's arbiter (NOT is_archived AND
		// lifecycle_state = 'chunk'), and the source id is compared as a uuid
		// as InsertChunk casts it, so a non-canonical spelling of the same id
		// still names this slot (#182). A malformed id names no slot.
		var sameSlot bool
		err := h.pool.QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM context_blocks
			    WHERE id = $1 AND source_id = $2::uuid AND chunk_index = $3
			      AND NOT is_archived AND lifecycle_state = 'chunk'
			 )`, match.ID, chunk.SourceID, chunk.ChunkIndex).Scan(&sameSlot)
		if err != nil && !pgxdb.MalformedUUID(err) {
			logRequestError(ctx, "ingest: chunk slot check error",
				"error", err, "index", index, "request_id", reqID)
			return IngestResult{Index: index, Status: "failed", Error: "chunk slot check failed"}
		}
		if !sameSlot {
			return IngestResult{Index: index, ID: match.ID, Status: "skipped"}
		}
	}

	// 2. Upsert or InsertChunk depending on whether SourceID is set.
	var block *store.Block
	var err error
	var action string

	if chunk.SourceID != "" {
		// Chunk mode: source_id + chunk_index for idempotent upsert.
		block, err = store.InsertChunk(ctx, h.pool,
			chunk.SourceID, chunk.ChunkIndex,
			chunk.Category, chunk.Title, chunk.Content,
			chunk.Tags, chunk.Metadata, scope,
		)
		if rej := requestKeyGoneReject(err); rej != nil {
			// The key was deleted while the request ran (issue #555):
			// nothing written, no line.
			return IngestResult{Index: index, Status: "failed", Error: rej.Msg}
		}
		if errors.Is(err, store.ErrChunkNotWritable) {
			// Absent source, foreign source and a slot held by another scope's
			// chunk are ONE answer (no oracle, §9e); nothing was written.
			return IngestResult{Index: index, Status: "failed", Error: "source not found"}
		}
		if rej := identityTakenReject(err); rej != nil {
			// Another active block holds the chunk's identity (issue #275):
			// the caller's input, no ERROR line.
			return IngestResult{Index: index, Status: "failed", Error: rej.Msg}
		}
		if err != nil {
			// InsertChunk is a labeled bracket since issue #236 (the write and
			// its invalidation commit together).
			logRequestError(ctx, "ingest: insert chunk error",
				"error", err, "index", index, "source_id", chunk.SourceID, "request_id", reqID, "write_outcome", writeOutcome(err))
			return IngestResult{Index: index, Status: "failed", Error: "insert chunk failed"}
		}
		action = "inserted"
	} else {
		// Block mode: upsert by (category, title, scope).
		block, err = store.UpsertBlock(ctx, h.pool,
			chunk.Category, chunk.Title, chunk.Content,
			chunk.Tags, chunk.Metadata, scope, false, store.SensitivityWrite{}, "",
		)
		if err != nil {
			// I7/S3: a chunk can hit a derivative's identity in a category that
			// is NOT reserved, so the batch-level S2 sweep above cannot see it.
			// The store refuses it; this arm keeps the per-chunk report shape
			// and names the reason instead of filing it under "upsert failed".
			if errors.Is(err, store.ErrProvenanceProtected) {
				return IngestResult{Index: index, Status: "failed",
					Error: "block carries derived provenance — not client-writable"}
			}
			if rej := requestKeyGoneReject(err); rej != nil {
				return IngestResult{Index: index, Status: "failed", Error: rej.Msg} // issue #555
			}
			logRequestError(ctx, "ingest: upsert block error",
				"error", err, "index", index, "request_id", reqID, "write_outcome", writeOutcome(err))
			return IngestResult{Index: index, Status: "failed", Error: "upsert failed"}
		}
		// UpsertBlock uses ON CONFLICT DO UPDATE, so both insert and update return a block.
		// We cannot distinguish insert vs update at this level, so we use "updated" as
		// the default since IdenticalContentMatch already catches a true no-op.
		action = "updated"

		// Auto-classify type_name from title and metadata with the request's
		// registry snapshot, like POST /api/store and the MCP store and update
		// paths. type_source='manual' blocks are never re-classified; no rule
		// match is a no-op. A failure keeps the written block with the type it
		// already has (the default on a new row), as on /api/store.
		//
		// Chunk-mode writes above do not run the classifier: InsertChunk has
		// no caller besides this handler, and classifying its writes is a
		// decision of its own. That is a statement about the write path, not
		// about chunk rows — uq_context_category_title_scope has no lifecycle
		// filter, so a block-mode write with new content naming a chunk row's
		// (category, title, scope) lands on that row and classifies it here.
		// The chunk is committed: its classification runs detached from the
		// client (issue #453).
		fctx, cancel := followUpContext(ctx)
		if _, err := store.ClassifyBlockAfterUpsert(fctx, h.pool, classifySet, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("ingest: auto-classify failed",
				"error", err, "index", index, "block_id", block.ID, "request_id", reqID)
		}
		cancel()
	}

	// 3. Log write (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The key gone with its tenant's delete after the write (issue #555):
		// nothing to book on, no fault.
		if logErr := store.LogAccess(bgCtx, h.pool, apiKeyID, block.ID, "write"); logErr != nil && !errors.Is(logErr, store.ErrRequestKeyGone) {
			slog.Error("ingest: write log error", "error", logErr, "block_id", block.ID, "request_id", reqID)
		}
	}()

	// 4. Temporal enrichment (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Always populated: createdAt is included as meta-anchor even without content times.
		if _, err := store.RefreshBlockTemporal(bgCtx, h.pool, block); err != nil {
			slog.Error("ingest: temporal refresh failed",
				"error", err, "block_id", block.ID, "request_id", reqID)
		}
	}()

	// Embedding generated async by scheduler backfill loop.
	return IngestResult{Index: index, ID: block.ID, Status: action}
}

// classifySet resolves the caller's block-type policy set for the classify
// hook. A nil registry (test wiring without classify) gives a nil set, and
// ClassifyBlockAfterUpsert then fails loudly instead of using a compiled-in
// set — the same contract as StoreHandler.classifySet.
func (h *IngestHandler) classifySet(ctx context.Context) *blocktype.Set {
	if h.blocktypes == nil {
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx)
}

// ingestRateLimited applies the write budget to the request and books it in
// the ingest bucket. It answers on w and reports whether it did.
func (h *IngestHandler) ingestRateLimited(w http.ResponseWriter, ctx context.Context, apiKeyID string) bool {
	if h.cfg == nil {
		return false
	}
	limit := h.cfg.SnapshotForRequest(ctx).Query.RateLimitWrite
	if limit <= 0 {
		return false
	}
	if rateLimitBlocked(w, ctx, h.pool, apiKeyID, ingestBucket, "ingest: rate limit check error", "ingest requests", limit) {
		return true
	}
	if err := store.LogAccess(ctx, h.pool, apiKeyID, "", ingestBucket); err != nil {
		if rej := requestKeyGoneReject(err); rej != nil {
			writeJSONReject(w, rej) // the key was deleted while the request ran (issue #555)
			return true
		}
		// Fail closed: an unbooked request would not count against the limit.
		logRequestError(ctx, "ingest: rate limit booking failed", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return true
	}
	return false
}
