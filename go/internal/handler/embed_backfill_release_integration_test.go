//go:build integration

// Issue #236, point one: a block the backfill parked (an oversize memo,
// next_attempt_at = 'infinity') came back only through a real change of its
// title or content. Raising embed_backfill.max_tokens, or moving to an embed
// backend with a larger context, left it parked until someone ran SQL.
// `manage embed-backfill-release` drops the regular backfill memos of one
// block or of one failure class (wire, oversize) in the caller's range, after
// a dry run whose count and as_of every confirmed page sends back.
//
// The fixtures go through the production paths: the blocks through
// store.UpsertBlock, the parks through the query path's backfill
// (backfillPending, its pre-wire estimate and its classification of the
// backend's oversize answer), the release through the manage dispatcher, the
// re-embed through backfillPending again.
//
//	go test -tags=integration ./internal/handler/ -run 'TestEmbedBackfillRelease' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// relOversizeMarker in a block's text makes relEmbedServer answer the
// backend's oversize refusal (llama.cpp's HTTP 400 exceed_context_size).
const relOversizeMarker = "REL236_TOO_LONG_FOR_THE_BACKEND"

// relEmbedServer answers the Ollama /api/embed wire shape and counts the
// calls: a vector, or the oversize refusal for an input with the marker.
func relEmbedServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		in, _ := json.Marshal(req.Input)
		if strings.Contains(string(in), relOversizeMarker) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"request exceeds the available context size","type":"exceed_context_size_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// relCfg is the backfill tuning of these tests: one block per pass, the
// pre-wire estimate at maxTokens.
func relCfg(maxTokens int) *config.Config {
	return &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 1, MaxTokens: maxTokens, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}
}

// relFixture is one database with the query handler (the backfill) and the
// manage handler (the release) on it.
type relFixture struct {
	pool  *pgxpool.Pool
	query *QueryHandler
	mh    *ManageHandler
	calls *atomic.Int32
}

func newRelFixture(t *testing.T) *relFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	srv, calls := relEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	return &relFixture{
		pool:  pool,
		query: NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t)),
		mh:    NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil),
		calls: calls,
	}
}

// backfill runs one pass of the query path's backfill over scopes.
func (f *relFixture) backfill(scopes []string, cfg *config.Config) int {
	return f.query.backfillPending(context.Background(), nil, scopes, f.query.embedAdmission(), cfg)
}

// release posts embed-backfill-release with data as ar.
func (f *relFixture) release(t *testing.T, ar *auth.AuthResult, data map[string]any) (int, map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"action": "embed-backfill-release", "data": data})
	r := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	r = r.WithContext(context.WithValue(r.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	f.mh.HandleManage(rec, r)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

// releaseConfirmed runs the dry run and then the confirmed call, and returns
// the confirmed call's answer.
func (f *relFixture) releaseConfirmed(t *testing.T, ar *auth.AuthResult, data map[string]any) (int, map[string]any, string) {
	t.Helper()
	code, body, raw := f.release(t, ar, data)
	if code != http.StatusOK || body["dry_run"] != true {
		t.Errorf("dry run %v: %d %s, want 200 with dry_run true", data, code, raw)
		return code, body, raw
	}
	confirmed := map[string]any{"confirm_memos": body["confirm_memos"], "as_of": body["as_of"]}
	for k, v := range data {
		confirmed[k] = v
	}
	return f.release(t, ar, confirmed)
}

// memo reads the block's regular backfill memo: whether there is one, its
// class, whether it parks for good, and its attempts.
func (f *relFixture) memo(t *testing.T, blockID string) (present bool, class string, infinity bool, attempts int) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT last_class, next_attempt_at = 'infinity', attempts FROM context_embed_failures
		  WHERE block_id = $1::uuid AND migration_id IS NULL`, blockID).Scan(&class, &infinity, &attempts)
	if err != nil {
		return false, "", false, 0
	}
	return true, class, infinity, attempts
}

func (f *relFixture) embedded(t *testing.T, blockID string) bool {
	t.Helper()
	var ok bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT embedding IS NOT NULL AND NOT embedding_stale FROM context_blocks WHERE id = $1::uuid`, blockID).Scan(&ok); err != nil {
		t.Fatalf("read %s: %v", blockID, err)
	}
	return ok
}

func relBlock(t *testing.T, pool *pgxpool.Pool, scope, title, content string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "learnings", title, content, nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

var relServerAdmin = &auth.AuthResult{IsValid: true, IsAdmin: true}

// TestEmbedBackfillReleaseAfterALimitRaise: a block parked by the pre-wire
// estimate stays parked when the limit is raised; after a release the next
// backfill embeds it.
func TestEmbedBackfillReleaseAfterALimitRaise(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newRelFixture(t)
	scopes := []string{"private"}
	id := relBlock(t, f.pool, "private", "rel236 long block", strings.Repeat("a long text ", 40))

	small, raised := relCfg(50), relCfg(1_000_000)
	if n := f.backfill(scopes, small); n != 0 {
		t.Fatalf("pass under the small limit embedded %d, want 0", n)
	}
	if ok, class, inf, _ := f.memo(t, id); !ok || class != "oversize" || !inf {
		t.Fatalf("after the pass under the small limit: memo=%v class=%q infinity=%v, want an oversize park", ok, class, inf)
	}
	// The limit alone does not bring the block back.
	if n := f.backfill(scopes, raised); n != 0 || f.calls.Load() != 0 {
		t.Fatalf("pass under the raised limit before a release: embedded %d, wire calls %d, want 0 and 0 (the block is parked)", n, f.calls.Load())
	}

	code, body, raw := f.releaseConfirmed(t, relServerAdmin, map[string]any{"block_id": id})
	if code != http.StatusOK || body["released"] != float64(1) || body["remaining"] != float64(0) {
		t.Errorf("confirmed release of the block: %d %s, want 200 released 1 remaining 0", code, raw)
	}
	if n := f.backfill(scopes, raised); n != 1 {
		ok, class, _, _ := f.memo(t, id)
		t.Fatalf("the backfill after the raise and the release embedded %d, want 1 (memo=%v class=%q)", n, ok, class)
	}
	if !f.embedded(t, id) {
		t.Errorf("block not embedded after the release")
	}
	if ok, _, _, _ := f.memo(t, id); ok {
		t.Errorf("memo left on the embedded block")
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("wire calls = %d, want 1 (one embed of the released block)", got)
	}
}

// TestEmbedBackfillReleaseOfABlockThatStillDoesNotFit: a block the backend
// refused as too long, released, is attempted exactly once more and parked
// again — the release is no loop.
func TestEmbedBackfillReleaseOfABlockThatStillDoesNotFit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newRelFixture(t)
	scopes := []string{"private"}
	cfg := relCfg(1_000_000)
	id := relBlock(t, f.pool, "private", "rel236 refused block", "text "+relOversizeMarker)

	if n := f.backfill(scopes, cfg); n != 0 || f.calls.Load() != 1 {
		t.Fatalf("first pass: embedded %d, wire calls %d, want 0 and 1", n, f.calls.Load())
	}
	if ok, class, inf, _ := f.memo(t, id); !ok || class != "oversize" || !inf {
		t.Fatalf("after the backend's refusal: memo=%v class=%q infinity=%v, want an oversize park", ok, class, inf)
	}
	code, body, raw := f.releaseConfirmed(t, relServerAdmin, map[string]any{"class": "oversize"})
	if code != http.StatusOK || body["released"] != float64(1) {
		t.Fatalf("confirmed release of the class: %d %s, want 200 released 1", code, raw)
	}
	for pass := 1; pass <= 4; pass++ {
		if n := f.backfill(scopes, cfg); n != 0 {
			t.Fatalf("pass %d after the release embedded %d, want 0", pass, n)
		}
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("wire calls = %d after the release and four passes, want 2 (one attempt, then parked again)", got)
	}
	if ok, class, inf, attempts := f.memo(t, id); !ok || class != "oversize" || !inf || attempts != 1 {
		t.Errorf("after the attempt: memo=%v class=%q infinity=%v attempts=%d, want the oversize park of one new attempt", ok, class, inf, attempts)
	}
}

// TestEmbedBackfillReleaseStaysInTheCallersRange: a tenant-admin releases the
// regular memos of its own tenant's blocks; another tenant's block answers as
// an unknown one, a migration's memo on the same block stays, and a key
// without an admin role is refused.
func TestEmbedBackfillReleaseStaysInTheCallersRange(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newRelFixture(t)
	ctx := context.Background()
	tenant := func(slug, scope string) string {
		tn, err := store.CreateTenant(ctx, f.pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		if _, err := store.AssignTenantScope(ctx, f.pool, tn.ID, scope, nil); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
		return tn.ID
	}
	const own, foreign = "rel-a:data", "rel-b:data"
	tA := tenant("rel-a", own)
	tenant("rel-b", foreign)
	long := strings.Repeat("a long text ", 40)
	ownID := relBlock(t, f.pool, own, "rel236 own", long)
	foreignID := relBlock(t, f.pool, foreign, "rel236 foreign", long)
	for range 2 {
		f.backfill([]string{own, foreign}, relCfg(50))
	}
	for _, id := range []string{ownID, foreignID} {
		if ok, class, _, _ := f.memo(t, id); !ok || class != "oversize" {
			t.Fatalf("fixture: block %s memo=%v class=%q, want parked", id, ok, class)
		}
	}
	// A re-embed migration's own memo on the tenant's block.
	if _, err := f.pool.Exec(ctx, `INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims)
		VALUES ('rel236-next', 'test', 1024, 1024) ON CONFLICT (model_key) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var migID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status, started_at)
		VALUES ('qwen3-embedding-8b', 'rel236-next', 'embed-a', 'running', now()) RETURNING id::text`).Scan(&migID); err != nil {
		t.Fatalf("migration row: %v", err)
	}
	if err := store.RecordEmbedFailureForMigration(ctx, f.pool, ownID, migID, store.EmbedFailureOversize,
		store.NormalizeEmbedError(store.EmbedFailureOversize, "migration park"), time.Minute, time.Hour); err != nil {
		t.Fatalf("migration memo: %v", err)
	}

	// The tenant reads the other tenant's scope through a tenant grant and
	// one of its blocks through a block grant: neither is the tenant's own.
	grantedID := relBlock(t, f.pool, foreign, "rel236 granted", long)
	f.backfill([]string{foreign}, relCfg(50))
	if _, err := store.CreateTenantGrant(ctx, f.pool, tA, foreign, ""); err != nil {
		t.Fatalf("tenant grant: %v", err)
	}
	if _, err := store.CreateBlockGrant(ctx, f.pool, grantedID, tA, ""); err != nil {
		t.Fatalf("block grant: %v", err)
	}
	tenantAdmin := &auth.AuthResult{IsValid: true, ApiKeyID: "rel-key", HomeScope: own, TenantID: tA, TenantRole: auth.RoleAdmin}
	member := &auth.AuthResult{IsValid: true, ApiKeyID: "rel-key", HomeScope: own, TenantID: tA, TenantRole: auth.RoleMember}

	if code, _, raw := f.release(t, member, map[string]any{"block_id": ownID}); code != http.StatusForbidden {
		t.Errorf("member key: %d %s, want 403", code, raw)
	}
	// One answer for both, but for the time of the count.
	c1, m1, b1 := f.release(t, tenantAdmin, map[string]any{"block_id": foreignID})
	c2, m2, b2 := f.release(t, tenantAdmin, map[string]any{"block_id": "00000000-0000-4000-8000-000000000236"})
	delete(m1, "as_of")
	delete(m2, "as_of")
	if c1 != http.StatusOK || c1 != c2 || !reflect.DeepEqual(m1, m2) || m1["counted"] != float64(0) {
		t.Errorf("another tenant's block = %d %s, an unknown block = %d %s — want one identical answer counting 0", c1, b1, c2, b2)
	}
	code, body, raw := f.release(t, tenantAdmin, map[string]any{"class": "oversize"})
	if code != http.StatusOK || body["counted"] != float64(1) {
		t.Fatalf("tenant-admin dry run of the class: %d %s, want counted 1 (its own block's regular memo, not the granted scope's or the granted block's)", code, raw)
	}
	code, body, raw = f.releaseConfirmed(t, tenantAdmin, map[string]any{"class": "oversize"})
	if code != http.StatusOK || body["released"] != float64(1) {
		t.Errorf("tenant-admin release of the class: %d %s, want released 1", code, raw)
	}
	if ok, _, _, _ := f.memo(t, ownID); ok {
		t.Errorf("the tenant's own regular memo is still there")
	}
	for _, id := range []string{foreignID, grantedID} {
		if ok, _, _, _ := f.memo(t, id); !ok {
			t.Errorf("another tenant's memo (block %s) was released by a tenant-admin", id)
		}
	}
	var migMemos int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id = $2::uuid`,
		ownID, migID).Scan(&migMemos); err != nil {
		t.Fatal(err)
	}
	if migMemos != 1 {
		t.Errorf("the migration's memo on the released block: %d rows, want 1 (a release takes regular memos only)", migMemos)
	}
	// The server-admin's range is every tenant.
	code, body, raw = f.releaseConfirmed(t, relServerAdmin, map[string]any{"class": "oversize"})
	if code != http.StatusOK || body["released"] != float64(2) {
		t.Errorf("server-admin release of the class: %d %s, want released 2 (the other tenant's)", code, raw)
	}
	if ok, _, _, _ := f.memo(t, foreignID); ok {
		t.Errorf("the server-admin's release left the other tenant's memo")
	}
}

// TestEmbedBackfillReleaseConfirmAndPages: the confirm binds to the count of
// the dry run, a page releases at most limit memos and answers what is left.
func TestEmbedBackfillReleaseConfirmAndPages(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newRelFixture(t)
	scopes := []string{"private"}
	long := strings.Repeat("a long text ", 40)
	ids := []string{
		relBlock(t, f.pool, "private", "rel236 page a", long),
		relBlock(t, f.pool, "private", "rel236 page b", long),
		relBlock(t, f.pool, "private", "rel236 page c", long),
	}
	for range ids {
		f.backfill(scopes, relCfg(50))
	}
	parked := func() int {
		n := 0
		for _, id := range ids {
			if ok, _, _, _ := f.memo(t, id); ok {
				n++
			}
		}
		return n
	}
	if parked() != 3 {
		t.Fatalf("fixture: %d parked, want 3", parked())
	}
	for _, bad := range []map[string]any{
		{},
		{"block_id": ids[0], "class": "oversize"},
		{"class": "sensitivity_ineligible"},
		{"class": "caller_timeout"},
		{"class": "oversize", "confirm_memos": 3},
		{"class": "oversize", "as_of": "2026-11-24T00:00:00Z"},
		{"class": "no-such-class"},
		{"block_id": "not-a-uuid"},
		{"class": "oversize", "confirm_memos": -1},
		{"class": "oversize", "limits": 1},
	} {
		if code, _, raw := f.release(t, relServerAdmin, bad); code != http.StatusBadRequest {
			t.Errorf("data %v: %d %s, want 400", bad, code, raw)
		}
	}
	// A limit above the page bound is clamped, not refused.
	code, dry, raw := f.release(t, relServerAdmin, map[string]any{"class": "oversize", "limit": 5000})
	if code != http.StatusOK || dry["counted"] != float64(3) || dry["as_of"] == nil {
		t.Fatalf("dry run with limit 5000: %d %s, want 200 counted 3 with as_of", code, raw)
	}
	asOf := dry["as_of"]
	if code, body, raw := f.release(t, relServerAdmin, map[string]any{"class": "oversize", "confirm_memos": 2, "as_of": asOf}); code != http.StatusConflict || body["counted"] != float64(3) {
		t.Errorf("confirm of a stale count: %d %s, want 409 with the fresh count 3", code, raw)
	}
	if parked() != 3 {
		t.Fatalf("a refused confirm released memos: %d parked, want 3", parked())
	}
	code, body, raw := f.release(t, relServerAdmin, map[string]any{"class": "oversize", "limit": 2, "confirm_memos": 3, "as_of": asOf})
	if code != http.StatusOK || body["released"] != float64(2) || body["remaining"] != float64(1) || body["confirm_memos"] != float64(1) {
		t.Fatalf("first page: %d %s, want released 2 remaining 1 confirm_memos 1", code, raw)
	}
	code, body, raw = f.release(t, relServerAdmin, map[string]any{"class": "oversize", "limit": 2, "confirm_memos": 1, "as_of": asOf})
	if code != http.StatusOK || body["released"] != float64(1) || body["remaining"] != float64(0) {
		t.Fatalf("second page: %d %s, want released 1 remaining 0", code, raw)
	}
	if parked() != 0 {
		t.Errorf("%d still parked after both pages, want 0", parked())
	}
}

// TestEmbedBackfillReleasePagesEndWhileBlocksKeepNotFitting: a released block
// that still does not fit is parked again with a new memo; the pages of the
// release are bound to its dry run (as_of), so they do not find it again and
// the release ends at remaining 0 after one attempt per block.
func TestEmbedBackfillReleasePagesEndWhileBlocksKeepNotFitting(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newRelFixture(t)
	scopes := []string{"private"}
	cfg := relCfg(1_000_000)
	ids := []string{
		relBlock(t, f.pool, "private", "rel236 refused a", "text a "+relOversizeMarker),
		relBlock(t, f.pool, "private", "rel236 refused b", "text b "+relOversizeMarker),
	}
	for range ids {
		f.backfill(scopes, cfg)
	}
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("fixture: %d wire calls, want 2 (both refused)", got)
	}
	code, dry, raw := f.release(t, relServerAdmin, map[string]any{"class": "oversize"})
	if code != http.StatusOK || dry["counted"] != float64(2) {
		t.Fatalf("dry run: %d %s, want counted 2", code, raw)
	}
	confirm := dry["confirm_memos"]
	for page := 1; page <= 4; page++ {
		code, body, raw := f.release(t, relServerAdmin, map[string]any{"class": "oversize", "limit": 1, "confirm_memos": confirm, "as_of": dry["as_of"]})
		if code != http.StatusOK {
			t.Fatalf("page %d: %d %s, want 200 — the re-parked blocks must not come back into the release", page, code, raw)
		}
		f.backfill(scopes, cfg) // the released block is attempted and parked again
		if body["remaining"] == float64(0) {
			if page != 2 {
				t.Errorf("the release ended after %d pages, want 2", page)
			}
			break
		}
		confirm = body["confirm_memos"]
	}
	if got := f.calls.Load(); got != 4 {
		t.Errorf("wire calls = %d, want 4 (one attempt per block after its release)", got)
	}
	for _, id := range ids {
		if ok, class, inf, _ := f.memo(t, id); !ok || class != "oversize" || !inf {
			t.Errorf("block %s after the release: memo=%v class=%q infinity=%v, want parked again", id, ok, class, inf)
		}
	}
}

// TestEmbedBackfillReleaseLeavesACallerTimeoutMemo: a caller_timeout memo is
// no park — the arm takes such a block at once, and with the arm off the
// query path retries it after its hold — so the release does not take it,
// by block or by class.
func TestEmbedBackfillReleaseLeavesACallerTimeoutMemo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newRelFixture(t)
	ctx := context.Background()
	title, content := "rel236 slow block", "a text the sync budget did not cover"
	id := relBlock(t, f.pool, "private", title, content)
	// The query path's memo of a cut embed (handler/query.go).
	if _, err := store.RecordEmbedFailureIfUnchanged(ctx, f.pool, id, title, content, store.EmbedFailureCallerTimeout,
		store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "sync budget"), time.Minute, time.Hour); err != nil {
		t.Fatalf("caller_timeout memo: %v", err)
	}
	if ok, class, _, _ := f.memo(t, id); !ok || class != "caller_timeout" {
		t.Fatalf("fixture: memo=%v class=%q", ok, class)
	}
	if code, body, raw := f.release(t, relServerAdmin, map[string]any{"block_id": id}); code != http.StatusOK || body["counted"] != float64(0) {
		t.Errorf("release of the block: %d %s, want counted 0", code, raw)
	}
	if code, _, raw := f.release(t, relServerAdmin, map[string]any{"class": "caller_timeout"}); code != http.StatusBadRequest {
		t.Errorf("release of the class caller_timeout: %d %s, want 400", code, raw)
	}
	if ok, _, _, _ := f.memo(t, id); !ok {
		t.Errorf("the caller_timeout memo is gone")
	}
}
