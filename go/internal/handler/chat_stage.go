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
