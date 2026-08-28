//go:build integration

// V-11 consumer gates (design/02 §5.1 BA7, Schicht 3): the untrusted framing
// must reach the READ surfaces — MCP `search`/`get`/`recent` and REST
// /api/search — not only the synthesis prompt.
//
// BA7's finding: the framing had exactly ONE consumption site in the whole
// tree (handler/query.go, the synthesis prompt), so a `checkpoint` block —
// captured session transcript, i.e. foreign text — arrived at every other
// reader looking exactly like a first-party knowledge block.
//
// Every probe decodes the SERIALIZED answer (JSON body / rendered tool text),
// never a Go struct field: before this wave the key simply did not exist and
// the decoder dropped it, and that silent absence IS the red state.
//
//	go test -tags=integration ./internal/handler/ -run TestUntrustedV11 -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

const v11Category = "v11-untrusted"

// v11Fixture seeds one block per trust class: `checkpoint` carries
// retrieval.untrusted=true (builtin.go, V-W7), `knowledge` does not — so the
// same probe covers both the positive assertion and the omitempty negative.
func v11Fixture(t *testing.T, pool *pgxpool.Pool) (untrustedID, trustedID string) {
	t.Helper()
	for _, fx := range []struct {
		title    string
		typeName string
		out      *string
	}{
		{"V11 Checkpoint", "checkpoint", &untrustedID},
		{"V11 Knowledge", "knowledge", &trustedID},
	} {
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ($1, $2, 'v11 fixture body', 'private', $3) RETURNING id::text`,
			v11Category, fx.title, fx.typeName).Scan(fx.out); err != nil {
			t.Fatalf("seed %s: %v", fx.title, err)
		}
	}
	return untrustedID, trustedID
}

func v11Registry(t *testing.T, pool *pgxpool.Pool) *blocktype.Registry {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg
}

func v11AuthCtx() context.Context {
	return context.WithValue(context.Background(), authResultKey, &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember,
		ApiKeyID:  "00000000-0000-7000-8000-000000000000",
		HomeScope: "private", ReadScopes: []string{"private"},
	})
}

// v11Rows decodes a JSON array of block rows into title → untrusted-key state.
func v11Rows(t *testing.T, raw string) map[string]struct{ present, value bool } {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("decode rows %s: %v", raw, err)
	}
	out := map[string]struct{ present, value bool }{}
	for _, r := range rows {
		if cat, _ := r["category"].(string); cat != v11Category {
			continue
		}
		title, _ := r["title"].(string)
		v, ok := r["untrusted"]
		b, _ := v.(bool)
		out[title] = struct{ present, value bool }{ok, b}
	}
	return out
}

func v11AssertRows(t *testing.T, where string, got map[string]struct{ present, value bool }) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("%s: fixture rows = %v, want both seeded blocks", where, got)
	}
	if c := got["V11 Checkpoint"]; !c.present || !c.value {
		t.Errorf("%s: checkpoint row untrusted=(present=%v,value=%v), want (true,true)", where, c.present, c.value)
	}
	if k := got["V11 Knowledge"]; k.present {
		t.Errorf("%s: knowledge row carries an untrusted key, want it omitted (omitempty)", where)
	}
}

// TestUntrustedV11MCPSearch — MCP `search` renders BlockPreview as JSON, so the
// framing rides the serialized field.
func TestUntrustedV11MCPSearch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	v11Fixture(t, pool)
	cfg := MCPConfig{Pool: pool, Blocktypes: v11Registry(t, pool)}

	var in searchInput
	if err := json.Unmarshal([]byte(`{"category":"`+v11Category+`","limit":50}`), &in); err != nil {
		t.Fatalf("decode tool arguments: %v", err)
	}
	res, _, err := mcpSearchHandler(cfg)(v11AuthCtx(), nil, in)
	if err != nil {
		t.Fatalf("mcp search: transport error %v", err)
	}
	if res.IsError {
		t.Fatalf("mcp search: IsError=true (%s)", mcpTextOf(res))
	}
	v11AssertRows(t, "mcp search", v11Rows(t, mcpTextOf(res)))
}

// TestUntrustedV11MCPGet — MCP `get` renders store.Block as JSON.
func TestUntrustedV11MCPGet_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	untrustedID, trustedID := v11Fixture(t, pool)
	cfg := MCPConfig{Pool: pool, Blocktypes: v11Registry(t, pool)}

	get := func(id string) (bool, bool) {
		t.Helper()
		res, _, err := mcpGetHandler(cfg)(v11AuthCtx(), nil, getInput{ID: id})
		if err != nil {
			t.Fatalf("mcp get %s: transport error %v", id, err)
		}
		if res.IsError {
			t.Fatalf("mcp get %s: IsError=true (%s)", id, mcpTextOf(res))
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(mcpTextOf(res)), &m); err != nil {
			t.Fatalf("decode get %s: %v", id, err)
		}
		v, ok := m["untrusted"]
		b, _ := v.(bool)
		return ok, b
	}

	if present, val := get(untrustedID); !present || !val {
		t.Errorf("mcp get(checkpoint) untrusted=(present=%v,value=%v), want (true,true)", present, val)
	}
	if present, _ := get(trustedID); present {
		t.Errorf("mcp get(knowledge) carries an untrusted key, want it omitted (omitempty)")
	}
}

// TestUntrustedV11MCPRecent — `recent` is the one MCP retrieval tool that does
// NOT go through store.RecentBlocks: it runs its own statement and renders
// PLAIN TEXT, so the framing needs an explicit marker there (§BA7 "the framing
// must be unconditional").
func TestUntrustedV11MCPRecent_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	v11Fixture(t, pool)
	cfg := MCPConfig{Pool: pool, Blocktypes: v11Registry(t, pool)}

	res, _, err := mcpRecentHandler(cfg)(v11AuthCtx(), nil, recentInput{Category: v11Category, Limit: 50})
	if err != nil {
		t.Fatalf("mcp recent: transport error %v", err)
	}
	if res.IsError {
		t.Fatalf("mcp recent: IsError=true (%s)", mcpTextOf(res))
	}
	text := mcpTextOf(res)

	var checkpointLine, knowledgeLine string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, "V11 Checkpoint"):
			checkpointLine = line
		case strings.Contains(line, "V11 Knowledge"):
			knowledgeLine = line
		}
	}
	if checkpointLine == "" || knowledgeLine == "" {
		t.Fatalf("mcp recent: fixture lines missing in %q", text)
	}
	if !strings.Contains(checkpointLine, untrustedMarker) {
		t.Errorf("mcp recent: checkpoint line %q carries no %s marker", checkpointLine, untrustedMarker)
	}
	if strings.Contains(knowledgeLine, untrustedMarker) {
		t.Errorf("mcp recent: knowledge line %q carries the %s marker", knowledgeLine, untrustedMarker)
	}
}

// TestUntrustedV11RESTSearch — /api/search is the REST half; it is also the
// probe that proves the SearchHandler's new registry wiring (a nil registry
// would silently answer without the flag).
func TestUntrustedV11RESTSearch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	v11Fixture(t, pool)
	h := NewSearchHandler(pool, staticConfigStore{cfg: &config.Config{}}, v11Registry(t, pool))

	body, _ := json.Marshal(map[string]any{"category": v11Category, "limit": 50})
	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(string(body)))
	req = req.WithContext(v11AuthCtx())
	rec := httptest.NewRecorder()
	h.HandleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rest search: status %d (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rest search: %v", err)
	}
	v11AssertRows(t, "rest search", v11Rows(t, string(resp.Results)))
}
