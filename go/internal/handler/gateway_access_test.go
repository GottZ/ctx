package handler

// W06-9 (f), B-06-22: the model groups' access log — one INFO line per
// successful request (gateway.access_log), refusals aggregated per
// (consumer, reason) into at most one WARN per window, and handler.Logger
// silent for every request the group logs itself.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// gwLogSink records every slog record it sees.
type gwLogSink struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (s *gwLogSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *gwLogSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	s.recs = append(s.recs, r.Clone())
	s.mu.Unlock()
	return nil
}
func (s *gwLogSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *gwLogSink) WithGroup(string) slog.Handler      { return s }

// lines returns the records with message msg, attributes flattened.
func (s *gwLogSink) lines(msg string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, r := range s.recs {
		if r.Message != msg {
			continue
		}
		m := map[string]any{"level": r.Level.String()}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value.Resolve().Any()
			return true
		})
		out = append(out, m)
	}
	return out
}

type gwAccessEnv struct {
	log      *GatewayAccessLog
	sink     *gwLogSink
	clock    time.Time
	settings oai.Settings
}

func newGWAccess(t *testing.T) *gwAccessEnv {
	t.Helper()
	e := &gwAccessEnv{sink: &gwLogSink{}, clock: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), settings: config.Defaults().GatewaySettings()}
	e.log = NewGatewayAccessLog(GatewayAccessLogConfig{
		Settings: func() oai.Settings { return e.settings },
		Logger:   slog.New(e.sink),
	})
	e.log.now = func() time.Time { return e.clock }
	e.log.lastFlush = e.clock
	return e
}

// serve runs one request through the stage; inner plays the stages behind
// it (door, handler).
func (e *gwAccessEnv) serve(inner http.HandlerFunc) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	e.log.Handler(inner).ServeHTTP(rec, r)
	return rec
}

func refuse(consumer, reason string, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ar := oai.AccessRecordFrom(r.Context()); ar != nil {
			ar.ConsumerID, ar.Reason = consumer, reason
		}
		w.WriteHeader(status)
	}
}

// TestGatewayAccessLogThrottlesRejections is B-06-22 at the stage: 1 000
// refusals of one (consumer, reason) inside a window are one WARN line with
// count=1000 at the window's end; nothing is logged per request; a quiet
// window logs nothing; keys are separate lines.
func TestGatewayAccessLogThrottlesRejections(t *testing.T) {
	e := newGWAccess(t)
	for range 1000 {
		e.serve(refuse("", oai.CodeInvalidAPIKey, http.StatusUnauthorized))
	}
	for range 3 {
		e.serve(refuse("c-1", oai.CodeRateLimitExceeded, http.StatusTooManyRequests))
	}
	if n := len(e.sink.recs); n != 0 {
		t.Fatalf("%d line(s) inside the window, want 0 (refusals are counted, not logged)", n)
	}
	e.clock = e.clock.Add(gatewayRejectionWindow - time.Millisecond)
	e.log.flushDue(e.clock)
	if n := len(e.sink.recs); n != 0 {
		t.Fatalf("%d line(s) before the window ended", n)
	}
	e.clock = e.clock.Add(time.Millisecond)
	e.log.flushDue(e.clock)
	lines := e.sink.lines("gateway rejections")
	if len(lines) != 2 || len(e.sink.recs) != 2 {
		t.Fatalf("lines %v, want exactly two rejection lines", lines)
	}
	want := []map[string]any{
		// detail is the refusal ledger's (W06-12): empty for a refusal no
		// stage declared, which the stage counts here directly.
		{"level": "WARN", "consumer_id": "", "reason": oai.CodeInvalidAPIKey, "detail": "", "count": int64(1000), "window": "1m"},
		{"level": "WARN", "consumer_id": "c-1", "reason": oai.CodeRateLimitExceeded, "detail": "", "count": int64(3), "window": "1m"},
	}
	for i := range want {
		if !maps.Equal(lines[i], want[i]) {
			t.Errorf("line %d = %v, want %v", i, lines[i], want[i])
		}
	}
	e.clock = e.clock.Add(gatewayRejectionWindow)
	e.log.flushDue(e.clock)
	if n := len(e.sink.recs); n != 2 {
		t.Fatalf("a quiet window logged %d more line(s)", n-2)
	}
}

// TestGatewayAccessLogLazyFlush: without the flusher's ticks the counts are
// not stranded — the first refusal after a window has passed flushes, and
// the next burst is again at most one line per window.
func TestGatewayAccessLogLazyFlush(t *testing.T) {
	e := newGWAccess(t)
	for range 10 {
		e.serve(refuse("", oai.CodeInvalidAPIKey, http.StatusUnauthorized))
	}
	e.clock = e.clock.Add(gatewayRejectionWindow)
	e.serve(refuse("", oai.CodeInvalidAPIKey, http.StatusUnauthorized))
	lines := e.sink.lines("gateway rejections")
	if len(lines) != 1 || lines[0]["count"] != int64(11) {
		t.Fatalf("lines %v, want one line with count=11", lines)
	}
	e.clock = e.clock.Add(time.Second)
	for range 500 {
		e.serve(refuse("", oai.CodeInvalidAPIKey, http.StatusUnauthorized))
	}
	if n := len(e.sink.lines("gateway rejections")); n != 1 {
		t.Fatalf("%d lines inside the second window, want still 1", n)
	}
}

// TestGatewayAccessLogFlushWritesPending: Flush (the flusher's last step at
// shutdown) writes what the window holds, so a shutdown loses no count.
func TestGatewayAccessLogFlushWritesPending(t *testing.T) {
	e := newGWAccess(t)
	e.serve(refuse("", "http_404", http.StatusNotFound))
	e.log.Flush()
	if lines := e.sink.lines("gateway rejections"); len(lines) != 1 || lines[0]["count"] != int64(1) {
		t.Fatalf("lines %v, want the pending count flushed", lines)
	}
}

// TestGatewayAccessLogDeclaredRefusalGoesToTheLedger (W06-12): a refusal a
// stage declared (oai.NoteRejection) is noted in the ledger once — with the
// record's consumer, op and alias — and NOT counted in the window directly;
// an undeclared refusal is counted in the window as before; a declaration on
// a request that did not end as a refusal (status < 400) counts nowhere.
func TestGatewayAccessLogDeclaredRefusalGoesToTheLedger(t *testing.T) {
	e := newGWAccess(t)
	ledger := keyquota.NewLedger(func() time.Time { return e.clock })
	e.log.ledger = ledger
	const consumer = "0195f0a0-0000-7000-8000-0000000000c1"
	declared := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ar := oai.AccessRecordFrom(r.Context())
			ar.ConsumerID, ar.Op, ar.Reason = consumer, "chat", oai.CodeModelNotFound
			ar.NoteRejection(keyquota.RejectModel, keyquota.DetailNotFound)
			ar.NoteRejection(keyquota.RejectAuth, keyquota.DetailInvalidKey) // the second declaration loses
			w.WriteHeader(status)
		}
	}
	e.serve(declared(http.StatusNotFound))
	e.serve(declared(http.StatusOK))
	e.serve(refuse(consumer, oai.CodeInternalError, http.StatusServiceUnavailable))
	d := ledger.Drain()
	if len(d) != 1 || d[0].N != 1 || d[0].ConsumerID != consumer || d[0].Op != "chat" || d[0].Alias != keyquota.UnresolvedAlias ||
		d[0].Reason != keyquota.RejectModel || d[0].Detail != keyquota.DetailNotFound {
		t.Fatalf("ledger %+v, want one model/not_found note of the 404 only", d)
	}
	e.log.Flush()
	lines := e.sink.lines("gateway rejections")
	if len(lines) != 1 || lines[0]["reason"] != oai.CodeInternalError || lines[0]["count"] != int64(1) {
		t.Fatalf("window lines %v, want only the undeclared 503", lines)
	}
}

// TestGatewayAccessLogCountRejectionsOneLinePerReason (W06-12, B-06-22):
// the ledger's counts reach the window through CountRejections — one line
// per (consumer, reason) with the details of the window, whatever the
// number of refusals and details.
func TestGatewayAccessLogCountRejectionsOneLinePerReason(t *testing.T) {
	e := newGWAccess(t)
	const consumer = "0195f0a0-0000-7000-8000-0000000000c1"
	e.log.CountRejections([]keyquota.RejectionDelta{
		{ConsumerID: consumer, Reason: keyquota.RejectCapacity, Detail: keyquota.DetailServerBusy, N: 600},
		{ConsumerID: consumer, Reason: keyquota.RejectCapacity, Detail: keyquota.DetailWaitExpired, N: 400},
		{ConsumerID: consumer, Reason: keyquota.RejectRate, Detail: keyquota.DetailRateTokens, N: 5},
		{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey, N: 7},
	})
	e.log.FlushDue()
	if n := len(e.sink.lines("gateway rejections")); n != 0 {
		t.Fatalf("%d line(s) before the window ended, want 0", n)
	}
	e.clock = e.clock.Add(gatewayRejectionWindow)
	e.log.FlushDue()
	lines := e.sink.lines("gateway rejections")
	got := map[string]string{}
	for _, l := range lines {
		got[l["consumer_id"].(string)+"|"+l["reason"].(string)] = fmt.Sprintf("%v|%v", l["count"], l["detail"])
	}
	want := map[string]string{
		consumer + "|capacity": "1000|server_busy,wait_expired",
		consumer + "|rate":     "5|rate_tokens",
		"|auth":                "7|invalid_key",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
}

// TestGatewayAccessLogSuccessLine: one INFO line per successful request,
// with exactly the fields of design/06 §4.6 — no label, no content, no key
// material — and none with gateway.access_log off.
func TestGatewayAccessLogSuccessLine(t *testing.T) {
	e := newGWAccess(t)
	qw := int64(12)
	ok := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ar := oai.AccessRecordFrom(r.Context())
			if ar == nil {
				t.Fatal("no access record in the request context")
			}
			ar.ConsumerID, ar.ModelKeyID, ar.TenantID = "c-1", "mk-1", "t-1"
			ar.Op, ar.Alias, ar.Stream = "chat", "team-chat", true
			ar.PromptTokens, ar.CompletionTokens, ar.Usage = 10, 20, "backend"
			ar.QueueWaitMs, ar.Class, ar.Hop = qw, "external", 1
			w.WriteHeader(status)
		}
	}
	e.serve(ok(http.StatusOK))
	e.serve(ok(http.StatusNotModified))
	lines := e.sink.lines("gateway request")
	if len(lines) != 2 {
		t.Fatalf("%d gateway request line(s), want 2", len(lines))
	}
	wantKeys := []string{"alias", "class", "completion_tokens", "consumer_id", "duration_ms", "hop", "level", "model_key_id",
		"op", "prompt_tokens", "queue_wait_ms", "request_id", "status", "stream", "tenant_id", "usage"}
	if got := slices.Sorted(maps.Keys(lines[0])); !slices.Equal(got, wantKeys) {
		t.Fatalf("fields %q, want %q", got, wantKeys)
	}
	for k, v := range map[string]any{
		"level": "INFO", "consumer_id": "c-1", "model_key_id": "mk-1", "tenant_id": "t-1", "op": "chat",
		"alias": "team-chat", "status": int64(200), "stream": true, "prompt_tokens": int64(10),
		"completion_tokens": int64(20), "usage": "backend", "queue_wait_ms": int64(12), "class": "external", "hop": int64(1),
	} {
		if lines[0][k] != v {
			t.Errorf("%s = %#v (%T), want %#v", k, lines[0][k], lines[0][k], v)
		}
	}
	if lines[1]["status"] != int64(304) {
		t.Errorf("a 304 is a success line with its status, got %v", lines[1]["status"])
	}
	if n := len(e.sink.lines("gateway rejections")); n != 0 {
		t.Errorf("successes produced rejection lines")
	}

	e.settings.AccessLog = false
	e.serve(ok(http.StatusOK))
	if n := len(e.sink.lines("gateway request")); n != 2 {
		t.Fatalf("gateway.access_log=false still logged (%d lines)", n)
	}
}

// TestGatewayAccessLogReason: a refusal names the reason the refusing stage
// set; a stage that sets none is counted under http_<status>; a handler
// that never writes a status answered 200.
func TestGatewayAccessLogReason(t *testing.T) {
	e := newGWAccess(t)
	e.serve(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	e.serve(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) })
	e.serve(func(http.ResponseWriter, *http.Request) {})
	e.serve(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusContinue)
		w.WriteHeader(http.StatusBadGateway)
	})
	// A reason outside the vocabulary is free text — counted under the
	// status, never as a key of its own; the door's own codes count.
	e.serve(refuse("", "upstream said: you sent 'hello'", http.StatusTooManyRequests))
	e.serve(refuse("", ModelAuthCodeOverloaded, http.StatusServiceUnavailable))
	e.log.Flush()
	lines := e.sink.lines("gateway rejections")
	if len(lines) != 3 || lines[0]["reason"] != ModelAuthCodeOverloaded || lines[1]["reason"] != "http_429" ||
		lines[1]["count"] != int64(2) || lines[2]["reason"] != "http_502" {
		t.Fatalf("rejection lines %v, want auth_overloaded, http_429 (2) and http_502", lines)
	}
	if n := len(e.sink.lines("gateway request")); n != 2 {
		t.Fatalf("%d success line(s), want 2 (implicit 200s)", n)
	}
}

// TestGatewayAccessWriterPassesThrough: the stage's writer keeps flushing
// (streams) and unwrapping (http.ResponseController) working.
func TestGatewayAccessWriterPassesThrough(t *testing.T) {
	e := newGWAccess(t)
	rec := e.serve(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: 1\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the access writer: %v", err)
		}
		if f, ok := w.(http.Flusher); !ok {
			t.Error("the access writer is no http.Flusher")
		} else {
			f.Flush()
		}
	})
	if !rec.Flushed || rec.Body.String() != "data: 1\n\n" {
		t.Fatalf("flushed=%v body=%q", rec.Flushed, rec.Body.String())
	}
}

// flushFailer is a writer whose flush fails, as when the client is gone.
type flushFailer struct{ *httptest.ResponseRecorder }

func (flushFailer) FlushError() error { return errClientGone }

var errClientGone = errors.New("client gone")

// TestGatewayAccessWriterFlushError: a failed flush underneath reaches the
// handler through the stage's writer (a stream must see its client leave).
func TestGatewayAccessWriterFlushError(t *testing.T) {
	e := newGWAccess(t)
	var got error
	h := e.log.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got = http.NewResponseController(w).Flush()
	}))
	h.ServeHTTP(flushFailer{httptest.NewRecorder()}, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !errors.Is(got, errClientGone) {
		t.Fatalf("flush error %v, want the writer's %v", got, errClientGone)
	}
}

func TestWindowLabel(t *testing.T) {
	for d, want := range map[time.Duration]string{
		gatewayRejectionWindow: "1m", 10 * time.Second: "10s", 90 * time.Second: "1m30s", 2 * time.Hour: "120m",
	} {
		if got := windowLabel(d); got != want {
			t.Errorf("windowLabel(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestLoggerSilentForGatewayRequests: handler.Logger writes no "http
// request" line for a request the gateway stage logs itself — exactly one
// line per request — and keeps logging every other request. A writer in
// between that does not unwrap costs the claim, never a line: the request
// is then logged twice, not zero times.
func TestLoggerSilentForGatewayRequests(t *testing.T) {
	sink := &gwLogSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(sink))
	t.Cleanup(func() { slog.SetDefault(prev) })
	e := newGWAccess(t)
	e.sink = sink
	e.log.log = slog.New(sink)

	gw := Logger(e.log.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	plain := Logger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	opaque := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.log.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
			ServeHTTP(struct{ http.ResponseWriter }{w}, r)
	}))
	for _, h := range []http.Handler{gw, gw, plain, opaque} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	}
	if got := len(sink.lines("http request")); got != 2 {
		t.Fatalf("%d http request line(s), want 2 (the plain route and the opaque writer)", got)
	}
	if got := len(sink.lines("gateway request")); got != 3 {
		t.Fatalf("%d gateway request line(s), want 3", got)
	}
}

// TestModelAuthFillsAccessRecord: the door names its refusal reason in the
// access record (stage 3 here — no database).
func TestModelAuthFillsAccessRecord(t *testing.T) {
	d := newUnitDoor(t, "")
	ar := &oai.AccessRecord{}
	r := unitReq(nil)
	r = r.WithContext(oai.WithAccessRecord(r.Context(), ar))
	d.serve(r)
	if ar.Reason != oai.CodeInvalidAPIKey || ar.ConsumerID != "" {
		t.Fatalf("access record %+v, want reason %s and no consumer", ar, oai.CodeInvalidAPIKey)
	}
	if d.logs.Len() != 0 {
		t.Errorf("the door logged a refusal per request: %q", d.logs.String())
	}
}
