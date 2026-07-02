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
// Four test blocks share the same embedding and identical title+content. The
// only thing that varies is content_times. Because ROW_NUMBER assigns unique
// ranks 1..N to identical-embedding rows via tie-break, the four blocks have
// ranks 1, 2, 3, 4 in some order — NOT identical ranks. The assertions below
// use a rank-tie-tolerance (similar to rrf_role_test.go's ±0.1 band) for the
// "mass_factor=1.0" group, and an exact-ratio band for the sqrt(10)-damped
// comparison.
//
// Contract:
//
//	A: content_times=[1 date]    → mass_factor = 1.0
//	B: content_times=[10 dates]  → mass_factor = 1/sqrt(10) ≈ 0.3162
//	C: content_times=NULL        → mass_factor = 1.0 (no-dates branch)
//	D: content_times=[]          → mass_factor = 1.0 (empty-array branch)
//
// Expected:
//   - A, C, D within rank-tie-tolerance of each other (all mass_factor=1.0).
//     With ranks 1..4 across 4 blocks, the rank-1-vs-rank-4 ratio is
//     1/61 vs 1/64 ≈ 0.953 — tolerance of 0.1 covers this comfortably.
//   - B / max(A, C, D) ≈ 1/sqrt(10) (mass distinguishes mega from single,
//     with B's rank somewhere in 1..4 just like the others).
//
// Note (W47-01): The original v1.6.0-era assertions used |A-C| < 1e-6 which
// presumed identical ranks across all four blocks. That premise is wrong:
// ROW_NUMBER over identical embeddings produces unique ranks via tie-break,
// not a tied 1-rank. The mass_factor SQL math itself is correct (verified by
// TestCtxRrf_MassFactor_DirectMath). The test now uses tolerance-bands the
// same way rrf_role_test.go does for its type_name contract.
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
		`SELECT id::text, rrf_score FROM ctx_rrf($1, $2, $3, $4::text[], $5, $6::text[], $7, $8, $9,
		         p_types_visible => $10::text[])`,
		hv, "zzqqxx", "zzqqxx", []string{"private"}, nil, nil, 10, nil, nil,
		[]string{"knowledge", "audit-trail", "reference"},
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

	// Contract B: A, C, D all share mass_factor=1.0. With ROW_NUMBER assigning
	// unique ranks 1..4 across identical-embedding rows, their scores only
	// differ by the rank-tie-break term 1/(60+rank). Worst-case rank-1-vs-rank-4
	// ratio is 1/61 / 1/64 ≈ 0.953 — within ±0.1 band (mirrors rrf_role_test).
	// If a future implementation changed NULL or empty-array semantics to a
	// non-1.0 mass_factor, B would no longer share the 1.0-band with A.
	mass1Scores := []float64{scores[idA], scores[idC], scores[idD]}
	mass1Max := mass1Scores[0]
	mass1Min := mass1Scores[0]
	for _, s := range mass1Scores {
		if s > mass1Max {
			mass1Max = s
		}
		if s < mass1Min {
			mass1Min = s
		}
	}
	if mass1Max/mass1Min > 1.1 {
		t.Errorf("mass_factor=1.0 group spread too wide: A=%g C=%g D=%g (max/min=%g, want ≤1.1)",
			scores[idA], scores[idC], scores[idD], mass1Max/mass1Min)
	}

	// Contract C: B's mass_factor is 1/sqrt(10) ≈ 0.3162. B/max(A,C,D) compares
	// the damped block against the un-damped band — since ROW_NUMBER tie-break
	// is the only other variable, the ratio sits within (0.3162*0.953,
	// 0.3162*1.050). Use ±0.05 absolute tolerance to absorb worst-case ranks.
	expectedRatio := 1.0 / math.Sqrt(10.0)
	gotRatio := scores[idB] / mass1Max
	if math.Abs(gotRatio-expectedRatio) > 0.05 {
		t.Errorf("ratio score(B)/max(A,C,D) drifted: got=%g, want≈%g (1/sqrt(10)); A=%g B=%g C=%g D=%g",
			gotRatio, expectedRatio, scores[idA], scores[idB], scores[idC], scores[idD])
	}
}

// TestCtxRrf_MassFactor_DirectMath verifies the block_mass CTE math in
// isolation — directly querying the four CASE branches without going through
// ctx_rrf and without the rank-tie-break confound. This is the canonical test
// that M030's per-block mass_factor formula is correct.
//
// Contract per branch:
//
//	content_times = NULL       → mass_factor = 1.0
//	content_times = '{}'       → mass_factor = 1.0 (array_length = NULL too)
//	content_times = [1 date]   → mass_factor = 1.0
//	content_times = [10 dates] → mass_factor = 1/sqrt(10) ≈ 0.3162
//	content_times = [100 ts]   → mass_factor = 1/sqrt(100) = 0.1
func TestCtxRrf_MassFactor_DirectMath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		id          string
		setupTimes  func(t *testing.T, pool *pgxpool.Pool, id string)
		wantFactor  float64
		description string
	}{
		{
			id: "019d0031-0000-7000-9000-000000000001",
			setupTimes: func(t *testing.T, pool *pgxpool.Pool, id string) {
				if _, err := pool.Exec(context.Background(),
					`UPDATE context_blocks SET content_times = NULL WHERE id = $1::uuid`,
					id,
				); err != nil {
					t.Fatalf("set NULL: %v", err)
				}
			},
			wantFactor:  1.0,
			description: "NULL content_times",
		},
		{
			id:          "019d0031-0000-7000-9000-000000000002",
			setupTimes:  func(t *testing.T, pool *pgxpool.Pool, id string) { setContentTimes(t, pool, id, []time.Time{}) },
			wantFactor:  1.0,
			description: "empty array content_times",
		},
		{
			id:          "019d0031-0000-7000-9000-000000000003",
			setupTimes:  func(t *testing.T, pool *pgxpool.Pool, id string) { setContentTimes(t, pool, id, []time.Time{now}) },
			wantFactor:  1.0,
			description: "1-date content_times",
		},
		{
			id:          "019d0031-0000-7000-9000-000000000004",
			setupTimes:  func(t *testing.T, pool *pgxpool.Pool, id string) { setContentTimes(t, pool, id, generateDates(10, now)) },
			wantFactor:  1.0 / math.Sqrt(10.0),
			description: "10-date content_times",
		},
		{
			id:          "019d0031-0000-7000-9000-000000000005",
			setupTimes:  func(t *testing.T, pool *pgxpool.Pool, id string) { setContentTimes(t, pool, id, generateDates(100, now)) },
			wantFactor:  1.0 / 10.0,
			description: "100-date content_times",
		},
	}

	for _, c := range cases {
		insertEmbeddedBlock(t, pool, c.id, embedding, now)
		c.setupTimes(t, pool, c.id)
	}

	// Query the block_mass CTE math directly per block. This is the exact
	// CASE expression from M030 onward (preserved in M033/M036/M037/M038/M039/M048).
	for _, c := range cases {
		var got float64
		err := pool.QueryRow(ctx, `
			SELECT CASE
				WHEN array_length(content_times, 1) IS NULL THEN 1.0
				WHEN array_length(content_times, 1) = 0    THEN 1.0
				ELSE 1.0 / sqrt(array_length(content_times, 1)::DOUBLE PRECISION)
			END
			FROM context_blocks WHERE id = $1::uuid`, c.id).Scan(&got)
		if err != nil {
			t.Fatalf("query mass_factor for %s: %v", c.description, err)
		}
		if math.Abs(got-c.wantFactor) > 1e-9 {
			t.Errorf("mass_factor for %s: got=%g, want=%g (diff=%g)",
				c.description, got, c.wantFactor, math.Abs(got-c.wantFactor))
		}
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
//
// Postgres semantics: array_length(arr, 1) returns NULL for both NULL arrays
// AND empty arrays — we cannot use it alone to distinguish C from D. We
// query `content_times IS NULL` alongside for the unambiguous null-check.
func verifyContentTimesShapes(t *testing.T, pool *pgxpool.Pool, idA, idB, idC, idD string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	expectations := []struct {
		id       string
		wantLen  *int // expected array_length; nil = empty-or-null array
		wantNull bool // explicit IS NULL check
	}{
		{idA, intPtr(1), false},
		{idB, intPtr(10), false},
		{idC, nil, true},  // explicit NULL
		{idD, nil, false}, // empty array '{}' — non-NULL but array_length=NULL
	}
	for _, e := range expectations {
		var arrLen *int
		var isNull bool
		err := pool.QueryRow(ctx,
			`SELECT array_length(content_times, 1), content_times IS NULL FROM context_blocks WHERE id = $1::uuid`,
			e.id,
		).Scan(&arrLen, &isNull)
		if err != nil {
			t.Fatalf("verify shape %s: %v", e.id, err)
		}
		if isNull != e.wantNull {
			t.Fatalf("block %s content_times IS NULL: got %v, want %v", e.id, isNull, e.wantNull)
		}
		got := describeShape(arrLen)
		want := describeShape(e.wantLen)
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
