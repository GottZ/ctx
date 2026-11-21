//go:build integration

package handler

// Issue #453 at real routes over a real database, on a real httptest server
// whose client gives up:
//
//   - model-alias-preview (point 3): the chain dry run waits on the request
//     context; its line went out through slog.ErrorContext, which the census
//     of #417 did not see, and stayed ERROR on a client abort.
//   - POST /api/store (point 4): the type classification after the committed
//     upsert waits; the client leaves. The classification runs detached and
//     lands; a shutdown waits for it, and its own bound ends it when the
//     database never answers.
//
//	go test -tags=integration ./internal/handler/ -run 'TestClientAbortFollowups' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// abortManage serves h's /api/manage as ar behind the production RequestID
// and Logger stages and gives up on the call once parked reports that the
// route waits.
func abortManage(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action string, data any, parked <-chan struct{}) <-chan struct{} {
	t.Helper()
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleManage(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}), nil)
	body, err := json.Marshal(map[string]any{"action": action, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/manage", strings.NewReader(string(body)))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-parked
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
	return done
}

// TestClientAbortFollowupsAliasPreview — the client gives up while the chain
// dry run waits: the preview's line is INFO with client_gone=true, the
// request log says 499.
func TestClientAbortFollowupsAliasPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "pv-abort")
	c := mcCreate(t, h, admin, map[string]any{"label": "pv-abort-app", "tenant_id": a.id, "tenant_scope": a.scope})

	parked := make(chan struct{})
	h.SetChainPreview(func(pctx context.Context, _ *auth.ConsumerAuth, _ string) (oai.ChainPreview, error) {
		close(parked)
		<-pctx.Done()
		return oai.ChainPreview{}, fmt.Errorf("chain preview: %w", pctx.Err())
	}, func(string) string { return "" })

	logs := installRejectLogCapture(t)
	waitDone(t, abortManage(t, h, admin, "model-alias-preview", map[string]any{"alias": "chat", "consumer_id": c.id}, parked))

	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that gave up on the preview produced ERROR lines: %+v", errs)
	}
	recs := logs.withMsg("model-alias-preview: chain dry run failed")
	if len(recs) != 1 || recs[0].level >= slog.LevelError || recs[0].attrs["client_gone"] != true {
		t.Errorf("preview line = %+v, want one INFO line with client_gone=true", recs)
	}
}

// classifyHoldTitle classifies to audit-trail by the seeded 'session' title
// pattern (as ingestClassifyAuditTitle); the hold trigger matches it.
const classifyHoldTitle = "Session 453 Handover Protokoll classify"

// classifyHoldLock is the advisory lock the hold trigger waits on.
const classifyHoldLock = 453453

// holdClassification makes the classification UPDATE of classifyHoldTitle
// wait on classifyHoldLock: a trigger that fires only when type_name
// changes, which the insert of the upsert does not do.
func holdClassification(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`CREATE FUNCTION i453_hold_classify() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN PERFORM pg_advisory_xact_lock(453453); RETURN NEW; END $$`,
		`CREATE TRIGGER i453_hold_classify BEFORE UPDATE OF type_name ON context_blocks FOR EACH ROW
		 WHEN (NEW.title = 'Session 453 Handover Protokoll classify' AND OLD.type_name IS DISTINCT FROM NEW.type_name)
		 EXECUTE FUNCTION i453_hold_classify()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("hold fixture: %v", err)
		}
	}
}

// settleCancellation waits until no backend waits on a lock any more (a
// cancelled statement left), at most one second (a detached one stays).
func settleCancellation(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*)::int FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n == 0 {
			return
		}
	}
}

func typeOf(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT type_name FROM context_blocks WHERE title = $1`, title).Scan(&name); err != nil {
		t.Fatalf("read type of %q: %v", title, err)
	}
	return name
}

// TestClientAbortFollowupsClassifyAfterCommit — the client leaves while the
// classification of its committed block waits.
func TestClientAbortFollowupsClassifyAfterCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	plain := abortKey(t, pool)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	if name, ok := reg.Snapshot().Classify(classifyHoldTitle, nil); !ok || name != "audit-trail" {
		t.Fatalf("fixture: %q classifies to (%q, %v), want audit-trail", classifyHoldTitle, name, ok)
	}
	cfg := staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	h := NewStoreHandler(pool, cfg, reg)
	holdClassification(t, pool)
	body := func(title string) string {
		raw, _ := json.Marshal(map[string]any{"category": "i453", "title": title, "content": "client abort after commit: " + title})
		return string(raw)
	}
	reset := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE title = $1`, classifyHoldTitle); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the classification lands after the client left", func(t *testing.T) {
		reset(t)
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleStore)
		release := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, classifyHoldLock)
		abortAfter(t, srv.URL+"/api/store", plain, body(classifyHoldTitle), func() { waitLockWaiters(t, pool, 1) })
		// A classification on the request context is cancelled in the
		// database once the server sees the client go; give that the time
		// it takes before the lock goes away.
		settleCancellation(t, pool)
		release()
		waitDone(t, done)
		assertClientGone(t, logs)
		if got := typeOf(t, pool, classifyHoldTitle); got != "audit-trail" {
			t.Errorf("type after a client abort past the commit = %q, want audit-trail (the classification ran on the request context)", got)
		}
	})

	t.Run("a shutdown waits for the detached classification", func(t *testing.T) {
		reset(t)
		installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleStore)
		release := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, classifyHoldLock)
		abortAfter(t, srv.URL+"/api/store", plain, body(classifyHoldTitle), func() { waitLockWaiters(t, pool, 1) })
		shut := make(chan error, 1)
		go func() {
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			shut <- srv.Config.Shutdown(sctx)
		}()
		select {
		case err := <-shut:
			t.Fatalf("Shutdown returned (%v) while the classification still waited", err)
		case <-time.After(300 * time.Millisecond):
		}
		release()
		if err := <-shut; err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if got := typeOf(t, pool, classifyHoldTitle); got != "audit-trail" {
			t.Errorf("type when Shutdown returned = %q, want audit-trail (the drain ended before the classification)", got)
		}
		waitDone(t, done)
	})

	t.Run("its own bound ends a classification the database never answers", func(t *testing.T) {
		reset(t)
		logs := installRejectLogCapture(t)
		srv, done := abortRoute(t, pool, h.HandleStore)
		release := lockedTx(t, pool, `SELECT pg_advisory_xact_lock($1)`, classifyHoldLock)
		abortAfter(t, srv.URL+"/api/store", plain, body(classifyHoldTitle), func() { waitLockWaiters(t, pool, 1) })
		begin := time.Now()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Config.Shutdown(sctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		elapsed := time.Since(begin)
		release()
		waitDone(t, done)
		// ctxd's HTTP drain (cmd/ctxd shutdownHTTPTimeout) is 15 s.
		if elapsed > followUpTimeout+2*time.Second || elapsed >= 15*time.Second {
			t.Errorf("Shutdown took %v, want it bounded by the follow-up bound %v inside the 15 s drain", elapsed, followUpTimeout)
		}
		recs := logs.withMsg("store: auto-classify failed")
		if len(recs) != 1 {
			t.Fatalf("want one auto-classify failure line, got %+v", logs.all())
		}
		if err, _ := recs[0].attrs["error"].(error); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("classification ended with %v, want its own deadline", recs[0].attrs["error"])
		}
	})
}
