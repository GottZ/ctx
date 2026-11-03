//go:build integration

package llmlog_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRecordCountsInsertFailure is the database half of W06-1 gate (a): an
// insert the database refuses — here because context_llm_log is gone — is
// no longer a Debug line and nothing else. It moves the process-wide failure
// counter by exactly one, and Settle still returns.
//
// The counter is process-wide (one ctxd process, one number on the status
// page), so the probe measures the delta, not the absolute value.
func TestRecordCountsInsertFailure(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE context_llm_log CASCADE`); err != nil {
		t.Fatalf("drop context_llm_log: %v", err)
	}

	before := llmlog.InsertFailures()
	llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-missing-table", Model: "m", Host: "h"})
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := llmlog.Settle(sctx, pool); err != nil {
		t.Fatalf("Settle after a refused insert: %v", err)
	}
	if got := llmlog.InsertFailures() - before; got != 1 {
		t.Fatalf("InsertFailures() moved by %d after one refused insert, want 1", got)
	}
}
