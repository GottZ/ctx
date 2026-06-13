//go:build integration

// External test package: llmlog → (via the testdb helper) store → … → llmlog
// would be an import cycle for an in-package test, so this lives in llmlog_test.
package llmlog_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestEvictBodies pins the masterplan-E4 retention contract: after the
// retention window the prompt/response BODIES are NULLed while the telemetry
// row survives (lossless egress audit), recent rows stay untouched, and a
// re-run is idempotent (IS NOT NULL guard). retention=0 is a no-op (unlimited).
func TestEvictBodies(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insert := func(ageDays int, pipeline string) {
		t.Helper()
		_, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log
			    (created_at, pipeline, model, host,
			     request_system, request_user, response_content,
			     block_ids, backend_name, prompt_tokens)
			 VALUES (now() - make_interval(days => $1), $2, 'qwen', 'h',
			         'sys', 'user', 'resp',
			         ARRAY['11111111-1111-1111-1111-111111111111']::uuid[],
			         'herbert-chat', 42)`,
			ageDays, pipeline)
		if err != nil {
			t.Fatalf("insert (%dd): %v", ageDays, err)
		}
	}
	insert(101, "old")  // older than the 90-day window
	insert(1, "recent") // inside the window

	// Evict at 90 days: only the old row's bodies go.
	n, err := llmlog.EvictBodies(ctx, pool, 90)
	if err != nil {
		t.Fatalf("EvictBodies: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row evicted, got %d", n)
	}

	// Old row: bodies NULL, telemetry intact (the audit must survive).
	var sys, user, resp *string
	var model, backend string
	var tokens, blockCount int
	if err := pool.QueryRow(ctx,
		`SELECT request_system, request_user, response_content, model,
		        backend_name, prompt_tokens, array_length(block_ids, 1)
		 FROM context_llm_log WHERE pipeline = 'old'`).
		Scan(&sys, &user, &resp, &model, &backend, &tokens, &blockCount); err != nil {
		t.Fatalf("scan old: %v", err)
	}
	if sys != nil || user != nil || resp != nil {
		t.Errorf("old row bodies must be NULL, got sys=%v user=%v resp=%v", sys, user, resp)
	}
	if model != "qwen" || backend != "herbert-chat" || tokens != 42 || blockCount != 1 {
		t.Errorf("old row telemetry must survive: model=%q backend=%q tokens=%d blocks=%d",
			model, backend, tokens, blockCount)
	}

	// Recent row: bodies untouched.
	var rsys *string
	if err := pool.QueryRow(ctx,
		`SELECT request_system FROM context_llm_log WHERE pipeline = 'recent'`).
		Scan(&rsys); err != nil {
		t.Fatalf("scan recent: %v", err)
	}
	if rsys == nil || *rsys != "sys" {
		t.Errorf("recent row body must survive, got %v", rsys)
	}

	// Idempotent: a re-run evicts nothing (the IS NOT NULL guard skips the
	// already-NULLed old row — no needless chunk rewrite).
	n2, err := llmlog.EvictBodies(ctx, pool, 90)
	if err != nil {
		t.Fatalf("EvictBodies re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("re-run must be idempotent, got %d rows", n2)
	}

	// retention=0 disables eviction (unlimited): the recent body stays.
	n3, err := llmlog.EvictBodies(ctx, pool, 0)
	if err != nil {
		t.Fatalf("EvictBodies(0): %v", err)
	}
	if n3 != 0 {
		t.Errorf("retention=0 must be a no-op, got %d rows", n3)
	}
}
