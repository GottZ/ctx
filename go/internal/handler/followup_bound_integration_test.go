//go:build integration

package handler

// Issue #453 point 6 — work after a commit hangs on a bound and on the
// shutdown, not on the client (the rule of #462), at the routes whose
// post-commit step ran on the request context or on an unbounded detached
// context:
//
//   - POST /api/store/batch (runStoreBatch, also the MCP store_batch tool)
//     and a confirmed batch (executeConfirmedStoreBatch): the afterwork of
//     every written block ran on context.WithoutCancel — a client that left
//     did not stop it, and nothing ended it. One followUpContext bounds all
//     of it.
//   - POST /api/digest: the type classification after the topic map's and
//     the root map's upsert ran on the request context; a client that left
//     after the upsert left the map with the type of its insert. The digest
//     and rootmap packages detach it with a bound of their own.
//   - the snapshot reloads after admin writes (settings, secrets, backends,
//     block types, the eject/gaming toggle): a client that left cut the
//     reload, and the instance went on with the snapshot from before the
//     write until the NOTIFY path caught up.
//
// Each route is parked on its post-commit step — a lock (lockedTx,
// holdAfterCommit) or a fake hook — and then its client leaves, or the server
// shuts down with ctxd's HTTP drain while the client still waits.
//
//	go test -tags=integration ./internal/handler/ -run 'TestFollowupBound' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// fbDrain is ctxd's HTTP drain (cmd/ctxd shutdownHTTPTimeout): what
// http.Server.Shutdown may take before the teardown goes on and the pool
// closes.
const fbDrain = 15 * time.Second

// Advisory lock keys of the holds below, one per route.
const (
	fbKeyBatch     = 4536001
	fbKeyTopicMap  = 4536002
	fbKeyRootMap   = 4536003
	fbKeySettings  = 4536004
	fbKeySecrets   = 4536005
	fbKeyBackends  = 4536006
	fbKeyBlockType = 4536007
)

// fbServer serves h like abortServer and hands out the server-side request
// context of each request, so a test can wait until the server saw its
// client go.
func fbServer(t *testing.T, h http.Handler) (*httptest.Server, <-chan struct{}, <-chan context.Context) {
	t.Helper()
	reqs := make(chan context.Context, 4)
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs <- r.Context()
		h.ServeHTTP(w, r)
	}), nil)
	return srv, done, reqs
}

// fbAs puts ar into each request's auth context in front of h, as the Auth
// middleware does for an authenticated key.
func fbAs(ar *auth.AuthResult, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	})
}

// fbRequest is one request in flight; cancel gives up on it as its client.
type fbRequest struct {
	cancel context.CancelFunc
	errc   <-chan error
}

// fbSend starts the request as a client that waits for the answer until it
// cancels; plain, when set, is the key the Auth middleware reads.
func fbSend(t *testing.T, method, url, plain, body string) fbRequest {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if plain != "" {
		req.Header.Set("X-Context-Key", plain)
	}
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		errc <- err
	}()
	return fbRequest{cancel: cancel, errc: errc}
}

// leave gives up on the request as its client, waits until the server's
// request context ended (the server saw the client go) and until a statement
// that cancellation reached left the database.
func (q fbRequest) leave(t *testing.T, pool *pgxpool.Pool, reqs <-chan context.Context) {
	t.Helper()
	var rctx context.Context
	select {
	case rctx = <-reqs:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the route")
	}
	q.cancel()
	if err := <-q.errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
	select {
	case <-rctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the server never saw its client go")
	}
	settleCancellation(t, pool)
}

// fbShutdown shuts srv down with ctxd's drain budget and returns how long
// Shutdown took and what it returned.
func fbShutdown(srv *httptest.Server) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fbDrain)
	defer cancel()
	begin := time.Now()
	err := srv.Config.Shutdown(ctx)
	return time.Since(begin), err
}

// assertDrained: Shutdown returned nil within bound (plus two seconds of
// slack), inside ctxd's drain.
func assertDrained(t *testing.T, elapsed time.Duration, err error, bound time.Duration) {
	t.Helper()
	if err != nil || elapsed > bound+2*time.Second {
		t.Errorf("Shutdown took %v and returned %v; want nil within the follow-up bound %v, inside ctxd's %v drain",
			elapsed.Round(time.Millisecond), err, bound, fbDrain)
	}
}

// assertDeadlineLines: every line msg carries the error of its own deadline,
// and there are want of them.
func assertDeadlineLines(t *testing.T, logs *rejectLogCapture, msg string, want int) {
	t.Helper()
	recs := logs.withMsg(msg)
	if len(recs) != want {
		t.Fatalf("want %d %q lines, got %+v", want, msg, recs)
	}
	for _, r := range recs {
		if err, _ := r.attrs["error"].(error); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%q ended with %v, want its own deadline", msg, r.attrs["error"])
		}
	}
}

// fbLockWaiter polls pg_stat_activity until a backend of this database other
// than skip waits on a lock of the kind event ('advisory', 'relation') and
// returns its pid.
func fbLockWaiter(t *testing.T, pool *pgxpool.Pool, event string, skip int32) int32 {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var pid int32
		err := pool.QueryRow(context.Background(),
			`SELECT pid FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = $1 AND pid <> $2
			  LIMIT 1`, event, skip).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
	}
	t.Fatalf("no backend started waiting on a %s lock within 10 s", event)
	return 0
}

// fbWaits polls until backend pid waits on a lock.
func fbWaits(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var waits bool
		if err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`, pid).Scan(&waits); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waits {
			return
		}
	}
	t.Fatalf("backend %d never queued for its lock", pid)
}

// fbHoldClassify makes the classification UPDATE of a block whose title is
// LIKE like wait on the advisory lock key — a trigger named name that fires
// only when type_name changes, which an upsert's insert does not do.
func fbHoldClassify(t *testing.T, pool *pgxpool.Pool, name, like string, key int64) {
	t.Helper()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION fb_hold_classify() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN PERFORM pg_advisory_xact_lock(TG_ARGV[0]::bigint); RETURN NEW; END $$`,
		fmt.Sprintf(`CREATE OR REPLACE TRIGGER %s BEFORE UPDATE OF type_name ON context_blocks FOR EACH ROW
		 WHEN (NEW.title LIKE '%s' AND OLD.type_name IS DISTINCT FROM NEW.type_name)
		 EXECUTE FUNCTION fb_hold_classify('%d')`, name, like, key),
	} {
		if _, err := pool.Exec(context.Background(), q); err != nil {
			t.Fatalf("classify hold fixture: %v", err)
		}
	}
}

// holdAfterCommit parks a route's first statement on holdTable after its
// write to writeTable committed — the route's post-commit step. parked waits
// until that statement waits; release lets it go on (also at cleanup).
//
// A trigger holds the write on the advisory lock key while a second
// transaction takes ACCESS EXCLUSIVE on holdTable — queued behind the write
// when it is the same table. Releasing the write lets it commit, the second
// transaction then holds the table, and the route's next statement on it
// waits. On the same table the hold is granted only once the write's
// transaction ended, so the waiting statement comes after it; on another
// table a statement BEFORE the commit could wait first, and committed (which
// must not read holdTable) tells the two apart — nil for the same table.
func holdAfterCommit(t *testing.T, pool *pgxpool.Pool, writeTable, holdTable string, key int64, committed func() bool) (parked, release func()) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION fb_hold_write() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN PERFORM pg_advisory_xact_lock(TG_ARGV[0]::bigint); RETURN NEW; END $$`,
		fmt.Sprintf(`CREATE OR REPLACE TRIGGER fb_hold_write BEFORE INSERT OR UPDATE ON %s
		 FOR EACH ROW EXECUTE FUNCTION fb_hold_write('%d')`, writeTable, key),
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("write hold fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS fb_hold_write ON %s`, writeTable))
	})
	releaseWrite := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, key)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin hold tx: %v", err)
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("hold tx pid: %v", err)
	}
	lockCtx, cancelLock := context.WithCancel(ctx)
	locked := make(chan error, 1)
	started := false
	var once sync.Once
	release = func() {
		once.Do(func() {
			releaseWrite()
			cancelLock()
			if started {
				<-locked
			}
			_ = tx.Rollback(context.Background())
		})
	}
	t.Cleanup(release)
	parked = func() {
		t.Helper()
		fbLockWaiter(t, pool, "advisory", 0) // the write waits in its trigger
		started = true
		go func() {
			_, err := tx.Exec(lockCtx, fmt.Sprintf(`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`, holdTable))
			locked <- err
		}()
		if holdTable == writeTable {
			fbWaits(t, pool, pid) // queued behind the write
		}
		releaseWrite()
		select {
		case err := <-locked:
			if err != nil {
				t.Fatalf("take %s: %v", holdTable, err)
			}
			locked <- nil // for release
		case <-time.After(10 * time.Second):
			t.Fatalf("the write never committed")
		}
		fbLockWaiter(t, pool, "relation", pid)
		if committed != nil && !committed() {
			t.Fatalf("the route waits on %s before its write committed", holdTable)
		}
	}
	return parked, release
}

// fbCount is the count of a fixture query.
func fbCount(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// fbEnvConfig is a config store built from an empty environment and the
// database's override rows, with a sealbox master key.
func fbEnvConfig(t *testing.T, pool *pgxpool.Pool) *config.Store {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	t.Setenv(sealbox.EnvKey, freshKeyHexHandler(t))
	t.Setenv(sealbox.EnvKeyPrev, "")
	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	st := config.NewStore(envCfg)
	if err := settings.Reload(context.Background(), pool, st); err != nil {
		t.Fatalf("settings reload: %v", err)
	}
	return st
}

// fbAdmin is a server-admin result on a real key row (the audit triggers
// cast the actor id to uuid).
func fbAdmin(t *testing.T, pool *pgxpool.Pool) *auth.AuthResult {
	t.Helper()
	k, _, err := store.CreateApiKey(context.Background(), pool, "fb-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	return &auth.AuthResult{IsValid: true, IsAdmin: true, ApiKeyID: k.ID,
		HomeScope: "_global", ReadScopes: []string{"_global"}}
}

// TestFollowupBoundStoreBatch — a batch's afterwork (classification and
// temporal enrichment of each written block) after its commit: it lands
// after the client left, and one bound ends all of it inside the drain.
func TestFollowupBoundStoreBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	fbHoldClassify(t, f.pool, "fb_hold_batch", "session i453 bound%", fbKeyBatch)
	batch := func(a, b string) string {
		raw, _ := json.Marshal(sbItems(sbItem(a, "afterwork of "+a), sbItem(b, "afterwork of "+b)))
		return string(raw)
	}
	directRoute := func(t *testing.T) (*httptest.Server, <-chan struct{}, <-chan context.Context) {
		return fbServer(t, Auth(f.pool)(http.HandlerFunc(NewStoreHandler(f.pool, f.cfg, f.reg).HandleStoreBatch)))
	}
	confirmRoute := func(t *testing.T) (*httptest.Server, <-chan struct{}, <-chan context.Context) {
		return fbServer(t, Auth(f.pool)(http.HandlerFunc(NewConfirmHandler(f.pool, f.reg).HandleConfirm)))
	}
	// staged stages the batch with a confirm_writes key and returns the key
	// and the confirm body.
	staged := func(t *testing.T, body string) (sbKey, string) {
		t.Helper()
		k := f.key(t, true)
		var items map[string]any
		_ = json.Unmarshal([]byte(body), &items)
		a := f.mcpCall(t, k, "store_batch", items)
		m := sbHashRe.FindStringSubmatch(a.Text)
		if m == nil {
			t.Fatalf("stage: %+v", a)
		}
		raw, _ := json.Marshal(map[string]any{"payload_hash": m[1]})
		return k, string(raw)
	}

	t.Run("direct: the afterwork lands after the client left", func(t *testing.T) {
		k := f.key(t, false)
		installRejectLogCapture(t)
		srv, done, reqs := directRoute(t)
		release := lockedTx(t, f.pool, `SELECT pg_advisory_xact_lock($1)`, fbKeyBatch)
		q := fbSend(t, http.MethodPost, srv.URL+"/api/store/batch", k.plain, batch("session i453 bound a1", "session i453 bound a2"))
		waitLockWaiters(t, f.pool, 1)
		q.leave(t, f.pool, reqs)
		release()
		waitDone(t, done)
		for _, title := range []string{"session i453 bound a1", "session i453 bound a2"} {
			if got := typeOf(t, f.pool, title); got != "audit-trail" {
				t.Errorf("type of %q after the client left = %q, want audit-trail", title, got)
			}
		}
	})

	t.Run("direct: one bound ends the whole afterwork inside the drain", func(t *testing.T) {
		k := f.key(t, false)
		logs := installRejectLogCapture(t)
		srv, done, _ := directRoute(t)
		release := lockedTx(t, f.pool, `SELECT pg_advisory_xact_lock($1)`, fbKeyBatch)
		fbSend(t, http.MethodPost, srv.URL+"/api/store/batch", k.plain, batch("session i453 bound b1", "session i453 bound b2"))
		waitLockWaiters(t, f.pool, 1)
		elapsed, err := fbShutdown(srv)
		release()
		waitDone(t, done)
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "store batch: auto-classify failed", 2)
	})

	t.Run("confirmed: the afterwork lands after the client left", func(t *testing.T) {
		k, body := staged(t, batch("session i453 bound c1", "session i453 bound c2"))
		installRejectLogCapture(t)
		srv, done, reqs := confirmRoute(t)
		release := lockedTx(t, f.pool, `SELECT pg_advisory_xact_lock($1)`, fbKeyBatch)
		q := fbSend(t, http.MethodPost, srv.URL+"/api/confirm", k.plain, body)
		waitLockWaiters(t, f.pool, 1)
		q.leave(t, f.pool, reqs)
		release()
		waitDone(t, done)
		for _, title := range []string{"session i453 bound c1", "session i453 bound c2"} {
			if got := typeOf(t, f.pool, title); got != "audit-trail" {
				t.Errorf("type of %q after the client left = %q, want audit-trail", title, got)
			}
		}
	})

	t.Run("confirmed: one bound ends the whole afterwork inside the drain", func(t *testing.T) {
		k, body := staged(t, batch("session i453 bound d1", "session i453 bound d2"))
		logs := installRejectLogCapture(t)
		srv, done, _ := confirmRoute(t)
		release := lockedTx(t, f.pool, `SELECT pg_advisory_xact_lock($1)`, fbKeyBatch)
		fbSend(t, http.MethodPost, srv.URL+"/api/confirm", k.plain, body)
		waitLockWaiters(t, f.pool, 1)
		elapsed, err := fbShutdown(srv)
		release()
		waitDone(t, done)
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "store batch: auto-classify failed", 2)
	})
}

// TestFollowupBoundBatchAfterworkFits measures the afterwork of the largest
// batch a config may allow (config.WritesBatchMaxItemsCeiling, issue #483)
// against the database — the loop runStoreBatch runs after the commit, every
// block classified and enriched — and holds that one followUpTimeout covers
// it.
func TestFollowupBoundBatchAfterworkFits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := v11Registry(t, pool)
	set := reg.Snapshot()
	maxItems := config.WritesBatchMaxItemsCeiling
	content := strings.Repeat("Termin am 2026-11-21 um 14:00, Folgetermin 3. Dezember 2026, Frist bis 15.01.2027. ", 12)
	var worst time.Duration
	for round := range 2 {
		items := make([]store.BatchUpsert, maxItems)
		for i := range items {
			items[i] = store.BatchUpsert{Category: "learnings", Title: fmt.Sprintf("session i453 measure %d-%04d", round, i),
				Content: content, Scope: "private", Sens: store.SensitivityWrite{Value: backends.SensInternal}}
		}
		results, err := store.UpsertBlocks(ctx, pool, items, "", set, nil)
		if err != nil {
			t.Fatalf("upsert batch: %v", err)
		}
		after, cancel := followUpContext(ctx)
		begin := time.Now()
		for _, r := range results {
			finishStoredBlock(after, pool, set, r.Block, "")
		}
		elapsed := time.Since(begin)
		cancel()
		worst = max(worst, elapsed)
		t.Logf("afterwork of a %d-item batch, round %d: %v", maxItems, round, elapsed.Round(time.Millisecond))
		if n := fbCount(t, pool, `SELECT count(*)::int FROM context_blocks WHERE title LIKE $1 AND type_name = 'audit-trail'`,
			fmt.Sprintf("session i453 measure %d-%%", round)); n != maxItems {
			t.Fatalf("round %d classified %d of %d blocks", round, n, maxItems)
		}
	}
	if worst > followUpTimeout {
		t.Errorf("the afterwork of a %d-item batch took %v; one follow-up bound of %v does not cover it", maxItems, worst, followUpTimeout)
	}
}

// TestFollowupBoundDigest — POST /api/digest: the classification after the
// topic map's upsert (full and stub mode) and after the root map's upsert
// lands after the client left; both end inside the drain on their bounds.
func TestFollowupBoundDigest(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	plain := abortKey(t, pool)
	reg := v11Registry(t, pool)
	if _, err := store.UpsertBlock(ctx, pool, "learnings", "fb digest fixture", "a block the maps index", nil, nil, "private", false,
		store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := wdDigestConfig()
	h := NewDigestHandler(pool, reg, staticConfigStore{cfg: cfg})
	fbHoldClassify(t, pool, "fb_hold_topic_map", "topic-map-private", fbKeyTopicMap)
	fbHoldClassify(t, pool, "fb_hold_root_map", "root-map-private", fbKeyRootMap)
	reset := func(t *testing.T, mode string, rootMap bool) {
		t.Helper()
		cfg.Digest.Mode, cfg.RootMap.Enabled = mode, rootMap
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE category = 'index' AND title IN ('topic-map-private', 'root-map-private')`); err != nil {
			t.Fatal(err)
		}
	}
	route := func(t *testing.T) (*httptest.Server, <-chan struct{}, <-chan context.Context) {
		return fbServer(t, Auth(pool)(http.HandlerFunc(h.HandleDigest)))
	}
	// classifiedAfterLeave parks the classification of title on key, lets
	// the client leave, then lets the classification go on. The request has
	// no body: the handler never reads one, and net/http sees a client go
	// only once the body is consumed — a client that sent one (the CLI sends
	// {"trigger":"manual"}) leaves unseen until the answer is written.
	classifiedAfterLeave := func(t *testing.T, title string, key int64) {
		t.Helper()
		installRejectLogCapture(t)
		srv, done, reqs := route(t)
		release := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, key)
		q := fbSend(t, http.MethodPost, srv.URL+"/api/digest", plain, "")
		waitLockWaiters(t, pool, 1)
		q.leave(t, pool, reqs)
		release()
		waitDone(t, done)
		if got := typeOf(t, pool, title); got != "system-meta" {
			t.Errorf("type of %s after the client left past its upsert = %q (in the retrieval allowlist: %v), want system-meta",
				title, got, slices.Contains(reg.Snapshot().VisibleTypes(), got))
		}
	}

	t.Run("full: the topic map's classification lands after the client left", func(t *testing.T) {
		reset(t, "full", false)
		classifiedAfterLeave(t, "topic-map-private", fbKeyTopicMap)
	})

	t.Run("stub: the stub's classification lands after the client left", func(t *testing.T) {
		reset(t, "stub", false)
		classifiedAfterLeave(t, "topic-map-private", fbKeyTopicMap)
	})

	t.Run("root map: its classification lands after the client left", func(t *testing.T) {
		reset(t, "full", true)
		classifiedAfterLeave(t, "root-map-private", fbKeyRootMap)
	})

	t.Run("both classifications end inside the drain on their bounds", func(t *testing.T) {
		reset(t, "full", true)
		logs := installRejectLogCapture(t)
		srv, done, _ := route(t)
		releaseTopic := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, fbKeyTopicMap)
		releaseRoot := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, fbKeyRootMap)
		fbSend(t, http.MethodPost, srv.URL+"/api/digest", plain, `{"trigger":"manual"}`)
		waitLockWaiters(t, pool, 1)
		elapsed, err := fbShutdown(srv)
		releaseTopic()
		releaseRoot()
		waitDone(t, done)
		// The two bounds together stay within the handler's follow-up bound.
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "digest: topic map auto-classify failed", 1)
		assertDeadlineLines(t, logs, "rootmap: classify failed", 1)
	})
}

// TestFollowupBoundSettingsReload — the config snapshot reload after PUT
// /api/settings/{key} and PUT /api/secrets/{name}: it completes after the
// client left, and its bound ends it inside the drain.
func TestFollowupBoundSettingsReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfgStore := fbEnvConfig(t, pool)
	ar := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: "private", ReadScopes: []string{"private"}}
	route := func(t *testing.T) (*httptest.Server, <-chan struct{}, <-chan context.Context) {
		r := chi.NewRouter()
		MountSettings(r, NewSettingsHandler(pool, cfgStore))
		MountSecrets(r, NewSecretsHandler(pool, cfgStore))
		return fbServer(t, fbAs(ar, r))
	}
	const key = "query.rate_limit_write"
	settingHeld := func(t *testing.T) (parked, release func()) {
		return holdAfterCommit(t, pool, "context_settings", "context_settings", fbKeySettings, nil)
	}
	secretHeld := func(t *testing.T, name string) (parked, release func()) {
		return holdAfterCommit(t, pool, "context_secrets", "context_settings", fbKeySecrets, func() bool {
			return fbCount(t, pool, `SELECT count(*)::int FROM context_secrets WHERE name = $1`, name) == 1
		})
	}

	t.Run("settings: the reload completes after the client left", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done, reqs := route(t)
		parked, release := settingHeld(t)
		q := fbSend(t, http.MethodPut, srv.URL+"/api/settings/"+key, "", `{"value":77}`)
		parked()
		q.leave(t, pool, reqs)
		release()
		waitDone(t, done)
		if got := cfgStore.Snapshot().Query.RateLimitWrite; got != 77 {
			t.Errorf("%s in the snapshot after the client left = %d, want 77 (the reload was cut)", key, got)
		}
		if recs := logs.withMsg("settings: persisted but reload failed"); len(recs) != 0 {
			t.Errorf("reload failed after the client left: %+v", recs)
		}
	})

	t.Run("settings: the reload's bound ends it inside the drain", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done, _ := route(t)
		parked, release := settingHeld(t)
		fbSend(t, http.MethodPut, srv.URL+"/api/settings/"+key, "", `{"value":78}`)
		parked()
		elapsed, err := fbShutdown(srv)
		release()
		waitDone(t, done)
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "settings: persisted but reload failed", 1)
	})

	t.Run("secrets: the reload completes after the client left", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done, reqs := route(t)
		before := cfgStore.Snapshot()
		parked, release := secretHeld(t, "fb-secret-one")
		q := fbSend(t, http.MethodPut, srv.URL+"/api/secrets/fb-secret-one", "", `{"value":"fb secret value one"}`)
		parked()
		q.leave(t, pool, reqs)
		release()
		waitDone(t, done)
		if cfgStore.Snapshot() == before {
			t.Errorf("the snapshot was not replaced after the client left (the reload was cut)")
		}
		if recs := logs.withMsg("secrets: persisted but reload failed"); len(recs) != 0 {
			t.Errorf("reload failed after the client left: %+v", recs)
		}
	})

	t.Run("secrets: the reload's bound ends it inside the drain", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done, _ := route(t)
		parked, release := secretHeld(t, "fb-secret-two")
		fbSend(t, http.MethodPut, srv.URL+"/api/secrets/fb-secret-two", "", `{"value":"fb secret value two"}`)
		parked()
		elapsed, err := fbShutdown(srv)
		release()
		waitDone(t, done)
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "secrets: persisted but reload failed", 1)
	})
}

// TestFollowupBoundBackendsReload — the backend pool reload after a backend
// write (reloadAfterMutation, every backend and disable-profile write and the
// eject toggle): it completes after the client left, and its bound ends it
// inside the drain.
func TestFollowupBoundBackendsReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfgStore := fbEnvConfig(t, pool)
	bp := backends.NewPool(pool, nil)
	if err := bp.Reload(context.Background()); err != nil {
		t.Fatalf("pool reload: %v", err)
	}
	h := NewManageHandler(pool, cfgStore, nil, bp, nil, nil, nil, nil)
	admin := fbAdmin(t, pool)
	create := func(name string) string {
		raw, _ := json.Marshal(map[string]any{"action": "backend-create", "data": map[string]any{
			"name": name, "base_url": "http://" + name, "roles": []string{"chat"},
			"model_map": map[string]any{"default": "stub-model"}}})
		return string(raw)
	}
	held := func(t *testing.T, name string) (parked, release func()) {
		return holdAfterCommit(t, pool, "context_backends", "context_disable_profiles", fbKeyBackends, func() bool {
			return fbCount(t, pool, `SELECT count(*)::int FROM context_backends WHERE name = $1`, name) == 1
		})
	}
	inSnapshot := func(name string) bool {
		return slices.ContainsFunc(bp.Snapshot(), func(b backends.Backend) bool { return b.Name == name })
	}

	t.Run("the reload completes after the client left", func(t *testing.T) {
		installRejectLogCapture(t)
		srv, done, reqs := fbServer(t, fbAs(admin, http.HandlerFunc(h.HandleManage)))
		parked, release := held(t, "fb-backend-one")
		q := fbSend(t, http.MethodPost, srv.URL+"/api/manage", "", create("fb-backend-one"))
		parked()
		q.leave(t, pool, reqs)
		release()
		waitDone(t, done)
		if !inSnapshot("fb-backend-one") {
			t.Errorf("the pool snapshot lacks the backend created before the client left (the reload was cut)")
		}
	})

	t.Run("the reload's bound ends it inside the drain", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done, _ := fbServer(t, fbAs(admin, http.HandlerFunc(h.HandleManage)))
		parked, release := held(t, "fb-backend-two")
		fbSend(t, http.MethodPost, srv.URL+"/api/manage", "", create("fb-backend-two"))
		parked()
		elapsed, err := fbShutdown(srv)
		release()
		waitDone(t, done)
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "backends: post-mutation reload failed — NOTIFY path will converge", 1)
	})
}

// TestFollowupBoundBlockTypeReload — the registry reload after a type write
// (reloadBlockTypes, the manage family and /api/types): it completes after
// the client left, and its bound ends it inside the drain.
func TestFollowupBoundBlockTypeReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	reg := v11Registry(t, pool)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	admin := fbAdmin(t, pool)
	create := func(name string) string {
		raw, _ := json.Marshal(map[string]any{"action": "type-create", "data": map[string]any{"name": name, "config": map[string]any{"v": 1}}})
		return string(raw)
	}
	held := func(t *testing.T) (parked, release func()) {
		return holdAfterCommit(t, pool, "context_block_types", "context_block_types", fbKeyBlockType, nil)
	}

	t.Run("the reload completes after the client left", func(t *testing.T) {
		installRejectLogCapture(t)
		srv, done, reqs := fbServer(t, fbAs(admin, http.HandlerFunc(h.HandleManage)))
		parked, release := held(t)
		q := fbSend(t, http.MethodPost, srv.URL+"/api/manage", "", create("followup-probe-one"))
		parked()
		q.leave(t, pool, reqs)
		release()
		waitDone(t, done)
		if !slices.Contains(reg.Snapshot().Names(), "followup-probe-one") {
			t.Errorf("the registry snapshot lacks the type created before the client left (the reload was cut)")
		}
	})

	t.Run("the reload's bound ends it inside the drain", func(t *testing.T) {
		logs := installRejectLogCapture(t)
		srv, done, _ := fbServer(t, fbAs(admin, http.HandlerFunc(h.HandleManage)))
		parked, release := held(t)
		fbSend(t, http.MethodPost, srv.URL+"/api/manage", "", create("followup-probe-two"))
		parked()
		elapsed, err := fbShutdown(srv)
		release()
		waitDone(t, done)
		assertDrained(t, elapsed, err, followUpTimeout)
		assertDeadlineLines(t, logs, "manage: block-type registry reload after mutation failed — NOTIFY listener will retry", 1)
	})
}

// TestFollowupBoundGamingReload — the settings reload after the eject/gaming
// toggle (the settingsReload hook, here a fake that waits until the test
// lets it go on): it runs on a context the client's leaving does not end,
// bounded by followUpTimeout.
func TestFollowupBoundGamingReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfgStore := fbEnvConfig(t, pool)
	bp := backends.NewPool(pool, nil)
	if err := bp.Reload(context.Background()); err != nil {
		t.Fatalf("pool reload: %v", err)
	}
	type seen struct {
		err         error
		deadline    time.Time
		hasDeadline bool
	}
	parked, proceed, got := make(chan struct{}), make(chan struct{}), make(chan seen, 1)
	reload := func(ctx context.Context) error {
		close(parked)
		<-proceed
		dl, ok := ctx.Deadline()
		got <- seen{err: ctx.Err(), deadline: dl, hasDeadline: ok}
		return ctx.Err()
	}
	h := NewManageHandler(pool, cfgStore, nil, bp, nil, reload, nil, nil)
	logs := installRejectLogCapture(t)
	srv, done, reqs := fbServer(t, fbAs(fbAdmin(t, pool), http.HandlerFunc(h.HandleManage)))
	q := fbSend(t, http.MethodPost, srv.URL+"/api/manage", "", `{"action":"gaming-mode","data":{"mode":"on"}}`)
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the toggle never reached its settings reload")
	}
	q.leave(t, pool, reqs)
	close(proceed)
	s := <-got
	waitDone(t, done)
	if s.err != nil {
		t.Errorf("the settings reload's context ended with the client (%v)", s.err)
	}
	if !s.hasDeadline || time.Until(s.deadline) > followUpTimeout {
		t.Errorf("the settings reload's context has deadline %v (set: %v), want one within %v", s.deadline, s.hasDeadline, followUpTimeout)
	}
	if recs := logs.withMsg("eject/gaming-mode: persisted but reload failed"); len(recs) != 0 {
		t.Errorf("the toggle reported a failed reload after the client left: %+v", recs)
	}
	for _, r := range logs.all() {
		if r.level >= slog.LevelError {
			t.Errorf("ERROR line after the client left: %+v", r)
		}
	}
}

// TestFollowupBoundGamingReloadsShareOneBound — the eject/gaming toggle runs
// two reloads after its commit, the backend pool's and the settings'; one
// followUpTimeout bounds both, so the pair stays inside the shutdown's drain
// (two bounds of 10 s would be 20 s against its 15 s). The backend reload is
// held on a table lock until it waits; the settings reload must then see a
// deadline that began before the lock went away.
func TestFollowupBoundGamingReloadsShareOneBound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfgStore := fbEnvConfig(t, pool)
	bp := backends.NewPool(pool, nil)
	if err := bp.Reload(context.Background()); err != nil {
		t.Fatalf("pool reload: %v", err)
	}
	var lockRelease func()
	prev := beforeGamingCommit
	beforeGamingCommit = func() error {
		lockRelease = lockedTx(t, pool, `LOCK TABLE context_backends IN ACCESS EXCLUSIVE MODE`)
		return nil
	}
	t.Cleanup(func() { beforeGamingCommit = prev })
	deadlines := make(chan time.Time, 1)
	reload := func(ctx context.Context) error {
		dl, _ := ctx.Deadline()
		deadlines <- dl
		return nil
	}
	h := NewManageHandler(pool, cfgStore, nil, bp, nil, reload, nil, nil)
	installRejectLogCapture(t)
	srv, done, _ := fbServer(t, fbAs(fbAdmin(t, pool), http.HandlerFunc(h.HandleManage)))
	q := fbSend(t, http.MethodPost, srv.URL+"/api/manage", "", `{"action":"gaming-mode","data":{"mode":"on"}}`)
	waitLockWaiters(t, pool, 1)
	released := time.Now()
	lockRelease()
	var dl time.Time
	select {
	case dl = <-deadlines:
	case <-time.After(15 * time.Second):
		t.Fatal("the settings reload never ran")
	}
	waitDone(t, done)
	_ = q
	if dl.IsZero() || !dl.Before(released.Add(followUpTimeout)) {
		t.Errorf("the settings reload's deadline %v began after the backend reload ended (lock released %v): two bounds of %v, not one", dl, released, followUpTimeout)
	}
}
