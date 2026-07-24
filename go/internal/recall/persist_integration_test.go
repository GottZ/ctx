//go:build integration

// Integration coverage for Achse 01 W01-1 (context_recall_runs persistence,
// migration 110): schema shape, the Insert/read-back round trip, the
// allowlist leak guard surviving an actual DB write attempt, LatestByStratum
// grouping, and the uuidv7() PK default. W01-1 ships no caller — these tests
// exercise the package directly, the same way persist.go will be driven once
// W01-2 lands the probe mechanics.
//
// Run with:
//
//	go test -tags=integration ./internal/recall/ -count=1 -v
package recall_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

func TestRecallRunsSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) Table exists after the fresh-DB migration chain.
	var tableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'context_recall_runs')`,
	).Scan(&tableExists); err != nil {
		t.Fatalf("probe table existence: %v", err)
	}
	if !tableExists {
		t.Fatal("context_recall_runs does not exist after migrations")
	}

	// Both indexes exist (pg_indexes probe).
	wantIdx := []string{"idx_recall_runs_ran_at", "idx_recall_runs_stratum"}
	for _, idx := range wantIdx {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx,
		).Scan(&exists); err != nil {
			t.Fatalf("probe index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("index %s does not exist after migrations", idx)
		}
	}
}

func TestInsertAndReadBack(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	group := uuid.NewString()
	run := recall.Run{
		RunGroup:       group,
		Stratum:        "medium",
		Scope:          ptr("private"),
		CorpusEmbedded: 42000,
		K:              10,
		NQueries:       500,
		QuerySource:    "log",
		EfSearch:       40,
		IterativeScan:  "off",
		Valid:          true,
		RecallAvg:      ptr(0.987),
		RecallMin:      ptr(0.62),
		AnnMsP50:       ptr(3.1),
		AnnMsP95:       ptr(9.4),
		ExactMsP50:     ptr(85.0),
		Meta: map[string]any{
			"pgvector_version": "0.8.0",
			"embed_model":      "qwen3-embedding:8b-ctx2k",
			"epsilon":          0.02,
		},
	}
	if err := recall.Insert(ctx, pool, run); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		gotStratum        string
		gotScope          *string
		gotCorpusEmbedded int
		gotK              int16
		gotValid          bool
		gotRecallAvg      *float64
		gotMeta           map[string]any
	)
	err := pool.QueryRow(ctx,
		`SELECT stratum, scope, corpus_embedded, k, valid, recall_avg, meta
		   FROM context_recall_runs WHERE run_group = $1`, group,
	).Scan(&gotStratum, &gotScope, &gotCorpusEmbedded, &gotK, &gotValid, &gotRecallAvg, &gotMeta)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotStratum != "medium" {
		t.Errorf("stratum = %q, want %q", gotStratum, "medium")
	}
	if gotScope == nil || *gotScope != "private" {
		t.Errorf("scope = %v, want \"private\"", gotScope)
	}
	if gotCorpusEmbedded != 42000 {
		t.Errorf("corpus_embedded = %d, want 42000", gotCorpusEmbedded)
	}
	if gotK != 10 {
		t.Errorf("k = %d, want 10", gotK)
	}
	if !gotValid {
		t.Error("valid = false, want true")
	}
	if gotRecallAvg == nil || *gotRecallAvg != 0.987 {
		t.Errorf("recall_avg = %v, want 0.987", gotRecallAvg)
	}
	if gotMeta["embed_model"] != "qwen3-embedding:8b-ctx2k" {
		t.Errorf("meta[embed_model] = %v, want qwen3-embedding:8b-ctx2k", gotMeta["embed_model"])
	}
}

func TestInsertRejectsUnknownMetaKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_recall_runs`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}

	run := recall.Run{
		RunGroup:      uuid.NewString(),
		Stratum:       "small",
		K:             10,
		NQueries:      50,
		QuerySource:   "loo",
		EfSearch:      0,
		IterativeScan: "off",
		Valid:         true,
		Meta: map[string]any{
			"sample_texts": "this is a leaked query text",
		},
	}
	err := recall.Insert(ctx, pool, run)
	if err == nil {
		t.Fatal("expected Insert to reject the unknown meta key sample_texts")
	}
	if !strings.Contains(err.Error(), "sample_texts") {
		t.Errorf("error should name the offending key, got: %v", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_recall_runs`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("row count changed from %d to %d — rejected meta must not reach the DB", before, after)
	}
}

func TestLatestByStratum(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	base := recall.Run{
		Stratum:       "large",
		Scope:         ptr("shared"),
		K:             10,
		NQueries:      1000,
		QuerySource:   "mixed",
		EfSearch:      40,
		IterativeScan: "off",
		Valid:         true,
	}

	insertAt := func(r recall.Run, group string, avg float64) {
		r.RunGroup = group
		r.RecallAvg = ptr(avg)
		if err := recall.Insert(ctx, pool, r); err != nil {
			t.Fatalf("insert: %v", err)
		}
		// ran_at defaults to now(); force separation so ORDER BY ran_at DESC
		// is unambiguous instead of racing on clock resolution.
		if _, err := pool.Exec(ctx,
			`UPDATE context_recall_runs SET ran_at = $2 WHERE run_group = $1`,
			group, time.Now().Add(time.Duration(avg*1e9)*time.Nanosecond),
		); err != nil {
			t.Fatalf("backdate ran_at: %v", err)
		}
	}

	// Group A: three rows, same (stratum, scope, k), increasing recall_avg
	// used as a monotonic ran_at proxy — 0.99 is the newest.
	insertAt(base, uuid.NewString(), 0.90)
	insertAt(base, uuid.NewString(), 0.95)
	newestA := uuid.NewString()
	insertAt(base, newestA, 0.99)

	// Group B: a distinct (stratum,scope,k) — different k.
	groupB := base
	groupB.K = 75
	newestB := uuid.NewString()
	insertAt(groupB, newestB, 0.80)

	got, err := recall.LatestByStratum(ctx, pool, 100)
	if err != nil {
		t.Fatalf("LatestByStratum: %v", err)
	}

	var sawA, sawB bool
	for _, r := range got {
		if r.RunGroup == newestA {
			sawA = true
		}
		if r.RunGroup == newestB {
			sawB = true
		}
		// Ensure no stale row from group A (0.90 / 0.95) leaked through.
		if r.Stratum == "large" && r.K == 10 && r.RunGroup != newestA &&
			r.Scope != nil && *r.Scope == "shared" {
			t.Errorf("stale row for (large,shared,10) returned: run_group=%s recall_avg=%v", r.RunGroup, r.RecallAvg)
		}
	}
	if !sawA {
		t.Error("expected the newest row of group A (k=10) in the result")
	}
	if !sawB {
		t.Error("expected the newest row of group B (k=75) in the result")
	}
}

func TestIDDefaultsToUUIDv7(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	group := uuid.NewString()
	run := recall.Run{
		RunGroup:      group,
		Stratum:       "all",
		K:             10,
		NQueries:      1,
		QuerySource:   "log",
		EfSearch:      0,
		IterativeScan: "off",
		Valid:         false,
		Meta:          map[string]any{"invalid_reason": "budget_exhausted"},
	}
	if err := recall.Insert(ctx, pool, run); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM context_recall_runs WHERE run_group = $1`, group,
	).Scan(&id); err != nil {
		t.Fatalf("read id: %v", err)
	}
	if id == "" {
		t.Fatal("id is empty — uuidv7() default did not fire")
	}
	// UUID version nibble sits at the first char of the 3rd group
	// (xxxxxxxx-xxxx-Vxxx-...). Version 7 => '7'.
	parts := strings.Split(id, "-")
	if len(parts) != 5 || len(parts[2]) == 0 || parts[2][0] != '7' {
		t.Errorf("id %q does not carry a version-7 nibble, want xxxxxxxx-xxxx-7xxx-...", id)
	}
}
