package oai_test

// The harness of the chat route (W02-07): a fixture alias registry, fake
// upstreams in the three answer shapes the live pool serves (SGLang,
// llama.cpp, OpenRouter), a seeded backend pool, the real dispatcher with the
// real planner (handler.V1Admission — the census recorder can sit between
// them), the real federation ingress and chain filter, a counting tenant
// gate, a fake limiter and a row sink. chatRig.router mounts the route the
// way the /v1 group does behind the door: consumer → access record →
// recovery → PeerIngress → in-flight budget → POST /v1/chat/completions.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// chatAlias is one fixture alias of the chat harness.
type chatAlias struct {
	id, scope, name, kind, role string
	sens                        backends.Sensitivity
	policy                      string
	enabled                     bool
}

const (
	idChat       = "00000000-0000-4000-8000-0000000c4a71"
	idChatTenant = "00000000-0000-4000-8000-0000000c4a72"
	idChatEmbed  = "00000000-0000-4000-8000-0000000c4a73"
	idChatVision = "00000000-0000-4000-8000-0000000c4a74"
	idChatPublic = "00000000-0000-4000-8000-0000000c4a75"
)

// chatAliases is the default fixture: a _global chat alias "chat"
// (credentials, the live default), a public alias "pub", a vision alias, an
// embedding alias and a tenant alias "tchat" of the consumer's tenant.
func chatAliases() []chatAlias {
	return []chatAlias{
		{idChat, backends.GlobalScope, "chat", "chat", "chat", backends.SensCredentials, `{"context_length":262144}`, true},
		{idChatPublic, backends.GlobalScope, "pub", "chat", "chat", backends.SensPublic, `{}`, true},
		{idChatVision, backends.GlobalScope, "vis", "chat", "chat", backends.SensCredentials, `{"vision":true,"vision_max_bytes":1024}`, true},
		{idChatEmbed, backends.GlobalScope, "emb", "embedding", "embed", backends.SensCredentials, `{}`, true},
		{idChatTenant, tenantScope, "tchat", "chat", "chat", backends.SensCredentials, `{}`, true},
	}
}

// chatRegistry loads rows into a registry over pgxmock; reload swaps them
// (the generation moves, as a NOTIFY reload would).
type chatRegistry struct {
	reg  *modelalias.Registry
	mock pgxmock.PgxPoolIface
}

func newChatRegistry(t *testing.T, rows []chatAlias) *chatRegistry {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	cr := &chatRegistry{reg: modelalias.NewRegistry(mock), mock: mock}
	cr.reload(t, rows)
	return cr
}

func (cr *chatRegistry) reload(t testing.TB, rows []chatAlias) {
	t.Helper()
	out := pgxmock.NewRows(aliasColumns)
	for _, r := range rows {
		out.AddRow(r.id, r.scope, r.name, r.kind, r.role, string(r.sens), []byte(r.policy), "", r.enabled, aliasCreated, aliasCreated)
	}
	cr.mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(out)
	if err := cr.reg.Reload(context.Background()); err != nil {
		t.Fatalf("reload chat fixture: %v", err)
	}
}

// The answer shapes of the three engine families the live chat chain serves
// (design/02 §4.6; canaries in every field that is not the model output).
const (
	answerSGLang = `{"id":"ID-CANARY-sglang","object":"chat.completion","created":1700000000,"model":"MODEL-CANARY",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from the engine","reasoning_content":"THINK-CANARY","tool_calls":null},` +
		`"logprobs":null,"finish_reason":"stop","matched_stop":151645}],` +
		`"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3,"prompt_tokens_details":null},"metadata":{"weight_version":"HOST-CANARY"}}`
	answerLlamaCpp = `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello from the engine"}}],` +
		`"created":1700000000,"model":"MODEL-CANARY","system_fingerprint":"b6000-HOST-CANARY","object":"chat.completion",` +
		`"usage":{"completion_tokens":3,"prompt_tokens":11,"total_tokens":14},"id":"chatcmpl-ID-CANARY",` +
		`"timings":{"prompt_n":11,"predicted_n":3,"prompt_ms":1.5}}`
	answerOpenRouter = `{"id":"gen-ID-CANARY","provider":"PROVIDER-CANARY","model":"MODEL-CANARY","object":"chat.completion","created":1700000000,` +
		`"choices":[{"logprobs":null,"finish_reason":"stop","native_finish_reason":"stop","index":0,` +
		`"message":{"role":"assistant","content":"hello from the engine","refusal":null,"reasoning":null}}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"cost":0.00042}}`
)

// upstream is one fake engine: it records every request it gets and answers
// with its shape, or with what respond says.
type upstream struct {
	t       testing.TB
	srv     *httptest.Server
	answer  string
	mu      sync.Mutex
	bodies  []map[string]any
	raws    [][]byte // the bodies as sent (a decoded map hides repeated keys)
	headers []http.Header
	hits    atomic.Int64
	// respond, when set, answers instead of the shape (status, headers).
	respond func(w http.ResponseWriter, r *http.Request)
}

func newUpstream(t testing.TB, answer string) *upstream {
	t.Helper()
	u := &upstream{t: t, answer: answer}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.raws = append(u.raws, raw)
		u.headers = append(u.headers, r.Header.Clone())
		respond := u.respond
		u.mu.Unlock()
		// Headers a careless relay would copy to the client (F-W3 review:
		// Ctx-* never relayed).
		w.Header().Set("Ctx-Instance", "INSTANCE-CANARY")
		w.Header().Set("Ctx-Loop-Token", "LOOP-CANARY")
		w.Header().Set("X-Fake-Header", "HEADER-CANARY")
		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, u.answer)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) setRespond(f func(w http.ResponseWriter, r *http.Request)) {
	u.mu.Lock()
	u.respond = f
	u.mu.Unlock()
}

// lastBody is the body of the last request the engine saw.
func (u *upstream) lastBody(t testing.TB) map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		t.Fatal("the upstream saw no request")
	}
	return u.bodies[len(u.bodies)-1]
}

// row builds a chat backend row on u.
func (u *upstream) row(name string, trust backends.Trust, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: u.srv.URL, Protocol: backends.ProtocolOpenAI, Model: "MODEL-CANARY",
		Roles: []string{"chat"}, Trust: trust, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"chat": 30},
	}
}

// gateCount is a counting tenant gate over a real accountant.
type gateCount struct {
	inner *backends.QuotaAccountant
	calls atomic.Int64
	ids   []string
	mu    sync.Mutex
}

func (g *gateCount) GateTenant(tenantID string, chain []backends.Backend) ([]backends.Backend, error) {
	g.calls.Add(1)
	g.mu.Lock()
	g.ids = append(g.ids, tenantID)
	g.mu.Unlock()
	return g.inner.GateTenant(tenantID, chain)
}

// fakeLimiter admits everything and counts how each reservation ended.
type fakeLimiter struct {
	admits, settles, aborts, rejects atomic.Int64
	refuse                           *oai.LimitRefusal
	lastReason                       atomic.Value
}

type fakeReservation struct{ l *fakeLimiter }

func (r fakeReservation) Settle(dispatch.Usage) { r.l.settles.Add(1) }
func (r fakeReservation) Abort()                { r.l.aborts.Add(1) }
func (r fakeReservation) Reject(reason string, _ time.Duration) {
	r.l.rejects.Add(1)
	r.l.lastReason.Store(reason)
}

func (l *fakeLimiter) Admit(context.Context, *auth.ConsumerAuth, modelalias.Alias, oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	l.admits.Add(1)
	if l.refuse != nil {
		return nil, l.refuse
	}
	return fakeReservation{l}, nil
}

// rowSink collects the llmlog rows of the route.
type rowSink struct {
	mu   sync.Mutex
	rows []llmlog.GatewayRow
}

func (s *rowSink) record(r llmlog.GatewayRow) {
	s.mu.Lock()
	s.rows = append(s.rows, r)
	s.mu.Unlock()
}

func (s *rowSink) all() []llmlog.GatewayRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llmlog.GatewayRow(nil), s.rows...)
}

// chatSettings are the production defaults of the gateway keys the route
// reads (config.GatewayConfig; internal/oai cannot import config).
func chatSettings() oai.Settings {
	return oai.Settings{
		Enabled: true, MaxRequestBytes: 8 << 20, InflightBudget: 1 << 30, MaxResponseBytes: 4 << 20,
		RequestTimeout: 900 * time.Second, RequestDeadline: 540 * time.Second,
		DefaultMaxTokens: 4096, MaxOutputTokensCeiling: 32768,
		TenantAliasCeiling: oai.AliasCeiling{MaxOutputTokens: 8192, MaxN: 1, Timeout: 600 * time.Second,
			EmbeddingMaxInputs: 32, RerankMaxDocuments: 128, ToolsMaxCount: 64},
		RetryAfterJitter: 0.5, BodyLogging: llmlog.BodyLoggingNone,
	}
}

// chatRig is one mounted chat route with all its parts.
type chatRig struct {
	t        testing.TB
	reg      *chatRegistry
	pool     *backends.Pool
	disp     *dispatch.Dispatcher
	recorder *v1census.Recorder
	serveBy  *serveByRecorder
	planner  *handler.V1Admission
	gate     *gateCount
	quota    *backends.QuotaAccountant
	limiter  *fakeLimiter
	rows     *rowSink
	ca       atomic.Pointer[auth.ConsumerAuth]
	settings atomic.Pointer[oai.Settings]
	overlay  atomic.Value    // backends.Sensitivity
	grants   map[string]bool // read under grantMu; setGrant while requests run
	grantMu  sync.Mutex
	grantErr atomic.Pointer[error]
	deps     oai.Deps
	router   chi.Router
	budget   oai.Budget
	recs     chan *oai.AccessRecord
}

// rigOption adjusts a rig before the route is mounted.
type rigOption func(*chatRig)

// withDeps edits the dependencies (a missing planner, limiter, …).
func withDeps(f func(*oai.Deps)) rigOption { return func(r *chatRig) { f(&r.deps) } }

type consumerKey struct{}

// rigIdentity is the harness instance's identity (cmd/ctxd puts the boot
// identity on the server context; the hop stamp of every wire reads it).
var rigIdentity = fedhop.NewIdentity("0123456789abcdef0123456789abcdef", []byte("w02-07-harness-secret-0123456789"))

// rigPrincipal is the dispatcher's principal hook of the harness: the
// consumer the harness put into the context (handler.RequestPrincipal's
// consumer branch, without handler's private context key).
func rigPrincipal(ctx context.Context) dispatch.Principal {
	ca, _ := ctx.Value(consumerKey{}).(*auth.ConsumerAuth)
	if ca == nil {
		return dispatch.Principal{}
	}
	return dispatch.Principal{TenantID: ca.TenantID, ConsumerID: ca.ConsumerID, ConsumerKind: string(ca.Kind), FairWeight: ca.FairWeight}
}

// urgentApp is a valid app consumer of acme on the urgent lane with all
// models, prompt sensitivity public (the floor that leaves the other terms
// visible).
func urgentApp() *auth.ConsumerAuth {
	ca := appConsumer()
	ca.AdmissionLane, ca.AllModels, ca.FairWeight, ca.PromptSensitivity = handler.LaneUrgent, true, 1, string(backends.SensPublic)
	return ca
}

// newChatRig mounts the chat route over rows (backends) and aliases. The
// dispatcher offers the external class on every row's host with s slots,
// r reserved.
func newChatRig(t testing.TB, rows []backends.Backend, slots, reserved int, opts ...rigOption) *chatRig {
	t.Helper()
	dispatch.SetPrincipalHook(rigPrincipal)
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	r := &chatRig{t: t, limiter: &fakeLimiter{}, rows: &rowSink{}, grants: map[string]bool{}, recs: make(chan *oai.AccessRecord, 4096)}
	if tt, ok := t.(*testing.T); ok {
		r.reg = newChatRegistry(tt, chatAliases())
	} else {
		t.Fatal("chatRig needs a *testing.T")
	}
	s := chatSettings()
	r.settings.Store(&s)
	r.overlay.Store(backends.SensPersonal)
	r.ca.Store(urgentApp())
	r.pool = backends.NewPool(nil, nil)
	r.pool.SeedSnapshotForTest(rows)
	r.disp = dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(r.disp.Close)
	pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
	for _, b := range rows {
		pol.Targets[b.Host] = dispatch.TargetPolicy{Slots: slots, InternalReserved: reserved}
	}
	r.disp.UpdatePolicy(pol)
	r.serveBy = &serveByRecorder{V1Dispatch: r.disp}
	r.recorder = v1census.NewRecorder(r.serveBy)
	get := func() oai.Settings { return *r.settings.Load() }
	r.planner = handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: r.recorder, Settings: get, Logger: slog.New(slog.DiscardHandler)})
	r.quota = backends.NewQuotaAccountant(nil, time.Hour)
	r.quota.SeedTenantForTest("seed-none", backends.TenantQuota{}, 0, 0) // warm, no policy for any consumer
	r.gate = &gateCount{inner: r.quota}
	r.deps = oai.Deps{
		Consumer: func(ctx context.Context) *auth.ConsumerAuth {
			ca, _ := ctx.Value(consumerKey{}).(*auth.ConsumerAuth)
			return ca
		},
		KnowledgeAuth: func(context.Context) bool { return false },
		Aliases:       r.reg.reg,
		GrantIDs:      func(context.Context, string) ([]string, error) { return nil, nil },
		HasGrant: func(_ context.Context, _, aliasID string) (bool, error) {
			if e := r.grantErr.Load(); e != nil {
				return false, *e
			}
			r.grantMu.Lock()
			defer r.grantMu.Unlock()
			return r.grants[aliasID], nil
		},
		Settings: get,
		Pool:     r.pool,
		Quota:    r.gate,
		Federation: oai.Federation{
			Floor: func(ctx context.Context) backends.Sensitivity { return federation.PolicyFrom(ctx).Required },
			Scan:  federation.ScanRaise,
			Filter: func(ctx context.Context, chain []backends.Backend, required backends.Sensitivity) ([]backends.Backend, error) {
				pol := federation.PolicyFrom(ctx)
				pol.Required = required
				return federation.FilterChain(chain, fedhop.InboundFrom(ctx), pol, r.pool.LearnedInstances())
			},
		},
		TenantSensitivity: func(context.Context) backends.Sensitivity { return r.overlay.Load().(backends.Sensitivity) },
		Record:            r.rows.record,
		MaxWait:           func() time.Duration { return 20 * time.Second },
		RetryAfterSteps:   r.disp.ExternalRetryAfterSteps,
		Planner:           r.planner,
		Limiter:           r.limiter,
	}
	for _, o := range opts {
		o(r)
	}
	r.router = r.mount(oai.ChatCompletions(r.deps))
	return r
}

// mount builds the /v1 stack around h.
func (r *chatRig) mount(h http.Handler) chi.Router {
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := &oai.AccessRecord{}
				ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), rec)
				if ca := r.ca.Load(); ca != nil {
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
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Logger: slog.New(slog.DiscardHandler)}))
		g.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Method(http.MethodPost, "/chat/completions", h)
	})
	return rt
}

// setGrant sets or withdraws the consumer's grant on aliasID (safe while a
// request runs).
func (r *chatRig) setGrant(aliasID string, granted bool) {
	r.grantMu.Lock()
	r.grants[aliasID] = granted
	r.grantMu.Unlock()
}

// setSettings edits the live settings snapshot (hot keys).
func (r *chatRig) setSettings(f func(*oai.Settings)) {
	s := *r.settings.Load()
	f(&s)
	r.settings.Store(&s)
}

// chatAnswer is one recorded answer.
type chatAnswer struct {
	status int
	header http.Header
	body   []byte
}

func (a chatAnswer) envelope(t testing.TB) (code, param, typ string) {
	t.Helper()
	var e struct {
		Error struct {
			Code, Param *string
			Type        string
		}
	}
	if err := json.Unmarshal(a.body, &e); err != nil {
		t.Fatalf("answer %d is no envelope: %q", a.status, a.body)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return deref(e.Error.Code), deref(e.Error.Param), e.Error.Type
}

// post sends body to the route; hdr are extra request headers.
func (r *chatRig) post(t testing.TB, body string, hdr ...string) chatAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	r.router.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// chatBody is a minimal valid request for model.
func chatBody(model string, extra ...string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"model":%q,"messages":[{"role":"user","content":"hi"}]`, model)
	for _, e := range extra {
		b.WriteString("," + e)
	}
	b.WriteString("}")
	return b.String()
}

// serveByRecorder records the ServeBy of every blocking acquire (lead
// decision W02-07 no. 5: the request deadline is on the context before the
// planner, so every acquire of the real route carries ServeBy ≠ 0).
type serveByRecorder struct {
	handler.V1Dispatch
	mu      sync.Mutex
	serveBy []time.Time
}

func (s *serveByRecorder) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	s.mu.Lock()
	s.serveBy = append(s.serveBy, req.ServeBy)
	s.mu.Unlock()
	return s.V1Dispatch.Acquire(ctx, req)
}

func (s *serveByRecorder) all() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.serveBy...)
}
