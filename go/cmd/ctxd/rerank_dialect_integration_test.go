//go:build integration

// W02-10 hardening 1 (external review 2 F-2): the dialect of the production
// route tables — v1Routes mounts oai.Rerank with RerankJina, v2Routes with
// RerankCohere — is the one thing that tells POST /v1/rerank from POST
// /v2/rerank; over the real router (buildRouter without a route wrap) a
// request without return_documents echoes every document on /v1 and none on
// /v2.
//
//	go test -tags=integration -p 1 -count=1 -run 'V2RerankDialect' -v ./cmd/ctxd/
package main

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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestV2RerankDialectOverTheRealRouter(t *testing.T) {
	var hits atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/rerank" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var call struct {
			Documents []string `json:"documents"`
		}
		_ = json.Unmarshal(raw, &call)
		res := make([]string, len(call.Documents))
		for i := range call.Documents {
			res[i] = fmt.Sprintf(`{"index":%d,"relevance_score":%d}`, i, i)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"engine","usage":{"prompt_tokens":%d,"total_tokens":%d},"results":[%s]}`,
			2*len(call.Documents), 2*len(call.Documents), strings.Join(res, ","))
	}))
	t.Cleanup(engine.Close)

	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	insertAlias(t, seed, "_global", "rr", "rerank", `{}`, true)
	created, adminID, err := store.BootstrapAdminKey(ctx, seed, v1AdminKey, "w02-10")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	_, _, key, err := store.CreateConsumerWithKey(ctx, seed,
		store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "w02-10-dialect", AllModels: true, AdmissionLane: handler.LaneUrgent},
		nil, store.APIStamp(adminID, "w02-10"))
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	dispatch.SetPrincipalHook(handler.RequestPrincipal) // as cmd/ctxd/main.go
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	row := backends.Backend{
		ID: "id-rerank", Name: "rerank-row", Host: engine.URL, Protocol: backends.ProtocolOpenAI, Model: "engine-model",
		Roles: []string{"rerank"}, Trust: backends.TrustFull, Locality: "local", Scope: backends.GlobalScope, Enabled: true,
		Priority: 1, Timeouts: map[string]int{"rerank": 30},
	}
	backendPool := backends.NewPool(pool, nil)
	backendPool.SeedSnapshotForTest([]backends.Backend{row})
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	scheduler.SetModelAliasRegistry(bootModelAliases(ctx, pool, modelAliasCeilings(cfgStore)))
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	disp.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{row.Host: {Slots: 4, InternalReserved: 1}}})
	scheduler.SetDispatcher(disp)
	sctx := fedhop.WithIdentity(ctx, fedhop.NewIdentity("00000000-0000-4000-8000-000000000210", bytes.Repeat([]byte{0x2a}, 32)))
	router, _, _ := buildRouter(sctx, pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, disp, nil)
	next := *cfgStore.Snapshot()
	next.Gateway.Enabled = true
	if err := cfgStore.Replace(&next); err != nil {
		t.Fatalf("set gateway.enabled=true: %v", err)
	}

	docs := []string{"alpha", "beta", "gamma"}
	body := `{"model":"rr","query":"q","documents":["alpha","beta","gamma"]}`
	for _, tc := range []struct {
		path string
		echo bool
	}{{"/v1/rerank", true}, {"/v2/rerank", false}} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.1:40000"
		req.TLS = &tls.ConnectionState{}
		req.Header.Set("Authorization", "Bearer "+key.Plaintext())
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var reply struct {
			Results []struct {
				Index    int `json:"index"`
				Document *struct {
					Text string `json:"text"`
				} `json:"document"`
			} `json:"results"`
		}
		err := json.Unmarshal(rec.Body.Bytes(), &reply)
		echoed := 0
		for _, r := range reply.Results {
			if r.Document != nil {
				echoed++
			}
		}
		t.Logf("W0210 dialect POST %s without return_documents: %d, %d results, %d with document.text (engine hits %d)",
			tc.path, rec.Code, len(reply.Results), echoed, hits.Load())
		if rec.Code != http.StatusOK || err != nil || len(reply.Results) != len(docs) {
			t.Fatalf("POST %s: %d %.300s (%v), want 200 with %d results", tc.path, rec.Code, rec.Body.String(), err, len(docs))
		}
		for _, r := range reply.Results {
			switch {
			case tc.echo && (r.Document == nil || r.Document.Text != docs[r.Index]):
				t.Errorf("POST %s: result index %d without document.text %q — the /v1 dialect (Jina) defaults return_documents to true", tc.path, r.Index, docs[r.Index])
			case !tc.echo && r.Document != nil:
				t.Errorf("POST %s: result index %d carries document %q — the /v2 dialect (Cohere v2) defaults return_documents to false", tc.path, r.Index, r.Document.Text)
			}
		}
	}
}
