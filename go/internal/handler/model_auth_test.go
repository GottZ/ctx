package handler

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
)

// unitDoorEnv is a door over a pool that points nowhere: pgxpool connects
// lazily, so a stage that touches the database answers 503 auth_unavailable
// — every test below that expects a different answer thereby also proves
// the stage made no database call.
type unitDoorEnv struct {
	door    *ModelDoor
	h       http.Handler
	reached int
	mu      sync.Mutex
	rejects []ModelAuthRejection
	logs    *bytes.Buffer
}

func newUnitDoor(t *testing.T, trustedProxy string) *unitDoorEnv {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(pool.Close)
	e := &unitDoorEnv{logs: &bytes.Buffer{}}
	settings := config.Defaults().GatewaySettings()
	e.door = ModelAuth(ModelAuthConfig{
		Pool:         pool,
		Settings:     func() oai.Settings { return settings },
		TrustedProxy: trustedProxy,
		Usage:        nopToucher{},
		Logger:       slog.New(slog.NewTextHandler(e.logs, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej ModelAuthRejection) {
			e.mu.Lock()
			e.rejects = append(e.rejects, rej)
			e.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rej.Status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": rej.Code}})
		},
	})
	e.h = e.door.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		e.reached++
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return e
}

type nopToucher struct{}

func (nopToucher) Touch(string) {}

func unitReq(mut func(r *http.Request)) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = "192.0.2.1:40000"
	if mut != nil {
		mut(r)
	}
	return r
}

func (e *unitDoorEnv) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func wantReject(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, code, retry string) {
	t.Helper()
	if rec.Code != status || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
		t.Errorf("%s: %d %s, want %d %s", name, rec.Code, strings.TrimSpace(rec.Body.String()), status, code)
	}
	if got := rec.Header().Get("x-should-retry"); got != retry {
		t.Errorf("%s: x-should-retry %q, want %q", name, got, retry)
	}
}

// TestModelAuthStage0WiringConflict: a knowledge AuthResult in the context
// is a wiring defect — 503 internal_error, x-should-retry false (masterplan
// K5/K29), ERROR, counter; the handler is never reached; no database call.
func TestModelAuthStage0WiringConflict(t *testing.T) {
	e := newUnitDoor(t, "")
	key, _ := modelkey.Generate()
	r := unitReq(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key.Plaintext())
		*r = *r.WithContext(context.WithValue(r.Context(), authResultKey, &auth.AuthResult{IsValid: true, ApiKeyID: "k"}))
	})
	rec := e.serve(r)
	wantReject(t, "stage 0", rec, http.StatusServiceUnavailable, oai.CodeInternalError, "false")
	if e.reached != 0 || e.door.Stats().WiringConflicts != 1 {
		t.Errorf("reached=%d conflicts=%d, want 0/1", e.reached, e.door.Stats().WiringConflicts)
	}
	if !strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("no ERROR line: %s", e.logs.String())
	}
}

// TestModelAuthStage1ProxyMisconfigured: X-Forwarded-For without a trusted
// proxy — the client address is unknowable (B22, E-01-13): 503
// proxy_misconfigured, x-should-retry false, ERROR once per door, counter
// per request, no database call.
func TestModelAuthStage1ProxyMisconfigured(t *testing.T) {
	e := newUnitDoor(t, "")
	key, _ := modelkey.Generate()
	for i := 0; i < 2; i++ {
		rec := e.serve(unitReq(func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+key.Plaintext())
			r.Header.Set("X-Forwarded-For", "198.51.100.7")
		}))
		wantReject(t, "stage 1", rec, http.StatusServiceUnavailable, ModelAuthCodeProxyMisconfigured, "false")
	}
	if n := strings.Count(e.logs.String(), "level=ERROR"); n != 1 {
		t.Errorf("%d ERROR lines for two requests, want 1: %s", n, e.logs.String())
	}
	if got := e.door.Stats().ProxyMisconfigured; got != 2 || e.reached != 0 {
		t.Errorf("counter=%d reached=%d, want 2/0", got, e.reached)
	}
}

// TestModelAuthCredentialExtraction: only Authorization: Bearer is read;
// every other carrier and every malformed or knowledge-shaped value is the
// one byte-identical 401 without a database call, and the key is never
// mirrored.
func TestModelAuthCredentialExtraction(t *testing.T) {
	e := newUnitDoor(t, "")
	key, _ := modelkey.Generate()
	pt := key.Plaintext()
	broken := pt[:len(pt)-1] + string("01"[func() int {
		if pt[len(pt)-1] == '0' {
			return 1
		}
		return 0
	}()])
	const hexKey = "a4112026110300000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	cases := []struct {
		name string
		form bool // counts as a form failure (stage 4), not as a missing credential (stage 3)
		mut  func(r *http.Request)
	}{
		{"none", false, nil},
		{"x-context-key", false, func(r *http.Request) { r.Header.Set("X-Context-Key", pt) }},
		{"session-cookie", false, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: pt}) }},
		{"query-param", false, func(r *http.Request) { r.URL.RawQuery = "api_key=" + pt + "&key=" + pt }},
		{"basic-scheme", false, func(r *http.Request) { r.Header.Set("Authorization", "Basic "+pt) }},
		{"bare-token", false, func(r *http.Request) { r.Header.Set("Authorization", pt) }},
		{"empty-bearer", false, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
		{"knowledge-key", true, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+hexKey) }},
		{"oauth-token", true, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ctxt_"+strings.Repeat("A", 43)) }},
		{"refresh-token", true, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ctxr_"+strings.Repeat("A", 43)) }},
		{"bad-checksum", true, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+broken) }},
		{"glued-hex", true, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ctxm_"+hexKey) }},
	}
	var first *httptest.ResponseRecorder
	forms, missing := 0, 0
	for _, c := range cases {
		rec := e.serve(unitReq(c.mut))
		wantReject(t, c.name, rec, http.StatusUnauthorized, oai.CodeInvalidAPIKey, "false")
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate %q, want exactly %q (no resource_metadata)", c.name, got, "Bearer")
		}
		if strings.Contains(rec.Body.String(), "ctxm_") || strings.Contains(rec.Body.String(), hexKey) {
			t.Errorf("%s: body mirrors the credential: %s", c.name, rec.Body.String())
		}
		if first == nil {
			first = rec
		} else if rec.Body.String() != first.Body.String() {
			t.Errorf("%s: body %q differs from %q", c.name, rec.Body.String(), first.Body.String())
		}
		if c.form {
			forms++
		} else {
			missing++
		}
	}
	st := e.door.Stats()
	if e.reached != 0 || st.FormFailures != uint64(forms) || st.NoCredential != uint64(missing) {
		t.Errorf("reached=%d form_failures=%d no_credential=%d, want 0/%d/%d", e.reached, st.FormFailures, st.NoCredential, forms, missing)
	}
	for i, rej := range e.rejects {
		if rej != e.rejects[0] {
			t.Errorf("rejection %d = %+v, differs from %+v", i, rej, e.rejects[0])
		}
	}
}

// TestRequestTenantScopeConsumer: the config hook resolves a valid
// consumer's tenant_scope (design/01 §4.5); invalid or conflicting contexts
// stay on the base snapshot.
func TestRequestTenantScopeConsumer(t *testing.T) {
	ctx := context.Background()
	ca := &auth.ConsumerAuth{IsValid: true, TenantScope: "acme:main"}
	if got := RequestTenantScope(context.WithValue(ctx, consumerAuthKey, ca)); got != "acme:main" {
		t.Errorf("consumer ctx: %q, want acme:main", got)
	}
	if got := RequestTenantScope(context.WithValue(ctx, consumerAuthKey, &auth.ConsumerAuth{TenantScope: "acme:main"})); got != "" {
		t.Errorf("invalid consumer: %q, want \"\"", got)
	}
	both := context.WithValue(context.WithValue(ctx, consumerAuthKey, ca), authResultKey, &auth.AuthResult{IsValid: true, HomeScope: "private"})
	if got := RequestTenantScope(both); got != "" {
		t.Errorf("both credentials: %q, want \"\" (conflict stays on the base snapshot)", got)
	}
	if got := RequestTenantScope(context.WithValue(ctx, authResultKey, &auth.AuthResult{IsValid: true, HomeScope: "private"})); got != "private" {
		t.Errorf("knowledge ctx: %q, want private (unchanged)", got)
	}
	if ConsumerAuthFromContext(ctx) != nil || ConsumerAuthFromContext(context.WithValue(ctx, consumerAuthKey, ca)) != ca {
		t.Error("ConsumerAuthFromContext does not round-trip")
	}
}

// TestAuthRefusesConsumerContext: handler.Auth's first check (design/01
// §4.2 Gegenstück) — a ConsumerAuth in the context is a wiring defect: 500
// in the house format, before any credential is read.
func TestAuthRefusesConsumerContext(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	reached := false
	h := Auth(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r := httptest.NewRequest(http.MethodGet, "/api/query", nil)
	r = r.WithContext(context.WithValue(r.Context(), consumerAuthKey, &auth.ConsumerAuth{IsValid: true}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != `{"error":"authentication error"}` || reached {
		t.Errorf("Auth with a consumer ctx: %d %q reached=%v, want 500 house error, not reached", rec.Code, rec.Body.String(), reached)
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("no ERROR line: %s", logs.String())
	}
}

// TestModelAuthOrigin pins stage 2 without a database: the client address
// and the TLS evidence, only from the trusted proxy, over every header line
// (Fremd-Review F1), with the proxy address compared as an address, not as
// a string (Fremd-Review N6: CTX_TRUSTED_PROXY=2001:DB8::10 must match the
// peer [2001:db8::10]).
func TestModelAuthOrigin(t *testing.T) {
	type tc struct {
		name, trusted, remote string
		lines                 map[string][]string
		tls                   bool
		ip                    string
		secure                bool
	}
	const proxy = "192.0.2.10"
	cases := []tc{
		{"direct, no proxy configured", "", "203.0.113.5:1", nil, false, "203.0.113.5", false},
		{"direct TLS", "", "203.0.113.5:1", nil, true, "203.0.113.5", true},
		{"non-proxy peer forges XFF and XFP", proxy, "203.0.113.5:1", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}, "X-Forwarded-Proto": {"https"}}, false, "203.0.113.5", false},
		{"proxy, https", proxy, proxy + ":1", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}, "X-Forwarded-Proto": {"https"}}, false, "198.51.100.7", true},
		{"proxy, HTTPS upper case", proxy, proxy + ":1", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}, "X-Forwarded-Proto": {"HTTPS"}}, false, "198.51.100.7", true},
		{"proxy, list https,http", proxy, proxy + ":1", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}, "X-Forwarded-Proto": {"https, http"}}, false, "198.51.100.7", false},
		{"proxy appends a second XFF line", proxy, proxy + ":1", map[string][]string{"X-Forwarded-For": {"198.51.100.7", "203.0.113.99"}}, true, "203.0.113.99", true},
		{"proxy appends a second XFP line", proxy, proxy + ":1", map[string][]string{"X-Forwarded-For": {"203.0.113.99"}, "X-Forwarded-Proto": {"https", "http"}}, false, "203.0.113.99", false},
		{"proxy without XFF", proxy, proxy + ":1", nil, false, proxy, false},
		{"proxy, garbage XFF", proxy, proxy + ":1", map[string][]string{"X-Forwarded-For": {"not-an-ip"}}, true, "invalid IP", true},
		{"IPv6 proxy spelled in upper case", "2001:DB8::10", "[2001:db8::10]:1", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}, "X-Forwarded-Proto": {"https"}}, false, "198.51.100.7", true},
		{"IPv4-mapped spelling of the proxy", proxy, "[::ffff:192.0.2.10]:1", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}}, false, "198.51.100.7", false},
	}
	for _, c := range cases {
		e := newUnitDoor(t, c.trusted)
		r := unitReq(func(r *http.Request) {
			r.RemoteAddr = c.remote
			for k, vs := range c.lines {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
		})
		ip, secure := e.door.origin(r)
		if ip.String() != c.ip || secure != c.secure {
			t.Errorf("%s: ip=%s secure=%v, want ip=%s secure=%v", c.name, ip, secure, c.ip, c.secure)
		}
	}
}

// TestModelAuthClientGoneIsNoOutage (Fremd-Review N4): a client that gives
// up while its check waits is counted as gone, not as a semaphore timeout
// or a database error — those counters are the overload signal (K-06-7).
func TestModelAuthClientGoneIsNoOutage(t *testing.T) {
	e := newUnitDoor(t, "")
	key, _ := modelkey.Generate()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := e.serve(unitReq(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key.Plaintext())
		*r = *r.WithContext(ctx)
	}))
	st := e.door.Stats()
	if rec.Code != http.StatusServiceUnavailable || st.ClientGone != 1 || st.SemaphoreTimeouts != 0 || st.DBErrors != 0 {
		t.Errorf("cancelled request: %d, client_gone=%d semaphore_timeouts=%d db_errors=%d, want 503 and 1/0/0", rec.Code, st.ClientGone, st.SemaphoreTimeouts, st.DBErrors)
	}
}
