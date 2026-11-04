//go:build integration

// W06-9 database gates (design/06 §4.6, §7 W06-9) over real inserts:
//
//	(a) RecordGateway with a consumer and a model key ⇒ consumer_id = C,
//	    api_key_id NULL, metadata.gw.model_key_id = K; the rollup row is
//	    source 'gateway' with the alias; a knowledge key the caller left on
//	    the entry is cleared before the insert, and an entry that carries it
//	    anyway is refused by ck_llm_log_one_identity (23514, InsertFailures)
//	(c) two attempts (502 → success) ⇒ ONE row, attempt = 2, two
//	    metadata.chain entries
//	(f) volume probe (opt-in, CTX_W069_VOLUME_ROWS): growth of
//	    hypertable_detailed_size('context_llm_log') per RecordGateway row,
//	    heap and index separately
//
//	go test -tags=integration -p 1 -count=1 -run 'TestRecordGateway|TestGatewayRow' -v ./internal/llmlog/
//	CTX_W069_VOLUME_ROWS=10000 go test -tags=integration -p 1 -count=1 -run TestRecordGatewayVolumeProbe -v ./internal/llmlog/
package llmlog_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	gwiConsumer = "0195a6f0-0000-7000-8000-00000000c911"
	gwiModelKey = "0195a6f0-0000-7000-8000-00000000d911"
	gwiKnowKey  = "0195a6f0-0000-7000-8000-00000000a911"
	gwiAlias    = "team-chat"
)

// gwiRow is the row W02-07 hands over for one non-streamed chat request.
func gwiRow(pipeline string) llmlog.GatewayRow {
	qw := int64(4)
	return llmlog.GatewayRow{
		Pipeline:   pipeline,
		ConsumerID: gwiConsumer,
		Gateway: llmlog.Gateway{
			Alias: gwiAlias, Hop: 1, Usage: llmlog.UsageBackend,
			Finish: llmlog.FinishStop, ModelKeyID: gwiModelKey, ClientReqID: "client-req-1",
		},
		Telemetry: llmlog.Entry{
			Model: "served-model", Host: "spark", Duration: 800 * time.Millisecond,
			PromptTokens: 120, CompletionTokens: 40,
			BackendName: "spark-chat", BackendTrust: "full-trust", BackendLocality: "lan",
			RequiredSensitivity: "internal", QueueWaitMs: &qw, DispatchClass: "external",
		},
		Attempts:    []llmlog.WireAttempt{{Backend: "spark-chat", Ms: 800, WaitMs: 4}},
		BodyLogging: llmlog.BodyLoggingNone,
	}
}

func gwiSettle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := llmlog.Settle(ctx, pool); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

// TestRecordGatewayIdentity is gate (a): the identity contract of a /v1
// row and its rollup row (Finding 1/24, §8 d01 (3)).
func TestRecordGatewayIdentity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fails := llmlog.InsertFailures()

	llmlog.RecordGateway(pool, gwiRow(llmlog.PipelineGatewayChat))
	gwiSettle(t, pool)
	if d := llmlog.InsertFailures() - fails; d != 0 {
		t.Fatalf("InsertFailures +%d, want 0", d)
	}
	var (
		consumer, key, mk, alias, op, pipeline *string
		attempt                                *int16
		consumerKey                            bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT consumer_id::text, api_key_id::text, metadata->'gw'->>'model_key_id',
		       metadata->'gw'->>'alias', metadata->'gw'->>'op', pipeline, attempt,
		       metadata ? 'consumer_id'
		  FROM context_llm_log`).Scan(&consumer, &key, &mk, &alias, &op, &pipeline, &attempt, &consumerKey); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if deref(consumer) != gwiConsumer || key != nil || deref(mk) != gwiModelKey {
		t.Fatalf("consumer_id=%v api_key_id=%v gw.model_key_id=%v, want %s / NULL / %s",
			deref(consumer), deref(key), deref(mk), gwiConsumer, gwiModelKey)
	}
	if deref(alias) != gwiAlias || deref(op) != "chat" || deref(pipeline) != llmlog.PipelineGatewayChat || attempt == nil || *attempt != 1 || consumerKey {
		t.Fatalf("alias=%v op=%v pipeline=%v attempt=%v metadata.consumer_id present=%v", deref(alias), deref(op), deref(pipeline), attempt, consumerKey)
	}

	var source, modelAlias string
	var calls int64
	var rollupKey *string
	if err := pool.QueryRow(ctx, `
		SELECT source, model_alias, calls, api_key_id::text
		  FROM context_llm_usage_hourly WHERE consumer_id = $1`, gwiConsumer).Scan(&source, &modelAlias, &calls, &rollupKey); err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	if source != "gateway" || modelAlias != gwiAlias || calls != 1 || rollupKey != nil {
		t.Fatalf("rollup source=%q model_alias=%q calls=%d api_key_id=%v, want gateway/%s/1/NULL", source, modelAlias, calls, deref(rollupKey), gwiAlias)
	}
}

// TestRecordGatewayClearsKnowledgeKey is the code side of gate (a): a
// knowledge key the walk left on the telemetry entry is cleared by the
// stamp (B17), so the row lands — attributed to the consumer only. The
// mutation "the stamp keeps APIKeyID" turns this red with 23514.
func TestRecordGatewayClearsKnowledgeKey(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	fails := llmlog.InsertFailures()
	r := gwiRow(llmlog.PipelineGatewayChat)
	r.Telemetry.APIKeyID = gwiKnowKey
	llmlog.RecordGateway(pool, r)
	gwiSettle(t, pool)
	if d := llmlog.InsertFailures() - fails; d != 0 {
		t.Fatalf("InsertFailures +%d — the row carried both identities and the CHECK refused it", d)
	}
	var consumer, key *string
	if err := pool.QueryRow(context.Background(),
		`SELECT consumer_id::text, api_key_id::text FROM context_llm_log`).Scan(&consumer, &key); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if deref(consumer) != gwiConsumer || key != nil {
		t.Fatalf("consumer_id=%v api_key_id=%v, want %s / NULL", deref(consumer), deref(key), gwiConsumer)
	}
}

// TestGatewayRowWithKnowledgeKeyIsRefused is the database backstop of gate
// (a): an entry that carries both identities — what any path around the
// stamp would write — is refused by ck_llm_log_one_identity with SQLSTATE
// 23514 and counted in InsertFailures; no row lands.
func TestGatewayRowWithKnowledgeKeyIsRefused(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fails := llmlog.InsertFailures()
	e := llmlog.GatewayEntry(gwiRow(llmlog.PipelineGatewayChat))
	e.APIKeyID = gwiKnowKey
	llmlog.Record(pool, e)
	gwiSettle(t, pool)
	if d := llmlog.InsertFailures() - fails; d != 1 {
		t.Fatalf("InsertFailures +%d, want +1", d)
	}
	if out := buf.String(); !strings.Contains(out, "23514") || !strings.Contains(out, "ck_llm_log_one_identity") {
		t.Fatalf("insert failure not the identity CHECK: %q", out)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_llm_log`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d (%v), want 0", n, err)
	}
}

// TestRecordGatewayOneRowPerRequest is gate (c): a failover (502 on the
// first backend, success on the second) is ONE row with attempt = 2 and both
// attempts in metadata.chain — not one row per physical attempt.
func TestRecordGatewayOneRowPerRequest(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	r := gwiRow(llmlog.PipelineGatewayChat)
	r.Attempts = []llmlog.WireAttempt{
		{Backend: "spark-chat", Class: "http_5xx", Ms: 40},
		{Backend: "herbert-chat", Ms: 760, WaitMs: 2},
	}
	r.Telemetry.BackendName = "herbert-chat"
	llmlog.RecordGateway(pool, r)
	gwiSettle(t, pool)

	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_llm_log`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one request with two attempts, want 1", rows)
	}
	var attempt *int16
	var chain int
	var first, second string
	if err := pool.QueryRow(context.Background(), `
		SELECT attempt, COALESCE(jsonb_array_length(metadata->'chain'), 0),
		       COALESCE(metadata->'chain'->0->>'err_class', ''), COALESCE(metadata->'chain'->1->>'backend', '')
		  FROM context_llm_log`).Scan(&attempt, &chain, &first, &second); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if attempt == nil || *attempt != 2 || chain != 2 || first != "http_5xx" || second != "herbert-chat" {
		t.Fatalf("attempt=%v chain=%d chain[0].err_class=%q chain[1].backend=%q, want 2/2/http_5xx/herbert-chat",
			attempt, chain, first, second)
	}
	var calls int64
	if err := pool.QueryRow(context.Background(),
		`SELECT sum(calls) FROM context_llm_usage_hourly WHERE consumer_id = $1`, gwiConsumer).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("rollup calls = %d (%v), want 1 — the request counts once", calls, err)
	}
}

// TestRecordGatewayStorableValues: what PostgreSQL refuses in a text or
// JSONB value — NUL, invalid UTF-8 — is cleaned before the insert, so a
// client cannot make its own row (and with it its attribution) vanish by
// sending it in a body, an alias or a peer header.
func TestRecordGatewayStorableValues(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	fails := llmlog.InsertFailures()
	r := gwiRow(llmlog.PipelineGatewayChat)
	r.BodyLogging = llmlog.BodyLoggingFull
	r.Request = "{\"content\":\"a\x00b\xffc\"}"
	r.Response = "answer\x00"
	r.Gateway.Alias = "al\x00ias\xfe"
	r.Gateway.PeerPipeline = "peer\x00"
	llmlog.RecordGateway(pool, r)
	gwiSettle(t, pool)
	if d := llmlog.InsertFailures() - fails; d != 0 {
		t.Fatalf("InsertFailures +%d — a NUL or invalid byte reached the insert", d)
	}
	var user, resp, alias string
	if err := pool.QueryRow(context.Background(),
		`SELECT request_user, response_content, metadata->'gw'->>'alias' FROM context_llm_log`).Scan(&user, &resp, &alias); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if user != "{\"content\":\"ab\uFFFDc\"}" || resp != "answer" || alias != "alias\uFFFD" {
		t.Fatalf("stored %q / %q / alias %q", user, resp, alias)
	}
}

// TestRecordGatewayVolumeProbe is measurement (f), Finding 23 / R9: the
// growth of context_llm_log per /v1 row at the default body policy, heap
// (table, TOAST included) and indexes reported separately. Opt-in: it
// writes CTX_W069_VOLUME_ROWS rows (the briefing's run: 10000).
func TestRecordGatewayVolumeProbe(t *testing.T) {
	raw := os.Getenv("CTX_W069_VOLUME_ROWS")
	if raw == "" {
		t.Skip("CTX_W069_VOLUME_ROWS not set — the volume probe is opt-in")
	}
	rows, err := strconv.Atoi(raw)
	if err != nil || rows <= 0 {
		t.Fatalf("CTX_W069_VOLUME_ROWS=%q: want a positive row count", raw)
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	size := func() (table, index, toast, total int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `ANALYZE context_llm_log`); err != nil {
			t.Fatalf("analyze: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(sum(table_bytes), 0)::bigint, COALESCE(sum(index_bytes), 0)::bigint,
			       COALESCE(sum(toast_bytes), 0)::bigint, COALESCE(sum(total_bytes), 0)::bigint
			  FROM hypertable_detailed_size('context_llm_log')`).Scan(&table, &index, &toast, &total); err != nil {
			t.Fatalf("hypertable_detailed_size: %v", err)
		}
		return
	}
	// One warm-up row creates the current chunk, so the delta measures rows,
	// not the chunk's fixed overhead.
	llmlog.RecordGateway(pool, gwiRow(llmlog.PipelineGatewayChat))
	gwiSettle(t, pool)
	t0, i0, s0, all0 := size()
	start := time.Now()
	pipelines := []string{llmlog.PipelineGatewayChat, llmlog.PipelineGatewayEmbeddings, llmlog.PipelineGatewayRerank}
	for i := range rows {
		r := gwiRow(pipelines[i%len(pipelines)])
		r.Gateway.ClientReqID = "req-" + strconv.Itoa(i)
		r.Gateway.Stream = i%2 == 0
		if r.Gateway.Stream {
			ttft := int64(100 + i%900)
			r.Gateway.TTFTMs = &ttft
		}
		llmlog.RecordGateway(pool, r)
		if i%1000 == 999 {
			gwiSettle(t, pool)
		}
	}
	gwiSettle(t, pool)
	elapsed := time.Since(start)
	t1, i1, s1, all1 := size()
	var n int64
	var meta, chain float64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), avg(pg_column_size(metadata)), avg(pg_column_size(metadata->'chain'))
		  FROM context_llm_log`).Scan(&n, &meta, &chain); err != nil {
		t.Fatalf("count: %v", err)
	}
	var chunks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM show_chunks('context_llm_log')`).Scan(&chunks); err != nil {
		t.Fatalf("chunks: %v", err)
	}
	per := func(a, b int64) float64 { return float64(b-a) / float64(rows) }
	t.Logf("volume probe: %d rows in %s (%d total, %d chunk(s))", rows, elapsed.Round(time.Millisecond), n, chunks)
	t.Logf("  table (heap)  %d → %d bytes: %.1f B/row", t0, t1, per(t0, t1))
	t.Logf("  indexes       %d → %d bytes: %.1f B/row", i0, i1, per(i0, i1))
	t.Logf("  toast         %d → %d bytes: %.1f B/row", s0, s1, per(s0, s1))
	t.Logf("  total         %d → %d bytes: %.1f B/row", all0, all1, per(all0, all1))
	t.Logf("  avg pg_column_size(metadata) %.1f B, of it metadata->'chain' %.1f B", meta, chain)
	// Per index, over the chunk indexes (named <chunk>_<hypertable index>).
	idx, err := pool.Query(ctx, `
		SELECT regexp_replace(i.indexname, '^_hyper_[0-9]+_[0-9]+_chunk_', ''),
		       sum(pg_relation_size(format('%I.%I', i.schemaname, i.indexname)::regclass))::bigint
		  FROM show_chunks('context_llm_log') c
		  JOIN pg_indexes i ON format('%I.%I', i.schemaname, i.tablename)::regclass = c
		 GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		t.Fatalf("index sizes: %v", err)
	}
	defer idx.Close()
	for idx.Next() {
		var name string
		var bytes int64
		if err := idx.Scan(&name, &bytes); err != nil {
			t.Fatalf("index sizes: %v", err)
		}
		t.Logf("  index %-32s %9d bytes ≈ %.1f B/row", name, bytes, float64(bytes)/float64(n))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("index sizes: %v", err)
	}
	if n != int64(rows)+1 {
		t.Fatalf("rows landed %d, want %d", n, rows+1)
	}
}
