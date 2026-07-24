//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-3 create-time
// validation (design/04-reembed-migration.md §4.2/§4.10/§6.1), pinned
// against migration 114's context_embed_models/context_embed_migrations
// tables and the single-active partial-unique index. Covers every gate
// named in the wave briefing's §7 line: single-active CAS via the unique
// index, tenant-scoped/external to_backend rejection, dims mismatch
// rejection, rest-embedding_next-data rejection, and disk pre-flight
// fail-closed (both the injected-negative and the real-statfs-positive
// path).
//
// Run: go test -tags=integration ./internal/embedmigration/ -count=1 -v
package embedmigration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// abundantDisk is a DiskChecker stub that always reports ample free space —
// the tests that aren't specifically exercising the disk gate use this so
// their totalBlocks (near-zero in a fresh test DB, and EstimateDiskBytes(0)
// is 0 anyway) can never accidentally trip it.
func abundantDisk() (uint64, error) { return 1 << 40, nil } // 1 TiB

// seedModel inserts a context_embed_models row with the given stored_dims —
// migration 114 already seeds 'qwen3-embedding-8b' at 1024; tests that need
// a SECOND model (to compose from_model/to_model pairs, or to force a dims
// mismatch) call this.
func seedModel(t *testing.T, pool *pgxpool.Pool, key string, storedDims int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims)
		 VALUES ($1, 'test-family', $2, $2)`,
		key, storedDims,
	); err != nil {
		t.Fatalf("seed model %s: %v", key, err)
	}
}

// seedBackend inserts a context_backends row via the production CreateBackend
// path (mirrors backends_gate_integration_test.go's mkBackend pattern) and
// commits immediately so subsequent pool-level Create() calls see it.
func seedBackend(t *testing.T, pool *pgxpool.Pool, name, locality, scope string, modelMap map[string]backends.ModelSpec) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	b := &backends.Backend{
		Name: name, Host: "http://127.0.0.1:11434", Protocol: backends.ProtocolOllama,
		ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull,
		Locality: locality, Roles: []string{backends.RoleEmbed},
		ModelMap: modelMap, Enabled: true, Scope: scope,
	}
	if _, err := store.CreateBackend(ctx, tx, b, nil); err != nil {
		t.Fatalf("create backend %s: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit backend %s: %v", name, err)
	}
}

func TestCreate_HappyPath_ThenSecondCreateHitsSingleActiveIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-a", 1024)
	seedBackend(t, pool, "backend-a", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-a"}})

	id, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-a", ToBackend: "backend-a",
	}, abundantDisk)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if id == "" {
		t.Fatalf("first Create returned empty id")
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM context_embed_migrations WHERE id = $1::uuid`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != string(embedmigration.StatusPending) {
		t.Errorf("status = %q, want %q", status, embedmigration.StatusPending)
	}

	// Second create — a DIFFERENT model pair, but the first migration is
	// still pending (active) — must fail on the single-active partial-unique
	// index (idx_embed_migration_single_active), not merely on application
	// logic (design §5 Bruchpfad 1: "der zweite create scheitert am
	// Constraint, nicht an Anwendungslogik").
	seedModel(t, pool, "to-model-b", 1024)
	seedBackend(t, pool, "backend-b", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-b"}})

	_, err = embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-b", ToBackend: "backend-b",
	}, abundantDisk)
	if !errors.Is(err, embedmigration.ErrActiveMigrationExists) {
		t.Fatalf("second Create error = %v, want ErrActiveMigrationExists", err)
	}
}

func TestCreate_RejectsTenantScopedToBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-tenant", 1024)
	seedBackend(t, pool, "backend-tenant", backends.LocalityLocal, "some-tenant",
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-tenant"}})

	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-tenant", ToBackend: "backend-tenant",
	}, abundantDisk)
	if !errors.Is(err, embedmigration.ErrBackendNotGlobalScoped) {
		t.Fatalf("Create error = %v, want ErrBackendNotGlobalScoped", err)
	}
}

func TestCreate_RejectsExternalLocalityToBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-ext", 1024)
	seedBackend(t, pool, "backend-ext", backends.LocalityExternal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-ext"}})

	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-ext", ToBackend: "backend-ext",
	}, abundantDisk)
	if !errors.Is(err, embedmigration.ErrBackendNotLocal) {
		t.Fatalf("Create error = %v, want ErrBackendNotLocal", err)
	}
}

func TestCreate_RejectsBackendMissingEmbedNextKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-nokey", 1024)
	// Backend carries a model_map but no "embed_next" entry at all.
	seedBackend(t, pool, "backend-nokey", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"default": {Model: "to-model-nokey"}})

	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-nokey", ToBackend: "backend-nokey",
	}, abundantDisk)
	if !errors.Is(err, embedmigration.ErrBackendMissingEmbedNext) {
		t.Fatalf("Create error = %v, want ErrBackendMissingEmbedNext", err)
	}
}

func TestCreate_RejectsDimsMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// stored_dims differs from the bundled 'qwen3-embedding-8b' (1024) —
	// v1 rejects a dimension-change migration outright (§6.4/E-04-6).
	seedModel(t, pool, "to-model-2048", 2048)
	seedBackend(t, pool, "backend-2048", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-2048"}})

	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-2048", ToBackend: "backend-2048",
	}, abundantDisk)
	if !errors.Is(err, embedmigration.ErrDimsMismatch) {
		t.Fatalf("Create error = %v, want ErrDimsMismatch", err)
	}
}

func TestCreate_RejectsLeftoverEmbeddingNextData(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-leftover", 1024)
	seedBackend(t, pool, "backend-leftover", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-leftover"}})

	// Simulate a block already carrying _next data from a prior (aborted,
	// never-purged) migration attempt.
	var blockID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('issue', 'w04-3-leftover', 'body', 'w04-3') RETURNING id::text`).Scan(&blockID); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET embedding_next = $2::vector, embed_model_next = 'stale-model' WHERE id = $1`,
		blockID, pgVecLiteral1024(0.1),
	); err != nil {
		t.Fatalf("seed leftover embedding_next: %v", err)
	}

	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-leftover", ToBackend: "backend-leftover",
	}, abundantDisk)
	if !errors.Is(err, embedmigration.ErrRestEmbeddingNextData) {
		t.Fatalf("Create error = %v, want ErrRestEmbeddingNextData", err)
	}
}

func TestCreate_DiskPreFlight_FailClosedOnInsufficientSpace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-disk", 1024)
	seedBackend(t, pool, "backend-disk", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-disk"}})

	// Seed enough embedded blocks that EstimateDiskBytes(total) > 0, then
	// inject a DiskChecker reporting less free space than the estimate —
	// the negative path per design §6.1 ("der Negativ-Pfad braucht
	// Injektion" — no real disk is filled to prove this).
	for i := 0; i < 5; i++ {
		var blockID string
		title := fmt.Sprintf("w04-3-disk-%d", i)
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', $1, 'body', 'w04-3') RETURNING id::text`, title).Scan(&blockID); err != nil {
			t.Fatalf("seed block: %v", err)
		}
		if err := store.StoreEmbedding(ctx, pool, blockID, "qwen3-embedding-8b", make([]float32, 1024)); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}

	starvedDisk := func() (uint64, error) { return 1, nil } // 1 byte free

	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-disk", ToBackend: "backend-disk",
	}, starvedDisk)
	var diskErr *embedmigration.DiskEstimate
	if !errors.As(err, &diskErr) {
		t.Fatalf("Create error = %v, want *DiskEstimate (via ErrDiskInsufficient)", err)
	}
	if diskErr.FreeBytes != 1 {
		t.Errorf("DiskEstimate.FreeBytes = %d, want 1", diskErr.FreeBytes)
	}
	if diskErr.NeededBytes == 0 {
		t.Errorf("DiskEstimate.NeededBytes = 0, want >0 (5 embedded blocks)")
	}
}

func TestCreate_DiskPreFlight_FailClosedWhenCheckerErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-diskerr", 1024)
	seedBackend(t, pool, "backend-diskerr", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-diskerr"}})

	brokenDisk := func() (uint64, error) { return 0, errors.New("statfs: permission denied") }

	// Even with ZERO eligible blocks (estimate=0), a disk checker that
	// itself errors must fail closed — Create never proceeds "blind"
	// because the estimate happened to be trivially satisfiable.
	_, err := embedmigration.Create(ctx, pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-diskerr", ToBackend: "backend-diskerr",
	}, brokenDisk)
	if !errors.Is(err, embedmigration.ErrDiskCheckFailed) {
		t.Fatalf("Create error = %v, want ErrDiskCheckFailed", err)
	}
}

// TestStatfsChecker_RealPositivePath exercises the ACTUAL statfs(2) syscall
// (no injection) against a guaranteed-to-exist directory — the design §6.1
// positive path ("realer statfs … reicht als Positiv-Pfad"). t.TempDir()
// rather than a hardcoded host path (e.g. /compose) so this test is
// portable to CI runners that don't share this build session's filesystem
// layout; the real mount was additionally verified manually during the W04-3
// build (see wave report).
func TestStatfsChecker_RealPositivePath(t *testing.T) {
	free, err := embedmigration.StatfsChecker(t.TempDir())()
	if err != nil {
		t.Fatalf("StatfsChecker real statfs call: %v", err)
	}
	if free == 0 {
		t.Errorf("StatfsChecker reported 0 free bytes on a live filesystem — implausible, check Bavail/Bsize wiring")
	}
}

// pgVecLiteral1024 renders a constant-fill 1024-dim pgvector text literal —
// this file's leftover-data seed needs a real vector, not a NULL, and
// StoreEmbeddingNext does not exist yet (W04-4).
func pgVecLiteral1024(fill float32) string {
	s := "["
	for i := 0; i < 1024; i++ {
		if i > 0 {
			s += ","
		}
		s += "0.1"
	}
	_ = fill
	return s + "]"
}
