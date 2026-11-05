//go:build integration

// W02-07 over the PRODUCTION router (buildRouter): POST /v1/chat/completions
// behind the whole chain — the global middleware (handler.RequestID), the
// model group (access log → recovery → EnabledGate → ModelAuth with a real
// ctxm_ key → LoopGuard → PeerIngress → body cap → in-flight budget) and the
// route's production dependencies (gatewayDeps: the real planner over the
// real dispatcher, the pool, the tenant quota, the federation filter, the
// llmlog writer) against a throwaway database. The one stand-in: the
// limiter. Production wires the per-consumer meter (W06-10; its gate runs
// against the unwrapped production router, v1_chat_meter_integration_test.go),
// but these tests are about the route, not the meter, so the route is rebuilt
// from gatewayDeps with a limiter that admits everything.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestV1Chat' -v ./cmd/ctxd/
//
// G11 (B16, RC-C form): a client X-Request-ID reaches neither the answer's
// id nor any upstream (body or headers; no upstream sees an X-Request-ID at
// all) and lands in metadata.gw.client_req_id. "A ctx row sees a different
// id" needs tier F (F-W6, RC-E) and is not tested here.
// G17 (design/06 §4.6): one request ⇒ exactly one gateway-chat row; a
// failover ⇒ one row with both wire attempts in metadata.chain; a planner
// refusal ⇒ no row.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// v1ChatAlias is the fixture's _global chat alias.
const v1ChatAlias = "cfchat"

// v1ChatClientID is the client's own request id: valid for the house
// RequestID middleware (hex), so the global middleware adopts it as ctx's
// request id — the value a careless route would reuse.
const v1ChatClientID = "deadbeef"

// v1ChatAnswer is an OpenRouter-shaped answer of the fake engines.
const v1ChatAnswer = `{"id":"gen-1","provider":"P","model":"M","object":"chat.completion","created":1700000000,` +
	`"choices":[{"logprobs":null,"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello"}}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

// v1ChatEngine is one fake engine: it records the raw body and the headers
// of every request and answers with status (200 = v1ChatAnswer).
type v1ChatEngine struct {
	srv     *httptest.Server
	status  int
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	hits    atomic.Int64
}

func newV1ChatEngine(t *testing.T, status int) *v1ChatEngine {
	t.Helper()
	e := &v1ChatEngine{status: status}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, raw)
		e.headers = append(e.headers, r.Header.Clone())
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if e.status != http.StatusOK {
			w.WriteHeader(e.status)
			_, _ = io.WriteString(w, `{"error":{"message":"engine down"}}`)
			return
		}
		_, _ = io.WriteString(w, v1ChatAnswer)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// row is a chat backend row on the engine.
func (e *v1ChatEngine) row(name string, prio int) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: e.srv.URL, Protocol: backends.ProtocolOpenAI, Model: "engine-model",
		Roles: []string{"chat"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: prio, Timeouts: map[string]int{"chat": 30},
	}
}

// sawValue reports whether any request of the engine carried v in its body
// or in any header name or value, and whether any carried X-Request-ID.
func (e *v1ChatEngine) sawValue(v string) (inBody, inHeader, requestIDHeader bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, b := range e.bodies {
		inBody = inBody || bytes.Contains(b, []byte(v))
		for k, vals := range e.headers[i] {
			if strings.EqualFold(k, "X-Request-ID") {
				requestIDHeader = true
			}
			for _, x := range vals {
				inHeader = inHeader || strings.Contains(x, v) || strings.Contains(k, v)
			}
		}
	}
	return inBody, inHeader, requestIDHeader
}

// v1ChatLimiter admits every request (the stand-in for the meter).
type v1ChatLimiter struct{ admits atomic.Int64 }

type v1ChatReservation struct{}

func (v1ChatReservation) Settle(dispatch.Usage)        {}
func (v1ChatReservation) Keep()                        {}
func (v1ChatReservation) Abort()                       {}
func (v1ChatReservation) Reject(string, time.Duration) {}
func (v1ChatReservation) Headers() http.Header         { return nil }

func (l *v1ChatLimiter) Admit(context.Context, *auth.ConsumerAuth, modelalias.Alias, oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	l.admits.Add(1)
	return v1ChatReservation{}, nil
}

// v1ChatFixture is the production router over a throwaway database: one
// _global chat alias, an urgent-lane consumer with all models, and the fake
// engines as the pool's chat rows. disp is the router's dispatcher (a test
// withdraws the external offer of its targets for a planner refusal).
type v1ChatFixture struct {
	router                *chi.Mux
	seed, pool            *pgxpool.Pool
	limiter               *v1ChatLimiter
	disp                  *dispatch.Dispatcher
	policy                dispatch.Policy
	urgentKey             string
	urgentID, urgentKeyID string
}

func newV1ChatFixture(t *testing.T, rows []backends.Backend) *v1ChatFixture {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	f := &v1ChatFixture{seed: seed, limiter: &v1ChatLimiter{}}
	insertAlias(t, seed, "_global", v1ChatAlias, "chat", `{}`, true)
	admin, _, err := store.CreateApiKey(ctx, seed, "w02-07-chat", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "w02-07")
	uc, uk, ukey, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w02-07-urgent", AllModels: true, AdmissionLane: handler.LaneUrgent}, nil, stamp)
	if err != nil {
		t.Fatalf("urgent consumer: %v", err)
	}
	f.urgentKey, f.urgentID, f.urgentKeyID = ukey.Plaintext(), uc.ID, uk.ID

	f.pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(f.pool.Close)
	// Every row Recorded on the pool has landed before it closes.
	t.Cleanup(func() { f.settle(t) })

	dispatch.SetPrincipalHook(handler.RequestPrincipal) // as cmd/ctxd/main.go
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	backendPool := backends.NewPool(f.pool, nil)
	backendPool.SeedSnapshotForTest(rows)
	scheduler := events.NewScheduler(f.pool, cfgStore, backendPool, events.StartupConfig{})
	aliases := bootModelAliases(ctx, f.pool, modelAliasCeilings(cfgStore))
	scheduler.SetModelAliasRegistry(aliases)
	projectHub := events.NewProjectHub(ctx, f.pool, cfgStore)
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
	for _, b := range rows {
		pol.Targets[b.Host] = dispatch.TargetPolicy{Slots: 4, InternalReserved: 1}
	}
	disp.UpdatePolicy(pol)
	f.disp, f.policy = disp, pol
	scheduler.SetDispatcher(disp)
	ident := fedhop.NewIdentity("00000000-0000-4000-8000-000000000207", bytes.Repeat([]byte{0x27}, 32))
	sctx := fedhop.WithIdentity(ctx, ident)
	// The route as production builds it (gatewayDeps with buildRouter's
	// arguments; the quota accountant is a second instance built like
	// buildRouter's), plus the admitting limiter.
	quota := backends.NewQuotaAccountant(f.pool, 30*time.Second)
	wrap := func(routes []modelRoute) []modelRoute {
		out := append([]modelRoute(nil), routes...)
		for i, r := range out {
			if r.method == http.MethodPost && r.pattern == "/chat/completions" {
				d := gatewayDeps(sctx, f.pool, cfgStore, aliases, backendPool, quota, disp, nil)
				d.Limiter = f.limiter
				out[i].handler = oai.ChatCompletions(d)
			}
		}
		return out
	}
	f.router, _, _ = buildRouter(sctx, f.pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, disp, wrap)
	next := *cfgStore.Snapshot()
	next.Gateway.Enabled = true
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatalf("gateway.enabled: %v", err)
	}
	return f
}

// post sends one chat request with key over TLS from a fixed client
// address (the door takes a model key over TLS only).
func (f *v1ChatFixture) post(t *testing.T, key, body string, hdr ...string) v1Answer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return v1Answer{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes()}
}

func (f *v1ChatFixture) settle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := llmlog.Settle(ctx, f.pool); err != nil {
		t.Fatalf("llmlog settle: %v", err)
	}
}

// v1ChatRow is one gateway-chat row as the tests read it.
type v1ChatRow struct {
	consumer, apiKey, modelKey, clientReq *string
	chain                                 int
}

// rows settles the async writer and reads every gateway-chat row.
func (f *v1ChatFixture) rows(t *testing.T) []v1ChatRow {
	t.Helper()
	f.settle(t)
	rs, err := f.seed.Query(context.Background(), `
		SELECT consumer_id::text, api_key_id::text, metadata->'gw'->>'model_key_id',
		       metadata->'gw'->>'client_req_id', COALESCE(jsonb_array_length(metadata->'chain'), 0)
		  FROM context_llm_log WHERE pipeline = $1 ORDER BY created_at`, llmlog.PipelineGatewayChat)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rs.Close()
	var out []v1ChatRow
	for rs.Next() {
		var r v1ChatRow
		if err := rs.Scan(&r.consumer, &r.apiKey, &r.modelKey, &r.clientReq, &r.chain); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func v1ChatDeref(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

func v1ChatBody() string {
	return `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"hi"}]}`
}

// TestV1ChatRequestIdentity is G11 (B16, RC-C form) through the real
// middleware chain.
func TestV1ChatRequestIdentity(t *testing.T) {
	eng := newV1ChatEngine(t, http.StatusOK)
	f := newV1ChatFixture(t, []backends.Backend{eng.row("primary", 1)})
	a := f.post(t, f.urgentKey, v1ChatBody(), "X-Request-ID", v1ChatClientID)
	if a.status != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", a.status, a.body)
	}
	// The global RequestID middleware adopted the client's id as ctx's
	// request id — the value the route must not reuse.
	if got := a.header.Get("X-Request-ID"); got != v1ChatClientID {
		t.Fatalf("X-Request-ID echo %q, want %q (the request did not pass handler.RequestID)", got, v1ChatClientID)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(a.body, &out); err != nil {
		t.Fatalf("answer %q: %v", a.body, err)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") || out.ID == "chatcmpl-"+v1ChatClientID || strings.Contains(out.ID, v1ChatClientID) {
		t.Errorf("answer id %q, want chatcmpl-<server-side id> without %q", out.ID, v1ChatClientID)
	}
	if n := eng.hits.Load(); n != 1 {
		t.Fatalf("the engine saw %d request(s), want 1", n)
	}
	inBody, inHeader, ridHeader := eng.sawValue(v1ChatClientID)
	if inBody || inHeader || ridHeader {
		t.Errorf("upstream saw the client id: body %v, header %v; X-Request-ID header present %v", inBody, inHeader, ridHeader)
	}
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d gateway-chat rows, want 1", len(rows))
	}
	if got := v1ChatDeref(rows[0].clientReq); got != v1ChatClientID {
		t.Errorf("metadata.gw.client_req_id %q, want %q", got, v1ChatClientID)
	}
}

// TestV1ChatOneLLMLogRow is G17: one request ⇒ exactly one gateway-chat
// row, attributed to the consumer and its model key, never to a knowledge
// key.
func TestV1ChatOneLLMLogRow(t *testing.T) {
	eng := newV1ChatEngine(t, http.StatusOK)
	f := newV1ChatFixture(t, []backends.Backend{eng.row("primary", 1)})
	if a := f.post(t, f.urgentKey, v1ChatBody()); a.status != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", a.status, a.body)
	}
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d gateway-chat rows, want 1", len(rows))
	}
	r := rows[0]
	if v1ChatDeref(r.consumer) != f.urgentID || r.apiKey != nil || v1ChatDeref(r.modelKey) != f.urgentKeyID {
		t.Errorf("consumer_id %s api_key_id %s gw.model_key_id %s, want %s / NULL / %s",
			v1ChatDeref(r.consumer), v1ChatDeref(r.apiKey), v1ChatDeref(r.modelKey), f.urgentID, f.urgentKeyID)
	}
	if r.chain != 1 {
		t.Errorf("metadata.chain has %d entries, want 1", r.chain)
	}
}

// TestV1ChatFailoverOneRow is G17's failover: the first row answers 502,
// the second serves ⇒ one row, both wire attempts in metadata.chain.
func TestV1ChatFailoverOneRow(t *testing.T) {
	down := newV1ChatEngine(t, http.StatusBadGateway)
	up := newV1ChatEngine(t, http.StatusOK)
	// Higher priority first (Pool.Chain): the failing row leads.
	f := newV1ChatFixture(t, []backends.Backend{down.row("down", 2), up.row("up", 1)})
	if a := f.post(t, f.urgentKey, v1ChatBody()); a.status != http.StatusOK {
		t.Fatalf("status %d (%s), want 200 from the second row", a.status, a.body)
	}
	if d, u := down.hits.Load(), up.hits.Load(); d != 1 || u != 1 {
		t.Fatalf("engine hits down=%d up=%d, want 1 and 1 (the walk did not fail over)", d, u)
	}
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d gateway-chat rows, want 1 for one request", len(rows))
	}
	if rows[0].chain != 2 {
		t.Errorf("metadata.chain has %d entries, want 2 (the 502 and the served attempt)", rows[0].chain)
	}
	if v1ChatDeref(rows[0].consumer) != f.urgentID || rows[0].apiKey != nil {
		t.Errorf("consumer_id %s api_key_id %s", v1ChatDeref(rows[0].consumer), v1ChatDeref(rows[0].apiKey))
	}
}

// TestV1ChatPlannerRejectionNoRow is G17's refusal: the dispatcher offers
// the chain's target no external share (its policy withdrawn) ⇒ the planner
// refuses with ErrChainNotOffered, 503 model_unavailable, before any wire
// attempt ⇒ no row; the engine is not reached. A refusal that holds before
// and after A03-W9 (Fremd-Review 2 H-6: the bulk lane is served there). The
// request after the offer is restored writes its one row — the control that
// the writer is live in the same fixture.
func TestV1ChatPlannerRejectionNoRow(t *testing.T) {
	eng := newV1ChatEngine(t, http.StatusOK)
	f := newV1ChatFixture(t, []backends.Backend{eng.row("primary", 1)})
	f.disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}})
	a := f.post(t, f.urgentKey, v1ChatBody())
	if a.status != http.StatusServiceUnavailable || a.code() != oai.CodeModelUnavailable {
		t.Fatalf("target not offered: status %d code %q (%s), want 503 %s", a.status, a.code(), a.body, oai.CodeModelUnavailable)
	}
	if n := eng.hits.Load(); n != 0 {
		t.Fatalf("the engine saw %d request(s) for a refused request", n)
	}
	if rows := f.rows(t); len(rows) != 0 {
		t.Fatalf("%d gateway-chat rows after a planner refusal, want 0", len(rows))
	}
	f.disp.UpdatePolicy(f.policy)
	if a := f.post(t, f.urgentKey, v1ChatBody()); a.status != http.StatusOK {
		t.Fatalf("control: status %d (%s), want 200", a.status, a.body)
	}
	rows := f.rows(t)
	if len(rows) != 1 || v1ChatDeref(rows[0].consumer) != f.urgentID {
		t.Fatalf("control: %d rows (%+v), want 1 of the urgent consumer", len(rows), rows)
	}
}

// classRows settles the writer and reads consumer_id and dispatch_class of
// every gateway-chat row, oldest first.
func (f *v1ChatFixture) classRows(t *testing.T) [][2]string {
	t.Helper()
	f.settle(t)
	rs, err := f.seed.Query(context.Background(), `
		SELECT COALESCE(consumer_id::text, '<NULL>'), COALESCE(dispatch_class, '<NULL>')
		  FROM context_llm_log WHERE pipeline = $1 ORDER BY created_at`, llmlog.PipelineGatewayChat)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rs.Close()
	var out [][2]string
	for rs.Next() {
		var r [2]string
		if err := rs.Scan(&r[0], &r[1]); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestV1ChatBulkLaneIsExternalBulk is W02-07 × A03-W9 over the production
// router (FOLGE-M M1): a consumer whose admission_lane is bulk is served —
// 200, service_tier "flex", one wire contact, exactly one gateway-chat row
// with dispatch_class external_bulk and the consumer's id. The comparison is
// the same consumer with its lane raised to urgent (the door reads the lane
// per request, no cache): service_tier "default", a second row with
// dispatch_class external.
func TestV1ChatBulkLaneIsExternalBulk(t *testing.T) {
	eng := newV1ChatEngine(t, http.StatusOK)
	f := newV1ChatFixture(t, []backends.Backend{eng.row("primary", 1)})
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, f.seed, "folge-m-bulk", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "folge-m")
	consumer, _, key, err := store.CreateConsumerWithKey(ctx, f.seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "folge-m-bulk", AllModels: true, AdmissionLane: handler.LaneBulk}, nil, stamp)
	if err != nil {
		t.Fatalf("bulk consumer: %v", err)
	}
	for i, tc := range []struct {
		lane, tier, class string
	}{
		{handler.LaneBulk, "flex", dispatch.ClassExternalBulk.String()},
		{handler.LaneUrgent, "default", dispatch.ClassExternal.String()},
	} {
		if i > 0 {
			lane := tc.lane
			if _, err := store.UpdateConsumer(ctx, f.seed, consumer.ID, store.ModelAccess{ServerAdmin: true},
				store.ModelConsumerPatch{AdmissionLane: &lane}, stamp); err != nil {
				t.Fatalf("lane %s: %v", tc.lane, err)
			}
		}
		a := f.post(t, key.Plaintext(), v1ChatBody())
		if a.status != http.StatusOK {
			t.Fatalf("lane %s: status %d (%s), want 200", tc.lane, a.status, a.body)
		}
		var out struct {
			ServiceTier string `json:"service_tier"`
		}
		if err := json.Unmarshal(a.body, &out); err != nil {
			t.Fatalf("lane %s: answer %q: %v", tc.lane, a.body, err)
		}
		if out.ServiceTier != tc.tier {
			t.Errorf("lane %s: service_tier %q, want %q", tc.lane, out.ServiceTier, tc.tier)
		}
		if n := eng.hits.Load(); n != int64(i+1) {
			t.Errorf("lane %s: the engine saw %d request(s), want %d", tc.lane, n, i+1)
		}
		rows := f.classRows(t)
		if len(rows) != i+1 {
			t.Fatalf("lane %s: %d gateway-chat rows %v, want %d", tc.lane, len(rows), rows, i+1)
		}
		if got, want := rows[i], [2]string{consumer.ID, tc.class}; got != want {
			t.Errorf("lane %s: row (consumer_id, dispatch_class) = %v, want %v", tc.lane, got, want)
		}
	}
}
