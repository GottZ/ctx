package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// fakeStatus is an in-memory statusProvider: it returns a canned snapshot and
// counts the broadcast refreshes + loop starts so the hub tests need no DB.
type fakeStatus struct {
	mu        sync.Mutex
	snap      statusResponse
	refreshes int
	loops     int
}

func (f *fakeStatus) Snapshot(context.Context) statusResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeStatus) refreshForBroadcast(context.Context) statusResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	return f.snap
}

func (f *fakeStatus) setBroadcasting(on bool) {
	if !on {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loops++
}

func (f *fakeStatus) set(s statusResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
}

func (f *fakeStatus) loopCount() int   { f.mu.Lock(); defer f.mu.Unlock(); return f.loops }
func (f *fakeStatus) refreshCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.refreshes }

// noLLM is the injected llmcalls seam for tests with no telemetry rows.
func noLLM(_ context.Context, cursor time.Time, _ int) ([]llmlogEntry, time.Time) {
	return nil, cursor
}

// eventsCfgStore builds a real config.Store carrying only the events/llmlog knobs
// the SSE layer reads.
func eventsCfgStore(tick, ping time.Duration, maxConn int) *config.Store {
	return config.NewStore(&config.Config{
		Events: config.EventsConfig{TickInterval: tick, PingInterval: ping, MaxConnections: maxConn},
		LLMLog: config.LLMLogConfig{MaxLimit: 200},
	})
}

func newTestHub(life context.Context, fs *fakeStatus, store *config.Store) *sseHub {
	return &sseHub{
		status:       fs,
		cfg:          store,
		life:         life,
		llmcalls:     noLLM,
		authenticate: func(context.Context, string) (*auth.AuthResult, error) { return &auth.AuthResult{IsValid: true, IsAdmin: true}, nil },
		subs:         map[*sseSub]struct{}{},
	}
}

// TestSSEWriterFrameFormat pins the wire framing: id is optional, the event
// name and data line are present, and every frame is terminated by a blank line.
func TestSSEWriterFrameFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	if err := sw.event("status", "42", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := sw.ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := sw.event("backends", "", []byte(`[]`)); err != nil {
		t.Fatalf("event no-id: %v", err)
	}
	got := rec.Body.String()
	want := "id: 42\nevent: status\ndata: {\"a\":1}\n\n" +
		": ping\n\n" +
		"event: backends\ndata: []\n\n"
	if got != want {
		t.Errorf("frame format:\n got %q\nwant %q", got, want)
	}
}

// TestSSEWriterOutlivesServerWriteTimeout is the rolling-deadline regression
// (the SSE sibling of TestHeartbeatOutlivesServerWriteTimeout): a stream that
// keeps writing must survive past http.Server's ABSOLUTE WriteTimeout because
// every frame rolls the connection write deadline forward.
func TestSSEWriterOutlivesServerWriteTimeout(t *testing.T) {
	orig := sseWriteWindow
	sseWriteWindow = 400 * time.Millisecond
	defer func() { sseWriteWindow = orig }()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sw := newSSEWriter(w)
		deadline := time.After(900 * time.Millisecond) // 3x the server WriteTimeout
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-deadline:
				_ = sw.event("status", "1", []byte(`{"ok":"made-it"}`))
				return
			case <-tk.C:
				if sw.ping() != nil {
					return
				}
			}
		}
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body (connection killed by WriteTimeout?): %v", err)
	}
	if !strings.Contains(string(body), "made-it") {
		t.Errorf("payload missing, got %d bytes: %q", len(body), string(body))
	}
}

// TestStatusDiffKeyExcludesAsOf proves the status diff ignores the ever-ticking
// as_of but reacts to a real field change — otherwise status would fire every
// tick.
func TestStatusDiffKeyExcludesAsOf(t *testing.T) {
	base := statusEvent{AsOf: time.Unix(1000, 0), Dream: dreamStatus{Mode: "on"}}
	later := base
	later.AsOf = time.Unix(2000, 0)
	if string(base.diffKey()) != string(later.diffKey()) {
		t.Error("as_of change must NOT alter the diff key")
	}
	changed := base
	changed.Dream.Mode = "throttled"
	if string(base.diffKey()) == string(changed.diffKey()) {
		t.Error("dream mode change MUST alter the diff key")
	}
}

// TestBackendsDiffKeyExcludesCooldown proves a cooldown countdown / health-probe
// timestamp does not churn the backends event, but a real state change does.
func TestBackendsDiffKeyExcludesCooldown(t *testing.T) {
	base := []backends.BackendStatus{{Name: "herbert-chat", EffectiveState: "cooldown", CooldownRemaining: 42, LastOK: "t1"}}
	ticked := []backends.BackendStatus{{Name: "herbert-chat", EffectiveState: "cooldown", CooldownRemaining: 41, LastOK: "t2"}}
	if string(backendsDiffKey(base)) != string(backendsDiffKey(ticked)) {
		t.Error("cooldown_remaining_s / last_ok ticking must NOT churn the backends diff")
	}
	recovered := []backends.BackendStatus{{Name: "herbert-chat", EffectiveState: "active", CooldownRemaining: 0, LastOK: "t3"}}
	if string(backendsDiffKey(base)) == string(backendsDiffKey(recovered)) {
		t.Error("effective_state change MUST alter the backends diff")
	}
}

// TestCredentialFromRequest pins the shared RAW extraction: X-Context-Key
// wins, Bearer is the fallback, empty stays empty — and the ctxt_/ctxr_
// prefix SURVIVES extraction from either header (S3: sanitizing here would
// hex-strip an opaque token into the raw-key path, design 03 §4
// RVW-Vollst-F6; SanitizeKey now lives inside resolveCredential's raw-key
// branch only).
func TestCredentialFromRequest(t *testing.T) {
	mk := func(set func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
		set(r)
		return r
	}
	bearerKey := "dead00" + "beef" // assembled (repo rule: no secret-shaped literals)
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"x-context-key", mk(func(r *http.Request) { r.Header.Set("X-Context-Key", "abc123") }), "abc123"},
		{"bearer", mk(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+bearerKey) }), bearerKey},
		{"x-key-wins", mk(func(r *http.Request) {
			r.Header.Set("X-Context-Key", "aaaa")
			r.Header.Set("Authorization", "Bearer bbbb")
		}), "aaaa"},
		{"token-prefix-survives-x-header", mk(func(r *http.Request) { r.Header.Set("X-Context-Key", "ctxt_abc123") }), "ctxt_abc123"},
		{"token-prefix-survives-bearer", mk(func(r *http.Request) { r.Header.Set("Authorization", "Bearer ctxr_abc123") }), "ctxr_abc123"},
		{"trimmed", mk(func(r *http.Request) { r.Header.Set("X-Context-Key", " abc123 ") }), "abc123"},
		{"empty", mk(func(r *http.Request) {}), ""},
	}
	for _, c := range cases {
		if got := credentialFromRequest(c.r); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSSEHubCapRefusesOverLimit proves events.max_connections is enforced and a
// freed slot is reusable.
func TestSSEHubCapRefusesOverLimit(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	h := newTestHub(life, fs, eventsCfgStore(time.Second, time.Second, 2))

	s1, ok1 := h.subscribe()
	_, ok2 := h.subscribe()
	_, ok3 := h.subscribe()
	if !ok1 || !ok2 {
		t.Fatal("first two subscribes (cap=2) must succeed")
	}
	if ok3 {
		t.Error("third subscribe must be refused at cap=2 (→ 429)")
	}
	h.unsubscribe(s1)
	if _, ok := h.subscribe(); !ok {
		t.Error("a freed slot must be reusable after unsubscribe")
	}
}

// TestSSEHubOneLoopForManyConns is the W6/W8 proof: N connections start exactly
// ONE broadcast loop (one snapshot build per tick fans out to all), not one
// loop per connection.
func TestSSEHubOneLoopForManyConns(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	h := newTestHub(life, fs, eventsCfgStore(10*time.Millisecond, time.Second, 8))

	for i := 0; i < 4; i++ {
		if _, ok := h.subscribe(); !ok {
			t.Fatalf("subscribe %d failed", i)
		}
	}
	// Wait for the single loop goroutine to start (setBroadcasting(true)).
	waitFor(t, time.Second, func() bool { return fs.loopCount() == 1 })
	// Let several ticks pass; the loop count must stay 1 and refreshes must
	// advance (the one loop is alive and refreshing once per tick).
	time.Sleep(80 * time.Millisecond)
	if got := fs.loopCount(); got != 1 {
		t.Errorf("loop count = %d, want exactly 1 for 4 connections", got)
	}
	if fs.refreshCount() == 0 {
		t.Error("the broadcast loop never refreshed — it is not running")
	}
}

// TestSSEHubDiffOnlyOnChange proves the loop broadcasts status/backends only
// when the meaningful content changes — a stable snapshot (as_of aside) yields
// exactly one of each, and a real change yields one more.
func TestSSEHubDiffOnlyOnChange(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStatus{snap: statusResponse{
		AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}, Dream: dreamStatus{Mode: "on"},
	}}
	h := newTestHub(life, fs, eventsCfgStore(15*time.Millisecond, time.Second, 8))

	sub, ok := h.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	var mu sync.Mutex
	counts := map[string]int{}
	go func() {
		for f := range sub.ch {
			mu.Lock()
			counts[f.name]++
			mu.Unlock()
		}
	}()

	// ~12 ticks of a stable snapshot (only as_of advances in the real collector;
	// here it is frozen, which is the stronger no-churn case).
	time.Sleep(180 * time.Millisecond)
	mu.Lock()
	gotStatus, gotBackends := counts["status"], counts["backends"]
	mu.Unlock()
	if gotStatus != 1 {
		t.Errorf("stable snapshot: got %d status events, want exactly 1", gotStatus)
	}
	if gotBackends != 1 {
		t.Errorf("stable snapshot: got %d backends events, want exactly 1", gotBackends)
	}

	// A real change must re-fire the status event.
	fs.set(statusResponse{AsOf: time.Unix(9, 0), Backends: []backends.BackendStatus{}, Dream: dreamStatus{Mode: "throttled"}})
	waitFor(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return counts["status"] == 2 })
}

// TestEventsReAuthEndsStream is the §5.4 negative probe: a periodic re-auth that
// no longer returns an admin key ends the stream with an error event instead of
// continuing to push admin telemetry to a revoked key.
func TestEventsReAuthEndsStream(t *testing.T) {
	orig := sseWriteWindow
	sseWriteWindow = time.Second
	defer func() { sseWriteWindow = orig }()

	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	// tick 5ms → re-auth fires at 12*5ms = 60ms.
	h := newTestHub(life, fs, eventsCfgStore(5*time.Millisecond, time.Second, 8))
	h.authenticate = func(context.Context, string) (*auth.AuthResult, error) {
		return &auth.AuthResult{IsValid: true, IsAdmin: false}, nil // admin revoked
	}
	eh := &EventsHandler{hub: h}

	srv := httptest.NewServer(http.HandlerFunc(eh.HandleEvents))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body) // blocks until the handler ends the stream
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "event: status") || !strings.Contains(s, "event: backends") {
		t.Errorf("initial full state missing: %q", s)
	}
	if !strings.Contains(s, "event: error") || !strings.Contains(s, "session revoked") {
		t.Errorf("expected error event on revoked admin key, got: %q", s)
	}
}

// waitFor polls cond until true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
