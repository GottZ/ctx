//go:build integration

// W9 SSE re-auth gates driven over the REAL GET /api/project/events entrypoint
// (httptest.Server + the production MountProjectEvents chain + a live projectHub
// against testcontainers PG18). Proves the two §4.5 negative probes that a pure
// identity compare (T37d) would fail:
//
//   - key-revoke ⇒ the stream ENDS within a bounded number of re-auth ticks;
//   - grant-revoke mid-stream (tenant_id/role UNCHANGED) ⇒ frames of the revoked
//     scope STOP within the same bound, while the stream + other scopes live on.
//
// Run: `go test -tags=integration ./internal/handler/ -run TestW9SSE -count=1 -v`.
package handler

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sseEvent is one parsed SSE frame (comments dropped).
type sseEvent struct{ name, data string }

// readSSE streams parsed events from an SSE response onto a channel until the
// body closes or ctx is cancelled.
func readSSE(ctx context.Context, resp *http.Response) <-chan sseEvent {
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" || ev.data != "" {
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// awaitProjectFrame drains events until a "project" frame whose data contains
// want appears, or fails after d.
func awaitProjectFrame(t *testing.T, ch <-chan sseEvent, want string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before a project frame containing %q", want)
			}
			if ev.name == "project" && strings.Contains(ev.data, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no project frame containing %q within %s", want, d)
		}
	}
}

// assertNoProjectFrame asserts NO "project" frame containing want arrives within
// d (an active-absence window). A closed stream is a hard failure here (the
// stream must live on — grant-revoke drops the scope, not the connection).
func assertNoProjectFrame(t *testing.T, ch <-chan sseEvent, want string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed (expected it to live on) while awaiting-absence of %q", want)
			}
			if ev.name == "project" && strings.Contains(ev.data, want) {
				t.Fatalf("LEAK: received a project frame containing %q after revoke", want)
			}
		case <-deadline:
			return // silence held
		}
	}
}

// w9SSESeedProject seeds tenant+scope+project, returns (projectID, scope).
func w9SSESeedProject(t *testing.T, pool *pgxpool.Pool, slug string) (string, string) {
	t.Helper()
	tn := be5SeedTenant(t, pool, slug)
	scope := slug + ":repo"
	be5SeedScope(t, pool, scope, tn)
	var pid string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_projects (tenant_id, scope, identity) VALUES ($1::uuid, $2, $3) RETURNING id::text`,
		tn, scope, "github:acme/"+slug).Scan(&pid); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return pid, scope
}

// w9SSEServer stands up the real MountProjectEvents chain with an injected
// initial AuthResult and a fake in-stream authenticate seam.
func w9SSEServer(t *testing.T, hub *events.ProjectHub, pool *pgxpool.Pool, cfg *config.Store, initial *auth.AuthResult, fake func(context.Context, string, bool) (*auth.AuthResult, error)) *httptest.Server {
	t.Helper()
	h := NewProjectEventsHandler(hub, pool, cfg)
	h.authenticate = fake
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, initial)))
			})
		})
		MountProjectEvents(r, h)
	})
	return httptest.NewServer(r)
}

func w9SSEConfig() *config.Store {
	return config.NewStore(&config.Config{Project: config.ProjectConfig{Events: config.ProjectEventsConfig{
		MaxConnections:    8,
		FlushInterval:     30 * time.Millisecond,
		PingInterval:      time.Second,
		CoalesceThreshold: 20,
	}}})
}

// TestW9SSEKeyRevoke: the stream ends within a bounded number of re-auth ticks
// once the key is revoked. RED without the in-stream re-auth (stream never ends).
func TestW9SSEKeyRevoke(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	old := projectSSEReauthOverride
	projectSSEReauthOverride = 80 * time.Millisecond
	defer func() { projectSSEReauthOverride = old }()

	_, scopeA := w9SSESeedProject(t, pool, "w9rev")
	hub := events.NewProjectHub(ctx, pool, config.NewStore(&config.Config{}))
	cfg := w9SSEConfig()

	initial := &auth.AuthResult{IsValid: true, TenantID: "tn-1", ReadScopes: []string{scopeA}}
	var calls int
	var mu sync.Mutex
	fake := func(context.Context, string, bool) (*auth.AuthResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls >= 2 {
			return &auth.AuthResult{IsValid: false}, nil // revoked
		}
		return initial, nil
	}
	srv := w9SSEServer(t, hub, pool, cfg, initial, fake)
	defer srv.Close()
	defer cancel() // LIFO: runs before srv.Close() so the streaming handler unblocks

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/project/events", nil)
	req.Header.Set("X-Context-Key", "deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	ch := readSSE(ctx, resp)

	// Within a few re-auth ticks the stream must deliver a "session revoked" error
	// and then close.
	deadline := time.After(3 * time.Second)
	sawRevoke := false
	for !sawRevoke {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed without a 'session revoked' error event")
			}
			if ev.name == "error" && strings.Contains(ev.data, "revoked") {
				sawRevoke = true
			}
		case <-deadline:
			t.Fatal("stream did not end within 3s of key revocation (re-auth not firing)")
		}
	}
	// And the channel closes (handler returned).
	select {
	case _, ok := <-ch:
		if ok {
			// drain any trailing then confirm closure
			for range ch {
			}
		}
	case <-time.After(1 * time.Second):
		t.Fatal("stream did not close after 'session revoked'")
	}
}

// TestW9SSEGrantRevoke: a cross-tenant scope grant revoked mid-stream (tenant_id
// UNCHANGED) stops that scope's frames within the re-auth bound, while the stream
// and the home scope keep flowing. RED against a pure identity compare (identity
// unchanged ⇒ the foreign scope would keep leaking).
func TestW9SSEGrantRevoke(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	old := projectSSEReauthOverride
	projectSSEReauthOverride = 80 * time.Millisecond
	defer func() { projectSSEReauthOverride = old }()

	projA, scopeA := w9SSESeedProject(t, pool, "w9grA") // home scope
	projB, scopeB := w9SSESeedProject(t, pool, "w9grB") // granted (cross-tenant) scope

	hub := events.NewProjectHub(ctx, pool, config.NewStore(&config.Config{
		Project: config.ProjectConfig{Events: config.ProjectEventsConfig{FlushInterval: 30 * time.Millisecond, CoalesceThreshold: 20}},
	}))
	cfg := w9SSEConfig()

	// tenant_id + role stay fixed across the revoke; only ReadScopes shrinks.
	initial := &auth.AuthResult{IsValid: true, TenantID: "tn-A", ReadScopes: []string{scopeA, scopeB}}
	var revoked bool
	var mu sync.Mutex
	fake := func(context.Context, string, bool) (*auth.AuthResult, error) {
		mu.Lock()
		defer mu.Unlock()
		rs := []string{scopeA, scopeB}
		if revoked {
			rs = []string{scopeA} // grant to scopeB revoked; SAME tenant, SAME role
		}
		return &auth.AuthResult{IsValid: true, TenantID: "tn-A", ReadScopes: rs}, nil
	}
	srv := w9SSEServer(t, hub, pool, cfg, initial, fake)
	defer srv.Close()
	defer cancel() // LIFO: runs before srv.Close() so the streaming handler unblocks

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/project/events", nil)
	req.Header.Set("X-Context-Key", "deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	ch := readSSE(ctx, resp)

	// Give the subscribe a moment, then a write in the granted scope B is seen.
	time.Sleep(150 * time.Millisecond)
	hub.Dispatch(`{"id":"blk-b1","op":"INSERT","scope":"` + scopeB + `","type":"issue"}`)
	awaitProjectFrame(t, ch, projB, 3*time.Second)

	// Revoke the grant; wait for at least one re-auth tick to nachführen the tags.
	mu.Lock()
	revoked = true
	mu.Unlock()
	time.Sleep(400 * time.Millisecond) // > several 80ms re-auth ticks

	// A further write in scope B must NOT arrive; a write in home scope A MUST
	// (stream alive → this is tag-filtering, not stream death).
	hub.Dispatch(`{"id":"blk-b2","op":"INSERT","scope":"` + scopeB + `","type":"issue"}`)
	hub.Dispatch(`{"id":"blk-a1","op":"INSERT","scope":"` + scopeA + `","type":"issue"}`)
	awaitProjectFrame(t, ch, projA, 3*time.Second) // home scope still flows
	assertNoProjectFrame(t, ch, projB, 700*time.Millisecond)
}

