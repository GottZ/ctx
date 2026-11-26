//go:build integration

// Issue #528, review round 1, through the production request path (POST
// /api/query, HandleQuery with stub backends) on corpora written through
// store.UpsertBlock, store.StoreEmbedding, store.RefreshTemporal and
// dream.WriteLinks: the confidence label is read on the fusion's scale whatever
// stages ran behind the fusion —
//   - the graph reinforcement without a gravity before it (M1);
//   - the cyclic gravity before the linear one (M2);
//   - the gravity before the reranker (L2);
//
// a synthesis rejection of an answer the gravity alone lifted reads that label
// (M3), and the retrieval-only label counts only the sources at or above
// query.score_threshold (L2). Every threshold is read off a response whose
// numbers do not depend on the change (a source's score, or the fusion score
// of a run in which a single stage recorded it), so the tests read the same
// corpus the same way before and after it.

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

// M1: the graph reinforcement keeps the fusion score, so the label of an
// undated answer the graph lifted across the threshold reads the fusion like
// the label of a dated one — and like the label of the same answer with the
// graph off.
func TestQueryLabelReadsTheFusionBehindTheGraph(t *testing.T) {
	e := newScaleEnv(t, 12,
		func(j int) string { return fmt.Sprintf("Cache layer note %02d", j) },
		func(j int) string { return fmt.Sprintf("Cache layer note %02d: eviction, warm-up, sizing.", j) }, false)
	// The second and the third note link to the first (production write
	// path), so the graph reinforces the list's first.
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), e.pool)
	for _, src := range e.ids[1:3] {
		n, err := dream.WriteLinks(context.Background(), e.pool, reg.Snapshot(), src, titleFloorScope, 1.0,
			[]dream.Link{{TargetID: e.ids[0], Relationship: "topical", Confidence: 0.95}})
		if err != nil || n != 1 {
			t.Fatalf("dream link from %s: written %d, err %v", src, n, err)
		}
	}
	const q = "How is the cache layer sized?"
	off := e.ask(t, e.handler(t, nil), q, false)
	on := e.ask(t, e.handler(t, graphOn), q, false)
	fusion := map[string]float64{}
	fusionMax := 0.0
	for _, s := range off.Sources {
		fusion[s.ID] = s.Score
		fusionMax = max(fusionMax, s.Score)
	}
	reinforced := 0
	for _, s := range on.Sources {
		f, ok := fusion[s.ID]
		if !ok || s.Score == f {
			continue
		}
		reinforced++
		if s.RRFScoreOriginal == nil || *s.RRFScoreOriginal != f {
			t.Errorf("source %s reinforced by the graph (%v → %v): rrf_score_original %v, want its fusion score", s.ID, f, s.Score, ptrVal(s.RRFScoreOriginal))
		}
	}
	thr := on.maxScore()
	if reinforced == 0 || !(thr > fusionMax) {
		t.Fatalf("precondition: the graph lifted no score above the list's fusion maximum (%d reinforced, %v vs %v)", reinforced, thr, fusionMax)
	}
	// The threshold at the graph's maximum: the fusion stays below it.
	hOff := e.handler(t, withThreshold(thr, nil))
	hOn := e.handler(t, withThreshold(thr, graphOn))
	for _, c := range []struct {
		what string
		h    *QueryHandler
		syn  bool
	}{{"graph off, retrieval-only", hOff, false}, {"graph on, retrieval-only", hOn, false}, {"graph off, synthesis", hOff, true}, {"graph on, synthesis", hOn, true}} {
		if got := e.ask(t, c.h, q, c.syn).Confidence; got != "low_confidence" {
			t.Errorf("%s: label %q, want low_confidence (fusion maximum %v below the threshold %v)", c.what, got, fusionMax, thr)
		}
	}
}

// dayTuesday is a Tuesday more than the point cutoff (14 days) before any day
// the test runs on, so the linear gravity of "am Dienstag" leaves every block
// at factor 1 and the cyclic one alone boosts the Tuesday blocks.
var dayTuesday = time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)

// M2: a question with a weekday runs the cyclic gravity before the linear one;
// the linear stage kept the cyclic stage's boosted score as the "original",
// so the retrieval-only label read the cyclic boost and the synthesis the
// full one. Both now read the fusion.
func TestQueryLabelCyclicThenLinearGravity(t *testing.T) {
	e := newScaleEnv(t, 12,
		func(j int) string { return fmt.Sprintf("Operations log %02d", j) },
		func(j int) string {
			return "Operations log of " + dayTuesday.AddDate(0, 0, j).Format("2006-01-02") + ": rotation, backups, one restart."
		}, true)
	const q = "Was war am Dienstag?"
	ro := e.ask(t, e.handler(t, nil), q, false)
	if ro.DimWeights["linear"] <= 0 || ro.DimWeights["weekday"] <= 0 {
		t.Fatalf("precondition: %q activates %v, want the linear and the weekday dimension", q, ro.DimWeights)
	}
	if len(ro.Sources) == 0 || ro.Sources[0].ID != e.ids[0] {
		t.Fatalf("precondition: the Tuesday block is not the first source")
	}
	top := ro.Sources[0]
	if top.RRFScoreOriginal == nil || !(*top.RRFScoreOriginal < top.Score) {
		t.Errorf("Tuesday block: rrf_score_original %v, want its fusion score below the boosted %v", ptrVal(top.RRFScoreOriginal), top.Score)
	}
	// The threshold at the boosted maximum: only the cyclic stage boosted it,
	// so a label read on the cyclic stage's output reaches it, one read on the
	// fusion does not.
	h := e.handler(t, withThreshold(ro.maxScore(), nil))
	if got := e.ask(t, h, q, false).Confidence; got != "low_confidence" {
		t.Errorf("retrieval-only label %q, want low_confidence (read on the fusion, below the threshold %v)", got, ro.maxScore())
	}
	if got := e.ask(t, h, q, true).Confidence; got != "low_confidence" {
		t.Errorf("synthesis label %q, want low_confidence (read on the fusion, below the threshold %v)", got, ro.maxScore())
	}
}

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

// M3: the model rejects the sources of an answer the gravity alone lifted
// across the threshold. The label is low_confidence on the fusion's scale, the
// prompt carries query.low_confidence_max_sources sources, and the rejection
// turns the label into no_relevant_blocks_found — the rule every undated
// low_confidence answer already followed (llm.ApplyConfidenceOverride).
func TestQueryRejectionOfAGravityLiftedAnswer(t *testing.T) {
	e := operationsEnv(t)
	ro := e.ask(t, e.handler(t, nil), operationsQuestion, false)
	if len(ro.Sources) == 0 || ro.Sources[0].RRFScoreOriginal == nil || !(*ro.Sources[0].RRFScoreOriginal < ro.Sources[0].Score) {
		t.Fatalf("precondition: the gravity lifted no source")
	}
	h := e.handler(t, withThreshold(ro.maxScore(), nil))
	e.chatAnswer.Store("NO_RELEVANT_SOURCES")
	sy := e.ask(t, h, operationsQuestion, true)
	if sy.Confidence != "no_relevant_blocks_found" {
		t.Errorf("rejected answer: label %q, want no_relevant_blocks_found (low_confidence on the fusion, then rejected)", sy.Confidence)
	}
	if n := sy.cited(); n != config.Defaults().Query.LowConfidenceMaxSources {
		t.Errorf("rejected answer: %d sources in the prompt, want query.low_confidence_max_sources = %d", n, config.Defaults().Query.LowConfidenceMaxSources)
	}
	if got := e.ask(t, h, operationsQuestion, false).Confidence; got != "low_confidence" {
		t.Errorf("retrieval-only label %q, want low_confidence", got)
	}
}

// L2: the reranker runs behind the gravity and keeps the fusion score the
// gravity recorded — not the gravity's boosted score, rounded — so the label
// reads the fusion with the reranker on as with it off.
func TestQueryLabelGravityThenReranker(t *testing.T) {
	e := operationsEnv(t)
	ref := e.ask(t, e.handler(t, nil), operationsQuestion, false)
	fusion := map[string]float64{}
	fusionMax := 0.0
	for _, s := range ref.Sources {
		if s.RRFScoreOriginal == nil {
			t.Fatalf("precondition: source %s carries no fusion score behind the gravity", s.ID)
		}
		fusion[s.ID] = *s.RRFScoreOriginal
		fusionMax = max(fusionMax, *s.RRFScoreOriginal)
	}
	if !(ref.maxScore() > fusionMax) {
		t.Fatalf("precondition: the gravity lifted no score (%v, fusion %v)", ref.maxScore(), fusionMax)
	}
	rerankOn := func(c *config.Config) {
		c.Rerank = config.RerankConfig{Enabled: true, MaxDocs: 50, BlendWeight: 0.5, Timeout: 5 * time.Second}
	}
	rr := e.ask(t, e.handler(t, rerankOn), operationsQuestion, false)
	for _, s := range rr.Sources {
		if f, ok := fusion[s.ID]; ok && (s.RRFScoreOriginal == nil || *s.RRFScoreOriginal != f) {
			t.Errorf("reranked source %s: rrf_score_original %v, want the fusion score %v", s.ID, ptrVal(s.RRFScoreOriginal), f)
		}
	}
	// A threshold between the fusion's maximum and the gravity's: low on both
	// paths, with the reranker as without it.
	thr := (fusionMax + ref.maxScore()) / 2
	h := e.handler(t, withThreshold(thr, rerankOn))
	for _, syn := range []bool{false, true} {
		if got := e.ask(t, h, operationsQuestion, syn).Confidence; got != "low_confidence" {
			t.Errorf("reranker on, synthesize=%v: label %q, want low_confidence (fusion maximum %v below %v)", syn, got, fusionMax, thr)
		}
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
