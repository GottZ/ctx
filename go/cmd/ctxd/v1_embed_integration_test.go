//go:build integration

package main

// POST /v1/embeddings end to end over the production router (W02-09
// hardening H-6, Fremd-Review 2 R2-7): the real door, model group, gateway
// dependencies (gatewayDeps), planner, dispatcher and llmlog writer over a
// throwaway database — the embeddings twin of TestV1ChatOneLLMLogRow. The
// limiter is the admitting stand-in of the chat fixture (W06-10's meter is
// not merged).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	v1EmbedAlias     = "cfemb"
	v1EmbedChatAlias = "cfchat-e"
)

// v1EmbedEngine answers /v1/embeddings with one 4-d vector per input.
type v1EmbedEngine struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newV1EmbedEngine(t *testing.T) *v1EmbedEngine {
	t.Helper()
	e := &v1EmbedEngine{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(raw, &req)
		var items []string
		for i := range req.Input {
			items = append(items, fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":[0.5,-0.25,%d,1]}`, i, i+1))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[%s],"model":"engine","usage":{"prompt_tokens":%d,"total_tokens":%d}}`,
			strings.Join(items, ","), 3*len(req.Input), 3*len(req.Input))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// v1EmbedFixture is the production router over a throwaway database: an
// embedding alias (role "embedding", native 4), a chat alias, the knowledge
// admin key, an urgent consumer with all models and the fake engine as the
// pool's embedding row.
type v1EmbedFixture struct {
	router                *chi.Mux
	seed, pool            *pgxpool.Pool
	urgentKey             string
	urgentID, urgentKeyID string
}

func newV1EmbedFixture(t *testing.T, eng *v1EmbedEngine) *v1EmbedFixture {
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
	f := &v1EmbedFixture{seed: seed}
	insertAlias(t, seed, "_global", v1EmbedAlias, "embedding", `{"embedding.native_dims":4}`, true)
	insertAlias(t, seed, "_global", v1EmbedChatAlias, "chat", `{}`, true)
	created, adminID, err := store.BootstrapAdminKey(ctx, seed, v1AdminKey, "w02-09")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	uc, uk, ukey, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w02-09-urgent", AllModels: true, AdmissionLane: handler.LaneUrgent},
		nil, store.APIStamp(adminID, "w02-09"))
	if err != nil {
		t.Fatalf("urgent consumer: %v", err)
	}
	f.urgentKey, f.urgentID, f.urgentKeyID = ukey.Plaintext(), uc.ID, uk.ID

	f.pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(f.pool.Close)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = llmlog.Settle(sctx, f.pool)
	})

	dispatch.SetPrincipalHook(handler.RequestPrincipal)
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	row := backends.Backend{
		ID: "id-embed", Name: "embed-row", Host: eng.srv.URL, Protocol: backends.ProtocolOpenAI, Model: "engine-model",
		Roles: []string{"embedding"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 1, Timeouts: map[string]int{"embedding": 30},
	}
	backendPool := backends.NewPool(f.pool, nil)
	backendPool.SeedSnapshotForTest([]backends.Backend{row})
	scheduler := events.NewScheduler(f.pool, cfgStore, backendPool, events.StartupConfig{})
	aliases := bootModelAliases(ctx, f.pool, modelAliasCeilings(cfgStore))
	scheduler.SetModelAliasRegistry(aliases)
	projectHub := events.NewProjectHub(ctx, f.pool, cfgStore)
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{row.Host: {Slots: 4, InternalReserved: 1}}})
	scheduler.SetDispatcher(disp)
	sctx := fedhop.WithIdentity(ctx, fedhop.NewIdentity("00000000-0000-4000-8000-000000000209", bytes.Repeat([]byte{0x29}, 32)))
	quota := backends.NewQuotaAccountant(f.pool, 30*time.Second)
	limiter := &v1ChatLimiter{}
	wrap := func(routes []modelRoute) []modelRoute {
		out := append([]modelRoute(nil), routes...)
		for i, r := range out {
			if r.method == http.MethodPost && r.pattern == "/embeddings" {
				d := gatewayDeps(sctx, f.pool, cfgStore, aliases, backendPool, quota, disp)
				d.Limiter = limiter
				out[i].handler = oai.Embeddings(d)
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

// post sends one embeddings request with key over TLS (the door takes a
// key over TLS only).
func (f *v1EmbedFixture) post(t *testing.T, key, body string) v1Answer {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return v1Answer{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes()}
}

// v1EmbedRow is one gateway-embeddings row.
type v1EmbedRow struct {
	consumer, apiKey, modelKey, op, subBatches *string
	tokens                                     *int
	chain                                      int
}

func (f *v1EmbedFixture) rows(t *testing.T) []v1EmbedRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := llmlog.Settle(ctx, f.pool); err != nil {
		t.Fatalf("llmlog settle: %v", err)
	}
	rs, err := f.seed.Query(context.Background(), `
		SELECT consumer_id::text, api_key_id::text, metadata->'gw'->>'model_key_id', metadata->'gw'->>'op',
		       metadata->>'sub_batches', prompt_tokens, COALESCE(jsonb_array_length(metadata->'chain'), 0)
		  FROM context_llm_log WHERE pipeline = $1 ORDER BY created_at`, llmlog.PipelineGatewayEmbeddings)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rs.Close()
	var out []v1EmbedRow
	for rs.Next() {
		var r v1EmbedRow
		if err := rs.Scan(&r.consumer, &r.apiKey, &r.modelKey, &r.op, &r.subBatches, &r.tokens, &r.chain); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestV1EmbeddingsOneLLMLogRow (H-6): over the production router a
// request of three inputs is served with three vectors at their index,
// and writes exactly one gateway-embeddings row — the consumer, no
// knowledge key, the model key, op embed, one sub-block, the tokens.
func TestV1EmbeddingsOneLLMLogRow(t *testing.T) {
	eng := newV1EmbedEngine(t)
	f := newV1EmbedFixture(t, eng)
	a := f.post(t, f.urgentKey, `{"model":"`+v1EmbedAlias+`","input":["alpha","beta","gamma"]}`)
	if a.status != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", a.status, a.body)
	}
	var ans struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(a.body, &ans); err != nil || len(ans.Data) != 3 || ans.Model != v1EmbedAlias {
		t.Fatalf("answer %s (%v), want three vectors of %s", a.body, err, v1EmbedAlias)
	}
	for i, d := range ans.Data {
		if d.Index != i || len(d.Embedding) != 4 || d.Embedding[2] != float32(i+1) {
			t.Errorf("data[%d] = %+v, want the engine's vector of input %d", i, d, i)
		}
	}
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d gateway-embeddings rows, want 1", len(rows))
	}
	r := rows[0]
	if v1ChatDeref(r.consumer) != f.urgentID || r.apiKey != nil || v1ChatDeref(r.modelKey) != f.urgentKeyID {
		t.Errorf("consumer_id %s api_key_id %s gw.model_key_id %s, want %s / NULL / %s",
			v1ChatDeref(r.consumer), v1ChatDeref(r.apiKey), v1ChatDeref(r.modelKey), f.urgentID, f.urgentKeyID)
	}
	if v1ChatDeref(r.op) != llmlog.GatewayOpEmbed || v1ChatDeref(r.subBatches) != "1" || r.tokens == nil || *r.tokens != 9 || r.chain != 1 {
		t.Errorf("row op %s sub_batches %s tokens %v chain %d, want embed / 1 / 9 / 1", v1ChatDeref(r.op), v1ChatDeref(r.subBatches), r.tokens, r.chain)
	}
}

// TestV1EmbeddingsDoorAndKind (H-6): the door refuses a knowledge
// credential on the route with 401 invalid_api_key, and a chat alias is
// 400 model_not_supported — neither reaches the engine nor writes a row.
func TestV1EmbeddingsDoorAndKind(t *testing.T) {
	eng := newV1EmbedEngine(t)
	f := newV1EmbedFixture(t, eng)
	if a := f.post(t, v1AdminKey, `{"model":"`+v1EmbedAlias+`","input":"x"}`); a.status != http.StatusUnauthorized || a.code() != oai.CodeInvalidAPIKey {
		t.Errorf("knowledge credential: %d %s, want 401 invalid_api_key", a.status, a.body)
	}
	if a := f.post(t, f.urgentKey, `{"model":"`+v1EmbedChatAlias+`","input":"x"}`); a.status != http.StatusBadRequest || a.code() != oai.CodeModelNotSupported {
		t.Errorf("chat alias: %d %s, want 400 model_not_supported", a.status, a.body)
	}
	if n := eng.hits.Load(); n != 0 {
		t.Errorf("engine hits %d, want 0", n)
	}
	if rows := f.rows(t); len(rows) != 0 {
		t.Errorf("%d rows, want 0", len(rows))
	}
}
