//go:build integration

// MW10 integration gates (design/05 A5-W4): the dispatch-telemetry columns
// land in context_llm_log with the exact NULL semantics of §3.2 (0 is a real
// wait measurement; the K9 rejection line has duration_ms NULL), and
// migration 091 is a pure catalog operation on a chunk-populated hypertable
// (B-R6: existing rows stay byte-identical readable, re-run idempotent).
//
// External test package like evict_integration_test.go (llmlog → testdb →
// store would cycle in-package).
package llmlog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// waitForRow polls until the async llmlog.Record insert for pipeline lands.
func waitForRow(t *testing.T, pool *pgxpool.Pool, pipeline string) {
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
			t.Fatalf("row for pipeline %q never landed (async Record)", pipeline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDispatchTelemetryColumns pins the column semantics at the DB level:
// a wired row persists queue_wait_ms 0 AS 0 (B-R4 — the nullInt convention
// must not swallow immediate admissions) plus its class; the K9 rejection
// line persists dispatch_abort with duration_ms NULL (no physical call) and
// the futile wait.
func TestDispatchTelemetryColumns(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	zero := int64(0)
	llmlog.Record(pool, llmlog.Entry{
		Pipeline:      "mw10-wired",
		Model:         "m",
		Host:          "h",
		Duration:      42 * time.Millisecond,
		QueueWaitMs:   &zero,
		DispatchClass: "interactive",
	})
	waitForRow(t, pool, "mw10-wired")

	var wait *int64
	var class, abort *string
	var durMs *int64
	if err := pool.QueryRow(ctx,
		`SELECT queue_wait_ms, dispatch_class, dispatch_abort, duration_ms
		 FROM context_llm_log WHERE pipeline = 'mw10-wired'`).
		Scan(&wait, &class, &abort, &durMs); err != nil {
		t.Fatalf("scan wired row: %v", err)
	}
	if wait == nil || *wait != 0 {
		t.Fatalf("B-R4: queue_wait_ms 0 must persist as 0, not NULL — got %v", wait)
	}
	if class == nil || *class != "interactive" || abort != nil {
		t.Fatalf("wired row class/abort wrong: class=%v abort=%v", class, abort)
	}
	if durMs == nil || *durMs != 42 {
		t.Fatalf("wired row duration_ms = %v, want 42", durMs)
	}

	// K9 rejection line: futile wait persisted, duration NULL.
	futile := int64(2500)
	llmlog.Record(pool, llmlog.Entry{
		Pipeline:      "mw10-rejected",
		BackendName:   "gpu",
		Host:          "http://gpu:8089",
		Err:           errors.New("dispatch: background queue full"),
		QueueWaitMs:   &futile,
		DispatchClass: "background",
		DispatchAbort: llmlog.AbortQueueFull,
		NoWireCall:    true,
	})
	waitForRow(t, pool, "mw10-rejected")
	var backend *string
	if err := pool.QueryRow(ctx,
		`SELECT queue_wait_ms, dispatch_class, dispatch_abort, duration_ms, backend_name
		 FROM context_llm_log WHERE pipeline = 'mw10-rejected'`).
		Scan(&wait, &class, &abort, &durMs, &backend); err != nil {
		t.Fatalf("scan K9 row: %v", err)
	}
	if wait == nil || *wait != 2500 {
		t.Fatalf("K9 futile wait = %v, want 2500", wait)
	}
	if abort == nil || *abort != "queue_full" || class == nil || *class != "background" {
		t.Fatalf("K9 abort/class wrong: abort=%v class=%v", abort, class)
	}
	if durMs != nil {
		t.Fatalf("K9 line has no physical call — duration_ms must be NULL, got %d", *durMs)
	}
	if backend == nil || *backend != "gpu" {
		t.Fatalf("K9 line must attribute the rejected target, got %v", backend)
	}

	// The partial abort index exists (per-day/target counts without a
	// chunk full-scan at the 1M+ target scale).
	var idx int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_llm_log_dispatch_abort'`).Scan(&idx); err != nil {
		t.Fatalf("index probe: %v", err)
	}
	if idx != 1 {
		t.Fatalf("idx_llm_log_dispatch_abort missing (got %d)", idx)
	}
}

// TestDispatchTelemetryMigrationOnChunkedHypertable is the B-R6 probe:
// migration 091 replayed onto a chunk-populated hypertable is a catalog-only
// operation — pre-existing rows stay byte-identical readable (fingerprint
// compare), the new columns arrive NULL on them, and a re-run is idempotent.
// The test reverts 091 (drop columns + tracking row), fills three weekly
// chunks, then replays the EMBEDDED migration file — the exact bytes
// production will run.
func TestDispatchTelemetryMigrationOnChunkedHypertable(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Revert 091 so the replay hits a populated table (SetupTestDB already
	// applied everything).
	if _, err := pool.Exec(ctx, `
		DROP INDEX IF EXISTS idx_llm_log_dispatch_abort;
		ALTER TABLE context_llm_log
		    DROP COLUMN IF EXISTS queue_wait_ms,
		    DROP COLUMN IF EXISTS dispatch_class,
		    DROP COLUMN IF EXISTS dispatch_abort;
		DELETE FROM _migrations WHERE version = 91;`); err != nil {
		t.Fatalf("revert 091: %v", err)
	}

	// Populate three weekly chunks (7-day chunking since M025).
	for _, ageDays := range []int{0, 14, 30} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log
			    (created_at, pipeline, model, host, duration_ms,
			     request_system, request_user, response_content, prompt_tokens)
			 VALUES (now() - make_interval(days => $1), 'legacy', 'qwen', 'h', 123,
			         'sys', 'user', 'resp', 7)`, ageDays); err != nil {
			t.Fatalf("insert legacy row (%dd): %v", ageDays, err)
		}
	}
	var chunks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM show_chunks('context_llm_log')`).Scan(&chunks); err != nil {
		t.Fatalf("chunk count: %v", err)
	}
	if chunks < 2 {
		t.Fatalf("probe needs a chunk-populated hypertable, got %d chunks", chunks)
	}

	const fingerprintQ = `
		SELECT md5(string_agg(concat_ws('|', pipeline, model, host,
		        duration_ms::text, request_system, request_user,
		        response_content, prompt_tokens::text, created_at::text), ','
		        ORDER BY created_at))
		FROM context_llm_log`
	var before string
	if err := pool.QueryRow(ctx, fingerprintQ).Scan(&before); err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}

	// Replay the EMBEDDED migration twice, each in its own transaction like
	// the runner (idempotence: IF NOT EXISTS + ON CONFLICT).
	sql, err := migrations.Section("091_dispatch_telemetry.sql")
	if err != nil {
		t.Fatalf("read embedded 091: %v", err)
	}
	for run := 1; run <= 2; run++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin (run %d): %v", run, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("replay 091 (run %d): %v", run, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit (run %d): %v", run, err)
		}
	}

	var after string
	if err := pool.QueryRow(ctx, fingerprintQ).Scan(&after); err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	if before != after {
		t.Fatalf("existing rows changed under the migration: %s != %s", before, after)
	}
	var dirty int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_llm_log
		 WHERE queue_wait_ms IS NOT NULL OR dispatch_class IS NOT NULL OR dispatch_abort IS NOT NULL`).
		Scan(&dirty); err != nil {
		t.Fatalf("NULL probe: %v", err)
	}
	if dirty != 0 {
		t.Fatalf("additive columns must arrive NULL on existing rows, %d rows dirty", dirty)
	}
	var tracked bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = 91)`).Scan(&tracked); err != nil {
		t.Fatalf("tracking probe: %v", err)
	}
	if !tracked {
		t.Fatal("091 must record itself in _migrations")
	}
}
