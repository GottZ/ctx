//go:build integration

// MW10 end-to-end gates through the real call sites (design/05 A5-W4): the
// ChainCall funnel writes the K9 rejection row for a never-admitted
// background acquire, and the query-synthesize row carries interactive +
// queue_wait_ms 0 (a real measurement in the pass-through state, B-R4).
//
// External test package (llm_test) like synthesize_attribution — testdb →
// store → rrf → llm would cycle in-package.
package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

// rejectingAdmitter rejects every acquire with a fixed error — the
// never-admitted background acquire of the K9 gate.
type rejectingAdmitter struct{ err error }

func (r rejectingAdmitter) Acquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, r.err
}

// waitForPipelineRow polls until the async llmlog insert lands.
func waitForPipelineRow(t *testing.T, pool *pgxpool.Pool, pipeline string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, pipeline).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("row for pipeline %q never landed", pipeline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestChainCallDo_WritesK9RejectionRow drives the K9 exception through the
// funnel: a background queue_full acquire writes EXACTLY ONE telemetry row —
// dispatch_abort='queue_full', duration_ms NULL (no physical call),
// backend_name = the rejected target — while the doctrine holds (terminal,
// no wire contact, no attempt).
func TestChainCallDo_WritesK9RejectionRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "gpu", Name: "gpu", Host: "http://gpu:8089", Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleTranslate},
	}})
	adm := llm.Admission{Admitter: rejectingAdmitter{err: dispatch.ErrQueueFull}, Class: dispatch.ClassBackground}

	_, err := llm.ChainCall{
		Pool: bpool, Role: backends.RoleTranslate, Required: backends.SensInternal,
		Pipeline: "mw10-k9-funnel", System: "s", User: "u", DefTimeout: time.Second,
	}.Do(context.Background(), pool, adm)
	if !llm.IsAdmissionError(err) {
		t.Fatalf("rejected Do must stay terminal, got %v", err)
	}

	waitForPipelineRow(t, pool, "mw10-k9-funnel")
	var wait *int64
	var class, abort, backend, errCol *string
	var durMs *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT queue_wait_ms, dispatch_class, dispatch_abort, duration_ms, backend_name, error
		 FROM context_llm_log WHERE pipeline = 'mw10-k9-funnel'`).
		Scan(&wait, &class, &abort, &durMs, &backend, &errCol); err != nil {
		t.Fatalf("scan K9 row: %v", err)
	}
	if abort == nil || *abort != "queue_full" {
		t.Fatalf("dispatch_abort = %v, want queue_full", abort)
	}
	if class == nil || *class != "background" {
		t.Fatalf("dispatch_class = %v, want background", class)
	}
	if durMs != nil {
		t.Fatalf("K9 row has no physical call — duration_ms must be NULL, got %d", *durMs)
	}
	if wait == nil {
		t.Fatal("queue_wait_ms must carry the futile wait (0 is a value)")
	}
	if backend == nil || *backend != "gpu" {
		t.Fatalf("backend_name = %v, want the rejected target 'gpu'", backend)
	}
	if errCol == nil {
		t.Fatal("the rejection error must be recorded")
	}
	// Exactly one row: the doctrine's no-attempt rule means no second
	// (regular) row for the same rejected walk.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_llm_log WHERE pipeline = 'mw10-k9-funnel'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("K9 writes EXACTLY ONE line, got %d", n)
	}
}

// TestSynthesize_RowCarriesInteractiveAndWait is the A5-W4 gate probe
// "query-synthesize-Zeile trägt interactive + Wait": the interactive
// pass-through admission yields dispatch_class='interactive' and
// queue_wait_ms=0 persisted AS 0 (B-R4), abort NULL, wait-free duration set.
func TestSynthesize_RowCarriesInteractiveAndWait(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"An answer."}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`)
	}))
	defer srv.Close()

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "wire", Name: "wire", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
		// NumCtx is DECLARED (H12): the prompt budget resolves over the chain,
		// so a synthesis member without a window refuses the prompt before this
		// test reaches the dispatch telemetry it exists to measure.
		NumCtx: 8192,
		Trust:  backends.TrustFull, Enabled: true, Roles: []string{backends.RoleSynthesis},
	}})

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	// MW4: interactive binds via the request ctx (llm.PrincipalCtxForTest),
	// never via an Admission field.
	adm := llm.Admission{Admitter: d, Class: dispatch.ClassInteractive}

	settings := llm.SynthesisSettings{ScoreThreshold: 0.001, ConfidentThreshold: 0.008, PromptVersion: llm.PromptVersionV52}
	sources := []llm.Source{{ID: "00000000-0000-0000-0000-000000000001", Title: "t", Category: "c", Content: "body", Score: 0.5, AgeDays: 1}}
	if _, err := llm.Synthesize(llm.PrincipalCtxForTest(), pool, bpool, nil,
		settings, backends.SensPersonal, "q", sources, nil, "", "", adm); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	waitForPipelineRow(t, pool, "query-synthesize")
	var wait *int64
	var class, abort *string
	var durMs *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT queue_wait_ms, dispatch_class, dispatch_abort, duration_ms
		 FROM context_llm_log WHERE pipeline = 'query-synthesize'`).
		Scan(&wait, &class, &abort, &durMs); err != nil {
		t.Fatalf("scan synthesize row: %v", err)
	}
	if class == nil || *class != "interactive" {
		t.Fatalf("dispatch_class = %v, want interactive", class)
	}
	if wait == nil {
		t.Fatal("B-R4: pass-through admission must persist queue_wait_ms 0, not NULL")
	}
	if *wait != 0 {
		t.Fatalf("pass-through wait = %d, want 0", *wait)
	}
	if abort != nil {
		t.Fatalf("class invariant: interactive row must never carry dispatch_abort, got %q", *abort)
	}
	if durMs == nil {
		t.Fatal("duration_ms must carry the wire attempt's elapsed")
	}
}
