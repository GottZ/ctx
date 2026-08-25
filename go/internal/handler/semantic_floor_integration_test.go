//go:build integration

// E-M6 handler gate: query.semantic_floor rejects an off-topic query BEFORE
// the synthesis call, and the refusal it emits is the shape the pre-LLM
// refusal already had.
//
// The unit table in semantic_floor_test.go owns the rule. What only the real
// request path can prove is the part the wave is actually about: that no LLM
// is called. The synthesis backend here counts requests, so the assertion is
// "hits == 0", not "the answer looks like a refusal" — an LLM that happened to
// refuse would produce the same body.
//
// The contrast run with the floor OFF is the permanent red probe: same corpus,
// same query, and the synthesis backend is hit exactly once. Without it a
// broken retrieval would satisfy the green side for the wrong reason.
//
//	go test -tags=integration ./internal/handler/ -run TestSemanticFloor -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sfKeyID = "019f4b03-3333-7000-9000-0000000000aa"
	sfScope = "em6-floor"
	// sfFloor sits between the two fixture similarities (0 and 1) with room on
	// both sides, so neither verdict can be a rounding artefact.
	sfFloor = 0.45
)

// sfFarVector is orthogonal to the vector fakeEmbedServer returns for the
// query (that one is 0 on even components, 2 on odd), so every fixture block
// built from it lands at cosine similarity 0.0 — the "semantic arm returned
// its 75 nearest neighbours and all of them are strangers" case.
func sfFarVector() []float32 {
	e := make([]float32, 1024)
	for i := 0; i < len(e); i += 2 {
		e[i] = 2
	}
	return e
}

// sfNearVector is the query vector itself: cosine similarity 1.0.
func sfNearVector() []float32 {
	e := make([]float32, 1024)
	for i := 1; i < len(e); i += 2 {
		e[i] = 2
	}
	return e
}

// sfSetup seeds five far blocks plus, optionally, one near block. The blocks
// share no vocabulary with the query text below, so the lexical arms
// contribute nothing and the semantic arm is the only reason anything is
// returned at all — exactly the situation the gate is built for.
func sfSetup(t *testing.T, withNear bool) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insert := func(id string, vec []float32) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', 'em6-'||right($1::text, 4), 'em6 fixture content', $2, $3, 'knowledge')`,
			id, sfScope, pgvec.NewVector(vec),
		); err != nil {
			t.Fatalf("insert block %s: %v", id, err)
		}
	}
	for i := 1; i <= 5; i++ {
		insert(fmt.Sprintf("019f4b03-0000-7000-9000-0000000010%02d", i), sfFarVector())
	}
	if withNear {
		insert("019f4b03-0000-7000-9000-000000002001", sfNearVector())
	}

	if _, err := pool.Exec(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('em6') RETURNING id)
		 INSERT INTO context_api_keys (id, key_hash, label, home_scope, allowed_scopes, principal_id)
		 SELECT $1::uuid, 'em6-test-hash', 'em6', $2, $3, p.id FROM p`,
		sfKeyID, sfScope, []string{sfScope},
	); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return pool
}

// sfSynthServer answers the Ollama chat wire and counts how often it was
// asked. The count is the gate.
func sfSynthServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"fixture answer"},"eval_count":7,"prompt_eval_count":3}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// sfPool carries both roles the synthesis path needs: the embed tuple for the
// query vector and the synthesis tuple the gate must not reach.
func sfPool(embedHost, synthHost string) *backends.Pool {
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{
			ID: "e", Name: "test-embed", Host: embedHost, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed},
		},
		{
			ID: "s", Name: "test-synth", Host: synthHost, Protocol: backends.ProtocolOllama,
			Model: "test-synth", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			NumCtx: 8192, Roles: []string{backends.RoleSynthesis},
		},
	})
	return bpool
}

// sfConfigWith is the request-path config with the E-M6 key set. Everything
// else is the registry default, so the run differs from a production request
// in exactly one value.
func sfConfigWith(floor float64) *config.Config {
	c := config.Defaults()
	c.Server.DBPass = "test-password"
	c.Query.Timezone = time.UTC
	c.Query.SemanticFloor = floor
	return c
}

// sfQuery drives one full-synthesis query and returns the decoded response
// plus everything slog wrote during the request.
func sfQuery(t *testing.T, pool *pgxpool.Pool, floor float64, synthHost string) (queryResponse, string) {
	t.Helper()
	embedSrv, _ := fakeEmbedServer(t)
	h := NewQueryHandler(pool, config.NewStore(sfConfigWith(floor)), sfPool(embedSrv.URL, synthHost),
		nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	body := `{"query":"zzqqxx yyppww vvnnmm","limit":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		ApiKeyID:   sfKeyID,
		HomeScope:  sfScope,
		ReadScopes: []string{sfScope},
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
	return resp, logs.String()
}

func TestSemanticFloorRejectsWithoutSynthesis(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	// RED PROBE, permanent and first: with the floor at its 0 default the SAME
	// corpus and the SAME query reach the LLM. A green run below therefore
	// means "the gate stopped it", never "retrieval found nothing".
	t.Run("floor off: the query reaches the LLM", func(t *testing.T) {
		synthSrv, hits := sfSynthServer(t)
		resp, _ := sfQuery(t, sfSetup(t, false), 0, synthSrv.URL)
		if got := hits.Load(); got != 1 {
			t.Fatalf("synthesis backend hits = %d, want 1 — the contrast run must reach the LLM, "+
				"otherwise the gate below proves nothing", got)
		}
		if resp.Answer != "fixture answer" {
			t.Errorf("answer = %q, want the fixture answer", resp.Answer)
		}
		if len(resp.Sources) == 0 {
			t.Error("contrast run returned no sources — the fixture corpus never entered the pipeline")
		}
	})

	t.Run("floor on, everything far: refused with no LLM call", func(t *testing.T) {
		synthSrv, hits := sfSynthServer(t)
		resp, logs := sfQuery(t, sfSetup(t, false), sfFloor, synthSrv.URL)

		if got := hits.Load(); got != 0 {
			t.Errorf("synthesis backend hits = %d, want 0 — the gate must reject BEFORE the LLM call", got)
		}
		if resp.Confidence != llm.ConfidenceNoRelevant {
			t.Errorf("confidence = %q, want %q", resp.Confidence, llm.ConfidenceNoRelevant)
		}
		if resp.Answer != llm.NoRelevantReplacement {
			t.Errorf("answer = %q, want the refusal text %q", resp.Answer, llm.NoRelevantReplacement)
		}
		if !resp.Success {
			t.Error("success = false — a refusal is a normal answer, not an error")
		}
		if len(resp.Sources) != 0 {
			t.Errorf("sources = %d, want 0 (the shape the score-filter refusal already emits)", len(resp.Sources))
		}
		if resp.EvalCount != 0 {
			t.Errorf("eval_count = %d, want 0 — nothing was generated", resp.EvalCount)
		}
		if resp.Model != "test-synth" {
			t.Errorf("model = %q, want the pool's primary synthesis model (the no-LLM path names who WOULD answer)", resp.Model)
		}
		if !strings.Contains(logs, "semantic floor") ||
			!strings.Contains(logs, "best_cos=") ||
			!strings.Contains(logs, fmt.Sprintf("floor=%v", sfFloor)) ||
			!strings.Contains(logs, "lexical=false") {
			t.Errorf("log line missing or incomplete; got:\n%s", logs)
		}
	})

	// The other side of the same floor: one block the query IS about, and the
	// gate steps aside. Without this the "reject" run above is satisfied by a
	// gate that rejects everything.
	t.Run("floor on, one near block: synthesis runs normally", func(t *testing.T) {
		synthSrv, hits := sfSynthServer(t)
		resp, _ := sfQuery(t, sfSetup(t, true), sfFloor, synthSrv.URL)
		if got := hits.Load(); got != 1 {
			t.Fatalf("synthesis backend hits = %d, want 1 — a result above the floor must be synthesized", got)
		}
		if resp.Answer != "fixture answer" {
			t.Errorf("answer = %q, want the fixture answer", resp.Answer)
		}
	})
}
