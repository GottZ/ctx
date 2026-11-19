//go:build integration

// Issue #208 at the handler: the include_content snippet of a retrieval-only
// query follows the tenant's query.max_source_runes, read from the request's
// config snapshot. The unit test pins retrievalSnippetRunes; this one pins that
// the handler hands it the snapshot's value and the response's source count.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
)

const k1LongBlockID = "019fa500-0000-7000-9000-0000000002aa"

// TestQuerySnippetFollowsMaxSourceRunes: a 9000-rune block that ranks first
// comes back cut at 1500 runes without the key and at 6000 with it (five
// sources at the default limit: SourceRuneCap(6000, 5) = 6000).
//
// Falsifying implementation: the handler passing 0 (or a constant) instead of
// cfg.Query.MaxSourceRunes — both runs then answer 1500.
func TestQuerySnippetFollowsMaxSourceRunes(t *testing.T) {
	pool := bw2Setup(t)
	// Collinear with the fake embed server's query vector, so it ranks first
	// and is inside the default limit of five.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
		 VALUES ($1::uuid, 'knowledge', 'long fixture record', $2, $3, $4, 'knowledge')`,
		k1LongBlockID, strings.Repeat("ab ", 3000), bw2Scope, pgvec.NewVector(mw2TopEmbedding()),
	); err != nil {
		t.Fatalf("insert long block: %v", err)
	}
	emb := bw2NewBackend(t)

	snippet := func(t *testing.T, maxSourceRunes int) (int, int) {
		t.Helper()
		cfg := bw2Config()
		cfg.Query.MaxSourceRunes = maxSourceRunes
		h := NewQueryHandler(pool, config.NewStore(cfg), bw2Pool(emb.srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		rec := bw2Do(t, h, bw2UserKeyID, false, `{"query":"`+bw2GoldenQuery+`","synthesize":false,"include_content":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		sources, _ := bw2DecodeBody(t, rec)["sources"].([]any)
		for _, s := range sources {
			m, _ := s.(map[string]any)
			if m["id"] == k1LongBlockID {
				content, _ := m["content"].(string)
				return len([]rune(content)), len(sources)
			}
		}
		t.Fatalf("the long block is not among the %d sources — the probe would pass vacuously", len(sources))
		return 0, 0
	}

	if got, _ := snippet(t, llm.MaxBlockChars); got != llm.MaxBlockChars {
		t.Errorf("default: snippet %d runes, want %d", got, llm.MaxBlockChars)
	}
	got, n := snippet(t, 6000)
	if want := llm.SourceRuneCap(6000, n); got != want || want <= llm.MaxBlockChars {
		t.Errorf("max_source_runes 6000 with %d sources: snippet %d runes, want %d (> %d)", n, got, want, llm.MaxBlockChars)
	}
}

const k1MidBlockID = "019fa500-0000-7000-9000-0000000002ab"

// TestQuerySnippetTruncatedFlagFollowsTheWindow (#234 with #208): with a raised
// query.max_source_runes the truncated flag of a retrieval-only answer marks
// the snippets cut at the raised window, over REST and over the MCP query tool
// alike. Two blocks rank first: one of 9000 runes, cut at the window and
// flagged, and one of 3000 runes, which the default window cuts and flags and
// the raised one carries whole and unflagged.
//
// Falsifying implementation: the flag compared against llm.MaxBlockChars (the
// old maxRetrievalSnippet) instead of the response's window — the whole
// 3000-rune block is then flagged on both surfaces.
func TestQuerySnippetTruncatedFlagFollowsTheWindow(t *testing.T) {
	pool := bw2Setup(t)
	ctx := context.Background()
	for _, b := range []struct{ id, title, content string }{
		{k1LongBlockID, "long fixture record", strings.Repeat("ab ", 3000)},
		{k1MidBlockID, "mid fixture record", strings.Repeat("cd ", 1000)},
	} {
		// Collinear with the fake embed server's query vector, so both rank
		// ahead of the fixture blocks and inside the default limit of five.
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', $2, $3, $4, $5, 'knowledge')`,
			b.id, b.title, b.content, bw2Scope, pgvec.NewVector(mw2TopEmbedding()),
		); err != nil {
			t.Fatalf("insert %s: %v", b.title, err)
		}
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	emb := bw2NewBackend(t)
	member := context.WithValue(ctx, authResultKey, &auth.AuthResult{
		ApiKeyID: bw2UserKeyID, HomeScope: bw2Scope, ReadScopes: []string{bw2Scope}, IsValid: true,
	})

	type row struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		Truncated *bool  `json:"truncated"`
	}
	answer := func(t *testing.T, maxSourceRunes int) (rest, tool map[string]row, n int) {
		t.Helper()
		cfg := bw2Config()
		cfg.Query.MaxSourceRunes = maxSourceRunes
		h := NewQueryHandler(pool, config.NewStore(cfg), bw2Pool(emb.srv.URL), nil, reg, snapshotTestAdmitter(t))
		const body = `{"query":"` + bw2GoldenQuery + `","synthesize":false,"include_content":true}`

		req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body)).WithContext(member)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.HandleQuery(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST: status %d, body %s", rec.Code, rec.Body.String())
		}
		var restOut struct {
			Sources []row `json:"sources"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &restOut); err != nil {
			t.Fatalf("REST: %v, body %s", err, rec.Body.String())
		}

		mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, QueryHandler: http.HandlerFunc(h.HandleQuery)}
		res, _, err := mcpQueryHandler(mcpCfg)(member, nil, typeFilterArgs[queryInput](t,
			`{"question":"`+bw2GoldenQuery+`","synthesize":false,"include_content":true}`))
		if err != nil || res.IsError {
			t.Fatalf("MCP: err=%v IsError=%v text=%s", err, res != nil && res.IsError, mcpTextOf(res))
		}
		var toolOut struct {
			Sources []row `json:"sources"`
		}
		if err := json.Unmarshal([]byte(mcpTextOf(res)), &toolOut); err != nil {
			t.Fatalf("MCP: the retrieval-only answer is not JSON: %v\n%s", err, mcpTextOf(res))
		}

		index := func(rows []row) map[string]row {
			m := make(map[string]row, len(rows))
			for _, r := range rows {
				m[r.ID] = r
			}
			return m
		}
		rest, tool = index(restOut.Sources), index(toolOut.Sources)
		for _, id := range []string{k1LongBlockID, k1MidBlockID} {
			if _, ok := rest[id]; !ok {
				t.Fatalf("REST: block %s is not among the %d sources — the probe would pass vacuously", id, len(restOut.Sources))
			}
			if _, ok := tool[id]; !ok {
				t.Fatalf("MCP: block %s is not among the %d sources", id, len(toolOut.Sources))
			}
		}
		return rest, tool, len(restOut.Sources)
	}
	check := func(t *testing.T, surface string, rows map[string]row, id string, wantRunes int, wantTruncated bool) {
		t.Helper()
		r := rows[id]
		if n := len([]rune(r.Content)); n != wantRunes {
			t.Errorf("%s %s: content %d runes, want %d", surface, id, n, wantRunes)
		}
		switch {
		case wantTruncated && (r.Truncated == nil || !*r.Truncated):
			t.Errorf("%s %s: cut at %d runes but carries no truncated:true", surface, id, wantRunes)
		case !wantTruncated && r.Truncated != nil:
			t.Errorf("%s %s: whole content (%d runes) carries truncated:%v", surface, id, wantRunes, *r.Truncated)
		}
	}
	const midRunes = 3000

	// Default window: both blocks are cut at 1500 and flagged.
	rest, tool, _ := answer(t, llm.MaxBlockChars)
	for surface, rows := range map[string]map[string]row{"REST": rest, "MCP": tool} {
		check(t, surface, rows, k1LongBlockID, llm.MaxBlockChars, true)
		check(t, surface, rows, k1MidBlockID, llm.MaxBlockChars, true)
	}

	// Raised window: the long block is cut at it and flagged, the mid block
	// fits and is whole, without the flag.
	rest, tool, n := answer(t, 6000)
	window := llm.SourceRuneCap(6000, n)
	if window <= midRunes {
		t.Fatalf("premise: window %d at %d sources must hold the %d-rune block", window, n, midRunes)
	}
	for surface, rows := range map[string]map[string]row{"REST": rest, "MCP": tool} {
		check(t, surface, rows, k1LongBlockID, window, true)
		check(t, surface, rows, k1MidBlockID, midRunes, false)
	}
}
