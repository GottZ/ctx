//go:build integration

// T7 EXPLAIN gate (design/01 §3.6/§6.4/§7-T7, G39 bench discipline): the
// guard pick AND both state-count subqueries must ride idx_guard_pending
// (M074 partial index) — no Seq Scan over context_blocks. Pre-M074 the pick
// predicate had NO carrying index (GIN(metadata) jsonb_ops does not serve
// `->> IS NULL`; idx_context_created only carries the ORDER BY): at 1M+
// blocks that was 2-3 full scans per batch.
//
// In-package test on purpose: it EXPLAINs the ACTUAL guardPendingWhere
// fragment the production queries embed — no SQL duplicated into the test.
//
// Synthesis corpus (1M-SHAPED, size documented in the wave return): the
// partial index only CONTAINS pending rows, so the planner-relevant shape is
// "huge table, small pending set" — bulk rows are cheap (embedding NULL =
// outside the index predicate; a checked-stamp variant is unnecessary), the
// pending set carries real vectors. Default 1M bulk + 5k pending; override
// via CTX_T7_EXPLAIN_BULK_N / CTX_T7_EXPLAIN_PENDING_N. The live-1M
// measurement on the G39 corpus stays integrator scope.
package guard

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestT7ExplainGuardPendingIndex(t *testing.T) {
	bulkN, pendingN := 1_000_000, 5_000
	if v := os.Getenv("CTX_T7_EXPLAIN_BULK_N"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			bulkN = p
		}
	}
	if v := os.Getenv("CTX_T7_EXPLAIN_PENDING_N"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			pendingN = p
		}
	}

	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	seedStart := time.Now()
	// Bulk load with user triggers OFF: context_blocks carries three NOTIFY
	// row triggers, and NOTIFY's per-tx duplicate check is O(n²) over the
	// pending list — 1M distinct payloads turn the seed quadratic (observed:
	// >17min without finishing). Triggers do not influence planner shape,
	// which is the only thing this gate measures; re-enabled right after.
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	// Bulk: 1M-shaped table mass OUTSIDE the partial index (embedding NULL).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f2209-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'bulk-' || i, 'x', 'private'
		FROM generate_series(0, $1::int - 1) AS g(i)`, bulkN); err != nil {
		t.Fatalf("seed bulk: %v", err)
	}
	// Pending: rows INSIDE idx_guard_pending (embedding set, unchecked). One
	// shared vector — the btree partial index under test never reads it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, embedding)
		SELECT ('019f220a-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'pending-' || i, 'x', 'private',
		       (SELECT ('[1' || repeat(',0', 1023) || ']')::vector(1024))
		FROM generate_series(0, $1::int - 1) AS g(i)`, pendingN); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER USER`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("seeded %d bulk + %d pending rows in %s", bulkN, pendingN, time.Since(seedStart).Round(time.Millisecond))

	types := []string{"audit-trail", "knowledge", "reference", "system-meta"}

	explain := func(name, q string, args ...any) string {
		t.Helper()
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", name, err)
		}
		plan := b.String()
		t.Logf("%s plan:\n%s", name, plan)
		if !strings.Contains(plan, "idx_guard_pending") {
			t.Errorf("%s: plan does not use idx_guard_pending", name)
		}
		if strings.Contains(plan, "Seq Scan on context_blocks") {
			t.Errorf("%s: plan seq-scans context_blocks", name)
		}
		return plan
	}

	// THE three production queries, built from the ACTUAL shared fragment.
	// Pick (without FOR UPDATE — EXPLAIN ANALYZE would lock/skip rows; the
	// LockRows node sits above the scan and does not change index choice).
	explain("guard-pick",
		`SELECT id, type_name FROM context_blocks
		WHERE `+guardPendingWhere("$1")+`
		ORDER BY created_at ASC
		LIMIT $2`, types, 100)
	explain("state-count",
		`SELECT count(*) FROM context_blocks WHERE `+guardPendingWhere("$1"), types)
	explain("state-count-int",
		`SELECT count(*)::int FROM context_blocks WHERE `+guardPendingWhere("$1"), types)
}
