//go:build integration

// Issue #198, part B, the background arm's side: a title or content change
// keeps the previous vector (marked stale), the arm's pick takes the stale
// block like an unembedded one, and its write of the new vector ends the
// window.
//
// The arm holds the row lock of the block it embeds from the pick to the
// commit, so a content change that arrives during the embed call waits for
// that commit and then marks the vector the arm just wrote as stale: the arm
// wrote it for the text it read, which the block no longer holds. The test
// runs exactly that interleaving with a second connection, and then a second
// cycle that embeds the current text.
//
//	go test -tags=integration ./internal/events/ -run TestBackfillStale -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// staleEmbedServer answers the Ollama /api/embed shape and runs onEmbed with
// the embed input before it answers; it records the inputs.
func staleEmbedServer(t *testing.T, mu *sync.Mutex, inputs *[]string, onEmbed *func(string)) *httptest.Server {
	t.Helper()
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		*inputs = append(*inputs, req.Input)
		hook := *onEmbed
		mu.Unlock()
		if hook != nil {
			hook(req.Input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}, "prompt_eval_count": 3})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitForLockWaiter waits until a statement on another connection waits for a
// row lock, the state of an update that arrived while the arm holds the row.
func waitForLockWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Errorf("lock probe: %v", err)
			return
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("no statement waited for the row lock within 10 s")
}

func TestBackfillStale_ArmReembedsAChangedBlock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "shared"
	strptr := func(s string) *string { return &s }
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	var (
		mu      sync.Mutex
		inputs  []string
		onEmbed func(string)
	)
	setHook := func(f func(string)) { mu.Lock(); onEmbed = f; mu.Unlock() }
	take := func() []string { mu.Lock(); defer mu.Unlock(); in := inputs; inputs = nil; return in }
	srv := staleEmbedServer(t, &mu, &inputs, &onEmbed)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	router := backfillRouter(bpool, d)
	cfg := headOfLineCfg()

	b, err := store.UpsertBlock(ctx, pool, "learnings", "stale198 arm", "first text", nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer clearBlocks(t, pool)
	hasVector := func(t *testing.T) bool {
		t.Helper()
		var has bool
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&has); err != nil {
			t.Fatal(err)
		}
		return has
	}

	// Cycle 1 embeds the first text; a content change arrives while the arm
	// holds the row and commits after it.
	var wg sync.WaitGroup
	setHook(func(string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.UpdateBlock(context.Background(), pool, b.ID,
				store.UpdateBlockData{Content: strptr("second text")}, []string{scope}); err != nil {
				t.Errorf("update during the embed: %v", err)
			}
		}()
		waitForLockWaiter(t, pool)
	})
	ok, err := s.backfillOneEmbedding(ctx, router, cfg, nil)
	wg.Wait()
	if err != nil || !ok {
		t.Fatalf("cycle 1: backfilled=%v err=%v, want true/nil", ok, err)
	}
	if in := take(); len(in) != 1 || !strings.Contains(in[0], "first text") {
		t.Fatalf("cycle 1 embedded %q, want the first text", in)
	}
	if !hasVector(t) {
		t.Fatalf("after the change that waited for the arm: the block has no vector, want the one the arm wrote kept until the next embed")
	}

	// Cycle 2 takes the block again and embeds the text it holds now.
	setHook(nil)
	ok, err = s.backfillOneEmbedding(ctx, router, cfg, nil)
	if err != nil || !ok {
		t.Fatalf("cycle 2: backfilled=%v err=%v, want true/nil (the changed block is pending)", ok, err)
	}
	if in := take(); len(in) != 1 || !strings.Contains(in[0], "second text") {
		t.Fatalf("cycle 2 embedded %q, want the second text", in)
	}
	var stale bool
	if err := pool.QueryRow(ctx, `SELECT embedding_stale FROM context_blocks WHERE id = $1::uuid`, b.ID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale || !hasVector(t) {
		t.Errorf("after cycle 2: stale=%v has_vector=%v, want a fresh vector", stale, hasVector(t))
	}

	// Cycle 3: nothing is left.
	ok, err = s.backfillOneEmbedding(ctx, router, cfg, nil)
	if err != nil || ok {
		t.Errorf("cycle 3: backfilled=%v err=%v, want false/nil", ok, err)
	}
}
