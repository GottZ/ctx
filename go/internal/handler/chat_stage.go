// F6-C6 D-W6b: the chat surface's StageRunner. The ctx web chat is itself a
// harness WITHOUT an own gating layer (DECISIONS §Klarstellung D-E1/E2) and
// runs the smallest model — so EVERY chat-initiated write is staged
// (default-confirm by birth, independent of the per-key confirm_writes flag);
// the ConfirmCard in the SPA is this harness's human-in-the-loop mechanism.
//
// The runner is the QueryRunner injection pattern (chat.go): the chat package
// cannot import handler (import cycle), so the handler hands this
// implementation into the executor. It reuses the exact direct-path building
// blocks — runStageWriteGates (stage_gates.go, D-W2) → store.CanonicalWrite
// from the POST-GATE result (the hash binds the resolved scope and
// post-detector sensitivity) → store.StagePendingWrite (089, origin 'chat').
package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/store"
)

type chatStageRunner struct {
	pool       *pgxpool.Pool
	cfg        ConfigStore
	blocktypes *blocktype.Registry
}

// StageWrite implements chat.StageRunner. The AuthResult travels in ctx (the
// request context flows from HandleStream through the engine into the tools);
// a missing identity is an infrastructural error, never a silent default
// (fail-closed, the T07 pattern).
func (s *chatStageRunner) StageWrite(ctx context.Context, category, title, content string, tags []string, metadata map[string]any) (*chat.StagedWrite, string, error) {
	ar := AuthResultFromContext(ctx)
	if ar == nil || !ar.IsValid {
		return nil, "", errors.New("chat stage: no resolved auth identity")
	}
	reqID := RequestIDFromContext(ctx)

	var set *blocktype.Set
	if s.blocktypes != nil {
		set = s.blocktypes.SnapshotForRequest(ctx)
	}
	var defaultSens backends.Sensitivity
	var rateLimit int
	var ttl time.Duration
	if s.cfg != nil {
		snap := s.cfg.SnapshotForRequest(ctx)
		defaultSens = snap.Pool.DefaultBlockSensitivity
		rateLimit = snap.Query.RateLimitWrite
		ttl = snap.Writes.ConfirmTTL
	}

	// The chat tool carries no scope/type/sensitivity input (storeToolDef):
	// scope resolves to the key's home_scope, sensitivity to the settings
	// default + detector, type stays auto-classify — the model steers none.
	res, rej := runStageWriteGates(ctx, s.pool, set, ar, storeRequest{
		Category: category,
		Title:    title,
		Content:  content,
		Tags:     tags,
		Metadata: metadata,
	}, defaultSens, rateLimit, reqID)
	if rej != nil {
		return nil, rej.Msg, nil
	}

	cw := store.CanonicalWrite{
		Op:                "store",
		Scope:             res.WriteScope,
		Category:          category,
		Title:             title,
		Content:           content,
		Tags:              tags,
		Metadata:          res.Metadata,
		Sensitivity:       string(res.Sens.Value),
		SensitivityManual: res.Sens.Manual,
		SensitivityDetect: res.Sens.Detector,
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("chat: canonicalize staged write failed", "error", err, "request_id", reqID)
		return nil, "", err
	}
	pw, err := store.StagePendingWrite(ctx, s.pool, ar.ApiKeyID, res.WriteScope, "store", "chat", canonical, hash, ttl)
	if err != nil {
		slog.Error("chat: stage pending write failed", "error", err, "request_id", reqID)
		return nil, "", err
	}

	return &chat.StagedWrite{
		PayloadHash:    hash,
		Op:             "store",
		Scope:          res.WriteScope,
		Category:       category,
		Title:          title,
		Sensitivity:    string(res.Sens.Value),
		ContentPreview: previewOf(content),
		ContentChars:   len(content),
		ExpiresAt:      pw.ExpiresAt,
	}, "", nil
}

// StageUpdate implements chat.StageRunner for field-level updates (D-W6c).
// Mirrors the MCP stage branch (mcpStageUpdate): write rate limit (staging is
// write intent), resolve within writableBlockScopes, size limits, then the
// D1-M3 TOCTOU pin — the target's updated_at at stage time, hash-bound via
// CanonicalWrite.BaseUpdatedAt. Deliberately NO credentials detector: the
// update EXECUTE path (store.UpdateBlock, REST parity) applies none either —
// the staged card must promise exactly what the confirm will do (D1-M2).
func (s *chatStageRunner) StageUpdate(ctx context.Context, id string, category, title, content *string, tags []string, metadata map[string]any) (*chat.StagedWrite, string, error) {
	ar := AuthResultFromContext(ctx)
	if ar == nil || !ar.IsValid {
		return nil, "", errors.New("chat stage: no resolved auth identity")
	}
	reqID := RequestIDFromContext(ctx)

	data := store.UpdateBlockData{Category: category, Title: title, Content: content, Tags: tags, Metadata: metadata}
	fields := make([]string, 0, 5)
	if category != nil {
		fields = append(fields, "category")
	}
	if title != nil {
		fields = append(fields, "title")
	}
	if content != nil {
		fields = append(fields, "content")
	}
	if tags != nil {
		fields = append(fields, "tags")
	}
	if metadata != nil {
		fields = append(fields, "metadata")
	}
	if len(fields) == 0 {
		return nil, "nothing to update — provide at least one of category, title, content, tags, metadata", nil
	}
	if msg := blockSizeLimit(strOrEmpty(data.Category), strOrEmpty(data.Title), strOrEmpty(data.Content)); msg != "" {
		return nil, msg, nil
	}

	var ttl time.Duration
	rateLimit := 0
	if s.cfg != nil {
		snap := s.cfg.SnapshotForRequest(ctx)
		ttl = snap.Writes.ConfirmTTL
		rateLimit = snap.Query.RateLimitWrite
	}
	if rateLimit > 0 {
		writeCount, err := store.CheckRateLimit(ctx, s.pool, ar.ApiKeyID)
		if err != nil {
			slog.Error("chat: stage update rate limit check error", "error", err, "request_id", reqID)
			return nil, "", err
		}
		if writeCount >= rateLimit {
			return nil, fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", rateLimit), nil
		}
	}

	resolvedID, _, err := store.ResolveBlockID(ctx, s.pool, id, writableBlockScopes(ar), nil)
	if err != nil {
		return nil, fmt.Sprintf("cannot resolve block id: %v", err), nil
	}
	if resolvedID == "" {
		return nil, "Block not found (or not in a writable scope of this key)", nil
	}
	// TOCTOU pin (D1-M3): read through the SAME write-eligible filter the
	// execute will use, capture updated_at as the base fingerprint.
	block, err := store.GetBlock(ctx, s.pool, resolvedID, writableBlockScopes(ar), nil)
	if err != nil {
		slog.Error("chat: stage update base read failed", "error", err, "block_id", resolvedID, "request_id", reqID)
		return nil, "", err
	}
	if block == nil {
		return nil, "Block not found (or not in a writable scope of this key)", nil
	}

	cw := store.CanonicalWrite{
		Op:            "update",
		ID:            resolvedID,
		Scope:         block.Scope,
		Category:      strOrEmpty(category),
		Title:         strOrEmpty(title),
		Content:       strOrEmpty(content),
		Tags:          tags,
		Metadata:      metadata,
		UpdateFields:  fields,
		BaseUpdatedAt: block.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	hash, canonical, err := cw.PayloadHash()
	if err != nil {
		slog.Error("chat: canonicalize staged update failed", "error", err, "request_id", reqID)
		return nil, "", err
	}
	pw, err := store.StagePendingWrite(ctx, s.pool, ar.ApiKeyID, block.Scope, "update", "chat", canonical, hash, ttl)
	if err != nil {
		slog.Error("chat: stage pending update failed", "error", err, "request_id", reqID)
		return nil, "", err
	}

	preview := ""
	previewChars := 0
	if content != nil {
		preview = previewOf(*content)
		previewChars = len(*content)
	}
	return &chat.StagedWrite{
		PayloadHash:    hash,
		Op:             "update",
		Scope:          block.Scope,
		Category:       block.Category,
		Title:          block.Title,
		ContentPreview: preview,
		ContentChars:   previewChars,
		ExpiresAt:      pw.ExpiresAt,
		TargetID:       resolvedID,
		UpdateFields:   cw.UpdateFields,
	}, "", nil
}

// previewOf bounds the card preview (display only — the authoritative payload
// is server-held; rune-safe cut).
func previewOf(content string) string {
	const max = 280
	r := []rune(content)
	if len(r) <= max {
		return content
	}
	return string(r[:max]) + "…"
}
