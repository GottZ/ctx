package oai_test

// The route gates of POST /v1/rerank and POST /v2/rerank (W02-10, briefing
// G6–G9): the sigmoid round trip over a hop, form and defaults per dialect,
// sub-blocks with sticky rows, and the error envelope pins.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/rerank"
)

// rerankSaturation is the largest logit one hop carries: a probability
// answer is mapped back with probabilityToLogit, which clamps p to
// [1e-7, 1 − 1e-7] (internal/rerank) — ln((1 − 1e-7)/1e-7).
var rerankSaturation = math.Log((1 - 1e-7) / 1e-7)

// TestRerankSigmoidRoundTrip (G6): a second instance (ctx-B, its own rig)
// whose rerank row is this gateway with score_domain probability gets back
// the direct path's logits — the engine's fixture logits — within 1e-6 for
// |logit| < 16: B's rerank.Score maps A's relevance score back to the logit
// (probabilityToLogit), B's answer is its sigmoid again, and the client
// reads the logit off it. A logit of 20 arrives saturated at
// ln((1 − 1e-7)/1e-7) ≈ 16.12 (the documented federation property,
// design/02 §4.9). Every relevance_score lies in [0, 1]. (rerank.Score is
// called by B's route, inside its walk: the X13 census admits no direct
// call in this package.)
func TestRerankSigmoidRoundTrip(t *testing.T) {
	fixture := []float64{-15.5, -9, -3.25, -0.5, 0, 0.75, 2.5, 7, 12, 15.9, 20}
	e := newRerankEngine(t)
	e.logits = map[string]float64{}
	docs := make([]string, len(fixture))
	for i, x := range fixture {
		docs[i] = fmt.Sprintf("fixture %02d", i)
		e.logits[docs[i]] = x
	}
	rigA := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1)
	// A plain rerank row to B: the federation stamps of B (the same harness
	// identity as A) are not passed on.
	gwA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if strings.HasPrefix(k, "Ctx-") {
				r.Header.Del(k)
			}
		}
		rigA.router.ServeHTTP(w, r)
	}))
	t.Cleanup(gwA.Close)
	for _, path := range []string{"/v1/rerank", "/v2/rerank"} {
		for _, res := range decodeRerank(t, rigA.postRerank(t, path, rerankBody("rr", "q", docs))).Results {
			if res.RelevanceScore < 0 || res.RelevanceScore > 1 {
				t.Fatalf("%s: relevance_score %v of document %d outside [0, 1]", path, res.RelevanceScore, res.Index)
			}
		}
	}
	hop := backends.Backend{
		ID: "id-ctx-a", Name: "ctx-a", Host: gwA.URL, Protocol: backends.ProtocolOpenAI, Model: "rr",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"rerank": 30}, Metadata: map[string]any{"score_domain": rerank.DomainProbability},
	}
	rigB := newRerankRig(t, []backends.Backend{hop}, 4, 1)
	r := decodeRerank(t, rigB.postRerank(t, "/v2/rerank", rerankBody("rr", "q", docs)))
	if len(r.Results) != len(fixture) || r.Usage.PromptTokens != 2*len(docs) {
		t.Fatalf("over the hop: %d results, %d prompt tokens — want %d, %d", len(r.Results), r.Usage.PromptTokens, len(fixture), 2*len(docs))
	}
	for _, res := range r.Results {
		x := fixture[res.Index]
		got := math.Log(res.RelevanceScore / (1 - res.RelevanceScore))
		d := math.Abs(got - x)
		switch {
		case math.Abs(x) < 16 && d >= 1e-6:
			t.Errorf("logit %v: over the hop %v, |Δ| = %g, want < 1e-6", x, got, d)
		case math.Abs(x) >= 16 && math.Abs(got-rerankSaturation) >= 1e-6:
			t.Errorf("logit %v: over the hop %v, want the saturation %v", x, got, rerankSaturation)
		}
		t.Logf("W0210 G6 logit %6.2f hop %.9f |Δ| %.3g", x, got, d)
	}
}

// TestRerankFormAndDefaults (G7): top_n cuts; equal scores keep the input
// order; /v1 echoes the documents without return_documents, /v2 does not,
// an explicit return_documents wins on both; {text} objects and strings are
// scored alike; more documents than max_documents and an unknown key are
// 400.
func TestRerankFormAndDefaults(t *testing.T) {
	e := newRerankEngine(t)
	e.logits = map[string]float64{"tie a": 1.5, "tie b": 1.5, "tie c": 1.5, "top": 9, "low": -4}
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1)
	docs := []string{"low", "tie a", "top", "tie b", "tie c"}

	t.Run("sorted, ties in index order, top_n cuts", func(t *testing.T) {
		r := decodeRerank(t, rig.postRerank(t, "/v1/rerank", rerankBody("rr", "q", docs)))
		var idx []int
		for k, res := range r.Results {
			idx = append(idx, res.Index)
			if k > 0 && res.RelevanceScore > r.Results[k-1].RelevanceScore {
				t.Errorf("results not descending at %d", k)
			}
		}
		if want := []int{2, 1, 3, 4, 0}; !slices.Equal(idx, want) {
			t.Fatalf("order %v, want %v (descending, ties in index order)", idx, want)
		}
		if !strings.HasPrefix(r.ID, "rerank-") || r.Model != "rr" || r.Usage.PromptTokens != 10 || r.Usage.TotalTokens != 10 {
			t.Errorf("id %q model %q usage %+v, want rerank-…, rr, 10/10", r.ID, r.Model, r.Usage)
		}
		r = decodeRerank(t, rig.postRerank(t, "/v2/rerank", rerankBody("rr", "q", docs, `"top_n":2`)))
		if len(r.Results) != 2 || r.Results[0].Index != 2 || r.Results[1].Index != 1 {
			t.Fatalf("top_n 2: %+v, want documents 2 and 1", r.Results)
		}
		r = decodeRerank(t, rig.postRerank(t, "/v2/rerank", rerankBody("rr", "q", docs, `"top_n":99`)))
		if len(r.Results) != len(docs) {
			t.Fatalf("top_n past n: %d results, want %d", len(r.Results), len(docs))
		}
	})
	t.Run("return_documents per dialect", func(t *testing.T) {
		for _, tc := range []struct {
			path, extra string
			echo        bool
		}{
			{"/v1/rerank", "", true},
			{"/v2/rerank", "", false},
			{"/v1/rerank", `"return_documents":false`, false},
			{"/v2/rerank", `"return_documents":true`, true},
			{"/v2/rerank", `"return_documents":null`, false},
		} {
			var extra []string
			if tc.extra != "" {
				extra = append(extra, tc.extra)
			}
			r := decodeRerank(t, rig.postRerank(t, tc.path, rerankBody("rr", "q", docs, extra...)))
			for _, res := range r.Results {
				switch {
				case tc.echo && (res.Document == nil || res.Document.Text != docs[res.Index]):
					t.Fatalf("%s %s: document %d echo %+v, want %q", tc.path, tc.extra, res.Index, res.Document, docs[res.Index])
				case !tc.echo && res.Document != nil:
					t.Fatalf("%s %s: document %d echoed, want none", tc.path, tc.extra, res.Index)
				}
			}
		}
	})
	t.Run("objects and strings alike", func(t *testing.T) {
		objs := make([]string, len(docs))
		for i, d := range docs {
			b, _ := json.Marshal(map[string]string{"text": d})
			objs[i] = string(b)
		}
		body := `{"model":"rr","query":"q","documents":[` + strings.Join(objs, ",") + `]}`
		a := decodeRerank(t, rig.postRerank(t, "/v1/rerank", body))
		b := decodeRerank(t, rig.postRerank(t, "/v1/rerank", rerankBody("rr", "q", docs)))
		flat := func(r rerankReply) string {
			var s strings.Builder
			for _, res := range r.Results {
				fmt.Fprintf(&s, "%d:%v:%q;", res.Index, res.RelevanceScore, res.Document.Text)
			}
			return s.String()
		}
		if flat(a) != flat(b) {
			t.Fatalf("objects %s, strings %s — want the same answer", flat(a), flat(b))
		}
	})
	t.Run("refusals", func(t *testing.T) {
		for _, tc := range []struct {
			name, body  string
			code, param string
		}{
			{"past max_documents", rerankBody("rrsmall", "q", rerankDocs(5)), oai.CodeUnsupportedValue, "documents"},
			{"past the ceiling", rerankBody("rr", "q", rerankDocs(1001)), oai.CodeUnsupportedValue, "documents"},
			{"unknown key", rerankBody("rr", "q", docs, `"rank_fields":["x"]`), oai.CodeUnsupportedParameter, "rank_fields"},
			{"unknown document key", `{"model":"rr","query":"q","documents":[{"text":"a","title":"b"}]}`, oai.CodeUnsupportedParameter, "documents[0].title"},
			{"repeated key", `{"model":"rr","query":"q","query":"r","documents":["a"]}`, oai.CodeInvalidJSON, "query"},
			{"no documents", `{"model":"rr","query":"q","documents":[]}`, oai.CodeUnsupportedValue, "documents"},
			{"empty query", `{"model":"rr","query":"","documents":["a"]}`, oai.CodeUnsupportedValue, "query"},
			{"top_n zero", rerankBody("rr", "q", docs, `"top_n":0`), oai.CodeUnsupportedValue, "top_n"},
			{"top_n float", rerankBody("rr", "q", docs, `"top_n":1.5`), oai.CodeInvalidType, "top_n"},
			{"return_documents string", rerankBody("rr", "q", docs, `"return_documents":"yes"`), oai.CodeInvalidType, "return_documents"},
			{"document number", `{"model":"rr","query":"q","documents":[1]}`, oai.CodeInvalidType, "documents[0]"},
			{"document text missing", `{"model":"rr","query":"q","documents":[{}]}`, oai.CodeMissingRequiredParameter, "documents[0].text"},
		} {
			hits := e.hits.Load()
			a := rig.postRerank(t, "/v1/rerank", tc.body)
			code, param, _ := a.envelope(t)
			if a.status != http.StatusBadRequest || code != tc.code || param != tc.param {
				t.Errorf("%s: %d %s, want 400 %s at %s", tc.name, a.status, a.body, tc.code, tc.param)
			}
			if e.hits.Load() != hits {
				t.Errorf("%s: a refusal reached the engine", tc.name)
			}
		}
	})
	t.Run("kind and model", func(t *testing.T) {
		a := rig.postRerank(t, "/v1/rerank", rerankBody("emb", "q", docs))
		if code, _, _ := a.envelope(t); a.status != http.StatusBadRequest || code != oai.CodeModelNotSupported {
			t.Errorf("embedding alias: %d %s, want 400 model_not_supported", a.status, a.body)
		}
		a = rig.postRerank(t, "/v2/rerank", rerankBody("nope", "q", docs))
		if a.status != http.StatusNotFound {
			t.Errorf("unknown alias: %d %s, want 404", a.status, a.body)
		}
	})
}

// TestRerankMaxTokensPerDoc: max_tokens_per_doc cuts every document to its
// 9/5 rune budget upstream (promptguard.RuneBudget); the echo stays the
// document as sent.
func TestRerankMaxTokensPerDoc(t *testing.T) {
	e := newRerankEngine(t)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1)
	long := strings.Repeat("äb", 20) // 40 runes
	r := decodeRerank(t, rig.postRerank(t, "/v1/rerank", rerankBody("rr", "q", []string{long, "short"}, `"max_tokens_per_doc":5`)))
	calls := e.allCalls()
	if len(calls) != 1 || calls[0].Documents[0] != strings.Repeat("äb", 4)+"ä" || calls[0].Documents[1] != "short" {
		t.Fatalf("upstream documents %q, want the 9 runes of the first and the second as sent", calls[0].Documents)
	}
	for _, res := range r.Results {
		if res.Index == 0 && res.Document.Text != long {
			t.Errorf("echo %q, want the document as sent", res.Document.Text)
		}
	}
	if want := []string{"documents", "model", "query"}; !slices.Equal(calls[0].keys, want) {
		t.Errorf("upstream keys %v, want %v (nothing of the client's request but the texts)", calls[0].keys, want)
	}
	// A count whose 9/5 budget overflows an int cuts nothing: 2 049 638 230
	// 412 172 402 × 9 wraps to 2 — without the guard every document would go
	// upstream empty (lead review 1 R-7).
	decodeRerank(t, rig.postRerank(t, "/v1/rerank", rerankBody("rr", "q", []string{long, "short"}, `"max_tokens_per_doc":2049638230412172402`)))
	calls = e.allCalls()
	if last := calls[len(calls)-1]; last.Documents[0] != long || last.Documents[1] != "short" {
		t.Errorf("max_tokens_per_doc past the budget's range: upstream documents %q, want them as sent", last.Documents)
	}
}

// TestRerankSubBlocksSticky (G8): 300 documents at sub_batch 128 with the
// continuation door ⇒ three leases on one row (128, 128, 44), the result
// indices [0, 300) complete; without the door ⇒ 400 naming the bound before
// any acquire; a failure of block two ⇒ 503 upstream_unavailable, no wire
// hit on the second row.
func TestRerankSubBlocksSticky(t *testing.T) {
	t.Run("three leases", func(t *testing.T) {
		e := newRerankEngine(t)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 200, 1)
		_ = rig.recorder.Take()
		r := decodeRerank(t, rig.postRerank(t, "/v2/rerank", rerankBody("rrwide", "q", rerankDocs(300))))
		var weights []int
		conts := 0
		for _, ev := range rig.recorder.Take() {
			if ev.Kind == v1census.KindAcquire {
				weights = append(weights, ev.Weight)
				if ev.Continuation {
					conts++
				}
			}
		}
		if !slices.Equal(weights, []int{128, 128, 44}) || conts != 2 {
			t.Fatalf("lease weights %v (continuations %d), want [128 128 44] (2)", weights, conts)
		}
		seen := make([]bool, 300)
		for _, res := range r.Results {
			seen[res.Index] = true
		}
		if slices.Contains(seen, false) || len(r.Results) != 300 {
			t.Fatalf("%d results, indices not [0, 300) complete", len(r.Results))
		}
		calls := e.allCalls()
		if len(calls) != 3 || len(calls[0].Documents) != 128 || calls[2].Documents[43] != "document 299" {
			t.Fatalf("engine calls %d, want 3 blocks in document order", len(calls))
		}
		rows := rig.rows.all()
		if len(rows) != 1 || rows[0].Telemetry.Metadata["sub_batches"] != 3 || rows[0].Telemetry.Metadata["sub_batches_done"] != 3 ||
			rows[0].Telemetry.PromptTokens != 600 || rows[0].Pipeline != "gateway-rerank" {
			t.Fatalf("row %+v, want one gateway-rerank row, 3/3 blocks, 600 tokens", rows)
		}
	})
	t.Run("without continuation", func(t *testing.T) {
		e := newRerankEngine(t)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 200, 1,
			func(r *chatRig) { r.deps.Planner = noContinuationPlanner{r.planner} })
		_ = rig.recorder.Take()
		a := rig.postRerank(t, "/v1/rerank", rerankBody("rrwide", "q", rerankDocs(300)))
		code, param, _ := a.envelope(t)
		if a.status != http.StatusBadRequest || code != oai.CodeUnsupportedValue || param != "documents" ||
			!strings.Contains(string(a.body), "at most 128 documents per request") {
			t.Fatalf("%d %s, want 400 unsupported_value documents \"at most 128 documents per request\"", a.status, a.body)
		}
		if n := acquires(rig.recorder.Take()); n != 0 || e.hits.Load() != 0 {
			t.Errorf("acquires %d wire hits %d, want 0/0", n, e.hits.Load())
		}
		if l := rig.limiter; l.aborts.Load() != 1 || l.settles.Load() != 0 {
			t.Errorf("limiter aborts %d settles %d, want 1/0", l.aborts.Load(), l.settles.Load())
		}
	})
	t.Run("block two fails", func(t *testing.T) {
		a := newRerankEngine(t)
		b := newRerankEngine(t)
		rig := newRerankRig(t, []backends.Backend{a.row("row-a", 20), b.row("row-b", 10)}, 3, 1)
		a.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, _ rerankCall) bool {
			if n == 2 {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			return false
		})
		ans := rig.postRerank(t, "/v1/rerank", rerankBody("rr", "q", rerankDocs(5)))
		code, _, _ := ans.envelope(t)
		if ans.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable || ans.header.Get("Retry-After") == "" {
			t.Fatalf("%d %s, want 503 upstream_unavailable with Retry-After", ans.status, ans.body)
		}
		if b.hits.Load() != 0 {
			t.Errorf("row B saw %d wire hits after block one was served on row A", b.hits.Load())
		}
		rows := rig.rows.all()
		if len(rows) != 1 || rows[0].Telemetry.Metadata["sub_batches_done"] != 1 || rows[0].Err == nil || rows[0].Err.Code != oai.CodeUpstreamUnavailable {
			t.Fatalf("row %+v, want one error row, one block done", rows)
		}
		if l := rig.limiter; l.settles.Load() != 1 {
			t.Errorf("limiter settles %d, want 1 (block one's usage)", l.settles.Load())
		}
	})
}

// TestRerankEnvelopePins (G9, K5/K29): a panic in the handler ⇒ 503
// internal_error, x-should-retry false; an acquirer sentinel of the planner
// ⇒ 503 internal_error; an empty chain (rerank off: no rerank row) ⇒ 503
// no_eligible_backend without Retry-After.
func TestRerankEnvelopePins(t *testing.T) {
	t.Run("panic", func(t *testing.T) {
		e := newRerankEngine(t)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 3, 1, func(r *chatRig) {
			r.deps.TenantSensitivity = func(context.Context) backends.Sensitivity { panic("rerank gate: panic in the handler") }
		})
		a := rig.postRerank(t, "/v2/rerank", rerankBody("rr", "q", rerankDocs(3)))
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" {
			t.Fatalf("%d %s retry=%q, want 503 internal_error x-should-retry false", a.status, a.body, a.header.Get("x-should-retry"))
		}
	})
	t.Run("planner sentinel", func(t *testing.T) {
		e := newRerankEngine(t)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 3, 1, func(r *chatRig) {
			r.deps.Planner = chFaultPlanner{AdmissionPlanner: r.planner, err: dispatch.ErrClassUnauthorized}
		})
		a := rig.postRerank(t, "/v1/rerank", rerankBody("rr", "q", rerankDocs(3)))
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" {
			t.Fatalf("%d %s retry=%q, want 503 internal_error x-should-retry false", a.status, a.body, a.header.Get("x-should-retry"))
		}
		if e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
			t.Errorf("wire hits %d rows %d, want 0/0", e.hits.Load(), len(rig.rows.all()))
		}
	})
	t.Run("empty chain", func(t *testing.T) {
		up := newUpstream(t, answerSGLang)
		rig := newRerankRig(t, []backends.Backend{up.row("spark-chat", backends.TrustFull, 10)}, 3, 1)
		for _, path := range []string{"/v1/rerank", "/v2/rerank"} {
			a := rig.postRerank(t, path, rerankBody("rr", "q", rerankDocs(3)))
			code, _, _ := a.envelope(t)
			if a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend || a.header.Get("Retry-After") != "" {
				t.Fatalf("%s: %d %s Retry-After %q, want 503 no_eligible_backend without Retry-After", path, a.status, a.body, a.header.Get("Retry-After"))
			}
		}
		if l := rig.limiter; l.admits.Load() != 0 {
			t.Errorf("limiter admits %d, want 0 (refused before the meter)", l.admits.Load())
		}
	})
}
