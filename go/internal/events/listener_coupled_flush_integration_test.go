//go:build integration

// Gate of Entflechtung-Stufe-2 wave α5 (A04-W3, design/04 §4.2 + §7 row 3,
// E-A04-2 variant A): the embed cache follows the POOL, the living serving
// truth, instead of the config generation it followed on the (since Stufe 1
// sealed) settings path.
//
// The five gate cases, all driven through the real notification funnel
// (HandleNotification), never by calling the flush directly:
//
//   - base_url edit on an embed row        → context_embed_cache flushed
//   - model-only edit on the same row      → cache untouched (it keys on model)
//   - profile disable with failover host   → cache flushed
//   - injected flush error                 → next notification retries
//   - pool reload failure                  → no flush (snapshot never moved)
//
// Run: go test -tags=integration ./internal/events/ -run TestCoupled -count=1 -v
//
// Negative probe of the wave: comment out the three flushIfCoupledChanged call
// sites in listener.go — the four flush cases go red, the two no-flush
// counter-cases stay green (they assert an absence).
package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

const coupledEmbedModel = "coupled-embed-model"

// coupledSeedBackend inserts an enabled, global-scoped embed backend through the
// production CreateBackend path and returns its id.
func coupledSeedBackend(t *testing.T, pool *pgxpool.Pool, name, host string, priority int) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &backends.Backend{
		Name: name, Host: host, Protocol: backends.ProtocolOllama,
		ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull,
		Locality: backends.LocalityLocal, Roles: []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: coupledEmbedModel}},
		Priority: priority, Enabled: true, Scope: backends.GlobalScope,
	}
	id, err := store.CreateBackend(ctx, tx, b, nil)
	if err != nil {
		t.Fatalf("create backend %s: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit backend %s: %v", name, err)
	}
	return id
}

// coupledSeedCache puts one row into context_embed_cache — the thing a flush is
// visible in.
func coupledSeedCache(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
		 VALUES ($1, $2, $3, 'coupled probe')`,
		[]byte("coupled-probe-hash"), coupledEmbedModel, pgvec.NewVector(make([]float32, 1024))); err != nil {
		t.Fatalf("seed embed cache: %v", err)
	}
}

func coupledCacheCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_embed_cache`).Scan(&n); err != nil {
		t.Fatalf("count embed cache: %v", err)
	}
	return n
}

// coupledNotify fabricates the payload the 053/092 triggers put on
// ctx_settings_write for the given entity.
func coupledNotify(entity string) *pgconn.Notification {
	payload, _ := json.Marshal(map[string]string{"entity": entity, "op": "UPDATE"})
	return &pgconn.Notification{Channel: channelSettingsWrite, Payload: string(payload)}
}

// coupledHandlerFor builds the handler on a given backend pool; the config store
// is a valid env-only generation so the backlog path's settings reload succeeds.
func coupledHandlerFor(t *testing.T, db *pgxpool.Pool, bp *backends.Pool) *SettingsWriteHandler {
	t.Helper()
	return NewSettingsWriteHandler(db, config.NewStore(envBaseConfig(t)), bp, nil)
}

// coupledHandler wires a handler on a freshly loaded pool — the boot posture
// (pool loaded before the listener exists, cmd/ctxd/main.go), so the baseline is
// the live topology.
func coupledHandler(t *testing.T, pool *pgxpool.Pool) (*SettingsWriteHandler, *backends.Pool) {
	t.Helper()
	bp := backends.NewPool(pool, nil)
	if err := bp.Reload(context.Background()); err != nil {
		t.Fatalf("initial pool reload: %v", err)
	}
	return coupledHandlerFor(t, pool, bp), bp
}

// TestCoupledFlushOnEmbedHostEdit is gate case 1: a base_url edit moves the
// vector space while the model name — the cache key — stays put. Against the
// pre-α5 stand this is exactly the silent stale-serving path (§5.1 R5): the
// pool reloads, nothing else happens, old vectors keep answering.
func TestCoupledFlushOnEmbedHostEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, pool, "coupled-embed-a", "http://127.0.0.1:11434", 70)
	h, _ := coupledHandler(t, pool)
	coupledSeedCache(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE context_backends SET base_url = $2 WHERE id = $1`, id, "http://10.13.37.19:11434"); err != nil {
		t.Fatalf("edit base_url: %v", err)
	}
	if err := h.HandleNotification(ctx, coupledNotify("context_backends"), nil); err != nil {
		t.Fatalf("handle notification: %v", err)
	}
	if n := coupledCacheCount(t, pool); n != 0 {
		t.Fatalf("embed cache rows after base_url edit = %d, want 0 (flush)", n)
	}
}

// TestCoupledNoFlushOnModelEdit is gate case 2: the counter-case that keeps the
// diff honest. context_embed_cache keys on (text_hash, model), so a model change
// addresses different rows by itself — flushing there would throw away a warm
// cache for nothing (same reasoning the config-side coupled check has carried).
func TestCoupledNoFlushOnModelEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, pool, "coupled-embed-a", "http://127.0.0.1:11434", 70)
	h, _ := coupledHandler(t, pool)
	coupledSeedCache(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE context_backends SET model_map = $2 WHERE id = $1`,
		id, []byte(`{"default":"coupled-other-model"}`)); err != nil {
		t.Fatalf("edit model_map: %v", err)
	}
	if err := h.HandleNotification(ctx, coupledNotify("context_backends"), nil); err != nil {
		t.Fatalf("handle notification: %v", err)
	}
	if n := coupledCacheCount(t, pool); n != 1 {
		t.Fatalf("embed cache rows after model-only edit = %d, want 1 (no flush)", n)
	}
}

// TestCoupledFlushOnProfileDisableFailover is gate case 3 and the reason the set
// is defined over serving-eligible rows rather than the enabled column: a
// disable profile never touches `enabled`, yet ejecting the primary hands embed
// serving to a failover on a different host — under the same model name. A
// diff blind to profiles would leave exactly that cross-space path open.
func TestCoupledFlushOnProfileDisableFailover(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	primary := coupledSeedBackend(t, pool, "coupled-embed-primary", "http://127.0.0.1:11434", 70)
	coupledSeedBackend(t, pool, "coupled-embed-failover", "http://10.13.37.19:11434", 50)
	h, _ := coupledHandler(t, pool)
	coupledSeedCache(t, pool)

	var profileID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_disable_profiles (scope, name, label, active)
		 VALUES ('_global', 'coupled-wartung', 'Wartung', true) RETURNING id`).Scan(&profileID); err != nil {
		t.Fatalf("create disable profile: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_disable_profile_backends (profile_id, backend_id) VALUES ($1, $2)`,
		profileID, primary); err != nil {
		t.Fatalf("add profile member: %v", err)
	}
	if err := h.HandleNotification(ctx, coupledNotify("context_disable_profile_backends"), nil); err != nil {
		t.Fatalf("handle notification: %v", err)
	}
	if n := coupledCacheCount(t, pool); n != 0 {
		t.Fatalf("embed cache rows after profile disable = %d, want 0 (flush)", n)
	}
}

// TestCoupledFlushRetriesAfterFailure is gate case 4 — the property that makes
// "the next write retries" a mechanism instead of a phrase: the comparison stand
// advances ONLY after a successful flush. Were it advanced unconditionally, the
// failed flush would leave identical sets forever and stale vectors would serve
// without limit.
func TestCoupledFlushRetriesAfterFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, pool, "coupled-embed-a", "http://127.0.0.1:11434", 70)
	h, _ := coupledHandler(t, pool)
	coupledSeedCache(t, pool)

	calls := 0
	h.flush = func(ctx context.Context, p *pgxpool.Pool) (int64, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("injected flush failure")
		}
		return embedcache.Flush(ctx, p)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE context_backends SET base_url = $2 WHERE id = $1`, id, "http://10.13.37.19:11434"); err != nil {
		t.Fatalf("edit base_url: %v", err)
	}
	if err := h.HandleNotification(ctx, coupledNotify("context_backends"), nil); err != nil {
		t.Fatalf("handle notification (failing flush): %v", err)
	}
	if calls != 1 {
		t.Fatalf("flush attempts after first notification = %d, want 1", calls)
	}
	if n := coupledCacheCount(t, pool); n != 1 {
		t.Fatalf("embed cache rows after failed flush = %d, want 1 (nothing deleted)", n)
	}

	// Second notification, no further row change: the un-advanced stand still
	// differs from the pool, so the flush is attempted again — and sticks.
	if err := h.HandleNotification(ctx, coupledNotify("context_backends"), nil); err != nil {
		t.Fatalf("handle notification (retry): %v", err)
	}
	if calls != 2 {
		t.Fatalf("flush attempts after retry notification = %d, want 2", calls)
	}
	if n := coupledCacheCount(t, pool); n != 0 {
		t.Fatalf("embed cache rows after retry = %d, want 0 (flush)", n)
	}
}

// coupledFailingQuerier makes backends.Pool.Reload fail without touching the DB.
type coupledFailingQuerier struct{}

func (coupledFailingQuerier) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("injected reload failure")
}

// TestCoupledNoFlushOnReloadFailure is gate case 5: a failed reload keeps the
// previous snapshot active ("previous snapshot stays active" doctrine), so the
// derived set is the same one as before and nothing is flushed. The DB row here
// carries the CHANGED host on purpose — a successful reload would have flushed,
// which is what makes the assertion say something.
func TestCoupledNoFlushOnReloadFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	coupledSeedBackend(t, pool, "coupled-embed-a", "http://10.13.37.19:11434", 70)
	coupledSeedCache(t, pool)

	bp := backends.NewPool(coupledFailingQuerier{}, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "a", Name: "coupled-embed-a", Host: "http://127.0.0.1:11434",
		Protocol: backends.ProtocolOllama, Roles: []string{backends.RoleEmbed},
		Enabled: true, Scope: backends.GlobalScope,
	}})
	h := coupledHandlerFor(t, pool, bp)
	calls := 0
	h.flush = func(context.Context, *pgxpool.Pool) (int64, error) { calls++; return 0, nil }

	if err := h.HandleNotification(ctx, coupledNotify("context_backends"), nil); err != nil {
		t.Fatalf("handle notification: %v", err)
	}
	if calls != 0 {
		t.Fatalf("flush attempts after failed reload = %d, want 0", calls)
	}
	if n := coupledCacheCount(t, pool); n != 1 {
		t.Fatalf("embed cache rows after failed reload = %d, want 1 (no flush)", n)
	}
}

// TestCoupledFlushOnBacklogReconnect covers the branch the gate list does not
// name but the funnel does: a backend edit during a listener disconnect loses
// its notification, and HandleBacklog is the only place that still sees it. A
// reconnect without any change diffs empty, so this costs no flush at idle.
func TestCoupledFlushOnBacklogReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	id := coupledSeedBackend(t, pool, "coupled-embed-a", "http://127.0.0.1:11434", 70)
	h, _ := coupledHandler(t, pool)
	coupledSeedCache(t, pool)

	if err := h.HandleBacklog(ctx, channelSettingsWrite, nil); err != nil {
		t.Fatalf("handle backlog (unchanged): %v", err)
	}
	if n := coupledCacheCount(t, pool); n != 1 {
		t.Fatalf("embed cache rows after unchanged reconnect = %d, want 1 (no flush)", n)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE context_backends SET base_url = $2 WHERE id = $1`, id, "http://10.13.37.19:11434"); err != nil {
		t.Fatalf("edit base_url: %v", err)
	}
	if err := h.HandleBacklog(ctx, channelSettingsWrite, nil); err != nil {
		t.Fatalf("handle backlog (missed edit): %v", err)
	}
	if n := coupledCacheCount(t, pool); n != 0 {
		t.Fatalf("embed cache rows after missed-edit reconnect = %d, want 0 (flush)", n)
	}
}
