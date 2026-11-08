package oai_test

// W06-12 refusal-ledger census: every exit of the three inference routes
// (chat, embeddings, rerank) and of GET /v1/models/{name} fires one request
// and pins, on the request's access record, (a) the answer's status and
// code and (b) the declaration the access-log stage would count —
// rec.Rejection() names exactly the expected (reason, detail), or nothing
// for the exits that are no refusal by a rule (parse 400, internal_error,
// service_unavailable, a missing planner, an upstream fault, the verdict
// ends, success). The census mounts each route behind the production stage
// order of the model groups (cmd/ctxd modelGateway.group): access record →
// recovery → PeerIngress → strict body cap → in-flight budget → route, so
// the stage refusals (413 of a declared body, 503 of the budget) are
// counted where production counts them.
//
// Out of the rig: the meter's own refusal (rate/concurrency/warming) is
// declared by the cmd/ctxd limiter adapter (gateway.go, Admit), not by the
// route — with the harness' fakeLimiter the route must leave the record
// empty (first declaration wins: a route declaration in front of the
// adapter would hide the meter's reason). The vain queue wait of an L2
// refusal is set by the adapter's Reject as well; the fakeLimiter sets
// none, so the L2 cases pin reason and detail only.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// censusEngine is one fake engine of a route: its row builder and a hook
// that answers instead of the engine's shape when f returns true.
type censusEngine struct {
	row     func(name string) backends.Backend
	respond func(f func(w http.ResponseWriter, r *http.Request) bool)
}

// censusRoute is one inference route of the census.
type censusRoute struct {
	op      string
	path    string // the route's path below /v1
	model   string // the served alias
	other   string // an alias of another kind
	tenant  string // the consumer's tenant alias (role not exported)
	aliases func() []chatAlias
	slots   int
	newRig  func(t *testing.T, rows []backends.Backend, slots, reserved int, opts ...rigOption) *chatRig
	handler func(oai.Deps) http.Handler
	engine  func(t *testing.T) censusEngine
	body    func(model string) string
	missing string // a body without a required member of the route
}

func censusRoutes() []censusRoute {
	return []censusRoute{
		{
			op: llmlog.GatewayOpChat, path: "/chat/completions", model: "chat", other: "emb", tenant: "tchat",
			aliases: chatAliases, slots: 4,
			newRig: func(t *testing.T, rows []backends.Backend, slots, reserved int, opts ...rigOption) *chatRig {
				return newChatRig(t, rows, slots, reserved, opts...)
			},
			handler: oai.ChatCompletions,
			engine: func(t *testing.T) censusEngine {
				u := newUpstream(t, answerSGLang)
				return censusEngine{
					row: func(name string) backends.Backend { return u.row(name, backends.TrustFull, 10) },
					respond: func(f func(http.ResponseWriter, *http.Request) bool) {
						u.setRespond(func(w http.ResponseWriter, r *http.Request) {
							if f(w, r) {
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(answerSGLang))
						})
					},
				}
			},
			body:    func(model string) string { return chatBody(model) },
			missing: `{"model":"chat"}`,
		},
		{
			op: llmlog.GatewayOpEmbed, path: "/embeddings", model: "emb", other: "chat", tenant: "temb",
			aliases: embedAliases, slots: 2,
			newRig:  newEmbedRig,
			handler: oai.Embeddings,
			engine: func(t *testing.T) censusEngine {
				e := newEmbedEngine(t, 8)
				return censusEngine{
					row: func(name string) backends.Backend { return e.row(name, backends.ProtocolOpenAI, 10) },
					respond: func(f func(http.ResponseWriter, *http.Request) bool) {
						e.setRespond(func(w http.ResponseWriter, r *http.Request, _ int64, _ []string) bool { return f(w, r) })
					},
				}
			},
			body:    func(model string) string { return embedBody(model, embedInputs(3)) },
			missing: `{"model":"emb"}`,
		},
		{
			op: llmlog.GatewayOpRerank, path: "/rerank", model: "rr", other: "chat", tenant: "trr",
			aliases: rerankAliases, slots: 3,
			newRig:  newRerankRig,
			handler: func(d oai.Deps) http.Handler { return oai.Rerank(d, oai.RerankJina) },
			engine: func(t *testing.T) censusEngine {
				e := newRerankEngine(t)
				return censusEngine{
					row: func(name string) backends.Backend { return e.row(name, 10) },
					respond: func(f func(http.ResponseWriter, *http.Request) bool) {
						e.setRespond(func(w http.ResponseWriter, r *http.Request, _ int64, _ rerankCall) bool { return f(w, r) })
					},
				}
			},
			body:    func(model string) string { return rerankBody(model, "q", rerankDocs(3)) },
			missing: `{"model":"rr","query":"q"}`,
		},
	}
}

// censusMount mounts h at /v1<path> behind the production stage order of
// a model group; every request's access record goes to r.recs.
func censusMount(r *chatRig, path string, h http.Handler) http.Handler {
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
		g.Use(federation.PeerIngress(federation.PeerIngressConfig{Consumer: r.deps.Consumer, Binder: acceptPeerBinder{}, Logger: slog.New(slog.DiscardHandler)}))
		g.Use(handler.MaxBodySizeStrictWith(func() int64 { return r.settings.Load().MaxRequestBytes }, oai.RefuseRequestTooLarge))
		g.Use(oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Method(http.MethodPost, path, h)
	})
	return rt
}

// censusPost sends body to path; undeclared sends it without a
// Content-Length (a chunked body).
func censusPost(h http.Handler, path, body string, undeclared bool) chatAnswer {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if undeclared {
		req.ContentLength = -1
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Result().Header, body: w.Body.Bytes()}
}

// censusRecord is the one access record the request left.
func censusRecord(t *testing.T, r *chatRig) *oai.AccessRecord {
	t.Helper()
	var recs []*oai.AccessRecord
drain:
	for {
		select {
		case rec := <-r.recs:
			recs = append(recs, rec)
		default:
			break drain
		}
	}
	if len(recs) != 1 {
		t.Fatalf("access records %d, want 1", len(recs))
	}
	return recs[0]
}

// censusCase is one exit of a route.
type censusCase struct {
	name string
	// rows replaces the default chain (one row "spark" on the engine).
	rows func(eng censusEngine) []backends.Backend
	// prep edits the rig (deps, settings, quota, limiter, dispatcher) before
	// the route is mounted.
	prep       func(t *testing.T, rt censusRoute, r *chatRig, eng censusEngine)
	body       func(rt censusRoute) string // nil: rt.body(rt.model)
	undeclared bool
	status     int
	code       string
	// reason and detail: the expected declaration; reason "" ⇒ none.
	reason, detail string
	// stage: the refusal is a stage's, before the route set rec.Op.
	stage bool
	// check runs extra assertions after the request.
	check func(t *testing.T, r *chatRig)
}

func censusCases() []censusCase {
	var writerCalls atomic.Int64 // the writer-window case's writer calls
	zero := 0
	zeroUSD := 0.0
	l2 := func(err error) func(t *testing.T, rt censusRoute, r *chatRig, eng censusEngine) {
		return func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
			r.deps.Planner = chFaultPlanner{AdmissionPlanner: r.planner, err: err}
		}
	}
	gateReached := func(t *testing.T, r *chatRig) {
		t.Helper()
		if n := r.gate.calls.Load(); n != 1 {
			t.Errorf("tenant gate calls %d, want 1 (the refusal must come from the gate)", n)
		}
	}
	return []censusCase{
		// --- ledger exits ---
		{name: "resolve 404 unknown model", body: func(censusRoute) string { return "" },
			status: http.StatusNotFound, code: oai.CodeModelNotFound, reason: keyquota.RejectModel, detail: keyquota.DetailNotFound},
		{name: "resolve 404 not granted",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				ca := urgentApp()
				ca.AllModels = false
				r.ca.Store(ca)
			},
			status: http.StatusNotFound, code: oai.CodeModelNotFound, reason: keyquota.RejectModel, detail: keyquota.DetailNotFound},
		{name: "resolve 404 overlong name", body: func(rt censusRoute) string { return rt.body(strings.Repeat("x", 4096)) },
			status: http.StatusNotFound, code: oai.CodeModelNotFound, reason: keyquota.RejectModel, detail: keyquota.DetailNotFound},
		{name: "kind 400 model_not_supported", body: func(rt censusRoute) string { return rt.body(rt.other) },
			status: http.StatusBadRequest, code: oai.CodeModelNotSupported, reason: keyquota.RejectModel, detail: keyquota.DetailNotSupported},
		{name: "body cap stage 413 (declared length)",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.setSettings(func(s *oai.Settings) { s.MaxRequestBytes = 16 })
			},
			status: http.StatusRequestEntityTooLarge, code: oai.CodeRequestTooLarge, stage: true,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailRequestTooLarge},
		{name: "body cap read 413 (undeclared length)", undeclared: true,
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.setSettings(func(s *oai.Settings) { s.MaxRequestBytes = 16 })
			},
			status: http.StatusRequestEntityTooLarge, code: oai.CodeRequestTooLarge,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailRequestTooLarge},
		// RC-D C8: the scan cap (gateway.scan_max_request_bytes) refuses a
		// body to an alias below credentials past it, in the route, before
		// the scan — a route declaration, not the stage's.
		{name: "scan cap 413 (alias below credentials)",
			prep: func(t *testing.T, rt censusRoute, r *chatRig, _ censusEngine) {
				if rt.op == llmlog.GatewayOpEmbed {
					r.reg.reload(t, embedScanAliases())
				}
				r.setSettings(func(s *oai.Settings) { s.ScanMaxRequestBytes = 16 })
			},
			body:   func(rt censusRoute) string { return rt.body(censusScanAliases[rt.op]) },
			status: http.StatusRequestEntityTooLarge, code: oai.CodeRequestTooLarge,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailRequestTooLarge},
		{name: "in-flight budget stage 503",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.setSettings(func(s *oai.Settings) { s.InflightBudget = 30 })
			},
			status: http.StatusServiceUnavailable, code: oai.CodeServerOverloaded, stage: true,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailInflightBudget},
		{name: "in-flight budget route 503",
			prep: func(_ *testing.T, rt censusRoute, r *chatRig, _ censusEngine) {
				// The stage's request share fits exactly; the route's next
				// share (nesting state, scan, answer) does not.
				n := int64(3 * len(rt.body(rt.model)))
				r.setSettings(func(s *oai.Settings) { s.InflightBudget = n })
			},
			status: http.StatusServiceUnavailable, code: oai.CodeServerOverloaded,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailInflightBudget},
		{name: "chain build 503 egress_denied", body: func(rt censusRoute) string { return rt.body(rt.tenant) },
			status: http.StatusServiceUnavailable, code: oai.CodeNoEligibleBackend,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailEgressDenied,
			check: func(t *testing.T, r *chatRig) {
				t.Helper()
				if n := r.gate.calls.Load(); n != 0 {
					t.Errorf("tenant gate calls %d, want 0 (refused by the chain build)", n)
				}
			}},
		{name: "tenant gate empty 503 egress_denied",
			rows: func(eng censusEngine) []backends.Backend {
				row := eng.row("external")
				row.Locality = backends.LocalityExternal
				return []backends.Backend{row}
			},
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.quota.SeedTenantForTest(r.ca.Load().TenantID,
					backends.TenantQuota{Enabled: true, DailyCostUSD: &zeroUSD, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)
			},
			status: http.StatusServiceUnavailable, code: oai.CodeNoEligibleBackend,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailEgressDenied, check: gateReached},
		{name: "tenant quota 429 daily calls",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.quota.SeedTenantForTest(r.ca.Load().TenantID, backends.TenantQuota{Enabled: true, DailyCalls: &zero}, 0, 0)
			},
			status: http.StatusTooManyRequests, code: oai.CodeInsufficientQuota,
			reason: keyquota.RejectQuota, detail: keyquota.DetailTenantCalls, check: gateReached},
		{name: "tenant quota 429 cost budget",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.quota.SeedTenantForTest(r.ca.Load().TenantID,
					backends.TenantQuota{Enabled: true, DailyCostUSD: &zeroUSD, OnExceed: backends.QuotaExceedBlock}, 0, 0)
			},
			status: http.StatusTooManyRequests, code: oai.CodeInsufficientQuota,
			reason: keyquota.RejectQuota, detail: keyquota.DetailTenantCost, check: gateReached},
		{name: "walk without a row 503 not_offered",
			rows: func(eng censusEngine) []backends.Backend {
				row := eng.row("nomodel")
				row.Model = ""
				return []backends.Backend{row}
			},
			status: http.StatusServiceUnavailable, code: oai.CodeNoEligibleBackend,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailNotOffered},
		{name: "L2 429 server_busy (rejection)",
			prep:   l2(&dispatch.RejectionError{Err: dispatch.ErrQueueFull, RetryAfter: time.Second}),
			status: http.StatusTooManyRequests, code: oai.CodeServerBusy,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailServerBusy},
		{name: "L2 429 server_busy (would block)",
			prep:   l2(dispatch.ErrWouldBlock),
			status: http.StatusTooManyRequests, code: oai.CodeServerBusy,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailServerBusy},
		{name: "L2 429 wait_expired",
			prep:   l2(&dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: 2 * time.Second}),
			status: http.StatusTooManyRequests, code: oai.CodeServerBusy,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailWaitExpired},
		{name: "L2 503 model_unavailable (plan not offered)",
			prep:   func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) { notOffered(r) },
			status: http.StatusServiceUnavailable, code: oai.CodeModelUnavailable,
			reason: keyquota.RejectCapacity, detail: keyquota.DetailNotOffered},

		// --- no declaration ---
		{name: "parse 400 invalid_json", body: func(censusRoute) string { return `{"model":` },
			status: http.StatusBadRequest, code: oai.CodeInvalidJSON},
		{name: "parse 400 missing member", body: func(rt censusRoute) string { return rt.missing },
			status: http.StatusBadRequest, code: oai.CodeMissingRequiredParameter},
		{name: "503 internal_error (consumer without tenant)",
			prep:   func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) { r.ca.Store(chNoTenant()) },
			status: http.StatusServiceUnavailable, code: oai.CodeInternalError},
		{name: "503 internal_error (admission sentinel)",
			prep:   l2(dispatch.ErrClassUnauthorized),
			status: http.StatusServiceUnavailable, code: oai.CodeInternalError},
		{name: "503 service_unavailable (no limiter)",
			prep:   func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) { r.deps.Limiter = nil },
			status: http.StatusServiceUnavailable, code: oai.CodeServiceUnavailable},
		{name: "503 no_eligible_backend (no planner)",
			prep:   func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) { r.deps.Planner = nil },
			status: http.StatusServiceUnavailable, code: oai.CodeNoEligibleBackend},
		{name: "429 meter refusal (declared by the cmd/ctxd adapter, not the route)",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				r.limiter.refuse = &oai.LimitRefusal{Code: oai.CodeRateLimitExceeded, Message: "Rate limit reached.", RetryAfter: time.Second, ShouldRetry: true}
			},
			status: http.StatusTooManyRequests, code: oai.CodeRateLimitExceeded},
		{name: "upstream 500",
			prep: func(_ *testing.T, _ censusRoute, _ *chatRig, eng censusEngine) {
				eng.respond(func(w http.ResponseWriter, _ *http.Request) bool {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
					return true
				})
			},
			status: http.StatusInternalServerError, code: oai.CodeUpstreamError},
		{name: "shutdown at entry",
			prep: func(_ *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				life, cancel := context.WithCancel(context.Background())
				cancel()
				r.deps.Lifecycle = life
			},
			status: http.StatusServiceUnavailable, code: oai.CodeInternalError},
		{name: "shutdown in the admission writer window",
			prep: func(t *testing.T, _ censusRoute, r *chatRig, _ censusEngine) {
				life, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				notOffered(r)
				r.deps.Lifecycle = life
				writerCalls.Store(0)
				r.deps.Planner = shWindowPlanner{AdmissionPlanner: r.planner, stop: cancel, calls: &writerCalls}
			},
			status: http.StatusServiceUnavailable, code: oai.CodeInternalError,
			check: func(t *testing.T, _ *chatRig) {
				t.Helper()
				if n := writerCalls.Load(); n != 1 {
					t.Errorf("admission writer calls %d, want 1 (the shutdown must land in the writer window)", n)
				}
			}},
		{name: "watcher 404 (alias disabled during the wire call)",
			prep: func(t *testing.T, rt censusRoute, r *chatRig, eng censusEngine) {
				t.Cleanup(oai.SetWatchEvery(20 * time.Millisecond))
				var once sync.Once
				eng.respond(func(_ http.ResponseWriter, req *http.Request) bool {
					first := false
					once.Do(func() { first = true })
					if !first {
						return false
					}
					rows := rt.aliases()
					for i := range rows {
						if rows[i].name == rt.model {
							rows[i].enabled = false
						}
					}
					if err := r.reg.reloadErr(rows); err != nil {
						t.Error(err)
					}
					<-req.Context().Done()
					return true
				})
			},
			status: http.StatusNotFound, code: oai.CodeModelNotFound},
		{name: "200 served", status: http.StatusOK},
	}
}

// censusScanAliases is each route's alias below credentials (the embeddings
// route's after the scan-cap case reloads embedScanAliases).
var censusScanAliases = map[string]string{llmlog.GatewayOpChat: "pub", llmlog.GatewayOpEmbed: "embpub", llmlog.GatewayOpRerank: "rrpub"}

// runCensus runs every case against rt.
func runCensus(t *testing.T, rt censusRoute) {
	for _, tc := range censusCases() {
		t.Run(tc.name, func(t *testing.T) {
			eng := rt.engine(t)
			rows := []backends.Backend{eng.row("spark")}
			if tc.rows != nil {
				rows = tc.rows(eng)
			}
			r := rt.newRig(t, rows, rt.slots, 1)
			if tc.prep != nil {
				tc.prep(t, rt, r, eng)
			}
			h := censusMount(r, rt.path, rt.handler(r.deps))
			body := rt.body(rt.model)
			if tc.body != nil {
				if body = tc.body(rt); body == "" {
					body = rt.body(fmt.Sprintf("no-such-model-%s", rt.op))
				}
			}
			a := censusPost(h, "/v1"+rt.path, body, tc.undeclared)
			rec := censusRecord(t, r)
			if a.status != tc.status {
				t.Fatalf("status %d, want %d: %.300s", a.status, tc.status, a.body)
			}
			if tc.code != "" {
				if code, _, _ := a.envelope(t); code != tc.code {
					t.Fatalf("code %q, want %q: %.300s", code, tc.code, a.body)
				}
			}
			rej, ok := rec.Rejection()
			if tc.reason == "" {
				if ok {
					t.Fatalf("declared %s/%s, want no declaration (no refusal by a rule)", rej.Reason, rej.Detail)
				}
			} else {
				if !ok {
					t.Fatalf("no declaration, want %s/%s", tc.reason, tc.detail)
				}
				if rej.Reason != tc.reason || rej.Detail != tc.detail {
					t.Fatalf("declared %s/%s, want %s/%s", rej.Reason, rej.Detail, tc.reason, tc.detail)
				}
				wantOp := rt.op
				if tc.stage {
					wantOp = ""
				}
				if rej.Op != wantOp {
					t.Errorf("declared op %q, want %q", rej.Op, wantOp)
				}
				if tc.reason == keyquota.RejectModel && (rej.Alias != "" || rec.Alias != "") {
					t.Errorf("model refusal alias %q (record %q), want \"\" (ledger %s)", rej.Alias, rec.Alias, keyquota.UnresolvedAlias)
				}
				// An acquire refusal carries the class the plan bound (the
				// route sets it after Plan; review F-2, drawn mutant d06).
				if (tc.detail == keyquota.DetailServerBusy || tc.detail == keyquota.DetailWaitExpired) && rej.Class != dispatch.ClassExternal.String() {
					t.Errorf("L2 refusal class %q, want %q", rej.Class, dispatch.ClassExternal)
				}
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

// TestRefusalLedgerCensusChat (W06-12): every exit of POST
// /v1/chat/completions declares exactly its ledger entry, or none.
func TestRefusalLedgerCensusChat(t *testing.T) { runCensus(t, censusRoutes()[0]) }

// TestRefusalLedgerCensusEmbeddings (W06-12): the same census for POST
// /v1/embeddings.
func TestRefusalLedgerCensusEmbeddings(t *testing.T) { runCensus(t, censusRoutes()[1]) }

// TestRefusalLedgerCensusRerank (W06-12): the same census for POST
// /v1/rerank (the Jina form; /v2 is the same handler with another shape).
func TestRefusalLedgerCensusRerank(t *testing.T) { runCensus(t, censusRoutes()[2]) }

// TestRefusalLedgerCensusGetModel (W06-12): GET /v1/models/{name} of an
// unknown name declares model/not_found with no alias; a served name
// declares nothing.
func TestRefusalLedgerCensusGetModel(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := &oai.AccessRecord{}
				ctx := context.WithValue(oai.WithAccessRecord(req.Context(), rec), consumerKey{}, r.ca.Load())
				next.ServeHTTP(w, req.WithContext(ctx))
				r.recs <- rec
			})
		})
		g.Method(http.MethodGet, "/models/*", oai.GetModel(r.deps))
	})
	for _, c := range []struct {
		name   string
		status int
		want   bool
	}{{"no-such-model", http.StatusNotFound, true}, {"chat", http.StatusOK, false}} {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models/"+c.name, nil))
		rec := censusRecord(t, r)
		if w.Code != c.status {
			t.Fatalf("%s: status %d, want %d: %s", c.name, w.Code, c.status, w.Body.Bytes())
		}
		rej, ok := rec.Rejection()
		switch {
		case ok != c.want:
			t.Errorf("%s: declared %v (%+v), want %v", c.name, ok, rej, c.want)
		case ok && (rej.Reason != keyquota.RejectModel || rej.Detail != keyquota.DetailNotFound || rej.Alias != ""):
			t.Errorf("%s: declared %s/%s alias %q, want model/not_found without alias", c.name, rej.Reason, rej.Detail, rej.Alias)
		}
	}
}

// censusRawNames is the request count of the raw-model-name pin per route.
const censusRawNames = 10000

// TestRefusalLedgerCensusKeepsTheRawModelNameOut (W06-12 gate (c), model
// half): censusRawNames requests per route, each with another unknown
// model name (every 97th past the parse's raw bound, the probe path) ⇒
// every record keeps Alias "" and declares model/not_found, and a real
// keyquota.Ledger fed with the declarations holds ONE cell (_unresolved,
// n = censusRawNames). Red would be a route that sets rec.Alias to the raw
// string before the resolution.
func TestRefusalLedgerCensusKeepsTheRawModelNameOut(t *testing.T) {
	for _, rt := range censusRoutes() {
		t.Run(rt.op, func(t *testing.T) {
			eng := rt.engine(t)
			r := rt.newRig(t, []backends.Backend{eng.row("spark")}, rt.slots, 1)
			h := censusMount(r, rt.path, rt.handler(r.deps))
			now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
			ledger := keyquota.NewLedger(func() time.Time { return now })
			start := time.Now()
			for i := range censusRawNames {
				name := fmt.Sprintf("unknown-model-%05d", i)
				if i%97 == 0 {
					name = fmt.Sprintf("%s-%s", name, strings.Repeat("y", 4096))
				}
				a := censusPost(h, "/v1"+rt.path, rt.body(name), false)
				rec := censusRecord(t, r)
				if a.status != http.StatusNotFound {
					t.Fatalf("request %d: status %d, want 404: %.200s", i, a.status, a.body)
				}
				rej, ok := rec.Rejection()
				if !ok || rej.Reason != keyquota.RejectModel || rej.Detail != keyquota.DetailNotFound {
					t.Fatalf("request %d: declared %v %s/%s, want model/not_found", i, ok, rej.Reason, rej.Detail)
				}
				if rec.Alias != "" || rej.Alias != "" {
					t.Fatalf("request %d: record alias %q, declared alias %q — the raw model name reached the record", i, rec.Alias, rej.Alias)
				}
				ledger.Note(rej)
			}
			elapsed := time.Since(start)
			cells := ledger.Drain()
			if len(cells) != 1 || cells[0].Alias != keyquota.UnresolvedAlias || cells[0].N != censusRawNames || ledger.Dropped() != 0 {
				t.Fatalf("ledger cells %+v dropped %d, want one cell %s n=%d", cells, ledger.Dropped(), keyquota.UnresolvedAlias, censusRawNames)
			}
			t.Logf("%s: %d unknown names in %v, one ledger cell (%s/%s, alias %s, op %q, n=%d)",
				rt.op, censusRawNames, elapsed.Round(time.Millisecond), cells[0].Reason, cells[0].Detail, cells[0].Alias, cells[0].Op, cells[0].N)
		})
	}
}
