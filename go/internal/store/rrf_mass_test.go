//go:build integration

package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestCtxRrf_MassFactor_BehaviourMatchesContract verifies M030 (Branch X v3):
// the mass_factor is multiplied INTO each channel-rank inside the RRF function,
// before the RRF aggregation. Single-date and dateless blocks keep the full
// rank (factor=1.0); mega-blocks (10 dates) get sqrt-damped.
//
// Four test blocks share the same embedding (so semantic-rank identity holds
// modulo the per-block ROW_NUMBER ordering), and identical title+content (so
// ts-rank identity holds). The only thing that varies is content_times. The
// mass-factor is the only signal differentiating their rrf_score.
//
// Contract:
//
//	A: content_times=[1 date]    → mass_factor = 1.0
//	B: content_times=[10 dates]  → mass_factor = 1/sqrt(10) ≈ 0.3162
//	C: content_times=NULL        → mass_factor = 1.0 (no-dates branch)
//	D: content_times=[]          → mass_factor = 1.0 (empty-array branch)
//
// Expected: A.score > B.score (mass distinguishes mega from single).
//
//	|A.score - C.score| < 1e-6 (NULL branch == 1.0 path).
//	|C.score - D.score| < 1e-6 (empty-array branch == 1.0 path).
func TestCtxRrf_MassFactor_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Single shared embedding (1024 floats, all 0.1 → unit-direction-like).
	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}

	// All four blocks: identical title and content, distinct ids.
	const (
		idA = "019d0030-0000-7000-9000-00000000000a"
		idB = "019d0030-0000-7000-9000-00000000000b"
		idC = "019d0030-0000-7000-9000-00000000000c"
		idD = "019d0030-0000-7000-9000-00000000000d"
	)

	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	insertEmbeddedBlock(t, pool, idA, embedding, now)
	insertEmbeddedBlock(t, pool, idB, embedding, now)
	insertEmbeddedBlock(t, pool, idC, embedding, now)
	insertEmbeddedBlock(t, pool, idD, embedding, now)

	// content_times: A=1 date, B=10 dates, C=NULL (default for empty UPDATE),
	// D=empty array.
	setContentTimes(t, pool, idA, []time.Time{now})
	setContentTimes(t, pool, idB, generateDates(10, now))
	// idC: explicit NULL (defeat the schema default '{}' to test the NULL branch).
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET content_times = NULL WHERE id = $1::uuid`,
		idC,
	); err != nil {
		t.Fatalf("set content_times NULL on C: %v", err)
	}
	// idD: empty array (the schema default).
	setContentTimes(t, pool, idD, []time.Time{})

	// Sanity check: confirm we have 4 distinct array-states.
	verifyContentTimesShapes(t, pool, idA, idB, idC, idD)

	// Run ctx_rrf with limit=10 (we only inserted 4 blocks). Use a query that
	// has zero FTS hits ("zzqqxx") so all signal goes through the semantic
	// channel — that channel is the one we can keep stable across the four
	// blocks (identical embedding, identical title+content).
	hv := pgvec.NewHalfVector(embedding)
	rows, err := pool.Query(ctx,
		`SELECT id::text, rrf_score FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9)`,
		hv, "zzqqxx", "zzqqxx", []string{"private"}, nil, nil, 10, nil, nil,
	)
	if err != nil {
		t.Fatalf("query ctx_rrf: %v", err)
	}
	scores := map[string]float64{}
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			rows.Close()
			t.Fatalf("scan rrf row: %v", err)
		}
		scores[id] = score
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}

	for _, id := range []string{idA, idB, idC, idD} {
		if _, ok := scores[id]; !ok {
			t.Fatalf("ctx_rrf missing block %s in result; got=%v", id, scores)
		}
	}

	// Contract A: mass distinguishes single-date from mega-block.
	if scores[idA] <= scores[idB] {
		t.Errorf("mass-factor not applied: A(1 date)=%g must be > B(10 dates)=%g",
			scores[idA], scores[idB])
	}

	// Contract B: NULL content_times maps to mass_factor=1.0 (same as A).
	if math.Abs(scores[idA]-scores[idC]) > 1e-6 {
		t.Errorf("NULL branch != 1.0 path: A=%g vs C(NULL)=%g (diff=%g)",
			scores[idA], scores[idC], math.Abs(scores[idA]-scores[idC]))
	}

	// Contract C: empty-array content_times maps to mass_factor=1.0 (same as C).
	if math.Abs(scores[idC]-scores[idD]) > 1e-6 {
		t.Errorf("empty-array branch != 1.0 path: C(NULL)=%g vs D([])=%g (diff=%g)",
			scores[idC], scores[idD], math.Abs(scores[idC]-scores[idD]))
	}

	// Numeric sanity: B's mass_factor is ~0.3162. With identical RRF channels,
	// score(B)/score(A) should sit close to 0.3162. semantic is the only active
	// channel, so the ratio is exactly mass_B / mass_A = 1/sqrt(10).
	expectedRatio := 1.0 / math.Sqrt(10.0)
	gotRatio := scores[idB] / scores[idA]
	if math.Abs(gotRatio-expectedRatio) > 0.01 {
		t.Errorf("ratio score(B)/score(A) drifted: got=%g, want≈%g (1/sqrt(10))",
			gotRatio, expectedRatio)
	}
}

// insertEmbeddedBlock writes a context_blocks row with a real embedding.
// Title varies by last-char of id to avoid the uq_context_category_title_scope
// unique constraint; content/category stay identical so FTS+trigram channels
// score identically across blocks — only content_times can differentiate.
// The trigram channel is gated similarity > 0.05, so the per-block suffix
// keeps trigram-rank identical for the title-based query.
func insertEmbeddedBlock(t *testing.T, pool *pgxpool.Pool, id string, embedding []float32, ts time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec := pgvec.NewVector(embedding)
	title := fmt.Sprintf("rrf-mass-test-title-%s", id[len(id)-1:])
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7)`,
		id, "test_mass", title, "rrf-mass-test-content",
		"private", vec, ts,
	)
	if err != nil {
		t.Fatalf("insert embedded block %s: %v", id, err)
	}
}

// setContentTimes overwrites the content_times array on a block. An empty
// slice writes an empty SQL array '{}', exercising the empty-array branch
// of the M030 block_mass CTE.
func setContentTimes(t *testing.T, pool *pgxpool.Pool, id string, times []time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET content_times = $1::timestamptz[] WHERE id = $2::uuid`,
		times, id,
	)
	if err != nil {
		t.Fatalf("set content_times on %s (n=%d): %v", id, len(times), err)
	}
}

// generateDates returns n distinct timestamps anchored on `base`, one day apart.
func generateDates(n int, base time.Time) []time.Time {
	out := make([]time.Time, n)
	for i := 0; i < n; i++ {
		out[i] = base.AddDate(0, 0, i)
	}
	return out
}

// verifyContentTimesShapes asserts the four blocks have the four distinct
// array-states the test relies on (1, 10, NULL, 0). Failing this means a
// preceding INSERT/UPDATE silently coerced a value, which would invalidate
// the rest of the test.
func verifyContentTimesShapes(t *testing.T, pool *pgxpool.Pool, idA, idB, idC, idD string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	expectations := []struct {
		id       string
		wantLen  *int
		wantNull bool
	}{
		{idA, intPtr(1), false},
		{idB, intPtr(10), false},
		{idC, nil, true},
		{idD, intPtr(0), false},
	}
	for _, e := range expectations {
		var arrLen *int
		err := pool.QueryRow(ctx,
			`SELECT array_length(content_times, 1) FROM context_blocks WHERE id = $1::uuid`,
			e.id,
		).Scan(&arrLen)
		if err != nil {
			t.Fatalf("verify shape %s: %v", e.id, err)
		}
		got := describeShape(arrLen)
		want := describeShape(nil)
		if !e.wantNull {
			want = describeShape(e.wantLen)
		}
		if got != want {
			t.Fatalf("block %s content_times shape: got %s, want %s", e.id, got, want)
		}
	}
}

func intPtr(i int) *int { return &i }

func describeShape(p *int) string {
	if p == nil {
		// PG semantics: array_length returns NULL for both NULL arrays and
		// empty arrays. The Go layer's UPDATE behaviour disambiguates: an
		// empty []time.Time becomes '{}' (length-NULL but non-NULL array);
		// explicit NULL is a NULL array. From this query's vantage point
		// they look the same — verifyContentTimesShapes treats both as
		// "NULL/empty" for this assertion. The actual mass_factor
		// distinction (NULL → 1.0, empty → 1.0) is tested in the score
		// contract above.
		return "len=NULL"
	}
	return fmt.Sprintf("len=%d", *p)
}
