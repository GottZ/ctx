//go:build integration

// Issue #234 point 1: the positive arm of TestMCPQueryRetrievalOnlyPrivilegeParity.
// That test shows that MCP surfaces the same refusal as REST for a key over its
// tenant quota; it stays green with the forwarding of `synthesize` and
// `include_content` removed, because the quota gate refuses before anything
// reads either field. This test runs the same non-admin member key UNDER its
// quota through the real /api/query pipeline, once over REST and once over the
// MCP `query` tool, both with synthesize:false and include_content:true, and
// holds the two answers against each other: the same rows, each with the same
// content snippet and the same truncation flag (point 2), and none of a scope
// the key does not read.
//
// The blocks are written through POST /api/store. The tool talks to the real
// query handler without a seam in between, so with the two forwarding lines
// gone the tool asks for synthesis, which this fixture has no backend for, and
// the arm goes red.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestMCPQueryRetrievalOnlyParityUnderQuota -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestMCPQueryRetrievalOnlyParityUnderQuota(t *testing.T) {
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
	keyCtx := func(label, home string) context.Context {
		t.Helper()
		_, plain, err := store.CreateApiKey(ctx, pool, label, home, nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", label, err)
		}
		if ar.IsAdmin || ar.TenantRole == auth.RoleAdmin {
			t.Fatalf("key %s is an admin key; the parity is about a member key", label)
		}
		return context.WithValue(ctx, authResultKey, ar)
	}
	member := keyCtx("i234-member", "private")
	foreign := keyCtx("i234-foreign", "i234-foreign")

	storeH := NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{
		Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
	}}, reg)
	long := strings.Repeat("ledger line with ä umlaut ", 120) // 3 120 runes, over the snippet cap
	for _, b := range []struct {
		ctx            context.Context
		title, content string
	}{
		{member, "parity short", "a short ledger note"},
		{member, "parity long", long},
		{foreign, "parity foreign", "a ledger note of another scope"},
	} {
		raw, _ := json.Marshal(map[string]any{"category": "i234", "title": b.title, "content": b.content})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(b.ctx)
		rec := httptest.NewRecorder()
		storeH.HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("store %q: status %d, body %s", b.title, rec.Code, rec.Body.String())
		}
	}

	// The member's tenant has a call budget and is under it: the gate runs and
	// lets the call through.
	budget := 1000
	acc := backends.NewQuotaAccountant(nil, 0)
	acc.SeedForTest("private", backends.TenantQuota{DailyCalls: &budget, Enabled: true}, 1, 0)
	backend := bw2NewBackend(t)
	h := NewQueryHandler(pool, config.NewStore(bw2Config()), bw2Pool(backend.srv.URL), acc, reg, snapshotTestAdmitter(t))

	type row struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Category  string `json:"category"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	rest := func(t *testing.T) []row {
		t.Helper()
		body := `{"query":"` + bw2GoldenQuery + `","synthesize":false,"include_content":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body)).WithContext(member)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.HandleQuery(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST: status %d, body %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Sources   []row  `json:"sources"`
			ErrorCode string `json:"error_code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ErrorCode != "" {
			t.Fatalf("REST: err=%v error_code=%q body %s", err, out.ErrorCode, rec.Body.String())
		}
		return out.Sources
	}
	tool := func(t *testing.T) []row {
		t.Helper()
		cfg := MCPConfig{Pool: pool, Blocktypes: reg, QueryHandler: http.HandlerFunc(h.HandleQuery)}
		res, _, err := mcpQueryHandler(cfg)(member, nil, typeFilterArgs[queryInput](t,
			`{"question":"`+bw2GoldenQuery+`","synthesize":false,"include_content":true}`))
		if err != nil || res.IsError {
			t.Fatalf("MCP: err=%v IsError=%v text=%s", err, res != nil && res.IsError, mcpTextOf(res))
		}
		var out struct {
			Sources []row `json:"sources"`
		}
		if err := json.Unmarshal([]byte(mcpTextOf(res)), &out); err != nil {
			t.Fatalf("MCP: the retrieval-only answer is not JSON: %v\n%s", err, mcpTextOf(res))
		}
		return out.Sources
	}
	byID := func(rows []row) []row {
		out := slices.Clone(rows)
		slices.SortFunc(out, func(a, b row) int { return strings.Compare(a.ID, b.ID) })
		return out
	}

	// The first query embeds the stored blocks inline (bw2Config); the two
	// answers compared below both see the embedded store.
	_ = rest(t)
	restRows, toolRows := byID(rest(t)), byID(tool(t))

	if !slices.Equal(restRows, toolRows) {
		t.Fatalf("REST and MCP answer different rows:\n REST %+v\n MCP  %+v", restRows, toolRows)
	}
	titles := map[string]row{}
	for _, r := range restRows {
		titles[r.Title] = r
	}
	if _, ok := titles["parity foreign"]; ok {
		t.Errorf("a block of a scope the key does not read is in the answer")
	}
	short, okShort := titles["parity short"]
	cut, okLong := titles["parity long"]
	if !okShort || !okLong {
		t.Fatalf("the answer lacks the member's blocks: %+v", restRows)
	}
	if short.Content != "a short ledger note" || short.Truncated {
		t.Errorf("short block: content %q truncated %v, want the whole content and no flag", short.Content, short.Truncated)
	}
	if n := len([]rune(cut.Content)); n != maxRetrievalSnippet || !cut.Truncated {
		t.Errorf("long block: %d runes, truncated %v; want the %d-rune snippet flagged truncated", n, cut.Truncated, maxRetrievalSnippet)
	}
	if !strings.HasPrefix(long, cut.Content) {
		t.Errorf("the snippet is not a prefix of the block")
	}
}
