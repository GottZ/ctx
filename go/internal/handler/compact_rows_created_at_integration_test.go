//go:build integration

// Issue #205 — a compact row answers `"created_at": "0001-01-01T00:00:00Z"`.
// The compact statement of SearchBlocks and the statement of RecentBlocks do
// not select the column, the scan leaves BlockPreview.CreatedAt at the zero
// time, and `omitempty` never drops a struct value, so the zero time reaches
// the wire as if it were a date.
//
// The probes read the SERIALIZED answer of every surface that renders
// BlockPreview as JSON — the MCP `search` tool, POST /api/search and
// POST /api/recent — and hold `created_at` against the stored column. (The MCP
// `recent` tool renders text with the update date only and is not affected.)
//
// The block is written twice through the MCP `store` tool, so created_at and
// updated_at differ: a row that answered updated_at under the created_at key
// would not pass. `compact:false` on /api/search always selected the column
// and is the control that the fixture and the comparison are alive.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestCompactRowsCarryCreatedAt -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i205CreatedAt returns the parsed created_at of the row with the given id, as
// the surface serialized it. A missing row or a missing key fails the test: the
// wanted answer is the real time, not an absent field.
func i205CreatedAt(t *testing.T, where, rawRows, id string) time.Time {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(rawRows), &rows); err != nil {
		t.Fatalf("%s: decode rows: %v (%s)", where, err, rawRows)
	}
	for _, r := range rows {
		if got, _ := r["id"].(string); got != id {
			continue
		}
		s, ok := r["created_at"].(string)
		if !ok {
			t.Fatalf("%s: row has no created_at string: %v", where, r)
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("%s: created_at %q is not RFC 3339: %v", where, s, err)
		}
		return ts
	}
	t.Fatalf("%s: block %s is not in the answer: %s", where, id, rawRows)
	return time.Time{}
}

func TestCompactRowsCarryCreatedAt(t *testing.T) {
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
	cfgStore := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfgStore}

	_, plain, err := store.CreateApiKey(ctx, pool, "i205", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)

	// Fixture through the production write path: store, then store the same
	// title again with new content, which moves updated_at and keeps created_at.
	const (
		category = "test"
		title    = "i205 compact row created_at"
	)
	storeTool := mcpStoreHandler(mcpCfg)
	for _, content := range []string{"first body of the i205 block", "second body of the i205 block"} {
		r, _, err := storeTool(keyCtx, nil, storeInput{Category: category, Title: title, Content: content})
		if err != nil {
			t.Fatalf("mcp store: protocol error %v", err)
		}
		if r.IsError {
			t.Fatalf("mcp store rejected: %s", resultText(t, r))
		}
	}

	var (
		id               string
		created, updated time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT id::text, created_at, updated_at
		   FROM context_blocks
		  WHERE category = $1 AND title = $2 AND scope = 'private' AND NOT is_archived`,
		category, title,
	).Scan(&id, &created, &updated); err != nil {
		t.Fatalf("read stored block: %v", err)
	}
	if created.IsZero() || created.Equal(updated) {
		t.Fatalf("fixture: created_at %s and updated_at %s must be set and differ", created, updated)
	}

	assert := func(t *testing.T, where, rawRows string) {
		t.Helper()
		got := i205CreatedAt(t, where, rawRows, id)
		if !got.Equal(created) {
			t.Errorf("%s: created_at = %s, want the stored %s (updated_at is %s)",
				where, got.Format(time.RFC3339Nano), created.Format(time.RFC3339Nano), updated.Format(time.RFC3339Nano))
		}
	}

	// rest runs one POST against a real handler and returns the `results` array
	// of the envelope as raw JSON.
	rest := func(t *testing.T, h http.HandlerFunc, path, body string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d, body %s", path, body, rec.Code, rec.Body.String())
		}
		var env struct {
			Results json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s %s: decode %v (body %s)", path, body, err, rec.Body.String())
		}
		return string(env.Results)
	}
	searchH := NewSearchHandler(pool, cfgStore, reg)
	recentH := NewRecentHandler(pool, cfgStore, reg)
	filter := `"category":"` + category + `","limit":50`

	t.Run("control_rest_search_full", func(t *testing.T) {
		assert(t, "POST /api/search compact:false", rest(t, searchH.HandleSearch, "/api/search", `{`+filter+`,"compact":false}`))
	})
	t.Run("mcp_search", func(t *testing.T) {
		r, _, err := mcpSearchHandler(mcpCfg)(keyCtx, nil, searchInput{Category: category, Limit: 50})
		if err != nil {
			t.Fatalf("mcp search: protocol error %v", err)
		}
		if r.IsError {
			t.Fatalf("mcp search rejected: %s", resultText(t, r))
		}
		assert(t, "MCP search", resultText(t, r))
	})
	t.Run("rest_search_compact", func(t *testing.T) {
		assert(t, "POST /api/search", rest(t, searchH.HandleSearch, "/api/search", `{`+filter+`}`))
	})
	t.Run("rest_recent", func(t *testing.T) {
		assert(t, "POST /api/recent", rest(t, recentH.HandleRecent, "/api/recent", `{`+filter+`}`))
	})
}
