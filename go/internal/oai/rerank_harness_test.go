package oai_test

// The harness of the rerank routes (W02-10): the chat rig (newChatRig —
// registry, pool, real dispatcher behind the census recorder, real planner,
// federation, quota gate, fake limiter, row sink) remounted with POST
// /v1/rerank (Jina) and POST /v2/rerank (Cohere v2), rerank aliases next to
// the chat fixture, and a fake cross-encoder on the rerank wire (llama.cpp's
// /v1/rerank) whose logits are a function of the query and the document —
// so every answer can be checked against the documents it belongs to.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/oai"
)

const (
	idRerank       = "00000000-0000-4000-8000-0000000f4e01"
	idRerankSmall  = "00000000-0000-4000-8000-0000000f4e02"
	idRerankWide   = "00000000-0000-4000-8000-0000000f4e03"
	idRerankPub    = "00000000-0000-4000-8000-0000000f4e04"
	idRerankTenant = "00000000-0000-4000-8000-0000000f4e05"
)

// rerankAliases extends the chat fixture: "rr" (no policy: max_documents and
// sub_batch at their defaults 128/128, credentials), "rrsmall" (4 documents
// in blocks of 2), "rrwide" (1000 documents in blocks of 128 — the code
// ceiling of max_documents), "rrpub" (public: the scan is booked) and the
// tenant alias "trr" of the consumer's tenant (clamped to the tenant
// ceiling, 128).
func rerankAliases() []chatAlias {
	return append(chatAliases(),
		chatAlias{idRerank, backends.GlobalScope, "rr", "rerank", "rerank", backends.SensCredentials, `{}`, true},
		chatAlias{idRerankSmall, backends.GlobalScope, "rrsmall", "rerank", "rerank", backends.SensCredentials,
			`{"rerank.max_documents":4,"rerank.sub_batch":2}`, true},
		chatAlias{idRerankWide, backends.GlobalScope, "rrwide", "rerank", "rerank", backends.SensCredentials,
			`{"rerank.max_documents":1000,"rerank.sub_batch":128}`, true},
		chatAlias{idRerankPub, backends.GlobalScope, "rrpub", "rerank", "rerank", backends.SensPublic,
			`{"rerank.max_documents":1000,"rerank.sub_batch":1000}`, true},
		chatAlias{idRerankTenant, tenantScope, "trr", "rerank", "rerank", backends.SensCredentials,
			`{"rerank.max_documents":1000}`, true},
	)
}

// rerankEngine is one fake cross-encoder.
type rerankEngine struct {
	t     testing.TB
	srv   *httptest.Server
	mu    sync.Mutex
	calls []rerankCall // every call, in order
	hits  atomic.Int64
	// logits, when set, overrides the logit of a document text.
	logits map[string]float64
	// probability answers relevance scores as probabilities (sigmoid of the
	// logit) under "results" — a backend of score_domain probability.
	probability bool
	// respond, when set, answers call n (1-based) instead of the engine;
	// false = the engine answers.
	respond func(w http.ResponseWriter, r *http.Request, n int64, call rerankCall) bool
}

// rerankCall is one wire call the engine saw.
type rerankCall struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	raw       []byte
	keys      []string
}

func newRerankEngine(t testing.TB) *rerankEngine {
	t.Helper()
	e := &rerankEngine{t: t}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := e.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var call rerankCall
		_ = json.Unmarshal(raw, &call)
		call.raw = raw
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		for k := range m {
			call.keys = append(call.keys, k)
		}
		slices.Sort(call.keys)
		e.mu.Lock()
		e.calls = append(e.calls, call)
		respond := e.respond
		e.mu.Unlock()
		if r.URL.Path != "/v1/rerank" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if respond != nil && respond(w, r, n, call) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, e.answer(call))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *rerankEngine) setRespond(f func(w http.ResponseWriter, r *http.Request, n int64, call rerankCall) bool) {
	e.mu.Lock()
	e.respond = f
	e.mu.Unlock()
}

func (e *rerankEngine) allCalls() []rerankCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]rerankCall(nil), e.calls...)
}

// logit is the engine's logit of doc for query: the override, else a value
// in [-8, 8) from the texts' hash.
func (e *rerankEngine) logit(query, doc string) float64 {
	if x, ok := e.logits[doc]; ok {
		return x
	}
	return engineLogit(query, doc)
}

func engineLogit(query, doc string) float64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, query)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, doc)
	return float64(h.Sum64()%16000)/1000 - 8
}

// answer is llama.cpp's rerank answer: one result per document, sorted by
// score descending (the route re-aligns by index), two prompt tokens per
// document.
func (e *rerankEngine) answer(call rerankCall) string {
	type res struct {
		i int
		x float64
	}
	rs := make([]res, len(call.Documents))
	for i, d := range call.Documents {
		rs[i] = res{i, e.logit(call.Query, d)}
	}
	slices.SortStableFunc(rs, func(a, b res) int {
		switch {
		case a.x > b.x:
			return -1
		case a.x < b.x:
			return 1
		}
		return 0
	})
	var b strings.Builder
	fmt.Fprintf(&b, `{"model":"ENGINE-CANARY","object":"list","usage":{"prompt_tokens":%d,"total_tokens":%d},"results":[`,
		2*len(call.Documents), 2*len(call.Documents))
	for k, r := range rs {
		if k > 0 {
			b.WriteByte(',')
		}
		x := r.x
		if e.probability {
			x = 1 / (1 + math.Exp(-x))
		}
		fmt.Fprintf(&b, `{"index":%d,"relevance_score":%s}`, r.i, strconv.FormatFloat(x, 'g', -1, 64))
	}
	b.WriteString("]}")
	return b.String()
}

// row builds a rerank backend row on e.
func (e *rerankEngine) row(name string, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: e.srv.URL, Protocol: backends.ProtocolOpenAI, Model: "RERANK-MODEL",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"rerank": 30},
	}
}

// newRerankRig mounts both rerank routes over rows; s slots, r reserved per
// row host (E = s − r).
func newRerankRig(t *testing.T, rows []backends.Backend, slots, reserved int, opts ...rigOption) *chatRig {
	t.Helper()
	rig := newChatRig(t, rows, slots, reserved, append([]rigOption{func(r *chatRig) {
		r.reg.reload(t, rerankAliases())
	}}, opts...)...)
	rig.router = rig.mountRerank(oai.Rerank(rig.deps, oai.RerankJina), oai.Rerank(rig.deps, oai.RerankCohere))
	return rig
}

// mountRerank is chatRig.mount for POST /v1/rerank and POST /v2/rerank — two
// groups with the same stack (one budget), as cmd/ctxd mounts them.
func (r *chatRig) mountRerank(v1, v2 http.Handler) chi.Router {
	rt := chi.NewRouter()
	for _, g := range []struct {
		prefix string
		h      http.Handler
	}{{"/v1", v1}, {"/v2", v2}} {
		rt.Route(g.prefix, func(gr chi.Router) {
			gr.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					rec := &oai.AccessRecord{}
					ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), rec)
					if ca := r.ca.Load(); ca != nil {
						if other := req.Header.Get(foreignTenantHeader); other != "" {
							ca = foreignConsumer(ca, other)
						}
						ctx = context.WithValue(ctx, consumerKey{}, ca)
					}
					next.ServeHTTP(w, req.WithContext(ctx))
					select {
					case r.recs <- rec:
					default:
					}
				})
			})
			gr.Use(oai.Recover)
			gr.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Logger: slog.New(slog.DiscardHandler)}))
			gr.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
			gr.Method(http.MethodPost, "/rerank", g.h)
		})
	}
	return rt
}

// postRerank sends body to path (/v1/rerank or /v2/rerank).
func (r *chatRig) postRerank(t testing.TB, path, body string, hdr ...string) chatAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	r.router.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// rerankDocs are n distinct documents.
func rerankDocs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("document %03d", i)
	}
	return out
}

// rerankBody is a request of model for query over docs; extra are further
// members.
func rerankBody(model, query string, docs []string, extra ...string) string {
	d, _ := json.Marshal(docs)
	q, _ := json.Marshal(query)
	var b strings.Builder
	fmt.Fprintf(&b, `{"model":%q,"query":%s,"documents":%s`, model, q, d)
	for _, e := range extra {
		b.WriteString("," + e)
	}
	b.WriteString("}")
	return b.String()
}

// rerankReply is a decoded rerank answer.
type rerankReply struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
		Document       *struct {
			Text string `json:"text"`
		} `json:"document"`
	} `json:"results"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeRerank(t testing.TB, a chatAnswer) rerankReply {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("answer %d %.400s, want 200", a.status, a.body)
	}
	var r rerankReply
	dec := json.NewDecoder(strings.NewReader(string(a.body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("answer is no rerank JSON: %v (%.300s)", err, a.body)
	}
	return r
}
