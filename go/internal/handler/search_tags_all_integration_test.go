//go:build integration

// Issue #219: the `tags` filter of the block search matches a block that
// carries ANY of the given tags (array overlap). `tags_all` asks for the
// blocks that carry ALL of them (containment), on REST /api/search and on the
// MCP `search` tool, through the one core both transports share. The two
// filters combine: "all of these, and any of those".
//
// The blocks are written through POST /api/store, and every probe goes over
// the wire vocabulary (a raw REST body, a raw tool-argument object), so a
// surface that drops the key answers the unfiltered listing — the RED state
// before the field existed.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestSearchTagsAll -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSearchTagsAll(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	_, plain, err := store.CreateApiKey(ctx, pool, "i219-tags-all", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)
	cfg := staticConfigStore{cfg: &config.Config{
		Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
	}}

	// Two tag dimensions: the entity (acme, globex) and the kind (invoice,
	// contract), plus one block with both of one dimension.
	storeH := NewStoreHandler(pool, cfg, reg)
	for _, b := range []struct {
		title string
		tags  []string
	}{
		{"acme invoice", []string{"acme", "invoice"}},
		{"acme contract", []string{"acme", "contract"}},
		{"globex invoice", []string{"globex", "invoice"}},
		{"acme invoice signed", []string{"acme", "invoice", "signed"}},
		{"untagged", nil},
	} {
		raw, _ := json.Marshal(map[string]any{
			"category": "i219", "title": b.title, "tags": b.tags,
			"content": "ledger record " + b.title,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		storeH.HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("store %q: status %d, body %s", b.title, rec.Code, rec.Body.String())
		}
	}

	searchH := NewSearchHandler(pool, cfg, reg)
	rest := func(t *testing.T, body string) (titles []string, resp map[string]json.RawMessage) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(body)).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		searchH.HandleSearch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("search %s: status %d, body %s", body, rec.Code, rec.Body.String())
		}
		var out struct {
			Results []struct {
				Title string `json:"title"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		for _, r := range out.Results {
			titles = append(titles, r.Title)
		}
		slices.Sort(titles)
		return titles, resp
	}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfg}
	tool := func(t *testing.T, raw string) []string {
		t.Helper()
		res, _, err := mcpSearchHandler(mcpCfg)(keyCtx, nil, typeFilterArgs[searchInput](t, raw))
		if err != nil || res.IsError {
			t.Fatalf("mcp search %s: err=%v answer=%s", raw, err, mcpTextOf(res))
		}
		text := mcpTextOf(res)
		var rows []struct {
			Title string `json:"title"`
		}
		if strings.HasPrefix(strings.TrimSpace(text), "{") {
			var page struct {
				Results []struct {
					Title string `json:"title"`
				} `json:"results"`
			}
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				t.Fatalf("mcp search %s: page answer is not JSON: %v\n%s", raw, err, text)
			}
			rows = page.Results
		} else if err := json.Unmarshal([]byte(text), &rows); err != nil {
			t.Fatalf("mcp search %s: answer is not JSON: %v\n%s", raw, err, text)
		}
		var titles []string
		for _, r := range rows {
			titles = append(titles, r.Title)
		}
		slices.Sort(titles)
		return titles
	}

	// Premise: the overlap filter keeps its meaning, so a containment probe
	// below cannot pass on a filter that silently changed.
	if got, _ := rest(t, `{"tags":["acme","invoice"]}`); !slices.Equal(got,
		[]string{"acme contract", "acme invoice", "acme invoice signed", "globex invoice"}) {
		t.Fatalf("tags (overlap) answered %v", got)
	}

	both := []string{"acme invoice", "acme invoice signed"}
	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"browse", `"tags_all":["acme","invoice"]`, both},
		{"fulltext", `"query":"ledger","tags_all":["acme","invoice"]`, both},
		{"one tag", `"tags_all":["invoice"]`, []string{"acme invoice", "acme invoice signed", "globex invoice"}},
		{"all and any", `"tags_all":["acme","invoice"],"tags":["signed","contract"]`, []string{"acme invoice signed"}},
		{"no block carries all", `"tags_all":["acme","globex"]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := rest(t, `{`+tc.args+`}`); !slices.Equal(got, tc.want) {
				t.Errorf("REST {%s} answered %v, want %v", tc.args, got, tc.want)
			}
			if got := tool(t, `{`+tc.args+`}`); !slices.Equal(got, tc.want) {
				t.Errorf("MCP {%s} answered %v, want %v", tc.args, got, tc.want)
			}
		})
	}

	// The MCP page form and REST's keyset cursor run the same filter page by
	// page: one row per page, two pages, and the second has no cursor.
	t.Run("paged", func(t *testing.T) {
		if got := tool(t, `{"tags_all":["acme","invoice"],"after":{}}`); !slices.Equal(got, both) {
			t.Errorf("MCP page form answered %v, want %v", got, both)
		}
		var seen []string
		body := `{"tags_all":["acme","invoice"],"limit":1}`
		for page := 0; page < 3; page++ {
			got, resp := rest(t, body)
			seen = append(seen, got...)
			if string(resp["next_after"]) == "null" {
				break
			}
			body = `{"tags_all":["acme","invoice"],"limit":1,"after":` + string(resp["next_after"]) + `}`
		}
		slices.Sort(seen)
		if !slices.Equal(seen, both) {
			t.Errorf("REST pages answered %v, want %v", seen, both)
		}
	})

	// The echo: filters.tags_all names the filter that ran, and a request
	// without the field answers the filters object it answered before.
	t.Run("echo", func(t *testing.T) {
		_, resp := rest(t, `{"tags_all":["acme","invoice"]}`)
		var filters map[string]json.RawMessage
		if err := json.Unmarshal(resp["filters"], &filters); err != nil {
			t.Fatalf("filters: %v", err)
		}
		if got := string(filters["tags_all"]); got != `["acme","invoice"]` {
			t.Errorf("filters.tags_all = %s, want [\"acme\",\"invoice\"]", got)
		}
		_, resp = rest(t, `{"tags":["acme"]}`)
		var without map[string]json.RawMessage
		if err := json.Unmarshal(resp["filters"], &without); err != nil {
			t.Fatalf("filters: %v", err)
		}
		if _, ok := without["tags_all"]; ok {
			t.Errorf("a request without tags_all echoes the key: %s", resp["filters"])
		}
	})
}
