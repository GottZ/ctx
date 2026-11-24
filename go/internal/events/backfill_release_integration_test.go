//go:build integration

// Issue #236, point one, on the background arm: a block the arm parked
// (oversize, next_attempt_at = 'infinity') is taken again by the arm's next
// pick once its memo is released (store.ReleaseBackfillMemos, the store half
// of `manage embed-backfill-release`) — embedded when it fits now, parked
// again after one attempt when it does not. And the release runs against a
// working arm without a lock wait cycle: the arm takes a block's row and then
// its memo row; the release takes memo rows only, skipping held ones.
//
//	go test -tags=integration ./internal/events/ -run 'TestBackfillRelease' -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// releaseEmbedServer answers the Ollama /api/embed shape after delay and
// counts the calls: the oversize refusal for an input with oversizeMarker, a
// 500 while down is set, a vector otherwise.
func releaseEmbedServer(t *testing.T, delay time.Duration, down *atomic.Bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(delay)
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Input, oversizeMarker):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"exceed_context_size_error","message":"input exceeds the configured context size"}}`))
		case down != nil && down.Load():
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"backend down"}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}, "prompt_eval_count": 3})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func releaseRouter(t *testing.T, url string) *dream.Router {
	t.Helper()
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", url, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return backfillRouter(bpool, d)
}

func releaseCfg(maxTokens int, base, capDur time.Duration) *config.Config {
	return &config.Config{EmbedBackfill: config.EmbedBackfillConfig{MaxTokens: maxTokens, BackoffBase: base, BackoffCap: capDur}}
}

func releaseBlock(t *testing.T, pool *pgxpool.Pool, title, content string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "learnings", title, content, nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

// releaseAll runs a confirmed release of r as a server-admin: the dry run's
// count, then the release bound to it.
func releaseAll(t *testing.T, pool *pgxpool.Pool, r store.BackfillRelease) store.BackfillReleaseResult {
	t.Helper()
	r.AllTenants = true
	dry, err := store.ReleaseBackfillMemos(context.Background(), pool, r)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	r.Confirm, r.AsOf = &dry.Counted, &dry.AsOf
	res, err := store.ReleaseBackfillMemos(context.Background(), pool, r)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	return res
}

func releaseMemo(t *testing.T, pool *pgxpool.Pool, id string) (class string, infinity bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT last_class, next_attempt_at = 'infinity' FROM context_embed_failures WHERE block_id = $1::uuid AND migration_id IS NULL`,
		id).Scan(&class, &infinity); err != nil {
		return "", false
	}
	return class, infinity
}

func TestBackfillReleaseArmTakesTheBlockAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	srv, calls := releaseEmbedServer(t, 0, nil)
	router := releaseRouter(t, srv.URL)
	small, raised := releaseCfg(50, time.Minute, time.Hour), releaseCfg(1_000_000, time.Minute, time.Hour)

	t.Run("a block parked by the estimate is embedded after a raise and a release", func(t *testing.T) {
		id := releaseBlock(t, pool, "rel236 arm long", strings.Repeat("a long text ", 40))
		if out, err := s.backfillStep(ctx, router, small, nil); err != nil || out != backfillParked {
			t.Fatalf("pick under the small limit: %v %v, want parked", out, err)
		}
		if out, err := s.backfillStep(ctx, router, raised, nil); err != nil || out != backfillIdle {
			t.Fatalf("pick under the raised limit before the release: %v %v, want idle (the block stays parked)", out, err)
		}
		if res := releaseAll(t, pool, store.BackfillRelease{BlockID: id}); res.Released != 1 {
			t.Fatalf("release: %+v, want released 1", res)
		}
		if out, err := s.backfillStep(ctx, router, raised, nil); err != nil || out != backfillEmbedded {
			t.Fatalf("pick after the release: %v %v, want embedded", out, err)
		}
		var embedded bool
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1::uuid`, id).Scan(&embedded); err != nil || !embedded {
			t.Errorf("block not embedded after the release: %v %v", embedded, err)
		}
		if class, _ := releaseMemo(t, pool, id); class != "" {
			t.Errorf("memo %q left on the embedded block", class)
		}
	})

	t.Run("a block the backend refuses is attempted once and parked again", func(t *testing.T) {
		id := releaseBlock(t, pool, "rel236 arm refused", "text "+oversizeMarker)
		before := calls.Load()
		if out, err := s.backfillStep(ctx, router, raised, nil); err != nil || out != backfillParked {
			t.Fatalf("first pick: %v %v, want parked by the backend's refusal", out, err)
		}
		if res := releaseAll(t, pool, store.BackfillRelease{Class: store.EmbedFailureOversize}); res.Released != 1 {
			t.Fatalf("release: %+v, want released 1", res)
		}
		if out, err := s.backfillStep(ctx, router, raised, nil); err != nil || out != backfillParked {
			t.Fatalf("pick after the release: %v %v, want parked again", out, err)
		}
		for i := range 3 {
			if out, err := s.backfillStep(ctx, router, raised, nil); err != nil || out != backfillIdle {
				t.Fatalf("pick %d after the re-park: %v %v, want idle", i, out, err)
			}
		}
		if got := calls.Load() - before; got != 2 {
			t.Errorf("wire calls = %d, want 2 (the first attempt and one after the release)", got)
		}
		if class, inf := releaseMemo(t, pool, id); class != "oversize" || !inf {
			t.Errorf("memo after the attempt: %q infinity=%v, want the oversize park", class, inf)
		}
	})
}

// TestBackfillReleaseAgainstAWorkingArm runs 200 class releases on one
// connection while the arm works on another: a backend that answers 500
// memoizes every block as wire with a backoff that has passed by the next
// pick, so the arm keeps taking blocks whose memo row exists, holding the
// block row through the wire call and then writing the memo row — the order
// a release would have to cross to close a cycle. Neither side may fail.
func TestBackfillReleaseAgainstAWorkingArm(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	var down atomic.Bool
	down.Store(true)
	srv, calls := releaseEmbedServer(t, 2*time.Millisecond, &down)
	router := releaseRouter(t, srv.URL)
	cfg := releaseCfg(0, time.Microsecond, time.Millisecond)
	const blocks, rounds = 12, 200
	for i := range blocks {
		releaseBlock(t, pool, fmt.Sprintf("rel236 race %02d", i), fmt.Sprintf("race text %d", i))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var steps atomic.Int64
	armErr := make(chan error, 1)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.backfillStep(ctx, router, cfg, nil); err != nil {
					select {
					case armErr <- err:
					default:
					}
					return
				}
				steps.Add(1)
			}
		}()
	}
	var released, mismatched int64
	var relErr error
	for round := 0; round < rounds && relErr == nil; round++ {
		time.Sleep(5 * time.Millisecond) // spread the rounds over the arm's picks
		dry, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{Class: store.EmbedFailureWire, AllTenants: true})
		if err != nil {
			relErr = fmt.Errorf("round %d: dry run: %w", round, err)
			break
		}
		res, err := store.ReleaseBackfillMemos(ctx, pool, store.BackfillRelease{Class: store.EmbedFailureWire, AllTenants: true, Confirm: &dry.Counted, AsOf: &dry.AsOf})
		switch {
		case errors.Is(err, store.ErrBackfillReleaseConfirmMismatch):
			mismatched++ // the arm wrote or replaced memos between the two counts
		case err != nil:
			relErr = fmt.Errorf("round %d: %w", round, err)
		default:
			released += res.Released
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-armErr:
		t.Errorf("the arm failed while the releases ran: %v", err)
	default:
	}
	if relErr != nil {
		t.Errorf("a release failed while the arm ran: %v", relErr)
	}
	t.Logf("%d rounds: %d memos released, %d confirms refused by a changed count; %d arm picks, %d wire calls",
		rounds, released, mismatched, steps.Load(), calls.Load())
	if released == 0 || steps.Load() == 0 {
		t.Errorf("no overlap exercised: released %d, arm picks %d", released, steps.Load())
	}
}
