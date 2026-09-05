package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/rootmap"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DigestHandler handles POST /api/digest.
type DigestHandler struct {
	pool       *pgxpool.Pool
	blocktypes *blocktype.Registry
	cfg        ConfigStore
}

// NewDigestHandler creates a new DigestHandler. blocktypes feeds the T4
// topic-map classify hook (registry snapshot, never the compiled-in set); cfg
// feeds the root_map.* policy of the W-D map trigger and the rate-limit budget.
func NewDigestHandler(pool *pgxpool.Pool, blocktypes *blocktype.Registry, cfg ConfigStore) *DigestHandler {
	return &DigestHandler{pool: pool, blocktypes: blocktypes, cfg: cfg}
}

// HandleDigest triggers a topic map rebuild for the authenticated scope.
//
// Since W-D it ALSO renders the root map — the deliberate second trigger of
// design/02 §4.2: a human can force the map without waiting out the rebuild
// cadence, and gets fresh coverage numbers over the same cluster state. What it
// still does NOT do is start a rebuild: a route that kicks off Louvain would
// hand every valid API key a lever on a job bounded only by
// graph_overview.rebuild_timeout.
func (h *DigestHandler) HandleDigest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	cfg := h.cfg.SnapshotForTenant(ctx, authResult.HomeScope)

	// Write rate limit — own action bucket "digest" (W-D). The route rebuilds a
	// corpus-wide artefact and, until this wave, was the ONE such endpoint
	// without a bucket while its read-only sibling GET /api/graph/overview has
	// carried one for waves. 0 = disabled, same convention as everywhere else.
	// The WRITE budget, deliberately — the read surfaces pass RateLimitRead.
	if rateLimitBlocked(w, ctx, h.pool, authResult.ApiKeyID, "digest", "digest: rate limit check error", "digest runs", cfg.Query.RateLimitWrite) {
		return
	}

	// Run digest. The request path resolves the policy overlay against the
	// caller's own tenant (HomeScope) — the same scope it writes the topic-map
	// under (T12: tenant key == write scope on the request path).
	err := digest.RunDigest(ctx, h.pool, h.blocktypes, cfg.Digest.Mode, cfg.Dream.Language, authResult.HomeScope, authResult.HomeScope, authResult.ReadScopes)
	if err != nil {
		slog.Error("digest: run error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Digest generation failed",
		})
		return
	}

	// Root map (W-D). A failure here does not fail the request: the topic map
	// already succeeded, and the map is an additive artefact — degrading to
	// "no root map this round" is honest, taking the whole endpoint down is not.
	rootMapTitle, rootMapLength := h.runRootMap(ctx, authResult, cfg, reqID)

	blockCount, categoryCount, contentLength := h.digestEnvelopeCounts(ctx, authResult, cfg, reqID)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"action":        "digest",
		"scope":         authResult.HomeScope,
		"title":         "topic-map-" + authResult.HomeScope,
		"blockCount":    blockCount,
		"categoryCount": categoryCount,
		"contentLength": contentLength,
		"rootMapTitle":  rootMapTitle,
		"rootMapLength": rootMapLength,
	})
}

// runRootMap renders the caller's root map and reports its title and size.
// Title is returned even when the map is off or unwritten — it is the ADDRESS
// of the artefact ("where do I look"), independent of whether this request
// changed it; a zero length says "not written in this round".
func (h *DigestHandler) runRootMap(ctx context.Context, auth *auth.AuthResult, cfg *config.Config, reqID string) (string, int) {
	if !cfg.RootMap.Enabled {
		return "", 0
	}
	if h.blocktypes == nil {
		slog.Warn("digest: root map skipped — block-type registry not wired", "request_id", reqID)
		return "", 0
	}
	res, err := rootmap.Run(ctx, h.pool, h.blocktypes.SnapshotForTenant(ctx, auth.HomeScope),
		rootmap.Config{
			Enabled:            true,
			BudgetBytes:        cfg.RootMap.BudgetBytes,
			FooterReserveBytes: cfg.RootMap.FooterReserveBytes,
			SmallClusterMax:    cfg.RootMap.SmallClusterMax,
			CountTimeout:       cfg.RootMap.CountTimeout,
			RebuildInterval:    cfg.GraphOverview.RebuildInterval,
			SuperEnabled:       cfg.RootMap.SuperEnabled,
			// E3-01: same corpus language knob as the labels; both digest triggers
			// must render the same bytes or content idempotency collapses.
			Language: cfg.Dream.Language,
		},
		auth.HomeScope, auth.ReadScopes)
	if err != nil {
		slog.Warn("digest: root map failed", "error", err, "request_id", reqID)
		return "", 0
	}
	return res.Title, res.Length
}

// digestEnvelopeCounts fills the three response numbers under the SAME cap the
// coverage denominator carries (root_map.count_timeout).
//
// Two of them — the corpus block count and the DISTINCT category count — are
// O(corpus) and ran uncapped in the request path until W-D; at 10M the DISTINCT
// count is the single most expensive statement of this whole axis, more so than
// the coverage count that §6.3 caps so carefully. They stay in the envelope
// (removing them would break the CLI's response shape) but can no longer run
// unbounded: on expiry the field degrades to 0, which is what "we did not
// measure it this round" looks like in a number.
func (h *DigestHandler) digestEnvelopeCounts(ctx context.Context, auth *auth.AuthResult, cfg *config.Config, reqID string) (int, int, int) {
	timeout := cfg.RootMap.CountTimeout
	blockCount, _, err := store.ActiveBlockCount(ctx, h.pool, auth.ReadScopes, timeout)
	if err != nil {
		slog.Warn("digest: block count failed", "error", err, "request_id", reqID)
	}
	categoryCount, _, err := store.ActiveCategoryCount(ctx, h.pool, auth.ReadScopes, timeout)
	if err != nil {
		slog.Warn("digest: category count failed", "error", err, "request_id", reqID)
	}
	contentLength, err := store.MapBlockLength(ctx, h.pool, "index", "topic-map-"+auth.HomeScope, auth.HomeScope)
	if err != nil {
		slog.Warn("digest: topic map length failed", "error", err, "request_id", reqID)
	}
	return blockCount, categoryCount, contentLength
}
