package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RecentHandler handles POST /api/recent — the REST half of the recent surface
// (T03-8b, DECISIONS K26). Until now "newest blocks first" existed only on the
// MCP tool and the chat ctx_recent tool; a plain HTTP caller had to abuse the
// empty-query browse path of /api/search, which orders by updated_at too but
// carries the cursor/compact/cluster machinery of a search route.
//
// The route is a thin wrapper by construction: the limit clamp (<=0 ⇒ 10,
// >50 ⇒ 50), the fail-closed scope guard and the block-grant OR-arm all live in
// store.RecentBlocks since T03-8, so both surfaces read through ONE statement.
// Clamping or guarding a second time here would be a second truth — the parity
// test (recent_parity_integration_test.go) pins that MCP and REST return the
// same block set for the same filters.
type RecentHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
	// blocktypes resolves the untrusted framing of each result row (V-11), same
	// contract as SearchHandler: nil (test wiring) leaves the field absent from
	// every row — no statement, never the positive claim "trusted".
	blocktypes *blocktype.Registry
}

// NewRecentHandler creates a new RecentHandler. The read rate limit comes from
// a config snapshot per request (F1-W7), not a boot copy.
func NewRecentHandler(pool *pgxpool.Pool, cfg ConfigStore, blocktypes *blocktype.Registry) *RecentHandler {
	return &RecentHandler{pool: pool, cfg: cfg, blocktypes: blocktypes}
}

// typeSnapshot is the per-request registry view the result framing reads.
// nil registry ⇒ nil set ⇒ no untrusted key on any row (store.untrustedOf).
func (h *RecentHandler) typeSnapshot(ctx context.Context) *blocktype.Set {
	if h.blocktypes == nil {
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx)
}

// recentRequest mirrors the MCP recentInput field for field (mcp.go recentInput)
// — same names, same semantics, so the two surfaces cannot drift apart.
// BlockRolesExclude is the documented legacy alias for TypesExclude (seam 17);
// both present ⇒ the UNION applies (monotone-restrictive), as everywhere else.
type recentRequest struct {
	Limit             int      `json:"limit"`
	Category          string   `json:"category"`
	Types             []string `json:"types"`
	TypesExclude      []string `json:"types_exclude"`
	BlockRolesExclude []string `json:"block_roles_exclude"`
}

// HandleRecent returns the newest visible blocks (no LLM, no FTS).
func (h *RecentHandler) HandleRecent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// ONE config snapshot per request. Read rate limit check (0 = disabled),
	// same bucket/noun/limit as /api/search: recent is the browse family's
	// second door, not a budget of its own — a caller must not double its read
	// allowance by alternating the two routes.
	cfgSnap := h.cfg.SnapshotForRequest(ctx)
	if rateLimitBlocked(w, ctx, h.pool, authResult.ApiKeyID, "query", "recent: read rate limit check error", "reads", cfgSnap.Query.RateLimitRead) {
		return
	}

	var req recentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("recent: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Grants are RESOLVED here, exactly as the MCP tool does it (resolveGrants,
	// mcp.go) — a REST caller sees the blocks its tenant was granted, or the
	// two surfaces would answer differently for the same key. The helper is
	// fail-closed for a resolver error (scope-only, no crash); the ONE error it
	// surfaces is the over-bound grant set (T04-20), and that one is refused on
	// both surfaces with the SAME prose rather than silently cut.
	grants, err := resolveGrants(ctx, h.pool, authResult)
	if errors.Is(err, store.ErrTooManyBlockGrants) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": tooManyGrantsMsg,
		})
		return
	}

	// Type filters (WF T10): types_exclude ∪ block_roles_exclude (legacy alias).
	typesExclude := unionExcludes(req.TypesExclude, req.BlockRolesExclude)
	results, err := store.RecentBlocks(ctx, h.pool, h.typeSnapshot(ctx), authResult.ReadScopes,
		req.Category, req.Limit, req.Types, typesExclude, grants)
	if err != nil {
		// Same status class and prose as /api/search's store error: no route
		// says out loud which of its guards refused. The MCP tool names the
		// scope guard because a model needs the reason in its transcript.
		logRequestError(ctx, "recent: query error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	var categoryFilter any = nil
	if req.Category != "" {
		categoryFilter = req.Category
	}
	var typesFilter any = nil
	if len(req.Types) > 0 {
		typesFilter = req.Types
	}
	var typesExcludeFilter any = nil
	if len(typesExclude) > 0 {
		typesExcludeFilter = typesExclude
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   len(results),
		// The EFFECTIVE filters (types_exclude already unioned with the legacy
		// alias). No "limit" key: the clamp lives in the store, and echoing the
		// requested number would state a limit that did not run (0 ⇒ 10,
		// 999 ⇒ 50). count is the honest figure.
		"filters": map[string]any{
			"category":      categoryFilter,
			"types":         typesFilter,
			"types_exclude": typesExcludeFilter,
		},
		"results": results,
	})
}
