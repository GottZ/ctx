//go:build integration

package main

// W06-12 end to end through the production router (newV1Fixture: the real
// /v1 group, door, routes, access log, ledger and flusher over a PG18
// database): refusals land in context_llm_rejections_hourly — a 401 before
// the door accepted as consumer_id NULL / auth, 1 000 different unknown model
// names as ONE '_unresolved' row (gate (c)) — no refusal writes an LLM-log
// row (I-06-2), the throttled log speaks at most once per consumer and
// reason (B-06-22), and the shutdown's ledger step writes what was noted and
// not flushed yet (the SIGTERM probe of W06-1's pattern).
//
//	go test -tags=integration -p 1 ./cmd/ctxd/ -run 'V1RefusalLedger' -count=1 -v

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

func chatBody(model string) func(*http.Request) {
	return func(r *http.Request) {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"x"}]}`, model)
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", "application/json")
	}
}

func both(fs ...func(*http.Request)) func(*http.Request) {
	return func(r *http.Request) {
		for _, f := range fs {
			f(r)
		}
	}
}

type ledgerTableRow struct {
	consumer                  *string
	op, alias, reason, detail string
	n                         int64
}

func readLedgerTable(t *testing.T, f *v1Fixture) []ledgerTableRow {
	t.Helper()
	rows, err := f.seed.Query(context.Background(), `SELECT consumer_id::text, op, model_alias, reason, detail, n
		FROM context_llm_rejections_hourly ORDER BY reason, detail`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []ledgerTableRow
	for rows.Next() {
		var r ledgerTableRow
		if err := rows.Scan(&r.consumer, &r.op, &r.alias, &r.reason, &r.detail, &r.n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestV1RefusalLedgerEndToEnd: 1 000 forged keys and 1 000 unknown model
// names through the production router ⇒ two ledger rows after the flush,
// no LLM-log row, one log line per (consumer, reason).
func TestV1RefusalLedgerEndToEnd(t *testing.T) {
	sink := &ledgerLogSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)

	for i := range 1000 {
		if a := f.do(http.MethodPost, "/v1/chat/completions", both(withBearer("sk-not-a-model-key"), chatBody("granted"))); a.status != http.StatusUnauthorized {
			t.Fatalf("forged key %d: %d %s", i, a.status, a.body)
		}
	}
	for i := range 1000 {
		a := f.do(http.MethodPost, "/v1/chat/completions", both(withBearer(f.allKey), chatBody(fmt.Sprintf("no-such-model-%d", i))))
		if a.status != http.StatusNotFound || a.code() != oai.CodeModelNotFound {
			t.Fatalf("unknown model %d: %d %s", i, a.status, a.body)
		}
	}
	f.gateway.flushRejections()

	rows := readLedgerTable(t, f)
	if len(rows) != 2 {
		t.Fatalf("ledger rows %+v, want 2", rows)
	}
	auth, model := rows[0], rows[1]
	if auth.reason != keyquota.RejectAuth || auth.detail != keyquota.DetailInvalidKey || auth.consumer != nil || auth.n != 1000 || auth.alias != keyquota.UnresolvedAlias {
		t.Errorf("auth row %+v, want consumer_id NULL auth/invalid_key n=1000 alias %q", auth, keyquota.UnresolvedAlias)
	}
	if model.reason != keyquota.RejectModel || model.detail != keyquota.DetailNotFound || model.consumer == nil || model.n != 1000 ||
		model.alias != keyquota.UnresolvedAlias || model.op != "chat" {
		t.Errorf("model row %+v, want the all-models consumer model/not_found op chat n=1000 alias %q (one row for 1 000 names)", model, keyquota.UnresolvedAlias)
	}
	var llmRows int
	if err := f.seed.QueryRow(context.Background(), `SELECT count(*) FROM context_llm_log WHERE pipeline LIKE 'gateway-%'`).Scan(&llmRows); err != nil || llmRows != 0 {
		t.Errorf("LLM-log rows of /v1: %d (%v), want 0 — a refusal writes none (I-06-2)", llmRows, err)
	}
	lines, logged := sink.counts("gateway rejections")
	if lines != 2 || logged != 2000 {
		t.Errorf("log: %d rejection line(s) counting %d, want 2 lines (one per consumer and reason) counting 2000", lines, logged)
	}
	if n := sink.countMsg("http request"); n != 0 {
		t.Errorf("%d http request line(s) under /v1, want 0", n)
	}
}

// TestV1RefusalLedgerShutdownProbe is the shutdown gate: 50 refusals noted
// and not flushed (the gateway's loops never ran), then the shutdown
// sequence — its ledger step writes all 50. stopGateway is a no-op here, so
// only the step itself can write them (red against a sequence without it).
func TestV1RefusalLedgerShutdownProbe(t *testing.T) {
	f := newV1Fixture(t, nil, nil)
	f.setEnabled(t, true)
	for i := range 50 {
		if a := f.do(http.MethodGet, "/v1/models", withBearer("sk-not-a-model-key")); a.status != http.StatusUnauthorized {
			t.Fatalf("forged key %d: %d %s", i, a.status, a.body)
		}
	}
	if rows := readLedgerTable(t, f); len(rows) != 0 {
		t.Fatalf("ledger rows before the shutdown: %+v, want none (nothing flushed yet)", rows)
	}
	shutdownSequence(&http.Server{}, noDrain{}, f.pool, f.gateway.flushRejections, func() {})
	var sum int64
	for _, r := range readLedgerTable(t, f) {
		sum += r.n
	}
	if sum != 50 {
		t.Fatalf("sum(n) after the shutdown = %d, want 50", sum)
	}
}
