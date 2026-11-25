//go:build integration

// Issue #547 point 7a: a guard verdict the run defers (it would have had to
// wait for another transaction) left only an INFO line per block. guard-stats
// carries the count: deferred_verdicts {since_start, last_run} of this
// process. Here the run defers a block whose matched neighbour another
// transaction holds FOR UPDATE (a purge or a prune would): two blocks, the
// count moves by two (blocks, not runs), and the next quiet run decides them.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

func gdVec(i int, a float32, j int, b float32) []float32 {
	v := make([]float32, 1024)
	v[i], v[j] = a, b
	return v
}

func (f *guardStatsFixture) deferred(t *testing.T, raw string) (sinceStart, lastRun float64) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(`{"action":"guard-stats"}`))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, f.auth(raw)))
	rec := httptest.NewRecorder()
	f.h.HandleManage(rec, req)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("guard-stats: %d %s", rec.Code, rec.Body.String())
	}
	dv, ok := m["deferred_verdicts"].(map[string]any)
	if !ok {
		t.Fatalf("guard-stats without deferred_verdicts: %v", m)
	}
	return dv["since_start"].(float64), dv["last_run"].(float64)
}

func TestGuardStatsCountsDeferredVerdicts(t *testing.T) {
	f := newGuardStatsFixture(t)
	raw := f.adminKey()
	set := f.reg.Snapshot()
	write := func(title string, vec []float32) string {
		b, err := store.UpsertBlock(f.ctx, f.pool, "q547", title, title+" content", nil, nil, "private", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(f.ctx, f.pool, b.ID, "q547", vec); err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	m := write("q547 neighbour", gdVec(5, 1, 6, 0))
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100, config.Defaults().GuardSearch()); err != nil {
		t.Fatal(err)
	}
	// Two blocks to review against the neighbour, so a count of runs and a
	// count of blocks differ (review round 1 of #547).
	write("q547 review", gdVec(5, 0.96, 6, 0.28)) // needs_review against the neighbour
	write("q547 review two", gdVec(5, 0.96, 7, 0.28)) // nearer to the neighbour (0.96) than to the first (0.92)
	before, _ := f.deferred(t, raw)

	holder, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(f.ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100, config.Defaults().GuardSearch()); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	since, last := f.deferred(t, raw)
	if since != before+2 || last != 2 {
		t.Errorf("deferred_verdicts after a run beside a held neighbour = {since_start %v, last_run %v}, want {%v, 2}", since, last, before+2)
	}
	n, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100, config.Defaults().GuardSearch())
	if err != nil || n != 2 {
		t.Errorf("the next quiet run processed %d (%v), want the two deferred blocks", n, err)
	}
	if since2, last2 := f.deferred(t, raw); since2 != since || last2 != 0 {
		t.Errorf("deferred_verdicts after the quiet run = {%v, %v}, want {%v, 0}", since2, last2, since)
	}
}

// TestGuardRunDecidesNewerBlocksPastDeferredOnes (issue #547, review round 1):
// a run whose whole pick is deferred — the neighbour of both picked blocks
// held FOR UPDATE, as a purge or prune batch holds it — takes as many blocks
// again past its own and decides them. Before, the deferred blocks stood at
// the front of every pick while the hold lasted and no newer block got a
// verdict.
func TestGuardRunDecidesNewerBlocksPastDeferredOnes(t *testing.T) {
	f := newGuardStatsFixture(t)
	set := f.reg.Snapshot()
	write := func(title string, vec []float32) string {
		b, err := store.UpsertBlock(f.ctx, f.pool, "q547", title, title+" content", nil, nil, "private", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(f.ctx, f.pool, b.ID, "q547", vec); err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	checked := func(id string) bool {
		var c bool
		if err := f.pool.QueryRow(f.ctx, `SELECT metadata ? 'guard_checked_at' FROM context_blocks WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// m: the held neighbour of the two older blocks; a: the neighbour of the
	// two newer ones, not held.
	m := write("q547p neighbour", gdVec(5, 1, 6, 0))
	write("q547p other neighbour", gdVec(100, 1, 101, 0))
	if _, err := guard.RunGuardBatch(f.ctx, f.pool, set, 100, config.Defaults().GuardSearch()); err != nil {
		t.Fatal(err)
	}
	old1 := write("q547p old one", gdVec(5, 0.96, 6, 0.28))
	old2 := write("q547p old two", gdVec(5, 0.96, 7, 0.28))
	new1 := write("q547p new one", gdVec(100, 0.96, 101, 0.28))
	new2 := write("q547p new two", gdVec(100, 0.96, 102, 0.28))

	holder, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(f.ctx) }()
	if _, err := holder.Exec(f.ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, m); err != nil {
		t.Fatal(err)
	}
	n, err := guard.RunGuardBatch(f.ctx, f.pool, set, 2, config.Defaults().GuardSearch())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || !checked(new1) || !checked(new2) || checked(old1) || checked(old2) {
		t.Errorf("a run of 2 beside the held neighbour processed %d: new %v %v, old %v %v — want the two newer decided, the two older deferred",
			n, checked(new1), checked(new2), checked(old1), checked(old2))
	}
}
