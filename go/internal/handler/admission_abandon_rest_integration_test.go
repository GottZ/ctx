//go:build integration

// Issue #483 point 10: the three fail-open waits #453 left unmarked — the
// LLM-judge rerank (rrf.Rerank kept its error to itself), the temporal LLM
// fallback (its caller only logged the error) and the inline embed backfill
// (the error went to the failure memo only). A client that gives up while
// one of them waits for its admission slot left a server that had no slot
// for it: the request's 499 line says so at WARN with waited_for=admission,
// as at the translation and the cross-encoder rerank. Each stage keeps its own
// WARN line unchanged.
//
//	go test -tags=integration ./internal/handler/ -run 'TestQueryAdmissionAbandonJudge|TestQueryAdmissionAbandonTemporal|TestBackfillAdmissionAbandon' -count=1 -v
package handler

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// aaqTemporalQuery has temporal intent the rules do not parse, and is not
// German: the translate role sees the temporal LLM fallback and nothing else.
const aaqTemporalQuery = "what changed since the database migration"

// TestQueryAdmissionAbandonJudgeRerankIsWarn — no backend carries the rerank
// role, so the rerank is the LLM judge over the synthesis chain, whose only
// slot is held; the judge queues; its client leaves.
func TestQueryAdmissionAbandonJudgeRerankIsWarn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "aaq-embed", Host: emb.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1, Roles: []string{backends.RoleEmbed}},
		{ID: "s", Name: "aaq-judge", Host: gateHeldOrigin, Protocol: backends.ProtocolOllama,
			Model: "test-judge", Trust: backends.TrustFull, Enabled: true, Priority: 1, Roles: []string{backends.RoleSynthesis}},
	})
	h := NewQueryHandler(pool, config.NewStore(aaqRerankConfig()), bpool, nil, blocktype.NewRegistry(), d)
	logs := installRejectLogCapture(t)
	srv, done := aaqServe(t, h, nil)

	abortAfter(t, srv.URL+"/api/query", "", `{"query":"`+bw2GoldenQuery+`","synthesize":false}`, aaqQueued(t, d, gateHeldOrigin))
	waitDone(t, done)

	if err := aaqStageLine(t, logs, "rerank: LLM call failed, returning original order"); !dispatch.AbandonedWait(err) {
		t.Errorf("judge error %v is no abandoned admission wait — the probe did not wait in the queue", err)
	}
	aaqRequestLog(t, logs, true)
}

// TestQueryAdmissionAbandonTemporalFallbackIsWarn — the rules do not parse
// the question's temporal intent, so the temporal LLM fallback asks the
// translate role, whose only slot is held; its client leaves.
func TestQueryAdmissionAbandonTemporalFallbackIsWarn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if !llm.HasTemporalIntent(aaqTemporalQuery) || llm.NormalizeTemporalRules(aaqTemporalQuery, time.Now()) != nil || llm.NeedsTranslation(aaqTemporalQuery) {
		t.Fatal("fixture question no longer takes the temporal LLM fallback alone — the probe would test something else")
	}
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)
	h := NewQueryHandler(pool, config.NewStore(bw2Config()), aaqPool(emb.srv.URL, gateHeldOrigin, ""), nil,
		blocktype.NewRegistry(), d)
	logs := installRejectLogCapture(t)
	srv, done := aaqServe(t, h, nil)

	abortAfter(t, srv.URL+"/api/query", "", `{"query":"`+aaqTemporalQuery+`","synthesize":false}`, aaqQueued(t, d, gateHeldOrigin))
	waitDone(t, done)

	if err := aaqStageLine(t, logs, "temporal LLM fallback failed, no temporal expansion available"); !dispatch.AbandonedWait(err) {
		t.Errorf("temporal fallback error %v is no abandoned admission wait — the probe did not wait in the queue", err)
	}
	aaqRequestLog(t, logs, true)
}

// TestBackfillAdmissionAbandonIsWarn — a pending block in the caller's scope,
// the embed role's only slot held: the backfill's embed queues; its client
// leaves. backfillPending runs on the request's context behind the production
// request chain, as HandleQuery calls it.
func TestBackfillAdmissionAbandonIsWarn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := testdb.SetupTestDB(t)
	if _, err := store.UpsertBlock(context.Background(), pool, "i483", "i483 pending block", "waits for its vector",
		nil, nil, "private", false, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("seed pending block: %v", err)
	}
	h := NewQueryHandler(pool, config.NewStore(snapshotTestConfig()), embedPool(gateHeldOrigin), nil, blocktype.NewRegistry(), d)
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 4, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}
	ar := &auth.AuthResult{ApiKeyID: bw2UserKeyID, HomeScope: "private", ReadScopes: []string{"private"}, IsValid: true}
	logs := installRejectLogCapture(t)
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // net/http sees a client leave only once the body is read
		h.backfillPending(withAuthResult(r.Context(), ar), nil, ar.ReadScopes, h.embedAdmission(), cfg)
	}), nil)

	abortAfter(t, srv.URL+"/api/query", "", `{}`, aaqQueued(t, d, gateHeldOrigin))
	waitDone(t, done)

	recs := logs.withMsg("query backfill: embed failed")
	if len(recs) != 1 {
		t.Fatalf("want the backfill's own line once, got %+v (all: %+v)", recs, logs.all())
	}
	if err, _ := recs[0].attrs["error"].(error); !dispatch.AbandonedWait(err) {
		t.Errorf("backfill error %v is no abandoned admission wait — the probe did not wait in the queue", err)
	}
	aaqRequestLog(t, logs, true)
}
