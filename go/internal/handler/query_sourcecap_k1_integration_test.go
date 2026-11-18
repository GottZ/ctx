//go:build integration

// Issue #208 at the handler: the include_content snippet of a retrieval-only
// query follows the tenant's query.max_source_runes, read from the request's
// config snapshot. The unit test pins retrievalSnippetRunes; this one pins that
// the handler hands it the snapshot's value and the response's source count.
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pgvec "github.com/pgvector/pgvector-go"

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
