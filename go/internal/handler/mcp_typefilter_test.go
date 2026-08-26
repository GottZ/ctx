// V-W6 gate (design/05 §7): the MCP retrieval tools `query` and `search` take
// `types` / `types_exclude`, and `types` CUTS against the request's visible
// allowlist instead of assigning visibility.
//
// Every probe below drives the handler through a JSON tool-argument object
// rather than a Go struct literal. That is deliberate and load-bearing: before
// this wave the field did not exist, an unknown JSON key was simply dropped by
// the decoder, and the RED state is exactly that silent drop — a struct literal
// would have failed to COMPILE and would have proved nothing about the wire.
//
// No database: the rejection paths answer before the pool is touched, and the
// query tool delegates to a recording stub instead of the real /api/query
// handler, so there is no LLM call either.
//
//	go test -short ./internal/handler/ -run TestMCPTypeFilter -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpTextOf concatenates the text content of a tool result. (The integration
// suite has its own copy behind the build tag; this one keeps the short probes
// tag-free.)
func mcpTextOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// typeFilterArgs decodes a raw MCP tool-argument object into the tool's input
// struct — the same step the SDK performs before it calls the handler.
func typeFilterArgs[T any](t *testing.T, raw string) T {
	t.Helper()
	var in T
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("decode tool arguments %s: %v", raw, err)
	}
	return in
}

// recordingQueryDelegate stands in for the /api/query handler the query tool
// delegates to. It records the internal request body — the ONE seam through
// which an MCP type filter can reach ctx_rrf's p_types_exclude — and answers a
// minimal valid query response so the handler runs to completion.
type recordingQueryDelegate struct{ body []byte }

func (h *recordingQueryDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.body, _ = io.ReadAll(r.Body)
	_, _ = w.Write([]byte(`{"answer":"stub","confidence":"confident","sources":[]}`))
}

// typeFilterCtx is a valid member identity — the rejections under test must be
// about the TYPE argument, never about auth.
func typeFilterCtx() context.Context {
	return context.WithValue(context.Background(), authResultKey, &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember,
		HomeScope: "private", ReadScopes: []string{"private"},
	})
}

// delegatedQueryBody runs the query tool over the given raw arguments and
// returns the recorded internal request body.
func delegatedQueryBody(t *testing.T, cfg MCPConfig, raw string) []byte {
	t.Helper()
	rec := &recordingQueryDelegate{}
	cfg.QueryHandler = rec
	res, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil, typeFilterArgs[queryInput](t, raw))
	if err != nil {
		t.Fatalf("query %s: transport error %v", raw, err)
	}
	if res.IsError {
		t.Fatalf("query %s: IsError=true (%s), want the delegated call", raw, mcpTextOf(res))
	}
	return rec.body
}

// TestMCPTypeFilterQueryReachesTheExcludeSeam pins that a positive `types` on
// the query tool arrives at the retrieval path the handler actually rides:
// /api/query's `types_exclude`, which the REST handler unions into ctx_rrf's
// p_types_exclude. ctx_rrf applies `type_name = ANY(p_types_visible) AND
// type_name != ALL(p_types_exclude)` (139:180-181), so excluding the COMPLEMENT
// of the requested cut inside the visible set is the cut itself — one filter
// mechanism, and no positive `types` on the REST request.
//
// RED before this wave: `types` is an unknown key, the decoder drops it, and
// the delegated body carries no types_exclude at all.
func TestMCPTypeFilterQueryReachesTheExcludeSeam(t *testing.T) {
	reg := blocktype.NewRegistry()
	cfg := MCPConfig{Blocktypes: reg}
	set := reg.Snapshot()

	body := delegatedQueryBody(t, cfg, `{"question":"warum bricht der Build","types":["knowledge"]}`)

	// The body is decoded with the REAL request struct: a wire-name typo here
	// would be invisible against a hand-written map.
	var req queryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode delegated body %s: %v", body, err)
	}
	excluded := map[string]bool{}
	for _, n := range req.TypesExclude {
		excluded[n] = true
	}
	if len(excluded) == 0 {
		t.Fatalf("types=[knowledge] left the delegated body without a type restriction: %s", body)
	}
	if excluded["knowledge"] {
		t.Errorf("the requested type itself is excluded — body %s", body)
	}
	// Concrete anchors, so the assertion is not merely the mirror of the
	// production expression: both of these are visible types today.
	for _, want := range []string{"audit-trail", "reference"} {
		if !excluded[want] {
			t.Errorf("visible type %q survives types=[knowledge] — body %s", want, body)
		}
	}
	// And the general contract over the whole visible set.
	for _, n := range set.VisibleTypes() {
		if n != "knowledge" && !excluded[n] {
			t.Errorf("visible type %q not excluded by types=[knowledge] — body %s", n, body)
		}
	}
	// checkpoint is retrieval-excluded: p_types_visible already keeps it out,
	// and naming it in the exclude list would only pretend it was ever a
	// candidate.
	if excluded["checkpoint"] {
		t.Errorf("retrieval-excluded type checkpoint listed in types_exclude — body %s", body)
	}
	// The legacy alias is REST-only (seam 17). The new MCP fields get no alias.
	if len(req.BlockRolesExclude) != 0 {
		t.Errorf("delegated body carries block_roles_exclude = %v, want none", req.BlockRolesExclude)
	}
}

// TestMCPTypeFilterQueryExcludeOnly pins that `types_exclude` alone is passed
// through restrictively — REST semantics, no alias, no validation (an unknown
// name simply excludes nothing, exactly as on /api/query).
func TestMCPTypeFilterQueryExcludeOnly(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry()}
	body := delegatedQueryBody(t, cfg, `{"question":"frage","types_exclude":["audit-trail"]}`)

	var req queryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode delegated body %s: %v", body, err)
	}
	if len(req.TypesExclude) != 1 || req.TypesExclude[0] != "audit-trail" {
		t.Fatalf("types_exclude=[audit-trail] → delegated %v, want exactly [audit-trail] — body %s",
			req.TypesExclude, body)
	}
}

// TestMCPTypeFilterQueryByteIdenticalWithoutFields is the non-regression half:
// with neither field the delegated body must be the exact bytes the handler
// produced before the fields existed.
func TestMCPTypeFilterQueryByteIdenticalWithoutFields(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry()}
	body := delegatedQueryBody(t, cfg, `{"question":"warum bricht der Build"}`)

	const want = `{"limit":5,"query":"warum bricht der Build"}`
	if string(body) != want {
		t.Fatalf("delegated body = %s, want byte-identical %s", body, want)
	}
}

// TestMCPTypeFilterRejections covers the two rejecting negative probes on BOTH
// retrieval tools:
//
//	(a) an unknown type name is a caller error, not an empty result — a typo
//	    must not read as "nothing matched";
//	(b) a name that EXISTS but is not retrieval-visible (checkpoint) is refused
//	    too. Admitting it would widen retrieval visibility for every key without
//	    an admin gate; dropping it silently would let the caller believe the
//	    filter applied. There is no admin bypass on this path.
//
// RED before this wave: both arguments are dropped by the decoder, the tools
// answer a normal (unfiltered) result and IsError stays false.
func TestMCPTypeFilterRejections(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: &recordingQueryDelegate{}}

	tools := map[string]func(context.Context, string) (*mcp.CallToolResult, error){
		"query": func(ctx context.Context, raw string) (*mcp.CallToolResult, error) {
			res, _, err := mcpQueryHandler(cfg)(ctx, nil, typeFilterArgs[queryInput](t, raw))
			return res, err
		},
		"search": func(ctx context.Context, raw string) (*mcp.CallToolResult, error) {
			res, _, err := mcpSearchHandler(cfg)(ctx, nil, typeFilterArgs[searchInput](t, raw))
			return res, err
		},
	}
	probes := map[string]string{
		"unknown_name":           `"types":["nicht-existent"]`,
		"invisible_but_existing": `"types":["checkpoint"]`,
	}

	for tool, call := range tools {
		for probe, arg := range probes {
			t.Run(tool+"/"+probe, func(t *testing.T) {
				raw := `{"question":"frage","query":"frage",` + arg + `}`
				res, err := call(typeFilterCtx(), raw)
				if err != nil {
					t.Fatalf("%s %s: transport error %v", tool, raw, err)
				}
				if !res.IsError {
					t.Fatalf("%s %s: IsError=false — the argument was ignored or silently emptied (%s)",
						tool, raw, mcpTextOf(res))
				}
			})
		}
	}
}

// TestMCPTypeFilterFailsClosedWithoutRegistry: without a registry snapshot
// there is no visible set to cut against. Answering the request unfiltered
// would silently widen it, so the tools refuse instead.
func TestMCPTypeFilterFailsClosedWithoutRegistry(t *testing.T) {
	cfg := MCPConfig{QueryHandler: &recordingQueryDelegate{}} // Blocktypes nil

	t.Run("query", func(t *testing.T) {
		res, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil,
			typeFilterArgs[queryInput](t, `{"question":"frage","types":["knowledge"]}`))
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("query with types and no registry: IsError=false (%s)", mcpTextOf(res))
		}
	})

	t.Run("search", func(t *testing.T) {
		res, _, err := mcpSearchHandler(cfg)(typeFilterCtx(), nil,
			typeFilterArgs[searchInput](t, `{"query":"frage","types":["knowledge"]}`))
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("search with types and no registry: IsError=false (%s)", mcpTextOf(res))
		}
	})
}

// TestMCPTypeFilterAddsNoTool restates the T17 half of the gate at the tool
// surface: V-W6 adds FIELDS, not tools. (test.sh T17 pins the same 15 against
// the live server; TestMCPToolsListCarriesBlobTools pins the count itself.)
func TestMCPTypeFilterAddsNoTool(t *testing.T) {
	names := mcpToolNames(t)
	if len(names) != 15 {
		t.Fatalf("tools/list exports %d tools, want 15 — V-W6 adds fields, not tools (%v)", len(names), names)
	}
	if !strings.Contains(strings.Join(names, ","), "search") {
		t.Fatalf("tools/list lost the search tool: %v", names)
	}
}
