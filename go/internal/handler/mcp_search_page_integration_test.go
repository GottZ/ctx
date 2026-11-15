//go:build integration

// Issue #206 — the MCP `search` tool pages.
//
// #206: the tool could not list past 50 rows. The statement builder clamps
// every caller at 50 (store.SearchMaxLimit), the tool took no cursor, and a
// caller that asked for 200 rows got 50 without being told. REST
// POST /api/search has had the keyset cursor (`after` / `next_after`) since
// block-workbench W7; the tool now takes the same cursor through the same
// shared core (search_core.go), and the answer says when the limit was lowered
// and whether more rows exist.
//
// Every probe goes through the real tool: over the wire (SDK schema
// validation, decoding, the answer a client reads) for the new arguments, the
// typed handler for the calls an existing client already makes. Fixtures are
// written through the MCP store tool or store.UpsertBlock; grants and the
// guard's metadata key have no client write path and use the helpers the
// existing grant and redaction tests use.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestMCPSearchPage -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// i206Answer is one tools/call answer as a client reads it off the wire.
type i206Answer struct {
	IsError    bool
	Texts      []string
	Structured json.RawMessage
}

func (a i206Answer) text() string {
	if len(a.Texts) == 0 {
		return ""
	}
	return a.Texts[0]
}

// i206Page is the paged answer object (after given).
type i206Page struct {
	NextAfter      *store.SearchCursor `json:"next_after"`
	Limit          int                 `json:"limit"`
	RequestedLimit int                 `json:"requested_limit"`
	Results        []map[string]any    `json:"results"`
}

// i206Wire posts one tools/call of `search` with the raw arguments to the real
// MCP handler, authenticated as ctx carries it.
func i206Wire(t *testing.T, ctx context.Context, cfg MCPConfig, args string) i206Answer {
	t.Helper()
	rpc := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":` + args + `}}`
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", strings.NewReader(rpc))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	NewMCPHandler(cfg).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %s: status %d, body %s", args, rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("search %s: decode %v (body %s)", args, err, rec.Body.String())
	}
	out := i206Answer{IsError: env.Result.IsError, Structured: env.Result.StructuredContent}
	for _, c := range env.Result.Content {
		out.Texts = append(out.Texts, c.Text)
	}
	return out
}

// i206PageOf calls the tool and decodes the paged answer object; a tool error
// fails the test.
func i206PageOf(t *testing.T, ctx context.Context, cfg MCPConfig, args string) i206Page {
	t.Helper()
	a := i206Wire(t, ctx, cfg, args)
	if a.IsError {
		t.Fatalf("search %s: tool error %q, want a page", args, a.text())
	}
	if len(a.Texts) != 1 {
		t.Fatalf("search %s: %d content blocks, want 1", args, len(a.Texts))
	}
	var p i206Page
	if err := json.Unmarshal([]byte(a.text()), &p); err != nil {
		t.Fatalf("search %s: the answer is not the page object: %v (%s)", args, err, a.text())
	}
	return p
}

func i206Cursor(t *testing.T, c *store.SearchCursor) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	return string(b)
}

func i206IDs(rows []map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// i206Fixture is the shared corpus of the test.
type i206Fixture struct {
	pool    *pgxpool.Pool
	cfg     MCPConfig
	cfgSt   staticConfigStore
	reg     *blocktype.Registry
	keyCtx  context.Context // real key, home scope private, default tenant
	otherAR *auth.AuthResult
}

func i206Setup(t *testing.T) *i206Fixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	cfgSt := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	_, plain, err := store.CreateApiKey(ctx, pool, "i206", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	other := bgTenant(t, pool, "i206-other")
	bgMapScope(t, pool, "i206-other", other)
	return &i206Fixture{
		pool:   pool,
		cfg:    MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfgSt},
		cfgSt:  cfgSt,
		reg:    reg,
		keyCtx: mcpAuthCtx(t, ctx, pool, plain),
		otherAR: &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember,
			HomeScope: "i206-other", ReadScopes: []string{"i206-other"}, TenantID: other,
		},
	}
}

// storeTool writes one block through the MCP store tool and returns its id.
func (f *i206Fixture) storeTool(t *testing.T, category, title, content string) string {
	t.Helper()
	r, _, err := mcpStoreHandler(f.cfg)(f.keyCtx, nil, storeInput{Category: category, Title: title, Content: content})
	if err != nil {
		t.Fatalf("mcp store %s: protocol error %v", title, err)
	}
	if r.IsError {
		t.Fatalf("mcp store %s rejected: %s", title, resultText(t, r))
	}
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM context_blocks WHERE category = $1 AND title = $2 AND scope = 'private' AND NOT is_archived`,
		category, title).Scan(&id); err != nil {
		t.Fatalf("read back %s: %v", title, err)
	}
	return id
}

// upsert writes one block through store.UpsertBlock (the store write path
// without the tool's gates) and returns its id.
func (f *i206Fixture) upsert(t *testing.T, scope, category, title, content, typeName string, metadata map[string]any) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, category, title, content, nil, metadata, scope, true,
		store.SensitivityWrite{Value: backends.SensPublic, Manual: true}, typeName)
	if err != nil {
		t.Fatalf("upsert %s: %v", title, err)
	}
	return b.ID
}

func (f *i206Fixture) archive(t *testing.T, id string) {
	t.Helper()
	r, _, err := mcpArchiveHandler(f.cfg)(f.keyCtx, nil, archiveInput{ID: id})
	if err != nil || r.IsError {
		t.Fatalf("mcp archive %s: err %v, result %s", id, err, mcpTextOf(r))
	}
}

// storeRows is what the store answers for a compact browse of one category —
// the rows the tool rendered before #206, for the byte pins.
func (f *i206Fixture) storeRows(t *testing.T, category string, limit int) []store.BlockPreview {
	t.Helper()
	rows, err := store.SearchBlocks(context.Background(), f.pool, f.reg.SnapshotForRequest(context.Background()), "",
		[]string{"private"}, category, nil, limit, true, nil, []string{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("store search %s: %v", category, err)
	}
	return rows
}

func (f *i206Fixture) rest(t *testing.T, ctx context.Context, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	NewSearchHandler(f.pool, f.cfgSt, f.reg).HandleSearch(rec, req)
	var env map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env
}

func TestMCPSearchPage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := i206Setup(t)

	// 120 blocks in one category, through the store tool.
	enum := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		enum = append(enum, f.storeTool(t, "i206-enum", fmt.Sprintf("i206 enum %03d", i), fmt.Sprintf("enumeration body %03d", i)))
	}
	// Exactly 50 blocks in another: the last page is a full one.
	for i := 0; i < 50; i++ {
		f.upsert(t, "private", "i206-fifty", fmt.Sprintf("i206 fifty %02d", i), fmt.Sprintf("fifty body %02d", i), "", nil)
	}

	t.Run("enumerates_every_row_once_across_pages", func(t *testing.T) { i206Enumerate(t, f, enum) })
	t.Run("lowered_limit_is_reported", func(t *testing.T) { i206LoweredLimit(t, f) })
	t.Run("a_full_last_page_has_no_next_after", func(t *testing.T) { i206FullLastPage(t, f) })
	t.Run("rest_full_last_page_has_no_next_after", func(t *testing.T) {
		code, env := f.rest(t, f.keyCtx, `{"category":"i206-fifty","limit":50}`)
		if code != http.StatusOK {
			t.Fatalf("REST status %d", code)
		}
		if got := string(env["next_after"]); got != "null" {
			t.Errorf("REST next_after on the last, full page = %s, want null (no further row exists)", got)
		}
	})
	t.Run("rest_malformed_cursor_is_a_400", func(t *testing.T) {
		code, env := f.rest(t, f.keyCtx, `{"category":"i206-enum","after":{"after_updated":"2026-01-01T00:00:00Z","after_id":"not-a-uuid"}}`)
		if code != http.StatusBadRequest {
			t.Errorf("REST malformed after_id: status %d (error %s), want 400", code, env["error"])
		}
	})
	t.Run("existing_calls_answer_the_bytes_they_did", func(t *testing.T) { i206LegacyBytes(t, f) })
	t.Run("cursor_is_a_position_not_a_grant", func(t *testing.T) { i206CursorSafety(t, f) })
	t.Run("after_with_a_query_is_refused", func(t *testing.T) {
		a := i206Wire(t, f.keyCtx, f.cfg, `{"query":"enumeration","after":{}}`)
		if !a.IsError || !strings.Contains(a.text(), "empty query") {
			t.Errorf("after + query: isError %t, text %q; want a refusal that names the empty-query browse path", a.IsError, a.text())
		}
	})
}

func i206Enumerate(t *testing.T, f *i206Fixture, initial []string) {
	page1 := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-enum","limit":50,"after":{}}`)
	if len(page1.Results) != 50 || page1.NextAfter == nil || page1.Limit != 50 || page1.RequestedLimit != 0 {
		t.Fatalf("page 1: %d rows, next_after %v, limit %d, requested %d; want 50 rows, a cursor, limit 50, no requested_limit",
			len(page1.Results), page1.NextAfter, page1.Limit, page1.RequestedLimit)
	}
	seen := map[string]int{}
	for _, id := range i206IDs(page1.Results) {
		seen[id]++
	}

	// Between the pages: three new blocks, one visited row archived, and the
	// OLDEST row (not visited yet) archived.
	var added []string
	for i := 0; i < 3; i++ {
		added = append(added, f.storeTool(t, "i206-enum", fmt.Sprintf("i206 enum late %d", i), "late body"))
	}
	f.archive(t, i206IDs(page1.Results)[10])
	oldest := initial[0]
	f.archive(t, oldest)

	after := page1.NextAfter
	pages := 1
	for after != nil {
		p := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-enum","limit":50,"after":`+i206Cursor(t, after)+`}`)
		pages++
		for _, id := range i206IDs(p.Results) {
			seen[id]++
		}
		after = p.NextAfter
		if pages > 5 {
			t.Fatalf("still paging after %d pages", pages)
		}
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 (50 + 50 + 19)", pages)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s answered %d times, want once", id, n)
		}
	}
	for _, id := range added {
		if seen[id] != 0 {
			t.Errorf("block %s written after page 1 is in the listing (it is newer than the cursor)", id)
		}
	}
	if seen[oldest] != 0 {
		t.Errorf("block %s archived before its page was read is in the listing", oldest)
	}
	for _, id := range initial {
		if id != oldest && seen[id] != 1 {
			t.Errorf("block %s that existed at page 1 was skipped", id)
		}
	}
}

func i206LoweredLimit(t *testing.T, f *i206Fixture) {
	p := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-enum","limit":200,"after":{}}`)
	if len(p.Results) != 50 || p.Limit != 50 || p.RequestedLimit != 200 {
		t.Errorf("limit 200: %d rows, limit %d, requested_limit %d; want 50, 50, 200", len(p.Results), p.Limit, p.RequestedLimit)
	}
}

func i206FullLastPage(t *testing.T, f *i206Fixture) {
	p := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-fifty","limit":50,"after":{}}`)
	if len(p.Results) != 50 {
		t.Fatalf("rows = %d, want 50", len(p.Results))
	}
	if p.NextAfter != nil {
		t.Errorf("next_after = %+v on the last page, want none (no further row exists)", *p.NextAfter)
	}
	p = i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-fifty","limit":49,"after":{}}`)
	if len(p.Results) != 49 || p.NextAfter == nil {
		t.Errorf("limit 49 of 50: %d rows, next_after %v; want 49 and a cursor", len(p.Results), p.NextAfter)
	}
}

// i206LegacyBytes: a call without the new arguments keeps its text byte for
// byte — the text a client parses. Where there is something to report (the
// limit was lowered, more rows exist), the signal rides beside it in
// structuredContent; where there is nothing, the answer is what it was.
func i206LegacyBytes(t *testing.T, f *i206Fixture) {
	call := func(in searchInput) *mcp.CallToolResult {
		r, _, err := mcpSearchHandler(f.cfg)(f.keyCtx, nil, in)
		if err != nil || r.IsError {
			t.Fatalf("search %+v: err %v, result %s", in, err, mcpTextOf(r))
		}
		if len(r.Content) != 1 {
			t.Fatalf("search %+v: %d content blocks, want 1", in, len(r.Content))
		}
		return r
	}
	render := func(rows []store.BlockPreview) string {
		data, _ := json.MarshalIndent(rows, "", "  ")
		return string(data)
	}

	t.Run("nothing_to_report", func(t *testing.T) {
		r := call(searchInput{Category: "i206-fifty", Limit: 50})
		if got, want := resultText(t, r), render(f.storeRows(t, "i206-fifty", 50)); got != want {
			t.Errorf("text changed:\n got %.300s\nwant %.300s", got, want)
		}
		if r.StructuredContent != nil {
			t.Errorf("structuredContent = %#v, want none when nothing was lowered and no row follows", r.StructuredContent)
		}
		q := call(searchInput{Query: "enumeration", Category: "i206-enum", Limit: 5})
		if q.StructuredContent != nil {
			t.Errorf("full-text call: structuredContent = %#v, want none (top matches never page)", q.StructuredContent)
		}
	})

	t.Run("lowered_limit_and_more_rows", func(t *testing.T) {
		r := call(searchInput{Category: "i206-enum", Limit: 200})
		rows := f.storeRows(t, "i206-enum", 50)
		if got, want := resultText(t, r), render(rows); got != want {
			t.Errorf("text changed:\n got %.300s\nwant %.300s", got, want)
		}
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil || r.StructuredContent == nil {
			t.Fatalf("structuredContent = %#v (err %v), want the paging signal", r.StructuredContent, err)
		}
		var sig i206Page
		if err := json.Unmarshal(raw, &sig); err != nil {
			t.Fatalf("decode structuredContent %s: %v", raw, err)
		}
		last := rows[len(rows)-1]
		if sig.Limit != 50 || sig.RequestedLimit != 200 || sig.NextAfter == nil ||
			sig.NextAfter.ID != last.ID || !sig.NextAfter.UpdatedAt.Equal(last.UpdatedAt) || sig.Results != nil {
			t.Errorf("structuredContent = %s; want limit 50, requested_limit 200, next_after at the last row %s, no rows", raw, last.ID)
		}
	})
}

func i206CursorSafety(t *testing.T, f *i206Fixture) {
	// The other tenant owns five blocks in the same category.
	var otherIDs []string
	for i := 0; i < 5; i++ {
		otherIDs = append(otherIDs, f.upsert(t, "i206-other", "i206-enum", fmt.Sprintf("i206 other %d", i), "other body", "", nil))
	}
	otherCtx := context.WithValue(context.Background(), authResultKey, f.otherAR)
	// The first key's newest row is written after them, so its cursor lies
	// ahead of every row of the other key: a cursor that leaked nothing and a
	// cursor that read nothing must not look alike.
	anchor := f.storeTool(t, "i206-enum", "i206 enum cursor anchor", "anchor body")
	mine := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-enum","limit":1,"after":{}}`)
	if mine.NextAfter == nil || mine.NextAfter.ID != anchor {
		t.Fatalf("fixture: first-page cursor %v, want the anchor %s", mine.NextAfter, anchor)
	}

	t.Run("a_foreign_cursor_reads_only_the_callers_rows", func(t *testing.T) {
		got := i206PageOf(t, otherCtx, f.cfg, `{"category":"i206-enum","limit":50,"after":`+i206Cursor(t, mine.NextAfter)+`}`)
		allowed := map[string]bool{}
		for _, id := range otherIDs {
			allowed[id] = true
		}
		for _, id := range i206IDs(got.Results) {
			if !allowed[id] {
				t.Errorf("the other key read %s through a cursor of the first key", id)
			}
		}
		if len(got.Results) != len(otherIDs) {
			t.Errorf("the other key read %d rows after the cursor, want its own %d", len(got.Results), len(otherIDs))
		}
		// The same position with an id that names no block answers the same:
		// the cursor is a position, never an existence check.
		synthetic := &store.SearchCursor{UpdatedAt: mine.NextAfter.UpdatedAt, ID: "00000000-0000-4000-8000-000000000000"}
		same := i206PageOf(t, otherCtx, f.cfg, `{"category":"i206-enum","limit":50,"after":`+i206Cursor(t, synthetic)+`}`)
		if !reflect.DeepEqual(i206IDs(got.Results), i206IDs(same.Results)) {
			t.Errorf("answer depends on whether the cursor's id is a readable block: %v vs %v", i206IDs(got.Results), i206IDs(same.Results))
		}
	})

	t.Run("a_cursor_under_other_filters_applies_those_filters", func(t *testing.T) {
		got := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-fifty","limit":50,"after":`+i206Cursor(t, mine.NextAfter)+`}`)
		for _, r := range got.Results {
			if r["category"] != "i206-fifty" {
				t.Errorf("row %v outside the filter of the call", r["id"])
			}
		}
		if len(got.Results) != 50 {
			t.Errorf("rows after the cursor under the other filter = %d, want all 50 of that filter", len(got.Results))
		}
	})

	t.Run("a_malformed_cursor_is_refused_without_a_store_error", func(t *testing.T) {
		a := i206Wire(t, f.keyCtx, f.cfg, `{"category":"i206-enum","after":{"after_updated":"2026-01-01T00:00:00Z","after_id":"not-a-uuid"}}`)
		if !a.IsError || !strings.Contains(a.text(), "next_after") || strings.Contains(a.text(), "SQLSTATE") || strings.Contains(a.text(), "store:") {
			t.Errorf("malformed after_id: isError %t, text %q; want the cursor refusal, no store error", a.IsError, a.text())
		}
	})

	t.Run("a_cursor_without_a_position_is_the_first_page", func(t *testing.T) {
		first := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-enum","limit":5,"after":{}}`)
		partial := i206PageOf(t, f.keyCtx, f.cfg, `{"category":"i206-enum","limit":5,"after":{"after_id":"00000000-0000-4000-8000-000000000000"}}`)
		if !reflect.DeepEqual(i206IDs(first.Results), i206IDs(partial.Results)) {
			t.Errorf("a cursor without after_updated = %v, want page 1 %v (the REST rule)", i206IDs(partial.Results), i206IDs(first.Results))
		}
	})
}
