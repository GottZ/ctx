//go:build integration

package llmlog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRecordingListPlans pins the cost of the recordings view's reads at the
// target scale's shape (issue #575): one recording consumer at 30 requests a
// second keeps 10^8 rows over the 90-day retention, so a list call must walk
// at most what it answers — never the consumer's line, never a sort over it,
// never the table. The table holds 3 M rows here (2 M of one consumer over
// 90 days, four more consumers with 250 k each; seeded by INSERT … SELECT,
// the only way to a table of this size in a test — the behaviour tests write
// their rows through the gateway). Each list shape is planned as the custom
// plan (the first executions) and as the generic plan (what a driver-cached
// statement runs after its fifth), under random_page_cost 1.1 and 4, and
// executed: the index scan of the walk returns at most the walk's bound plus
// one created_at group, wherever the cursor stands.
//
//	go test -tags=integration -p 1 -count=1 -run TestRecordingListPlans -v ./internal/llmlog/
func TestRecordingListPlans(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const heavy = "0190d2c4-0000-7000-8000-00000000000a"
	seed := func(consumer string, n int, step string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO context_llm_recordings
			(id, created_at, finished_at, process_id, consumer_id, path, op, stream, request_bytes, request_complete,
			 request_body, state, status, response_bytes)
			SELECT uuidv7(), ts, CASE WHEN g % 1000 = 0 THEN NULL ELSE ts + interval '1 second' END,
			       '0190d2c4-0000-7000-8000-0000000000ff'::uuid, $1::uuid, '/v1/chat/completions',
			       CASE WHEN g % 7 = 0 THEN 'embed' ELSE 'chat' END, false, 2, true, '\x7b7d'::bytea,
			       CASE WHEN g % 1000 = 0 THEN 'open' ELSE 'done' END,
			       CASE WHEN g % 1000 = 0 THEN NULL WHEN g % 50 = 0 THEN 500 WHEN g % 20 = 0 THEN 429 ELSE 200 END, 0
			  FROM generate_series(1, $2::int) g, LATERAL (SELECT now() - g * $3::interval AS ts) x`,
			consumer, n, step); err != nil {
			t.Fatalf("seed %s: %v", consumer, err)
		}
	}
	start := time.Now()
	seed(heavy, 2_000_000, "3.888 seconds")
	for i := 1; i <= 4; i++ {
		seed(fmt.Sprintf("0190d2c4-0000-7000-8000-00000000000%d", i), 250_000, "31.1 seconds")
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_llm_recordings`); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded 3 M rows in %s", time.Since(start).Round(time.Second))

	var deep RecordingCursor
	if err := pool.QueryRow(ctx, `SELECT created_at, id::text FROM context_llm_recordings WHERE consumer_id = $1::uuid
		ORDER BY created_at DESC, id DESC OFFSET 1500000 LIMIT 1`, heavy).Scan(&deep.CreatedAt, &deep.ID); err != nil {
		t.Fatal(err)
	}
	open, c5 := 0, 5
	since := deep.CreatedAt.Add(-48 * time.Hour)
	until := deep.CreatedAt
	shapes := []struct {
		name string
		q    RecordingListQuery
		scan int
	}{
		{"first_page", RecordingListQuery{ConsumerID: heavy, Limit: 50}, 51},
		{"deep_cursor", RecordingListQuery{ConsumerID: heavy, Limit: 50, Cursor: &deep}, 51},
		{"time_window", RecordingListQuery{ConsumerID: heavy, Limit: 100, Since: &since, Until: &until}, 101},
		{"rare_filter_deep", RecordingListQuery{ConsumerID: heavy, Limit: 50, Cursor: &deep, State: RecordingOpen, StatusClass: &open}, RecordingListScan},
		{"status_and_op", RecordingListQuery{ConsumerID: heavy, Limit: 50, StatusClass: &c5, Ops: []string{"embed"}}, RecordingListScan},
	}
	for _, rpc := range testdb.RandomPageCosts {
		p := testdb.WithRandomPageCost(t, pool, rpc)
		for _, sh := range shapes {
			sql, args := RecordingListSQL(sh.q, sh.scan, min(sh.q.Limit, RecordingListMax)+1)
			for kind, pc := range map[string]testdb.PlanCost{
				"custom":  testdb.ExplainCustom(t, p, sql, args...),
				"generic": testdb.ExplainGeneric(t, p, sql, args...),
			} {
				walked := checkRecordingPlan(t, fmt.Sprintf("rpc %s %s %s", rpc, sh.name, kind), pc.Plan)
				if walked > int64(sh.scan)+64 {
					t.Errorf("rpc %s %s %s: the walk read %d index rows, bound %d", rpc, sh.name, kind, walked, sh.scan)
				}
				t.Logf("rpc %s %-17s %-7s walked %6d rows, %5d buffers, %.2f ms", rpc, sh.name, kind, walked, pc.Buffers, pc.ExecMs)
			}
		}
		pc := testdb.ExplainCustom(t, p, RecordingConsumersSQL)
		checkRecordingPlan(t, "rpc "+rpc+" consumers", pc.Plan)
		t.Logf("rpc %s consumers: %d buffers, %.2f ms", rpc, pc.Buffers, pc.ExecMs)
	}

	// The statement answers what the plan promises: a deep page is the 50
	// rows after the cursor, and a rare filter goes on from where its walk
	// stopped.
	rec := Recordings{Pool: pool}
	page, err := rec.ListRecordings(ctx, RecordingListQuery{ConsumerID: heavy, Limit: 50, Cursor: &deep})
	if err != nil || len(page.Recordings) != 50 || page.Next == nil {
		t.Fatalf("deep page: %d rows, next %v, err %v", len(page.Recordings), page.Next, err)
	}
	if !page.Recordings[0].CreatedAt.Before(deep.CreatedAt) && page.Recordings[0].ID >= deep.ID {
		t.Errorf("the deep page starts at %s/%s, not after the cursor %s/%s", page.Recordings[0].CreatedAt, page.Recordings[0].ID, deep.CreatedAt, deep.ID)
	}
	rare, err := rec.ListRecordings(ctx, RecordingListQuery{ConsumerID: heavy, Limit: 50, Cursor: &deep, State: RecordingOpen, StatusClass: &open})
	if err != nil || !rare.ScanLimitReached || rare.Next == nil || rare.Scanned != RecordingListScan {
		t.Fatalf("rare filter: %d rows, scanned %d, limit reached %v, next %v, err %v", len(rare.Recordings), rare.Scanned, rare.ScanLimitReached, rare.Next, err)
	}
	for _, m := range rare.Recordings {
		if m.State != RecordingOpen || m.Status != nil {
			t.Errorf("rare filter answered a %s row with status %v", m.State, m.Status)
		}
	}
}

// checkRecordingPlan fails a plan that scans context_llm_recordings
// sequentially or sorts rows that come from it with a full Sort (an
// Incremental Sort over the index order is the allowed tie-break), and
// returns the rows the plan's scans of the table returned.
func checkRecordingPlan(t *testing.T, name, raw string) int64 {
	t.Helper()
	var root []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &root); err != nil || len(root) == 0 {
		t.Fatalf("%s: plan: %v", name, err)
	}
	var walked int64
	var walk func(n map[string]any, underSort bool)
	walk = func(n map[string]any, underSort bool) {
		typ, _ := n["Node Type"].(string)
		rel, _ := n["Relation Name"].(string)
		if rel == "context_llm_recordings" {
			if strings.Contains(typ, "Seq Scan") || strings.Contains(typ, "Bitmap") {
				t.Errorf("%s: %s on context_llm_recordings:\n%s", name, typ, raw)
			}
			if underSort {
				t.Errorf("%s: a full Sort over rows of context_llm_recordings:\n%s", name, raw)
			}
			if r, ok := n["Actual Rows"].(float64); ok {
				loops, _ := n["Actual Loops"].(float64)
				walked += int64(r * max(loops, 1))
			}
		}
		sorting := underSort || typ == "Sort"
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					// A CTE scan reads the materialized walk, not the
					// table; an init plan or a sub plan computes a value of
					// a row, its rows are not the rows sorted.
					rel, _ := m["Parent Relationship"].(string)
					walk(m, sorting && typ != "CTE Scan" && rel != "SubPlan" && rel != "InitPlan")
				}
			}
		}
	}
	walk(root[0].Plan, false)
	return walked
}
