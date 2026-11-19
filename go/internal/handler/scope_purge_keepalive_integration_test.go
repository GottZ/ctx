//go:build integration

// Issue #277: a confirmed `manage purge` runs on after the client is gone
// (the run is detached from the request), but its answer was written only
// when the run ended. http.Server's WriteTimeout (main.go: 120 s) is an
// absolute deadline fixed when the request was read, so a run longer than
// that was logged and never delivered — a purge at the 1M-block scale takes
// minutes. The tests drive the REAL manage handler behind a real http.Server
// whose WriteTimeout is shrunk to 300 ms, and make the run wait 900 ms for a
// row lock another transaction holds on the scope's blocks.
//
//	go test -tags=integration ./internal/handler/ -run TestScopePurgeKeepalive -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const pkScope = "pk-a:import"

// pkSetup seeds a tenant-owned scope with n blocks through the store's write
// path and returns the pool and a tenant-admin of that tenant.
func pkSetup(t *testing.T, n int) (*pgxpool.Pool, *auth.AuthResult) {
	t.Helper()
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	tn, err := store.CreateTenant(ctx, pool, "pk-a", "pk-a")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, pkScope, nil); err != nil {
		t.Fatalf("scope: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", "pk-block-"+string(rune('a'+i)), "x",
			[]string{"import"}, nil, pkScope, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var key string
	if err := pool.QueryRow(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('pk') RETURNING id)
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id, tenant_role)
		 SELECT 'pk-hash', 'pk-tenant-admin', $1, $2::uuid, p.id, 'admin' FROM p RETURNING id::text`, pkScope, tn.ID).Scan(&key); err != nil {
		t.Fatalf("key: %v", err)
	}
	return pool, &auth.AuthResult{IsValid: true, ApiKeyID: key, HomeScope: pkScope, TenantID: tn.ID, TenantRole: auth.RoleAdmin}
}

// pkServer serves the manage handler behind an http.Server with the given
// WriteTimeout, as ar.
func pkServer(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleManage(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// pkHoldScope locks every block of the scope FOR UPDATE in a transaction of
// its own and releases it after d. It returns once the lock is held.
func pkHoldScope(t *testing.T, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM context_blocks WHERE scope = $1 FOR UPDATE`, pkScope); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	go func() {
		time.Sleep(d)
		_ = tx.Commit(ctx)
	}()
}

// pkPurge posts one purge and returns status, the time until the first body
// byte and the body. A transport error — the connection cut at the
// WriteTimeout — fails the test.
func pkPurge(t *testing.T, url string, data map[string]any) (int, time.Duration, []byte) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"action": "purge", "data": data})
	start := time.Now()
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST purge: %v — the answer did not arrive (cut at the server WriteTimeout?)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first body byte: %v — the answer did not arrive (cut at the server WriteTimeout?)", err)
	}
	ttfb := time.Since(start)
	body.Write(first)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v — the answer was cut", err)
	}
	return resp.StatusCode, ttfb, body.Bytes()
}

func pkDeleted(t *testing.T, body []byte) float64 {
	t.Helper()
	var resp struct {
		Success bool `json:"success"`
		DryRun  bool `json:"dry_run"`
		Deleted struct {
			Blocks float64 `json:"blocks"`
		} `json:"deleted"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if !resp.Success || resp.DryRun {
		t.Fatalf("success=%v dry_run=%v: %s", resp.Success, resp.DryRun, body)
	}
	return resp.Deleted.Blocks
}

// TestScopePurgeKeepaliveConfirmedRunOutlivesWriteTimeout: a confirmed run
// that takes three WriteTimeouts answers its counts, as the same plain JSON
// body it always had — the write deadline rolls while the run works, no byte
// goes out before the answer.
//
// RED before the fix: the client reads EOF (the answer is written after the
// absolute deadline and the connection is closed).
func TestScopePurgeKeepaliveConfirmedRunOutlivesWriteTimeout(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 500*time.Millisecond)
	pool, ar := pkSetup(t, 3)
	srv := pkServer(t, pool, ar, 300*time.Millisecond)

	pkHoldScope(t, pool, 900*time.Millisecond)
	start := time.Now()
	status, _, body := pkPurge(t, srv.URL, map[string]any{"scope": pkScope, "confirm_blocks": 3})
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Fatalf("the run took %v — it did not wait for the held lock, the test proves nothing", took)
	}
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}
	if body[0] != '{' {
		t.Errorf("body starts with %q — a request without keepalive must not get keepalive bytes", body[:min(8, len(body))])
	}
	if n := pkDeleted(t, body); n != 3 {
		t.Errorf("deleted.blocks = %v, want 3", n)
	}
}

// TestScopePurgeKeepaliveFromTheStart: with keepalive:true the response
// commits 200 on the first tick, long before the run ends, and the JSON
// follows the whitespace — the form for a reverse proxy whose read timeout
// only bytes reset.
//
// RED before the fix: the field is refused as unknown (400).
func TestScopePurgeKeepaliveFromTheStart(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 500*time.Millisecond)
	pool, ar := pkSetup(t, 3)
	srv := pkServer(t, pool, ar, 300*time.Millisecond)

	pkHoldScope(t, pool, 900*time.Millisecond)
	status, ttfb, body := pkPurge(t, srv.URL, map[string]any{"scope": pkScope, "confirm_blocks": 3, "keepalive": true})
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}
	if ttfb >= 600*time.Millisecond {
		t.Errorf("first byte after %v — the keepalive did not commit before the run ended", ttfb)
	}
	if body[0] != ' ' {
		t.Errorf("body starts with %q, want the keepalive space", body[:min(8, len(body))])
	}
	if n := pkDeleted(t, body); n != 3 {
		t.Errorf("deleted.blocks = %v, want 3", n)
	}
}

// TestScopePurgeKeepaliveFastAnswersUnchanged: a purge that answers before the
// first tick keeps its status and its body byte for byte, with and without
// keepalive — a refusal included.
func TestScopePurgeKeepaliveFastAnswersUnchanged(t *testing.T) {
	shrinkHeartbeat(t, 5*time.Second, 10*time.Second)
	pool, ar := pkSetup(t, 2)
	srv := pkServer(t, pool, ar, 300*time.Millisecond)

	for _, ka := range []bool{false, true} {
		data := map[string]any{"scope": pkScope, "confirm_blocks": 7}
		if ka {
			data["keepalive"] = true
		}
		status, _, body := pkPurge(t, srv.URL, data)
		if status != http.StatusConflict || body[0] != '{' || !strings.Contains(string(body), `"confirm_blocks":2`) {
			t.Errorf("keepalive=%v: a confirm that does not match: status %d body %s, want the plain 409 with the fresh count", ka, status, body)
		}
	}
}
