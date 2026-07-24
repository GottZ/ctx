//go:build integration

// Gates for the Evokoa-Clean-Room-Plan Achse 04 W04-4 (design/04 §4.3/§4.4,
// §7 wave row W04-4): the re-embed migration scheduler arm.
//
//   - G-Rot (model_guard_pauses_without_embed_next_key): two-backend-class
//     setup where the target backend carries NO embed_next model_map key —
//     the arm MUST transition running → paused (Model-Guard, §4.2) and must
//     NOT write a single _next vector with the default-resolved old model.
//     RED evidence: run against the guard-less intermediate build (worker
//     without the per-cycle guard) — status stays 'running' and
//     embedding_next gets filled with embed_model_next='test-embed' (the OLD
//     model silently re-labeled as migrated), exactly §5 Bruchpfad 2.
//   - G-Grün (kernpfad): embedding byte-identical untouched, embedding_next
//     filled, embed_model_next = to_model, counters exact against count
//     queries, wire calls carry the embed_next-resolved model.
//   - G-Cursor (cursor_explain_pin): 50 clustered oversize skips at the
//     queue head — the peek EXPLAIN shows an index scan on
//     idx_embedding_next_pending with the cursor as range condition, never a
//     re-scan of the skip prefix.
//   - Plus: oversize pre-wire skip, HTTP-400 exceed_context_size_error wire
//     classification, transient-failure backoff, sensitivity_ineligible,
//     verifying drain, paused idle, ClearEmbedding convergence, runtime
//     index lifecycle (CIC / INVALID recovery / terminal drop).
//
// Run: go test -tags=integration ./internal/events/ -run TestEmbedMigrate_Integration -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

const (
	migFromModel = "test-embed"
	migToModel   = "test-embed-next"
	// migOversizeWireMarker in a block's content makes the fake backend
	// answer the MEASURED llama.cpp oversize contract (HTTP 400,
	// type "exceed_context_size_error" — Lead-Messung 2026-07-24).
	migOversizeWireMarker = "OVERSIZE_WIRE_MARKER_W04_4"
	// migWireFailMarker triggers a plain 500 (transient wire failure class).
	migWireFailMarker = "WIRE_FAIL_MARKER_W04_4"
)

// migrateEmbedServer serves the ollama /api/embed wire shape and records
// (model, input) per request — the model recording is the wire-side proof
// that the worker resolved via the embed_next model_map key.
type migrateEmbedServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []struct{ Model, Input string }
}

func newMigrateEmbedServer(t *testing.T) *migrateEmbedServer {
	t.Helper()
	es := &migrateEmbedServer{}
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2) // passes the quality gate after L2 norm
	}
	es.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		es.mu.Lock()
		es.reqs = append(es.reqs, struct{ Model, Input string }{req.Model, req.Input})
		es.mu.Unlock()
		switch {
		case strings.Contains(req.Input, migOversizeWireMarker):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"request (33000 tokens) exceeds the available context size (32768 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":33000,"n_ctx":32768}}`))
		case strings.Contains(req.Input, migWireFailMarker):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"embeddings":        [][]float64{vec},
				"prompt_eval_count": 3,
			})
		}
	}))
	t.Cleanup(es.srv.Close)
	return es
}

func (es *migrateEmbedServer) recorded() []struct{ Model, Input string } {
	es.mu.Lock()
	defer es.mu.Unlock()
	out := make([]struct{ Model, Input string }, len(es.reqs))
	copy(out, es.reqs)
	return out
}

// embedNextPoolRow is embedPoolRow plus the armed embed_next model_map key
// (§4.2: the operator's per-backend migration arming).
func embedNextPoolRow(name, host string, priority int) backends.Backend {
	b := embedPoolRow(name, host, priority)
	b.ModelMap = map[string]backends.ModelSpec{
		"default":    {Model: migFromModel},
		"embed_next": {Model: migToModel},
	}
	return b
}

// seedMigrationModels registers the two registry rows (idempotent).
func seedMigrationModels(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, m := range []string{migFromModel, migToModel} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims)
			 VALUES ($1, 'test', 1024, 1024) ON CONFLICT (model_key) DO NOTHING`, m); err != nil {
			t.Fatalf("seed model %s: %v", m, err)
		}
	}
}

// seedMigrationRow inserts the single active migration in the given status
// and returns its id. Bypasses embedmigration.Create deliberately: the
// worker tests exercise the ARM, not the create validation (W04-3 covers
// that) — a synthetic row keeps the fixture free of context_backends setup.
func seedMigrationRow(t *testing.T, pool *pgxpool.Pool, status string) string {
	t.Helper()
	seedMigrationModels(t, pool)
	var id string
	sql := `INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status, started_at)
	        VALUES ($1, $2, 'embed-a', $3, now()) RETURNING id::text`
	if status == "verifying" {
		sql = `INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status, started_at, verify_started_at)
		       VALUES ($1, $2, 'embed-a', $3, now(), now()) RETURNING id::text`
	}
	if err := pool.QueryRow(context.Background(), sql, migFromModel, migToModel, status).Scan(&id); err != nil {
		t.Fatalf("seed migration row: %v", err)
	}
	return id
}

// seedMigratableBlock inserts a block that already carries an OLD-space
// vector (the migration pending universe: embedding IS NOT NULL AND
// embedding_next IS NULL) and returns its id.
func seedMigratableBlock(t *testing.T, pool *pgxpool.Pool, title, content, sensitivity string, age time.Duration) string {
	t.Helper()
	vec := make([]float32, embed.TargetDims)
	for i := range vec {
		vec[i] = float32((i%3)-1) * 0.03
	}
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity, embedding, embed_model, created_at, updated_at)
		 VALUES ('learnings', $1, $2, 'shared', $3, $4, $5, now() - $6::interval, now())
		 RETURNING id::text`,
		title, content, sensitivity, pgvec.NewVector(vec), migFromModel, age.String()).Scan(&id); err != nil {
		t.Fatalf("seed migratable block %s: %v", title, err)
	}
	return id
}

// resetMigrationFixture wipes blocks, memos and migration rows between
// subtests (single shared container). The registry rows and the runtime
// index may persist — both are idempotent fixtures.
func resetMigrationFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		`DELETE FROM context_embed_failures`,
		`DELETE FROM context_embed_migrations`,
		`DELETE FROM context_blocks`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("reset fixture (%s): %v", sql, err)
		}
	}
}

// migCfg builds the per-subtest EmbedMigration config surface.
func migCfg(batch, maxTokens int) *config.Config {
	return &config.Config{
		EmbedMigration: config.EmbedMigrationConfig{
			BatchPerCycle: batch,
			MaxTokens:     maxTokens,
			BackoffBase:   time.Hour, // larger than any test wall-clock: memoized rows stay excluded
			BackoffCap:    24 * time.Hour,
		},
	}
}

// migrationRowState reads the counters/cursor/status of the migration row.
type migrationRowState struct {
	Status   string
	Migrated int64
	Failed   int64
	Skipped  int64
	Cursor   *time.Time
	LastErr  *string
}

func readMigrationRow(t *testing.T, pool *pgxpool.Pool, id string) migrationRowState {
	t.Helper()
	var st migrationRowState
	if err := pool.QueryRow(context.Background(),
		`SELECT status, migrated_count, failed_count, skipped_count, cursor_created_at, last_error
		 FROM context_embed_migrations WHERE id = $1::uuid`, id).
		Scan(&st.Status, &st.Migrated, &st.Failed, &st.Skipped, &st.Cursor, &st.LastErr); err != nil {
		t.Fatalf("read migration row: %v", err)
	}
	return st
}

func countNextEmbedded(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks WHERE embedding_next IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count embedding_next: %v", err)
	}
	return n
}

func TestEmbedMigrate_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	// model_guard_pauses_without_embed_next_key is G-Rot (§4.2, §5
	// Bruchpfad 2): the backend serves the OLD model under "default" and
	// carries NO embed_next key — ModelFor("embed_next") silently falls
	// back to the old model. The guard must pause the migration BEFORE any
	// wire call; without the guard the worker would relabel old-space
	// vectors as migrated.
	t.Run("model_guard_pauses_without_embed_next_key", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		seedMigratableBlock(t, pool, "guard-victim", "some content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.srv.URL, 100)}) // NO embed_next key
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}

		st := readMigrationRow(t, pool, migID)
		if st.Status != "paused" {
			t.Errorf("status = %q, want %q (Model-Guard must pause)", st.Status, "paused")
		}
		if st.LastErr == nil || !strings.Contains(*st.LastErr, "model guard") ||
			!strings.Contains(*st.LastErr, migToModel) {
			t.Errorf("last_error = %v, want model-guard message naming expected %q", st.LastErr, migToModel)
		}
		if got := len(srv.recorded()); got != 0 {
			t.Errorf("wire calls = %d, want 0 (guard fires BEFORE any wire call)", got)
		}
		if got := countNextEmbedded(t, pool); got != 0 {
			t.Errorf("embedding_next rows = %d, want 0 (no old-model vector may be relabeled as migrated)", got)
		}
	})

	// kernpfad is G-Grün: three old-space blocks migrate; the serving pair
	// stays byte-identical; counters match exact count queries; the wire
	// carries the embed_next-resolved model; the cursor wraps to NULL at
	// queue end.
	t.Run("kernpfad", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		ids := []string{
			seedMigratableBlock(t, pool, "kern-a", "content a", "internal", 3*time.Hour),
			seedMigratableBlock(t, pool, "kern-b", "content b", "internal", 2*time.Hour),
			seedMigratableBlock(t, pool, "kern-c", "content c", "internal", time.Hour),
		}

		before := make(map[string]string, len(ids))
		for _, id := range ids {
			var v string
			if err := pool.QueryRow(ctx,
				`SELECT embedding::text FROM context_blocks WHERE id = $1`, id).Scan(&v); err != nil {
				t.Fatalf("capture embedding before: %v", err)
			}
			before[id] = v
		}

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}

		for _, id := range ids {
			var after string
			var nextSet bool
			var nextModel *string
			if err := pool.QueryRow(ctx,
				`SELECT embedding::text, embedding_next IS NOT NULL, embed_model_next
				 FROM context_blocks WHERE id = $1`, id).Scan(&after, &nextSet, &nextModel); err != nil {
				t.Fatalf("read block %s: %v", id, err)
			}
			if after != before[id] {
				t.Errorf("block %s: serving embedding CHANGED — must stay byte-identical until cutover", id)
			}
			if !nextSet {
				t.Errorf("block %s: embedding_next not filled", id)
			}
			if nextModel == nil || *nextModel != migToModel {
				t.Errorf("block %s: embed_model_next = %v, want %q", id, nextModel, migToModel)
			}
		}

		st := readMigrationRow(t, pool, migID)
		if want := countNextEmbedded(t, pool); st.Migrated != want || want != int64(len(ids)) {
			t.Errorf("migrated_count = %d, want %d (== count query)", st.Migrated, want)
		}
		if st.Failed != 0 || st.Skipped != 0 {
			t.Errorf("failed/skipped = %d/%d, want 0/0", st.Failed, st.Skipped)
		}
		if st.Status != "running" {
			t.Errorf("status = %q, want running", st.Status)
		}
		if st.Cursor != nil {
			t.Errorf("cursor_created_at = %v, want NULL (wrap-around at queue end)", st.Cursor)
		}
		for i, r := range srv.recorded() {
			if r.Model != migToModel {
				t.Errorf("wire call %d used model %q, want %q (embed_next resolution)", i, r.Model, migToModel)
			}
		}
		if got := len(srv.recorded()); got != len(ids) {
			t.Errorf("wire calls = %d, want %d", got, len(ids))
		}
	})

	// oversize_pre_wire_skip: the len/4 estimate gate parks the block with
	// an infinity memo WITHOUT a wire call; the younger block still
	// migrates in the same cycle (infinity blocks never block the queue).
	t.Run("oversize_pre_wire_skip", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		bigID := seedMigratableBlock(t, pool, "oversize-old", strings.Repeat("x", 1000), "internal", 2*time.Hour)
		seedMigratableBlock(t, pool, "normal-young", "small content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		// MaxTokens 50: the 1000-char block estimates 250 tokens > 50.
		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 50)); err != nil {
			t.Fatalf("cycle: %v", err)
		}

		var lastClass string
		var isInfinity bool
		var memoMig *string
		if err := pool.QueryRow(ctx,
			`SELECT last_class, next_attempt_at = 'infinity', migration_id::text
			 FROM context_embed_failures WHERE block_id = $1`, bigID).
			Scan(&lastClass, &isInfinity, &memoMig); err != nil {
			t.Fatalf("read oversize memo: %v", err)
		}
		if lastClass != "oversize" || !isInfinity {
			t.Errorf("memo = (%q, infinity=%v), want (oversize, true)", lastClass, isInfinity)
		}
		if memoMig == nil || *memoMig != migID {
			t.Errorf("memo migration_id = %v, want %q (migration-scoped)", memoMig, migID)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Skipped != 1 || st.Migrated != 1 || st.Failed != 0 {
			t.Errorf("counters (m/f/s) = %d/%d/%d, want 1/0/1", st.Migrated, st.Failed, st.Skipped)
		}
		reqs := srv.recorded()
		if len(reqs) != 1 || !strings.Contains(reqs[0].Input, "small content") {
			t.Errorf("wire calls = %v, want exactly the younger block (oversize skips pre-wire)", reqs)
		}
	})

	// oversize_wire_400_classified: the estimate gate is OFF; the backend
	// answers the measured llama.cpp contract (400 +
	// exceed_context_size_error) — the worker classifies it as oversize
	// (infinity park, skipped_count), never a generic retry loop.
	t.Run("oversize_wire_400_classified", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		bigID := seedMigratableBlock(t, pool, "wire-oversize-old", "content "+migOversizeWireMarker, "internal", 2*time.Hour)
		seedMigratableBlock(t, pool, "wire-normal-young", "fine content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}

		var lastClass string
		var isInfinity bool
		if err := pool.QueryRow(ctx,
			`SELECT last_class, next_attempt_at = 'infinity'
			 FROM context_embed_failures WHERE block_id = $1`, bigID).Scan(&lastClass, &isInfinity); err != nil {
			t.Fatalf("read wire-oversize memo: %v", err)
		}
		if lastClass != "oversize" || !isInfinity {
			t.Errorf("memo = (%q, infinity=%v), want (oversize, true) — 400+exceed_context_size_error must classify as oversize", lastClass, isInfinity)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Skipped != 1 || st.Migrated != 1 || st.Failed != 0 {
			t.Errorf("counters (m/f/s) = %d/%d/%d, want 1/0/1", st.Migrated, st.Failed, st.Skipped)
		}
	})

	// wire_failure_backoff: a transient 500 gets a FINITE exponential
	// backoff memo (failed_count), the cursor moves past it and the
	// younger block migrates in the same cycle.
	t.Run("wire_failure_backoff", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		failID := seedMigratableBlock(t, pool, "fail-old", "content "+migWireFailMarker, "internal", 2*time.Hour)
		seedMigratableBlock(t, pool, "fail-normal-young", "healthy content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}

		var lastClass string
		var isInfinity, inFuture bool
		if err := pool.QueryRow(ctx,
			`SELECT last_class, next_attempt_at = 'infinity', next_attempt_at > now()
			 FROM context_embed_failures WHERE block_id = $1`, failID).
			Scan(&lastClass, &isInfinity, &inFuture); err != nil {
			t.Fatalf("read wire-failure memo: %v", err)
		}
		if lastClass != "wire" || isInfinity || !inFuture {
			t.Errorf("memo = (%q, infinity=%v, future=%v), want (wire, false, true) — finite backoff", lastClass, isInfinity, inFuture)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Failed != 1 || st.Migrated != 1 || st.Skipped != 0 {
			t.Errorf("counters (m/f/s) = %d/%d/%d, want 1/1/0", st.Migrated, st.Failed, st.Skipped)
		}
	})

	// sensitivity_ineligible: a public-trust-only backend cannot receive a
	// credentials block — the worker parks it with a sensitivity memo
	// (infinity, NEVER escalation across the trust border) and migrates
	// the eligible public block.
	t.Run("sensitivity_ineligible", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		credID := seedMigratableBlock(t, pool, "cred-old", "secret content", "credentials", 2*time.Hour)
		seedMigratableBlock(t, pool, "public-young", "public content", "public", time.Hour)

		srv := newMigrateEmbedServer(t)
		row := embedNextPoolRow("embed-a", srv.srv.URL, 100)
		row.Trust = backends.TrustPublic
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{row})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}

		var lastClass string
		var isInfinity bool
		if err := pool.QueryRow(ctx,
			`SELECT last_class, next_attempt_at = 'infinity'
			 FROM context_embed_failures WHERE block_id = $1`, credID).Scan(&lastClass, &isInfinity); err != nil {
			t.Fatalf("read sensitivity memo: %v", err)
		}
		if lastClass != "sensitivity_ineligible" || !isInfinity {
			t.Errorf("memo = (%q, infinity=%v), want (sensitivity_ineligible, true)", lastClass, isInfinity)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Skipped != 1 || st.Migrated != 1 {
			t.Errorf("counters (m/s) = %d/%d, want 1/1", st.Migrated, st.Skipped)
		}
		var credNext bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding_next IS NOT NULL FROM context_blocks WHERE id = $1`, credID).Scan(&credNext); err != nil {
			t.Fatalf("read cred block: %v", err)
		}
		if credNext {
			t.Errorf("credentials block got a _next vector from a public-trust backend — trust border crossed")
		}
	})

	// verifying_drains pins the §4.1 drain semantics: the arm keeps
	// working in 'verifying' (organic writes never stop; a running-only
	// arm would livelock the confirm).
	t.Run("verifying_drains", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedMigratableBlock(t, pool, "verify-drain", "late content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Migrated != 1 {
			t.Errorf("migrated_count = %d, want 1 (arm must drain in verifying)", st.Migrated)
		}
		if st.Status != "verifying" {
			t.Errorf("status = %q, want verifying (drain, not transition)", st.Status)
		}
	})

	// paused_idles: a paused migration does NOTHING — no wire, no writes,
	// no counter movement.
	t.Run("paused_idles", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "paused")
		seedMigratableBlock(t, pool, "paused-block", "content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)

		if err := s.runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Migrated != 0 || len(srv.recorded()) != 0 || countNextEmbedded(t, pool) != 0 {
			t.Errorf("paused migration did work: migrated=%d wire=%d next=%d, want all 0",
				st.Migrated, len(srv.recorded()), countNextEmbedded(t, pool))
		}
	})

	// clear_embedding_converges is the Konvergenz-Probe (§4.3): a migrated
	// block whose content changes (ClearEmbedding) loses BOTH pairs, the
	// regular backfill refills the old space, and the migration worker
	// re-migrates it on the next pass.
	t.Run("clear_embedding_converges", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		seedMigrationRow(t, pool, "running")
		blockID := seedMigratableBlock(t, pool, "converge", "original content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		router := backfillRouter(bpool, d)

		if err := s.runEmbedMigrationCycle(ctx, router, migCfg(8, 0)); err != nil {
			t.Fatalf("cycle 1: %v", err)
		}
		if countNextEmbedded(t, pool) != 1 {
			t.Fatalf("precondition: block not migrated")
		}

		// Content change → ClearEmbedding nulls BOTH pairs (W04-3).
		if err := store.ClearEmbedding(ctx, pool, blockID); err != nil {
			t.Fatalf("clear embedding: %v", err)
		}
		var liveNull, nextNull bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL, embedding_next IS NULL FROM context_blocks WHERE id = $1`,
			blockID).Scan(&liveNull, &nextNull); err != nil {
			t.Fatalf("read cleared block: %v", err)
		}
		if !liveNull || !nextNull {
			t.Fatalf("after ClearEmbedding: live NULL=%v next NULL=%v, want both true", liveNull, nextNull)
		}

		// Regular backfill refills the OLD space (simulated via the same
		// primitive Pfad B uses).
		vec := make([]float32, embed.TargetDims)
		for i := range vec {
			vec[i] = 0.01
		}
		if err := store.StoreEmbedding(ctx, pool, blockID, migFromModel, vec); err != nil {
			t.Fatalf("re-backfill old space: %v", err)
		}

		// Next migration pass (cursor wrapped to NULL after cycle 1) picks
		// the block up again.
		if err := s.runEmbedMigrationCycle(ctx, router, migCfg(8, 0)); err != nil {
			t.Fatalf("cycle 2: %v", err)
		}
		var nextSet bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding_next IS NOT NULL FROM context_blocks WHERE id = $1`, blockID).Scan(&nextSet); err != nil {
			t.Fatalf("read reconverged block: %v", err)
		}
		if !nextSet {
			t.Errorf("block did not reconverge into the new space after ClearEmbedding + re-backfill")
		}
	})

	// runtime_index_lifecycle: CIC creation on an active cycle, INVALID
	// recovery (fabricated via pg_index), terminal drop.
	t.Run("runtime_index_lifecycle", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		seedMigratableBlock(t, pool, "index-block", "content", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		router := backfillRouter(bpool, d)

		if err := s.runEmbedMigrationCycle(ctx, router, migCfg(8, 0)); err != nil {
			t.Fatalf("cycle: %v", err)
		}
		var valid bool
		if err := pool.QueryRow(ctx,
			`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			 WHERE c.relname = $1`, migrationPendingIndexName).Scan(&valid); err != nil {
			t.Fatalf("index missing after active cycle: %v", err)
		}
		if !valid {
			t.Fatalf("index INVALID after clean CIC")
		}

		// INVALID recovery: fabricate the crashed-CIC leftover state.
		if _, err := pool.Exec(ctx,
			`UPDATE pg_index SET indisvalid = false
			 WHERE indexrelid = $1::regclass`, migrationPendingIndexName); err != nil {
			t.Skipf("cannot fabricate INVALID index (needs catalog write): %v", err)
		}
		if err := s.runEmbedMigrationCycle(ctx, router, migCfg(8, 0)); err != nil {
			t.Fatalf("recovery cycle: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			 WHERE c.relname = $1`, migrationPendingIndexName).Scan(&valid); err != nil {
			t.Fatalf("index missing after recovery: %v", err)
		}
		if !valid {
			t.Errorf("index still INVALID after recovery cycle")
		}

		// Terminal → drop.
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_migrations SET status = 'aborted', abort_reason = 'test' WHERE id = $1::uuid`,
			migID); err != nil {
			t.Fatalf("abort migration: %v", err)
		}
		if err := s.runEmbedMigrationCycle(ctx, router, migCfg(8, 0)); err != nil {
			t.Fatalf("terminal cycle: %v", err)
		}
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'i')`,
			migrationPendingIndexName).Scan(&exists); err != nil {
			t.Fatalf("existence probe: %v", err)
		}
		if exists {
			t.Errorf("runtime index survived terminal status — must be dropped")
		}
	})

	// cursor_explain_pin is G-Cursor (§4.3): 50 clustered oversize skips at
	// the queue head; after they are consumed, the peek must be an INDEX
	// range scan starting at the persistent cursor — O(Batch), never a
	// re-scan of the skip prefix.
	t.Run("cursor_explain_pin", func(t *testing.T) {
		defer resetMigrationFixture(t, pool)
		migID := seedMigrationRow(t, pool, "running")
		for i := 0; i < 50; i++ {
			seedMigratableBlock(t, pool, fmt.Sprintf("skip-%02d", i),
				strings.Repeat("y", 1000), "internal", 100*time.Hour-time.Duration(i)*time.Minute)
		}
		seedMigratableBlock(t, pool, "tail-a", "small a", "internal", 3*time.Hour)
		seedMigratableBlock(t, pool, "tail-b", "small b", "internal", 2*time.Hour)
		seedMigratableBlock(t, pool, "tail-c", "small c", "internal", time.Hour)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		router := backfillRouter(bpool, d)
		cfg := migCfg(8, 50) // 1000-char skips estimate 250 tokens > 50

		// 6 cycles à batch 8 = 48 picks — all of them skips, so the cursor
		// stands DEEP inside the skip cluster (48 of 50 consumed) and is
		// still non-NULL (no wrap yet): the exact mid-stream state the
		// EXPLAIN pin needs.
		for i := 0; i < 6; i++ {
			if err := s.runEmbedMigrationCycle(ctx, router, cfg); err != nil {
				t.Fatalf("cycle %d: %v", i, err)
			}
		}
		st := readMigrationRow(t, pool, migID)
		if st.Skipped != 48 {
			t.Fatalf("skipped_count = %d, want 48 (fixture premise: 6 cycles à 8 picks, all skips)", st.Skipped)
		}
		if st.Cursor == nil {
			t.Fatalf("cursor is NULL mid-stream — cannot pin the range scan")
		}

		// EXPLAIN the peek with the PERSISTED cursor. enable_seqscan off:
		// at 53 rows the planner would otherwise always seq-scan — the pin
		// is about the plan SHAPE being available (index range from the
		// cursor), which is what carries at 10M.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin explain tx: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // probe tx, never committed
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatalf("disable seqscan: %v", err)
		}
		rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF) `+migratePeekSQL(true), migID, *st.Cursor)
		if err != nil {
			t.Fatalf("explain peek: %v", err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan line: %v", err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		planText := strings.Join(plan, "\n")
		t.Logf("peek EXPLAIN with cursor:\n%s", planText)
		if !strings.Contains(planText, "Index Scan using "+migrationPendingIndexName) {
			t.Errorf("peek plan does not use %s:\n%s", migrationPendingIndexName, planText)
		}
		if !strings.Contains(planText, "Index Cond: (created_at > ") {
			t.Errorf("peek plan has no created_at range condition from the cursor:\n%s", planText)
		}

		// Finish the queue: tail blocks migrate, cursor wraps, counters
		// stay exact against count queries.
		for i := 0; i < 3; i++ {
			if err := s.runEmbedMigrationCycle(ctx, router, cfg); err != nil {
				t.Fatalf("tail cycle %d: %v", i, err)
			}
		}
		st = readMigrationRow(t, pool, migID)
		if st.Migrated != 3 || st.Skipped != 50 || st.Failed != 0 {
			t.Errorf("final counters (m/f/s) = %d/%d/%d, want 3/0/50", st.Migrated, st.Failed, st.Skipped)
		}
		if want := countNextEmbedded(t, pool); st.Migrated != want {
			t.Errorf("migrated_count = %d, want %d (== count query)", st.Migrated, want)
		}
		var memoCount int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_embed_failures WHERE migration_id = $1::uuid AND next_attempt_at = 'infinity'`,
			migID).Scan(&memoCount); err != nil {
			t.Fatalf("memo count: %v", err)
		}
		if memoCount != st.Skipped {
			t.Errorf("infinity memos = %d, want %d (== skipped_count)", memoCount, st.Skipped)
		}
		if st.Cursor != nil {
			t.Errorf("cursor = %v, want NULL after full drain (wrap)", st.Cursor)
		}
	})
}
