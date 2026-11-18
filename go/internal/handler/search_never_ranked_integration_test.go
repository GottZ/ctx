//go:build integration

// Issue #342, step (c): search.leave_out_never_ranked. A full-text search that
// names no type leaves the never-ranked types out (the types the caller's
// registry snapshot marks retrieval-excluded: checkpoint, system-meta, insight,
// catalog on a stock registry) when the key is on, and answers exactly as before
// when it is off. Naming a type in `types` brings its rows back; a listing
// (empty query) is not affected. REST and the MCP tool share the search core,
// the web chat's ctx_search calls the same rule (store.SearchTypesExclude).
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestSearchLeaveOutNeverRanked' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	i342cCategory = "i342c"
	i342cTerm     = "quarzkopf"
	i342cTag      = "i342c-tag"
)

// i342cNeverRanked are the retrieval-excluded types of a stock registry, in
// name order — what a full-text search without types leaves out with the key on.
var i342cNeverRanked = []string{"catalog", "checkpoint", "insight", "system-meta"}

// i342cSeed writes one block per type into scope, in the fixture category,
// with the fixture tag and the term. A plain INSERT (the type triggers and the
// full-text vector trigger run); system-meta and the derived layers have no
// client write path.
func i342cSeed(t *testing.T, pool *pgxpool.Pool, scope string, typeNames ...string) {
	t.Helper()
	for _, name := range typeNames {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO context_blocks (category, title, content, scope, type_name, tags)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			i342cCategory, "i342c "+name, "i342c fixture "+i342cTerm+" of type "+name, scope, name, []string{i342cTag}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

// i342cSurfaces are the three search surfaces of one key, all with the key at on.
type i342cSurfaces struct {
	rest *SearchHandler
	mcp  MCPConfig
	chat *chat.Executor
	ar   *auth.AuthResult
}

func i342cWire(t *testing.T, pool *pgxpool.Pool, reg *blocktype.Registry, ar *auth.AuthResult, on bool) i342cSurfaces {
	t.Helper()
	cfg := staticConfigStore{cfg: &config.Config{Search: config.SearchConfig{LeaveOutNeverRanked: on}}}
	ctx := context.WithValue(context.Background(), authResultKey, ar)
	return i342cSurfaces{
		rest: NewSearchHandler(pool, cfg, reg),
		mcp:  MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfg},
		chat: chat.NewExecutor(pool, nil, 0).WithSearchTypes(reg.SnapshotForRequest(ctx), on),
		ar:   ar,
	}
}

func (s i342cSurfaces) ctx() context.Context {
	return context.WithValue(context.Background(), authResultKey, s.ar)
}

// restRaw answers the REST body for a request body.
func (s i342cSurfaces) restRaw(t *testing.T, body string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(body))
	req = req.WithContext(s.ctx())
	rec := httptest.NewRecorder()
	s.rest.HandleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST %s: status %d (%s)", body, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// mcpRaw answers the MCP tool's text for an argument object.
func (s i342cSurfaces) mcpRaw(t *testing.T, args string) string {
	t.Helper()
	var in searchInput
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		t.Fatalf("decode %s: %v", args, err)
	}
	res, _, err := mcpSearchHandler(s.mcp)(s.ctx(), nil, in)
	if err != nil || res.IsError {
		t.Fatalf("MCP %s: err %v, answer %s", args, err, mcpTextOf(res))
	}
	return mcpTextOf(res)
}

// chatRaw answers the chat tool's content for an argument object.
func (s i342cSurfaces) chatRaw(t *testing.T, args string) string {
	t.Helper()
	out := s.chat.Run(s.ctx(), s.ar.HomeScope, s.ar.ReadScopes, s.ar.ApiKeyID,
		llm.ToolCall{Function: llm.ToolCallFunction{Name: "ctx_search", Arguments: json.RawMessage(args)}})
	if !out.OK {
		t.Fatalf("chat %s: %s", args, out.Content)
	}
	return out.Content
}

// i342cRESTTypes counts the rows of a REST answer by type.
func i342cRESTTypes(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	var resp struct {
		Results []struct {
			Type string `json:"type"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode REST: %v", err)
	}
	out := map[string]int{}
	for _, r := range resp.Results {
		out[r.Type]++
	}
	return out
}

func i342cMCPTypes(t *testing.T, text string) map[string]int {
	t.Helper()
	var rows []i342Row
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("decode MCP: %v (%s)", err, text)
	}
	return i342TypesOf(rows)
}

// i342cChatTitles lists the titles of a chat answer, sorted (the chat rows
// carry no type; the fixture titles name it).
func i342cChatTitles(t *testing.T, content string) []string {
	t.Helper()
	var resp struct {
		Blocks []struct {
			Title string `json:"title"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(content), &resp); err != nil {
		t.Fatalf("decode chat: %v (%s)", err, content)
	}
	out := make([]string, 0, len(resp.Blocks))
	for _, b := range resp.Blocks {
		out = append(out, b.Title)
	}
	sort.Strings(out)
	return out
}

func i342cTitles(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "i342c "+n)
	}
	sort.Strings(out)
	return out
}

func i342cWithout(m map[string]int, drop []string) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		if !slices.Contains(drop, k) {
			out[k] = v
		}
	}
	return out
}

func TestSearchLeaveOutNeverRanked_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ranked := []string{"knowledge", "audit-trail"}
	i342cSeed(t, pool, "private", append(slices.Clone(ranked), i342cNeverRanked...)...)
	reg := vw6Cfg(t, pool).Blocktypes
	ar := &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, ApiKeyID: "00000000-0000-7000-8000-000000000342",
		HomeScope: "private", ReadScopes: []string{"private"}}

	// Premise: the four are exactly the retrieval-excluded types of the stock registry.
	set := reg.SnapshotForRequest(context.WithValue(context.Background(), authResultKey, ar))
	var excluded []string
	for _, name := range set.Names() {
		if !slices.Contains(set.VisibleTypes(), name) {
			excluded = append(excluded, name)
		}
	}
	if !reflect.DeepEqual(excluded, i342cNeverRanked) {
		t.Fatalf("fixture premise: retrieval-excluded types %v, want %v", excluded, i342cNeverRanked)
	}

	off := i342cWire(t, pool, reg, ar, false)
	on := i342cWire(t, pool, reg, ar, true)
	all := map[string]int{"knowledge": 1, "audit-trail": 1, "catalog": 1, "checkpoint": 1, "insight": 1, "system-meta": 1}

	t.Run("key off answers every type, as before", func(t *testing.T) {
		if got := i342cRESTTypes(t, off.restRaw(t, `{"query":"`+i342cTerm+`","limit":50}`)); !reflect.DeepEqual(got, all) {
			t.Errorf("REST: %v, want %v", got, all)
		}
		if got := i342cMCPTypes(t, off.mcpRaw(t, `{"query":"`+i342cTerm+`","limit":50}`)); !reflect.DeepEqual(got, all) {
			t.Errorf("MCP: %v, want %v", got, all)
		}
		if got, want := i342cChatTitles(t, off.chatRaw(t, `{"query":"`+i342cTerm+`","limit":20}`)), i342cTitles(append(slices.Clone(ranked), i342cNeverRanked...)...); !reflect.DeepEqual(got, want) {
			t.Errorf("chat: %v, want %v", got, want)
		}
	})

	t.Run("key on leaves the never-ranked types out of full text without types", func(t *testing.T) {
		want := map[string]int{"knowledge": 1, "audit-trail": 1}
		raw := on.restRaw(t, `{"query":"`+i342cTerm+`","limit":50}`)
		if got := i342cRESTTypes(t, raw); !reflect.DeepEqual(got, want) {
			t.Errorf("REST: %v, want %v", got, want)
		}
		if got := i342cMCPTypes(t, on.mcpRaw(t, `{"query":"`+i342cTerm+`","limit":50}`)); !reflect.DeepEqual(got, want) {
			t.Errorf("MCP: %v, want %v", got, want)
		}
		if got, want := i342cChatTitles(t, on.chatRaw(t, `{"query":"`+i342cTerm+`","limit":20}`)), i342cTitles(ranked...); !reflect.DeepEqual(got, want) {
			t.Errorf("chat: %v, want %v", got, want)
		}
		// The REST answer echoes the exclusion that ran, so it does not read as
		// the whole match set.
		var resp struct {
			Filters map[string]any `json:"filters"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		if got, _ := json.Marshal(resp.Filters["types_exclude"]); string(got) != `["catalog","checkpoint","insight","system-meta"]` {
			t.Errorf("REST filters.types_exclude = %s, want the four left-out types", got)
		}
	})

	// The same request in both modes: everything but a full-text search
	// without types answers the same bytes; that one answers the key-off rows
	// without the never-ranked types.
	t.Run("same requests in both modes", func(t *testing.T) {
		for _, body := range []string{
			`{"category":"` + i342cCategory + `","limit":50}`,
			`{"category":"` + i342cCategory + `","limit":2}`,
			`{"query":"` + i342cTerm + `","types":["checkpoint"]}`,
			`{"query":"` + i342cTerm + `","types":["system-meta"]}`,
			`{"query":"` + i342cTerm + `","types":["insight","catalog"]}`,
			`{"query":"` + i342cTerm + `","types":["checkpoint","knowledge"]}`,
			`{"query":"` + i342cTerm + `","types":["knowledge"]}`,
		} {
			if a, b := off.restRaw(t, body), on.restRaw(t, body); !bytes.Equal(a, b) {
				t.Errorf("REST %s: key on answers\n%s\nkey off\n%s", body, b, a)
			}
			if a, b := off.mcpRaw(t, body), on.mcpRaw(t, body); a != b {
				t.Errorf("MCP %s: key on answers\n%s\nkey off\n%s", body, b, a)
			}
		}
		for _, body := range []string{
			`{"query":"` + i342cTerm + `","limit":50}`,
			`{"query":"` + i342cTerm + `","category":"` + i342cCategory + `","limit":50}`,
			`{"query":"` + i342cTerm + `","tags":["` + i342cTag + `"],"limit":50}`,
			`{"query":"` + i342cTerm + `","types_exclude":["audit-trail"],"limit":50}`,
			`{"query":"` + i342cTerm + `","types_exclude":["checkpoint"],"limit":50}`,
		} {
			want := i342cWithout(i342cRESTTypes(t, off.restRaw(t, body)), i342cNeverRanked)
			if got := i342cRESTTypes(t, on.restRaw(t, body)); !reflect.DeepEqual(got, want) {
				t.Errorf("REST %s: key on %v, want key off without the never-ranked types %v", body, got, want)
			}
			if got := i342cMCPTypes(t, on.mcpRaw(t, body)); !reflect.DeepEqual(got, want) {
				t.Errorf("MCP %s: key on %v, want %v", body, got, want)
			}
		}
		for _, args := range []string{
			`{"category":"` + i342cCategory + `","limit":20}`,
			`{"query":"` + i342cTerm + `","types":["checkpoint"]}`,
		} {
			if a, b := off.chatRaw(t, args), on.chatRaw(t, args); a != b {
				t.Errorf("chat %s: key on answers\n%s\nkey off\n%s", args, b, a)
			}
		}
	})

	t.Run("key on, a named type answers its rows", func(t *testing.T) {
		for _, tc := range []struct {
			body string
			want map[string]int
		}{
			{`{"query":"` + i342cTerm + `","types":["checkpoint"]}`, map[string]int{"checkpoint": 1}},
			{`{"query":"` + i342cTerm + `","types":["system-meta"]}`, map[string]int{"system-meta": 1}},
			{`{"query":"` + i342cTerm + `","types":["checkpoint","knowledge"]}`, map[string]int{"checkpoint": 1, "knowledge": 1}},
		} {
			if got := i342cRESTTypes(t, on.restRaw(t, tc.body)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("REST %s: %v, want %v", tc.body, got, tc.want)
			}
			if got := i342cMCPTypes(t, on.mcpRaw(t, tc.body)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MCP %s: %v, want %v", tc.body, got, tc.want)
			}
		}
		if got, want := i342cChatTitles(t, on.chatRaw(t, `{"query":"`+i342cTerm+`","types":["checkpoint"]}`)), i342cTitles("checkpoint"); !reflect.DeepEqual(got, want) {
			t.Errorf("chat types=[checkpoint]: %v, want %v", got, want)
		}
	})

	t.Run("key on, limit and the listing cursor are unchanged", func(t *testing.T) {
		if got := i342cRESTTypes(t, on.restRaw(t, `{"query":"`+i342cTerm+`","limit":1}`)); len(got) != 1 || got["knowledge"]+got["audit-trail"] != 1 {
			t.Errorf("REST limit 1: %v, want one ranked row", got)
		}
		var page struct {
			NextAfter *store.SearchCursor `json:"next_after"`
		}
		if err := json.Unmarshal(on.restRaw(t, `{"category":"`+i342cCategory+`","limit":2}`), &page); err != nil || page.NextAfter == nil {
			t.Fatalf("listing page 1: next_after %v (%v), want a cursor", page.NextAfter, err)
		}
	})
}

// TestSearchLeaveOutNeverRankedTenantRegistry_Integration: what is never-ranked
// is decided by the caller's tenant-resolved registry snapshot. Tenant B's own
// excluded type is left out for B; B's overlay that lifts checkpoint into
// retrieval keeps B's checkpoint rows in B's full-text search; tenant A, on the
// stock registry, still leaves checkpoint out.
func TestSearchLeaveOutNeverRankedTenantRegistry_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	bg := context.Background()
	blocktype.SetRequestScopeHook(RequestTenantScope)
	t.Cleanup(func() { blocktype.SetRequestScopeHook(nil) })

	const own = "i342c-b-log"
	for _, w := range []store.BlockTypeWrite{
		{Name: own, Scope: "i342c-b", Config: json.RawMessage(`{"v":1,"retrieval":{"policy":"excluded"}}`)},
		{Name: "checkpoint", Scope: "i342c-b", Config: json.RawMessage(`{"v":1,"retrieval":{"policy":"full-pass"}}`)},
	} {
		if _, err := store.CreateBlockType(bg, pool, w, nil, ""); err != nil {
			t.Fatalf("create tenant B's type %s: %v", w.Name, err)
		}
	}
	i342cSeed(t, pool, "i342c-b", own, "checkpoint", "knowledge")
	i342cSeed(t, pool, "i342c-a", "checkpoint", "knowledge")
	reg := vw6Cfg(t, pool).Blocktypes

	member := func(scope string) *auth.AuthResult {
		return &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, HomeScope: scope, ReadScopes: []string{scope}}
	}
	for _, tc := range []struct {
		scope   string
		offWant map[string]int
		onWant  map[string]int
	}{
		{"i342c-b", map[string]int{own: 1, "checkpoint": 1, "knowledge": 1}, map[string]int{"checkpoint": 1, "knowledge": 1}},
		{"i342c-a", map[string]int{"checkpoint": 1, "knowledge": 1}, map[string]int{"knowledge": 1}},
	} {
		ar := member(tc.scope)
		off, on := i342cWire(t, pool, reg, ar, false), i342cWire(t, pool, reg, ar, true)
		body := `{"query":"` + i342cTerm + `","limit":50}`
		if got := i342cRESTTypes(t, off.restRaw(t, body)); !reflect.DeepEqual(got, tc.offWant) {
			t.Errorf("%s, key off, REST: %v, want %v", tc.scope, got, tc.offWant)
		}
		if got := i342cRESTTypes(t, on.restRaw(t, body)); !reflect.DeepEqual(got, tc.onWant) {
			t.Errorf("%s, key on, REST: %v, want %v", tc.scope, got, tc.onWant)
		}
		if got := i342cMCPTypes(t, on.mcpRaw(t, body)); !reflect.DeepEqual(got, tc.onWant) {
			t.Errorf("%s, key on, MCP: %v, want %v", tc.scope, got, tc.onWant)
		}
	}
	// B names its own excluded type and gets it.
	on := i342cWire(t, pool, reg, member("i342c-b"), true)
	if got := i342cMCPTypes(t, on.mcpRaw(t, `{"query":"`+i342cTerm+`","types":["`+own+`"]}`)); !reflect.DeepEqual(got, map[string]int{own: 1}) {
		t.Errorf("tenant B, key on, types=[%s]: %v", own, got)
	}
}
