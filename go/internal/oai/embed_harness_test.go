package oai_test

// The harness of the embeddings route (W02-09): the chat rig (newChatRig —
// registry, pool, real dispatcher behind the census recorder, real planner,
// federation, quota gate, fake limiter, row sink) remounted with POST
// /v1/embeddings, embedding aliases next to the chat fixture, and a fake
// embedding engine on both wires whose vectors are a function of the input
// text — so every answer can be checked against the text it belongs to.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

const (
	idEmbNative = "00000000-0000-4000-8000-0000000e4b01"
	idEmbSmall  = "00000000-0000-4000-8000-0000000e4b02"
	idEmbBig    = "00000000-0000-4000-8000-0000000e4b03"
	idEmbOne    = "00000000-0000-4000-8000-0000000e4b04"
	idEmbTenant = "00000000-0000-4000-8000-0000000e4b05"
	idEmbRunes  = "00000000-0000-4000-8000-0000000e4b06"
)

// embedAliases extends the chat fixture: "emb" (no policy: native dimension
// unknown, defaults 32/32), "emb4k" (native 4096, matryoshka from 256,
// 128 inputs in sub-blocks of 32), "embsmall" (native 8, max 4 inputs,
// sub_batch 2, 16 runes per input), "embbig" (native 65536, 256 inputs in
// blocks of 256 — the reserve gate's ceiling, raised for the test) and
// "emb1" (native 8, max 8 inputs, one input per block), "embrunes" (native
// 8, 2000 runes per input — above a small chain window) and the tenant
// alias "temb" of the consumer's tenant (native 8, max 32 inputs).
func embedAliases() []chatAlias {
	return append(chatAliases(),
		chatAlias{idEmbNative, backends.GlobalScope, "emb4k", "embedding", "embed", backends.SensCredentials,
			`{"embedding.native_dims":4096,"embedding.matryoshka_min":256,"embedding.max_inputs":128,"embedding.sub_batch":32}`, true},
		chatAlias{idEmbSmall, backends.GlobalScope, "embsmall", "embedding", "embed", backends.SensCredentials,
			`{"embedding.native_dims":8,"embedding.max_inputs":4,"embedding.sub_batch":2,"embedding.max_input_runes":16}`, true},
		chatAlias{idEmbBig, backends.GlobalScope, "embbig", "embedding", "embed", backends.SensCredentials,
			`{"embedding.native_dims":65536,"embedding.max_inputs":256,"embedding.sub_batch":256}`, true},
		chatAlias{idEmbOne, backends.GlobalScope, "emb1", "embedding", "embed", backends.SensCredentials,
			`{"embedding.native_dims":8,"embedding.max_inputs":8,"embedding.sub_batch":1}`, true},
		chatAlias{idEmbRunes, backends.GlobalScope, "embrunes", "embedding", "embed", backends.SensCredentials,
			`{"embedding.native_dims":8,"embedding.max_input_runes":2000}`, true},
		chatAlias{idEmbTenant, tenantScope, "temb", "embedding", "embed", backends.SensCredentials,
			`{"embedding.native_dims":8,"embedding.max_inputs":32}`, true},
	)
}

// reloadErr is chatRegistry.reload for a goroutine other than the test's —
// a fake engine's respond callback runs on an httptest handler goroutine,
// where t.Fatal would end the handler, not the test (CodeRabbit, PR #157):
// it returns the reload's error for the caller's t.Error.
func (cr *chatRegistry) reloadErr(rows []chatAlias) error {
	out := pgxmock.NewRows(aliasColumns)
	for _, r := range rows {
		out.AddRow(r.id, r.scope, r.name, r.kind, r.role, string(r.sens), []byte(r.policy), "", r.enabled, aliasCreated, aliasCreated)
	}
	cr.mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(out)
	if err := cr.reg.Reload(context.Background()); err != nil {
		return fmt.Errorf("reload chat fixture: %w", err)
	}
	return nil
}

// embedEngine is one fake embedding engine.
type embedEngine struct {
	t      testing.TB
	srv    *httptest.Server
	dims   int
	mu     sync.Mutex
	calls  [][]string // the inputs of every call, in order
	bodies []string
	hits   atomic.Int64
	// respond, when set, answers call n (1-based) instead of the engine;
	// false = the engine answers.
	respond func(w http.ResponseWriter, r *http.Request, n int64, inputs []string) bool
}

func newEmbedEngine(t testing.TB, dims int) *embedEngine {
	t.Helper()
	e := &embedEngine{t: t, dims: dims}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := e.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(raw, &req)
		var inputs []string
		if json.Unmarshal(req.Input, &inputs) != nil {
			var one string
			_ = json.Unmarshal(req.Input, &one)
			inputs = []string{one}
		}
		e.mu.Lock()
		e.calls = append(e.calls, inputs)
		e.bodies = append(e.bodies, string(raw))
		respond := e.respond
		e.mu.Unlock()
		if respond != nil && respond(w, r, n, inputs) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, e.answer(r.URL.Path, inputs))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *embedEngine) setRespond(f func(w http.ResponseWriter, r *http.Request, n int64, inputs []string) bool) {
	e.mu.Lock()
	e.respond = f
	e.mu.Unlock()
}

func (e *embedEngine) allCalls() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.calls...)
}

// answer is the engine's answer in the wire shape of path; the OpenAI
// shape lists the entries in reverse order (the route must re-align by
// index). Two prompt tokens per input.
func (e *embedEngine) answer(path string, inputs []string) string {
	var b strings.Builder
	if path == "/api/embed" {
		b.WriteString(`{"embeddings":[`)
		for i, in := range inputs {
			if i > 0 {
				b.WriteByte(',')
			}
			writeVec(&b, engineVec(in, e.dims))
		}
		fmt.Fprintf(&b, `],"prompt_eval_count":%d}`, 2*len(inputs))
		return b.String()
	}
	b.WriteString(`{"object":"list","data":[`)
	for k := len(inputs) - 1; k >= 0; k-- {
		if k < len(inputs)-1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"object":"embedding","index":%d,"embedding":`, k)
		writeVec(&b, engineVec(inputs[k], e.dims))
		b.WriteByte('}')
	}
	fmt.Fprintf(&b, `],"model":"ENGINE-CANARY","usage":{"prompt_tokens":%d,"total_tokens":%d}}`, 2*len(inputs), 2*len(inputs))
	return b.String()
}

func writeVec(b *strings.Builder, v []float32) {
	b.WriteByte('[')
	for j, x := range v {
		if j > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
}

// engineVec is the engine's vector of text: dims components from the text's
// hash, not normalized (the raw path normalizes nothing).
func engineVec(text string, dims int) []float32 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, text)
	seed := float64(h.Sum64()%100000) / 1000
	v := make([]float32, dims)
	for j := range v {
		v[j] = float32(0.5 + math.Sin(seed+float64(j)*0.37))
	}
	return v
}

// row builds an embedding backend row on e.
func (e *embedEngine) row(name string, p backends.Protocol, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: e.srv.URL, Protocol: p, Model: "EMBED-MODEL",
		Roles: []string{"embed"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"embed": 30},
	}
}

// newEmbedRig mounts the embeddings route over rows; s slots, r reserved
// per row host (E = s − r).
func newEmbedRig(t *testing.T, rows []backends.Backend, slots, reserved int, opts ...rigOption) *chatRig {
	t.Helper()
	rig := newChatRig(t, rows, slots, reserved, append([]rigOption{func(r *chatRig) {
		r.reg.reload(t, embedAliases())
		r.setSettings(func(s *oai.Settings) { s.EmbeddingMaxFloats = 1 << 20 })
	}}, opts...)...)
	rig.router = rig.mountEmbed(oai.Embeddings(rig.deps))
	return rig
}

// withoutCensus plans over the dispatcher directly: the census Recorder
// serves one request at a time (its rule 7 counts every lease granted since
// the last Take as held by the request that asks), so a test that runs
// requests concurrently must not plan through it.
func withoutCensus(r *chatRig) {
	r.deps.Planner = handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: r.serveBy,
		Settings: func() oai.Settings { return *r.settings.Load() }, Logger: slog.New(slog.DiscardHandler)})
}

// mountEmbed is chatRig.mount for POST /v1/embeddings.
func (r *chatRig) mountEmbed(h http.Handler) chi.Router {
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
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
		g.Use(oai.Recover)
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Binder: acceptPeerBinder{}, Logger: slog.New(slog.DiscardHandler)}))
		g.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Method(http.MethodPost, "/embeddings", h)
	})
	return rt
}

// foreignTenantHeader names a test request of another consumer of another
// tenant (the fairness and contention gates run two at once); the value
// tells the consumers apart.
const foreignTenantHeader = "X-Test-Foreign-Tenant"

// foreignConsumer is ca as a consumer of its own tenant, keyed by tag.
func foreignConsumer(ca *auth.ConsumerAuth, tag string) *auth.ConsumerAuth {
	c := *ca
	h := fnv.New32a()
	_, _ = io.WriteString(h, tag)
	c.ConsumerID = fmt.Sprintf("00000000-0000-4000-8000-0000%08x", h.Sum32())
	c.TenantID = fmt.Sprintf("00000000-0000-4000-9000-0000%08x", h.Sum32())
	c.ModelKeyID = ""
	return &c
}

// postEmbed sends body to the embeddings route.
func (r *chatRig) postEmbed(t testing.TB, body string, hdr ...string) chatAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	r.router.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// embedInputs are n distinct texts.
func embedInputs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("text %03d", i)
	}
	return out
}

// embedBody is a request of model over inputs; extra are further members.
func embedBody(model string, inputs []string, extra ...string) string {
	in, _ := json.Marshal(inputs)
	var b strings.Builder
	fmt.Fprintf(&b, `{"model":%q,"input":%s`, model, in)
	for _, e := range extra {
		b.WriteString("," + e)
	}
	b.WriteString("}")
	return b.String()
}

// embedReply is a decoded embeddings answer.
type embedReply struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string          `json:"object"`
		Index     int             `json:"index"`
		Embedding json.RawMessage `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeEmbed(t testing.TB, a chatAnswer) embedReply {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("answer %d %.400s, want 200", a.status, a.body)
	}
	var r embedReply
	if err := json.Unmarshal(a.body, &r); err != nil {
		t.Fatalf("answer is no JSON: %v (%.300s)", err, a.body)
	}
	return r
}

// vectors decodes data[i].embedding (float list or base64).
func (r embedReply) vectors(t testing.TB) [][]float32 {
	t.Helper()
	out := make([][]float32, len(r.Data))
	for i, d := range r.Data {
		if d.Index != i {
			t.Fatalf("data[%d].index = %d", i, d.Index)
		}
		var v []float32
		if err := json.Unmarshal(d.Embedding, &v); err == nil {
			out[i] = v
			continue
		}
		var s string
		if err := json.Unmarshal(d.Embedding, &s); err != nil {
			t.Fatalf("data[%d].embedding is neither a list nor a string: %.80s", i, d.Embedding)
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw)%4 != 0 {
			t.Fatalf("data[%d].embedding: bad base64 (%v)", i, err)
		}
		v = make([]float32, len(raw)/4)
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*j:]))
		}
		out[i] = v
	}
	return out
}
