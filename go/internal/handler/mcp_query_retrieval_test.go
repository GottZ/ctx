// The MCP `query` tool takes /api/query's own `synthesize` and
// `include_content` fields (queryRequest, query.go). With synthesize:false the
// tool answers the ranked sources as JSON instead of a synthesized answer; the
// default call keeps the synthesis path and its prose answer.
//
// Every probe drives the handler through a raw JSON tool-argument object, as
// mcp_typefilter_test.go does: before the fields existed the decoder dropped
// the unknown keys, and that silent drop is the RED state — a struct literal
// would fail to compile and prove nothing about the wire.
//
// The canned /api/query answers are built from the REAL response DTO
// (queryResponse + buildSourceResponses), so the rendering is tested against
// the shape the handler emits, not against a hand-written guess of it.
//
//	go test -short ./internal/handler/ -run TestMCPQueryRetrieval -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/llm"
)

// countingQueryDelegate answers a fixed body and counts the delegated calls.
type countingQueryDelegate struct {
	body  string
	calls int
}

func (h *countingQueryDelegate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls++
	_, _ = w.Write([]byte(h.body))
}

// retrievalOnlyBody is a retrieval-only /api/query answer as the handler builds
// it (Step 7b): sources from buildSourceResponses with include_content on, no
// answer, no citation_index. Order is the retrieval order the tool must keep.
func retrievalOnlyBody(t *testing.T) string {
	t.Helper()
	long := strings.Repeat("x", maxRetrievalSnippet+200)
	sources := []llm.Source{
		{ID: "id-b", Title: "Second by id, first by rank", Category: "decisions", Score: 0.91, Content: "content B"},
		{ID: "id-a", Title: "Captured tool output", Category: "learnings", Score: 0.42, Content: long, Untrusted: true},
		{ID: "id-c", Title: "Old plan", Category: "projects", Score: 0.17, Content: "content C"},
		// Score 0 must still serialize: the rank key is never an optional field.
		{ID: "id-d", Title: "Zero score", Category: "projects", Score: 0, Content: "content D"},
	}
	resp := queryResponse{
		Success:    true,
		Sources:    buildSourceResponses(sources, map[string][]string{"id-c": {"id-new"}}, true),
		Confidence: "confident",
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mcpRetrievalRow is what a test reads back from the tool's JSON answer.
type mcpRetrievalRow struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Category     string   `json:"category"`
	Score        *float64 `json:"score"`
	SupersededBy *string  `json:"superseded_by"`
	Untrusted    bool     `json:"untrusted"`
	Content      *string  `json:"content"`
}

func runMCPQuery(t *testing.T, delegate http.Handler, raw string) (text string, isError bool) {
	t.Helper()
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: delegate}
	res, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil, typeFilterArgs[queryInput](t, raw))
	if err != nil {
		t.Fatalf("query %s: transport error %v", raw, err)
	}
	return mcpTextOf(res), res.IsError
}

// TestMCPQueryRetrievalOnlyReachesREST: both fields arrive in the delegated
// /api/query body under REST's own wire names, decoded with the REAL request
// struct, and nothing else rides along.
//
// RED before the fields existed: the decoder drops both keys and the body is
// the plain {"limit":5,"query":…}.
func TestMCPQueryRetrievalOnlyReachesREST(t *testing.T) {
	rec := &recordingQueryDelegate{}
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: rec}
	raw := `{"question":"welche Szene","synthesize":false,"include_content":true}`
	if _, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil, typeFilterArgs[queryInput](t, raw)); err != nil {
		t.Fatalf("transport error: %v", err)
	}

	var req queryRequest
	if err := json.Unmarshal(rec.body, &req); err != nil {
		t.Fatalf("decode delegated body %s: %v", rec.body, err)
	}
	if req.Synthesize == nil || *req.Synthesize {
		t.Errorf("synthesize:false did not reach /api/query — body %s", rec.body)
	}
	if !req.IncludeContent {
		t.Errorf("include_content:true did not reach /api/query — body %s", rec.body)
	}
	// Exactly these keys: the tool forwards the two fields and adds no
	// measurement field (arm_ranks, pinned_*, shadow_types stay REST/admin).
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(rec.body, &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		switch k {
		case "query", "limit", "synthesize", "include_content":
		default:
			t.Errorf("delegated body carries unexpected key %q — body %s", k, rec.body)
		}
	}
}

// TestMCPQueryRetrievalOnlyIgnoresMeasurementKeys: a caller naming REST's
// admin measurement fields gets none of them forwarded — the tool's argument
// struct has no such field, so the decoder drops them before the delegation.
func TestMCPQueryRetrievalOnlyIgnoresMeasurementKeys(t *testing.T) {
	rec := &recordingQueryDelegate{}
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: rec}
	raw := `{"question":"q","synthesize":false,"arm_ranks":true,"shadow_types":["knowledge"],"pinned_translation":"x"}`
	if _, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil, typeFilterArgs[queryInput](t, raw)); err != nil {
		t.Fatalf("transport error: %v", err)
	}
	const want = `{"limit":5,"query":"q","synthesize":false}`
	if string(rec.body) != want {
		t.Fatalf("delegated body = %s, want %s", rec.body, want)
	}
}

// TestMCPQueryDefaultStillSynthesizes is the negative control: without the
// fields the delegated body is byte-identical to before, and with
// synthesize:true the body says so and the answer is the prose rendering.
func TestMCPQueryDefaultStillSynthesizes(t *testing.T) {
	const prose = `{"answer":"stub answer","confidence":"confident","sources":[{"id":"id1","title":"T1","category":"decisions","citation_index":1}]}`

	for _, tc := range []struct {
		raw, wantBody string
	}{
		{`{"question":"q"}`, `{"limit":5,"query":"q"}`},
		{`{"question":"q","synthesize":true}`, `{"limit":5,"query":"q","synthesize":true}`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			rec := &recordingQueryDelegate{}
			cfg := MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: rec}
			if _, _, err := mcpQueryHandler(cfg)(typeFilterCtx(), nil, typeFilterArgs[queryInput](t, tc.raw)); err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if string(rec.body) != tc.wantBody {
				t.Errorf("delegated body = %s, want %s", rec.body, tc.wantBody)
			}

			text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: prose}, tc.raw)
			if isErr {
				t.Fatalf("IsError=true: %s", text)
			}
			if !strings.HasPrefix(text, "stub answer\n\nSources:\n[1] T1 (decisions) id:id1") {
				t.Errorf("synthesis answer not rendered as prose:\n%s", text)
			}
		})
	}
}

// TestMCPQueryRetrievalOnlyRendersRankedSources: with synthesize:false the tool
// answers the ranked sources as JSON — retrieval order kept, id/title/category/
// score per row, the content snippet when include_content was set, and the
// untrusted framing (V-11) and superseded_by carried through.
//
// RED before the fields existed: the tool rendered the empty answer plus the
// prose "Sources:" list, which is not JSON and drops score and content.
func TestMCPQueryRetrievalOnlyRendersRankedSources(t *testing.T) {
	text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: retrievalOnlyBody(t)},
		`{"question":"q","synthesize":false,"include_content":true}`)
	if isErr {
		t.Fatalf("IsError=true: %s", text)
	}
	var got struct {
		Confidence string            `json:"confidence"`
		Sources    []mcpRetrievalRow `json:"sources"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("retrieval-only answer is not JSON (%v):\n%s", err, text)
	}
	if got.Confidence != "confident" {
		t.Errorf("confidence = %q, want confident", got.Confidence)
	}
	if len(got.Sources) != 4 {
		t.Fatalf("sources = %d, want 4:\n%s", len(got.Sources), text)
	}
	wantIDs := []string{"id-b", "id-a", "id-c", "id-d"}
	wantScores := []float64{0.91, 0.42, 0.17, 0}
	for i, s := range got.Sources {
		if s.ID != wantIDs[i] {
			t.Errorf("sources[%d].id = %q, want %q (retrieval order)", i, s.ID, wantIDs[i])
		}
		if s.Score == nil || *s.Score != wantScores[i] {
			t.Errorf("sources[%d].score = %v, want %v", i, s.Score, wantScores[i])
		}
		if s.Title == "" || s.Category == "" {
			t.Errorf("sources[%d] lost title/category: %+v", i, s)
		}
		if s.Content == nil || *s.Content == "" {
			t.Errorf("sources[%d] carries no content under include_content", i)
		}
	}
	if n := len([]rune(*got.Sources[1].Content)); n != maxRetrievalSnippet {
		t.Errorf("content snippet = %d runes, want the REST cap %d", n, maxRetrievalSnippet)
	}
	if !got.Sources[1].Untrusted || got.Sources[0].Untrusted || got.Sources[2].Untrusted || got.Sources[3].Untrusted {
		t.Errorf("untrusted framing not carried per source: %+v", got.Sources)
	}
	if got.Sources[2].SupersededBy == nil || *got.Sources[2].SupersededBy != "id-new" {
		t.Errorf("superseded_by not carried: %+v", got.Sources[2])
	}
	if strings.Contains(text, "Sources:") || strings.Contains(text, "citation_index") {
		t.Errorf("retrieval-only answer carries synthesis rendering:\n%s", text)
	}
}

// TestMCPQueryRetrievalOnlyEmptyIsArray: no hits is an empty JSON array, not
// null — a consumer iterating the list must not have to special-case it.
func TestMCPQueryRetrievalOnlyEmptyIsArray(t *testing.T) {
	for _, body := range []string{
		`{"success":true,"answer":"","sources":[],"confidence":"no_relevant_blocks_found"}`,
		`{"success":true,"sources":null,"confidence":"no_relevant_blocks_found"}`,
		`{"success":true,"confidence":"no_relevant_blocks_found"}`,
	} {
		text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: body}, `{"question":"q","synthesize":false}`)
		if isErr {
			t.Fatalf("%s: IsError=true: %s", body, text)
		}
		if !strings.Contains(text, `"sources": []`) {
			t.Errorf("%s: empty retrieval-only answer has no empty sources array:\n%s", body, text)
		}
	}
}

// TestMCPQueryIncludeContentRequiresRetrievalOnly: /api/query attaches content
// only on the retrieval-only path (Step 7b passes req.IncludeContent, the
// synthesis path passes false), so include_content without synthesize:false
// would be a silent no-op. The tool refuses it before any delegation, the way
// /api/query refuses arm_ranks without synthesize:false.
func TestMCPQueryIncludeContentRequiresRetrievalOnly(t *testing.T) {
	for _, raw := range []string{
		`{"question":"q","include_content":true}`,
		`{"question":"q","synthesize":true,"include_content":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			d := &countingQueryDelegate{body: `{"answer":"stub","confidence":"confident","sources":[]}`}
			text, isErr := runMCPQuery(t, d, raw)
			if !isErr || !strings.Contains(text, "include_content requires synthesize:false") {
				t.Errorf("IsError=%v text=%q; want the include_content refusal", isErr, text)
			}
			if d.calls != 0 {
				t.Errorf("refused call still delegated %d time(s)", d.calls)
			}
		})
	}
}

// TestMCPQueryRetrievalOnlyPrivilegeParity runs the REAL query handler (DB-less,
// as TestMCPQueryQuotaRefusalIsToolError does) for a non-admin member key whose
// tenant is over its call budget, once over REST and once over MCP, both with
// synthesize:false + include_content:true. Neither surface has an admin gate on
// the two fields: both reach the same tenant quota gate and are refused there
// with the same code, and the embed backend is never reached.
func TestMCPQueryRetrievalOnlyPrivilegeParity(t *testing.T) {
	zero := 0
	acc := backends.NewQuotaAccountant(nil, 0)
	acc.SeedForTest("acme:work", backends.TenantQuota{DailyCalls: &zero, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)
	srv, hits := fakeEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(brokenPool(t), st, embedPool(srv.URL), acc, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	ctx := context.WithValue(context.Background(), authResultKey, &auth.AuthResult{
		ApiKeyID: "019e0011-aaaa-7000-9000-000000000001", IsValid: true, TenantRole: auth.RoleMember,
		HomeScope: "acme:work", ReadScopes: []string{"acme:work"},
	})

	// REST.
	body := `{"query":"alpha bravo charlie","synthesize":false,"include_content":true}`
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/query", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleQuery(w, r)
	var restResp struct {
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &restResp)
	if w.Code == http.StatusForbidden || restResp.ErrorCode != "quota_exceeded" {
		t.Fatalf("REST: status %d body %s; want the quota_exceeded refusal", w.Code, w.Body.String())
	}

	// MCP, same key, same two fields.
	res, _, err := mcpQueryHandler(MCPConfig{QueryHandler: http.HandlerFunc(h.HandleQuery)})(ctx, nil,
		typeFilterArgs[queryInput](t, `{"question":"alpha bravo charlie","synthesize":false,"include_content":true}`))
	if err != nil {
		t.Fatalf("MCP: transport error %v", err)
	}
	if env, ok := res.StructuredContent.(mcpErrorEnvelope); !res.IsError || !ok || env.Code != restResp.ErrorCode {
		t.Fatalf("MCP: IsError=%v structured=%#v text=%q; want the REST refusal code %q",
			res.IsError, res.StructuredContent, mcpTextOf(res), restResp.ErrorCode)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("embed hits = %d; want 0", n)
	}
}

// mcpWireCall posts one JSON-RPC request to the real MCP handler (SDK schema
// inference, argument validation and decoding included) and returns the raw
// response body.
func mcpWireCall(t *testing.T, cfg MCPConfig, rpc string) []byte {
	t.Helper()
	req := httptest.NewRequestWithContext(typeFilterCtx(), http.MethodPost, "/mcp", strings.NewReader(rpc))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	NewMCPHandler(cfg).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// TestMCPQueryRetrievalOnlyOverTheWire runs the two fields through the SDK
// layer a client actually talks to: tools/list advertises both as optional
// booleans, and a tools/call with synthesize:false reaches /api/query with
// both fields and answers JSON.
func TestMCPQueryRetrievalOnlyOverTheWire(t *testing.T) {
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]struct {
						Type any `json:"type"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpWireCall(t, MCPConfig{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range list.Result.Tools {
		if tool.Name != "query" {
			continue
		}
		found = true
		for _, p := range []string{"synthesize", "include_content"} {
			prop, ok := tool.InputSchema.Properties[p]
			if !ok {
				t.Errorf("query inputSchema lacks %q", p)
				continue
			}
			if !strings.Contains(fmt.Sprint(prop.Type), "boolean") {
				t.Errorf("query inputSchema %q type = %v, want boolean", p, prop.Type)
			}
		}
		if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "question" {
			t.Errorf("query required = %v, want [question]", tool.InputSchema.Required)
		}
	}
	if !found {
		t.Fatal("tools/list has no query tool")
	}

	rec := &recordingQueryDelegate{}
	cfg := MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: rec}
	raw := mcpWireCall(t, cfg, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query",`+
		`"arguments":{"question":"q","synthesize":false,"include_content":true}}}`)
	if want := `{"include_content":true,"limit":5,"query":"q","synthesize":false}`; string(rec.body) != want {
		t.Errorf("delegated body = %s, want %s", rec.body, want)
	}
	var call struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		t.Fatalf("decode tools/call: %v (%s)", err, raw)
	}
	if call.Result.IsError || len(call.Result.Content) != 1 {
		t.Fatalf("tools/call result = %s", raw)
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Result.Content[0].Text), &answer); err != nil {
		t.Fatalf("retrieval-only text is not JSON over the wire: %v (%s)", err, raw)
	}
	if _, ok := answer["sources"]; !ok {
		t.Errorf("retrieval-only answer lacks sources: %s", call.Result.Content[0].Text)
	}

	// REST's admin measurement field is not an argument of this tool: the SDK
	// refuses the call against the input schema, and nothing is delegated.
	probe := &recordingQueryDelegate{}
	raw = mcpWireCall(t, MCPConfig{Blocktypes: blocktype.NewRegistry(), QueryHandler: probe},
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"query",`+
			`"arguments":{"question":"q","synthesize":false,"arm_ranks":true}}}`)
	call.Result.IsError = false
	if err := json.Unmarshal(raw, &call); err != nil {
		t.Fatalf("decode tools/call: %v (%s)", err, raw)
	}
	if !call.Result.IsError || probe.body != nil {
		t.Errorf("arm_ranks over MCP: isError=%v delegated=%q, want a refusal without delegation (%s)",
			call.Result.IsError, probe.body, raw)
	}
}
