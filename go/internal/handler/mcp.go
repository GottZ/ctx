// mcp.go — MCP (Model Context Protocol) server for ctx.
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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
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
	// (pool.default_block_sensitivity, F3 §3.5) and the types the search and
	// recent tools book in the read record (mcp.read_log_types, #345) — one
	// snapshot per call.
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
//
// The SDK handler is wrapped in the /mcp keepalive (mcp_keepalive.go), which
// only the query tool arms: every other request passes through unchanged. The
// outer wrapper hands the request context to the query tool, which ends its
// delegation with it (mcp_cancel.go).
func NewMCPHandler(cfg MCPConfig) http.Handler {
	return withMCPRequestContext(withMCPKeepalive(newMCPSDKHandler(cfg)))
}

// newMCPSDKHandler is the bare MCP SDK handler, without the keepalive.
func newMCPSDKHandler(cfg MCPConfig) http.Handler {
	server := newMCPServer(cfg)
	return mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:    true, // No session state needed — each call is independent.
			JSONResponse: true, // Prefer JSON over SSE for simple request/response tools.
		},
	)
}

// newMCPServer is the MCP server with every tool registered and the panic
// guard (mcp_recover.go) around all of them. The guard is added LAST, so it
// is the outermost receiving middleware: anything installed after it would
// run outside the guard, on the same goroutine the SDK does not recover.
//
// The server is built once, so its ServerOptions carry no Instructions: the
// text is a hot setting, and mcpInstructionsMiddleware writes it into every
// initialize (and server/discover) answer from the snapshot of that request.
func newMCPServer(cfg MCPConfig) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "ctx",
			Version: "0.25.3",
		},
		&mcp.ServerOptions{},
	)
	registerTools(server, cfg)
	server.AddReceivingMiddleware(mcpInstructionsMiddleware(cfg))
	server.AddReceivingMiddleware(mcpRecover) // last = outermost: it guards the instructions middleware too
	return server
}

// mcpInstructionsHistory is the shipped history the MCP server and the
// settings API resolve mcp.instructions against — config.ShippedMCPInstructions
// in production, a package var as a test seam (pattern: embedcache's
// cacheProbe) so a test can stand in for a release that ships a newer wording.
var mcpInstructionsHistory = config.ShippedMCPInstructions

// mcpInstructionsFor is the text the next initialize answer carries: the
// resolution of mcp.instructions_enabled and mcp.instructions (#315) in the
// snapshot of this request. Without a config store (unit tests that wire no
// settings) it is the registry default — the newest shipped wording.
func mcpInstructionsFor(ctx context.Context, cfg MCPConfig) string {
	history := mcpInstructionsHistory()
	if cfg.Cfg == nil {
		return config.ResolveMCPInstructions(true, history[len(history)-1].Text, history).Sent
	}
	snap := cfg.Cfg.SnapshotForRequest(ctx)
	return config.ResolveMCPInstructions(snap.MCP.InstructionsEnabled, snap.MCP.Instructions, history).Sent
}

// mcpInstructionsMiddleware sets the instructions of every initialize and
// server/discover answer — the two results the SDK fills from
// ServerOptions.Instructions — from the current settings, so a settings write
// reaches the next connecting agent without a restart. Off (or nothing to
// send) leaves the field empty, which the wire omits.
func mcpInstructionsMiddleware(cfg MCPConfig) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			switch r := res.(type) {
			case *mcp.InitializeResult:
				r.Instructions = mcpInstructionsFor(ctx, cfg)
			case *mcp.DiscoverResult:
				r.Instructions = mcpInstructionsFor(ctx, cfg)
			}
			return res, err
		}
	}
}

// Tool definitions.

type queryInput struct {
	Question string `json:"question" jsonschema:"the question to answer using hybrid search + LLM synthesis"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max sources to return (default 5)"`
	// Category, Tags and CategoriesExclude are /api/query's own three fields
	// (queryRequest), forwarded under the same wire names and with its
	// semantics. They carry no new reach: each is a restrictive predicate
	// inside the scopes the key already reads, so unlike `types` below they
	// need no visibility check. Absent or empty ⇒ no key in the delegated
	// body; an empty category is "no filter", as on `search` and `recent`.
	// Like every /api/query filter they bind the rank-fusion candidates and
	// every stage after it that can add a block — cluster injection, graph
	// expansion, aggregate fold (#225).
	Category          string   `json:"category,omitempty" jsonschema:"retrieve only blocks of this category"`
	Tags              []string `json:"tags,omitempty" jsonschema:"retrieve only blocks that carry at least one of these tags"`
	CategoriesExclude []string `json:"categories_exclude,omitempty" jsonschema:"do not retrieve blocks of these categories"`
	// V-W6: the MCP-only type filters of the RETRIEVAL tools. `types` CUTS
	// against the request's visible allowlist — it never assigns visibility;
	// see resolveMCPVisibleTypes for the two rejections and for why
	// recentInput's raw predicate is deliberately not the model. Both fields
	// absent ⇒ the delegated request body is byte-identical to before they
	// existed.
	Types        []string `json:"types,omitempty" jsonschema:"restrict retrieval to these block types; each name must be a retrieval-visible type (unknown or non-visible name is an error)"`
	TypesExclude []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types from retrieval"`
	// Synthesize and IncludeContent are /api/query's own two fields
	// (queryRequest.Synthesize, .IncludeContent), forwarded under the same wire
	// names. Neither carries authority REST does not give the same key: REST
	// gates neither field, the retrieval-only path runs the same scope filter,
	// rate limit and tenant quota, and the content snippet is a prefix of a
	// block the key may already read in full with `get`. Absent ⇒ the delegated
	// body is byte-identical to before the fields existed.
	Synthesize     *bool `json:"synthesize,omitempty" jsonschema:"false = retrieval only: answer the ranked sources as JSON without the LLM synthesis (default true)"`
	IncludeContent bool  `json:"include_content,omitempty" jsonschema:"with synthesize false: attach each source's content (up to 1500 characters by default, settings key query.max_source_runes; a cut one carries truncated true, get the id for the full text); an error otherwise"`
}

type storeInput struct {
	Category    string         `json:"category" jsonschema:"block category (e.g. decisions, learnings, infrastructure)"`
	Title       string         `json:"title" jsonschema:"block title (upserts on same category+title+scope)"`
	Content     string         `json:"content" jsonschema:"block content (max 50KB)"`
	Tags        []string       `json:"tags,omitempty" jsonschema:"optional tags for filtering"`
	Metadata    map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object; put the source of the knowledge (URL, file, conversation, person) into metadata.source"`
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
	// Supersedes is /api/store's field of the same name (storeRequest.
	// Supersedes, issue #197), gated and applied by the same store function;
	// absent ⇒ the call is byte-identical to before the field existed.
	Supersedes string `json:"supersedes,omitempty" jsonschema:"optional id of a block in the same scope that this block replaces (a correction or newer version); the old block is kept as history and leaves dateless retrieval"`
}

type searchInput struct {
	Query    string   `json:"query,omitempty" jsonschema:"search text"`
	Category string   `json:"category,omitempty" jsonschema:"filter by category"`
	Tags     []string `json:"tags,omitempty" jsonschema:"filter by tags: blocks that carry at least one of these tags"`
	// TagsAll (#219) is POST /api/search's field of the same name, through the
	// same core: containment beside the overlap of `tags`. Absent ⇒ the call
	// binds exactly what it bound before the field existed.
	TagsAll []string `json:"tags_all,omitempty" jsonschema:"only blocks that carry all of these tags; combines with tags (all of these, and at least one of those)"`
	Limit   int      `json:"limit,omitempty" jsonschema:"max results (default 10, max 50)"`
	// After and IncludeContent (#206, #207) are POST /api/search's keyset
	// cursor and the inverse of its `compact`, through the same core. Either one
	// present switches the answer to the page object (mcp_search_page.go); a
	// call with neither keeps the row array byte for byte.
	After          *store.SearchCursor `json:"after,omitempty" jsonschema:"page through a listing with an empty query: {} for the first page, then the next_after of the previous answer; the answer is then an object with next_after (absent on the last page), limit and results"`
	IncludeContent bool                `json:"include_content,omitempty" jsonschema:"answer each block's full content and metadata instead of a 200-character preview; the answer is then an object with results, and a page ends early once its rows reach 64 KiB of JSON (truncated true, next_after continues with an empty query)"`
	// Cluster is the C6 topic facet — a FIELD on the existing tool, not an
	// eighth tool: a new tool raises the selection load of every MCP client,
	// an optional argument on a known one does not (design/03 §4.8).
	Cluster string `json:"cluster,omitempty" jsonschema:"restrict to one topic by its stable handle (from the graph surfaces)"`
	// V-W6, widened by #342: `types` names any type of the caller's registry
	// snapshot, the retrieval-excluded ones (checkpoint, system-meta, the
	// derived layers) included, and an unknown name is a tool error
	// (resolveMCPKnownTypes). The tool answers rows of those types without a
	// filter, so naming one only narrows an answer the key already gets; until
	// #342 the field cut against the retrieval-visible set like queryInput's,
	// and a client could not narrow a search to its checkpoint blocks. REST
	// /api/search binds the field without any check (an unknown name matches
	// nothing); the query tool keeps the retrieval-visible cut.
	Types        []string `json:"types,omitempty" jsonschema:"restrict results to these block types; any registered type, including checkpoint and system-meta, which query never ranks and a full-text search without types may leave out (an unknown name is an error)"`
	TypesExclude []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types from the results"`
	// BlockRolesExclude is the documented legacy alias of types_exclude (seam
	// 17), accepted here since T03-9 (E03-3 = A). Before that this tool was the
	// ONE surface of the three that dropped the name: REST /api/search unioned
	// it, the `recent` tool unioned it, and `search` answered a request carrying
	// it UNFILTERED and without an error — i.e. with more rows than the caller
	// asked for. Accepting it is purely additive: both names present ⇒ the union
	// applies (monotone-restrictive, it can never widen), and a caller that
	// never sends it binds byte-identically to before. Retiring the alias on all
	// three surfaces is a separate, announced break (E03-3 = C), not this.
	BlockRolesExclude []string `json:"block_roles_exclude,omitempty" jsonschema:"legacy alias for types_exclude"`
}

// coreArgs maps the tool's arguments onto the argument set both search
// transports share (search_core.go). Twin of searchRequest.coreArgs; the parity
// test holds the two against each other without a database.
//
// Limit rides in RAW: default and cap are the core's (searchLimit), and the
// handler compares the effective value with this one to report a lowered
// limit. The cap is the store's clamp at 50, which this tool has always run
// into; E03-2 = B / K28 records the surface as uncapped, and lifting the clamp
// for it is open there (search_core.go, LIMIT note).
func (input searchInput) coreArgs(facetEnabled, leaveOutNeverRanked bool) searchArgs {
	return searchArgs{
		Query:             input.Query,
		Category:          input.Category,
		Cluster:           input.Cluster,
		Tags:              input.Tags,
		TagsAll:           input.TagsAll,
		Types:             input.Types,
		TypesExclude:      input.TypesExclude,
		BlockRolesExclude: input.BlockRolesExclude,
		Limit:             input.Limit,
		// Compact unless include_content (#207); the cursor as given (#206),
		// read by the core exactly as REST's.
		Compact: !input.IncludeContent,
		After:   input.After,
		// `types` names registered types only: an unknown name is a tool error
		// here, while the REST browse route binds any name. That strictness is
		// the documented difference between the two and stays a visible field
		// rather than a hidden branch (mcp.go searchInput.Types).
		KnownTypesOnly:      true,
		FacetEnabled:        facetEnabled,
		LeaveOutNeverRanked: leaveOutNeverRanked,
	}
}

type getInput struct {
	ID string `json:"id" jsonschema:"block UUID"`
	// IncludeArchived (#201) is the opt-in read of an archived block, so a
	// citation by id stays checkable after the cited block was archived. Full
	// id only, the key's own scopes only (never through a block or scope grant),
	// and the answer carries archived: true. Absent ⇒ the call is answered
	// exactly as before the field existed.
	IncludeArchived bool `json:"include_archived,omitempty" jsonschema:"also read an archived block: full block id only, within this key's own scopes (never through a grant); the answer then carries archived: true"`
	// Fields and the content window (#407, get_fields.go): the keys and the
	// part of the content a reader takes, checked and answered by the same
	// functions as REST manage get. Absent ⇒ the whole block, as before.
	Fields        []string `json:"fields,omitempty" jsonschema:"answer only these keys of the block: id, category, tags, title, content, metadata, scope, sensitivity, sensitivity_source, type, lifecycle_state, type_source, untrusted, untrusted_origin, archived, created_at, updated_at. id always comes along, and untrusted, untrusted_origin and archived whenever they apply; omit for the whole block"`
	ContentOffset *int     `json:"content_offset,omitempty" jsonschema:"first character of the content to answer, counted in characters (code points) from 0; with content_offset or content_limit the answer adds content_length (the whole content's length) and content_truncated (true when the content goes on after the window)"`
	ContentLimit  *int     `json:"content_limit,omitempty" jsonschema:"at most this many characters (code points) of the content, from content_offset; 0 answers none and still reports content_length. When content_truncated is true the next window starts at content_offset + content_limit"`
}

type recentInput struct {
	Limit    int    `json:"limit,omitempty" jsonschema:"max blocks to return (default 10, max 50)"`
	Category string `json:"category,omitempty" jsonschema:"filter by category"`
	// WF T10: opt-in server-side type filters (bind parameters; the recent
	// surface lives here, on the chat ctx_recent tool and — since T03-8b — on
	// REST POST /api/recent, whose recentRequest mirrors these fields one for
	// one). block_roles_exclude is the legacy alias for types_exclude
	// (seam 17); both present ⇒ union.
	Types             []string `json:"types,omitempty" jsonschema:"only these block types (e.g. knowledge, audit-trail)"`
	TypesExclude      []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types"`
	BlockRolesExclude []string `json:"block_roles_exclude,omitempty" jsonschema:"legacy alias for types_exclude"`
}

func registerTools(server *mcp.Server, cfg MCPConfig) {
	// query
	addMCPTool(server, &mcp.Tool{
		Name:        "query",
		Description: "Hybrid semantic + fulltext search with LLM synthesis. Ask a question, get a sourced answer. With synthesize false: the ranked sources only, as JSON, without the synthesis step.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpQueryHandler(cfg))

	// store
	addMCPTool(server, &mcp.Tool{
		Name:        "store",
		Description: "Save or update a knowledge block. Upserts on (category, title, scope). Writes the key's home scope unless an explicit scope the key may write is given. With supersedes: the written block replaces that block of the same scope — the old block stays as history (a query with a date still finds it), a query without a date no longer returns it.",
	}, mcpStoreHandler(cfg))

	// store_batch (#211): several store writes in one transaction, all or
	// none. A tool of its own rather than a list argument on `store`: the
	// store tool's schema requires category, title and content, and a list
	// beside them would have to drop that requirement for every caller.
	registerStoreBatchTool(server, cfg)

	// search
	addMCPTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Lightweight search without LLM synthesis. Returns matching blocks with preview, at most 50 per call. With an empty query it lists the blocks of the filters newest first; pass after to page through all of them, and include_content to read their full text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpSearchHandler(cfg))

	// get
	addMCPTool(server, &mcp.Tool{
		Name:        "get",
		Description: "Fetch a block by ID: the whole block, or with fields only the named keys, and with content_offset/content_limit a window of its content.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpGetHandler(cfg))

	// recent
	addMCPTool(server, &mcp.Tool{
		Name:        "recent",
		Description: "List recently created or updated blocks. Useful for temporal queries like 'what was saved this week'.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpRecentHandler(cfg))

	// update (F6-C6 D-W6a): field-level block update, REST manage-update
	// parity. Stages for confirm_writes keys (op 'update', TOCTOU-pinned).
	// `type` since issue #203 (mcp_update.go).
	addMCPTool(server, &mcp.Tool{
		Name:        "update",
		Description: "Update fields of an existing block (category, title, content, tags, metadata, type). To change part of the content, send edits (exact old → new replacements) instead of content; to add or remove metadata keys, send metadata_merge instead of metadata. Resolves the id within this key's writable scopes. A new type is checked against the block-type registry like the store tool's and kept as a manual choice; leaving a type marked untrusted records that type on the block as untrusted_origin, and a comment or an issue keeps its type.",
	}, mcpUpdateHandler(cfg))

	// archive (#201): manage-delete parity — archives one block the key may
	// write. Stages for confirm_writes keys (op 'archive', drift-pinned).
	addMCPTool(server, &mcp.Tool{
		Name:        "archive",
		Description: "Archive a block: it leaves query, search, recent and get, and stays readable by full id with get include_archived. Resolves the id within this key's writable scopes. There is no unarchive tool.",
	}, mcpArchiveHandler(cfg))

	// confirm (F6-C6 D-W5/W6a): executes a staged write by payload hash. Only
	// meaningful for confirm_writes keys — for everyone else every hash is a
	// generic miss (the store/update tools never stage for them).
	addMCPTool(server, &mcp.Tool{
		Name:        "confirm",
		Description: "Execute a staged write. A key with the confirm_writes capability gets store/update/archive calls STAGED (response carries a payload_hash); calling confirm with that hash executes the exact server-held write.",
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

// resolveMCPVisibleTypes turns the `types` argument of the MCP query tool into
// the effective allowlist: intersect(set.VisibleTypes(), requested). (The
// search tool used it too until #342; it now takes resolveMCPKnownTypes.)
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

// resolveMCPKnownTypes turns the `types` argument of the MCP search tool into
// the list the statement binds (#342). It admits every type of the request's
// registry snapshot, the retrieval-excluded ones included. The search tool
// answers rows of those types without a filter, so naming one narrows an answer
// the key already gets and grants nothing: the statement still applies the
// key's read scopes and grants. Retrieval visibility matters where a type's
// policy decides whether it is ranked at all — the query tool, which keeps
// resolveMCPVisibleTypes.
//
// An unknown name stays a refusal, worded as before, so a typo does not read as
// "nothing matched". The snapshot is the request's tenant-resolved one
// (Registry.SnapshotForRequest): a type another tenant registered for itself is
// not in it and is refused exactly like a name nobody registered, so the answer
// says nothing about other tenants' types.
//
// An empty/absent list returns (nil, "") — the caller binds no type filter, as
// before the field existed. Duplicates are dropped; the second return value is
// the refusal text, empty when there is none.
func resolveMCPKnownTypes(set *blocktype.Set, requested []string) ([]string, string) {
	if len(requested) == 0 {
		return nil, ""
	}
	if set == nil {
		return nil, mcpTypeFilterUnwired
	}
	known := make([]string, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, name := range requested {
		if _, ok := set.Resolve(name); !ok {
			return nil, fmt.Sprintf("unknown block type %q", name)
		}
		if !seen[name] {
			seen[name] = true
			known = append(known, name)
		}
	}
	return known, ""
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
	// handler runs over types_exclude ∪ block_roles_exclude. The QUERY tool's
	// two fields get no legacy alias of their own (the `search` tool took one in
	// T03-9/E03-3 = A, the retrieval-with-synthesis surface did not: nothing
	// ever sent the alias here, so adding it would create legacy rather than
	// keep faith with it).
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
	// SupersededBy (issue #197) is rendered on the source line, so an MCP
	// client in synthesis mode can tell that a cited source was replaced —
	// the retrieval-only JSON carries the field since #183.
	SupersededBy *string `json:"superseded_by"`
	// UntrustedOrigin (issue #280) is rendered on a line of its own under the
	// source line (mcpSourceLine).
	UntrustedOrigin string `json:"untrusted_origin"`
}

// mcpSourceLineTail is what follows "id:<id>" on a source line: nothing, or
// " superseded_by:<id>" for a source another block supersedes — so a line
// without it keeps its exact text.
func mcpSourceLineTail(s mcpQuerySource) string {
	if s.SupersededBy == nil || *s.SupersededBy == "" {
		return ""
	}
	return " superseded_by:" + *s.SupersededBy
}

// mcpSourceLine writes one source line of the "Sources:" list: the label (the
// citation ordinal, the position, or mcpNoCitationMarker), then title,
// category, id and mcpSourceLineTail; a source with an untrusted_origin record
// (issue #280) gets store.UntrustedOriginNote on an indented line below it.
// The note sits on a line of its own rather than in the tail so that a parser
// that reads a source line up to its id (the Hermes plugin's does) still reads
// the source, and a source without a record keeps its exact bytes.
func mcpSourceLine(sb *strings.Builder, label string, s mcpQuerySource) {
	fmt.Fprintf(sb, "%s %s (%s) id:%s%s\n", label, s.Title, s.Category, s.ID, mcpSourceLineTail(s))
	if note := store.UntrustedOriginNote(s.UntrustedOrigin); note != "" {
		sb.WriteString("    " + note + "\n")
	}
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
// so it needs a token: it reads its rows through store.RecentBlocks
// (mcpRecentHandler below), which fills BlockPreview.Untrusted per row, and the
// prose rendering has no field to carry that flag.
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
// A response in which NO source carries an ordinal comes from a server from
// before V-W1 (a retrieval-only answer never reaches this function: the handler
// renders it with renderMCPRetrievalOnly). That list keeps position numbering
// and is byte-identical to what this tool emitted before V-W1b.
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
			mcpSourceLine(sb, fmt.Sprintf("[%d]", i+1), s)
		}
		return
	}
	// Stable: two sources can only share an ordinal if the server contradicts
	// itself, and even then the response order decides — never the sort.
	sort.SliceStable(cited, func(i, j int) bool {
		return *cited[i].CitationIndex < *cited[j].CitationIndex
	})
	for _, s := range cited {
		mcpSourceLine(sb, fmt.Sprintf("[%d]", *s.CitationIndex), s)
	}
	for _, s := range uncited {
		mcpSourceLine(sb, mcpNoCitationMarker, s)
	}
}

// mcpRetrievalSource is one row of the query tool's retrieval-only answer: the
// subset of /api/query's sourceResponse a caller needs to pick blocks for
// `get`, under the same wire names. Untrusted (V-11) rides along because the
// row now carries foreign text — the content snippet, and the title in any
// case — and a JSON answer frames it the way `search` and `get` do.
// citation_index is left out: no prompt was built, so there is none.
type mcpRetrievalSource struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Category     string  `json:"category"`
	Score        float64 `json:"score"`
	SupersededBy *string `json:"superseded_by,omitempty"`
	Untrusted    bool    `json:"untrusted,omitempty"`
	// UntrustedOrigin (issue #280): /api/query's field under the same name.
	UntrustedOrigin string `json:"untrusted_origin,omitempty"`
	Content      string  `json:"content,omitempty"`
	// Truncated (#234): /api/query's flag on a cut snippet, under its name.
	Truncated bool `json:"truncated,omitempty"`
}

// renderMCPRetrievalOnly answers a synthesize:false call: the ranked sources in
// retrieval order as a JSON document, not the prose rendering. JSON because the
// rows may carry content, and a snippet inside a JSON string cannot pass itself
// off as another source line the way it could in the "Sources:" list.
func renderMCPRetrievalOnly(body []byte) (*mcp.CallToolResult, any, error) {
	var out struct {
		Confidence string               `json:"confidence"`
		Sources    []mcpRetrievalSource `json:"sources"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return errResult("failed to parse query response"), nil, err
	}
	if out.Sources == nil {
		out.Sources = []mcpRetrievalSource{}
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{textContent(string(data))},
	}, nil, nil
}

// Tool handlers.

func mcpQueryHandler(cfg MCPConfig) mcp.ToolHandlerFor[queryInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input queryInput) (*mcp.CallToolResult, any, error) {
		if input.Question == "" {
			return errResult("question is required"), nil, nil
		}
		// A client that disconnects ends the call, and with it the delegated
		// pipeline below (mcp_cancel.go): no backend call and no quota for an
		// answer nobody receives.
		ctx, release := mcpRequestBoundContext(ctx)
		defer release()
		// From here on the call can run for minutes (backfill, embedding,
		// rerank, synthesis) while the SDK holds the whole answer back. Keep
		// the /mcp response alive until this handler returns; stop runs
		// before the SDK writes its body (mcp_keepalive.go).
		stopKeepalive := armMCPKeepalive(ctx)
		defer stopKeepalive()
		limit := input.Limit
		if limit <= 0 {
			limit = 5
		}
		// /api/query attaches content on the retrieval-only path alone (the
		// synthesis path builds its sources with contentRunes 0), so
		// include_content next to synthesis would be a silent no-op. Refused
		// before any delegation, as /api/query refuses arm_ranks without
		// synthesize:false.
		retrievalOnly := input.Synthesize != nil && !*input.Synthesize
		if input.IncludeContent && !retrievalOnly {
			return errResult("include_content requires synthesize:false"), nil, nil
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
		// /api/query's own filters, forwarded unchanged and only when given.
		if input.Category != "" {
			payload["category"] = input.Category
		}
		if len(input.Tags) > 0 {
			payload["tags"] = input.Tags
		}
		if len(input.CategoriesExclude) > 0 {
			payload["categories_exclude"] = input.CategoriesExclude
		}
		// The key is added ONLY when a filter was asked for, so a request
		// without both fields marshals to the exact bytes it did before V-W6.
		if excl := mcpQueryTypesExclude(typeSet, cut, input.TypesExclude); len(excl) > 0 {
			payload["types_exclude"] = excl
		}
		// Same rule for the two retrieval-only fields: added only when given.
		if input.Synthesize != nil {
			payload["synthesize"] = *input.Synthesize
		}
		if input.IncludeContent {
			payload["include_content"] = true
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
			ErrorCode  string           `json:"error_code"`
		}
		if err := json.Unmarshal(rec.body, &qr); err != nil {
			return errResult("failed to parse query response"), nil, err
		}
		if qr.Error != "" {
			return errResult(qr.Error), nil, nil
		}
		// A refusal that carries only error_code (the tenant quota gate's
		// quota_exceeded, no_eligible_backend) must not read as an empty
		// answer. Before the heartbeat it is a real 4xx/5xx; on the synthesis
		// path the 200 is already committed and only the body says so. The
		// text names the code alone — the other fields of the body (the role and
		// required sensitivity a no_eligible_backend body carries) stay out
		// of the tool result.
		if qr.ErrorCode != "" {
			return errResultCoded(qr.ErrorCode, "query refused: "+qr.ErrorCode), nil, nil
		}
		if rec.statusCode >= http.StatusBadRequest {
			return errResult(fmt.Sprintf("query failed (HTTP %d)", rec.statusCode)), nil, nil
		}

		if retrievalOnly {
			return renderMCPRetrievalOnly(rec.body)
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
		//     way (context_store.go: scope gate, then its identical-content
		//     check — store.IdenticalContentMatch, which compares content,
		//     tags, metadata, sensitivity and type).
		//   - a refusal here also precedes the D-W5 stage branch, so a
		//     flagged key never gets a CARD for a scope it may not write.
		//
		// The gate chain below evaluates the same function again; it is pure,
		// so the second evaluation cannot disagree with this one.
		scope, _, scopeRej := resolveWriteScope(ar, input.Scope)
		if scopeRej != nil {
			return errResultReject(scopeRej), nil, nil
		}

		// Identical-content check. Runs BEFORE the D-W5 stage branch on purpose:
		// an identical write is a no-op for flagged keys too — no card for a
		// write that would change nothing (D-W2 note 3). The raw metadata and
		// sensitivity are the write intent: IdenticalContentMatch applies the
		// detector itself, and an omitted sensitivity leaves an existing row
		// untouched just as the resolved settings default does in UpsertBlock.
		//
		// A write that names a block it supersedes is never a no-op: the
		// declaration is part of the write (REST skips its shortcut likewise).
		match, err := store.IdenticalContentMatch(ctx, cfg.Pool, input.Content, scope, input.Category, input.Title,
			input.Tags, input.Metadata, store.SensitivityWrite{
				Value:  backends.Sensitivity(input.Sensitivity),
				Manual: input.Sensitivity != "",
			}, input.Type)
		if err == nil && match.Unchanged() && input.Supersedes == "" {
			noteToolBlock(ctx, match.ID)
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("No change needed — identical content already exists (id: %s)", match.ID))},
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
		block, err := upsertStoreBlock(ctx, cfg.Pool, set, ar.ApiKeyID, input.Category, input.Title, input.Content,
			input.Tags, res.Metadata, res.WriteScope, res.ScopeExplicit, res.Sens, input.Type, input.Supersedes)
		if err != nil {
			// I7/S3 and a refused supersedes declaration: same class as REST —
			// the store decided it, this arm only renders it.
			if rej := supersedesReject(err); rej != nil {
				return errResultReject(rej), nil, nil
			}
			if rej := retypeBoundReject(err); rej != nil {
				return errResultReject(rej), nil, nil
			}
			if rej := provenanceRejectOr(err, nil); rej != nil {
				return errResultReject(rej), nil, nil
			}
			// A server fault: its line names what the write left behind, as
			// POST /api/store's does (issue #453). The answer is unchanged.
			noteToolWrite(ctx, err)
			logRequestError(ctx, "mcp: store error", "error", err, "write_outcome", writeOutcome(err), "request_id", RequestIDFromContext(ctx))
			return errResultReject(classInternal.reject(fmt.Sprintf("store failed: %v", err))), nil, nil
		}
		noteToolWrite(ctx, nil)
		noteToolBlock(ctx, block.ID)

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
			logRequestError(ctx, "mcp: write log error", "error", err, "block_id", block.ID)
		}

		// Temporal enrichment (inline, not async — MCP calls are not latency-sensitive).
		_, _ = store.RefreshBlockTemporal(ctx, cfg.Pool, block)

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Stored: %s (id: %s, category: %s%s)", block.Title, block.ID, block.Category,
				supersedesSuffix(input.Supersedes)))},
		}, nil, nil
	}
}

// supersedesSuffix is the ", supersedes: <id>" the store answers carry when the
// write named a block it supersedes, and "" otherwise — so an answer without
// the field keeps its exact text.
func supersedesSuffix(supersedes string) string {
	if supersedes == "" {
		return ""
	}
	return ", supersedes: " + strings.ToLower(supersedes)
}

func mcpSearchHandler(cfg MCPConfig) mcp.ToolHandlerFor[searchInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		// The C6 flag out of ONE config snapshot for this call, handed to the
		// core as a value (search_core.go: the core reads no config, or the
		// gate would come from a second generation). cfg.Cfg is nil in tests
		// without config wiring — a nil there means "not configured", which
		// reads as off, the fail-closed direction for a dark feature.
		//
		// The same snapshot names the types the read record books (#345).
		var snap *config.Config
		if cfg.Cfg != nil {
			snap = cfg.Cfg.SnapshotForRequest(ctx)
		}
		facetEnabled := snap != nil && snap.ClusterOps.FacetEnabled
		// search.leave_out_never_ranked out of the same snapshot (#342); no
		// config wiring reads as off, the behaviour before the key.
		leaveOutNeverRanked := snap != nil && snap.Search.LeaveOutNeverRanked

		// REST leaves a cursor next to a query inert; here it is refused, so a
		// caller cannot take the repeated top matches for a second page (#206).
		if input.After != nil && input.Query != "" {
			return errResult(mcpSearchAfterNeedsBrowseMsg), nil, nil
		}
		if mcpSearchAfterIsHalf(input.After) {
			return errResult(mcpSearchAfterHalfMsg), nil, nil
		}

		// One rule set for both search transports (T03-9, search_core.go): the
		// V-W6 type cut ahead of every pool touch, the C6 facet gate, the
		// types_exclude ∪ block_roles_exclude union and the grant resolution.
		// The registry snapshot handed in is the SAME one that frames the
		// untrusted rows (V-11) — flag and admission to retrieval can never come
		// from two registry generations.
		page, err := executeSearch(ctx, cfg.Pool, cfg.mcpTypeSnapshot(ctx), ar, input.coreArgs(facetEnabled, leaveOutNeverRanked))
		if err != nil {
			var rejected *searchRejected
			switch {
			case errors.As(err, &rejected):
				// Caller error the core holds the prose for: the V-W6 type
				// rejections and the malformed facet handle, each with the text
				// this tool printed before the core existed, and the malformed
				// cursor.
				return errResult(rejected.Error()), nil, nil
			case errors.Is(err, store.ErrTooManyBlockGrants): // T04-20: refuse, never read scope-only
				return errResult(tooManyGrantsMsg), nil, nil
			default:
				return errResult(fmt.Sprintf("search failed: %v", err)), nil, nil
			}
		}

		res, answered := renderMCPSearch(input, page)
		store.RecordBlockReads(cfg.Pool, ar.ApiKeyID, store.ActionMCPSearch, readLogIDs(snap, answered))
		return res, nil, nil
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
		if archivedReadRefused(input.ID, input.IncludeArchived) {
			return errResult(archivedReadNeedsFullIDMsg), nil, nil
		}
		// #407: fields and the content window, checked before any lookup.
		opts, err := newGetOptions(input.Fields, input.ContentOffset, input.ContentLimit)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		scopes := ar.ReadScopes

		grants, err := resolveGrants(ctx, cfg.Pool, ar)
		if errors.Is(err, store.ErrTooManyBlockGrants) { // T04-20: refuse, never read scope-only
			return errResult(tooManyGrantsMsg), nil, nil
		}
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

		block, err := readBlockForGet(ctx, cfg.Pool, cfg.mcpTypeSnapshot(ctx), ar, resolvedID, grants, input.IncludeArchived, opts.omit())
		if err != nil {
			return errResult(fmt.Sprintf("get failed: %v", err)), nil, nil
		}
		// #201: a full id passes ResolveBlockID unchecked, so an unknown, a
		// foreign and an archived id all arrive here as nil — and answer what
		// the prefix miss above answers, not a JSON null.
		if block == nil {
			return errResult("block not found"), nil, nil
		}
		answer, err := opts.answer(block)
		if err != nil {
			return errResult(fmt.Sprintf("get failed: %v", err)), nil, nil
		}
		// The read record (#345): booked only for a block this answer hands
		// over — every refusal above leaves no row, as on REST manage-get.
		store.RecordBlockReads(cfg.Pool, ar.ApiKeyID, store.ActionMCPGet, []string{block.ID})

		data, _ := json.MarshalIndent(answer, "", "  ")
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
		// The T07 scope guard, the limit clamp (<=0 → 10, >50 → 50) and the
		// block-grant OR-arm (T40a) all live in store.RecentBlocks now; the
		// guard's error is mapped back to the byte-identical prose below.
		// The type registry snapshot rides along for the V-11 untrusted framing
		// only — the row's type name is never rendered (registry vocabulary is
		// not the model's decision), just resolved to the trust class per row.
		// The one type name a row may show is its untrusted_origin record (issue
		// #280), whose value is the type the text came from.
		grants, err := resolveGrants(ctx, cfg.Pool, ar)
		if errors.Is(err, store.ErrTooManyBlockGrants) { // T04-20: refuse, never read scope-only
			return errResult(tooManyGrantsMsg), nil, nil
		}
		previews, err := store.RecentBlocks(ctx, cfg.Pool, cfg.mcpTypeSnapshot(ctx), ar.ReadScopes,
			input.Category, input.Limit, input.Types,
			unionExcludes(input.TypesExclude, input.BlockRolesExclude), // WF T10 alias union — see recentInput
			grants)
		if err != nil {
			if errors.Is(err, store.ErrNoScopes) { // T07 fail-closed (design/01 §5.4)
				return errResult("unauthorized: no resolved scopes"), nil, nil
			}
			return errResult(fmt.Sprintf("recent failed: %v", err)), nil, nil
		}

		if len(previews) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent("No blocks found.")},
			}, nil, nil
		}
		var snap *config.Config
		if cfg.Cfg != nil {
			snap = cfg.Cfg.SnapshotForRequest(ctx)
		}
		store.RecordBlockReads(cfg.Pool, ar.ApiKeyID, store.ActionMCPRecent, readLogIDs(snap, previews))

		var sb strings.Builder
		for i, bp := range previews {
			trust := ""
			if bp.Untrusted { // V-11
				trust = " " + untrustedMarker
			}
			fmt.Fprintf(&sb, "[%d] %s (%s, %s) id:%s%s\n", i+1, bp.Title, bp.Category, bp.UpdatedAt.Format("2006-01-02"), bp.ID, trust)
			// issue #280: the record on a line of its own, the query tool's form.
			if note := store.UntrustedOriginNote(bp.UntrustedOrigin); note != "" {
				sb.WriteString("    " + note + "\n")
			}
			fmt.Fprintf(&sb, "    %s\n", bp.ContentPreview)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(sb.String())},
		}, nil, nil
	}
}

// Helpers.

// tooManyGrantsMsg is the ONE rejection prose for an over-bound grant set
// (store.ErrTooManyBlockGrants, T04-20/K12). Every read surface renders this
// exact text — the three MCP tools through errResult, the three HTTP read paths
// through their own envelopes — so a caller who hits the bound reads the same
// sentence no matter which door they used, and the fix it names is the real one
// (share the scope, do not collect grants). REST-search inherits it in T03-9.
const tooManyGrantsMsg = "too many block grants for this tenant: the row-level grant set exceeds the per-request bound — revoke grants or share at scope level"

// resolveGrants resolves the block-grant set for the caller's tenant (T40a,
// design/07 §4). It distinguishes two failures, and the distinction is the
// point:
//
//   - A resolver FAILURE (transient: pool, timeout, DB hiccup) stays
//     fail-closed-and-quiet: log, return an empty set, let the read proceed
//     scope-only. A grant lookup blip must never crash a read or widen
//     visibility.
//   - An OVER-BOUND grant set (store.ErrTooManyBlockGrants) is returned as an
//     error and the caller REFUSES the read. It is structural, not transient —
//     degrading to scope-only would hide every granted block silently and for
//     as long as the condition lasts, which is exactly the invisible cut the
//     bound exists to prevent (T04-20).
//
// store.ErrTooManyBlockGrants is therefore the ONLY non-nil error this returns,
// which is why the six read sites match on that sentinel rather than on
// err != nil: the mapping names the condition it refuses, and a second error
// class would have to be given its own answer here before it could reach them.
//
// TENANT-DECISION(authresult-tenantid-shape): the design/07 briefing assumed
// auth.AuthResult.TenantID *string (nullable). The canonical type is a plain
// string (auth.go:24), empty in the sentinel/no-tenant paths. store.GrantedBlockIDs
// already short-circuits an empty/whitespace tenantID to []string{}, so passing
// ar.TenantID directly preserves the intended semantics (empty tenant ⇒ '{}'
// ⇒ no-op OR-arm) without a nil deref. design/07 §4.1.
func resolveGrants(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult) ([]string, error) {
	grants, err := store.GrantedBlockIDs(ctx, pool, ar.TenantID)
	if err != nil {
		if errors.Is(err, store.ErrTooManyBlockGrants) {
			logRequestError(ctx, "block grant set exceeds the per-request bound — read refused", "error", err)
			return nil, err
		}
		// fail-closed for grant visibility: proceed scope-only, never crash the read
		slog.Warn("mcp: resolve granted block ids failed — scope-only visibility", "error", err)
		return []string{}, nil
	}
	return grants, nil
}

// readLogIDs is what the read record books for a search or recent answer
// (#345): the ids of the answered rows whose type is in mcp.read_log_types of
// the request's config snapshot. A nil snapshot (MCPConfig.Cfg is nil only in
// tests without config wiring) reads as an empty list, so a listing books
// nothing there.
func readLogIDs(snap *config.Config, rows []store.BlockPreview) []string {
	if snap == nil || len(rows) == 0 {
		return nil
	}
	types := snap.MCP.ReadLogTypes
	var ids []string
	for _, r := range rows {
		if slices.Contains(types, r.TypeName) {
			ids = append(ids, r.ID)
		}
	}
	return ids
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
