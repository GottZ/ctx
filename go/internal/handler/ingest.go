// Package handler — Ingest endpoint for bulk block import.
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// POST /api/ingest accepts up to 200 chunks per request.
// Each chunk is validated, hash-checked, upserted, embedded, and temporally enriched.
// No rate-limit on this endpoint (internal API path).
//
// Source: https://github.com/GottZ/ctx
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// maxIngestChunks is the maximum number of chunks per ingest request.
	maxIngestChunks = 200

	// maxContentSize is the maximum content size per chunk (50 KB).
	maxContentSize = 50 * 1024

	// maxCategoryLen is the maximum category length (100 chars).
	maxCategoryLen = 100

	// maxTitleLen is the maximum title length (500 chars).
	maxTitleLen = 500
)

// IngestHandler handles POST /api/ingest.
type IngestHandler struct {
	pool *pgxpool.Pool
}

// NewIngestHandler creates a new IngestHandler.
func NewIngestHandler(pool *pgxpool.Pool) *IngestHandler {
	return &IngestHandler{pool: pool}
}

// IngestRequest is the JSON body for POST /api/ingest.
type IngestRequest struct {
	Source string        `json:"source"`
	Chunks []IngestChunk `json:"chunks"`
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
	Success  bool           `json:"success"`
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
			errs = append(errs, fmt.Sprintf("%s: category exceeds %d characters", prefix, maxCategoryLen))
		}

		if strings.TrimSpace(c.Title) == "" {
			errs = append(errs, fmt.Sprintf("%s: title is required", prefix))
		} else if len(c.Title) > maxTitleLen {
			errs = append(errs, fmt.Sprintf("%s: title exceeds %d characters", prefix, maxTitleLen))
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

	writeScope := authResult.HomeScope

	// Process each chunk.
	resp := IngestResponse{
		Total:   len(req.Chunks),
		Results: make([]IngestResult, 0, len(req.Chunks)),
	}

	for i, chunk := range req.Chunks {
		result := h.processChunk(ctx, reqID, chunk, i, writeScope, authResult.ApiKeyID)
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
func (h *IngestHandler) processChunk(ctx context.Context, reqID string, chunk IngestChunk, index int, scope, apiKeyID string) IngestResult {
	// 1. Hash NOOP check — skip if identical content exists.
	existingID, err := store.HashNOOPCheck(ctx, h.pool, chunk.Content, scope, chunk.Category, chunk.Title)
	if err != nil {
		slog.Error("ingest: hash noop check error",
			"error", err, "index", index, "request_id", reqID)
		return IngestResult{Index: index, Status: "failed", Error: "hash check failed"}
	}
	if existingID != "" {
		return IngestResult{Index: index, ID: existingID, Status: "skipped"}
	}

	// 2. Upsert or InsertChunk depending on whether SourceID is set.
	var block *store.Block
	var action string

	if chunk.SourceID != "" {
		// Chunk mode: source_id + chunk_index for idempotent upsert.
		block, err = store.InsertChunk(ctx, h.pool,
			chunk.SourceID, chunk.ChunkIndex,
			chunk.Category, chunk.Title, chunk.Content,
			chunk.Tags, chunk.Metadata, scope,
		)
		if err != nil {
			slog.Error("ingest: insert chunk error",
				"error", err, "index", index, "source_id", chunk.SourceID, "request_id", reqID)
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
			slog.Error("ingest: upsert block error",
				"error", err, "index", index, "request_id", reqID)
			return IngestResult{Index: index, Status: "failed", Error: "upsert failed"}
		}
		// UpsertBlock uses ON CONFLICT DO UPDATE, so both insert and update return a block.
		// We cannot distinguish insert vs update at this level, so we use "updated" as
		// the default since HashNOOPCheck already catches identical content (= true skip).
		action = "updated"
	}

	// 3. Log write (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if logErr := store.LogAccess(bgCtx, h.pool, apiKeyID, block.ID, "write"); logErr != nil {
			slog.Error("ingest: write log error", "error", logErr, "block_id", block.ID, "request_id", reqID)
		}
	}()

	// 4. Temporal enrichment (fire-and-forget).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(bgCtx, h.pool, block.ID, times); err != nil {
			slog.Error("ingest: content_times update failed",
				"error", err, "block_id", block.ID, "request_id", reqID)
		}
		// Always populate: createdAt is included as meta-anchor even without content times.
		if err := store.PopulateTemporal(bgCtx, h.pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("ingest: temporal populate failed",
				"error", err, "block_id", block.ID, "request_id", reqID)
		}
	}()

	// Embedding generated async by scheduler backfill loop.
	return IngestResult{Index: index, ID: block.ID, Status: action}
}
