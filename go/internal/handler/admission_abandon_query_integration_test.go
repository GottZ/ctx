//go:build integration

// Issue #453: a client that gives up while the query pipeline waits for an
// admission slot at one of its fail-open stages — the translation and the
// cross-encoder rerank — left a server that had no slot ready for it, the
// same signal as an abandoned wait at the query embed. The stage keeps its
// own WARN line and the pipeline goes on, so the request fails later on the
// ended context with an error that is no abandoned wait; the request log
// must still mark the 499 at WARN with waited_for=admission. A client that
// leaves without a wait at that stage stays INFO without waited_for.
//
// Each run drives HandleQuery through the production middleware (RequestID,
// Logger, the instance identity) on a real httptest server against a PG18
// testcontainer and a real dispatcher whose only slot on the stage's origin
// is held; the client leaves once the dispatcher counts the request in that
// origin's queue.
//
//	go test -tags=integration ./internal/handler/ -run 'TestQueryAdmissionAbandon' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llm"
)

const (
	// aaqGermanQuery matches the non-English heuristic and carries no
	// temporal word: the translate role sees the translation and nothing
	// else.
	aaqGermanQuery = "wie ist der status der datenbank"

	aaqTranslateMsg = "translation failed, using original"
	aaqRerankMsg    = "rerank admission rejected, using original order"
)

// aaqPool seeds the embed row on the fake backend and the translate and
// rerank rows on the given origins ("" leaves the role out).
func aaqPool(embedHost, translateHost, rerankHost string) *backends.Pool {
	rows := []backends.Backend{{ID: "e", Name: "aaq-embed", Host: embedHost, Protocol: backends.ProtocolOllama,
		Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
		Roles: []string{backends.RoleEmbed}}}
	if translateHost != "" {
		rows = append(rows, backends.Backend{ID: "t", Name: "aaq-translate", Host: translateHost, Protocol: backends.ProtocolOllama,
			Model: "test-translate", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleTranslate}})
	}
	if rerankHost != "" {
		rows = append(rows, backends.Backend{ID: "r", Name: "aaq-rerank", Host: rerankHost, Protocol: "rerank",
			Model: "test-rerank", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleRerank}})
	}
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest(rows)
	return bpool
}

// aaqServe serves h.HandleQuery behind RequestID, Logger and the instance
// identity (the embed call's hop stamp needs it) with the bw2 user's
// credentials in the request context, as the Auth middleware puts them
// there. The body is read first, as HandleQuery reads it; before (nil: none)
// runs after that.
func aaqServe(t *testing.T, h *QueryHandler, before func(r *http.Request)) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	ar := &auth.AuthResult{ApiKeyID: bw2UserKeyID, HomeScope: bw2Scope, ReadScopes: []string{bw2Scope}, IsValid: true}
	id := fedhop.NewIdentity("00000000-0000-4000-8000-000000000453", bytes.Repeat([]byte{0x45}, 32))
	return abortServer(t, fedhop.IdentityMiddleware(id)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if before != nil {
			before(r)
		}
		h.HandleQuery(w, r.WithContext(withAuthResult(r.Context(), ar)))
	})), nil)
}

// aaqQueued returns the moment the client leaves: one interactive request
// waits in origin's admission queue.
func aaqQueued(t *testing.T, d *dispatch.Dispatcher, origin string) func() {
	return func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for targetSnap(d, origin).Interactive.Waiting != 1 {
			if time.Now().After(deadline) {
				t.Fatal("the request never queued for its admission")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// aaqStageLine returns the stage's own line: once, at WARN, as it was —
// no client_gone, no waited_for of its own — and the error it carries.
func aaqStageLine(t *testing.T, logs *rejectLogCapture, msg string) error {
	t.Helper()
	recs := logs.withMsg(msg)
	if len(recs) != 1 {
		t.Fatalf("want the stage line %q once, got %+v (all: %+v)", msg, recs, logs.all())
	}
	line := recs[0]
	if line.level != slog.LevelWarn || line.attrs["client_gone"] != nil || line.attrs["waited_for"] != nil {
		t.Errorf("stage line = %+v, want it unchanged at WARN", line)
	}
	err, _ := line.attrs["error"].(error)
	if err == nil {
		t.Fatalf("stage line carries no error: %+v", line)
	}
	return err
}

// aaqRequestLog checks the run's verdict: no ERROR line, and the 499 of the
// request log at WARN with waited_for=admission (waited) or at INFO without
// waited_for.
func aaqRequestLog(t *testing.T, logs *rejectLogCapture, waited bool) {
	t.Helper()
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up produced ERROR lines: %+v", errs)
	}
	req := requestLogOf(t, logs)
	wantLevel, wantWaited := slog.LevelInfo, any(nil)
	if waited {
		wantLevel, wantWaited = slog.LevelWarn, "admission"
	}
	if statusOf(t, req) != clientClosedStatus || req.level != wantLevel || req.attrs["waited_for"] != wantWaited {
		t.Errorf("request log = %+v, want 499 at %v with waited_for=%v", req, wantLevel, wantWaited)
	}
}

func aaqFixtureQuestion(t *testing.T) {
	t.Helper()
	if !llm.NeedsTranslation(aaqGermanQuery) || llm.HasTemporalIntent(aaqGermanQuery) {
		t.Fatal("fixture question no longer takes the translation alone — the probe would test something else")
	}
}

// TestQueryAdmissionAbandonTranslationIsWarn — the translate origin's only
// slot is held; the translation queues; its client leaves.
func TestQueryAdmissionAbandonTranslationIsWarn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	aaqFixtureQuestion(t)
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)
	h := NewQueryHandler(pool, config.NewStore(bw2Config()), aaqPool(emb.srv.URL, gateHeldOrigin, ""), nil,
		blocktype.NewRegistry(), d)
	logs := installRejectLogCapture(t)
	srv, done := aaqServe(t, h, nil)

	abortAfter(t, srv.URL+"/api/query", "", `{"query":"`+aaqGermanQuery+`","synthesize":false}`, aaqQueued(t, d, gateHeldOrigin))
	waitDone(t, done)

	if err := aaqStageLine(t, logs, aaqTranslateMsg); !dispatch.AbandonedWait(err) {
		t.Errorf("translation error %v is no abandoned admission wait — the probe did not wait in the queue", err)
	}
	aaqRequestLog(t, logs, true)
}

// TestQueryAdmissionAbandonTranslationWithoutWaitStaysInfo — the client left
// before the request reached the translation's admission door: the
// translation fails at the door (a context that already ended takes no wait
// slot), nothing waited, the 499 stays INFO.
func TestQueryAdmissionAbandonTranslationWithoutWaitStaysInfo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	aaqFixtureQuestion(t)
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)
	h := NewQueryHandler(pool, config.NewStore(bw2Config()), aaqPool(emb.srv.URL, gateFreeOrigin, ""), nil,
		blocktype.NewRegistry(), d)
	logs := installRejectLogCapture(t)
	started := make(chan struct{})
	srv, done := aaqServe(t, h, func(r *http.Request) {
		close(started)
		<-r.Context().Done()
	})

	abortAfter(t, srv.URL+"/api/query", "", `{"query":"`+aaqGermanQuery+`","synthesize":false}`, func() { <-started })
	waitDone(t, done)

	err := aaqStageLine(t, logs, aaqTranslateMsg)
	if !llm.IsAdmissionError(err) || !errors.Is(err, context.Canceled) || dispatch.AbandonedWait(err) {
		t.Errorf("translation error %v, want a cancellation at the admission door, not an abandoned wait", err)
	}
	aaqRequestLog(t, logs, false)
}

// aaqRerankConfig is the bw2 request-path config with the cross-encoder on.
func aaqRerankConfig() *config.Config {
	cfg := bw2Config()
	cfg.Rerank = config.RerankConfig{Enabled: true, MaxDocs: 50, BlendWeight: 1.0}
	return cfg
}

// TestQueryAdmissionAbandonRerankIsWarn — the rerank origin's only slot is
// held; the cross-encoder call queues; its client leaves.
func TestQueryAdmissionAbandonRerankIsWarn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)
	h := NewQueryHandler(pool, config.NewStore(aaqRerankConfig()), aaqPool(emb.srv.URL, "", gateHeldOrigin), nil,
		blocktype.NewRegistry(), d)
	logs := installRejectLogCapture(t)
	srv, done := aaqServe(t, h, nil)

	abortAfter(t, srv.URL+"/api/query", "", `{"query":"`+bw2GoldenQuery+`","synthesize":false}`, aaqQueued(t, d, gateHeldOrigin))
	waitDone(t, done)

	if err := aaqStageLine(t, logs, aaqRerankMsg); !dispatch.AbandonedWait(err) {
		t.Errorf("rerank error %v is no abandoned admission wait — the probe did not wait in the queue", err)
	}
	aaqRequestLog(t, logs, true)
}

// TestQueryAdmissionAbandonRerankWithoutWaitStaysInfo — the client left while
// the request waited on a table lock between the search and the rerank (the
// graph expansion reads the dream links; nothing before it does): the
// cross-encoder call fails at its admission door, nothing waited for a slot,
// the 499 stays INFO.
func TestQueryAdmissionAbandonRerankWithoutWaitStaysInfo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	installRequestPrincipalHook(t)
	d := newGateDispatcher(t)
	pool := bw2Setup(t)
	emb := bw2NewBackend(t)
	cfg := aaqRerankConfig()
	cfg.Graph.Enabled, cfg.Graph.SeedCount = true, bw2Blocks // every result seeds the dream-link fetch
	h := NewQueryHandler(pool, config.NewStore(cfg), aaqPool(emb.srv.URL, "", gateFreeOrigin), nil,
		blocktype.NewRegistry(), d)
	logs := installRejectLogCapture(t)
	srv, done := aaqServe(t, h, nil)
	release := lockedTx(t, pool, `LOCK TABLE context_dream_links IN ACCESS EXCLUSIVE MODE`)

	abortAfter(t, srv.URL+"/api/query", "", `{"query":"`+bw2GoldenQuery+`","synthesize":false}`, func() { waitLockWaiters(t, pool, 1) })
	waitDone(t, done)
	release()

	if recs := logs.withMsg("graph expand failed; using pre-expansion results"); len(recs) != 1 {
		t.Fatalf("the client did not leave at the graph expansion: %+v", logs.all())
	}
	err := aaqStageLine(t, logs, aaqRerankMsg)
	if !llm.IsAdmissionError(err) || !errors.Is(err, context.Canceled) || dispatch.AbandonedWait(err) {
		t.Errorf("rerank error %v, want a cancellation at the admission door, not an abandoned wait", err)
	}
	aaqRequestLog(t, logs, false)
}
