package handler

// W06-12 hook census, handler half: every refusal of the model-key door, the
// federation loop guard and the admission writer, triggered once behind the
// real access-log stage with a ledger — a refusal by a rule is exactly one
// ledger note with its reason and detail and no direct window count; a fault
// of the server (proxy, unavailable, stacked doors, internal) is no note and
// one direct count. Gate "no exit without a note, no note without an exit".

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
)

// censusStage is the access-log stage with a ledger and a log sink.
type censusStage struct {
	log    *GatewayAccessLog
	ledger *keyquota.Ledger
	sink   *gwLogSink
}

func newCensusStage(t *testing.T) *censusStage {
	t.Helper()
	c := &censusStage{ledger: keyquota.NewLedger(time.Now), sink: &gwLogSink{}}
	settings := config.Defaults().GatewaySettings()
	c.log = NewGatewayAccessLog(GatewayAccessLogConfig{
		Settings: func() oai.Settings { return settings }, Logger: slog.New(c.sink), Ledger: c.ledger,
	})
	return c
}

// expect drains the ledger and the window after one refusal: want "" ⇒ no
// note and one direct count; else exactly one note reason/detail and no
// direct count.
func (c *censusStage) expect(t *testing.T, name, wantReason, wantDetail string) {
	t.Helper()
	d := c.ledger.Drain()
	c.log.Flush()
	lines := c.sink.lines("gateway rejections")
	c.sink.mu.Lock()
	c.sink.recs = nil
	c.sink.mu.Unlock()
	if wantReason == "" {
		if len(d) != 0 || len(lines) != 1 || lines[0]["count"] != int64(1) {
			t.Errorf("%s: ledger %+v, window %v — want no note and one direct count", name, d, lines)
		}
		return
	}
	if len(d) != 1 || d[0].N != 1 || d[0].Reason != wantReason || d[0].Detail != wantDetail || len(lines) != 0 {
		t.Errorf("%s: ledger %+v, window %v — want one note %s/%s and no direct count", name, d, lines, wantReason, wantDetail)
	}
}

// TestRefusalLedgerCensusDoor: the door's refusals (stages 0, 1, 3, 4, 5,
// 6, 7) behind the stage.
func TestRefusalLedgerCensusDoor(t *testing.T) {
	c := newCensusStage(t)
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	settings := config.Defaults().GatewaySettings()
	settings.Auth.ProbeRate = 1
	door := ModelAuth(ModelAuthConfig{
		Pool: pool, Settings: func() oai.Settings { return settings }, Usage: nopToucher{},
		Logger: slog.New(slog.DiscardHandler),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej ModelAuthRejection) { w.WriteHeader(rej.Status) },
	})
	h := c.log.Handler(door.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	serve := func(name string, status int, mut func(*http.Request)) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = "192.0.2.1:40000"
		mut(r)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != status {
			t.Fatalf("%s: status %d, want %d", name, rec.Code, status)
		}
	}
	key, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	other, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	bearer := func(k string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+k) }
	}

	serve("no credential", http.StatusUnauthorized, func(*http.Request) {})
	c.expect(t, "401 no credential", keyquota.RejectAuth, keyquota.DetailInvalidKey)
	serve("malformed key", http.StatusUnauthorized, bearer("sk-not-a-model-key"))
	c.expect(t, "401 malformed", keyquota.RejectAuth, keyquota.DetailInvalidKey)
	serve("proxy", http.StatusServiceUnavailable, func(r *http.Request) { r.Header.Set("X-Forwarded-For", "198.51.100.7") })
	c.expect(t, "503 proxy_misconfigured", "", "")
	serve("stacked doors", http.StatusServiceUnavailable, func(r *http.Request) {
		*r = *r.WithContext(context.WithValue(r.Context(), authResultKey, &auth.AuthResult{}))
	})
	c.expect(t, "503 internal (stacked doors)", "", "")
	// The first unknown key takes the one probe token and meets the
	// unreachable database: 503 auth_unavailable — a fault, no note.
	serve("database down", http.StatusServiceUnavailable, bearer(key.Plaintext()))
	c.expect(t, "503 auth_unavailable", "", "")
	// The next unknown key finds the probe bucket empty: 503 overloaded.
	serve("probe bucket", http.StatusServiceUnavailable, bearer(other.Plaintext()))
	c.expect(t, "503 auth_overloaded", keyquota.RejectCapacity, keyquota.DetailAuthOverloaded)
	// The failure lock of a key from this network: 429.
	for range settings.Auth.FailureLimit {
		door.limiter.fail(limiterKey(netip.MustParseAddr("192.0.2.1"), key.HashPrefix()), settings.Auth.FailureWindow)
	}
	serve("locked", http.StatusTooManyRequests, bearer(key.Plaintext()))
	c.expect(t, "429 locked", keyquota.RejectAuth, keyquota.DetailLocked)
}

// TestRefusalLedgerCensusLoopGuard: the guard's refusals behind the stage —
// 400 header_invalid, 508 loop (duplicate entry and own token), 508 depth —
// and the wiring defect (no identity) as no note.
func TestRefusalLedgerCensusLoopGuard(t *testing.T) {
	c := newCensusStage(t)
	guard := LoopGuard(LoopGuardConfig{MaxDepth: func() int { return 2 }, Envelope: LoopGuardOpenAI})
	h := c.log.Handler(guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	serve := func(name string, status int, withID bool, hdr map[string][]string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if withID {
			r = r.WithContext(fedhop.WithIdentity(r.Context(), lgOwn))
		}
		for k, vs := range hdr {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != status {
			t.Fatalf("%s: status %d, want %d (%s)", name, rec.Code, status, rec.Body.String())
		}
	}
	f1, f2 := lgForeign.MintToken(), lgForeign.MintToken()
	serve("header invalid", http.StatusBadRequest, true, map[string][]string{fedhop.HeaderHop: {"ZZ"}})
	c.expect(t, "400 header_invalid", keyquota.RejectFederation, keyquota.DetailHeaderInvalid)
	serve("duplicate token", http.StatusLoopDetected, true, map[string][]string{fedhop.HeaderHop: {f1 + ", " + f1}})
	c.expect(t, "508 loop (duplicate)", keyquota.RejectFederation, keyquota.DetailLoop)
	serve("own token", http.StatusLoopDetected, true, map[string][]string{fedhop.HeaderHop: {lgOwn.MintToken()}})
	c.expect(t, "508 loop (own token)", keyquota.RejectFederation, keyquota.DetailLoop)
	serve("depth", http.StatusLoopDetected, true, map[string][]string{fedhop.HeaderHop: {f1 + ", " + f2}})
	c.expect(t, "508 depth", keyquota.RejectFederation, keyquota.DetailDepth)
	serve("no identity", http.StatusServiceUnavailable, false, map[string][]string{fedhop.HeaderHop: {f1}})
	c.expect(t, "503 internal (no identity)", "", "")
}

// TestRefusalLedgerCensusAdmissionWriter: the admission writer declares the
// L2 refusals — server_busy (cap ladder, queue full, a busy failover row),
// wait_expired (the wait budget, run out or predicted), not_offered — and
// nothing for an internal error or a gone client.
func TestRefusalLedgerCensusAdmissionWriter(t *testing.T) {
	disp := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(disp.Close)
	settings := config.Defaults().GatewaySettings()
	a := NewV1Admission(V1AdmissionConfig{Dispatch: disp, Settings: func() oai.Settings { return settings }, Logger: slog.New(slog.DiscardHandler)})
	cases := []struct {
		name           string
		err            error
		cancel         bool
		reason, detail string
	}{
		{"cap ladder", &dispatch.RejectionError{Err: dispatch.ErrPrincipalSaturated, RetryAfter: time.Second}, false, keyquota.RejectCapacity, keyquota.DetailServerBusy},
		{"queue full", dispatch.ErrQueueFull, false, keyquota.RejectCapacity, keyquota.DetailServerBusy},
		{"busy failover row", dispatch.ErrWouldBlock, false, keyquota.RejectCapacity, keyquota.DetailServerBusy},
		{"wait expired", &dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: time.Second}, false, keyquota.RejectCapacity, keyquota.DetailWaitExpired},
		{"not offered", dispatch.ErrExternalNotOffered, false, keyquota.RejectCapacity, keyquota.DetailNotOffered},
		{"lane not offered", ErrLaneNotOffered, false, keyquota.RejectCapacity, keyquota.DetailNotOffered},
		{"internal", dispatch.ErrInvalidClass, false, "", ""},
		{"foreign error", errors.New("an attempt error"), false, "", ""},
		{"client gone", dispatch.ErrQueueFull, true, "", ""},
	}
	for _, tc := range cases {
		rec := &oai.AccessRecord{}
		ctx, cancel := context.WithCancel(oai.WithAccessRecord(context.Background(), rec))
		if tc.cancel {
			cancel()
		}
		code := a.WriteAdmissionError(ctx, httptest.NewRecorder(), tc.err)
		cancel()
		got, ok := rec.Rejection()
		switch {
		case tc.reason == "" && ok:
			t.Errorf("%s: declared %s/%s (code %q), want nothing", tc.name, got.Reason, got.Detail, code)
		case tc.reason != "" && (!ok || got.Reason != tc.reason || got.Detail != tc.detail):
			t.Errorf("%s: declared %+v ok=%v (code %q), want %s/%s", tc.name, got, ok, code, tc.reason, tc.detail)
		}
		if strings.Contains(tc.name, "client gone") && code != "" {
			t.Errorf("client gone: code %q, want none written", code)
		}
	}
	// A nil context is the writer's own contract (no record): no panic.
	_ = a.WriteAdmissionError(nil, httptest.NewRecorder(), dispatch.ErrQueueFull) //nolint:staticcheck // SA1012: the writer accepts nil
}
