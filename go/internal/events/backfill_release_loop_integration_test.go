//go:build integration

// Issue #236, review of the release (F1): a tenant-admin that releases its
// own oversize memos in a loop, whose blocks keep not fitting, must not get
// the arm's turns for nothing. Before the bound every release handed each of
// its blocks back to the pick, the arm parked it again in the tenant's next
// turn, and the next release found it again: with two tenants the looping
// one held every second turn of the arm for good (a pick each, and a wire
// call each for a block the backend refuses), while its way back without the
// action — a content change per block — costs a write each.
//
// Measured at the exact measure, the arm's own turns (backfillArmTurn over
// the real pace), not the wall clock: tenant T has blocks the pre-wire
// estimate parks and blocks the backend refuses; tenant U has a backlog. T's
// tenant-admin releases everything it can before every turn. The first round
// gives each of T's blocks its one attempt; after it, T takes no turn and
// sends no wire call, and U gets the turns.
//
//	go test -tags=integration ./internal/events/ -run TestBackfillReleaseLoopOfATenantAdmin -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillReleaseLoopOfATenantAdminTakesNoTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The backend counts the calls of each tenant and refuses T's marked
	// blocks as too long.
	var wireT, wireU atomic.Int64
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		in := fmt.Sprint(req.Input)
		if strings.Contains(in, "LOOPT") {
			wireT.Add(1)
		} else {
			wireU.Add(1)
		}
		if strings.Contains(in, oversizeMarker) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"exceed_context_size_error","message":"input exceeds the configured context size"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}, "prompt_eval_count": 3})
	}))
	t.Cleanup(srv.Close)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-loop", srv.URL, 100)})
	const iv = 15 * time.Second
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		Interval: iv, MaxTokens: 50, BackoffBase: time.Minute, BackoffCap: 24 * time.Hour,
	}}
	s := NewScheduler(pool, config.NewStore(cfg), bpool, StartupConfig{})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s.SetDispatcher(d)

	tenant := func(slug, scope string) string {
		tn, err := store.CreateTenant(ctx, pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
		return tn.ID
	}
	const scopeT, scopeU = "loop-t:data", "loop-u:data"
	tT := tenant("loop-t", scopeT)
	tenant("loop-u", scopeU)
	const estimateParked, refused, backlog = 20, 20, 400
	write := func(scope, title, content string) {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, content, nil, nil, scope, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("write %s: %v", title, err)
		}
	}
	// The refused blocks are the older ones, so the pick reaches them first.
	for i := range refused {
		write(scopeT, fmt.Sprintf("LOOPT refused %02d", i), "LOOPT "+oversizeMarker)
	}
	for i := range estimateParked {
		write(scopeT, fmt.Sprintf("LOOPT long %02d", i), strings.Repeat("a long text LOOPT ", 30))
	}
	// T's blocks are parked by the arm before U has a backlog.
	for {
		out, err := s.backfillStep(ctx, s.newRouter(cfg, scopeT), cfg, []string{scopeT})
		if err != nil {
			t.Fatalf("park T: %v", err)
		}
		if out == backfillIdle {
			break
		}
	}
	for i := range backlog {
		write(scopeU, fmt.Sprintf("LOOPU %03d", i), fmt.Sprintf("short text %d", i))
	}

	// Every memo write of T's blocks is one pick of T that took a block.
	for _, q := range []string{
		`CREATE TABLE loop_picks (n bigint NOT NULL)`, `INSERT INTO loop_picks VALUES (0)`,
		`CREATE FUNCTION loop_count() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF EXISTS (SELECT 1 FROM context_blocks WHERE id = NEW.block_id AND scope = 'loop-t:data') THEN
		     UPDATE loop_picks SET n = n + 1;
		   END IF;
		   RETURN NEW;
		 END $$`,
		`CREATE TRIGGER loop_count AFTER INSERT OR UPDATE OF attempts ON context_embed_failures FOR EACH ROW EXECUTE FUNCTION loop_count()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("instrument: %v", err)
		}
	}
	count := func(q string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	picksT := func() int64 { return count(`SELECT n FROM loop_picks`) }
	embeddedU := func() int64 {
		return count(`SELECT count(*) FROM context_blocks WHERE scope = 'loop-u:data' AND embedding IS NOT NULL`)
	}

	// T's tenant-admin: a dry run and every page it allows, before each turn.
	releaseT := func() int64 {
		dry, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{Class: store.EmbedFailureOversize, TenantID: tT, Hold: time.Hour, HoldCap: 24 * time.Hour})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		var released int64
		confirm := dry.Counted
		for confirm > 0 {
			res, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{Class: store.EmbedFailureOversize, TenantID: tT, Confirm: &confirm, AsOf: &dry.AsOf, Hold: time.Hour, HoldCap: 24 * time.Hour})
			if err != nil {
				t.Fatalf("release: %v", err)
			}
			released += res.Released
			if res.Released == 0 {
				break
			}
			confirm = res.Remaining
		}
		return released
	}
	pace := newBackfillArmPace()
	turns := func(n int) (released int64) {
		for range n {
			released += releaseT()
			if sleep := s.backfillArmTurn(ctx, pace, iv); sleep < 0 {
				t.Fatalf("turn: sleep %v", sleep)
			}
		}
		return released
	}

	// The first round: each of T's blocks gets its attempt after its release.
	p0, w0 := picksT(), wireT.Load()
	released := turns(3 * (estimateParked + refused))
	t.Logf("first round: %d released, %d picks of T, %d wire calls of T, U embedded %d", released, picksT()-p0, wireT.Load()-w0, embeddedU())
	if got := picksT() - p0; got < estimateParked+refused {
		t.Fatalf("first round: %d picks of T, want each of its %d blocks attempted once after the release", got, estimateParked+refused)
	}

	// The measure: the loop goes on.
	const window = 200
	p1, w1, u1 := picksT(), wireT.Load(), embeddedU()
	released = turns(window)
	tPicks, tWire, uEmb := picksT()-p1, wireT.Load()-w1, embeddedU()-u1
	t.Logf("%d turns of the loop: %d released, %d picks of T, %d wire calls of T, %d blocks of U embedded", window, released, tPicks, tWire, uEmb)
	if tPicks != 0 || tWire != 0 {
		t.Errorf("a tenant-admin's release loop took %d of %d turns and sent %d wire calls after its blocks had their attempt — want 0 and 0", tPicks, window, tWire)
	}
	if uEmb < window*9/10 {
		t.Errorf("U embedded %d blocks in %d turns, want at least %d (the turns T no longer takes)", uEmb, window, window*9/10)
	}
}
