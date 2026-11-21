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

// SearchHandler handles POST /api/search.
type SearchHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
	// blocktypes resolves the untrusted framing of each result row (V-11,
	// design/02 §5.1 BA7 layer 3). nil (test wiring) leaves the field absent
	// from every row — no statement, never the positive claim "trusted".
	blocktypes *blocktype.Registry
}

// NewSearchHandler creates a new SearchHandler. The read rate limit comes
// from a config snapshot per request (F1-W7), not a boot copy. blocktypes is
// the block-type registry whose per-request snapshot frames untrusted result
// rows (V-11).
func NewSearchHandler(pool *pgxpool.Pool, cfg ConfigStore, blocktypes *blocktype.Registry) *SearchHandler {
	return &SearchHandler{pool: pool, cfg: cfg, blocktypes: blocktypes}
}

// typeSnapshot is the per-request registry view the result framing reads.
// nil registry ⇒ nil set ⇒ no untrusted key on any row (store.untrustedOf).
func (h *SearchHandler) typeSnapshot(ctx context.Context) *blocktype.Set {
	if h.blocktypes == nil {
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx)
}

type searchRequest struct {
	Query    string   `json:"query"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	// TagsAll (#219): only blocks that carry every one of these tags; `tags`
	// keeps its overlap meaning, and both given means both apply. Absent =
	// no filter, and the answer echoes the key only when it was given.
	TagsAll []string `json:"tags_all"`
	Compact *bool    `json:"compact"`
	Limit   int      `json:"limit"`
	// After is the keyset-pagination cursor (block-workbench W7) for the
	// empty-query browse path: the {after_updated, after_id} of the last row of
	// the previous page. nil/absent = page 1 (unchanged). Garbage is rejected
	// defensively (400) rather than crashing; the FTS path ignores it.
	After *store.SearchCursor `json:"after"`
	// Types/TypesExclude (WF T10, design/01 §7-T10 R1): opt-in SERVER-side
	// type filters — bind parameters in the store layer, never a client
	// filter over paginated lists (at 10k+ issues per scope, knowledge
	// browsing would page through issue pages). NOT a hard exclude: the D5
	// browse asymmetry stays (retrieval-excluded types remain browseable).
	// BlockRolesExclude is the documented legacy alias for TypesExclude
	// (seam 17); both present ⇒ the UNION applies (monotone-restrictive).
	Types             []string `json:"types"`
	TypesExclude      []string `json:"types_exclude"`
	BlockRolesExclude []string `json:"block_roles_exclude"`
	// Cluster is the `cluster:<handle>` facet (Cluster-Topic-Map C6, design/03
	// §4.8): a hard restriction to ONE topic — the stable handle the graph
	// surfaces emit, never the internal cluster_id. Empty = no restriction.
	//
	// Gated on cluster.facet_enabled (default off). While the gate is closed the
	// field is ignored COMPLETELY — not rejected, not echoed — so the dark state
	// is byte-identical to the time before C6, when an unknown JSON key was
	// simply dropped by the decoder.
	Cluster string `json:"cluster"`
}

// coreArgs maps the decoded body onto the argument set both search transports
// share (search_core.go). It is a named function, not an inline literal, so the
// parity test can hold this mapping against the MCP tool's twin
// (searchInput.coreArgs) without a database: the one way the two surfaces can
// still drift is a field one of them forgets to fill, and that is exactly what
// TestSearchTransportsAgreeOnTheAliasUnion reads.
//
// The four parameters are the values the ENVELOPE also needs and the transport
// therefore computes itself: the effective limit (echoed, and the input of the
// next-page cursor), compact (echoed), the sanitised cursor, and the C6 flag
// out of this request's ONE config snapshot.
func (req searchRequest) coreArgs(limit int, compact bool, after *store.SearchCursor, facetEnabled, leaveOutNeverRanked bool) searchArgs {
	return searchArgs{
		Query:             req.Query,
		Category:          req.Category,
		Cluster:           req.Cluster,
		Tags:              req.Tags,
		TagsAll:           req.TagsAll,
		Types:             req.Types,
		TypesExclude:      req.TypesExclude,
		BlockRolesExclude: req.BlockRolesExclude,
		Limit:             limit,
		Compact:           compact,
		After:             after,
		// KnownTypesOnly stays FALSE here: on the browse route `types` is a
		// pure opt-in bind parameter, an unknown name matches nothing, and
		// retrieval-excluded types stay browseable (D5). The refusal of an
		// unknown name belongs to the MCP tool alone.
		KnownTypesOnly:      false,
		FacetEnabled:        facetEnabled,
		LeaveOutNeverRanked: leaveOutNeverRanked,
	}
}

// HandleSearch processes lightweight search requests (no LLM).
func (h *SearchHandler) HandleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// ONE config snapshot per request: the rate limit and the C6 facet gate must
	// never come from two different generations.
	cfgSnap := h.cfg.SnapshotForRequest(ctx)

	// Read rate limit check (0 = disabled). MT 06-C5: per-tenant via the
	// request context (tenant's own RateLimitRead override, else _global).
	if rateLimitBlocked(w, ctx, h.pool, authResult.ApiKeyID, "query", "search: read rate limit check error", "reads", cfgSnap.Query.RateLimitRead) {
		return
	}

	// Parse body.
	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("search: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// Defaults.
	compact := true
	if req.Compact != nil {
		compact = *req.Compact
	}
	// Default (10) and cap (store.SearchMaxLimit) are the shared rule of both
	// search surfaces (searchLimit); the effective value is echoed in `filters`.
	limit := searchLimit(req.Limit)

	// Keyset cursor (W7), read by the core (searchCursorOf): a cursor with a
	// zero/empty position is page 1, an after_id that is not a block id is a
	// 400 (it used to reach the uuid bind and answer 500). The FTS path ignores
	// any cursor (LIMIT-only).
	after := req.After

	// Search through the ONE rule set both transports share (T03-9,
	// search_core.go): the C6 facet gate, the types_exclude ∪ block_roles_exclude
	// union and the block-grant resolution live there. The last of the three is
	// a behaviour change on THIS route and the point of E03-1 = A: /api/search
	// was the only read surface that passed a literal nil for the grant set, so
	// a block a tenant had been granted was readable through the MCP tools and
	// not here. It is affordable because T04-20 bounds the set per request.
	//
	// The C6 flag rides in as a VALUE out of the ONE snapshot taken above — the
	// core must never take a second one (search_core.go, searchArgs.FacetEnabled).
	a := req.coreArgs(limit, compact, after, cfgSnap.ClusterOps.FacetEnabled, cfgSnap.Search.LeaveOutNeverRanked)
	page, err := executeSearch(ctx, h.pool, h.typeSnapshot(ctx), authResult, a)
	if err != nil {
		var rejected *searchRejected
		switch {
		case errors.As(err, &rejected):
			// Caller error with prose the core holds (the malformed facet handle,
			// the malformed cursor) — same 400 envelope as before the core existed.
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": rejected.Error(),
			})
		case errors.Is(err, store.ErrTooManyBlockGrants):
			// T04-20: an over-bound grant set is REFUSED, never quietly cut down
			// to scope-only; tooManyGrantsMsg is the one prose every read
			// surface renders for it.
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": tooManyGrantsMsg,
			})
		default:
			logRequestError(ctx, "search: query error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
		}
		return
	}

	// Next-page cursor (W7). Only the empty-query browse path paginates; the
	// FTS path is "top matches" (no loadMore). The core reads one probe row, so
	// the cursor exists exactly when another row does — a full last page no
	// longer answers a cursor to an empty page (#206).
	results, nextAfter := page.Rows, page.NextAfter

	// Format response.
	var categoryFilter any = nil
	if req.Category != "" {
		categoryFilter = req.Category
	}
	var tagsFilter any = nil
	if len(req.Tags) > 0 {
		tagsFilter = req.Tags
	}
	var queryFilter any = nil
	if req.Query != "" {
		queryFilter = req.Query
	}
	var typesFilter any = nil
	if len(req.Types) > 0 {
		typesFilter = req.Types
	}
	// The EFFECTIVE exclude list, the one the core bound (searchPage.
	// TypesExclude: types_exclude ∪ block_roles_exclude, plus the never-ranked
	// types a full-text search without types left out when
	// search.leave_out_never_ranked is on) — the echo can therefore not claim
	// a different filter from the one that ran.
	typesExclude := page.TypesExclude
	var typesExcludeFilter any = nil
	if len(typesExclude) > 0 {
		typesExcludeFilter = typesExclude
	}

	filters := map[string]any{
		"query":    queryFilter,
		"category": categoryFilter,
		"tags":     tagsFilter,
		"limit":    limit,
		// WF T10: the EFFECTIVE type filters (types_exclude already
		// unioned with the legacy block_roles_exclude alias).
		"types":         typesFilter,
		"types_exclude": typesExcludeFilter,
	}
	// The facet is echoed only when it was APPLIED — an unconditional key would
	// move a byte in the dark state, and a key echoing an ignored value would
	// claim a filter that did not run. clusterFacetOf is the same first stage
	// the core ran; a request that got here passed its form check.
	if clusterFacet := clusterFacetOf(a.FacetEnabled, a.Cluster); clusterFacet != nil {
		filters["cluster"] = *clusterFacet
	}
	// tags_all (#219) the same way: echoed when it ran, absent otherwise, so a
	// request without it answers the filters object it answered before.
	if len(req.TagsAll) > 0 {
		filters["tags_all"] = req.TagsAll
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"count":   len(results),
		"compact": compact,
		"filters": filters,
		"results": results,
		// next_after is the cursor for the FOLLOWING page (W7 "Load more"), or
		// null when there is no next page (last page / FTS top-matches mode).
		"next_after": nextAfter,
	})
}
