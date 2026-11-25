//go:build integration

// Issue #555, point 1: a request that authenticated before a tenant-delete
// began and writes after it reported success. The delete's first step makes
// ctx_auth refuse the tenant's keys, so only a request already past its
// authentication can still write — and before this fix its write, committed
// after the last closing round, stayed in a scope no tenant owned, where the
// next tenant assigned that scope name (a tenant created with the deleted
// one's slug) read it.
//
// The requests run through the PRODUCTION router (NewRouter: the whole
// middleware chain, the handlers, the store). Each is stopped at the first
// statement it sends after its authentication — before any write transaction
// — the tenant is deleted while it stands there, and then it goes on.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// inflightMark is the context key a held request carries its hold under.
type inflightMark struct{}

// inflightHold stops the first statement of its request that is not the
// authentication's own (ctx_auth) — or, with match, the first that match
// accepts — until release is closed; held is closed once the request stands
// there.
type inflightHold struct {
	match   func(sql string) bool
	once    sync.Once
	held    chan struct{}
	release chan struct{}
}

func newInflightHold() *inflightHold {
	return &inflightHold{held: make(chan struct{}), release: make(chan struct{})}
}

// inflightTracer is the router pool's tracer: it holds a statement whose
// context carries an inflightHold.
type inflightTracer struct{}

func (inflightTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if h, ok := ctx.Value(inflightMark{}).(*inflightHold); ok && !strings.Contains(d.SQL, "ctx_auth") && (h.match == nil || h.match(d.SQL)) {
		h.once.Do(func() {
			close(h.held)
			<-h.release
		})
	}
	return ctx
}

func (inflightTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type inflightFixture struct {
	pool   *pgxpool.Pool
	router http.Handler
	log    *nulSweepLog
}

// newInflightFixture builds the production router over a pool of its own
// that carries inflightTracer, beside a plain pool for the test's own reads.
func newInflightFixture(t *testing.T, env ...string) *inflightFixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	lg := &nulSweepLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(lg))
	t.Cleanup(func() { slog.SetDefault(prev) })
	full := map[string]string{"CTX_SECRETS_KEY": strings.Repeat("ab", 32)}
	for i := 0; i+1 < len(env); i += 2 {
		full[env[i]] = env[i+1]
	}
	applyEnv(t, full)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = inflightTracer{}
	rp, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rp.Close)
	return &inflightFixture{pool: pool, router: nulSweepInstance(t, ctx, rp), log: lg}
}

// tenant creates a tenant with one scope and an owner key of it, the way
// tenant-create bootstraps one (store.MintOwnerKey).
func (f *inflightFixture) tenant(t *testing.T, slug string) (tenantID, scope, key string) {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, f.pool, slug, slug)
	if err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	scope = slug + ":main"
	if _, err := store.AssignTenantScope(ctx, f.pool, tn.ID, scope, nil); err != nil {
		t.Fatalf("scope %s: %v", scope, err)
	}
	if _, key, err = store.MintOwnerKey(ctx, f.pool, slug+" owner", scope, nil, tn.ID, store.AuditStamp{}); err != nil {
		t.Fatalf("key %s: %v", slug, err)
	}
	return tn.ID, scope, key
}

type inflightAnswer struct {
	status      int
	body        string
	isError     bool
	text, code  string
	linesAbove  []string
}

// send runs one request through the router; hold (may be nil) stops it as
// inflightTracer says.
func (f *inflightFixture) send(t *testing.T, key, method, path, body string, mcp bool, hold *inflightHold, reqID string) inflightAnswer {
	t.Helper()
	ctx := context.Background()
	if hold != nil {
		ctx = context.WithValue(ctx, inflightMark{}, hold)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd).WithContext(ctx)
	req.RemoteAddr = "192.0.2.55:4000"
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mcp {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	a := inflightAnswer{status: rec.Code, body: rec.Body.String()}
	if mcp {
		a.isError, a.text, a.code = nulSweepMCPAnswer(a.body)
	}
	time.Sleep(20 * time.Millisecond) // afterwork logs after the answer
	a.linesAbove = f.log.above(reqID)
	return a
}

// rowsIn counts the rows of every table the tenant-delete drains by scope.
func rowsIn(t *testing.T, pool *pgxpool.Pool, scope string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"context_blocks", "context_dream_links", "context_blobs", "context_sources", "context_chat_sessions", "context_secrets"} {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*)::int FROM `+table+` WHERE scope = $1`, scope).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n > 0 {
			out[table] = n
		}
	}
	return out
}

// inflightCase is one write surface: the request, and what the answer of a
// request whose key is gone looks like on it.
type inflightCase struct {
	name, method, path string
	body               func(scope string) string
	mcp                bool
	// refused judges the answer of the write whose key was deleted while it
	// stood: the house's refusal, no 500.
	refused func(a inflightAnswer) bool
}

func restUnauthorized(a inflightAnswer) bool {
	return a.status == http.StatusUnauthorized && strings.Contains(a.body, `"code":"unauthorized"`)
}

func mcpUnauthorized(a inflightAnswer) bool {
	return a.isError && a.code == "unauthorized"
}

func inflightCases() []inflightCase {
	blob := base64.StdEncoding.EncodeToString([]byte("a file written after the delete"))
	js := nulSweepJSON
	return []inflightCase{
		{name: "REST store", method: http.MethodPost, path: "/api/store",
			body:    func(string) string { return js(map[string]any{"category": "learnings", "title": "late store", "content": "written after the delete"}) },
			refused: restUnauthorized},
		{name: "REST store batch", method: http.MethodPost, path: "/api/store/batch",
			body: func(string) string {
				return js(map[string]any{"items": []any{
					map[string]any{"category": "learnings", "title": "late batch a", "content": "written after the delete"},
					map[string]any{"category": "learnings", "title": "late batch b", "content": "written after the delete"}}})
			},
			refused: restUnauthorized},
		{name: "MCP store", method: http.MethodPost, path: "/mcp", mcp: true,
			body: func(string) string {
				return nulSweepMCP("store", map[string]any{"category": "learnings", "title": "late mcp store", "content": "written after the delete"})
			},
			refused: mcpUnauthorized},
		{name: "MCP store_batch", method: http.MethodPost, path: "/mcp", mcp: true,
			body: func(string) string {
				return nulSweepMCP("store_batch", map[string]any{"items": []any{
					map[string]any{"category": "learnings", "title": "late mcp batch", "content": "written after the delete"}}})
			},
			refused: mcpUnauthorized},
		{name: "REST blob store", method: http.MethodPost, path: "/api/blob/store",
			body: func(string) string {
				return js(map[string]any{"file": blob, "filename": "late.txt", "category": "files", "title": "late blob", "mime_type": "text/plain"})
			},
			refused: restUnauthorized},
		{name: "MCP blob_store", method: http.MethodPost, path: "/mcp", mcp: true,
			body: func(string) string {
				return nulSweepMCP("blob_store", map[string]any{"text": "a file written after the delete", "filename": "late.txt", "mime_type": "text/plain", "category": "files", "title": "late mcp blob"})
			},
			refused: mcpUnauthorized},
		{name: "REST ingest", method: http.MethodPost, path: "/api/ingest",
			body: func(string) string {
				return js(map[string]any{"chunks": []any{map[string]any{"category": "learnings", "title": "late ingest", "content": "written after the delete"}}})
			},
			refused: restUnauthorized}, // the ingest books the request before its chunks
		{name: "MCP issue_create", method: http.MethodPost, path: "/mcp", mcp: true,
			body: func(string) string {
				return nulSweepMCP("issue_create", map[string]any{"title": "late issue", "content": "written after the delete"})
			},
			refused: mcpUnauthorized},
		{name: "REST secret", method: http.MethodPut, path: "/api/secrets/late_secret",
			body:    func(string) string { return js(map[string]any{"value": "s3cr3t written after the delete"}) },
			refused: restUnauthorized},
		{name: "REST chat session", method: http.MethodPost, path: "/api/chat/stream",
			body:    func(string) string { return js(map[string]any{"message": "hello after the delete"}) },
			refused: restUnauthorized},
	}
}

// TestTenantDeleteBesideAWriteInFlight: for every write surface that puts a
// row into a scope, a request of the tenant's key that authenticated before
// the delete and writes after the delete answered success leaves no row, and
// the next tenant assigned the scope name reads nothing; the request gets
// the house's refusal (401 unauthorized) with no ERROR line.
func TestTenantDeleteBesideAWriteInFlight(t *testing.T) {
	f := newInflightFixture(t)
	ctx := context.Background()
	for i, c := range inflightCases() {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("inflight%02d", i)
			tenantID, scope, key := f.tenant(t, slug)
			if a := f.send(t, key, http.MethodPost, "/api/store", nulSweepJSON(map[string]any{"category": "learnings", "title": "seed", "content": "the tenant's block"}), false, nil, fmt.Sprintf("0555-%02x-a", i)); a.status != http.StatusOK {
				t.Fatalf("seed: %d %s", a.status, a.body)
			}

			hold := newInflightHold()
			done := make(chan inflightAnswer, 1)
			go func() { done <- f.send(t, key, c.method, c.path, c.body(scope), c.mcp, hold, fmt.Sprintf("0555-%02x-b", i)) }()
			select {
			case <-hold.held:
			case a := <-done:
				t.Fatalf("the request ended before its first statement after authentication: %d %s", a.status, a.body)
			case <-time.After(20 * time.Second):
				t.Fatal("the request did not reach a statement after its authentication")
			}
			if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
				close(hold.release)
				<-done
				t.Fatalf("tenant-delete while the request stood after its authentication: %v", err)
			}
			close(hold.release)
			a := <-done

			left := rowsIn(t, f.pool, scope)
			// The next tenant with the deleted one's slug, assigned its scope
			// name, reads what is left in it.
			next, err := store.CreateTenant(ctx, f.pool, slug, slug)
			if err != nil {
				t.Fatalf("tenant with the deleted one's slug: %v", err)
			}
			if _, err := store.AssignTenantScope(ctx, f.pool, next.ID, scope, nil); err != nil {
				t.Fatalf("assign %s to the next tenant: %v", scope, err)
			}
			adopted := rowsIn(t, f.pool, scope)
			t.Logf("%s: answer %d isError=%v code=%q text=%.160q body=%.200q; left in %s after the delete: %v; read by the next tenant: %v; lines above INFO: %v",
				c.name, a.status, a.isError, a.code, a.text, a.body, scope, left, adopted, a.linesAbove)
			if len(left) != 0 {
				t.Errorf("rows outlived a tenant-delete that answered success: %v (the next tenant assigned %s reads %v)", left, scope, adopted)
			}
			if !c.refused(a) {
				t.Errorf("the write whose key was deleted while it ran: answer %d %.300q, want the house's refusal", a.status, a.body)
			}
			// The refusal's own line is the WARN attribution of /api/store
			// (write_reject_log.go), as for every refusal there; no ERROR.
			for _, l := range a.linesAbove {
				if strings.HasPrefix(l, "ERROR") {
					t.Errorf("an ERROR line for the refused write: %v", a.linesAbove)
					break
				}
			}
		})
	}
}

// TestTenantDeleteBesideAWriteAtItsStatement: the write stands at its own
// statement — past every read of its request, its transaction open — while
// the tenant-delete runs. Before the fix the delete went through to success
// and the write landed afterwards in a scope no tenant owned. Now the write
// holds its key (its transaction's first lock), the delete's key step waits
// for it, and the closing rounds after the key step delete what it wrote: the
// delete answers success and nothing is left, whatever the write answered.
func TestTenantDeleteBesideAWriteAtItsStatement(t *testing.T) {
	f := newInflightFixture(t)
	ctx := context.Background()
	blockInsert := func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blocks") }
	blobInsert := func(sql string) bool { return strings.Contains(sql, "INSERT INTO context_blobs") }
	cases := []struct {
		name, method, path, body string
		mcp                      bool
		at                       func(string) bool
	}{
		{"REST store", http.MethodPost, "/api/store", nulSweepJSON(map[string]any{"category": "learnings", "title": "late store", "content": "written after the delete"}), false, blockInsert},
		{"MCP store", http.MethodPost, "/mcp", nulSweepMCP("store", map[string]any{"category": "learnings", "title": "late mcp store", "content": "written after the delete"}), true, blockInsert},
		{"REST blob store", http.MethodPost, "/api/blob/store", nulSweepJSON(map[string]any{"file": base64.StdEncoding.EncodeToString([]byte("late file")), "filename": "late.txt", "category": "files", "title": "late blob", "mime_type": "text/plain"}), false, blobInsert},
		{"REST ingest", http.MethodPost, "/api/ingest", nulSweepJSON(map[string]any{"chunks": []any{map[string]any{"category": "learnings", "title": "late ingest", "content": "written after the delete"}}}), false, blockInsert},
		{"MCP issue_create", http.MethodPost, "/mcp", nulSweepMCP("issue_create", map[string]any{"title": "late issue", "content": "written after the delete"}), true, blockInsert},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("atstmt%02d", i)
			tenantID, scope, key := f.tenant(t, slug)
			if a := f.send(t, key, http.MethodPost, "/api/store", nulSweepJSON(map[string]any{"category": "learnings", "title": "seed", "content": "the tenant's block"}), false, nil, fmt.Sprintf("0555-1%02x-a", i)); a.status != http.StatusOK {
				t.Fatalf("seed: %d %s", a.status, a.body)
			}
			hold := newInflightHold()
			hold.match = c.at
			done := make(chan inflightAnswer, 1)
			go func() { done <- f.send(t, key, c.method, c.path, c.body, c.mcp, hold, fmt.Sprintf("0555-1%02x-b", i)) }()
			select {
			case <-hold.held:
			case a := <-done:
				t.Fatalf("the request ended before its write statement: %d %s", a.status, a.body)
			case <-time.After(20 * time.Second):
				t.Fatal("the request did not reach its write statement")
			}
			pruned := make(chan error, 1)
			go func() { pruned <- store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}) }()
			var perr error
			finished := false
			select {
			case perr = <-pruned: // the delete did not wait for the write
				finished = true
			case <-time.After(2 * time.Second): // the delete waits for the write
			}
			close(hold.release)
			a := <-done
			if !finished {
				perr = <-pruned
			}
			if perr != nil {
				t.Fatalf("tenant-delete beside the write: %v", perr)
			}
			left := rowsIn(t, f.pool, scope)
			next, err := store.CreateTenant(ctx, f.pool, slug, slug)
			if err != nil {
				t.Fatalf("tenant with the deleted one's slug: %v", err)
			}
			if _, err := store.AssignTenantScope(ctx, f.pool, next.ID, scope, nil); err != nil {
				t.Fatalf("assign %s to the next tenant: %v", scope, err)
			}
			adopted := rowsIn(t, f.pool, scope)
			t.Logf("%s: the delete finished while the write stood: %v; answer %d isError=%v code=%q; left after the delete: %v; read by the next tenant: %v; lines above INFO: %v",
				c.name, finished, a.status, a.isError, a.code, left, adopted, a.linesAbove)
			if len(left) != 0 {
				t.Errorf("rows outlived a tenant-delete that answered success: %v (the next tenant assigned %s reads %v)", left, scope, adopted)
			}
			for _, l := range a.linesAbove {
				if strings.HasPrefix(l, "ERROR") {
					t.Errorf("an ERROR line for the write: %v", a.linesAbove)
					break
				}
			}
		})
	}
}

// TestTenantDeleteBesideABatchBehindItsLastRound: a block batch of a request
// that authenticated before the delete arrives while the delete's last
// closing round holds the scope locks (second review of PR #554). Before the
// fix it waited for the round and then failed at its booking with the foreign
// key of the deleted key (500 and an ERROR line naming the constraint); now it
// is refused at once, 401 unauthorized, no ERROR line, nothing written.
func TestTenantDeleteBesideABatchBehindItsLastRound(t *testing.T) {
	f := newInflightFixture(t)
	ctx := context.Background()
	for i, c := range []struct {
		name, path, body string
		mcp              bool
	}{
		{"REST store batch", "/api/store/batch", nulSweepJSON(map[string]any{"items": []any{
			map[string]any{"category": "learnings", "title": "late batch a", "content": "behind the last round"},
			map[string]any{"category": "learnings", "title": "late batch b", "content": "behind the last round"}}}), false},
		{"MCP store_batch", "/mcp", nulSweepMCP("store_batch", map[string]any{"items": []any{
			map[string]any{"category": "learnings", "title": "late mcp batch", "content": "behind the last round"}}}), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("lastround%02d", i)
			tenantID, scope, key := f.tenant(t, slug)
			if a := f.send(t, key, http.MethodPost, "/api/store", nulSweepJSON(map[string]any{"category": "learnings", "title": "seed", "content": "the tenant's block"}), false, nil, fmt.Sprintf("0555-2%02x-a", i)); a.status != http.StatusOK {
				t.Fatalf("seed: %d %s", a.status, a.body)
			}
			// The request authenticates before the delete begins and stands
			// at its first statement after that.
			hold := newInflightHold()
			done := make(chan inflightAnswer, 1)
			go func() { done <- f.send(t, key, http.MethodPost, c.path, c.body, c.mcp, hold, fmt.Sprintf("0555-2%02x-b", i)) }()
			select {
			case <-hold.held:
			case a := <-done:
				t.Fatalf("the request ended before its first statement after authentication: %d %s", a.status, a.body)
			case <-time.After(20 * time.Second):
				t.Fatal("the request did not reach a statement after its authentication")
			}
			// The delete stands at its last round's tenant delete, holding
			// every scope lock of the tenant.
			ph := &prunePause{match: func(sql string) bool { return strings.Contains(sql, "DELETE FROM context_tenants") },
				held: make(chan struct{}), release: make(chan struct{})}
			pp := prunePausePool(t, f, ph)
			pruned := make(chan error, 1)
			go func() { pruned <- store.PruneTenant(ctx, pp, tenantID, store.AuditStamp{}) }()
			select {
			case <-ph.held:
			case err := <-pruned:
				t.Fatalf("the delete ended (%v) before its last round", err)
			case <-time.After(20 * time.Second):
				t.Fatal("the delete did not reach its last round")
			}
			close(hold.release)
			time.Sleep(300 * time.Millisecond) // a batch that takes the scope lock waits for the round now
			close(ph.release)
			if err := <-pruned; err != nil {
				t.Fatalf("tenant-delete: %v", err)
			}
			a := <-done
			left := rowsIn(t, f.pool, scope)
			t.Logf("%s: answer %d isError=%v code=%q body=%.200q; left: %v; lines above INFO: %v", c.name, a.status, a.isError, a.code, a.body, left, a.linesAbove)
			if len(left) != 0 {
				t.Errorf("rows outlived a tenant-delete that answered success: %v", left)
			}
			if c.mcp && !mcpUnauthorized(a) || !c.mcp && !restUnauthorized(a) {
				t.Errorf("the batch behind the last round: answer %d %.300q, want 401 unauthorized", a.status, a.body)
			}
			for _, l := range a.linesAbove {
				if strings.HasPrefix(l, "ERROR") {
					t.Errorf("an ERROR line for the refused batch: %v", a.linesAbove)
					break
				}
			}
		})
	}
}

// prunePause stops the delete's first statement match accepts until release.
type prunePause struct {
	match   func(sql string) bool
	once    sync.Once
	held    chan struct{}
	release chan struct{}
}

func (p *prunePause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if p.match(d.SQL) {
		p.once.Do(func() {
			close(p.held)
			<-p.release
		})
	}
	return ctx
}

func (p *prunePause) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func prunePausePool(t *testing.T, f *inflightFixture, p *prunePause) *pgxpool.Pool {
	t.Helper()
	cfg := f.pool.Config().Copy()
	cfg.ConnConfig.Tracer = p
	cfg.MaxConns = 2
	pp, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pp.Close)
	return pp
}

// TestStoreThroughputProbe555 measures POST /api/store through the production
// router: eight parallel writers of one key into its home scope, writes per
// second over 5 s runs. Opt-in (CTX_P555_COST=1); the same file runs on the
// base and on this branch to compare the write's cost with and without the
// key share (issue #555, point 1).
func TestStoreThroughputProbe555(t *testing.T) {
	if os.Getenv("CTX_P555_COST") == "" {
		t.Skip("measurement: set CTX_P555_COST=1")
	}
	f := newInflightFixture(t, "CTX_RATE_LIMIT_WRITE", "0") // no write budget: the store's own cost
	_, _, key := f.tenant(t, "probe555")
	const writers = 8
	var rates []string
	for round := range 4 {
		var n atomic.Int64
		stop := time.Now().Add(5 * time.Second)
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; time.Now().Before(stop); i++ {
					body := nulSweepJSON(map[string]any{"category": "learnings", "title": fmt.Sprintf("probe %d %d %d", round, w, i), "content": "a single store's block"})
					req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(body))
					req.RemoteAddr = "192.0.2.56:4000"
					req.Header.Set("Authorization", "Bearer "+key)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					f.router.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Errorf("store: %d %s", rec.Code, rec.Body.String())
						return
					}
					n.Add(1)
				}
			}()
		}
		wg.Wait()
		if round > 0 { // the first run warms the pool and the caches
			rates = append(rates, fmt.Sprintf("%.0f", float64(n.Load())/5))
		}
	}
	t.Logf("POST /api/store, 8 writers of one key, writes per second (5 s runs): %s", strings.Join(rates, ", "))
}
