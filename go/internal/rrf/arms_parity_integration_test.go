//go:build integration

// B-W1 gates for migration 137_rrf_arms.sql (Achse 04 §4.2): ctx_rrf_arms is
// the per-arm rank sister of ctx_rrf. It runs the same prologue, the same four
// arms and the same block_mass/type_factor CTEs, but stops before the weight
// expression — it projects raw ranks plus the two multiplicative factors and
// no content at all.
//
// The claim that makes it useful is a strong one: that the ranks are a
// FAITHFUL decomposition of the live score. Nothing about a duplicated
// function body guarantees that, and a comment saying "copied from 134" ages
// badly. So the gate proves it arithmetically — the fusion is recomputed in Go
// from the arm ranks (fuseArms, arms_fusion_test.go) and compared row for row
// against ctx_rrf on the same fixture, in the SAME transaction, over 54
// generated queries in both semantic modes.
//
// Historic RED probe, run against the tree with 137_rrf_arms.sql moved out of
// go/migrations/ (go:embed only carries files present at compile time), so the
// chain ends at 136:
//
//	q00: ctx_rrf_arms call: ERROR: function ctx_rrf_arms(unknown, text,
//	text, text[], text, text[], integer, text, text, text[], text[], double
//	precision[], text[], text[], uuid[], text, integer, integer) does not
//	exist (SQLSTATE 42883)
//
// The same 42883 on all four gates; the fixture itself was already green
// (220 blocks, types map[audit-trail:31 checkpoint:31 knowledge:96
// reference:62], archived=10, null_embedding=17, one granted foreign-scope
// block), so the red was the missing function and nothing else.
//
// Tx-seam note: both calls must sit in ONE transaction. The ann arm sets
// hnsw.iterative_scan via SET LOCAL, which — in a function without its own SET
// clause — lasts to the end of the transaction, not the end of the call. Split
// across two transactions the two functions would search different ANN
// candidate spaces and the parity measurement would be quietly wrong. Gate (e)
// pins that the GUC really travels.
//
//	go test -tags=integration ./internal/rrf/ -run TestBW1Arms -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	bw1ScopeA       = "bw1a"  // primary read scope
	bw1ScopeB       = "bw1b"  // second read scope
	bw1ScopeForeign = "bw1x"  // NOT in p_scopes; reachable only via a grant
	bw1Blocks       = 220     // > 200, per the brief
	bw1Seed         = 0x5177f // fixed: the whole fixture must be reproducible
	bw1ExactCap     = 5000    // comfortably above the fixture, so the TOCTOU guard passes
	bw1Limit        = 20      // p_limit for the fusion parity gate
	bw1BigLimit     = 100000  // "all candidates" for the visibility gate
	bw1Eps          = 1e-12   // score tolerance, per the brief
)

// bw1VisibleTypes is the allowlist under test. `checkpoint` is deliberately
// absent (retrieval policy 'excluded'), so the fixture's checkpoint blocks are
// the negative probe's prey.
var bw1VisibleTypes = []string{"knowledge", "reference", "audit-trail"}

var (
	bw1DampedTypes   = []string{"audit-trail"}
	bw1DampedFactors = []float64{0.3}
)

// bw1Types cycles four block types across the fixture: two full-pass, one
// damped (audit-trail → type_factor 0.3) and one excluded (checkpoint). The
// length is 7 and the scope pattern below has period 10 — deliberately
// coprime, so every type occurs in every scope instead of the type being a
// function of the scope (which would leave the grant arm without a visible
// candidate).
var bw1Types = []string{
	"knowledge", "knowledge", "knowledge",
	"reference", "reference",
	"audit-trail",
	"checkpoint",
}

// bw1WordsDE / bw1WordsEN feed both the block texts and the queries, so all
// four arms (german FTS, english FTS, trigram over the title, semantic) have
// real hits instead of an empty CTE that would make the parity vacuous.
var bw1WordsDE = []string{"Datenbank", "Migration", "Funktion", "Sichtbarkeit", "Abfrage", "Speicher", "Vektor", "Fusion", "Gewicht", "Rangfolge"}
var bw1WordsEN = []string{"database", "migration", "function", "visibility", "query", "storage", "vector", "fusion", "weight", "ranking"}

// bw1FixedQuery is the single query the set-comparison gates (b)-(e) use. Two
// words, one per language, so that all four arms contribute to the candidate
// set rather than the set being the semantic arm alone.
const bw1FixedQuery = "Datenbank function"

type bw1Fixture struct {
	granted  []string // one foreign-scope block id, reachable only via p_granted_block_ids
	types    map[string]string
	scopes   map[string]string
	archived map[string]bool
}

// bw1SeedCorpus writes 220 blocks across three scopes and four types, with
// archived rows, NULL embeddings, content_times (mass_factor != 1), German and
// English text, and deterministic pseudo-embeddings from a fixed seed.
func bw1SeedCorpus(t *testing.T, pool *pgxpool.Pool) bw1Fixture {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(bw1Seed))
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	fx := bw1Fixture{types: map[string]string{}, scopes: map[string]string{}, archived: map[string]bool{}}
	var archived, nullEmb, byType = 0, 0, map[string]int{}

	for i := 0; i < bw1Blocks; i++ {
		id := fmt.Sprintf("019fa402-0000-7000-9000-0000000%05d", 50000+i)

		scope := bw1ScopeA
		switch {
		case i%10 == 9:
			scope = bw1ScopeForeign
		case i%10 >= 7:
			scope = bw1ScopeB
		}
		typeName := bw1Types[i%len(bw1Types)]
		isArchived := i%23 == 0

		de := bw1WordsDE[i%len(bw1WordsDE)]
		de2 := bw1WordsDE[(i*3+1)%len(bw1WordsDE)]
		en := bw1WordsEN[(i*7+2)%len(bw1WordsEN)]
		en2 := bw1WordsEN[(i*5+4)%len(bw1WordsEN)]
		title := fmt.Sprintf("%s %s block %03d %s", de, de2, i, en)
		content := fmt.Sprintf("Der %s in der %s beschreibt die %s. The %s of the %s explains the %s. block %03d",
			de, de2, bw1WordsDE[(i*11+5)%len(bw1WordsDE)], en, en2, bw1WordsEN[(i*13+6)%len(bw1WordsEN)], i)

		category := "alpha"
		if i%2 == 1 {
			category = "beta"
		}
		var tags []string
		if i%4 == 0 {
			tags = []string{"tagx"}
		}

		// Deterministic pseudo-embedding; every 13th block has none at all
		// (Gen 16 drops those from the semantic arm entirely).
		var embParam interface{}
		if i%13 != 0 {
			e := make([]float32, 1024)
			for k := range e {
				e[k] = float32(rng.Float64())
			}
			embParam = pgvec.NewVector(e)
		} else {
			nullEmb++
			// Keep the RNG stream independent of the NULL pattern.
			for k := 0; k < 1024; k++ {
				rng.Float64()
			}
		}

		// content_times drives block_mass: 1/sqrt(n) for n entries.
		var times []time.Time
		if i%6 == 0 {
			for k := 0; k < (i%4)+2; k++ {
				times = append(times, base.Add(time.Duration(k)*time.Hour))
			}
		}

		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks
				(id, category, title, content, scope, embedding, created_at, updated_at,
				 type_name, is_archived, tags, content_times)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, $8, $9, $10::text[], $11::timestamptz[])`,
			id, category, title, content, scope, embParam, base, typeName, isArchived, tags, times,
		); err != nil {
			t.Fatalf("insert fixture block %d (%s): %v", i, id, err)
		}

		fx.types[id] = typeName
		fx.scopes[id] = scope
		fx.archived[id] = isArchived
		byType[typeName]++
		if isArchived {
			archived++
		}
		if scope == bw1ScopeForeign && len(fx.granted) == 0 && typeName == "knowledge" && !isArchived {
			fx.granted = append(fx.granted, id)
		}
	}
	if len(fx.granted) == 0 {
		t.Fatal("fixture produced no grantable foreign-scope block — the grant arm would be untested")
	}
	t.Logf("fixture: %d blocks, types=%v, archived=%d, null_embedding=%d, granted=%v",
		bw1Blocks, byType, archived, nullEmb, fx.granted)
	return fx
}

// ---------------------------------------------------------------------------
// Call surface
// ---------------------------------------------------------------------------

// bw1Query is one generated fixture query: the 18-argument surface both
// functions share.
type bw1Query struct {
	name         string
	text         string
	spaced       string
	or           interface{}
	temporal     interface{}
	category     interface{}
	tags         []string
	typesExclude []string
	mode         string
}

// bw1Queries builds 54 deterministic queries — every one of them exercised in
// both functions. Roughly a third run in exact mode; the rest in ann.
func bw1Queries() []bw1Query {
	var qs []bw1Query
	for i := 0; i < 54; i++ {
		// Two words only. plainto_tsquery ANDs every lexeme, so a longer
		// query (a block number, say) would starve both full-text arms and
		// make the parity vacuous for two of the four channels — the arm
		// census at the end of gate (a) is what keeps that honest.
		de := bw1WordsDE[i%len(bw1WordsDE)]
		en := bw1WordsEN[(i*3+1)%len(bw1WordsEN)]
		text := fmt.Sprintf("%s %s", de, en)
		q := bw1Query{
			name:   fmt.Sprintf("q%02d", i),
			text:   text,
			spaced: strings.Join(strings.Fields(text), "  "),
			mode:   "ann",
		}
		if i%3 == 2 {
			q.mode = "exact"
		}
		if i%5 == 0 {
			q.or = bw1WordsEN[(i*7)%len(bw1WordsEN)]
		}
		if i%9 == 4 {
			q.temporal = "2026"
		}
		if i%7 == 3 {
			q.category = "alpha"
		}
		if i%11 == 5 {
			q.tags = []string{"tagx"}
		}
		if i%13 == 6 {
			q.typesExclude = []string{"reference"}
		}
		qs = append(qs, q)
	}
	return qs
}

// bw1Args assembles the 18 positional arguments shared by ctx_rrf and
// ctx_rrf_arms. One slice, used verbatim for both calls — that identity is
// part of what the gate asserts.
func bw1Args(q bw1Query, emb []float32, granted []string, limit int) []any {
	var tagsParam, excludeParam, grantedParam interface{}
	if len(q.tags) > 0 {
		tagsParam = q.tags
	}
	if len(q.typesExclude) > 0 {
		excludeParam = q.typesExclude
	}
	if len(granted) > 0 {
		grantedParam = granted
	}
	var exactCap interface{}
	if q.mode == "exact" {
		exactCap = bw1ExactCap
	}
	return []any{
		pgvNewHalfVec(emb),             // 1  p_embedding
		q.text,                         // 2  p_query
		q.spaced,                       // 3  p_query_spaced
		[]string{bw1ScopeA, bw1ScopeB}, // 4  p_scopes
		q.category,                     // 5  p_category
		tagsParam,                      // 6  p_tags
		limit,                          // 7  p_limit
		q.temporal,                     // 8  p_temporal
		q.or,                           // 9  p_query_or
		bw1VisibleTypes,                // 10 p_types_visible
		bw1DampedTypes,                 // 11 p_damped_types
		bw1DampedFactors,               // 12 p_damped_factors
		nil,                            // 13 p_categories_exclude
		excludeParam,                   // 14 p_types_exclude
		grantedParam,                   // 15 p_granted_block_ids
		q.mode,                         // 16 p_semantic_mode
		nil,                            // 17 p_scan_tuples
		exactCap,                       // 18 p_exact_cap
	}
}

// bw1ArgList is the $1..$18 placeholder list with explicit casts, so a NULL
// argument never leaves pgx guessing a type.
const bw1ArgList = `$1, $2::text, $3::text, $4::text[], $5::text, $6::text[], $7::int, $8::text, $9::text,
	$10::text[], $11::text[], $12::double precision[], $13::text[], $14::text[], $15::uuid[],
	$16::text, $17::int, $18::int`

// bw1CallRRF runs the live fusion function.
func bw1CallRRF(ctx context.Context, q pgx.Tx, args []any) ([]g15Row, error) {
	rows, err := q.Query(ctx, `SELECT id, rrf_score, cosine_sim FROM ctx_rrf(`+bw1ArgList+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []g15Row
	for rows.Next() {
		var r g15Row
		if err := rows.Scan(&r.id, &r.score, &r.cos); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// bw1CallArms runs the per-arm sister with the SAME 18 arguments. Extra
// parameters (19-23) are left at their defaults, which are the Ist literals of
// ctx_rrf — the default call must therefore be the identical candidate space.
func bw1CallArms(ctx context.Context, q pgx.Tx, fn string, args []any, extra string) ([]armRow, error) {
	sql := fmt.Sprintf(
		`SELECT id, rank_semantic, rank_fts_de, rank_fts_en, rank_trigram, cos_sim, mass_factor, type_factor
		 FROM %s(%s%s)`, fn, bw1ArgList, extra)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []armRow
	for rows.Next() {
		var r armRow
		if err := rows.Scan(&r.ID, &r.Semantic, &r.FtsDE, &r.FtsEN, &r.Trigram, &r.Cos, &r.Mass, &r.Type); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// bw1Embedding is the deterministic query vector; k shifts it so different
// queries reach different neighbourhoods.
func bw1Embedding(k int) []float32 {
	rng := rand.New(rand.NewSource(int64(bw1Seed) + int64(k)))
	e := make([]float32, 1024)
	for i := range e {
		e[i] = float32(rng.Float64())
	}
	return e
}

// ---------------------------------------------------------------------------
// Gate (a): fusion parity
// ---------------------------------------------------------------------------

// TestBW1ArmsFusionParity is the load-bearing gate. For every generated query
// it calls ctx_rrf and ctx_rrf_arms with identical arguments inside ONE
// transaction, refuses the fusion offline from the arm ranks, and requires the
// two to agree on ids, order, score (|delta| < 1e-12) and cosine.
//
// Ties: ctx_rrf orders by score alone, with no tiebreak, so two candidates
// with an identical score come back in an unspecified order. That is a
// property of Generation 16, not of this migration — the gate therefore
// compares tie groups as SETS and reports the tie population as a finding of
// its own rather than silently tolerating it or inventing a tiebreak here.
func TestBW1ArmsFusionParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	var (
		queriesWithTies int
		tieGroupSizes   []int
		boundaryTies    int
		totalCandidates int
		totalDelivered  int
		armCensus       [4]int // semantic, fts_de, fts_en, trigram
	)
	maxDelta := 0.0

	for qi, q := range bw1Queries() {
		emb := bw1Embedding(qi)
		args := bw1Args(q, emb, fx.granted, bw1Limit)

		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", q.name, err)
		}

		live, err := bw1CallRRF(ctx, tx, args)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: ctx_rrf call: %v", q.name, err)
		}
		arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, "")
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: ctx_rrf_arms call: %v", q.name, err)
		}
		_ = tx.Rollback(ctx)

		totalCandidates += len(arms)
		totalDelivered += len(live)
		for _, a := range arms {
			for k, p := range []*int{a.Semantic, a.FtsDE, a.FtsEN, a.Trigram} {
				if p != nil {
					armCensus[k]++
				}
			}
		}

		want := fuseArms(arms, armsLiveWeights, armsRRFK)
		n := bw1Limit
		if len(want) < n {
			n = len(want)
		}
		if len(live) != n {
			t.Fatalf("%s: ctx_rrf returned %d rows, offline fusion of %d candidates expects %d",
				q.name, len(live), len(arms), n)
		}

		// Score sequence: ties or not, the SCORES must line up exactly.
		for i := 0; i < n; i++ {
			d := math.Abs(live[i].score - want[i].Score)
			if d > maxDelta {
				maxDelta = d
			}
			if d > bw1Eps {
				t.Errorf("%s position %d: ctx_rrf score = %.17g, offline fusion = %.17g (delta %.3g > %g)",
					q.name, i, live[i].score, want[i].Score, d, bw1Eps)
			}
		}

		// Id/order comparison, tie-group aware.
		groups := armsTieGroups(want, bw1Eps)
		hasTie := false
		for _, g := range groups {
			if g[0] >= n {
				break
			}
			if g[1]-g[0] > 1 {
				hasTie = true
				tieGroupSizes = append(tieGroupSizes, g[1]-g[0])
			}
			end := g[1]
			partial := false
			if end > n {
				end, partial = n, true
			}
			wantSet := map[string]bool{}
			for i := g[0]; i < g[1]; i++ {
				wantSet[want[i].ID] = true
			}
			gotSet := map[string]bool{}
			for i := g[0]; i < end; i++ {
				gotSet[live[i].id] = true
				if !wantSet[live[i].id] {
					t.Errorf("%s position %d: ctx_rrf delivered %s, which is not in the offline score group %v (score %.17g)",
						q.name, i, live[i].id, want[g[0]:g[1]], live[i].score)
				}
			}
			if partial {
				boundaryTies++
			} else if len(gotSet) != len(wantSet) {
				t.Errorf("%s score group [%d,%d): ctx_rrf delivered %d distinct ids, offline fusion has %d",
					q.name, g[0], g[1], len(gotSet), len(wantSet))
			}
		}
		if hasTie {
			queriesWithTies++
		}

		// Cosine parity: the value ctx_rrf projects is the semantic arm's own.
		armCos := map[string]*float64{}
		for _, a := range arms {
			armCos[a.ID] = a.Cos
		}
		for i := 0; i < n; i++ {
			ac, ok := armCos[live[i].id]
			if !ok {
				t.Errorf("%s position %d: id %s is absent from ctx_rrf_arms entirely", q.name, i, live[i].id)
				continue
			}
			switch {
			case live[i].cos == nil && ac == nil:
			case live[i].cos == nil || ac == nil:
				t.Errorf("%s position %d (%s): cosine NULL-ness differs — ctx_rrf %v, arms %v",
					q.name, i, live[i].id, live[i].cos, ac)
			case math.Abs(*live[i].cos-*ac) > bw1Eps:
				t.Errorf("%s position %d (%s): cosine %.17g vs %.17g", q.name, i, live[i].id, *live[i].cos, *ac)
			}
		}
	}

	sort.Ints(tieGroupSizes)
	t.Logf("gate (a): 54 queries, %d fused candidates, %d delivered rows, max |score delta| = %.3g (tolerance %g)",
		totalCandidates, totalDelivered, maxDelta, bw1Eps)
	// Arm census: a channel that never produced a rank would make the parity
	// claim vacuous for that channel's weight, and the gate would pass on a
	// body whose fts_en arm was deleted outright.
	t.Logf("gate (a) arm census (ranked rows across all 54 queries): semantic=%d fts_de=%d fts_en=%d trigram=%d",
		armCensus[0], armCensus[1], armCensus[2], armCensus[3])
	for k, name := range []string{"semantic", "fts_de", "fts_en", "trigram"} {
		if armCensus[k] == 0 {
			t.Errorf("arm %s produced no rank at all — the fusion parity says nothing about its weight", name)
		}
	}
	t.Logf("gate (a) TIE FINDING: queries with at least one score tie = %d/54, tie groups = %d, group sizes = %v, groups cut by p_limit = %d",
		queriesWithTies, len(tieGroupSizes), tieGroupSizes, boundaryTies)
}

// ---------------------------------------------------------------------------
// Gate (b): visibility parity
// ---------------------------------------------------------------------------

// TestBW1ArmsVisibilityParity pins that the sister's candidate SET is exactly
// ctx_rrf's — over a fixture with two read scopes, an excluded type, archived
// rows and a granted foreign-scope block — and that an empty allowlist is
// fail-closed on both sides.
func TestBW1ArmsVisibilityParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	q := bw1Query{name: "vis", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	emb := bw1Embedding(0)

	liveIDs, armIDs := bw1VisibilitySets(t, ctx, conn, q, emb, fx.granted)
	bw1AssertSameSet(t, "gate (b)", liveIDs, armIDs)

	// The gate is only worth anything if the fixture really pushes candidates
	// through every visibility branch.
	var sawB, sawGrant, sawDamped bool
	for id := range armIDs {
		switch {
		case fx.scopes[id] == bw1ScopeB:
			sawB = true
		case fx.scopes[id] == bw1ScopeForeign:
			sawGrant = true
		}
		if fx.types[id] == "audit-trail" {
			sawDamped = true
		}
		if fx.types[id] == "checkpoint" {
			t.Errorf("gate (b): excluded type checkpoint surfaced (%s) — allowlist leak", id)
		}
	}
	if !sawB || !sawGrant || !sawDamped {
		t.Errorf("gate (b) fixture too thin: second_scope=%v granted=%v damped_type=%v", sawB, sawGrant, sawDamped)
	}
	t.Logf("gate (b): candidate sets equal, |set| = %d, second scope=%v granted=%v damped=%v, checkpoint rows = 0",
		len(armIDs), sawB, sawGrant, sawDamped)

	// p_types_visible = NULL: 0 rows from both (fail-closed allowlist).
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	args := bw1Args(q, emb, fx.granted, bw1BigLimit)
	args[9] = nil // p_types_visible
	live, err := bw1CallRRF(ctx, tx, args)
	if err != nil {
		t.Fatalf("ctx_rrf with NULL allowlist: %v", err)
	}
	arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, "")
	if err != nil {
		t.Fatalf("ctx_rrf_arms with NULL allowlist: %v", err)
	}
	t.Logf("gate (b) fail-closed: p_types_visible = NULL → ctx_rrf %d rows, ctx_rrf_arms %d rows", len(live), len(arms))
	if len(live) != 0 || len(arms) != 0 {
		t.Errorf("NULL allowlist must yield 0 rows on both sides, got ctx_rrf=%d ctx_rrf_arms=%d", len(live), len(arms))
	}
}

func bw1VisibilitySets(t *testing.T, ctx context.Context, conn *pgxpool.Conn, q bw1Query, emb []float32, granted []string) (map[string]bool, map[string]bool) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	args := bw1Args(q, emb, granted, bw1BigLimit)
	live, err := bw1CallRRF(ctx, tx, args)
	if err != nil {
		t.Fatalf("ctx_rrf call: %v", err)
	}
	arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, "")
	if err != nil {
		t.Fatalf("ctx_rrf_arms call: %v", err)
	}
	liveIDs := map[string]bool{}
	for _, r := range live {
		liveIDs[r.id] = true
	}
	armIDs := map[string]bool{}
	for _, r := range arms {
		armIDs[r.ID] = true
	}
	return liveIDs, armIDs
}

func bw1AssertSameSet(t *testing.T, label string, want, got map[string]bool) {
	t.Helper()
	var missing, extra []string
	for id := range want {
		if !got[id] {
			missing = append(missing, id)
		}
	}
	for id := range got {
		if !want[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("%s: candidate sets differ — %d only in ctx_rrf (%v), %d only in ctx_rrf_arms (%v)",
			label, len(missing), missing, len(extra), extra)
	}
}

// ---------------------------------------------------------------------------
// Gate (c): negative probe
// ---------------------------------------------------------------------------

// bw1MakeProbe loads 137_rrf_arms.sql out of the embedded FS, renames the
// function, applies mutate to the part of the text AFTER `RETURN QUERY` (so a
// surgery can never silently hit the prologue's TOCTOU guard instead of an
// arm), and installs the result. Every mutation must change the text and must
// change it the expected number of times — a drifted body would otherwise turn
// the red probe green by accident.
func bw1MakeProbe(t *testing.T, pool *pgxpool.Pool, name, needle string, want int) {
	t.Helper()
	raw, err := migrations.Section("137_rrf_arms.sql")
	if err != nil {
		t.Fatalf("read embedded 137: %v", err)
	}
	def := strings.ReplaceAll(string(raw), "ctx_rrf_arms", name)
	cut := strings.Index(def, "RETURN QUERY")
	if cut < 0 {
		t.Fatal("marker \"RETURN QUERY\" not found in 137_rrf_arms.sql")
	}
	head, tail := def[:cut], def[cut:]
	if n := strings.Count(tail, needle); n != want {
		t.Fatalf("probe %s: body carries %d occurrences of %q after RETURN QUERY, want %d — migration drifted?",
			name, n, needle, want)
	}
	mutated := head + strings.ReplaceAll(tail, needle, "true")
	if mutated == def {
		t.Fatalf("probe %s: mutation was a no-op", name)
	}
	if _, err := pool.Exec(context.Background(), mutated); err != nil {
		t.Fatalf("install probe %s: %v", name, err)
	}
}

// TestBW1ArmsNegativeProbe is the red side of gate (b): a body whose allowlist
// conjunct is gone must surface MORE ids including the excluded type, and a
// body without the archived filter must surface more ids including archived
// ones. If either probe stayed green, gate (b) would be asserting nothing.
func TestBW1ArmsNegativeProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	q := bw1Query{name: "probe", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	emb := bw1Embedding(0)
	liveIDs, armIDs := bw1VisibilitySets(t, ctx, conn, q, emb, fx.granted)
	bw1AssertSameSet(t, "gate (c) precondition", liveIDs, armIDs)

	bw1MakeProbe(t, pool, "ctx_rrf_arms_probe", "cb.type_name = ANY(p_types_visible)", 7)
	bw1MakeProbe(t, pool, "ctx_rrf_arms_probe_arch", "NOT cb.is_archived", 7)

	run := func(fn string) map[string]bool {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		rows, err := bw1CallArms(ctx, tx, fn, bw1Args(q, emb, fx.granted, bw1BigLimit), "")
		if err != nil {
			t.Fatalf("%s call: %v", fn, err)
		}
		ids := map[string]bool{}
		for _, r := range rows {
			ids[r.ID] = true
		}
		return ids
	}

	// The probes are compared by WHAT leaks, not by how many rows come back.
	// The arms are capped (75/100/100/30), so admitting forbidden candidates
	// can push permitted ones out and shrink the result set — measured: the
	// archived probe returns 92 ids where the green body returns 93. Set size
	// is therefore not a monotone signal; the presence of a row that must not
	// exist is.
	countClass := func(ids map[string]bool, class func(string) bool) int {
		n := 0
		for id := range ids {
			if class(id) {
				n++
			}
		}
		return n
	}
	isCheckpoint := func(id string) bool { return fx.types[id] == "checkpoint" }
	isArchived := func(id string) bool { return fx.archived[id] }

	if n := countClass(armIDs, isCheckpoint); n != 0 {
		t.Errorf("gate (c) precondition: green body already surfaces %d checkpoint blocks", n)
	}
	if n := countClass(armIDs, isArchived); n != 0 {
		t.Errorf("gate (c) precondition: green body already surfaces %d archived blocks", n)
	}

	// Probe 1: allowlist conjunct removed.
	probeIDs := run("ctx_rrf_arms_probe")
	leakedCheckpoint := countClass(probeIDs, isCheckpoint)
	t.Logf("gate (c) RED (no allowlist conjunct): |set| = %d vs green %d, excluded type checkpoint = %d vs green 0",
		len(probeIDs), len(armIDs), leakedCheckpoint)
	if leakedCheckpoint == 0 {
		t.Error("RED probe 1 stayed green: no checkpoint block surfaced without the allowlist conjunct")
	}

	// Probe 2: archived filter removed.
	archIDs := run("ctx_rrf_arms_probe_arch")
	leakedArchived := countClass(archIDs, isArchived)
	t.Logf("gate (c) RED (no is_archived filter): |set| = %d vs green %d, archived blocks = %d vs green 0",
		len(archIDs), len(armIDs), leakedArchived)
	if leakedArchived == 0 {
		t.Error("RED probe 2 stayed green: no archived block surfaced without the is_archived filter")
	}
}

// ---------------------------------------------------------------------------
// Gate (d): cap parameters
// ---------------------------------------------------------------------------

// TestBW1ArmsCapParameters pins the five new parameters: an explicit
// p_cap_semantic really truncates the semantic arm, and the default call is
// byte-for-byte the Ist behaviour (defaults == the literals in 134).
func TestBW1ArmsCapParameters(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	q := bw1Query{name: "caps", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	emb := bw1Embedding(0)

	call := func(extra string) []armRow {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		rows, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", bw1Args(q, emb, fx.granted, bw1BigLimit), extra)
		if err != nil {
			t.Fatalf("ctx_rrf_arms(%s): %v", extra, err)
		}
		return rows
	}

	countArm := func(rows []armRow, pick func(armRow) *int) int {
		n := 0
		for _, r := range rows {
			if pick(r) != nil {
				n++
			}
		}
		return n
	}

	def := call("")
	defSem := countArm(def, func(r armRow) *int { return r.Semantic })
	if defSem < 6 {
		t.Fatalf("default semantic arm holds only %d rows — the cap test would be vacuous", defSem)
	}
	// bw1FixedQuery is also what gates (b), (c) and (e) run, so this doubles as
	// their precondition: a channel that returns nothing there would make their
	// candidate-set comparison a statement about the other three arms only.
	for _, a := range []struct {
		name string
		pick func(armRow) *int
	}{
		{"semantic", func(r armRow) *int { return r.Semantic }},
		{"fts_de", func(r armRow) *int { return r.FtsDE }},
		{"fts_en", func(r armRow) *int { return r.FtsEN }},
		{"trigram", func(r armRow) *int { return r.Trigram }},
	} {
		if countArm(def, a.pick) == 0 {
			t.Errorf("arm %s is empty for bw1FixedQuery %q — gates (b)-(e) would not cover it", a.name, bw1FixedQuery)
		}
	}

	capped := call(", p_cap_semantic => 5")
	capSem := countArm(capped, func(r armRow) *int { return r.Semantic })
	t.Logf("gate (d): default semantic arm = %d rows, p_cap_semantic=5 → %d rows (fts_de %d→%d, fts_en %d→%d, trigram %d→%d)",
		defSem, capSem,
		countArm(def, func(r armRow) *int { return r.FtsDE }), countArm(capped, func(r armRow) *int { return r.FtsDE }),
		countArm(def, func(r armRow) *int { return r.FtsEN }), countArm(capped, func(r armRow) *int { return r.FtsEN }),
		countArm(def, func(r armRow) *int { return r.Trigram }), countArm(capped, func(r armRow) *int { return r.Trigram }))
	if capSem > 5 {
		t.Errorf("p_cap_semantic=5 left %d blocks with a semantic rank", capSem)
	}

	// Default call == explicit Ist literals (75/100/100/30/0.05).
	explicit := call(", p_cap_semantic => 75, p_cap_fts_de => 100, p_cap_fts_en => 100, p_cap_trigram => 30, p_trgm_threshold => 0.05")
	bw1AssertSameRows(t, "gate (d) defaults == Ist literals", def, explicit)
	t.Logf("gate (d): default call and explicit Ist-literal call agree on all %d rows", len(def))
}

func bw1AssertSameRows(t *testing.T, label string, a, b []armRow) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d vs %d rows", label, len(a), len(b))
	}
	key := func(r armRow) string {
		f := func(p *int) string {
			if p == nil {
				return "-"
			}
			return fmt.Sprint(*p)
		}
		return fmt.Sprintf("%s|%s|%s|%s|%s|%.17g|%.17g", r.ID, f(r.Semantic), f(r.FtsDE), f(r.FtsEN), f(r.Trigram), r.Mass, r.Type)
	}
	ka := make([]string, 0, len(a))
	kb := make([]string, 0, len(b))
	for _, r := range a {
		ka = append(ka, key(r))
	}
	for _, r := range b {
		kb = append(kb, key(r))
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			t.Errorf("%s: row %d differs — %s vs %s", label, i, ka[i], kb[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Gate (e): GUC seam
// ---------------------------------------------------------------------------

// TestBW1ArmsGUCSeam proves the ann arm's SET LOCAL really travels out of the
// function and lasts to the end of the transaction. That is the mechanical
// reason the parity gate must keep both calls in one transaction — and it also
// pins that ctx_rrf_arms did not lose the GUC block while copying the body.
func TestBW1ArmsGUCSeam(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Make the GUC known to the session before reading it — a fresh backend
	// has not loaded pgvector's GUC table yet.
	if _, err := tx.Exec(ctx, `SELECT '[1]'::vector <=> '[1]'::vector`); err != nil {
		t.Fatalf("prime vector: %v", err)
	}
	var before string
	if err := tx.QueryRow(ctx, `SELECT current_setting('hnsw.iterative_scan')`).Scan(&before); err != nil {
		t.Fatalf("read GUC before: %v", err)
	}
	if before == "relaxed_order" {
		t.Fatalf("hnsw.iterative_scan is already %q before the call — gate (e) would be vacuous", before)
	}

	q := bw1Query{name: "guc", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	if _, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", bw1Args(q, bw1Embedding(0), fx.granted, bw1Limit), ""); err != nil {
		t.Fatalf("ctx_rrf_arms call: %v", err)
	}

	var after string
	if err := tx.QueryRow(ctx, `SELECT current_setting('hnsw.iterative_scan')`).Scan(&after); err != nil {
		t.Fatalf("read GUC after: %v", err)
	}
	t.Logf("gate (e): hnsw.iterative_scan before = %q, after ctx_rrf_arms = %q", before, after)
	if after != "relaxed_order" {
		t.Errorf("hnsw.iterative_scan = %q after the ann call, want relaxed_order", after)
	}
}
