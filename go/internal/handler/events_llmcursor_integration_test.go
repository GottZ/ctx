//go:build integration

// S14 — the SSE telemetry cursor is the TUPLE (created_at, id).
//
// Ist-Stand before this wave: the broadcast loop carried max(created_at) and
// asked for `created_at > $1` (events.go:407-421 + :438-440). context_llm_log
// has PK (id, created_at) with gen_random_uuid() (025_llm_log.sql:9,24) — no
// monotonic secondary key — so created_at is not a total order. Two rows
// sharing one timestamp tie; when the LIMIT cut fell between them the cursor
// advanced past BOTH and the second row was never delivered on any later tick.
// Measured against the unchanged code, the two-tied-rows / LIMIT 1 fixture
// below delivered 1 of 2 distinct rows.
//
// The probes carry their brief letters (design 05 §7, row S14):
//
//	a  Tie         — two rows, identical created_at, LIMIT 1 ⇒ both arrive
//	                 across two ticks, in (created_at, id) order
//	b  NoDouble    — replaying the cursor never delivers a row twice
//	c  Plan        — context_llm_log_created_at_idx stays the leading access,
//	                 and chunk exclusion survives the row comparison
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestS14LLMCallCursor -count=1 -v
package handler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// s14ChunkIndex is the per-chunk name TimescaleDB gives the hypertable's
// created_at index. Spelled out here on purpose: the gate must not read the
// name from the code it guards.
const s14ChunkIndex = "context_llm_log_created_at_idx"

func TestS14LLMCallCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Run("a_tie_survives_the_limit_cut", func(t *testing.T) {
		s14Truncate(t, ctx, pool)
		start := time.Now().UTC().Add(-time.Hour)
		tie := start.Add(time.Minute)

		// Deliberately seeded in DESCENDING id order: if the query relied on
		// physical order rather than ORDER BY id, the delivery order below
		// would come out reversed.
		want := []string{
			"019f0000-0000-7000-8000-0000000000aa",
			"019f0000-0000-7000-8000-0000000000bb",
			"019f0000-0000-7000-8000-0000000000cc",
		}
		for i := len(want) - 1; i >= 0; i-- {
			s14Seed(t, ctx, pool, want[i], tie)
		}

		// Drive the cursor exactly the way runLoop does — one page per tick,
		// LIMIT 1, so every tick's cut lands INSIDE the tie group.
		var got []string
		cur := newLLMCursor(start)
		for tick := 0; tick < len(want)+2; tick++ {
			rows, next := fetchLLMCalls(ctx, pool, cur, 1)
			for _, r := range rows {
				got = append(got, r.ID)
			}
			t.Logf("tick %d: cursor=%s rows=%d next=%s", tick, cur, len(rows), next)
			cur = next
		}

		if len(got) != len(want) {
			t.Fatalf("delivered %d rows, want %d — a tied row was lost at the LIMIT cut (got=%v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("delivery order[%d] = %s, want %s (rows must arrive in (created_at, id) order)", i, got[i], want[i])
			}
		}
	})

	t.Run("b_no_row_is_delivered_twice", func(t *testing.T) {
		s14Truncate(t, ctx, pool)
		start := time.Now().UTC().Add(-time.Hour)

		// 40 rows over 4 timestamps: 10-way ties, so pages of 3 cut inside a
		// tie group repeatedly. This is where a `>=` cursor double-delivers.
		var ids []string
		for g := 0; g < 4; g++ {
			at := start.Add(time.Duration(g+1) * time.Minute)
			for i := 0; i < 10; i++ {
				id := fmt.Sprintf("019f0000-0000-7000-8000-%012d", g*100+i)
				s14Seed(t, ctx, pool, id, at)
				ids = append(ids, id)
			}
		}

		seen := map[string]int{}
		var order []string
		cur := newLLMCursor(start)
		for tick := 0; tick < 40; tick++ {
			rows, next := fetchLLMCalls(ctx, pool, cur, 3)
			for _, r := range rows {
				seen[r.ID]++
				order = append(order, r.ID)
			}
			cur = next
			if len(rows) == 0 {
				break
			}
		}

		for _, id := range ids {
			switch seen[id] {
			case 1:
			case 0:
				t.Errorf("row %s never delivered", id)
			default:
				t.Errorf("row %s delivered %d times, want exactly 1", id, seen[id])
			}
		}
		if len(order) != len(ids) {
			t.Errorf("delivered %d rows in total, want %d", len(order), len(ids))
		}
		if !sort.SliceIsSorted(order, func(i, j int) bool { return order[i] < order[j] }) {
			t.Errorf("delivery order is not monotonic in (created_at, id): %v", order)
		}
	})

	t.Run("c_created_at_index_stays_leading", func(t *testing.T) {
		s14Truncate(t, ctx, pool)
		// 60k rows over 7 weeks ⇒ 7 chunks, so chunk exclusion is measurable
		// and a Seq Scan is no longer the cheapest plan.
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_log (created_at, pipeline, model, host)
			SELECT now() - (i || ' minutes')::interval, 'query-synthesize', 'qwen3.5:9b', 'local'
			FROM generate_series(0, 59999) AS g(i)`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := pool.Exec(ctx, `ANALYZE context_llm_log`); err != nil {
			t.Fatalf("analyze: %v", err)
		}
		var chunks int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM timescaledb_information.chunks
			WHERE hypertable_name = 'context_llm_log'`).Scan(&chunks); err != nil {
			t.Fatalf("count chunks: %v", err)
		}
		if chunks < 2 {
			t.Fatalf("fixture built %d chunk(s) — chunk exclusion is not measurable below 2", chunks)
		}

		// EXPLAIN the SHIPPED statement, not a copy of it.
		plan := s14Explain(t, ctx, pool, llmcallCursorSQL,
			time.Now().UTC().Add(-30*time.Hour), llmCursorZeroID, 200)

		if !strings.Contains(plan, s14ChunkIndex) {
			t.Errorf("plan does not name %s — the created_at index is no longer the leading access:\n%s", s14ChunkIndex, plan)
		}
		if strings.Contains(plan, "Seq Scan") {
			t.Errorf("plan contains a Seq Scan:\n%s", plan)
		}
		// The tuple order is (created_at, id) while the index carries only
		// created_at: an Incremental Sort is the honest cost of the tiebreaker.
		// A FULL Sort would mean the index stopped ordering the scan.
		if strings.Contains(plan, "Sort Key") && !strings.Contains(plan, "Presorted Key: ") {
			t.Errorf("plan sorts without a presorted key — the index no longer orders the scan:\n%s", plan)
		}
		// Chunk exclusion: only the chunks the cursor window actually overlaps
		// may be opened. That count is 1 on most days and 2 when the 30 h
		// window straddles a chunk boundary (TimescaleDB chunks are
		// epoch-aligned 7-day ranges — a hardcoded `want 1` was red every
		// Thursday). Compute the expectation from the chunk catalog instead:
		// a chunk is opened iff its range_end lies after the window start.
		// Dropping `created_at >= $1` from the statement still plans a
		// ChunkAppend across EVERY chunk, so the exclusion probe stays sharp.
		var expectChunks int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM timescaledb_information.chunks
			WHERE hypertable_name = 'context_llm_log' AND range_end > $1`,
			time.Now().UTC().Add(-30*time.Hour)).Scan(&expectChunks); err != nil {
			t.Fatalf("count window chunks: %v", err)
		}
		if expectChunks >= chunks {
			t.Fatalf("window overlaps all %d chunks — exclusion would be unmeasurable (fixture too shallow)", chunks)
		}
		if n := strings.Count(plan, s14ChunkIndex); n != expectChunks {
			t.Errorf("plan opens %d chunks, want %d (window-overlapped) — chunk exclusion was lost:\n%s", n, expectChunks, plan)
		}
	})
}

// s14Truncate empties the hypertable between sub-probes.
func s14Truncate(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `TRUNCATE context_llm_log`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// s14Seed writes one telemetry row at an EXPLICIT (id, created_at).
func s14Seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log (id, created_at, pipeline, model, host)
		VALUES ($1::uuid, $2, 'query-synthesize', 'qwen3.5:9b', 'local')`, id, at); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// s14Explain runs EXPLAIN (ANALYZE, BUFFERS) and returns the plan text.
func s14Explain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	t.Logf("plan:\n%s", b.String())
	return b.String()
}
