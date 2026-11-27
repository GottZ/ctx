//go:build integration

// Issue #575 over the PRODUCTION router (buildRouter through the
// v1ChatFixture of v1_chat_integration_test.go) and a real database: the
// recordings of a recording consumer (#571), written by the gateway's own
// recording stage from real /v1 requests — no row is inserted by hand —, are
// read through the five manage actions model-recording-* by a server-admin,
// and by nobody else:
//
//   - TestRecordingViewCallers: every action answers every caller that is
//     not a server-admin — a tenant owner, a member, each as key, OAuth
//     token and web session — with the shared 403, the model key and no key
//     with 401, a session without its CSRF token with 403; nothing of the
//     recording in any of those answers, no audit row, no model call.
//
//   - TestRecordingViewContent: a server-admin reads the overview, the list
//     (keyset, filters, no bodies), the blocks, windows, and has a block
//     translated by a stub model: credentials chain, the block between
//     tagged markers, no tools, the answer handed back and written nowhere
//     — the context_llm_log row without bodies, no process log line with the
//     content —, one audit row per content read, the recording row
//     byte-equal (xmin included) after every read; the brake answers a
//     second translation of the same person 429 while the recording
//     consumer's own request is served; a canceled translation cancels the
//     model call; an audit that fails serves nothing.
//
//   - TestRecordingViewTranslateTrust: a pool without a local full-trust
//     chat row — only an external one and a local no-credentials one — is
//     503 no_eligible_backend and no wire call.
//
//     go test -tags=integration -p 1 -count=1 -run 'TestRecordingView' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

// recViewSecret is a credentials pattern a recorded prompt carries.
const recViewSecret = "password=Tr0ub4dor-recview-575"

// recViewMarker is in the system prompt of every translation call.
const recViewMarker = "reading aid for a server operator"

// recViewEngine is the stub model of these tests: an OpenAI-shaped chat
// server that answers the gateway's requests with v1ChatAnswer and a
// translation call (its system prompt carries recViewMarker) with answer.
type recViewEngine struct {
	srv      *httptest.Server
	answer   atomic.Pointer[string]
	mu       sync.Mutex
	bodies   [][]byte
	hits     atomic.Int64 // translation calls
	canceled atomic.Int64 // translation calls whose request ended before the answer
	gate     atomic.Pointer[chan struct{}]
	arrived  chan struct{}
	// gateV1 holds the gateway's own requests (not the translations).
	gateV1 atomic.Pointer[chan struct{}]
	v1Hits atomic.Int64
	// fail, when not 0, answers a translation with this status and an error
	// body that quotes the call's user message (a validation error echoing
	// its input).
	fail atomic.Int64
}

func newRecViewEngine(t *testing.T) *recViewEngine {
	t.Helper()
	e := &recViewEngine{arrived: make(chan struct{}, 16)}
	a := "rendered"
	e.answer.Store(&a)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if !bytes.Contains(raw, []byte(recViewMarker)) {
			e.v1Hits.Add(1)
			if g := e.gateV1.Load(); g != nil {
				select {
				case <-*g:
				case <-r.Context().Done():
				}
			}
			_, _ = io.WriteString(w, v1ChatAnswer)
			return
		}
		e.hits.Add(1)
		e.mu.Lock()
		e.bodies = append(e.bodies, raw)
		e.mu.Unlock()
		select {
		case e.arrived <- struct{}{}:
		default:
		}
		if g := e.gate.Load(); g != nil {
			select {
			case <-*g:
			case <-r.Context().Done():
				e.canceled.Add(1)
				return
			}
		}
		if status := e.fail.Load(); status != 0 {
			var req struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			_ = json.Unmarshal(raw, &req)
			var user string
			if len(req.Messages) == 2 {
				user = req.Messages[1].Content
			}
			w.WriteHeader(int(status))
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rejected input: " + user}})
			return
		}
		out, _ := json.Marshal(map[string]any{
			"id": "gen-tr", "object": "chat.completion", "created": 1700000000, "model": "engine-model",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": *e.answer.Load()}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
		_, _ = w.Write(out)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// row is a chat row of the engine; trust and locality vary per test.
func (e *recViewEngine) row(name string, trust backends.Trust, locality string) backends.Backend {
	return backends.Backend{
		ID: "id-" + name, Name: name, Host: e.srv.URL, Protocol: backends.ProtocolOpenAI, Model: "engine-model",
		Roles: []string{"chat"}, Trust: trust, Locality: locality, Scope: backends.GlobalScope, Enabled: true,
		Priority: 10, Timeouts: map[string]int{"chat": 30}, NumCtx: 32768,
	}
}

// hold makes the engine hold every translation call until release.
func (e *recViewEngine) hold(t *testing.T) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	e.gate.Store(&ch)
	var once sync.Once
	release = func() { once.Do(func() { close(ch); e.gate.Store(nil) }) }
	t.Cleanup(release)
	return release
}

// drain forgets the arrivals of earlier calls.
func (e *recViewEngine) drain() {
	for {
		select {
		case <-e.arrived:
		default:
			return
		}
	}
}

func (e *recViewEngine) lastBody() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) == 0 {
		return nil
	}
	return e.bodies[len(e.bodies)-1]
}

// recViewCaller is one way to call /api/manage.
type recViewCaller struct {
	name string
	set  func(*http.Request)
}

// recViewWorld is a fixture with a recording consumer and its callers.
type recViewWorld struct {
	f                     *v1ChatFixture
	e                     *recViewEngine
	adminKey, adminKeyID  string
	ownerKey, ownerKeyID  string
	memberKey             string
	recID, recKey, plainI string
	admin                 []recViewCaller // server-admin: key, token, session
	others                []recViewCaller // everybody else
}

// recViewSession mints a token pair and a web session for a key; it returns
// the session id, its CSRF secret and the access token.
func recViewSession(t *testing.T, f *v1ChatFixture, keyID, csrf string) (session, token string) {
	t.Helper()
	ctx := context.Background()
	var principal string
	if err := f.seed.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, keyID).Scan(&principal); err != nil {
		t.Fatalf("principal: %v", err)
	}
	pair, err := store.MintTokenPair(ctx, f.seed, store.OAuthToken{APIKeyID: keyID, PrincipalID: principal, Audiences: []string{"https://ctx.example/mcp", "https://ctx.example/web"}, IssuedVia: "login"}, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	var tokenRow, family string
	if err := f.seed.QueryRow(ctx, `SELECT id::text, refresh_family::text FROM context_access_tokens WHERE token_hash = $1`,
		store.TokenHash(pair.AccessToken)).Scan(&tokenRow, &family); err != nil {
		t.Fatalf("token row: %v", err)
	}
	if err := f.seed.QueryRow(ctx, `INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4) RETURNING id::text`, principal, tokenRow, family, csrf).Scan(&session); err != nil {
		t.Fatalf("session: %v", err)
	}
	return session, pair.AccessToken
}

func recBearer(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+v) }
}

func cookie(session, csrf string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Cookie", "ctx_session="+session)
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
	}
}

func newRecViewWorld(t *testing.T, rows func(*recViewEngine) []backends.Backend) *recViewWorld {
	t.Helper()
	e := newRecViewEngine(t)
	f := newV1ChatFixture(t, rows(e))
	w := &recViewWorld{f: f, e: e}
	ctx := context.Background()
	k, adminKey, err := store.CreateApiKey(ctx, f.seed, "recview-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	if _, err := f.seed.Exec(ctx, `UPDATE context_api_keys SET is_admin = true, tenant_role = 'owner' WHERE id = $1::uuid`, k.ID); err != nil {
		t.Fatalf("promote admin key: %v", err)
	}
	w.adminKey, w.adminKeyID = adminKey, k.ID
	status, body, resp := v1RecAPI(t, f, http.MethodPost, "/api/manage", adminKey, map[string]any{
		"action": "tenant-create", "data": map[string]any{"slug": "recview", "display_name": "recview"}})
	owner, _ := resp["owner_key"].(string)
	tn, _ := resp["tenant"].(map[string]any)
	tenantID, _ := tn["id"].(string)
	if status != http.StatusOK || owner == "" || tenantID == "" {
		t.Fatalf("tenant-create: %d %s", status, body)
	}
	w.ownerKey = owner
	if err := f.seed.QueryRow(ctx, `SELECT id::text FROM context_api_keys WHERE tenant_id = $1::uuid AND tenant_role = 'owner'`, tenantID).Scan(&w.ownerKeyID); err != nil {
		t.Fatalf("owner key id: %v", err)
	}
	_, w.memberKey, err = store.CreateApiKey(ctx, f.seed, "recview-member", "private", nil, "")
	if err != nil {
		t.Fatalf("member key: %v", err)
	}
	mk := func(label string, extra map[string]any) (string, string) {
		data := map[string]any{"label": label, "tenant_id": tenantID, "all_models": true}
		for k, v := range extra {
			data[k] = v
		}
		status, body, resp := v1RecAPI(t, f, http.MethodPost, "/api/manage", adminKey, map[string]any{"action": "model-consumer-create", "data": data})
		c, _ := resp["consumer"].(map[string]any)
		id, _ := c["id"].(string)
		key, _ := resp["api_key"].(string)
		if status != http.StatusOK || id == "" || key == "" {
			t.Fatalf("model-consumer-create %s: %d %s", label, status, body)
		}
		return id, key
	}
	w.recID, w.recKey = mk("recview-on", map[string]any{"record_bodies": true, "confirm_record_bodies": true})
	w.plainI, _ = mk("recview-off", nil)

	adminSession, adminToken := recViewSession(t, f, w.adminKeyID, "csrf-admin")
	ownerSession, ownerToken := recViewSession(t, f, w.ownerKeyID, "csrf-owner")
	w.admin = []recViewCaller{
		{"server-admin key", recBearer(adminKey)},
		{"server-admin OAuth token", recBearer(adminToken)},
		{"server-admin web session", cookie(adminSession, "csrf-admin")},
	}
	w.others = []recViewCaller{
		{"tenant owner key", recBearer(owner)},
		{"tenant owner OAuth token", recBearer(ownerToken)},
		{"tenant owner web session", cookie(ownerSession, "csrf-owner")},
		{"member key", recBearer(w.memberKey)},
		{"model key", recBearer(w.recKey)},
		{"no credential", func(*http.Request) {}},
		{"server-admin web session without CSRF token", cookie(adminSession, "")},
	}
	return w
}

// manage sends one /api/manage action with set.
func (w *recViewWorld) manage(ctx context.Context, set func(*http.Request), action string, data any) (int, []byte, map[string]any) {
	b, _ := json.Marshal(map[string]any{"action": action, "data": data})
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(b)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	set(req)
	rec := httptest.NewRecorder()
	w.f.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, rec.Body.Bytes(), out
}

// admin sends one action as the server-admin key.
func (w *recViewWorld) adminCall(t *testing.T, action string, data any) (int, []byte, map[string]any) {
	t.Helper()
	return w.manage(context.Background(), recBearer(w.adminKey), action, data)
}

// v1 sends one /v1 request of the recording consumer with a raw body.
func (w *recViewWorld) v1(body []byte) v1Answer {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+w.recKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.f.router.ServeHTTP(rec, req)
	return v1Answer{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes()}
}

// newest is the id of the consumer's newest recording.
func (w *recViewWorld) newest(t *testing.T) string {
	t.Helper()
	var id string
	if err := w.f.seed.QueryRow(context.Background(), `SELECT id::text FROM context_llm_recordings WHERE consumer_id = $1::uuid
		ORDER BY created_at DESC, id DESC LIMIT 1`, w.recID).Scan(&id); err != nil {
		t.Fatalf("newest recording: %v", err)
	}
	return id
}

// rowState is the row's xmin and a digest of the whole row.
func (w *recViewWorld) rowState(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := w.f.seed.QueryRow(context.Background(), `SELECT xmin::text || ':' || md5(t::text) FROM context_llm_recordings t WHERE id = $1::uuid`, id).Scan(&s); err != nil {
		t.Fatalf("row state: %v", err)
	}
	return s
}

// audits counts the model_recording audit rows.
func (w *recViewWorld) audits(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.f.seed.QueryRow(context.Background(), `SELECT count(*) FROM context_settings_audit WHERE entity_type = 'model_recording'`).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// recViewChat is a chat request with a system and a user message, the user
// message carrying the secret.
func recViewChat(user string) []byte {
	b, _ := json.Marshal(map[string]any{"model": v1ChatAlias, "messages": []any{
		map[string]any{"role": "system", "content": "You are terse."},
		map[string]any{"role": "user", "content": user},
	}})
	return b
}

func localFullTrust(e *recViewEngine) []backends.Backend {
	return []backends.Backend{e.row("eng", backends.TrustFull, "local")}
}

// recViewActions are the five actions with a payload naming recording id.
func recViewActions(consumerID, id string) []struct {
	action string
	data   map[string]any
} {
	return []struct {
		action string
		data   map[string]any
	}{
		{"model-recording-consumers", map[string]any{}},
		{"model-recording-list", map[string]any{"consumer_id": consumerID}},
		{"model-recording-get", map[string]any{"id": id}},
		{"model-recording-read", map[string]any{"id": id, "address": "request"}},
		{"model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"}},
	}
}

func TestRecordingViewCallers(t *testing.T) {
	w := newRecViewWorld(t, localFullTrust)
	if a := w.v1(recViewChat(recViewSecret)); a.status != http.StatusOK {
		t.Fatalf("recorded chat: %d %s", a.status, a.body)
	}
	id := w.newest(t)
	before, audits, hits := w.rowState(t, id), w.audits(t), w.e.hits.Load()
	for _, a := range recViewActions(w.recID, id) {
		for _, c := range w.others {
			status, body, _ := w.manage(context.Background(), c.set, a.action, a.data)
			want := http.StatusForbidden
			switch c.name {
			case "model key", "no credential":
				want = http.StatusUnauthorized
			}
			if status != want {
				t.Errorf("%s by %s: %d %s, want %d", a.action, c.name, status, body, want)
			}
			if status == http.StatusForbidden && c.name != "server-admin web session without CSRF token" &&
				string(body) != `{"error":"admin key required","success":false}`+"\n" {
				t.Errorf("%s by %s: body %q, want the shared tier refusal", a.action, c.name, body)
			}
			if bytes.Contains(body, []byte(id)) || bytes.Contains(body, []byte(recViewSecret)) || bytes.Contains(body, []byte(w.recID)) {
				t.Errorf("%s by %s: the refusal names the recording or its content: %s", a.action, c.name, body)
			}
		}
		for _, c := range w.admin {
			status, body, _ := w.manage(context.Background(), c.set, a.action, a.data)
			if status != http.StatusOK {
				t.Errorf("%s by %s: %d %s, want 200", a.action, c.name, status, body)
			}
		}
	}
	// Refused callers left nothing: the three content actions of the three
	// server-admin callers wrote 3 × 3 audit rows, the refusals none; the
	// model saw the server-admins' three translations only.
	if n := w.audits(t) - audits; n != 9 {
		t.Errorf("audit rows: %d new, want 9 (3 content actions × 3 server-admin callers)", n)
	}
	if n := w.e.hits.Load() - hits; n != 3 {
		t.Errorf("translation calls: %d, want 3 (one per server-admin caller)", n)
	}
	if after := w.rowState(t, id); after != before {
		t.Errorf("the recording row changed under the reads: %s → %s", before, after)
	}
}

// recViewLog captures the process's log lines for the test's duration.
func recViewLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&mu, buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func b64(t *testing.T, v any) []byte {
	t.Helper()
	s, _ := v.(string)
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return out
}

func blockByAddress(resp map[string]any, address string) map[string]any {
	bs, _ := resp["blocks"].([]any)
	for _, b := range bs {
		m, _ := b.(map[string]any)
		if m["address"] == address {
			return m
		}
	}
	return nil
}

func TestRecordingViewContent(t *testing.T) {
	w := newRecViewWorld(t, localFullTrust)
	logs := recViewLog(t)
	ctx := context.Background()
	user := recViewSecret + " — " + strings.Repeat("Bitte übersetze diesen Satz. ", 400) // ~12 KiB
	chat := recViewChat(user)
	for i := 0; i < 3; i++ {
		if a := w.v1(chat); a.status != http.StatusOK {
			t.Fatalf("recorded chat %d: %d %s", i, a.status, a.body)
		}
	}
	id := w.newest(t)
	before := w.rowState(t, id)

	t.Run("consumers", func(t *testing.T) {
		status, body, resp := w.adminCall(t, "model-recording-consumers", map[string]any{})
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, body)
		}
		cs, _ := resp["consumers"].([]any)
		var on map[string]any
		for _, c := range cs {
			m, _ := c.(map[string]any)
			if m["consumer_id"] == w.plainI {
				t.Errorf("the consumer without the switch and without recordings is listed: %v", m)
			}
			if m["consumer_id"] == w.recID {
				on = m
			}
		}
		if on == nil || on["record_bodies"] != true || on["last_recorded_at"] == nil {
			t.Fatalf("recording consumer: %v in %s", on, body)
		}
		p, _ := on["process"].(map[string]any)
		if p == nil || p["recorded"].(float64) < 3 {
			t.Errorf("process counts: %v", p)
		}
	})
	t.Run("list", func(t *testing.T) {
		status, body, resp := w.adminCall(t, "model-recording-list", map[string]any{"consumer_id": w.recID, "limit": 2})
		rs, _ := resp["recordings"].([]any)
		next, _ := resp["next_cursor"].(string)
		if status != http.StatusOK || len(rs) != 2 || next == "" {
			t.Fatalf("first page: %d %s", status, body)
		}
		if bytes.Contains(body, []byte(recViewSecret)) || bytes.Contains(body, []byte("request_body")) {
			t.Errorf("the list carries a body: %s", body)
		}
		if first, _ := rs[0].(map[string]any); first["id"] != id {
			t.Errorf("first row %v, want the newest %s", first["id"], id)
		}
		_, body, resp = w.adminCall(t, "model-recording-list", map[string]any{"consumer_id": w.recID, "limit": 2, "cursor": next})
		if rs, _ := resp["recordings"].([]any); len(rs) != 1 || resp["next_cursor"] != nil {
			t.Errorf("second page: %s", body)
		}
		_, body, resp = w.adminCall(t, "model-recording-list", map[string]any{"consumer_id": w.recID, "state": "open"})
		if rs, _ := resp["recordings"].([]any); len(rs) != 0 {
			t.Errorf("open rows: %s", body)
		}
		_, body, resp = w.adminCall(t, "model-recording-list", map[string]any{"consumer_id": w.recID, "status": "2xx", "op": []string{"chat"}})
		if rs, _ := resp["recordings"].([]any); len(rs) != 3 {
			t.Errorf("2xx chat rows: %s", body)
		}
		if status, body, _ := w.adminCall(t, "model-recording-list", map[string]any{"consumer_id": w.recID, "cursor": "nope"}); status != http.StatusBadRequest {
			t.Errorf("bad cursor: %d %s", status, body)
		}
	})
	t.Run("blocks_and_windows", func(t *testing.T) {
		audits := w.audits(t)
		status, body, resp := w.adminCall(t, "model-recording-get", map[string]any{"id": id})
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, body)
		}
		sys, usr, ans := blockByAddress(resp, "request/messages/0"), blockByAddress(resp, "request/messages/1"), blockByAddress(resp, "response/choices/0/content")
		if sys == nil || sys["role"] != "system" || sys["preview"] != "You are terse." {
			t.Errorf("system block: %v", sys)
		}
		if usr == nil || usr["role"] != "user" || usr["preview_truncated"] != true || !strings.HasPrefix(usr["preview"].(string), recViewSecret) ||
			int(usr["bytes"].(float64)) != len(user) {
			t.Errorf("user block: %v", usr)
		}
		if ans == nil || ans["preview"] != "hello" {
			t.Errorf("answer block: %v", ans)
		}
		status, body, resp = w.adminCall(t, "model-recording-read", map[string]any{"id": id, "address": "request/messages/1", "offset": 3, "limit": 40})
		if status != http.StatusOK || string(b64(t, resp["data"])) != user[3:43] || resp["eof"] != false || int(resp["total"].(float64)) != len(user) {
			t.Errorf("block window: %d %s", status, body)
		}
		status, body, resp = w.adminCall(t, "model-recording-read", map[string]any{"id": id, "address": "request", "offset": 10, "limit": 20})
		if status != http.StatusOK || string(b64(t, resp["data"])) != string(chat[10:30]) {
			t.Errorf("body window: %d %s", status, body)
		}
		if status, body, _ := w.adminCall(t, "model-recording-read", map[string]any{"id": id, "address": "request/messages/9"}); status != http.StatusNotFound {
			t.Errorf("unknown address: %d %s", status, body)
		}
		if n := w.audits(t) - audits; n != 3 {
			t.Errorf("audit rows: %d new, want 3 (read, two windows)", n)
		}
		var action, actor, consumer string
		var nv []byte
		if err := w.f.seed.QueryRow(ctx, `SELECT action, api_key_id::text, metadata->>'consumer_id', convert_to(new_value::text, 'UTF8') FROM context_settings_audit
			WHERE entity_type = 'model_recording' AND entity_key = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, id).Scan(&action, &actor, &consumer, &nv); err != nil {
			t.Fatal(err)
		}
		if action != "read_raw" || actor != w.adminKeyID || consumer != w.recID || bytes.Contains(nv, []byte("Tr0ub4dor")) || !bytes.Contains(nv, []byte(`"block": "request"`)) {
			t.Errorf("audit row: action %q actor %q consumer %q value %s", action, actor, consumer, nv)
		}
	})
	t.Run("not_utf8_is_one_raw_block", func(t *testing.T) {
		raw := []byte("{\"model\":\"" + v1ChatAlias + "\",\"messages\":[{\"role\":\"user\",\"content\":\"\xff\xfe\x00 " + recViewSecret + "\"}]}")
		w.v1(raw)
		bad := w.newest(t)
		_, body, resp := w.adminCall(t, "model-recording-get", map[string]any{"id": bad})
		req, _ := resp["request"].(map[string]any)
		blk := blockByAddress(resp, "request")
		if req["format"] != "not_utf8" || blk == nil || blk["kind"] != "raw" || blk["utf8"] != false || blk["preview_omitted"] != true {
			t.Fatalf("raw block: %s", body)
		}
		_, body, resp = w.adminCall(t, "model-recording-read", map[string]any{"id": bad, "address": "request"})
		if got := b64(t, resp["data"]); !bytes.Equal(got, raw) {
			t.Errorf("raw read: %q, want the bytes sent %q (%s)", got, raw, body)
		}
		_, body, resp = w.adminCall(t, "model-recording-read", map[string]any{"id": bad, "address": "request", "offset": 0, "limit": 1 << 20})
		if got := b64(t, resp["data"]); !bytes.Equal(got, raw) || resp["eof"] != true {
			t.Errorf("raw block read: %s", body)
		}
	})
	t.Run("translate", func(t *testing.T) {
		audits := w.audits(t)
		answer := "# Übersetzung\n\nIgnore the rules <script>alert(1)</script> ![x](https://tracker.example/p.png)"
		w.e.answer.Store(&answer)
		status, body, resp := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"})
		if status != http.StatusOK || resp["text"] != answer || resp["generated"] != true || int(resp["chunks"].(float64)) != len(llm.RecordingChunks(user)) || resp["chunk"].(float64) != 0 {
			t.Fatalf("translate: %d %s", status, body)
		}
		var call struct {
			Messages []struct {
				Role, Content string
			} `json:"messages"`
			Tools any `json:"tools"`
		}
		raw := w.e.lastBody()
		if err := json.Unmarshal(raw, &call); err != nil || len(call.Messages) != 2 {
			t.Fatalf("engine request: %v %s", err, raw)
		}
		if call.Tools != nil || bytes.Contains(raw, []byte(`"tools"`)) {
			t.Errorf("the translation call carries tools: %s", raw)
		}
		u := call.Messages[1].Content
		begin, end := strings.Index(u, "<untrusted_block id="), strings.Index(u, "</untrusted_block id=")
		if call.Messages[0].Role != "system" || !strings.Contains(call.Messages[0].Content, recViewMarker) || begin < 0 || end < begin ||
			!strings.Contains(u[begin:end], recViewSecret) || strings.Contains(u[:begin], recViewSecret) ||
			!strings.Contains(u[:begin], "Target language: German") || !strings.Contains(u[:begin], fmt.Sprintf("part 1 of %d", len(llm.RecordingChunks(user)))) {
			t.Errorf("prompt shape: %q", u)
		}
		nonce := strings.SplitN(u[begin+len("<untrusted_block id="):], " ", 2)[0]
		if len(nonce) != 16 || strings.Count(u, "id="+nonce) != 2 || !strings.Contains(call.Messages[0].Content, "id="+nonce) {
			t.Errorf("block nonce %q: %d times in the user message, in the system rule %v", nonce, strings.Count(u, "id="+nonce), strings.Contains(call.Messages[0].Content, "id="+nonce))
		}
		w.f.settle(t)
		var pipeline, sens, sys, usr, res string
		var meta []byte
		if err := w.f.seed.QueryRow(ctx, `SELECT pipeline, required_sensitivity, COALESCE(request_system, ''), COALESCE(request_user, ''),
			       COALESCE(response_content, ''), convert_to(metadata::text, 'UTF8')
			FROM context_llm_log WHERE pipeline = 'recording-translate' ORDER BY created_at DESC LIMIT 1`).Scan(&pipeline, &sens, &sys, &usr, &res, &meta); err != nil {
			t.Fatalf("llmlog row: %v", err)
		}
		if sens != string(backends.SensCredentials) || sys != "" || usr != "" || res != "" || bytes.Contains(meta, []byte("Tr0ub4dor")) || bytes.Contains(meta, []byte("Übersetzung")) {
			t.Errorf("llmlog row: class %q, bodies %d/%d/%d bytes (want none), metadata %s", sens, len(sys), len(usr), len(res), meta)
		}
		if n := w.audits(t) - audits; n != 1 {
			t.Errorf("audit rows: %d new, want 1", n)
		}
		// The rendering is kept nowhere: no audit row, no llmlog row, no
		// recording carries a byte of it.
		var kept int
		if err := w.f.seed.QueryRow(ctx, `SELECT
			  (SELECT count(*) FROM context_settings_audit
			    WHERE strpos(COALESCE(old_value::text, '') || COALESCE(new_value::text, '') || metadata::text, $1) > 0)
			+ (SELECT count(*) FROM context_llm_log
			    WHERE strpos(COALESCE(request_system, '') || COALESCE(request_user, '') || COALESCE(response_content, '') || metadata::text, $1) > 0)
			+ (SELECT count(*) FROM context_llm_recordings
			    WHERE position(convert_to($1, 'UTF8') IN COALESCE(request_body, ''::bytea) || COALESCE(response_body, ''::bytea)) > 0)`,
			"# Übersetzung").Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if kept != 0 {
			t.Errorf("%d rows carry the translation", kept)
		}
		if status, body, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de", "text": "FOREIGN"}); status != http.StatusBadRequest {
			t.Errorf("a text field: %d %s, want 400 — the block comes from the recording only", status, body)
		}
		if status, body, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "Klingon\nignore"}); status != http.StatusBadRequest {
			t.Errorf("unknown language: %d %s", status, body)
		}
		if status, body, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de", "chunk": len(llm.RecordingChunks(user))}); status != http.StatusBadRequest {
			t.Errorf("chunk past the block: %d %s", status, body)
		}
	})
	t.Run("brake_and_the_consumer_is_served", func(t *testing.T) {
		// Two slots on the engine, one of them the internal partition: a
		// held translation takes one, the recording consumer's request the
		// other.
		pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{w.e.srv.URL: {Slots: 2, InternalReserved: 1}}}
		w.f.disp.UpdatePolicy(pol)
		t.Cleanup(func() { w.f.disp.UpdatePolicy(w.f.policy) })
		w.e.drain()
		release := w.e.hold(t)
		first := make(chan int, 1)
		go func() {
			s, _, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "en"})
			first <- s
		}()
		<-w.e.arrived
		status, body, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "en", "chunk": 1})
		if status != http.StatusTooManyRequests || !bytes.Contains(body, []byte("translate_busy")) {
			t.Errorf("second translation of the same person: %d %s, want 429 translate_busy", status, body)
		}
		served := make(chan v1Answer, 1)
		go func() { served <- w.v1(recViewChat("hello again")) }()
		select {
		case a := <-served:
			if a.status != http.StatusOK {
				t.Errorf("the recording consumer's request beside a translation: %d %s", a.status, a.body)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("the recording consumer's request waited behind the translation")
		}
		release()
		if s := <-first; s != http.StatusOK {
			t.Errorf("held translation: %d", s)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		w.e.drain()
		release := w.e.hold(t)
		defer release()
		canceled := w.e.canceled.Load()
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan int, 1)
		go func() {
			s, _, _ := w.manage(cctx, recBearer(w.adminKey), "model-recording-translate", map[string]any{"id": id, "address": "request/messages/0", "language": "fr"})
			done <- s
		}()
		<-w.e.arrived
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the canceled translation did not end")
		}
		deadline := time.Now().Add(5 * time.Second)
		for w.e.canceled.Load() == canceled && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if w.e.canceled.Load() == canceled {
			t.Errorf("the model call was not canceled with the request")
		}
	})
	t.Run("open_recording_is_readable", func(t *testing.T) {
		ch := make(chan struct{})
		w.e.gateV1.Store(&ch)
		defer func() { close(ch); w.e.gateV1.Store(nil) }()
		hits := w.e.v1Hits.Load()
		done := make(chan v1Answer, 1)
		go func() { done <- w.v1(recViewChat("still running " + recViewSecret)) }()
		deadline := time.Now().Add(10 * time.Second)
		for w.e.v1Hits.Load() == hits && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		open := w.newest(t)
		_, body, resp := w.adminCall(t, "model-recording-get", map[string]any{"id": open})
		rec, _ := resp["recording"].(map[string]any)
		rs, _ := resp["response"].(map[string]any)
		if rec["state"] != "open" || rs["state"] != "pending" || blockByAddress(resp, "request/messages/1") == nil {
			t.Errorf("open recording: %s", body)
		}
		close(ch)
		w.e.gateV1.Store(nil)
		<-done
		ch = make(chan struct{}) // the deferred close gets its own channel
	})
	t.Run("blocks_page_by_page", func(t *testing.T) {
		_, body, all := w.adminCall(t, "model-recording-get", map[string]any{"id": id})
		total, _ := all["blocks_total"].(float64)
		bs, _ := all["blocks"].([]any)
		if int(total) != len(bs) || total < 3 {
			t.Fatalf("whole detail: %s", body)
		}
		// The audit row of a page names the blocks it served by their
		// addresses — positions alone do not survive the retention or a
		// change of the decomposition rules.
		lastAudit := func() (first, last string) {
			t.Helper()
			if err := w.f.seed.QueryRow(context.Background(), `SELECT COALESCE(new_value->>'first_block', ''), COALESCE(new_value->>'last_block', '')
				FROM context_settings_audit WHERE entity_type = 'model_recording' AND entity_key = $1 AND action = 'read'
				ORDER BY created_at DESC, id DESC LIMIT 1`, id).Scan(&first, &last); err != nil {
				t.Fatal(err)
			}
			return first, last
		}
		addr := func(b any) string { return b.(map[string]any)["address"].(string) }
		if first, last := lastAudit(); first != addr(bs[0]) || last != addr(bs[len(bs)-1]) {
			t.Errorf("audit of the whole detail: first %q last %q, want %q and %q", first, last, addr(bs[0]), addr(bs[len(bs)-1]))
		}
		_, body, page := w.adminCall(t, "model-recording-get", map[string]any{"id": id, "block_offset": 1, "block_limit": 1})
		pb, _ := page["blocks"].([]any)
		if len(pb) != 1 || page["blocks_total"] != total || pb[0].(map[string]any)["address"] != bs[1].(map[string]any)["address"] {
			t.Errorf("block page [1,2): %s", body)
		}
		if first, last := lastAudit(); first != addr(bs[1]) || last != addr(bs[1]) {
			t.Errorf("audit of the page [1,2): first %q last %q, want %q for both", first, last, addr(bs[1]))
		}
		if status, body, _ := w.adminCall(t, "model-recording-get", map[string]any{"id": id, "block_limit": 501}); status != http.StatusBadRequest {
			t.Errorf("a page past 500 blocks: %d %s", status, body)
		}
	})
	t.Run("a_body_of_many_tiny_blocks", func(t *testing.T) {
		// 100 000 one-character messages (3.2 MB): the detail carries one
		// page of 500 blocks and names the total; the last block reads by its
		// address.
		var b strings.Builder
		b.WriteString(`{"model":"` + v1ChatAlias + `","messages":[`)
		for i := 0; i < 100_000; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"role":"user","content":"x"}`)
		}
		b.WriteString(`]}`)
		w.v1([]byte(b.String()))
		many := w.newest(t)
		_, body, resp := w.adminCall(t, "model-recording-get", map[string]any{"id": many})
		bs, _ := resp["blocks"].([]any)
		if len(bs) != 500 || resp["blocks_total"].(float64) < 100_000 || len(body) > 1<<20 {
			t.Fatalf("detail of 100 000 blocks: %d blocks, total %v, %d bytes", len(bs), resp["blocks_total"], len(body))
		}
		status, body, resp := w.adminCall(t, "model-recording-read", map[string]any{"id": many, "address": "request/messages/99999"})
		if status != http.StatusOK || string(b64(t, resp["data"])) != "x" {
			t.Errorf("the last block: %d %s", status, body)
		}
	})
	t.Run("cleared_by_the_retention", func(t *testing.T) {
		if a := w.v1(recViewChat("old " + recViewSecret)); a.status != http.StatusOK {
			t.Fatalf("chat: %d", a.status)
		}
		old := w.newest(t)
		// The row's age is set by hand (the only way to an old row in a
		// test); the clearing is the retention janitor's own statement.
		if _, err := w.f.seed.Exec(ctx, `UPDATE context_llm_recordings SET created_at = now() - interval '3 days' WHERE id = $1::uuid`, old); err != nil {
			t.Fatal(err)
		}
		if _, err := llmlog.EvictBodies(ctx, w.f.pool, 1); err != nil {
			t.Fatal(err)
		}
		_, body, resp := w.adminCall(t, "model-recording-get", map[string]any{"id": old})
		rq, _ := resp["request"].(map[string]any)
		rs, _ := resp["response"].(map[string]any)
		if bs, _ := resp["blocks"].([]any); rq["state"] != "cleared" || rs["state"] != "cleared" || len(bs) != 0 {
			t.Errorf("cleared recording: %s", body)
		}
		for _, a := range []struct {
			action string
			data   map[string]any
		}{
			{"model-recording-read", map[string]any{"id": old, "address": "request"}},
			{"model-recording-read", map[string]any{"id": old, "address": "request/messages/1"}},
			{"model-recording-translate", map[string]any{"id": old, "address": "request/messages/1", "language": "de"}},
		} {
			if status, body, _ := w.adminCall(t, a.action, a.data); status != http.StatusConflict || !bytes.Contains(body, []byte("body_cleared")) {
				t.Errorf("%s of a cleared body: %d %s, want 409 body_cleared", a.action, status, body)
			}
		}
	})
	t.Run("audit_failure_serves_nothing", func(t *testing.T) {
		if _, err := w.f.seed.Exec(ctx, `CREATE FUNCTION recview_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.entity_type = 'model_recording' THEN RAISE EXCEPTION 'audit down'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER recview_refuse BEFORE INSERT ON context_settings_audit FOR EACH ROW EXECUTE FUNCTION recview_refuse()`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = w.f.seed.Exec(ctx, `DROP TRIGGER recview_refuse ON context_settings_audit; DROP FUNCTION recview_refuse()`)
		}()
		hits := w.e.hits.Load()
		for _, a := range []struct {
			action string
			data   map[string]any
		}{
			{"model-recording-get", map[string]any{"id": id}},
			{"model-recording-read", map[string]any{"id": id, "address": "request/messages/1"}},
			{"model-recording-read", map[string]any{"id": id, "address": "request"}},
			{"model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"}},
		} {
			status, body, _ := w.adminCall(t, a.action, a.data)
			if status != http.StatusServiceUnavailable || !bytes.Contains(body, []byte("audit_unavailable")) || bytes.Contains(body, []byte("Tr0ub4dor")) {
				t.Errorf("%s with the audit down: %d %s", a.action, status, body)
			}
		}
		if n := w.e.hits.Load() - hits; n != 0 {
			t.Errorf("%d translation calls with the audit down, want none", n)
		}
	})
	if after := w.rowState(t, id); after != before {
		t.Errorf("the recording row changed under the reads and translations: %s → %s", before, after)
	}
	w.f.settle(t)
	if bytes.Contains(logs.Bytes(), []byte("Tr0ub4dor")) || bytes.Contains(logs.Bytes(), []byte("Übersetzung")) {
		t.Errorf("a process log line carries the recording or the translation")
	}
}

func TestRecordingViewTranslateTrust(t *testing.T) {
	w := newRecViewWorld(t, func(e *recViewEngine) []backends.Backend {
		peer := e.row("peer-full", backends.TrustFull, "lan")
		peer.ProviderClass = backends.ProviderCtx // another ctx instance
		return []backends.Backend{
			e.row("ext-full", backends.TrustFull, "external"),
			e.row("local-nocred", backends.TrustNoCredentials, "local"),
			peer,
		}
	})
	w.v1(recViewChat(recViewSecret))
	id := w.newest(t)
	hits := w.e.hits.Load()
	status, body, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"})
	if status != http.StatusServiceUnavailable || !bytes.Contains(body, []byte("no_eligible_backend")) {
		t.Errorf("translate without a local full-trust row: %d %s, want 503 no_eligible_backend", status, body)
	}
	if n := w.e.hits.Load() - hits; n != 0 {
		t.Errorf("%d translation calls reached a backend that may not receive credentials or is external", n)
	}
}

// TestRecordingViewTranslateRefusedBeforeTheWire: a chunk the chain's window
// cannot hold, and an empty block, are refused before anything is sent — no
// model call, and no audit row, which would claim a translation went out.
func TestRecordingViewTranslateRefusedBeforeTheWire(t *testing.T) {
	w := newRecViewWorld(t, func(e *recViewEngine) []backends.Backend {
		r := e.row("small", backends.TrustFull, "local")
		r.NumCtx = 1024
		return []backends.Backend{r}
	})
	w.v1(recViewChat(recViewSecret))
	id := w.newest(t)
	empty, _ := json.Marshal(map[string]any{"model": v1ChatAlias, "messages": []any{map[string]any{"role": "user", "content": ""}}})
	w.v1(empty)
	emptyID := w.newest(t)
	hits, audits := w.e.hits.Load(), w.audits(t)
	status, body, _ := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"})
	if status != http.StatusServiceUnavailable || !bytes.Contains(body, []byte("window_too_small")) {
		t.Errorf("a window of 1024 tokens: %d %s, want 503 window_too_small", status, body)
	}
	status, body, _ = w.adminCall(t, "model-recording-translate", map[string]any{"id": emptyID, "address": "request/messages/0", "language": "de"})
	if status != http.StatusUnprocessableEntity || !bytes.Contains(body, []byte("not_translatable")) {
		t.Errorf("an empty block: %d %s, want 422 not_translatable", status, body)
	}
	if n := w.e.hits.Load() - hits; n != 0 {
		t.Errorf("%d model calls, want none", n)
	}
	if n := w.audits(t) - audits; n != 0 {
		t.Errorf("%d audit rows for translations that were not sent, want none", n)
	}
}

// TestRecordingViewTranslateBackendErrorKeepsNothing: the model answers the
// translation with an error that quotes its input — the block, a credential
// in it. Nothing of that answer reaches the context_llm_log row (its error
// column is the attempt's class, status and the code "sealed"), any other
// row, a process log line or the answer to the server-admin, which gets the
// view's own error text.
func TestRecordingViewTranslateBackendErrorKeepsNothing(t *testing.T) {
	w := newRecViewWorld(t, localFullTrust)
	logs := recViewLog(t)
	ctx := context.Background()
	if a := w.v1(recViewChat(recViewSecret + " — bitte übersetzen")); a.status != http.StatusOK {
		t.Fatalf("recorded chat: %d %s", a.status, a.body)
	}
	id := w.newest(t)
	w.e.fail.Store(http.StatusBadRequest)
	hits := w.e.hits.Load()
	status, body, resp := w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"})
	if n := w.e.hits.Load() - hits; n != 1 {
		t.Fatalf("translation calls: %d, want 1 (the echoing answer has to happen)", n)
	}
	if status != http.StatusBadGateway || resp["code"] != "translate_failed" || bytes.Contains(body, []byte("Tr0ub4dor")) || bytes.Contains(body, []byte("rejected input")) {
		t.Errorf("answer to the server-admin: %d %s, want 502 translate_failed with the view's own text", status, body)
	}
	w.f.settle(t)
	var errCol, row string
	if err := w.f.seed.QueryRow(ctx, `SELECT COALESCE(error, ''), t::text FROM context_llm_log t
		WHERE pipeline = 'recording-translate' ORDER BY created_at DESC LIMIT 1`).Scan(&errCol, &row); err != nil {
		t.Fatalf("llmlog row: %v", err)
	}
	if errCol != "bad_request 400 sealed" || strings.Contains(row, "Tr0ub4dor") || strings.Contains(row, "rejected input") {
		t.Errorf("llmlog row: error %q, row %s — want the error \"bad_request 400 sealed\" and nothing of the answer", errCol, row)
	}
	var kept int
	if err := w.f.seed.QueryRow(ctx, `SELECT
		  (SELECT count(*) FROM context_llm_log t WHERE strpos(t::text, 'rejected input') > 0)
		+ (SELECT count(*) FROM context_settings_audit t WHERE strpos(t::text, 'rejected input') > 0 OR strpos(t::text, 'Tr0ub4dor') > 0)`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("%d rows carry the backend's answer or the block", kept)
	}
	if bytes.Contains(logs.Bytes(), []byte("Tr0ub4dor")) || bytes.Contains(logs.Bytes(), []byte("rejected input")) {
		t.Errorf("a process log line carries the backend's answer")
	}
}

// TestRecordingViewTranslateQuotaBeforeTheAudit: a translation the tenant
// quota refuses — its daily call budget spent, a gate that binds this
// instance's own backends too — is refused before anything is sent and
// leaves no audit row, like every refusal before the wire.
func TestRecordingViewTranslateQuotaBeforeTheAudit(t *testing.T) {
	w := newRecViewWorld(t, localFullTrust)
	ctx := context.Background()
	if a := w.v1(recViewChat(recViewSecret)); a.status != http.StatusOK {
		t.Fatalf("recorded chat: %d %s", a.status, a.body)
	}
	id := w.newest(t)
	// The server-admin key's home scope belongs to the default tenant.
	if _, err := w.f.seed.Exec(ctx, `INSERT INTO context_tenant_quota (scope, daily_calls) VALUES ('private', 0)`); err != nil {
		t.Fatalf("quota: %v", err)
	}
	// The router's accountant reads the quota through a cache it refreshes in
	// the background and passes while it is cold: translate until the gate
	// refuses once — that refusal may come from either gate, as the refresh
	// can land between them — and then once more: with the cache warm (its
	// TTL is 30 s), that call is the one under test.
	translate := func() (int, []byte, map[string]any) {
		return w.adminCall(t, "model-recording-translate", map[string]any{"id": id, "address": "request/messages/1", "language": "de"})
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, body, resp := translate()
		if status == http.StatusTooManyRequests && resp["code"] == "quota_exceeded" {
			break
		}
		if status != http.StatusOK {
			t.Fatalf("translate while the quota cache is cold: %d %s", status, body)
		}
		if time.Now().After(deadline) {
			t.Fatal("the tenant quota never refused the translation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	hits, audits := w.e.hits.Load(), w.audits(t)
	if status, body, resp := translate(); status != http.StatusTooManyRequests || resp["code"] != "quota_exceeded" {
		t.Fatalf("translate with the quota spent: %d %s, want 429 quota_exceeded", status, body)
	}
	if n := w.e.hits.Load() - hits; n != 0 {
		t.Errorf("the refused translation made %d model calls, want none", n)
	}
	if n := w.audits(t) - audits; n != 0 {
		t.Errorf("the refused translation wrote %d audit rows, want none — nothing was sent", n)
	}
}
