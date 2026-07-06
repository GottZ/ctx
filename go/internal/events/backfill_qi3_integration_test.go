//go:build integration

// Integration probes for wave MW9 (Q-I3, design/03 §4.4 / D3-W5): the
// scheduler's embed-backfill acquires its admission lease BEFORE the
// row-lock transaction opens (lease-then-tx order) and never blocks on a
// follow-up target under the held tx (mechanical TryAcquire + defer). The
// pg_stat_activity probe is the D3-W5 gate instrument: a blocked admission
// must coincide with ZERO open backfill transactions — and the old-shape
// fixture proves the probe CAN see the hazard (Rot-Probe capability).
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestBackfillQI3_Integration -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backfillPickMarker identifies the backfill pick statement in
// pg_stat_activity — the only backfill query carrying this clause.
const backfillPickMarker = "%FOR UPDATE SKIP LOCKED%"

// openBackfillTxCount counts foreign sessions of this database sitting in an
// OPEN transaction whose last statement was the backfill pick — the D3-W5
// probe: during a blocked admission this must be zero.
func openBackfillTxCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND pid <> pg_backend_pid()
		   AND state = 'idle in transaction'
		   AND query LIKE $1`, backfillPickMarker).Scan(&n)
	if err != nil {
		t.Fatalf("pg_stat_activity probe: %v", err)
	}
	return n
}

// qi3EmbedServer serves the ollama /api/embed wire shape (embedcache test
// pattern) with an optional per-request gate and scripted status.
type qi3EmbedServer struct {
	srv    *httptest.Server
	status int
	gate   func()
	mu     sync.Mutex
	inputs []string
}

func newQI3EmbedServer(t *testing.T, status int) *qi3EmbedServer {
	t.Helper()
	es := &qi3EmbedServer{status: status}
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2) // passes the quality gate after L2 norm
	}
	es.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		es.mu.Lock()
		es.inputs = append(es.inputs, req.Input)
		es.mu.Unlock()
		if es.gate != nil {
			es.gate()
		}
		if es.status != http.StatusOK {
			w.WriteHeader(es.status)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings":        [][]float64{vec},
			"prompt_eval_count": 3,
		})
	}))
	t.Cleanup(es.srv.Close)
	return es
}

func (es *qi3EmbedServer) recorded() []string {
	es.mu.Lock()
	defer es.mu.Unlock()
	out := make([]string, len(es.inputs))
	copy(out, es.inputs)
	return out
}

// embedPoolRow builds one enabled full-trust embed-role row (dreamPoolRow
// sibling for the embed chain).
func embedPoolRow(name, host string, priority int) backends.Backend {
	return backends.Backend{
		ID: name, Name: name,
		Host: host, Protocol: backends.ProtocolOllama,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "test-embed"}},
		Priority: priority, Enabled: true,
	}
}

// qi3Admitter wraps the real dispatcher and instruments BOTH admission
// doors: every Acquire entry records the open-backfill-tx count (the
// lease-before-tx order probe) and signals entry; every TryAcquire records
// its wall time (the non-blocking guarantee).
type qi3Admitter struct {
	t         *testing.T
	d         *dispatch.Dispatcher
	probePool *pgxpool.Pool
	entered   chan struct{}
	mu        sync.Mutex
	acquireTx []int
	tryDurs   []time.Duration
}

func newQI3Admitter(t *testing.T, d *dispatch.Dispatcher, probePool *pgxpool.Pool) *qi3Admitter {
	return &qi3Admitter{t: t, d: d, probePool: probePool, entered: make(chan struct{}, 8)}
}

func (a *qi3Admitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	n := openBackfillTxCount(a.t, a.probePool)
	a.mu.Lock()
	a.acquireTx = append(a.acquireTx, n)
	a.mu.Unlock()
	select {
	case a.entered <- struct{}{}:
	default:
	}
	return a.d.Acquire(ctx, req)
}

func (a *qi3Admitter) TryAcquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	start := time.Now()
	l, rc, err := a.d.TryAcquire(ctx, req)
	a.mu.Lock()
	a.tryDurs = append(a.tryDurs, time.Since(start))
	a.mu.Unlock()
	return l, rc, err
}

func (a *qi3Admitter) acquireTxCounts() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int(nil), a.acquireTx...)
}

func (a *qi3Admitter) tryDurations() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Duration(nil), a.tryDurs...)
}

func seedPendingBlock(t *testing.T, pool *pgxpool.Pool, title string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('learnings', $1, $2, 'shared', now() - $3::interval, now())`,
		title, "content of "+title, age.String())
	if err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
}

func pendingCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks WHERE embedding IS NULL AND NOT is_archived`).Scan(&n); err != nil {
		t.Fatalf("pending count: %v", err)
	}
	return n
}

func clearBlocks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM context_blocks`); err != nil {
		t.Fatalf("clear blocks: %v", err)
	}
}

func originOf(t *testing.T, rawURL string) string {
	t.Helper()
	o, err := dispatch.NormalizeOrigin(rawURL)
	if err != nil {
		t.Fatalf("normalize %q: %v", rawURL, err)
	}
	return o
}

func backfillRouter(bpool *backends.Pool, adm dispatch.Admitter) *dream.Router {
	return &dream.Router{
		Pool:  bpool,
		Admit: llm.Admission{Admitter: adm, Class: dispatch.ClassBackground},
	}
}

func TestBackfillQI3_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	// probe_sees_old_shape is the permanent Rot-Probe capability fixture
	// (D3-W5): replicate the OLD order — pick under an open tx — and prove
	// the pg_stat_activity probe detects exactly this hazard. Without this,
	// a zero count in the order probe below could mean "probe is blind".
	t.Run("probe_sees_old_shape", func(t *testing.T) {
		seedPendingBlock(t, pool, "old-shape", time.Hour)
		defer clearBlocks(t, pool)

		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // fixture cleanup
		var id string
		err = tx.QueryRow(ctx,
			`SELECT id FROM context_blocks
			WHERE embedding IS NULL AND NOT is_archived
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED`).Scan(&id)
		if err != nil {
			t.Fatalf("old-shape pick: %v", err)
		}
		if n := openBackfillTxCount(t, pool); n != 1 {
			t.Fatalf("probe on old shape = %d open backfill tx, want 1 (probe must SEE the hazard)", n)
		}
	})

	// lease_before_tx is the D3-W5 order gate: the slot is held by a foreign
	// lease, so the backfill's background admission BLOCKS — and while it
	// waits, there must be NO open backfill transaction (lease-then-tx).
	// Against the pre-MW9 shape (acquire under tx) this probe reads 1 → red.
	t.Run("lease_before_tx", func(t *testing.T) {
		seedPendingBlock(t, pool, "order-probe", time.Hour)
		defer clearBlocks(t, pool)

		srv := newQI3EmbedServer(t, http.StatusOK)
		origin := originOf(t, srv.srv.URL)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.srv.URL, 100)})

		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})
		rec := newQI3Admitter(t, d, pool)

		holder, _, err := d.Acquire(ctx, dispatch.Request{
			Target: dispatch.Target{Origin: origin}, Class: dispatch.ClassBackground, Role: "embed"})
		if err != nil {
			t.Fatalf("holder acquire: %v", err)
		}

		type result struct {
			ok  bool
			err error
		}
		done := make(chan result, 1)
		go func() {
			ok, err := s.backfillOneEmbedding(ctx, backfillRouter(bpool, rec))
			done <- result{ok, err}
		}()

		// Wait for the backfill to reach its admission door, then sample the
		// probe repeatedly WHILE it stays blocked on the held slot.
		select {
		case <-rec.entered:
		case <-time.After(15 * time.Second):
			t.Fatal("backfill never reached the admission door")
		}
		maxDuringWait := 0
		for i := 0; i < 20; i++ {
			if n := openBackfillTxCount(t, pool); n > maxDuringWait {
				maxDuringWait = n
			}
			time.Sleep(10 * time.Millisecond)
		}
		holder.Release()

		select {
		case r := <-done:
			if r.err != nil || !r.ok {
				t.Fatalf("backfill after release = (%v, %v), want (true, nil)", r.ok, r.err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("backfill did not finish after holder release")
		}

		for i, n := range rec.acquireTxCounts() {
			if n != 0 {
				t.Errorf("acquire %d entered with %d open backfill tx, want 0 (lease BEFORE tx, Q-I3)", i, n)
			}
		}
		if maxDuringWait != 0 {
			t.Errorf("blocked admission coincided with %d open backfill tx, want 0 (D3-W5 gate)", maxDuringWait)
		}
		if got := pendingCount(t, pool); got != 0 {
			t.Errorf("pending blocks after backfill = %d, want 0 (embedding stored)", got)
		}
	})

	// failover_tryacquire_defers is the D3-W5 failover case: first target
	// down (502 → Next()-class), follow-up target's slot busy — under the
	// held tx the mechanical rule answers via TryAcquire, the backfill
	// defers (false, nil) instead of waiting, and a later run succeeds.
	t.Run("failover_tryacquire_defers", func(t *testing.T) {
		seedPendingBlock(t, pool, "failover-defer", time.Hour)
		defer clearBlocks(t, pool)

		downSrv := newQI3EmbedServer(t, http.StatusBadGateway)
		upSrv := newQI3EmbedServer(t, http.StatusOK)
		downOrigin := originOf(t, downSrv.srv.URL)
		upOrigin := originOf(t, upSrv.srv.URL)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{
			embedPoolRow("embed-down", downSrv.srv.URL, 100),
			embedPoolRow("embed-up", upSrv.srv.URL, 50),
		})

		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
			downOrigin: {Slots: 1},
			upOrigin:   {Slots: 1},
		}})
		rec := newQI3Admitter(t, d, pool)

		holder, _, err := d.Acquire(ctx, dispatch.Request{
			Target: dispatch.Target{Origin: upOrigin}, Class: dispatch.ClassBackground, Role: "embed"})
		if err != nil {
			t.Fatalf("up-slot holder acquire: %v", err)
		}

		start := time.Now()
		ok, err := s.backfillOneEmbedding(ctx, backfillRouter(bpool, rec))
		elapsed := time.Since(start)
		if err != nil || ok {
			t.Fatalf("deferred backfill = (%v, %v), want (false, nil) — Q-I3 defer, not an error loop", ok, err)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("deferred backfill took %v — it must not wait on the busy follow-up target", elapsed)
		}
		durs := rec.tryDurations()
		if len(durs) == 0 {
			t.Fatal("no TryAcquire recorded — follow-up target must go through the non-blocking door")
		}
		for i, dur := range durs {
			if dur > 500*time.Millisecond {
				t.Errorf("TryAcquire %d took %v, want immediate", i, dur)
			}
		}
		if got := pendingCount(t, pool); got != 1 {
			t.Fatalf("pending after defer = %d, want 1 (block deferred, not lost)", got)
		}

		// Defer means LATER works: free the follow-up target, run again.
		holder.Release()
		ok, err = s.backfillOneEmbedding(ctx, backfillRouter(bpool, rec))
		if err != nil || !ok {
			t.Fatalf("retry after defer = (%v, %v), want (true, nil)", ok, err)
		}
		if got := pendingCount(t, pool); got != 0 {
			t.Errorf("pending after retry = %d, want 0", got)
		}
	})

	// parallel_workers_pick_disjoint pins the Welle-49 duplicate guard the
	// tx-wrap exists for: two concurrent workers overlap on the wire (the
	// gate holds the first responder until both picked) and must embed two
	// DISTINCT blocks — the row lock held over the embed keeps them apart.
	t.Run("parallel_workers_pick_disjoint", func(t *testing.T) {
		seedPendingBlock(t, pool, "disjoint-a", 2*time.Hour)
		seedPendingBlock(t, pool, "disjoint-b", time.Hour)
		defer clearBlocks(t, pool)

		srv := newQI3EmbedServer(t, http.StatusOK)
		var gateOnce sync.Once
		barrier := make(chan struct{})
		srv.gate = func() {
			// First request waits until the second arrived (bounded), so
			// both row locks are provably held at the same time.
			gateOnce.Do(func() {
				select {
				case <-barrier:
				case <-time.After(5 * time.Second):
				}
			})
			select {
			case barrier <- struct{}{}:
			default:
			}
		}
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.srv.URL, 100)})

		d := dispatch.New(nil, dispatch.DefaultSettings()) // empty policy: pass-through
		t.Cleanup(d.Close)
		rec := newQI3Admitter(t, d, pool)

		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := s.backfillOneEmbedding(ctx, backfillRouter(bpool, rec))
				if err == nil && !ok {
					err = context.DeadlineExceeded // marker: nothing picked
				}
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatalf("parallel worker: %v", err)
			}
		}
		inputs := srv.recorded()
		if len(inputs) != 2 || inputs[0] == inputs[1] {
			t.Fatalf("wire inputs = %q, want 2 DISTINCT texts (disjoint picks)", inputs)
		}
		if got := pendingCount(t, pool); got != 0 {
			t.Errorf("pending after parallel backfill = %d, want 0", got)
		}
	})

	// passthrough_baseline pins behavior neutrality with empty policy
	// (D3-W5): one block, one wire call, embedding stored, and the empty
	// table answers (false, nil) — the pre-MW9 consumer contract.
	t.Run("passthrough_baseline", func(t *testing.T) {
		seedPendingBlock(t, pool, "baseline", time.Hour)
		defer clearBlocks(t, pool)

		srv := newQI3EmbedServer(t, http.StatusOK)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		rec := newQI3Admitter(t, d, pool)

		ok, err := s.backfillOneEmbedding(ctx, backfillRouter(bpool, rec))
		if err != nil || !ok {
			t.Fatalf("baseline backfill = (%v, %v), want (true, nil)", ok, err)
		}
		if got := len(srv.recorded()); got != 1 {
			t.Fatalf("wire calls = %d, want 1", got)
		}
		if got := pendingCount(t, pool); got != 0 {
			t.Fatalf("pending after baseline = %d, want 0", got)
		}
		ok, err = s.backfillOneEmbedding(ctx, backfillRouter(bpool, rec))
		if err != nil || ok {
			t.Fatalf("empty-table backfill = (%v, %v), want (false, nil)", ok, err)
		}
	})
}
