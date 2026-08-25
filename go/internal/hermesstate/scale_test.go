// scale_test.go — the target-scale gate. The live stores hold tens of thousands
// of rows today; the plan this package pins is a promise about a store with
// millions, and a promise measured at 1 329 rows is not measured at all. So the
// fixture is built at the size the promise is made for, and the degraded plan
// is shown failing against the same file before the pinned one is shown passing.
//
// Source: https://github.com/GottZ/ctx
package hermesstate_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/hermesstate"
)

const (
	// scaleRows is the target-scale row count of design §7 (W03-2).
	scaleRows = 2_000_000
	// scaleSessions interleave over one global id space, as they do live.
	scaleSessions = 12
	// scaleBatchBudget is the per-batch latency ceiling of the gate.
	scaleBatchBudget = 50 * time.Millisecond
)

// scaleSessionID names the n-th fixture session.
func scaleSessionID(n int) string { return fmt.Sprintf("2026082%d_scale_%02d", n%10, n) }

// buildScale writes a store of scaleRows rows over scaleSessions interleaved
// sessions, built from upstream SCHEMA_SQL WITHOUT the (session_id, id) index —
// the shape in which the naive phrasing degrades.
//
// Row mix per session: the first 80 % of a session's rows are archived
// (compacted=1, active=0), the rest are live; two of every five rows are tool
// results. Contents are short on purpose — the gate measures the access path,
// and a fat payload would measure the page cache instead.
func buildScale(t *testing.T, path string) time.Duration {
	t.Helper()
	start := time.Now()

	db := writable(t, path, "_pragma=journal_mode(off)", "_pragma=synchronous(off)", "_pragma=cache_size(-65536)")
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close scale fixture: %v", err)
		}
	}()
	mustExec(t, db, schemaSQL(t, false))

	perSession := scaleRows / scaleSessions
	archivedBelow := perSession * 4 / 5
	for n := range scaleSessions {
		insertSession(t, db, scaleSessionID(n), "", istBaseTS)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	const chunk = 500
	ins, err := tx.Prepare(bulkInsertSQL(chunk))
	if err != nil {
		t.Fatalf("prepare bulk: %v", err)
	}

	args := make([]any, 0, chunk*7)
	rank := make([]int, scaleSessions)
	base := float64(istBaseTS.Unix())
	for id := 1; id <= scaleRows; id++ {
		s := id % scaleSessions
		rank[s]++
		role, tool := "assistant", any(nil)
		if id%5 < 2 {
			role, tool = "tool", any(fmt.Sprintf("tool_%d", id%9))
		}
		active, compacted := 1, 0
		if rank[s] <= archivedBelow {
			active, compacted = 0, 1
		}
		args = append(args, id, scaleSessionID(s), role,
			fmt.Sprintf("row %d", id), tool, base+float64(id), active, compacted)
		if len(args)/8 == chunk {
			if _, err := ins.Exec(args...); err != nil {
				t.Fatalf("bulk insert at id %d: %v", id, err)
			}
			args = args[:0]
		}
	}
	if len(args) > 0 {
		if _, err := tx.Exec(bulkInsertSQL(len(args)/8), args...); err != nil {
			t.Fatalf("bulk tail: %v", err)
		}
	}
	if err := ins.Close(); err != nil {
		t.Fatalf("close bulk stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return time.Since(start)
}

func bulkInsertSQL(rows int) string {
	var b strings.Builder
	b.WriteString(`INSERT INTO messages
		(id, session_id, role, content, tool_name, timestamp, active, compacted) VALUES `)
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,?,?,?,?)")
	}
	return b.String()
}

// TestScaleBatchLatency is the W8 gate: at target scale, a batch read stays
// under the budget, and the degraded plan is shown to be a real risk on the
// very same file rather than a hypothetical one.
func TestScaleBatchLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("scale fixture: builds a multi-million-row store")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	buildTime := buildScale(t, path)
	t.Logf("scale fixture: %d rows over %d sessions built in %s", scaleRows, scaleSessions, buildTime.Round(time.Millisecond))

	if got := countRows(t, path, "1 = 1"); got != scaleRows {
		t.Fatalf("fixture holds %d rows, want %d", got, scaleRows)
	}

	// ROT first: the natural phrasing, without the strategy hint, against this
	// exact file. If this ever stops being slow the pinned plan has become
	// unnecessary — and the gate should say so rather than keep guarding.
	degraded := explainRaw(t, path, `SELECT id, tool_name, content, timestamp FROM messages
		 WHERE session_id = ? AND compacted = 1 AND role = 'tool' AND id > ?
		 ORDER BY id ASC LIMIT ?`, scaleSessionID(3), 0, 400)
	t.Logf("unhinted plan: %s", degraded)
	if !strings.Contains(degraded, "idx_messages_session ") {
		t.Errorf("unhinted plan does not use idx_messages_session: %s", degraded)
	}
	if !strings.Contains(strings.ToUpper(degraded), "USE TEMP B-TREE") {
		t.Errorf("unhinted plan does not materialise a sort: %s", degraded)
	}

	src, err := hermesstate.Open(path, "scale", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()
	if src.Strategy() != hermesstate.StrategyRowidRange {
		t.Fatalf("strategy = %q, want %q on a store without the (session_id, id) index",
			src.Strategy(), hermesstate.StrategyRowidRange)
	}
	t.Logf("pinned plan: %s", src.Plan())

	ctx := t.Context()
	session := scaleSessionID(3)
	head, err := src.MaxCompactedID(ctx, session)
	if err != nil {
		t.Fatalf("MaxCompactedID: %v", err)
	}

	// Batches spread over the whole id space of the session: the first one
	// starts at the very beginning, the last one deep inside the file.
	const batches = 20
	var worst time.Duration
	var worstAt int64
	total := 0
	for i := range batches {
		after := head * int64(i) / int64(batches)
		t0 := time.Now()
		rows, dropped, err := src.ToolRows(ctx, session, after, 400)
		d := time.Since(t0)
		if err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		if dropped != 0 {
			t.Errorf("batch %d dropped %d rows", i, dropped)
		}
		if len(rows) == 0 {
			t.Errorf("batch %d after id %d returned nothing", i, after)
		}
		for _, r := range rows {
			if r.ID <= after || r.ID > head {
				t.Fatalf("batch %d returned id %d outside (%d, %d]", i, r.ID, after, head)
			}
		}
		total += len(rows)
		if d > worst {
			worst, worstAt = d, after
		}
	}
	t.Logf("%d batches, %d rows, worst batch %s (after id %d), budget %s",
		batches, total, worst.Round(time.Microsecond), worstAt, scaleBatchBudget)
	if worst > scaleBatchBudget {
		t.Errorf("worst batch %s exceeds the %s budget", worst, scaleBatchBudget)
	}

	// The per-tick pre-gates must stay cheap at this size too.
	t0 := time.Now()
	if _, err := src.GlobalMaxID(ctx); err != nil {
		t.Fatalf("GlobalMaxID: %v", err)
	}
	globalMax := time.Since(t0)
	t0 = time.Now()
	if _, err := src.HasNewArchived(ctx, session, head-1); err != nil {
		t.Fatalf("HasNewArchived: %v", err)
	}
	probe := time.Since(t0)
	t.Logf("GlobalMaxID %s, HasNewArchived %s", globalMax.Round(time.Microsecond), probe.Round(time.Microsecond))
	if globalMax > scaleBatchBudget || probe > scaleBatchBudget {
		t.Errorf("pre-gates exceed the %s budget: GlobalMaxID %s, HasNewArchived %s", scaleBatchBudget, globalMax, probe)
	}

	// The red state, measured rather than described: the same batch, same file,
	// same warm cache — phrased without the hint.
	degradedAt := head * 19 / 20
	slow := timeRaw(t, path, `SELECT id, tool_name, content, timestamp FROM messages
		 WHERE session_id = ? AND compacted = 1 AND role = 'tool' AND id > ?
		 ORDER BY id ASC LIMIT ?`, session, degradedAt, 400)
	t.Logf("unhinted batch after id %d: %s (pinned worst %s)", degradedAt, slow.Round(time.Microsecond), worst.Round(time.Microsecond))
	if slow <= worst {
		t.Errorf("the unhinted phrasing (%s) is not slower than the pinned one (%s) — the gate has stopped measuring anything", slow, worst)
	}
}

// timeRaw runs a query through a plain handle and reports how long draining it
// took. Used only to put a number on the degraded plan.
func timeRaw(t *testing.T, path, query string, args ...any) time.Duration {
	t.Helper()
	db := writable(t, path)
	defer func() { _ = db.Close() }()
	// One warm-up, so the measurement is not a page-cache artefact in either
	// direction: the pinned batches it is compared against ran warm too.
	for range 2 {
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatalf("raw query: %v", err)
		}
		for rows.Next() { //nolint:revive // draining is the measurement
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("raw rows: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close raw rows: %v", err)
		}
	}
	t0 := time.Now()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	for rows.Next() { //nolint:revive // draining is the measurement
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("raw rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close raw rows: %v", err)
	}
	return time.Since(t0)
}

// TestScaleStrategyChoiceWithIndex is the other branch at scale: the same
// fixture with the (session_id, id) index added afterwards — exactly how both
// live stores acquired it — must land on index-seek.
func TestScaleStrategyChoiceWithIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("scale fixture: builds a multi-million-row store")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	buildTime := buildScale(t, path)

	db := writable(t, path)
	t0 := time.Now()
	mustExec(t, db, sessionIDIndexLine)
	indexTime := time.Since(t0)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Logf("fixture built in %s, index added in %s", buildTime.Round(time.Millisecond), indexTime.Round(time.Millisecond))

	src, err := hermesstate.Open(path, "scale-idx", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()
	if src.Strategy() != hermesstate.StrategyIndexSeek {
		t.Fatalf("strategy = %q, want %q", src.Strategy(), hermesstate.StrategyIndexSeek)
	}
	if !strings.Contains(src.Plan(), "idx_messages_session_id") {
		t.Errorf("plan = %q, want it to name idx_messages_session_id", src.Plan())
	}
	t.Logf("pinned plan: %s", src.Plan())

	ctx := t.Context()
	session := scaleSessionID(3)
	head, err := src.MaxCompactedID(ctx, session)
	if err != nil {
		t.Fatalf("MaxCompactedID: %v", err)
	}
	var worst time.Duration
	for i := range 20 {
		after := head * int64(i) / 20
		t0 := time.Now()
		if _, _, err := src.ToolRows(ctx, session, after, 400); err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	t.Logf("index-seek worst batch %s, budget %s", worst.Round(time.Microsecond), scaleBatchBudget)
	if worst > scaleBatchBudget {
		t.Errorf("worst batch %s exceeds the %s budget", worst, scaleBatchBudget)
	}
}

// explainRaw runs EXPLAIN QUERY PLAN through a plain handle, so the negative
// probe does not depend on the package it is probing.
func explainRaw(t *testing.T, path, query string, args ...any) string {
	t.Helper()
	db := writable(t, path)
	defer func() { _ = db.Close() }()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var a, b, c int
		var detail sql.NullString
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		details = append(details, detail.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return strings.Join(details, " | ")
}
