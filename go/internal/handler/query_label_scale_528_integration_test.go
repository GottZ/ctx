//go:build integration

// Issue #528 through the production request path (POST /api/query,
// HandleQuery with stub backends) on corpora written through
// store.UpsertBlock, store.StoreEmbedding, store.RefreshTemporal and
// dream.WriteLinks: the retrieval-only path and the synthesis label an answer
// with one function on the final score — the one the synthesis has always
// classified — whatever stage behind the fusion lifted it (the graph
// reinforcement, the linear gravity, the cyclic gravity before the linear one,
// the reranker behind the gravity), and the synthesis prompt carries every
// filtered source of such an answer. The retrieval-only label counts the
// sources at or above query.score_threshold only. Every threshold is read off
// a response whose numbers do not depend on the change (a source's score).

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scaleResp reads the fields the label tests need.
type scaleResp struct {
	Confidence string             `json:"confidence"`
	DimWeights map[string]float64 `json:"activated_dim_weights"`
	Sources    []struct {
		ID               string   `json:"id"`
		Score            float64  `json:"score"`
		RRFScoreOriginal *float64 `json:"rrf_score_original"`
		CitationIndex    *int     `json:"citation_index"`
	} `json:"sources"`
}

func (r scaleResp) maxScore() float64 {
	m := 0.0
	for _, s := range r.Sources {
		m = max(m, s.Score)
	}
	return m
}

// scaleEnv is a corpus in the title-floor scope and the stub backends of its
// handlers: the embed stub hands every question the vector on axis 0, the
// chat stub answers chatAnswer, the rerank stub scores the documents in
// reverse order of their place.
type scaleEnv struct {
	pool       *pgxpool.Pool
	ids        []string
	srv        *httptest.Server
	chatAnswer atomic.Value // string
}

func newScaleEnv(t *testing.T, n int, title func(j int) string, content func(j int) string, temporal bool) *scaleEnv {
	t.Helper()
	e := &scaleEnv{pool: testdb.SetupTestDB(t)}
	e.chatAnswer.Store("The report says the build was green [1].")
	ctx := context.Background()
	for j := 0; j < n; j++ {
		b, err := store.UpsertBlock(ctx, e.pool, "learnings", title(j), content(j), nil, nil, titleFloorScope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed %d: %v", j, err)
		}
		if err := store.StoreEmbedding(ctx, e.pool, b.ID, "stub-tf507", titleFloorVec(0.90-0.02*float64(j), j)); err != nil {
			t.Fatalf("seed %d: embedding: %v", j, err)
		}
		if temporal {
			// The dimension rows of the content's dates (the content anchors
			// only, as the temporal ranking test writes them).
			if _, err := store.RefreshTemporal(ctx, e.pool, b.ID, b.Content, time.Time{}); err != nil {
				t.Fatalf("seed %d: temporal: %v", j, err)
			}
		}
		e.ids = append(e.ids, b.ID)
	}
	qvec := make([]float64, 1024)
	qvec[0] = 1
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/embed":
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{qvec}})
		case "/api/chat":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]any{"role": "assistant", "content": e.chatAnswer.Load().(string)},
				"done":    true, "eval_count": 1, "prompt_eval_count": 1,
			})
		case "/v1/rerank":
			var req struct {
				Documents []string `json:"documents"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			res := make([]map[string]any, len(req.Documents))
			for i := range req.Documents {
				res[i] = map[string]any{"index": i, "relevance_score": float64(i + 1)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "stub-rerank", "object": "list", "results": res})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// handler is a query handler on the env's stubs with the default query
// configuration (translation off, UTC) that mod may adjust.
func (e *scaleEnv) handler(t *testing.T, mod func(*config.Config)) *QueryHandler {
	t.Helper()
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "sc528-embed", Host: e.srv.URL, Protocol: backends.ProtocolOllama, Model: "stub-tf507",
			Trust: backends.TrustFull, Enabled: true, Priority: 1, Roles: []string{backends.RoleEmbed}},
		{ID: "s", Name: "sc528-synthesis", Host: e.srv.URL, Protocol: backends.ProtocolOllama, Model: "stub-chat",
			Trust: backends.TrustFull, Enabled: true, Priority: 1, NumCtx: 32768, Roles: []string{backends.RoleSynthesis}},
		{ID: "r", Name: "sc528-rerank", Host: e.srv.URL, Protocol: "rerank", Model: "stub-rerank",
			Trust: backends.TrustFull, Enabled: true, Priority: 1, Roles: []string{backends.RoleRerank}},
	})
	cfg := bw2Config()
	q := config.Defaults().Query
	q.Timezone = time.UTC
	q.Translate = "off"
	cfg.Query = q
	if mod != nil {
		mod(cfg)
	}
	return NewQueryHandler(e.pool, config.NewStore(cfg), bpool, nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
}

func (e *scaleEnv) ask(t *testing.T, h *QueryHandler, q string, synthesize bool) scaleResp {
	t.Helper()
	body := map[string]any{"query": q, "limit": 10}
	if !synthesize {
		body["synthesize"] = false
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ar := &auth.AuthResult{ApiKeyID: "sc528-key", HomeScope: titleFloorScope, ReadScopes: []string{titleFloorScope}, IsValid: true}
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%q: status %d: %s", q, rec.Code, rec.Body.String())
	}
	var out scaleResp
	if err := json.Unmarshal(bytes.TrimSpace(rec.Body.Bytes()), &out); err != nil {
		t.Fatalf("%q: decode: %v", q, err)
	}
	return out
}

// ptrVal prints a score pointer as its value ("<nil>" for none).
func ptrVal(p *float64) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func withThreshold(thr float64, mod func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) {
		if mod != nil {
			mod(c)
		}
		c.Query.ConfidentThreshold = thr
	}
}

func graphOn(c *config.Config) {
	c.Graph = config.Defaults().Graph
	c.Graph.Enabled = true
}

// dayTuesday is a Tuesday more than the point cutoff (14 days) before any day
// the test runs on, so the linear gravity of "am Dienstag" leaves every block
// at factor 1 and the cyclic one alone boosts the Tuesday blocks.
var dayTuesday = time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)

// operationsEnv: twelve operations logs, one per day of August 2026; the one
// nearest the question vector is the log of the day the question names, so
// the linear gravity boosts the list's first.
func operationsEnv(t *testing.T) *scaleEnv {
	t.Helper()
	return newScaleEnv(t, 12,
		func(j int) string { return fmt.Sprintf("Operations log %02d", j) },
		func(j int) string {
			return fmt.Sprintf("Operations log of 2026-08-%02d: rotation, backups, one restart.", 31-j)
		}, false)
}

const operationsQuestion = "Was geschah am 2026-08-31?"


// cited is the number of sources that stood in the synthesis prompt.
func (r scaleResp) cited() int {
	n := 0
	for _, s := range r.Sources {
		if s.CitationIndex != nil {
			n++
		}
	}
	return n
}

// graphEnv: twelve undated notes; the second and the third link to the first
// (dream.WriteLinks, the production write path), so with the graph on the
// list's first is reinforced.
func graphEnv(t *testing.T) *scaleEnv {
	t.Helper()
	e := newScaleEnv(t, 12,
		func(j int) string { return fmt.Sprintf("Cache layer note %02d", j) },
		func(j int) string { return fmt.Sprintf("Cache layer note %02d: eviction, warm-up, sizing.", j) }, false)
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), e.pool)
	for _, src := range e.ids[1:3] {
		n, err := dream.WriteLinks(context.Background(), e.pool, reg.Snapshot(), src, titleFloorScope, 1.0,
			[]dream.Link{{TargetID: e.ids[0], Relationship: "topical", Confidence: 0.95}})
		if err != nil || n != 1 {
			t.Fatalf("dream link from %s: written %d, err %v", src, n, err)
		}
	}
	return e
}

// tuesdayEnv: twelve operations logs of twelve days in a row, the first a
// Tuesday more than the point cutoff (14 days) before any day the test runs
// on: the linear gravity of "am Dienstag" leaves them at factor 1, the cyclic
// one boosts the Tuesday blocks.
func tuesdayEnv(t *testing.T) *scaleEnv {
	t.Helper()
	return newScaleEnv(t, 12,
		func(j int) string { return fmt.Sprintf("Operations log %02d", j) },
		func(j int) string {
			return "Operations log of " + dayTuesday.AddDate(0, 0, j).Format("2006-01-02") + ": rotation, backups, one restart."
		}, true)
}

// One label for one answer, on the final score: at a threshold equal to the
// highest final score of an answer some stage behind the fusion lifted, both
// paths say confident, and the synthesis prompt carries every source — what
// the synthesis gave before issue #528. Read on the fusion's score instead,
// each cell is low_confidence with a prompt of
// query.low_confidence_max_sources sources.
func TestQueryOneLabelOnTheFinalScore(t *testing.T) {
	graph, ops, tue := graphEnv(t), operationsEnv(t), tuesdayEnv(t)
	rerankOn := func(c *config.Config) {
		c.Rerank = config.RerankConfig{Enabled: true, MaxDocs: 50, BlendWeight: 0.5, Timeout: 5 * time.Second}
	}
	for _, c := range []struct {
		name string
		env  *scaleEnv
		q    string
		mod  func(*config.Config)
	}{
		{"graph reinforcement, no date", graph, "How is the cache layer sized?", graphOn},
		{"linear gravity", ops, operationsQuestion, nil},
		{"cyclic then linear gravity", tue, "Was war am Dienstag?", nil},
		{"gravity then reranker", ops, operationsQuestion, rerankOn},
	} {
		t.Run(c.name, func(t *testing.T) {
			ro := c.env.ask(t, c.env.handler(t, c.mod), c.q, false)
			if len(ro.Sources) == 0 {
				t.Fatalf("precondition: no source")
			}
			// Precondition: a stage behind the fusion lifted the top score.
			switch c.env {
			case graph:
				off := c.env.ask(t, c.env.handler(t, nil), c.q, false)
				if !(ro.maxScore() > off.maxScore()) {
					t.Fatalf("precondition: the graph lifted nothing (%v, off %v)", ro.maxScore(), off.maxScore())
				}
			case tue:
				// The linear stage records the cyclic stage's score as the
				// original; the cyclic run is read off the activated weights.
				if ro.DimWeights["weekday"] <= 0 || ro.DimWeights["linear"] <= 0 || ro.Sources[0].ID != tue.ids[0] {
					t.Fatalf("precondition: %q activates %v, first source %s (want the Tuesday block %s)", c.q, ro.DimWeights, ro.Sources[0].ID, tue.ids[0])
				}
			default:
				if o := ro.Sources[0].RRFScoreOriginal; o == nil || !(*o < ro.Sources[0].Score) {
					t.Fatalf("precondition: no stage lifted the first source (score %v, rrf_score_original %v)", ro.Sources[0].Score, ptrVal(o))
				}
			}
			thr := ro.maxScore()
			h := c.env.handler(t, withThreshold(thr, c.mod))
			if got := c.env.ask(t, h, c.q, false).Confidence; got != "confident" {
				t.Errorf("retrieval-only label %q, want confident (highest final score %v = threshold)", got, thr)
			}
			sy := c.env.ask(t, h, c.q, true)
			if sy.Confidence != "confident" {
				t.Errorf("synthesis label %q, want confident (highest final score %v = threshold)", sy.Confidence, thr)
			}
			if n := sy.cited(); n != len(sy.Sources) {
				t.Errorf("%d of %d sources in the synthesis prompt, want all", n, len(sy.Sources))
			}
		})
	}
}

// L2: the retrieval-only label counts the sources at or above
// query.score_threshold only, like the synthesis — a source exactly at the
// score threshold counts, none below it does.
func TestQueryRetrievalOnlyLabelScoreThreshold(t *testing.T) {
	e := operationsEnv(t)
	ref := e.ask(t, e.handler(t, nil), "Operations log rotation", false)
	top := ref.maxScore()
	if top <= 0 {
		t.Fatalf("precondition: no source")
	}
	at := func(scoreThr float64) func(*config.Config) {
		return func(c *config.Config) {
			c.Query.ScoreThreshold = scoreThr
			c.Query.ConfidentThreshold = top / 2
		}
	}
	if got := e.ask(t, e.handler(t, at(top)), "Operations log rotation", false).Confidence; got != "confident" {
		t.Errorf("score threshold at the top score: label %q, want confident (the top source counts)", got)
	}
	if got := e.ask(t, e.handler(t, at(top*1.0001)), "Operations log rotation", false).Confidence; got == "confident" {
		t.Errorf("score threshold above every score: label %q, want no source to count", got)
	}
}
