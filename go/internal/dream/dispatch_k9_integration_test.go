//go:build integration

// MW11 end-to-end K9 gate through a REAL dream pipeline (design/05 A5-W5:
// dream-evaluate stands in for the five applyChainTelemetry pipelines): a
// never-admitted background acquire writes EXACTLY ONE rejection row —
// dispatch_abort='queue_full', dispatch_class='background', duration_ms
// NULL (no physical call), queue_wait_ms NOT NULL, NO prompt bodies —
// instead of the pre-MW11 wire-shaped error row the site's deferred Record
// used to persist (the MW3 gap).
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

// rejectingAdmitter rejects every acquire with a fixed error — the
// never-admitted background acquire of the K9 gate.
type rejectingAdmitter struct{ err error }

func (r rejectingAdmitter) Acquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, r.err
}

func TestDreamEvaluate_WritesK9RejectionRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "gpu", Name: "gpu", Host: "http://gpu:8089", Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleDream},
	}})
	r := &dream.Router{
		Pool:  bpool,
		Admit: llm.Admission{Admitter: rejectingAdmitter{err: dispatch.ErrQueueFull}, Class: dispatch.ClassBackground},
	}

	source := dream.BlockInfo{ID: "0198c0de-0000-7000-8000-000000000001", Title: "src", Content: "src content", Sensitivity: backends.SensInternal}
	cand := dream.BlockInfo{ID: "0198c0de-0000-7000-8000-000000000002", Title: "cand", Content: "cand content", Sensitivity: backends.SensInternal}
	if _, err := dream.EvaluateRelationships(context.Background(), pool, r, dream.DreamOptions(), source, []dream.BlockInfo{cand}); err == nil {
		t.Fatal("rejected evaluate must return its error")
	}

	waitForK9Row(t, pool, "dream-eval")
	var (
		n         int
		abort     *string
		class     *string
		durMs     *int64
		waitMs    *int64
		reqSystem *string
		reqUser   *string
		backend   *string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) OVER (), dispatch_abort, dispatch_class, duration_ms, queue_wait_ms,
		        request_system, request_user, backend_name
		 FROM context_llm_log WHERE pipeline = 'dream-eval'`,
	).Scan(&n, &abort, &class, &durMs, &waitMs, &reqSystem, &reqUser, &backend); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want EXACTLY ONE rejection line", n)
	}
	if abort == nil || *abort != "queue_full" {
		t.Errorf("dispatch_abort = %v, want queue_full", abort)
	}
	if class == nil || *class != "background" {
		t.Errorf("dispatch_class = %v, want background", class)
	}
	if durMs != nil {
		t.Errorf("duration_ms = %v, want NULL (no physical call)", *durMs)
	}
	if waitMs == nil {
		t.Error("queue_wait_ms = NULL, want the futile wait (0 is a real value, B-R4)")
	}
	if (reqSystem != nil && *reqSystem != "") || (reqUser != nil && *reqUser != "") {
		t.Error("prompt bodies survived — the K9 line must carry none (no wire-shaped error row)")
	}
	if backend == nil || *backend != "gpu" {
		t.Errorf("backend_name = %v, want the rejected target gpu", backend)
	}
}

// waitForK9Row polls until the async llmlog insert lands.
func waitForK9Row(t *testing.T, pool *pgxpool.Pool, pipeline string) {
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
