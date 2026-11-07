package main

// W06-12, the cmd/ctxd half of the hook census: the meter's refusals are
// declared on the request's record with the meter's ledger reason (rate,
// concurrency, capacity/warming; an invalid subject declares nothing), an
// admitted request declares nothing, and the reservation's Reject hands the
// queue wait to the record (B-06-25) — plus the flusher: one write per
// flush, the window fed once per refusal, a failed write retried without a
// second log count.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

const ledgerConsumer = "0195f0a0-0000-7000-8000-0000000000d1"

var aliasNone modelalias.Alias

func declared(rec *oai.AccessRecord) string {
	r, ok := rec.Rejection()
	if !ok {
		return ""
	}
	return r.Reason + "/" + r.Detail
}

// TestMeterLimiterDeclaresItsRefusals: each meter refusal is declared once
// on the record with the meter's ledger reason; an admission and an invalid
// subject declare nothing.
func TestMeterLimiterDeclaresItsRefusals(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{
		"CTX_GATEWAY_CHAT_RPM_DEFAULT": "1", "CTX_GATEWAY_CHAT_TPM_DEFAULT": "60000",
		"CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT": "1",
	})
	settings := gatewaySettingsGetter(cfgStore)
	meter := keyquota.New(time.Now)
	l := meterLimiter{meter: meter, settings: settings}
	est := oai.Estimate{Op: llmlog.GatewayOpChat, PromptRunes: 10, MaxTokens: 10, ClientMaxTokens: true}
	admit := func(ca string) (*oai.AccessRecord, oai.LimitReservation, *oai.LimitRefusal) {
		rec := &oai.AccessRecord{}
		ctx := oai.WithAccessRecord(context.Background(), rec)
		consumer := urgentConsumer(ca)
		if ca == "" {
			consumer = nil
		}
		res, ref := l.Admit(ctx, consumer, aliasNone, est)
		return rec, res, ref
	}

	if rec, _, ref := admit(ledgerConsumer); ref == nil || declared(rec) != "capacity/warming" {
		t.Fatalf("unseeded meter: refusal %+v, declared %q, want capacity/warming", ref, declared(rec))
	}
	meter.MarkSeeded()
	rec, res, ref := admit(ledgerConsumer)
	if ref != nil || res == nil || declared(rec) != "" {
		t.Fatalf("first request: refusal %+v, declared %q, want an admission and no declaration", ref, declared(rec))
	}
	if rec2, _, ref := admit(ledgerConsumer); ref == nil || declared(rec2) != "rate/rate_requests" {
		t.Fatalf("second request at 1 rpm: refusal %+v, declared %q, want rate/rate_requests", ref, declared(rec2))
	}
	if rec3, _, ref := admit(""); ref == nil || ref.Code != oai.CodeInternalError || declared(rec3) != "" {
		t.Fatalf("no consumer: refusal %+v, declared %q, want internal_error and no declaration", ref, declared(rec3))
	}
	res.Abort()

	// L1: a second consumer at C = 1 with a generous rpm.
	cfgStore2 := meterTestStore(t, map[string]string{
		"CTX_GATEWAY_CHAT_RPM_DEFAULT": "1000", "CTX_GATEWAY_CONSUMER_CONCURRENCY_DEFAULT": "1",
	})
	l = meterLimiter{meter: meter, settings: gatewaySettingsGetter(cfgStore2)}
	const other = "0195f0a0-0000-7000-8000-0000000000d2"
	_, held, ref := admit(other)
	if ref != nil {
		t.Fatalf("first request of %s refused: %+v", other, ref)
	}
	if rec4, _, ref := admit(other); ref == nil || ref.Code != oai.CodeConcurrencyLimitExceeded || declared(rec4) != "concurrency/" {
		t.Fatalf("second open request at C=1: refusal %+v, declared %q, want concurrency", ref, declared(rec4))
	}

	// Reject hands the queue wait to the record; the writer's declaration
	// (here by hand) carries it then.
	rec5 := &oai.AccessRecord{}
	ctx5 := oai.WithAccessRecord(context.Background(), rec5)
	held.Abort()
	r5, ref := l.Admit(ctx5, urgentConsumer(other), aliasNone, est)
	if ref != nil {
		t.Fatalf("admission after the slot was freed: %+v", ref)
	}
	rec5.NoteRejection(keyquota.RejectCapacity, keyquota.DetailWaitExpired)
	r5.Reject(oai.CodeServerBusy, 150*time.Millisecond)
	if got, ok := rec5.Rejection(); !ok || !got.Waited || got.Wait != 150*time.Millisecond {
		t.Fatalf("rejection after Reject: %+v ok=%v, want wait 150ms waited", got, ok)
	}
	if _, ref := l.Admit(ctx5, urgentConsumer(other), aliasNone, est); ref != nil {
		t.Fatalf("Reject did not free the L1 slot: %+v", ref)
	}
}

// fakeWrite records the flusher's writes and can fail.
type fakeWrite struct {
	mu    sync.Mutex
	calls int
	rows  []llmusage.RejectionRow
	fail  error
}

func (f *fakeWrite) write(_ context.Context, rows []llmusage.RejectionRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.rows = append(f.rows, rows...)
	return nil
}

type ledgerLogSink struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (s *ledgerLogSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *ledgerLogSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	s.recs = append(s.recs, r.Clone())
	s.mu.Unlock()
	return nil
}
func (s *ledgerLogSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *ledgerLogSink) WithGroup(string) slog.Handler      { return s }

func (s *ledgerLogSink) countMsg(msg string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.recs {
		if r.Message == msg {
			n++
		}
	}
	return n
}

func (s *ledgerLogSink) counts(msg string) (lines int, sum int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.Message != msg {
			continue
		}
		lines++
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "count" {
				sum += a.Value.Int64()
			}
			return true
		})
	}
	return lines, sum
}

// TestRejectionFlusherRetriesWithoutDoubleLogCount: a failed write keeps
// the counts for the next flush (which writes them once, summed), the
// window has counted each refusal exactly once, and every flush is one
// write.
func TestRejectionFlusherRetriesWithoutDoubleLogCount(t *testing.T) {
	sink := &ledgerLogSink{}
	cfg := config.Defaults().GatewaySettings()
	ledger := keyquota.NewLedger(time.Now)
	access := handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: func() oai.Settings { return cfg }, Logger: slog.New(sink), Ledger: ledger})
	w := &fakeWrite{fail: errors.New("database down")}
	f := newRejectionFlusher(ledger, access, w.write)
	for range 1000 {
		ledger.Note(keyquota.Rejection{ConsumerID: ledgerConsumer, Op: "chat", Alias: "qwen", Reason: keyquota.RejectRate, Detail: keyquota.DetailRateTokens})
	}
	f.flush(context.Background())
	w.fail = nil
	for range 5 {
		ledger.Note(keyquota.Rejection{ConsumerID: ledgerConsumer, Op: "chat", Alias: "qwen", Reason: keyquota.RejectRate, Detail: keyquota.DetailRateTokens})
	}
	f.flush(context.Background())
	f.flush(context.Background())
	if w.calls != 2 {
		t.Fatalf("writes %d, want 2 (failed + retry; an empty flush writes nothing)", w.calls)
	}
	var sum int64
	for _, r := range w.rows {
		sum += r.N
	}
	if sum != 1005 {
		t.Fatalf("written %d refusals in %d row(s), want 1005", sum, len(w.rows))
	}
	f.final()
	lines, logged := sink.counts("gateway rejections")
	if lines != 1 || logged != 1005 {
		t.Fatalf("log: %d line(s) counting %d, want one line counting 1005 (no count twice after the retry)", lines, logged)
	}
}

// TestRejectionFlusherFinalCancelsAStuckTick (Fremd-Review F-3): a periodic
// flush stuck on a slow database is cancelled by the shutdown step, whose
// own flush then writes everything — the step takes one timeout, not two.
func TestRejectionFlusherFinalCancelsAStuckTick(t *testing.T) {
	cfg := config.Defaults().GatewaySettings()
	ledger := keyquota.NewLedger(time.Now)
	access := handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: func() oai.Settings { return cfg }, Logger: slog.New(&ledgerLogSink{}), Ledger: ledger})
	started := make(chan struct{})
	var mu sync.Mutex
	var calls int
	var written int64
	write := func(ctx context.Context, rows []llmusage.RejectionRow) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
			<-ctx.Done() // the slow database
			return ctx.Err()
		}
		mu.Lock()
		for _, r := range rows {
			written += r.N
		}
		mu.Unlock()
		return nil
	}
	f := newRejectionFlusher(ledger, access, write)
	for range 10 {
		ledger.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	}
	tickDone := make(chan struct{})
	go func() { f.tick(context.Background()); close(tickDone) }()
	<-started
	for range 5 {
		ledger.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	}
	begin := time.Now()
	f.final()
	took := time.Since(begin)
	<-tickDone
	t.Logf("shutdown flush with a stuck tick: %s (bound %s)", took.Round(time.Millisecond), rejectionFlushTimeout)
	if took >= rejectionFlushTimeout/2 {
		t.Fatalf("final took %s with a stuck tick, want well under %s (the tick is cancelled, not waited out)", took, rejectionFlushTimeout)
	}
	mu.Lock()
	defer mu.Unlock()
	if written != 15 || calls != 2 {
		t.Fatalf("written %d in %d write(s), want 15 in 2 (the stuck batch retried by final)", written, calls)
	}
}

// TestRejectionFlusherDropsAPoisonBatch (Fremd-Review F-4): a batch the
// database refuses as such (SQLSTATE 21/22/23) is dropped, not retried —
// a retry would fail again and take every fresh count with it.
func TestRejectionFlusherDropsAPoisonBatch(t *testing.T) {
	cfg := config.Defaults().GatewaySettings()
	ledger := keyquota.NewLedger(time.Now)
	access := handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: func() oai.Settings { return cfg }, Logger: slog.New(&ledgerLogSink{}), Ledger: ledger})
	w := &fakeWrite{fail: &pgconn.PgError{Code: "21000"}}
	f := newRejectionFlusher(ledger, access, w.write)
	ledger.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	f.flush(context.Background())
	w.fail = nil
	for range 3 {
		ledger.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	}
	f.flush(context.Background())
	var sum int64
	for _, r := range w.rows {
		sum += r.N
	}
	if sum != 3 {
		t.Fatalf("written %d, want 3 (the refused batch dropped, the fresh counts written)", sum)
	}
	if !permanentFlushError(&pgconn.PgError{Code: "23514"}) || permanentFlushError(errors.New("conn reset")) || permanentFlushError(&pgconn.PgError{Code: "57014"}) {
		t.Fatal("permanentFlushError: want classes 21/22/23 permanent, a connection error and 57014 (cancel) transient")
	}
}
