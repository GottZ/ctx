// Package handler — MCP (Model Context Protocol) server for ctx.
// Exposes ctx tools (query, store, search, get, dream) via the
// Streamable HTTP transport for remote MCP clients like Claude Code.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPConfig holds the wiring needed for the MCP tool handlers. The tools
// consume no runtime configuration (F1-W4): the query tool delegates to the
// QueryHandler, whose per-request config snapshot applies; store/search/get/
// recent are pure pool operations — block embeddings are generated
// asynchronously by the scheduler backfill, not here.
type MCPConfig struct {
	Pool         *pgxpool.Pool
	QueryHandler http.Handler // The full /api/query handler (with scheduler wiring).
	// Cfg feeds the store tool's sensitivity default
	// (pool.default_block_sensitivity, F3 §3.5) — one snapshot per call.
	Cfg ConfigStore
	// Blocktypes feeds the store tool's classify hook (WF T4) — registry
	// snapshot per call, never the compiled-in builtin set. nil in tests
	// without classify wiring: the hook then errors and is logged, the block
	// stays at the default type.
	Blocktypes *blocktype.Registry
}

// NewMCPHandler creates a Streamable HTTP handler for the MCP protocol.
// Auth is handled by wrapping this handler with the existing Auth middleware,
// extracting X-Context-Key (or Authorization: Bearer) from the request.
func NewMCPHandler(cfg MCPConfig) http.Handler {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "ctx",
			Version: "0.25.3",
		},
		&mcp.ServerOptions{
			Instructions: "ctx is a persistent knowledge store with hybrid retrieval. Use 'query' for questions, 'store' to save knowledge, 'search' for lightweight lookups.",
		},
	)

	registerTools(server, cfg)

	return mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:    true, // No session state needed — each call is independent.
			JSONResponse: true, // Prefer JSON over SSE for simple request/response tools.
		},
	)
}

// Tool definitions.

type queryInput struct {
	Question string `json:"question" jsonschema:"the question to answer using hybrid search + LLM synthesis"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max sources to return (default 5)"`
	// V-W6: the MCP-only type filters of the RETRIEVAL tools. `types` CUTS
	// against the request's visible allowlist — it never assigns visibility;
	// see resolveMCPVisibleTypes for the two rejections and for why
	// recentInput's raw predicate is deliberately not the model. Both fields
	// absent ⇒ the delegated request body is byte-identical to before they
	// existed.
	Types        []string `json:"types,omitempty" jsonschema:"restrict retrieval to these block types; each name must be a retrieval-visible type (unknown or non-visible name is an error)"`
	TypesExclude []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types from retrieval"`
}

type storeInput struct {
	Category    string         `json:"category" jsonschema:"block category (e.g. decisions, learnings, infrastructure)"`
	Title       string         `json:"title" jsonschema:"block title (upserts on same category+title+scope)"`
	Content     string         `json:"content" jsonschema:"block content (max 50KB)"`
	Tags        []string       `json:"tags,omitempty" jsonschema:"optional tags for filtering"`
	Metadata    map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object"`
	Sensitivity string         `json:"sensitivity,omitempty" jsonschema:"content sensitivity for trust gating: credentials|personal|internal|public (default from settings, fail-closed)"`
	// N-26: the explicit block type. Omitted = the title classifier keeps
	// guessing (type_source='auto'); named = the writer ASSERTS the type and
	// the block carries type_source='manual', which the auto-classifier never
	// re-touches. Validated against the per-request registry snapshot in the
	// gate chain (unknown name ⇒ rejected, fail-closed) — an MCP writer now
	// reaches the same axis REST /api/store has taken since WF T10.
	Type string `json:"type,omitempty" jsonschema:"explicit block type name from the registry (e.g. reference, checkpoint); omit to let the title classifier decide"`
	// E-M4 (2026-08-25) SUPERSEDES decision D4, which kept the MCP write tools
	// scope-free: an MCP writer always wrote its key's home scope, while the
	// same key over REST /api/store could name any scope its rights covered.
	// That was one principal with two authorisation answers for one scope, and
	// the narrower one silently attached to the transport an agent actually
	// uses. The field carries no new authority — it is gated by
	// resolveWriteScope, the same function REST runs, so a scope outside
	// writableBlockScopes is scope_denied here exactly as it is there, and an
	// absent/empty value resolves to ar.HomeScope byte-identically to the
	// pre-E-M4 behaviour.
	Scope string `json:"scope,omitempty" jsonschema:"optional target scope; default = the key's home scope; must be a scope the key may write"`
}

type searchInput struct {
	Query    string   `json:"query,omitempty" jsonschema:"search text"`
	Category string   `json:"category,omitempty" jsonschema:"filter by category"`
	Tags     []string `json:"tags,omitempty" jsonschema:"filter by tags"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max results (default 10)"`
	// Cluster is the C6 topic facet — a FIELD on the existing tool, not an
	// eighth tool: a new tool raises the selection load of every MCP client,
	// an optional argument on a known one does not (design/03 §4.8).
	Cluster string `json:"cluster,omitempty" jsonschema:"restrict to one topic by its stable handle (from the graph surfaces)"`
	// V-W6: same two fields, same semantics as on queryInput — `types` cuts
	// against the visible allowlist. This is STRICTER than the identically
	// named field on REST /api/search, which is a pure opt-in bind parameter
	// and deliberately keeps retrieval-excluded types browseable (searchRequest,
	// context_search.go). The MCP tools are a model's retrieval surface, not the
	// operator browse route, and the A/B measurement they exist for must not be
	// able to name a type retrieval policy keeps out.
	Types        []string `json:"types,omitempty" jsonschema:"restrict results to these block types; each name must be a retrieval-visible type (unknown or non-visible name is an error)"`
	TypesExclude []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types from the results"`
}

type getInput struct {
	ID string `json:"id" jsonschema:"block UUID"`
}

type recentInput struct {
	Limit    int    `json:"limit,omitempty" jsonschema:"max blocks to return (default 10, max 50)"`
	Category string `json:"category,omitempty" jsonschema:"filter by category"`
	// WF T10: opt-in server-side type filters (bind parameters; the recent
	// surface lives here + the chat ctx_recent tool — there is no REST
	// /api/recent route). block_roles_exclude is the legacy alias for
	// types_exclude (seam 17); both present ⇒ union.
	Types             []string `json:"types,omitempty" jsonschema:"only these block types (e.g. knowledge, audit-trail)"`
	TypesExclude      []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types"`
	BlockRolesExclude []string `json:"block_roles_exclude,omitempty" jsonschema:"legacy alias for types_exclude"`
}

func registerTools(server *mcp.Server, cfg MCPConfig) {
	// query
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Description: "Hybrid semantic + fulltext search with LLM synthesis. Ask a question, get a sourced answer.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpQueryHandler(cfg))

	// store
	mcp.AddTool(server, &mcp.Tool{
		Name:        "store",
		Description: "Save or update a knowledge block. Upserts on (category, title, scope). Writes the key's home scope unless an explicit scope the key may write is given.",
	}, mcpStoreHandler(cfg))

	// search
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Lightweight search without LLM synthesis. Returns matching blocks with preview.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpSearchHandler(cfg))

	// get
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get",
		Description: "Fetch a full block by ID.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpGetHandler(cfg))

	// recent
	mcp.AddTool(server, &mcp.Tool{
		Name:        "recent",
		Description: "List recently created or updated blocks. Useful for temporal queries like 'what was saved this week'.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpRecentHandler(cfg))

	// update (F6-C6 D-W6a): field-level block update, REST manage-update
	// parity. Stages for confirm_writes keys (op 'update', TOCTOU-pinned).
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update",
		Description: "Update fields of an existing block (category, title, content, tags, metadata). Resolves the id within this key's writable scopes.",
	}, mcpUpdateHandler(cfg))

	// confirm (F6-C6 D-W5/W6a): executes a staged write by payload hash. Only
	// meaningful for confirm_writes keys — for everyone else every hash is a
	// generic miss (the store/update tools never stage for them).
	mcp.AddTool(server, &mcp.Tool{
		Name:        "confirm",
		Description: "Execute a staged write. A key with the confirm_writes capability gets store/update calls STAGED (response carries a payload_hash); calling confirm with that hash executes the exact server-held write.",
	}, mcpConfirmHandler(cfg))

	// W12: issue-content write tools (issue_create/issue_comment/issue_state,
	// E5 (a) — create+comment+state, no delete). Registered in their own file.
	registerIssueTools(server, cfg)

	// Guard W3: review-queue tools (guard_list/guard_resolve), REST manage
	// guard-list/guard-resolve parity incl. the ids[] batch contract.
	registerGuardTools(server, cfg)

	// W02-8: blob_store/blob_fetch on the shared blob write core — one scope
	// path with /api/blob/store, staging parity for confirm_writes keys, and
	// ranged reads so a payload is drilled into rather than pulled whole.
	registerBlobTools(server, cfg)
}

// Type filters of the MCP retrieval tools (V-W6, design/05 §7).

// mcpTypeFilterUnwired is the fail-closed answer when a retrieval tool is asked
// for a type filter while the block-type registry is not wired: without a
// snapshot there is no visible set to cut against, and answering the request
// unfiltered would silently WIDEN what the caller asked to narrow.
const mcpTypeFilterUnwired = "type filter unavailable: block-type registry not wired"

// resolveMCPVisibleTypes turns the `types` argument of the MCP retrieval tools
// into the effective allowlist: intersect(set.VisibleTypes(), requested).
//
// `types` CUTS, it does not assign. Two rejections keep that honest instead of
// silent:
//
//   - an UNKNOWN name (not in the registry snapshot) is a caller error. Folding
//     it into an empty intersection would answer a typo with "nothing matched",
//     which reads as a statement about the corpus.
//   - a name that EXISTS but is not retrieval-visible (checkpoint, system-meta)
//     is refused as well. Admitting it would widen retrieval visibility for
//     EVERY key without an admin gate — the very surface design/05 §4.2 gates
//     sevenfold — and dropping it silently would let the caller believe the
//     filter applied. There is no admin bypass here; shadow visibility is its
//     own wave (M-W2).
//
// recentInput's `types` (a raw `type_name = ANY($n)` with no policy check at
// all, mcpRecentHandler below) is deliberately NOT the model.
//
// An empty/absent request list returns (nil, "") and the caller then passes nil
// — behaviour byte-identical to the time before the field existed. The second
// return value is the rejection text, empty when there is none.
func resolveMCPVisibleTypes(set *blocktype.Set, requested []string) ([]string, string) {
	if len(requested) == 0 {
		return nil, ""
	}
	if set == nil {
		return nil, mcpTypeFilterUnwired
	}
	visible := make(map[string]bool, len(set.VisibleTypes()))
	for _, name := range set.VisibleTypes() {
		visible[name] = true
	}
	cut := make([]string, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		if _, ok := set.Resolve(name); !ok {
			return nil, fmt.Sprintf("unknown block type %q", name)
		}
		if !visible[name] {
			return nil, fmt.Sprintf("block type %q is not retrieval-visible", name)
		}
		if !seen[name] {
			seen[name] = true
			cut = append(cut, name)
		}
	}
	return cut, ""
}

// mcpQueryTypesExclude folds the query tool's positive cut into the ONE seam
// /api/query already speaks: types_exclude → p_types_exclude.
//
// ctx_rrf applies `type_name = ANY(p_types_visible) AND type_name !=
// ALL(p_types_exclude)` (145:435-436, the CURRENT definition — the body moved
// 139 → 140 → 145) and p_types_visible is the handler's own Set.VisibleTypes(),
// so excluding the COMPLEMENT of the cut inside that set IS the cut — one
// filter mechanism, no second one, and no positive `types` on the REST request
// (queryRequest stays restrict-only by decision, query.go).
//
// Since migration 145 the two FTS arms carry a THIRD conjunct,
// `type_name NOT IN ('checkpoint','system-meta')` (145:500, 536), so the planner
// can use the partial FTS GIN indexes. It does not affect this seam: both names
// are retrieval.policy='excluded' and therefore never in Set.VisibleTypes(), so
// neither the cut nor its complement can ever contain them.
//
// cut is non-empty only after resolveMCPVisibleTypes accepted it, which implies
// set != nil.
func mcpQueryTypesExclude(set *blocktype.Set, cut, explicitExclude []string) []string {
	if len(cut) == 0 {
		return explicitExclude
	}
	keep := make(map[string]bool, len(cut))
	for _, name := range cut {
		keep[name] = true
	}
	visible := set.VisibleTypes()
	complement := make([]string, 0, len(visible))
	for _, name := range visible {
		if !keep[name] {
			complement = append(complement, name)
		}
	}
	// unionExcludes dedupes and is monotone-restrictive — the same fold the REST
	// handler runs over types_exclude ∪ block_roles_exclude. The MCP fields get
	// NO legacy alias of their own.
	return unionExcludes(explicitExclude, complement)
}

// mcpTypeSnapshot is the per-call registry view the retrieval tools cut against.
// nil when the registry is not wired; resolveMCPVisibleTypes then fails closed
// for a request that actually asked for a filter.
func (cfg MCPConfig) mcpTypeSnapshot(ctx context.Context) *blocktype.Set {
	if cfg.Blocktypes == nil {
		return nil
	}
	return cfg.Blocktypes.SnapshotForRequest(ctx)
}

// mcpQuerySource is the part of /api/query's source DTO (sourceResponse,
// query.go) the query tool renders. CitationIndex is deliberately NOT
// `omitempty`-shaped as a value: absent and 0 have to stay distinguishable,
// because absent means "this source never entered the prompt" while a number
// means "the model saw it under exactly this id".
type mcpQuerySource struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Category      string `json:"category"`
	CitationIndex *int   `json:"citation_index"`
}

// mcpNoCitationMarker replaces the number for a source that never entered the
// prompt. ASCII on purpose, and deliberately not a dash: it must be impossible
// to read as an ordinal (or as a truncated one) by the model consuming this
// text, and it has to survive transports that drop non-ASCII runes.
const mcpNoCitationMarker = "[n/a]"

// untrustedMarker frames a row of a `retrieval.untrusted` type in a TEXT
// rendering (V-11, design/02 §5.1 BA7 layer 3).
//
// The JSON-rendering tools (`search`, `get`) carry the framing as the
// serialized `untrusted` field of the response type. `recent` renders prose,
// so it needs a token — and it is the one retrieval tool that does not go
// through store.RecentBlocks at all (it runs its own statement below), which is
// exactly the kind of second path BA7 was written about.
//
// ASCII and bracketed like mcpNoCitationMarker: it must survive transports that
// drop non-ASCII runes, and it must not read as part of the block's own title
// or preview text.
const untrustedMarker = "[untrusted]"

// renderMCPQuerySources writes the tool's "Sources:" section.
//
// The number in front of a source is the <source id="N"> ordinal it carried in
// the prompt (`citation_index`, V-W1), not its position in the response. For
// n >= 3 the two differ: the response keeps retrieval order while the prompt
// list went through the low-confidence cap (llm/synthesize.go:710-712), the
// token budget (fitSourcesToBudget, :549-613) and LostInMiddleReorder
// (:322-331). Numbering by
// position therefore printed "[2]" in front of a source that the answer's "[2]"
// does not name — the same offset V-W1 closed on the REST seam, one level up.
//
// The cited sources are sorted by that ordinal so the list reads in the order
// the answer refers to it. Sources without an ordinal never reached the prompt,
// so the answer cannot be citing them: they keep the response order behind the
// cited ones and are marked instead of numbered.
//
// A response in which NO source carries an ordinal is a retrieval-only answer
// (or a server from before V-W1). That list keeps position numbering and is
// byte-identical to what this tool emitted before V-W1b.
func renderMCPQuerySources(sb *strings.Builder, sources []mcpQuerySource) {
	if len(sources) == 0 {
		return
	}
	cited := make([]mcpQuerySource, 0, len(sources))
	uncited := make([]mcpQuerySource, 0, len(sources))
	for _, s := range sources {
		if s.CitationIndex == nil {
			uncited = append(uncited, s)
			continue
		}
		cited = append(cited, s)
	}

	sb.WriteString("\n\nSources:\n")
	if len(cited) == 0 {
		for i, s := range sources {
			fmt.Fprintf(sb, "[%d] %s (%s) id:%s\n", i+1, s.Title, s.Category, s.ID)
		}
		return
	}
	// Stable: two sources can only share an ordinal if the server contradicts
	// itself, and even then the response order decides — never the sort.
	sort.SliceStable(cited, func(i, j int) bool {
		return *cited[i].CitationIndex < *cited[j].CitationIndex
	})
	for _, s := range cited {
		fmt.Fprintf(sb, "[%d] %s (%s) id:%s\n", *s.CitationIndex, s.Title, s.Category, s.ID)
	}
	for _, s := range uncited {
		fmt.Fprintf(sb, "%s %s (%s) id:%s\n", mcpNoCitationMarker, s.Title, s.Category, s.ID)
	}
}

// Tool handlers.

func mcpQueryHandler(cfg MCPConfig) mcp.ToolHandlerFor[queryInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input queryInput) (*mcp.CallToolResult, any, error) {
		if input.Question == "" {
			return errResult("question is required"), nil, nil
		}
		limit := input.Limit
		if limit <= 0 {
			limit = 5
		}

		// V-W6 type filter, resolved BEFORE the delegation so a rejected filter
		// never costs a retrieval round trip. ONE registry snapshot per call:
		// the cut and its complement must not come from two generations.
		typeSet := cfg.mcpTypeSnapshot(ctx)
		cut, rejection := resolveMCPVisibleTypes(typeSet, input.Types)
		if rejection != "" {
			return errResult(rejection), nil, nil
		}

		// Delegate to the internal /api/query handler for full pipeline
		// (temporal, embedding, RRF, gravity, rerank, supersedes, synthesis).
		// The ctx already carries the AuthResult from MCP auth middleware.
		payload := map[string]any{"query": input.Question, "limit": limit}
		// The key is added ONLY when a filter was asked for, so a request
		// without both fields marshals to the exact bytes it did before V-W6.
		if excl := mcpQueryTypesExclude(typeSet, cut, input.TypesExclude); len(excl) > 0 {
			payload["types_exclude"] = excl
		}
		body, _ := json.Marshal(payload)
		internalReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/query", io.NopCloser(strings.NewReader(string(body))))
		internalReq.Header.Set("Content-Type", "application/json")

		rec := &responseRecorder{headers: make(http.Header)}
		cfg.QueryHandler.ServeHTTP(rec, internalReq)

		var qr struct {
			Answer     string           `json:"answer"`
			Confidence string           `json:"confidence"`
			Sources    []mcpQuerySource `json:"sources"`
			Error      string           `json:"error"`
		}
		if err := json.Unmarshal(rec.body, &qr); err != nil {
			return errResult("failed to parse query response"), nil, err
		}
		if qr.Error != "" {
			return errResult(qr.Error), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(qr.Answer)
		renderMCPQuerySources(&sb, qr.Sources)

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(sb.String())},
		}, nil, nil
	}
}

func mcpStoreHandler(cfg MCPConfig) mcp.ToolHandlerFor[storeInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input storeInput) (*mcp.CallToolResult, any, error) {
		// Gap-C6-c: the store tool's rejections carry the machine code of
		// their class in StructuredContent — the same code /api/store emits
		// for the same class, even where the two surfaces word it differently
		// (this sentence vs "Missing required fields: …"). The text itself is
		// untouched.
		if input.Category == "" || input.Title == "" || input.Content == "" {
			return classMissingFields.errResult("category, title, and content are required"), nil, nil
		}

		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return classUnauthorized.errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		// The scope gate, ahead of the NOOP check (E-M4). Both halves of that
		// placement are load-bearing:
		//
		//   - the NOOP check is scoped, so it has to run against the scope
		//     this call would WRITE. Resolving it afterwards would ask the
		//     home scope whether a write to a foreign scope is a no-op, and
		//     answer "No change needed" for a block that does not exist
		//     where the caller asked for it. REST has always ordered it this
		//     way (context_store.go: scope gate, then HashNOOPCheck).
		//   - a refusal here also precedes the D-W5 stage branch, so a
		//     flagged key never gets a CARD for a scope it may not write.
		//
		// The gate chain below evaluates the same function again; it is pure,
		// so the second evaluation cannot disagree with this one.
		scope, _, scopeRej := resolveWriteScope(ar, input.Scope)
		if scopeRej != nil {
			return errResultReject(scopeRej), nil, nil
		}

		// Hash NOOP check. Runs BEFORE the D-W5 stage branch on purpose: an
		// identical-content call is a no-op for flagged keys too — no card
		// for a write that would change nothing (D-W2 note 3).
		existingID, err := store.HashNOOPCheck(ctx, cfg.Pool, input.Content, scope, input.Category, input.Title)
		if err == nil && existingID != "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("No change needed — identical content already exists (id: %s)", existingID))},
			}, nil, nil
		}

		// D-W5 branch (F6-C6): a confirm_writes key (090) stages instead of
		// executing — and answers IsError=true (D3-C3). Handler-level on
		// purpose — digest/dream write through store.UpsertBlock internally and
		// must never self-stage. Keys without the flag fall through to the
		// direct path below (fail-open, D-E2). Since Gap-C6-a the two arms run
		// the IDENTICAL gate chain (runStageWriteGates) and differ only in what
		// they do with the verdict: execute now, or stage for a confirm.
		if ar.ConfirmWrites {
			return mcpStageStore(ctx, cfg, ar, input)
		}

		// One config snapshot for the whole gate chain. MT 06-C5: per-tenant via
		// the request context (the MCP ctx carries the resolved AuthResult, used
		// for the scope above) — the tenant's own overrides apply, else the
		// _global values.
		rateLimit := 0
		var defaultSens backends.Sensitivity
		if cfg.Cfg != nil {
			snap := cfg.Cfg.SnapshotForRequest(ctx)
			rateLimit = snap.Query.RateLimitWrite
			defaultSens = snap.Pool.DefaultBlockSensitivity
		}
		// One registry snapshot for the gate chain AND the classify hook below —
		// a single per-call view, never the compiled-in builtin set.
		var set *blocktype.Set
		if cfg.Blocktypes != nil {
			set = cfg.Blocktypes.SnapshotForRequest(ctx)
		}

		// Gap-C6-a: the direct arm runs the SAME chain as the staged one
		// (runStageWriteGates) instead of a hand-rolled subset. H-W8 wired up
		// only the rate limit, which left the direct arm without the size cap
		// and without the G40 credentials detector: a 51 KB block and a leaked
		// vendor token both walked in here, while the identical payload was
		// rejected / upgraded on REST /api/store and on the staged MCP arm.
		// Running the chain closes that split by construction — a gate added to
		// the chain now reaches all three surfaces at once.
		//
		// Scope IS a field since E-M4 (superseding D4): empty resolves to the
		// home scope with ScopeExplicit=false — byte-identical to the value this
		// handler passed before — and a named scope runs the chain's scope gate,
		// which is resolveWriteScope, the same one REST /api/store runs. The
		// type IS a field since N-26: empty keeps the auto-classify behaviour, a
		// named type is validated by the chain (validateTypeNameAgainstSet) and
		// written as manual provenance.
		//
		// res is threaded into the upsert (sensitivity, detector metadata,
		// scope): the gates must DECIDE the write, not merely veto it. The chain
		// checks the rate limit exactly ONCE; the booking below is what feeds it.
		res, rej := runStageWriteGates(ctx, cfg.Pool, set, ar, storeRequest{
			Category:    input.Category,
			Title:       input.Title,
			Content:     input.Content,
			Tags:        input.Tags,
			Metadata:    input.Metadata,
			Sensitivity: input.Sensitivity,
			Type:        input.Type,
			Scope:       input.Scope,
		}, defaultSens, rateLimit, RequestIDFromContext(ctx))
		if rej != nil {
			return errResultReject(rej), nil, nil
		}

		// Upsert.
		block, err := store.UpsertBlock(ctx, cfg.Pool, input.Category, input.Title, input.Content, input.Tags, res.Metadata, res.WriteScope, res.ScopeExplicit, res.Sens, input.Type)
		if err != nil {
			// I7/S3: same sentinel, same 403 class as REST — the store decided
			// it, this arm only renders it.
			return errResultReject(provenanceRejectOr(err,
				classInternal.reject(fmt.Sprintf("store failed: %v", err)))), nil, nil
		}

		// Welle 44 / WF T4: Auto-classify type_name from the registry
		// snapshot. MCP audit-blocks (welle promotes, ctx-system docs) used to
		// need a follow-up SQL UPDATE — handled inline. Errors are LOGGED, not
		// silently dropped (T4 gate: the pre-T4 `_, _, _ =` discarded a failed
		// classification without a trace).
		if _, err := store.ClassifyBlockAfterUpsert(ctx, cfg.Pool, set, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("mcp: auto-classify failed", "error", err, "block_id", block.ID)
		}

		// H-W8: book the write, same (api_key_id, block_id, 'write') shape the
		// REST arm books in context_store.go. This row IS the rate-limit counter
		// — without it the gate above can never see an MCP write. Inline rather
		// than the REST arm's fire-and-forget goroutine, matching this handler's
		// established style (temporal enrichment below is inline too — MCP calls
		// are not latency-sensitive) and closing the race a detached booking
		// would leave: back-to-back MCP writes can no longer slip past the limit
		// while the counter is still in flight. A failed booking is logged, never
		// fatal: the block is written, and losing an audit row must not turn a
		// completed write into an error result.
		if err := store.LogAccess(ctx, cfg.Pool, ar.ApiKeyID, block.ID, "write"); err != nil {
			slog.Error("mcp: write log error", "error", err, "block_id", block.ID)
		}

		// Temporal enrichment (inline, not async — MCP calls are not latency-sensitive).
		times := store.ExtractDates(block.Content)
		_ = store.UpdateContentTimes(ctx, cfg.Pool, block.ID, times)
		_ = store.PopulateTemporal(ctx, cfg.Pool, block.ID, times, block.CreatedAt)

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Stored: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
		}, nil, nil
	}
}

func mcpSearchHandler(cfg MCPConfig) mcp.ToolHandlerFor[searchInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scopes := ar.ReadScopes

		// V-W6 type filter, ahead of every pool touch: a rejected filter must
		// not cost a grant lookup or a search statement. The SAME snapshot then
		// frames the untrusted rows (V-11) — flag and admission to retrieval can
		// never come from two registry generations.
		typeSet := cfg.mcpTypeSnapshot(ctx)
		cut, rejection := resolveMCPVisibleTypes(typeSet, input.Types)
		if rejection != "" {
			return errResult(rejection), nil, nil
		}

		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}

		// C6 facet, same three rules as the REST handler: flag first (off ⇒ the
		// field does not exist), then form (before the uuid cast), never an
		// existence check. cfg.Cfg is nil in tests without config wiring — a nil
		// there means "not configured", which reads as off, the fail-closed
		// direction for a dark feature.
		var clusterFacet *string
		if cfg.Cfg != nil && cfg.Cfg.SnapshotForRequest(ctx).ClusterOps.FacetEnabled && input.Cluster != "" {
			if !fullUUIDRe.MatchString(input.Cluster) {
				return errResult("cluster must be a full UUID"), nil, nil
			}
			clusterFacet = &input.Cluster
		}

		grants := resolveGrants(ctx, cfg.Pool, ar)
		// cut/TypesExclude ride the store layer's EXISTING type parameters (WF
		// T10) — the same bind parameters /api/search fills. nil/nil when the
		// caller named neither field, i.e. the pre-V-W6 call.
		results, err := store.SearchBlocks(ctx, cfg.Pool, typeSet, input.Query, scopes, input.Category, input.Tags, limit, true, nil, grants, cut, input.TypesExclude, clusterFacet)
		if err != nil {
			return errResult(fmt.Sprintf("search failed: %v", err)), nil, nil
		}

		data, _ := json.MarshalIndent(results, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(data))},
		}, nil, nil
	}
}

func mcpGetHandler(cfg MCPConfig) mcp.ToolHandlerFor[getInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return errResult("id is required"), nil, nil
		}

		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scopes := ar.ReadScopes

		grants := resolveGrants(ctx, cfg.Pool, ar)
		resolvedID, matches, err := store.ResolveBlockID(ctx, cfg.Pool, input.ID, scopes, grants)
		if err != nil {
			if errors.Is(err, store.ErrAmbiguousID) {
				body := map[string]any{
					"error":   "Ambiguous id prefix — re-specify with a longer prefix or full id",
					"matches": matches,
				}
				data, _ := json.MarshalIndent(body, "", "  ")
				return errResult(string(data)), nil, nil
			}
			return errResult(fmt.Sprintf("resolve failed: %v", err)), nil, nil
		}
		if resolvedID == "" {
			return errResult("block not found"), nil, nil
		}

		block, err := store.GetBlock(ctx, cfg.Pool, cfg.mcpTypeSnapshot(ctx), resolvedID, scopes, grants)
		if err != nil {
			return errResult(fmt.Sprintf("get failed: %v", err)), nil, nil
		}

		data, _ := json.MarshalIndent(block, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(data))},
		}, nil, nil
	}
}

func mcpRecentHandler(cfg MCPConfig) mcp.ToolHandlerFor[recentInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input recentInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scopes := ar.ReadScopes

		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}

		if err := store.RequireScopes(scopes); err != nil { // T07 fail-closed: this INLINE query bypasses the store-layer guards
			return errResult("unauthorized: no resolved scopes"), nil, nil
		}
		grants := resolveGrants(ctx, cfg.Pool, ar)
		// $1=scopes, $2=grants (block-grant OR-arm, T40a). The mandatory
		// parentheses keep NOT is_archived OUTSIDE the scope/grant OR (a granted
		// archived block must not leak). category, if present, shifts to $3.
		// type_name rides along for the V-11 untrusted framing only; it is not
		// rendered as a name (registry vocabulary is not the model's decision),
		// just resolved to the trust class through the registry snapshot.
		typeSet := cfg.mcpTypeSnapshot(ctx)
		query := `SELECT id, title, category, LEFT(content, 200) AS preview, updated_at, COALESCE(type_name, '')
			FROM context_blocks
			WHERE NOT is_archived AND ( scope = ANY($1::text[]) OR id = ANY($2::uuid[]) )`
		args := []any{scopes, grants}

		if input.Category != "" {
			args = append(args, input.Category)
			query += fmt.Sprintf(` AND category = $%d`, len(args))
		}
		// WF T10: opt-in server-side type filters (bind parameters; alias
		// union — see recentInput).
		if len(input.Types) > 0 {
			args = append(args, input.Types)
			query += fmt.Sprintf(` AND type_name = ANY($%d::text[])`, len(args))
		}
		if excl := unionExcludes(input.TypesExclude, input.BlockRolesExclude); len(excl) > 0 {
			args = append(args, excl)
			query += fmt.Sprintf(` AND NOT (type_name = ANY($%d::text[]))`, len(args))
		}
		query += ` ORDER BY updated_at DESC LIMIT ` + fmt.Sprintf("%d", limit)

		rows, err := cfg.Pool.Query(ctx, query, args...)
		if err != nil {
			return errResult(fmt.Sprintf("recent failed: %v", err)), nil, nil
		}
		defer rows.Close()

		var sb strings.Builder
		i := 0
		for rows.Next() {
			var id, title, category, preview, typeName string
			var updatedAt time.Time
			if err := rows.Scan(&id, &title, &category, &preview, &updatedAt, &typeName); err != nil {
				continue
			}
			i++
			trust := ""
			if typeSet != nil && typeSet.IsUntrusted(typeName) {
				trust = " " + untrustedMarker
			}
			fmt.Fprintf(&sb, "[%d] %s (%s, %s) id:%s%s\n    %s\n", i, title, category, updatedAt.Format("2006-01-02"), id, trust, preview)
		}

		if i == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent("No blocks found.")},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(sb.String())},
		}, nil, nil
	}
}

// Helpers.

// resolveGrants resolves the block-grant set for the caller's tenant (T40a,
// design/07 §4) and is FAIL-CLOSED for grant visibility: on any resolver error
// it logs and returns an empty set so the read proceeds scope-only — a grant
// lookup failure must never crash a read or silently widen visibility.
//
// TENANT-DECISION(authresult-tenantid-shape): the design/07 briefing assumed
// auth.AuthResult.TenantID *string (nullable). The canonical type is a plain
// string (auth.go:24), empty in the sentinel/no-tenant paths. store.GrantedBlockIDs
// already short-circuits an empty/whitespace tenantID to []string{}, so passing
// ar.TenantID directly preserves the intended semantics (empty tenant ⇒ '{}'
// ⇒ no-op OR-arm) without a nil deref. design/07 §4.1.
func resolveGrants(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult) []string {
	grants, err := store.GrantedBlockIDs(ctx, pool, ar.TenantID)
	if err != nil {
		// fail-closed for grant visibility: proceed scope-only, never crash the read
		slog.Warn("mcp: resolve granted block ids failed — scope-only visibility", "error", err)
		return []string{}
	}
	return grants
}

func textContent(text string) mcp.Content {
	return &mcp.TextContent{Text: text}
}

func errResult(msg string) *mcp.CallToolResult {
	r := &mcp.CallToolResult{
		Content: []mcp.Content{textContent(msg)},
	}
	r.IsError = true
	return r
}

// responseRecorder captures an HTTP response for internal handler delegation.
type responseRecorder struct {
	headers    http.Header
	body       []byte
	statusCode int
}

func (r *responseRecorder) Header() http.Header  { return r.headers }
func (r *responseRecorder) WriteHeader(code int) { r.statusCode = code }
func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}

// Flush and SetWriteDeadline are no-ops so HandleQuery's body-heartbeat
// (always armed when rerank is on, query.go) does not emit a "not flushable" /
// "cannot extend write deadline" warning per internal delegation — that
// regression alarm must stay sharp on the REAL HTTP path. The heartbeat's
// leading-whitespace writes land in the recorded body; RFC-8259 tolerates
// leading whitespace, so the JSON decode stays correct. Covers the MCP query
// delegation and the F6 ctx_query tool delegation alike.
func (r *responseRecorder) Flush()                           {}
func (r *responseRecorder) SetWriteDeadline(time.Time) error { return nil }
