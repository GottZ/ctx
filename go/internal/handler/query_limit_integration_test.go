//go:build integration

// Issue #40 Bug 1 at the pipeline level: ONE non-archived comment block in the
// caller's read scope arms the aggregate over-fetch (query_fold.go
// aggregateOverFetchLimit widens internalLimit 200 -> 400), and the former
// `limit > 200 => 5` reset in rrf.Search turned that widened window into a
// five-row retrieval — for every query in that scope, at any user limit.
//
// Driven through the REAL request path (POST /api/query, retrieval-only, fake
// embed backend), same shape as the W02-3 selector gates in this package. The
// numbers are literals so the test stays executable against a build that
// predates rrf.MaxSearchLimit.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryOverFetch -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ofKeyID = "019f4a02-2222-7000-9000-0000000000aa"
	ofScope = "aw1-overfetch"
	// ofKnowledgeN is well above the five-row default AND above the user limit
	// asked for below, so a truncation can never be mistaken for the collapse.
	ofKnowledgeN = 11
	ofUserLimit  = 10
)

// ofSetup seeds the retrievable corpus plus EXACTLY ONE aggregate-type block:
// the comment is what arms the over-fetch probe, it is not the thing under
// test. It carries no parent (orphan) so the fold keeps it raw and cannot
// change the row count either way.
func ofSetup(t *testing.T, withComment bool) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insert := func(id, typeName string) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', 'aw1-'||right($1::text, 4), 'aw1 content alpha bravo', $2, $3, $4)`,
			id, ofScope, pgvec.NewVector(w023Embedding(0)), typeName,
		); err != nil {
			t.Fatalf("insert block %s: %v", id, err)
		}
	}
	for i := 1; i <= ofKnowledgeN; i++ {
		insert(fmt.Sprintf("019f4a02-0000-7000-9000-0000000010%02d", i), "knowledge")
	}
	if withComment {
		insert("019f4a02-0000-7000-9000-000000002001", "comment")
	}

	if _, err := pool.Exec(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('aw1') RETURNING id)
		 INSERT INTO context_api_keys (id, key_hash, label, home_scope, allowed_scopes, principal_id)
		 SELECT $1::uuid, 'aw1-test-hash', 'aw1', $2, $3, p.id FROM p`,
		ofKeyID, ofScope, []string{ofScope},
	); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return pool
}

// ofQuery runs one retrieval-only query through HandleQuery and returns the
// delivered sources.
func ofQuery(t *testing.T, pool *pgxpool.Pool) []sourceResponse {
	t.Helper()
	srv, _ := fakeEmbedServer(t)
	h := NewQueryHandler(pool, config.NewStore(w023Config(false)), embedPool(srv.URL), nil,
		blocktype.NewRegistry(), snapshotTestAdmitter(t))

	body := fmt.Sprintf(`{"query":"aw1 alpha bravo","synthesize":false,"limit":%d}`, ofUserLimit)
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		ApiKeyID:   ofKeyID,
		HomeScope:  ofScope,
		ReadScopes: []string{ofScope},
		IsValid:    true,
	}))
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp queryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return resp.Sources
}

// TestQueryOverFetchDoesNotCollapseResults is the A-W1 pipeline gate. The
// contrast run without the comment block pins that the corpus itself delivers
// the full user limit — so a short result WITH the comment is the over-fetch
// path collapsing, not a thin corpus.
func TestQueryOverFetchDoesNotCollapseResults(t *testing.T) {
	t.Run("contrast: no aggregate block, no over-fetch", func(t *testing.T) {
		if got := len(ofQuery(t, ofSetup(t, false))); got != ofUserLimit {
			t.Fatalf("without a comment block the query returned %d sources, want %d — corpus too thin, gate inconclusive",
				got, ofUserLimit)
		}
	})

	t.Run("one comment block arms the over-fetch", func(t *testing.T) {
		got := len(ofQuery(t, ofSetup(t, true)))
		if got <= 5 {
			t.Fatalf("with one comment block in scope the query returned %d sources, want %d "+
				"(the widened internal limit collapsed the retrieval to the default)", got, ofUserLimit)
		}
		if got != ofUserLimit {
			t.Errorf("query returned %d sources, want the full user limit %d", got, ofUserLimit)
		}
	})
}
