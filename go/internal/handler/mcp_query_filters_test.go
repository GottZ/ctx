// Issue #196: the MCP `query` tool takes /api/query's own `category`, `tags`
// and `categories_exclude` (queryRequest, query.go) and forwards them under
// the same wire names. Before the fields existed a client that reaches ctx
// over MCP alone could narrow the hybrid path by block type only.
//
// Every probe drives the handler through a raw JSON tool-argument object, as
// mcp_typefilter_test.go does: an unknown key is dropped by the decoder, and
// that silent drop is the RED state. A struct literal would fail to compile
// and prove nothing about the wire.
//
// No database: the tool delegates to a recording stub. That the forwarded
// fields narrow the result of the real pipeline is pinned behind the build tag
// in mcp_query_filters_integration_test.go.
//
//	go test -short ./internal/handler/ -run TestMCPQueryFilters -count=1 -v
package handler

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// TestMCPQueryFiltersReachREST: all three fields arrive in the delegated
// /api/query body, decoded with the REAL request struct, so a wire-name typo
// cannot hide behind a hand-written map.
//
// RED before the fields existed: the decoder drops the three keys and the
// delegated request carries no filter.
func TestMCPQueryFiltersReachREST(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry()}
	body := delegatedQueryBody(t, cfg,
		`{"question":"q","category":"Open Items ","tags":["beta","alpha"],"categories_exclude":["closed","archive"]}`)

	var req queryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode delegated body %s: %v", body, err)
	}
	if req.Category == nil || *req.Category != "Open Items " {
		t.Errorf("category did not reach /api/query as given: body %s", body)
	}
	if want := []string{"beta", "alpha"}; !reflect.DeepEqual(req.Tags, want) {
		t.Errorf("tags reached /api/query as %v, want %v: body %s", req.Tags, want, body)
	}
	if want := []string{"closed", "archive"}; !reflect.DeepEqual(req.CategoriesExclude, want) {
		t.Errorf("categories_exclude reached /api/query as %v, want %v: body %s", req.CategoriesExclude, want, body)
	}
	// The values are forwarded unchanged (case, trailing space and order as
	// given) and nothing else rides along.
	const want = `{"categories_exclude":["closed","archive"],"category":"Open Items ","limit":5,"query":"q","tags":["beta","alpha"]}`
	if string(body) != want {
		t.Errorf("delegated body = %s, want %s", body, want)
	}
}

// TestMCPQueryFiltersEachFieldAlone: a field adds its own key and no other, on
// its own and next to the type filter that shares the payload (each field, and
// all three together).
//
// RED before the fields existed for every row.
func TestMCPQueryFiltersEachFieldAlone(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry()}
	for _, tc := range []struct{ raw, want string }{
		{`{"question":"q","category":"open"}`,
			`{"category":"open","limit":5,"query":"q"}`},
		{`{"question":"q","tags":["alpha"]}`,
			`{"limit":5,"query":"q","tags":["alpha"]}`},
		{`{"question":"q","categories_exclude":["closed"]}`,
			`{"categories_exclude":["closed"],"limit":5,"query":"q"}`},
		{`{"question":"q","category":"open","types_exclude":["audit-trail"]}`,
			`{"category":"open","limit":5,"query":"q","types_exclude":["audit-trail"]}`},
		{`{"question":"q","tags":["alpha"],"types_exclude":["audit-trail"]}`,
			`{"limit":5,"query":"q","tags":["alpha"],"types_exclude":["audit-trail"]}`},
		{`{"question":"q","categories_exclude":["closed"],"types_exclude":["audit-trail"]}`,
			`{"categories_exclude":["closed"],"limit":5,"query":"q","types_exclude":["audit-trail"]}`},
		{`{"question":"q","category":"open","tags":["alpha"],"categories_exclude":["closed"],"types_exclude":["audit-trail"]}`,
			`{"categories_exclude":["closed"],"category":"open","limit":5,"query":"q","tags":["alpha"],"types_exclude":["audit-trail"]}`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if body := delegatedQueryBody(t, cfg, tc.raw); string(body) != tc.want {
				t.Errorf("delegated body = %s, want %s", body, tc.want)
			}
		})
	}
}

// TestMCPQueryFiltersBesideTypes: the positive `types` filter rewrites the
// payload's type restriction (it becomes a types_exclude complement); the three
// fields survive next to it.
//
// RED before the fields existed: only the type restriction arrives.
func TestMCPQueryFiltersBesideTypes(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry()}
	body := delegatedQueryBody(t, cfg,
		`{"question":"q","types":["knowledge"],"category":"open","tags":["alpha"],"categories_exclude":["closed"]}`)

	var req queryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode delegated body %s: %v", body, err)
	}
	if len(req.TypesExclude) == 0 {
		t.Fatalf("types=[knowledge] left no type restriction, premise broken: body %s", body)
	}
	if req.Category == nil || *req.Category != "open" {
		t.Errorf("category lost next to types: body %s", body)
	}
	if want := []string{"alpha"}; !reflect.DeepEqual(req.Tags, want) {
		t.Errorf("tags next to types = %v, want %v: body %s", req.Tags, want, body)
	}
	if want := []string{"closed"}; !reflect.DeepEqual(req.CategoriesExclude, want) {
		t.Errorf("categories_exclude next to types = %v, want %v: body %s", req.CategoriesExclude, want, body)
	}
}

// TestMCPQueryFiltersEmptyAddNoKey is the non-regression half. An absent or
// empty field adds no key, so the delegated body is the exact bytes the tool
// produced before the fields existed. An empty category in particular must
// not be forwarded: /api/query would bind the empty string and answer
// nothing, where the `search` and `recent` tools read "" as no filter.
//
// Passes before the fields existed as well (the decoder drops the keys); it
// guards the forwarding against sending an empty filter.
func TestMCPQueryFiltersEmptyAddNoKey(t *testing.T) {
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry()}
	const want = `{"limit":5,"query":"q"}`
	for _, raw := range []string{
		`{"question":"q"}`,
		`{"question":"q","category":""}`,
		`{"question":"q","tags":[]}`,
		`{"question":"q","categories_exclude":[]}`,
		`{"question":"q","category":"","tags":null,"categories_exclude":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if body := delegatedQueryBody(t, cfg, raw); string(body) != want {
				t.Errorf("delegated body = %s, want byte-identical %s", body, want)
			}
		})
	}
}
