//go:build integration

// External test package (import cycle via testdb, see evict_integration_test.go).
package llmlog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

// kw1Fixture seeds the three body classes of design/02 §3.2: a normal row,
// a credentials-slim row (bodies '' — Slimmed()), and an evicted row (bodies
// NULL — EvictBodies). Ages are distinct so keyset order is deterministic.
func kw1Fixture(t *testing.T, pool *pgxpool.Pool, withNull bool) {
	t.Helper()
	ctx := context.Background()
	ins := func(ageMin int, pipeline, sys, user, resp string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log
			    (created_at, pipeline, model, host, request_system, request_user, response_content,
			     required_sensitivity, prompt_tokens, block_ids, metadata)
			 VALUES (now() - make_interval(mins => $1), $2, 'qwen', 'h', $3, $4, $5,
			         'personal', 7, ARRAY['11111111-1111-1111-1111-111111111111']::uuid[], '{"k":1}')`,
			ageMin, pipeline, sys, user, resp); err != nil {
			t.Fatalf("insert %s: %v", pipeline, err)
		}
	}
	ins(30, "normal", "sys", "user", "resp")
	ins(20, "slim", "", "", "") // credentials-slim: '' NOT NULL
	if withNull {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log
			    (created_at, pipeline, model, host, request_system, request_user, response_content)
			 VALUES (now() - make_interval(mins => 10), 'evicted', 'qwen', 'h', NULL, NULL, NULL)`); err != nil {
			t.Fatalf("insert evicted: %v", err)
		}
	}
}

func liveColumns(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'context_llm_log'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return cols
}

// TestExportKW1 pins the KW1 gate (design/02 §7): the classifier counts ''
// and NULL both as bodyless (2× bodyless, 1 candidate), the naive
// IS NOT NULL reference query does NOT (reference proof of the trap),
// rescue-first exports everything and THEN fails, -strict aborts at once,
// the field set equals the live schema, and count(*) in the same snapshot
// equals rows_total.
func TestExportKW1(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	kw1Fixture(t, pool, false)

	t.Run("reference: naive IS NOT NULL filter lets the '' row through", func(t *testing.T) {
		var naive int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_llm_log
			 WHERE request_system IS NOT NULL OR request_user IS NOT NULL`).Scan(&naive); err != nil {
			t.Fatal(err)
		}
		// 2 = the trap: the credentials-slim row would become an empty
		// training prompt. This subtest documents WHY the classifier below
		// must treat '' as bodyless (design/02 §5.2).
		if naive != 2 {
			t.Fatalf("reference query should count 2 (normal + slim), got %d — fixture drifted", naive)
		}
	})

	t.Run("classifier: 1 candidate, 1 slim, 0 null; fields == live schema; count gate", func(t *testing.T) {
		var buf bytes.Buffer
		sum, err := llmlog.Export(ctx, pool, &buf, llmlog.ExportOptions{BatchSize: 1})
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if sum.RowsTotal != 2 || sum.RowsBody != 1 || sum.RowsBodylessSlim != 1 || sum.RowsBodylessNull != 0 {
			t.Fatalf("classifier: total=%d body=%d slim=%d null=%d",
				sum.RowsTotal, sum.RowsBody, sum.RowsBodylessSlim, sum.RowsBodylessNull)
		}
		if sum.CountGate != sum.RowsTotal {
			t.Fatalf("count gate: %d != %d", sum.CountGate, sum.RowsTotal)
		}
		if sum.Watermark == nil || sum.Watermark.IsZero() {
			t.Fatal("watermark missing")
		}
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 JSONL lines, got %d", len(lines))
		}
		// keyset order: created_at ascending → normal (30 min) before slim (20 min)
		var first map[string]json.RawMessage
		if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
			t.Fatalf("line 0 not JSON: %v", err)
		}
		if string(first["pipeline"]) != `"normal"` {
			t.Fatalf("keyset order: first row pipeline=%s", first["pipeline"])
		}
		if string(first["request_user"]) != `"user"` || string(first["metadata"]) != `{"k": 1}` {
			t.Fatalf("body/metadata not 1:1: user=%s metadata=%s", first["request_user"], first["metadata"])
		}
		keys := make([]string, 0, len(first))
		for k := range first {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if live := liveColumns(t, pool); strings.Join(keys, ",") != strings.Join(live, ",") {
			t.Fatalf("export field set != live schema\n export: %v\n live:   %v", keys, live)
		}
		// slim row: '' must be exported as "" (not null) — the classifier and
		// downstream consumers must be able to tell slim from evicted.
		var second map[string]json.RawMessage
		if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
			t.Fatal(err)
		}
		if string(second["request_system"]) != `""` {
			t.Fatalf("slim row must export '' as \"\", got %s", second["request_system"])
		}
	})

	t.Run("window: pipeline filter + until pin", func(t *testing.T) {
		var buf bytes.Buffer
		sum, err := llmlog.Export(ctx, pool, &buf, llmlog.ExportOptions{Pipelines: []string{"slim"}})
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if sum.RowsTotal != 1 || sum.RowsBodylessSlim != 1 {
			t.Fatalf("pipeline filter: total=%d slim=%d", sum.RowsTotal, sum.RowsBodylessSlim)
		}
		// until pinned before the newest row → newest excluded, count gate still holds
		buf.Reset()
		sum, err = llmlog.Export(ctx, pool, &buf, llmlog.ExportOptions{Until: time.Now().Add(-25 * time.Minute)})
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if sum.RowsTotal != 1 || sum.RowsBody != 1 || sum.CountGate != 1 {
			t.Fatalf("until pin: total=%d body=%d gate=%d", sum.RowsTotal, sum.RowsBody, sum.CountGate)
		}
	})

	// ── Rescue-first: add the evicted row.
	kw1Fixture(t, pool, true) // adds normal+slim again (different created_at) + evicted

	t.Run("rescue-first: full export, THEN error", func(t *testing.T) {
		var buf bytes.Buffer
		sum, err := llmlog.Export(ctx, pool, &buf, llmlog.ExportOptions{BatchSize: 2})
		if !errors.Is(err, llmlog.ErrBodiesEvicted) {
			t.Fatalf("expected ErrBodiesEvicted, got %v", err)
		}
		if sum.RowsTotal != 5 || sum.RowsBody != 2 || sum.RowsBodylessSlim != 2 || sum.RowsBodylessNull != 1 {
			t.Fatalf("rescue counts: total=%d body=%d slim=%d null=%d",
				sum.RowsTotal, sum.RowsBody, sum.RowsBodylessSlim, sum.RowsBodylessNull)
		}
		if got := strings.Count(buf.String(), "\n"); got != 5 {
			t.Fatalf("rescue-first must export ALL rows before failing: %d lines", got)
		}
		if sum.CountGate != 5 {
			t.Fatalf("count gate under rescue: %d", sum.CountGate)
		}
	})

	t.Run("strict: abort at first NULL", func(t *testing.T) {
		var buf bytes.Buffer
		sum, err := llmlog.Export(ctx, pool, &buf, llmlog.ExportOptions{Strict: true, BatchSize: 100})
		if !errors.Is(err, llmlog.ErrBodiesEvicted) {
			t.Fatalf("expected ErrBodiesEvicted, got %v", err)
		}
		// evicted row is the youngest → the abort happens on the last row;
		// the four older rows were already written, the NULL row is not.
		if sum.RowsBodylessNull != 1 || sum.RowsTotal != 5 {
			t.Fatalf("strict counts: total=%d null=%d", sum.RowsTotal, sum.RowsBodylessNull)
		}
		if got := strings.Count(buf.String(), "\n"); got != 4 {
			t.Fatalf("strict must stop writing at the NULL row: %d lines", got)
		}
	})
}
