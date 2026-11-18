//go:build integration

// Issue #341: manage guard-stats reports the blocks the guard still has to
// check, counted on demand within the caller's read scopes, with dirty_since
// the oldest updated_at among them. Every fixture is written through the
// production write paths (store.UpsertBlock, store.StoreEmbedding,
// store.UpdateBlock, store.DeleteBlock), every read goes through
// auth.AuthenticateRaw + ManageHandler.HandleManage.
package handler

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type guardStatsFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	reg  *blocktype.Registry
	h    *ManageHandler
}

func newGuardStatsFixture(t *testing.T) *guardStatsFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	return &guardStatsFixture{t: t, ctx: ctx, pool: pool, reg: reg,
		h: NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)}
}

func guardStatsVec() []float32 {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = rand.Float32() - 0.5
	}
	return v
}

// tenantKey creates a tenant owning scope and returns its key's raw secret.
func (f *guardStatsFixture) tenantKey(scope string) (tenantID, raw string) {
	f.t.Helper()
	tn, err := store.CreateTenant(f.ctx, f.pool, scope, scope)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := store.AssignTenantScope(f.ctx, f.pool, tn.ID, scope, nil); err != nil {
		f.t.Fatal(err)
	}
	_, raw, err = store.CreateApiKey(f.ctx, f.pool, "k-"+scope, scope, nil, tn.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return tn.ID, raw
}

// adminKey creates a server-admin key (is_admin is written by no code path,
// so the fixture sets it as an operator would).
func (f *guardStatsFixture) adminKey() string {
	f.t.Helper()
	k, raw, err := store.CreateApiKey(f.ctx, f.pool, "k-server-admin", "private", nil, "")
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, k.ID); err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *guardStatsFixture) auth(raw string) *auth.AuthResult {
	f.t.Helper()
	ar, err := auth.AuthenticateRaw(f.ctx, f.pool, raw)
	if err != nil || !ar.IsValid {
		f.t.Fatalf("auth: %v %+v", err, ar)
	}
	return ar
}

type guardStatsView struct {
	pending int
	dirty   *time.Time
}

func (f *guardStatsFixture) stats(ar *auth.AuthResult) guardStatsView {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(`{"action":"guard-stats"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	f.h.HandleManage(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("guard-stats: %d %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		f.t.Fatal(err)
	}
	for _, k := range []string{"pending_count", "dirty_since", "last_guard_at", "total_blocks", "unchecked", "write_log_entries"} {
		if _, ok := m[k]; !ok {
			f.t.Fatalf("guard-stats lost wire field %q: %v", k, m)
		}
	}
	v := guardStatsView{pending: int(m["pending_count"].(float64))}
	if s, ok := m["dirty_since"].(string); ok {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			f.t.Fatal(err)
		}
		v.dirty = &ts
	}
	return v
}

// truth is what the guard pick would take in the given scopes, read with the
// pick's own conditions (idx_guard_pending conjuncts + the type allowlist).
func (f *guardStatsFixture) truth(scopes []string) guardStatsView {
	f.t.Helper()
	var v guardStatsView
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*)::int, min(updated_at) FROM context_blocks
		WHERE scope = ANY($1::text[]) AND NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL AND NOT embedding_stale
		  AND category != 'index' AND lifecycle_state = 'knowledge' AND type_name = ANY($2::text[])`,
		scopes, f.reg.Snapshot().GuardCheckTypes()).Scan(&v.pending, &v.dirty); err != nil {
		f.t.Fatal(err)
	}
	return v
}

// pendingBlock writes a block and its vector: the block is then guard-pending.
func (f *guardStatsFixture) pendingBlock(scope, title, typeName string) *store.Block {
	f.t.Helper()
	b, err := store.UpsertBlock(f.ctx, f.pool, "q341", title, "content of "+title, nil, nil, scope, true, store.SensitivityWrite{}, typeName)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := store.StoreEmbedding(f.ctx, f.pool, b.ID, "q341", guardStatsVec()); err != nil {
		f.t.Fatal(err)
	}
	return b
}

func sameStamp(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}

func (f *guardStatsFixture) expect(label string, ar *auth.AuthResult, want guardStatsView) {
	f.t.Helper()
	got := f.stats(ar)
	if got.pending != want.pending || !sameStamp(got.dirty, want.dirty) {
		f.t.Errorf("%s: guard-stats pending_count=%d dirty_since=%v, want %d / %v", label, got.pending, got.dirty, want.pending, want.dirty)
	}
}

// Pflichttest 5: the key of tenant B learns nothing of tenant A's queue; a
// key with a read grant on A's scope sees that scope; a server-admin sees
// what its read scopes cover.
func TestGuardStatsPendingWithinCallerScopes(t *testing.T) {
	f := newGuardStatsFixture(t)
	_, rawA := f.tenantKey("q341-a")
	_, rawB := f.tenantKey("q341-b")
	tenantC, rawC := f.tenantKey("q341-c")
	if _, err := store.CreateTenantGrant(f.ctx, f.pool, tenantC, "q341-a", ""); err != nil {
		t.Fatal(err)
	}
	rawAdmin := f.adminKey()

	for i := 0; i < 3; i++ {
		f.pendingBlock("q341-a", "a block "+string(rune('a'+i)), "")
		time.Sleep(2 * time.Millisecond)
	}
	f.pendingBlock("q341-c", "c block", "")

	arA, arB, arC, arAdmin := f.auth(rawA), f.auth(rawB), f.auth(rawC), f.auth(rawAdmin)
	if !slices.Contains(arC.ReadScopes, "q341-a") {
		t.Fatalf("grant fixture: C's read scopes %v lack q341-a", arC.ReadScopes)
	}
	f.expect("tenant B", arB, guardStatsView{})
	f.expect("tenant A", arA, f.truth([]string{"q341-a"}))
	if got := f.stats(arA); got.pending != 3 {
		t.Errorf("tenant A: pending_count=%d, want its 3 blocks", got.pending)
	}
	f.expect("tenant C with a read grant on q341-a", arC, f.truth([]string{"q341-a", "q341-c"}))
	if got := f.stats(arC); got.pending != 4 {
		t.Errorf("tenant C: pending_count=%d, want 3 granted + 1 own", got.pending)
	}
	f.expect("server-admin", arAdmin, f.truth(arAdmin.ReadScopes))
	t.Logf("server-admin read scopes: %v", arAdmin.ReadScopes)
}

// Pflichttest 6: the count is the pick's set — type allowlist, archive state,
// scope — and follows a block that changes type, scope or archive state.
func TestGuardStatsPendingFollowsTypeScopeAndArchive(t *testing.T) {
	f := newGuardStatsFixture(t)
	_, raw := f.tenantKey("q341-t")
	ar := f.auth(raw)
	scopes := []string{"q341-t"}
	set := f.reg.Snapshot()
	var unchecked string
	for _, name := range []string{"comment", "tool-evidence", "checkpoint", "insight"} {
		if !slices.Contains(set.GuardCheckTypes(), name) {
			unchecked = name
			break
		}
	}
	if unchecked == "" {
		t.Fatalf("no builtin type with guard.check=false among the candidates: %v", set.GuardCheckTypes())
	}

	k := f.pendingBlock("q341-t", "knowledge one", "")
	f.expect("one pending knowledge block", ar, f.truth(scopes))
	if got := f.stats(ar); got.pending != 1 {
		t.Errorf("pending_count=%d, want 1", got.pending)
	}

	f.pendingBlock("q341-t", "unchecked type", unchecked)
	if got := f.stats(ar); got.pending != 1 {
		t.Errorf("a %s block (guard.check=false) is counted: pending_count=%d, want 1", unchecked, got.pending)
	}

	if _, err := store.DeleteBlock(f.ctx, f.pool, k.ID, scopes); err != nil {
		t.Fatal(err)
	}
	f.expect("after archiving the pending block", ar, guardStatsView{})

	k2 := f.pendingBlock("q341-t", "knowledge two", "")
	f.expect("a second pending block", ar, f.truth(scopes))
	retype := unchecked
	if _, _, err := store.UpdateBlock(f.ctx, f.pool, k2.ID, store.UpdateBlockData{Type: &retype}, scopes); err != nil {
		t.Fatalf("retype: %v", err)
	}
	f.expect("after the type change to "+unchecked, ar, f.truth(scopes))
	if got := f.stats(ar); got.pending != 0 {
		t.Errorf("after retype: pending_count=%d, want 0", got.pending)
	}

	k3 := f.pendingBlock("q341-t", "knowledge three", "")
	f.expect("a third pending block", ar, f.truth(scopes))
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO context_tenant_scopes (tenant_id, scope) SELECT tenant_id, 'q341-t2' FROM context_tenant_scopes WHERE scope = 'q341-t'`); err != nil {
		t.Fatalf("second scope: %v", err)
	}
	moved := "q341-t2"
	if _, _, err := store.UpdateBlock(f.ctx, f.pool, k3.ID, store.UpdateBlockData{Scope: &moved}, []string{"q341-t", "q341-t2"}); err != nil {
		t.Fatalf("scope move: %v", err)
	}
	f.expect("after moving the block out of the key's read scopes", ar, f.truth(scopes))
	if got := f.stats(ar); got.pending != 0 {
		t.Errorf("after scope move: pending_count=%d, want 0", got.pending)
	}
}

// dirty_since is the oldest updated_at among the waiting blocks, not the
// oldest created_at: an old block whose text changed waits since its change.
func TestGuardStatsDirtySinceIsTheOldestChange(t *testing.T) {
	f := newGuardStatsFixture(t)
	_, raw := f.tenantKey("q341-d")
	ar := f.auth(raw)
	scopes := []string{"q341-d"}
	old := f.pendingBlock("q341-d", "created first", "")
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, f.reg.Snapshot(), 100); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	young := f.pendingBlock("q341-d", "created second", "")
	time.Sleep(5 * time.Millisecond)
	edited := "content of created first, edited"
	if _, _, err := store.UpdateBlock(f.ctx, f.pool, old.ID, store.UpdateBlockData{Content: &edited}, scopes); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(f.ctx, f.pool, old.ID, "q341", guardStatsVec()); err != nil {
		t.Fatal(err)
	}
	var youngUpdated, oldCreated time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT (SELECT updated_at FROM context_blocks WHERE id = $1::uuid),
		(SELECT created_at FROM context_blocks WHERE id = $2::uuid)`, young.ID, old.ID).Scan(&youngUpdated, &oldCreated); err != nil {
		t.Fatal(err)
	}
	if !oldCreated.Before(youngUpdated) {
		t.Fatalf("fixture: the first block's created_at %v is not before the second's updated_at %v", oldCreated, youngUpdated)
	}
	f.expect("two waiting blocks, the older one edited", ar, guardStatsView{pending: 2, dirty: &youngUpdated})
}

// Pflichttest 7: for a server-admin on a mixed store, the count equals the
// number of blocks the guard actually processes until its pick runs empty.
func TestGuardStatsPendingEqualsWhatTheGuardProcesses(t *testing.T) {
	f := newGuardStatsFixture(t)
	ar := f.auth(f.adminKey())
	set := f.reg.Snapshot()
	scope := "private"

	// checked before the count: one guard run takes them out.
	for i := 0; i < 3; i++ {
		f.pendingBlock(scope, "checked "+string(rune('a'+i)), "")
	}
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100); err != nil {
		t.Fatal(err)
	}
	// pending
	for i := 0; i < 5; i++ {
		f.pendingBlock(scope, "pending "+string(rune('a'+i)), "")
	}
	// not pending: no vector, archived, category index, a type outside the
	// allowlist, a stale vector after a content edit.
	if _, err := store.UpsertBlock(f.ctx, f.pool, "q341", "no vector", "content without vector", nil, nil, scope, true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatal(err)
	}
	arch := f.pendingBlock(scope, "archived", "")
	if _, err := store.DeleteBlock(f.ctx, f.pool, arch.ID, []string{scope}); err != nil {
		t.Fatal(err)
	}
	idx, err := store.UpsertBlock(f.ctx, f.pool, "index", "index block", "content of the index block", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(f.ctx, f.pool, idx.ID, "q341", guardStatsVec()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"comment", "tool-evidence", "checkpoint", "insight"} {
		if !slices.Contains(set.GuardCheckTypes(), name) {
			f.pendingBlock(scope, "unchecked type "+name, name)
			break
		}
	}
	stale := f.pendingBlock(scope, "stale", "")
	edited := "content of stale, edited"
	if _, _, err := store.UpdateBlock(f.ctx, f.pool, stale.ID, store.UpdateBlockData{Content: &edited}, []string{scope}); err != nil {
		t.Fatal(err)
	}

	before := f.stats(ar)
	processed := 0
	for i := 0; i < 20; i++ {
		n, err := guard.RunGuardBatch(f.ctx, f.pool, set, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		processed += n
	}
	if before.pending != processed {
		t.Errorf("guard-stats pending_count=%d before the guard ran, the guard then processed %d blocks", before.pending, processed)
	}
	if processed != 5 {
		t.Errorf("fixture: the guard processed %d blocks, want the 5 pending ones", processed)
	}
	f.expect("after the guard ran empty", ar, guardStatsView{})
}

// Pflichttest 4 (D1): a guard run that finds nothing while the write that
// makes a block pending is in flight. Afterwards guard-stats shows the block.
func TestGuardStatsPendingAfterEmptyRunRacesAWrite(t *testing.T) {
	f := newGuardStatsFixture(t)
	ar := f.auth(f.adminKey())
	set := f.reg.Snapshot()
	x, err := store.UpsertBlock(f.ctx, f.pool, "q341", "x one", "content of x one", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if err := store.StoreEmbedding(f.ctx, tx, x.ID, "q341", guardStatsVec()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100)
		done <- err
	}()
	// Let the run reach whatever it waits for (today: the state row) or end.
	waitForGuardWaitOrDone(t, f.pool, 2*time.Second)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := f.truth(ar.ReadScopes)
	if want.pending != 1 {
		t.Fatalf("fixture: %d pending blocks, want 1", want.pending)
	}
	f.expect("D1 after the race", ar, want)
}

// Pflichttest 4 (D2): the run re-checks a block whose verdict does not count
// and recounts while another block's vector write is in flight.
func TestGuardStatsPendingAfterRecountRacesAWrite(t *testing.T) {
	f := newGuardStatsFixture(t)
	ar := f.auth(f.adminKey())
	set := f.reg.Snapshot()
	x2 := f.pendingBlock("private", "x two", "")
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100); err != nil {
		t.Fatal(err)
	}
	edited := "content of x two, edited"
	if _, _, err := store.UpdateBlock(f.ctx, f.pool, x2.ID, store.UpdateBlockData{Content: &edited}, []string{"private"}); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(f.ctx, f.pool, x2.ID, "q341", guardStatsVec()); err != nil {
		t.Fatal(err)
	}
	y2, err := store.UpsertBlock(f.ctx, f.pool, "q341", "y two", "content of y two", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if err := store.StoreEmbedding(f.ctx, tx, y2.ID, "q341", guardStatsVec()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		n, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100)
		if err == nil && n != 1 {
			t.Errorf("fixture: the run processed %d blocks, want x2", n)
		}
		done <- err
	}()
	waitForGuardWaitOrDone(t, f.pool, 2*time.Second)
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := f.truth(ar.ReadScopes)
	if want.pending != 1 {
		t.Fatalf("fixture: %d pending blocks, want 1 (y2)", want.pending)
	}
	f.expect("D2 after the race", ar, want)
}

// waitForGuardWaitOrDone gives the guard run up to d to either wait for a
// lock (the interleaving of phase 1) or finish (no wait to reach any more).
func waitForGuardWaitOrDone(t *testing.T, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			t.Logf("guard run waits for a lock")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("no lock wait within %v (the run did not wait)", d)
}
