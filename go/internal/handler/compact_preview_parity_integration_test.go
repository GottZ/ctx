//go:build integration

// Issue #342: the compact answers of search and recent are a page subquery with
// the preview computed above it. This test holds every compact answer, through
// the real handlers, against the full answer of the same request — a statement
// #342 did not change, which shares the filters, the order and the limit with
// the compact one and outputs the content the preview is cut from. Same rows,
// same order, every field equal, and preview and length taken from that
// content: LEFT(content, 200) and the character count, multi-byte characters
// across the 200th position and TOAST-wide rows included.
//
// The surfaces: POST /api/search (compact true against false, browse with the
// keyset cursor and full text), the MCP search tool (without against with
// include_content), POST /api/recent and the MCP recent tool (against the full
// browse answer with the same filters), and the chat tools ctx_search and
// ctx_recent (against the store's full read). The fixture is written through
// the production write path: two tenants, a reader with two scopes, block
// grants (one of them on an archived block), an archived block of its own,
// a tenant's own retrieval-excluded type, checkpoint blocks of transcript width.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ppTerm is the full-text term of the fixture; block n carries it n times, so
// no two matching blocks rank alike and the full-text order is total.
const ppTerm = "paritybegriff"

// ppContent is the content of the n-th block: its title line, multi-byte
// characters on both sides of the 200th character, the term n times, and
// filler up to width characters.
func ppContent(n, width int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "block %03d ", n)
	for b.Len() < 190 {
		b.WriteString("x")
	}
	b.WriteString("äöü—🙂ßé«»") // characters 191-200 and beyond: one to four bytes each
	for i := 0; i < n; i++ {
		b.WriteString(" " + ppTerm)
	}
	for i := 0; utf8.RuneCountInString(b.String()) < width; i++ {
		fmt.Fprintf(&b, " filler%d Übergabe", i)
	}
	return b.String()
}

func ppPreview(content string) (string, int) {
	r := []rune(content)
	if len(r) > 200 {
		return string(r[:200]), len(r)
	}
	return content, len(r)
}

// ppSameRow fails unless compact is the preview form of full.
func ppSameRow(t *testing.T, form string, i int, compact, full store.BlockPreview) {
	t.Helper()
	preview, length := ppPreview(full.Content)
	if compact.ID != full.ID {
		t.Fatalf("%s row %d: compact id %s, full id %s", form, i, compact.ID, full.ID)
	}
	if compact.ContentPreview != preview || compact.ContentLength != length {
		t.Fatalf("%s row %d (%s): preview %q / length %d, want %q / %d", form, i, full.ID, compact.ContentPreview, compact.ContentLength, preview, length)
	}
	if compact.Content != "" || compact.Metadata != nil {
		t.Fatalf("%s row %d: the compact row carries content or metadata", form, i)
	}
	c, f := compact, full
	c.ContentPreview, c.ContentLength = "", 0
	f.Content, f.Metadata = "", nil
	cj, _ := json.Marshal(c)
	fj, _ := json.Marshal(f)
	if !bytes.Equal(cj, fj) {
		t.Fatalf("%s row %d: fields differ\n compact %s\n full    %s", form, i, cj, fj)
	}
}

func ppSameRows(t *testing.T, form string, compact, full []store.BlockPreview) {
	t.Helper()
	if len(compact) != len(full) {
		t.Fatalf("%s: %d compact rows, %d full rows", form, len(compact), len(full))
	}
	for i := range compact {
		ppSameRow(t, form, i, compact[i], full[i])
	}
}

type ppSearchAnswer struct {
	Success   bool                `json:"success"`
	Results   []store.BlockPreview `json:"results"`
	NextAfter *store.SearchCursor `json:"next_after"`
}

func ppREST(t *testing.T, h *SearchHandler, ar *auth.AuthResult, body map[string]any) ppSearchAnswer {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleSearch(rec, req)
	var out ppSearchAnswer
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Success {
		t.Fatalf("REST search %s: status %d, body %s", raw, rec.Code, rec.Body.String())
	}
	return out
}

func ppWith(body map[string]any, k string, v any) map[string]any {
	out := map[string]any{k: v}
	for bk, bv := range body {
		out[bk] = bv
	}
	return out
}

// ppFixture writes the blocks and returns the reader and the ids of the two
// archived blocks (one in the reader's scope, one granted to it).
func ppFixture(t *testing.T, pool *pgxpool.Pool) (*auth.AuthResult, []string) {
	t.Helper()
	ctx := context.Background()
	tenant := func(slug string, scopes ...string) string {
		tn, err := store.CreateTenant(ctx, pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		for _, s := range scopes {
			if _, err := store.AssignTenantScope(ctx, pool, tn.ID, s, nil); err != nil {
				t.Fatalf("assign %s: %v", s, err)
			}
		}
		return tn.ID
	}
	reader := tenant("pp-reader", "pp-a", "pp-b")
	tenant("pp-owner", "pp-f")
	cfg := json.RawMessage(`{"v":1,"retrieval":{"policy":"excluded"},"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},"parent":{"mode":"none"},"classify":{"priority":100}}`)
	if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "pp-transcript", Scope: "pp-a", DisplayName: "pp-transcript", Config: cfg}, nil, "pp-test"); err != nil {
		t.Fatalf("type: %v", err)
	}
	n := 0
	write := func(scope, category, typ string, width int, tags []string) string {
		t.Helper()
		n++
		b, err := store.UpsertBlock(ctx, pool, category, fmt.Sprintf("pp block %03d %s", n, typ), ppContent(n, width), tags, nil, scope, true, store.SensitivityWrite{}, typ)
		if err != nil {
			t.Fatalf("upsert %d: %v", n, err)
		}
		time.Sleep(time.Millisecond) // distinct updated_at: recent orders by it alone
		return b.ID
	}
	const wide = 35000
	for i := 0; i < 3; i++ {
		write("pp-a", "pp-cat-1", "knowledge", 600, []string{"pp-tag"})
		write("pp-a", "compaction-checkpoints", "checkpoint", wide, []string{"checkpoint-head"})
		write("pp-a", "pp-cat-2", "pp-transcript", wide, nil)
		write("pp-b", "pp-cat-2", "knowledge", 3000, nil)
		write("pp-b", "compaction-checkpoints", "checkpoint", wide, nil)
		write("pp-a", "pp-cat-1", "audit-trail", 150, []string{"pp-tag"})
	}
	granted := write("pp-f", "pp-cat-1", "knowledge", 800, nil)
	grantedWide := write("pp-f", "compaction-checkpoints", "checkpoint", wide, nil)
	grantedArchived := write("pp-f", "pp-cat-1", "knowledge", 800, nil)
	write("pp-f", "pp-cat-1", "knowledge", 800, nil) // not granted
	ownArchived := write("pp-a", "pp-cat-1", "knowledge", wide, []string{"pp-tag"})
	for _, id := range []string{granted, grantedWide, grantedArchived} {
		if _, err := store.CreateBlockGrant(ctx, pool, id, reader, ""); err != nil {
			t.Fatalf("grant %s: %v", id, err)
		}
	}
	if _, err := store.DeleteBlock(ctx, pool, grantedArchived, []string{"pp-f"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := store.DeleteBlock(ctx, pool, ownArchived, []string{"pp-a"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// One block written twice: created_at and updated_at differ.
	if _, err := store.UpsertBlock(ctx, pool, "pp-cat-1", "pp block 001 knowledge", ppContent(1, 700)+" again", []string{"pp-tag"}, nil, "pp-a", true, store.SensitivityWrite{}, "knowledge"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember,
		HomeScope: "pp-a", ReadScopes: []string{"pp-a", "pp-b"}, TenantID: reader,
	}, []string{ownArchived, grantedArchived}
}

func TestCompactPreviewParity_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	ar, archived := ppFixture(t, pool)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	cfgStore := staticConfigStore{cfg: &config.Config{}}
	searchH := NewSearchHandler(pool, cfgStore, reg)
	recentH := NewRecentHandler(pool, cfgStore, reg)
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg}
	mcpCtx := context.WithValue(ctx, authResultKey, ar)

	wideSeen, foreignSeen := false, false
	note := func(rows []store.BlockPreview) {
		for _, r := range rows {
			wideSeen = wideSeen || len(r.Content) > 30000
			foreignSeen = foreignSeen || r.Scope == "pp-f"
			for _, id := range archived {
				if r.ID == id {
					t.Fatalf("archived block %s in an answer", id)
				}
			}
		}
	}

	// POST /api/search: every form compact against full.
	forms := []map[string]any{
		{}, {"limit": 50}, {"limit": 3}, {"category": "pp-cat-2"}, {"tags": []string{"pp-tag"}},
		{"types": []string{"checkpoint"}}, {"types": []string{"pp-transcript"}},
		{"types_exclude": []string{"checkpoint"}}, {"block_roles_exclude": []string{"knowledge"}},
		{"query": ppTerm}, {"query": ppTerm, "limit": 50}, {"query": ppTerm, "limit": 3},
		{"query": ppTerm, "category": "compaction-checkpoints"}, {"query": ppTerm, "types": []string{"checkpoint"}},
		{"query": ppTerm, "types_exclude": []string{"checkpoint", "system-meta"}},
		{"query": ppTerm, "tags": []string{"pp-tag"}}, {"query": "nowherefoundterm"},
	}
	for _, f := range forms {
		compact := ppREST(t, searchH, ar, ppWith(f, "compact", true))
		full := ppREST(t, searchH, ar, ppWith(f, "compact", false))
		form, _ := json.Marshal(f)
		ppSameRows(t, "REST search "+string(form), compact.Results, full.Results)
		note(full.Results)
	}
	// Browse pages through the keyset cursor, three rows a page.
	var after *store.SearchCursor
	for page := 1; page <= 20; page++ {
		body := map[string]any{"limit": 3}
		if after != nil {
			body["after"] = after
		}
		compact := ppREST(t, searchH, ar, ppWith(body, "compact", true))
		full := ppREST(t, searchH, ar, ppWith(body, "compact", false))
		ppSameRows(t, fmt.Sprintf("REST browse page %d", page), compact.Results, full.Results)
		if (compact.NextAfter == nil) != (full.NextAfter == nil) || (compact.NextAfter != nil && *compact.NextAfter != *full.NextAfter) {
			t.Fatalf("browse page %d: next_after %v (compact) against %v (full)", page, compact.NextAfter, full.NextAfter)
		}
		if compact.NextAfter == nil {
			break
		}
		after = compact.NextAfter
	}
	if !wideSeen || !foreignSeen {
		t.Fatalf("fixture did not reach the answers: wide rows %t, granted rows %t", wideSeen, foreignSeen)
	}

	// MCP search: without include_content against with it (a page of full
	// rows ends at 64 KiB, so it is a prefix of the compact page).
	for _, in := range []searchInput{
		{}, {Limit: 50}, {Category: "pp-cat-2"}, {Types: []string{"knowledge"}}, {TypesExclude: []string{"checkpoint"}},
		{Query: ppTerm}, {Query: ppTerm, Limit: 50}, {Query: ppTerm, Category: "compaction-checkpoints"},
	} {
		run := func(in searchInput) []store.BlockPreview {
			res, _, err := mcpSearchHandler(mcpCfg)(mcpCtx, nil, in)
			if err != nil || res.IsError {
				t.Fatalf("MCP search %+v: %v %s", in, err, mcpResultText(res))
			}
			text := mcpResultText(res)
			var rows []store.BlockPreview
			if in.IncludeContent {
				var page struct {
					Results []store.BlockPreview `json:"results"`
				}
				if err := json.Unmarshal([]byte(text), &page); err != nil {
					t.Fatalf("MCP search page: %v", err)
				}
				return page.Results
			}
			if err := json.Unmarshal([]byte(text), &rows); err != nil {
				t.Fatalf("MCP search rows: %v", err)
			}
			return rows
		}
		compact := run(in)
		withContent := in
		withContent.IncludeContent = true
		full := run(withContent)
		if len(full) == 0 && len(compact) > 0 {
			t.Fatalf("MCP search %+v: no full rows", in)
		}
		if len(full) > len(compact) {
			t.Fatalf("MCP search %+v: %d full rows, %d compact", in, len(full), len(compact))
		}
		ppSameRows(t, fmt.Sprintf("MCP search %+v", in), compact[:len(full)], full)
	}

	// recent: REST and MCP against the full browse answer of the same filters.
	recentForms := []struct {
		body map[string]any
		in   recentInput
	}{
		{map[string]any{}, recentInput{}},
		{map[string]any{"limit": 50}, recentInput{Limit: 50}},
		{map[string]any{"limit": 3}, recentInput{Limit: 3}},
		{map[string]any{"category": "compaction-checkpoints", "limit": 20}, recentInput{Category: "compaction-checkpoints", Limit: 20}},
		{map[string]any{"types": []string{"pp-transcript"}}, recentInput{Types: []string{"pp-transcript"}}},
		{map[string]any{"types_exclude": []string{"knowledge"}, "limit": 50}, recentInput{TypesExclude: []string{"knowledge"}, Limit: 50}},
	}
	for _, f := range recentForms {
		full := ppREST(t, searchH, ar, ppWith(f.body, "compact", false)).Results
		raw, _ := json.Marshal(f.body)
		req := httptest.NewRequest(http.MethodPost, "/api/recent", bytes.NewReader(raw))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		recentH.HandleRecent(rec, req)
		var rest struct {
			Results []store.BlockPreview `json:"results"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rest) != nil {
			t.Fatalf("REST recent %s: %d %s", raw, rec.Code, rec.Body.String())
		}
		// recent selects no sensitivity; everything else is the browse row.
		fullNoSens := make([]store.BlockPreview, len(full))
		for i, r := range full {
			r.Sensitivity = ""
			fullNoSens[i] = r
		}
		ppSameRows(t, "REST recent "+string(raw), rest.Results, fullNoSens)

		res, _, err := mcpRecentHandler(mcpCfg)(mcpCtx, nil, f.in)
		if err != nil || res.IsError {
			t.Fatalf("MCP recent %+v: %v", f.in, err)
		}
		text := mcpResultText(res)
		ids := rpIDRe.FindAllStringSubmatch(text, -1)
		if len(ids) != len(full) {
			t.Fatalf("MCP recent %+v: %d rows, want %d\n%s", f.in, len(ids), len(full), text)
		}
		for i, m := range ids {
			preview, _ := ppPreview(full[i].Content)
			if m[1] != full[i].ID || !strings.Contains(text, "    "+preview+"\n") {
				t.Fatalf("MCP recent %+v row %d: id %s preview missing, want %s", f.in, i, m[1], full[i].ID)
			}
		}
	}

	// chat tools: against the store's full read (the chat tools read scope-only).
	exec := chat.NewExecutor(pool, nil, 8000)
	type chatAnswer struct {
		Blocks []struct {
			ID      string `json:"id"`
			Preview string `json:"preview"`
		} `json:"blocks"`
	}
	for _, c := range []struct {
		tool, args string
		query      string
		category   string
		limit      int
	}{
		{"ctx_search", `{"query":""}`, "", "", 10},
		{"ctx_search", `{"query":"` + ppTerm + `","limit":20}`, ppTerm, "", 20},
		{"ctx_search", `{"query":"` + ppTerm + `","category":"pp-cat-2"}`, ppTerm, "pp-cat-2", 10},
		{"ctx_recent", `{"limit":20}`, "", "", 20},
		{"ctx_recent", `{"category":"compaction-checkpoints"}`, "", "compaction-checkpoints", 10},
	} {
		out := exec.Run(ctx, "pp-a", ar.ReadScopes, "", llm.ToolCall{Function: llm.ToolCallFunction{Name: c.tool, Arguments: json.RawMessage(c.args)}})
		if !out.OK {
			t.Fatalf("%s %s: %s", c.tool, c.args, out.Content)
		}
		var got chatAnswer
		if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		full, err := store.SearchBlocks(ctx, pool, nil, c.query, ar.ReadScopes, c.category, nil, c.limit, false, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Blocks) != len(full) {
			t.Fatalf("%s %s: %d blocks, want %d", c.tool, c.args, len(got.Blocks), len(full))
		}
		for i, b := range got.Blocks {
			preview, _ := ppPreview(full[i].Content)
			if b.ID != full[i].ID || b.Preview != preview {
				t.Fatalf("%s %s row %d: %s %q, want %s %q", c.tool, c.args, i, b.ID, b.Preview, full[i].ID, preview)
			}
		}
	}
}
