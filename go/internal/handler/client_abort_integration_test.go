//go:build integration

// Issue #417 at the three representative routes, through the production
// chain (RequestID, Logger, Auth) on a real httptest server whose client
// gives up while the route waits on the database or on the embed backend:
//
//   - POST /api/manage stats (read): the stats query waits on a table lock;
//   - POST /api/store (write): the upsert waits inside its transaction (the
//     write rolls back), the parent link waits after the upsert committed
//     (the block exists), and the COMMIT itself waits (the outcome is unknown
//     to the server — the row lands once the commit goes through);
//   - POST /api/query (the long path): the embed backend holds the call.
//
// Each run sees no ERROR line and a 499 in the request log, and the write
// runs name the write's outcome. The contrast runs pin what stays an error:
// a statement timeout on a live request (the server's own deadline).
//
//	go test -tags=integration ./internal/handler/ -run 'TestClientAbort' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// abortKey creates a member key in the private scope and returns it plain.
func abortKey(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	_, plain, err := store.CreateApiKey(context.Background(), pool, "i417-abort", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return plain
}

// abortRoute serves h behind RequestID, Logger and Auth (the production
// order) and reports through done when the request log line is written.
func abortRoute(t *testing.T, pool *pgxpool.Pool, h http.HandlerFunc) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	return abortServer(t, Auth(pool)(h), nil)
}

// waitLockWaiters polls pg_stat_activity until n backends of this database
// wait on a lock (heavyweight or advisory) — the moment the route is parked.
func waitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend started waiting on the lock within 10 s")
}

// abortAfter sends the request and cancels the client once parked returns.
func abortAfter(t *testing.T, url, plain, body string, parked func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Context-Key", plain)
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	parked()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
}

// assertClientGone checks the verdict every abort run shares: one request log
// line with 499 and no record at ERROR or above.
func assertClientGone(t *testing.T, logs *rejectLogCapture) {
	t.Helper()
	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up produced ERROR lines: %+v", errs)
	}
}

// lockedTx opens a transaction on its own connection that takes lockSQL and
// holds it until the returned release runs (also at cleanup).
func lockedTx(t *testing.T, pool *pgxpool.Pool, lockSQL string, args ...any) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	if _, err := tx.Exec(ctx, lockSQL, args...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("take lock %q: %v", lockSQL, err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

func TestClientAbortManageStats(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	plain := abortKey(t, pool)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	t.Run("client gives up while stats waits", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleManage)
		release := lockedTx(t, pool, `LOCK TABLE context_blocks IN ACCESS EXCLUSIVE MODE`)
		abortAfter(t, srv.URL+"/api/manage", plain, `{"action":"stats"}`, func() { waitLockWaiters(t, pool, 1) })
		waitDone(t, done)
		release()
		assertClientGone(t, logs)
		if recs := logs.withMsg("manage: stats error"); len(recs) != 1 {
			t.Errorf("want the stats line once (below ERROR), got %+v", recs)
		}
	})

	t.Run("pre-cancelled request context at the handler", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		ctx, cancel := context.WithCancel(mcpAuthCtx(t, context.Background(), pool, plain))
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(`{"action":"stats"}`)).WithContext(ctx)
		RequestID(Logger(http.HandlerFunc(h.HandleManage))).ServeHTTP(httptest.NewRecorder(), req)
		assertClientGone(t, logs)
	})

	t.Run("contrast: statement timeout on a live request stays an error", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		cfg := pool.Config().Copy()
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = "200"
		short, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(short.Close)
		srv, done := abortRoute(t, pool, NewManageHandler(short, nil, nil, nil, nil, nil, nil, nil).HandleManage)
		release := lockedTx(t, pool, `LOCK TABLE context_blocks IN ACCESS EXCLUSIVE MODE`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/manage", strings.NewReader(`{"action":"stats"}`))
		req.Header.Set("X-Context-Key", plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		waitDone(t, done)
		release()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("client got %d, want 500", resp.StatusCode)
		}
		if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusInternalServerError {
			t.Errorf("request log status = %d, want 500", got)
		}
		if recs := logs.withMsg("manage: stats error"); len(recs) != 1 || recs[0].level != slog.LevelError {
			t.Errorf("the server's statement timeout must stay an ERROR line: %+v", recs)
		}
	})
}

func TestClientAbortStoreWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	plain := abortKey(t, pool)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	cfg := staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	h := NewStoreHandler(pool, cfg, reg)

	blockCount := func(t *testing.T, title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", title, err)
		}
		return n
	}
	// outcomeOf returns the write outcome the route's line states.
	outcomeOf := func(t *testing.T, logs *rejectLogCapture, msg string) string {
		t.Helper()
		recs := logs.withMsg(msg)
		if len(recs) != 1 {
			t.Fatalf("want one %q line, got %+v (all: %+v)", msg, recs, logs.all())
		}
		s, _ := recs[0].attrs["write_outcome"].(string)
		return s
	}
	storeBody := func(title, parentID string) string {
		b := map[string]any{"category": "i417", "title": title, "content": "client abort probe " + title}
		if parentID != "" {
			b["parent_id"] = parentID
		}
		raw, _ := json.Marshal(b)
		return string(raw)
	}

	t.Run("abort inside the write transaction rolls back", func(t *testing.T) {
		const title = "i417-abort-in-tx"
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleStore)
		release := lockedTx(t, pool, `LOCK TABLE context_blocks IN SHARE MODE`)
		abortAfter(t, srv.URL+"/api/store", plain, storeBody(title, ""), func() { waitLockWaiters(t, pool, 1) })
		waitDone(t, done)
		release()
		assertClientGone(t, logs)
		if got := outcomeOf(t, logs, "store: upsert error"); got != "rolled_back" {
			t.Errorf("write_outcome = %q, want rolled_back", got)
		}
		if n := blockCount(t, title); n != 0 {
			t.Errorf("the rolled-back write left %d rows", n)
		}
	})

	// Since issue #211 a store that names a parent writes the block and the
	// link in ONE transaction: a client that goes away while the write waits
	// for its parent leaves nothing — before, the upsert had committed and the
	// block stayed without its parent (write_outcome committed).
	t.Run("abort while the write waits for its parent rolls the block back", func(t *testing.T) {
		const title = "i417-abort-after-commit"
		parent, err := store.UpsertBlock(ctx, pool, "i417", "i417-parent", "parent", nil, nil, "private", true,
			store.SensitivityWrite{Value: backends.SensPublic}, "")
		if err != nil {
			t.Fatalf("seed parent: %v", err)
		}
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleStore)
		release := lockedTx(t, pool, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, parent.ID)
		abortAfter(t, srv.URL+"/api/store", plain, storeBody(title, parent.ID), func() { waitLockWaiters(t, pool, 1) })
		waitDone(t, done)
		release()
		assertClientGone(t, logs)
		if got := outcomeOf(t, logs, "store: upsert error"); got != "rolled_back" {
			t.Errorf("write_outcome = %q, want rolled_back", got)
		}
		if n := blockCount(t, title); n != 0 {
			t.Errorf("the rolled-back write left %d rows, want 0", n)
		}
	})

	t.Run("abort during the commit leaves the outcome unknown", func(t *testing.T) {
		const title = "i417-abort-in-commit"
		const lockKey = 417417
		for _, ddl := range []string{
			`CREATE FUNCTION i417_hold_commit() RETURNS trigger LANGUAGE plpgsql AS
			 $$ BEGIN PERFORM pg_advisory_xact_lock(417417); RETURN NULL; END $$`,
			`CREATE CONSTRAINT TRIGGER i417_hold_commit AFTER INSERT ON context_blocks
			 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
			 WHEN (NEW.title = 'i417-abort-in-commit') EXECUTE FUNCTION i417_hold_commit()`,
		} {
			if _, err := pool.Exec(ctx, ddl); err != nil {
				t.Fatalf("fixture DDL: %v", err)
			}
		}
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleStore)
		release := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, lockKey)
		abortAfter(t, srv.URL+"/api/store", plain, storeBody(title, ""), func() { waitLockWaiters(t, pool, 1) })
		waitDone(t, done)
		assertClientGone(t, logs)
		if got := outcomeOf(t, logs, "store: upsert error"); got != "unknown" {
			t.Errorf("write_outcome = %q, want unknown (the cancel hit the COMMIT on the wire)", got)
		}
		// Why "unknown" is the honest word: the backend still holds the
		// COMMIT; once it may go on, the row lands although the route failed.
		release()
		deadline := time.Now().Add(10 * time.Second)
		for blockCount(t, title) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		t.Logf("rows after the held COMMIT went on: %d", blockCount(t, title))
	})
}

func TestClientAbortQueryEmbed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	plain := abortKey(t, pool)
	embedParked, embedRelease := make(chan struct{}, 1), make(chan struct{})
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case embedParked <- struct{}{}:
		default:
		}
		// The backend holds the call until the caller drops it (or the
		// test ends: Close waits for this handler).
		select {
		case <-r.Context().Done():
		case <-embedRelease:
		}
	}))
	t.Cleanup(embedSrv.Close)
	t.Cleanup(func() { close(embedRelease) }) // runs before Close
	h := NewQueryHandler(pool, config.NewStore(w023Config(false)), embedPool(embedSrv.URL), nil,
		blocktype.NewRegistry(), snapshotTestAdmitter(t))

	logs := installRejectLogCapture(t)
	// The production chain puts the instance identity first (buildRouter);
	// without it the embed call's hop stamp writes an ERROR of its own.
	id := fedhop.NewIdentity("00000000-0000-4000-8000-000000000417", bytes.Repeat([]byte{0x41}, 32))
	srv, done := abortServer(t, fedhop.IdentityMiddleware(id)(Auth(pool)(http.HandlerFunc(h.HandleQuery))), nil)
	abortAfter(t, srv.URL+"/api/query", plain, `{"query":"i417 client abort","synthesize":false}`, func() {
		select {
		case <-embedParked:
		case <-time.After(10 * time.Second):
			t.Fatal("the query never reached the embed backend")
		}
	})
	waitDone(t, done)
	assertClientGone(t, logs)
}
