//go:build integration

// Issue #381: the guard batch searches with its own hnsw.ef_search, and a
// block whose candidate circle is small is checked exactly, without the HNSW
// index. Four pins, all deterministic (no ANN recall is asserted — a graph
// cannot be seeded):
//
//   - the batch's search window is the configured value at every check of
//     its transaction (jit off with it) and gone from the connection after
//     the commit;
//   - a small circle reads no HNSW block and finds every duplicate, also on a
//     connection whose planner is pushed onto the index (generic plans, no
//     sequential or bitmap scan, no sort); above the circle limit the same
//     connection reads the index;
//   - the exact statement answers like ctx_guard_check computed exactly (the
//     index dropped inside the transaction), column by column, over tenants,
//     an unowned scope, archived, stale, unembedded and zero-norm blocks,
//     long titles, both candidate policies, several thresholds and candidate
//     lists — a zero-norm vector on either side included, which both answer
//     clean since migration 188 (issue #396);
//   - a zero-norm vector archives no block, on the candidate side and on the
//     checked side, through the exact statement and through the function.
//
// Fixtures go through the production write path (store.UpsertBlock,
// store.StoreEmbedding, store.DeleteBlock, store.UpdateBlock,
// store.CreateTenant, store.AssignTenantScope).
package guard_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// epVec draws a unit vector; epNear one at cosine c to v (before the
// halfvec rounding of the index and the check).
func epVec(rng *rand.Rand) []float32 {
	v := make([]float32, 1024)
	var n float64
	for i := range v {
		x := rng.NormFloat64()
		v[i] = float32(x)
		n += x * x
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}

func epNear(rng *rand.Rand, v []float32, c float64) []float32 {
	u := epVec(rng)
	var d float64
	for i := range v {
		d += float64(u[i]) * float64(v[i])
	}
	var n float64
	w := make([]float64, len(v))
	for i := range v {
		w[i] = float64(u[i]) - d*float64(v[i])
		n += w[i] * w[i]
	}
	n = math.Sqrt(n)
	s := math.Sqrt(1 - c*c)
	out := make([]float32, len(v))
	for i := range v {
		out[i] = float32(c*float64(v[i]) + s*w[i]/n)
	}
	return out
}

// epZeroNormForms are the two vectors of zero norm in halfvec(1024), the type
// the check computes in: all zeros, and a vector of positive norm whose
// components (1e-8) lie below half the smallest half-precision subnormal
// (2^-24) and round to zero in the cast. store.StoreEmbedding takes both.
var epZeroNormForms = []struct {
	name string
	vec  []float32
}{
	{"zero", make([]float32, 1024)},
	{"underflow", func() []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = 1e-8
		}
		return v
	}()},
}

// epWrite stores a block and, when vec is not nil, its vector.
func epWrite(t *testing.T, pool *pgxpool.Pool, scope, typeName, title string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "ep381", title, "body of "+title, nil, nil,
		scope, true, store.SensitivityWrite{}, typeName)
	if err != nil {
		t.Fatalf("upsert %q: %v", title, err)
	}
	if vec != nil {
		if err := store.StoreEmbedding(ctx, pool, b.ID, "ep381-model", vec); err != nil {
			t.Fatalf("embed %q: %v", title, err)
		}
	}
	return b.ID
}

// epTenant creates a tenant owning the given scopes.
func epTenant(t *testing.T, pool *pgxpool.Pool, slug string, scopes ...string) {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	for _, s := range scopes {
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, s, nil); err != nil {
			t.Fatalf("assign %s to %s: %v", s, slug, err)
		}
	}
}

// epProbe runs the guard batch on one connection of its own and observes its
// transaction: hnsw.ef_search before the commit, and the HNSW blocks the
// duplicate checks read (pg_stat_get_xact_blocks_fetched around each check
// statement — the batch's verdict UPDATEs also read the index, to insert the
// new row versions, which is not a search) together with the route each
// check took.
type epProbe struct {
	conn        *pgxpool.Conn
	checkHNSW   int64    // HNSW blocks read by the check statements
	indexChecks int      // checks through ctx_guard_check
	exactChecks int      // checks through the exact statement
	efAtCheck   []string // hnsw.ef_search in the transaction at each check
	jitAtCheck  []string // jit in the transaction at each check
	efAtCommit  string
	jitAtCommit string
	libAtBegin  int // rows of pg_settings for hnsw.ef_search right after BEGIN
	t           *testing.T
}

const epHNSWBlocksSQL = `SELECT pg_stat_get_xact_blocks_fetched('idx_embedding_hnsw'::regclass)`

func (p *epProbe) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM pg_settings WHERE name = 'hnsw.ef_search'`).Scan(&p.libAtBegin); err != nil {
		p.t.Fatalf("probe settings: %v", err)
	}
	return &epProbeTx{Tx: tx, p: p}, nil
}

func (p *epProbe) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.conn.Exec(ctx, sql, args...)
}

type epProbeTx struct {
	pgx.Tx
	p *epProbe
}

func (tx *epProbeTx) Commit(ctx context.Context) error {
	if err := tx.Tx.QueryRow(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('jit')`).Scan(&tx.p.efAtCommit, &tx.p.jitAtCommit); err != nil {
		tx.p.t.Fatalf("probe commit: %v", err)
	}
	return tx.Tx.Commit(ctx)
}

// QueryRow measures checkBlock's statement (the one that selects
// g.is_cross_scope), with the transaction's hnsw.ef_search and jit right
// before it, and passes every other statement through.
func (tx *epProbeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "g.is_cross_scope,") {
		return tx.Tx.QueryRow(ctx, sql, args...)
	}
	var ef, jit string
	if err := tx.Tx.QueryRow(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('jit')`).Scan(&ef, &jit); err != nil {
		tx.p.t.Fatalf("probe settings at check: %v", err)
	}
	tx.p.efAtCheck = append(tx.p.efAtCheck, ef)
	tx.p.jitAtCheck = append(tx.p.jitAtCheck, jit)
	var before int64
	if err := tx.Tx.QueryRow(ctx, epHNSWBlocksSQL).Scan(&before); err != nil {
		tx.p.t.Fatalf("probe before check: %v", err)
	}
	if strings.Contains(sql, "ctx_guard_check(") {
		tx.p.indexChecks++
	} else {
		tx.p.exactChecks++
	}
	return &epProbeRow{Row: tx.Tx.QueryRow(ctx, sql, args...), tx: tx, before: before}
}

type epProbeRow struct {
	pgx.Row
	tx     *epProbeTx
	before int64
}

func (r *epProbeRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if err == nil {
		var after int64
		if e := r.tx.Tx.QueryRow(context.Background(), epHNSWBlocksSQL).Scan(&after); e != nil {
			r.tx.p.t.Fatalf("probe after check: %v", e)
		}
		r.tx.p.checkHNSW += after - r.before
	}
	return err
}

// epOwnPool opens a pool of one fresh connection on the test database, with
// the given parameters as the session's defaults (startup parameters, which a
// SET LOCAL … TO DEFAULT inside the batch falls back to).
func epOwnPool(t *testing.T, pool *pgxpool.Pool, params map[string]string) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	own, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("own pool: %v", err)
	}
	t.Cleanup(own.Close)
	return own
}

func epRun(t *testing.T, own *pgxpool.Pool, set *blocktype.Set, limit int, search guard.Search) (*epProbe, int) {
	t.Helper()
	ctx := context.Background()
	conn, err := own.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	p := &epProbe{conn: conn, t: t}
	n, err := guard.RunGuardBatch(ctx, p, set, limit, search)
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	return p, n
}

// TestGuardBatchEfSearchIsTransactionLocal: the batch runs every check with
// the configured hnsw.ef_search and jit off (read in the transaction right
// before each check), and the value ends with its transaction —
// the next statement on the same connection sees pgvector's default again.
// The first run is on a fresh backend (the setting is a placeholder until the
// first vector operation loads pgvector inside the batch).
func TestGuardBatchEfSearchIsTransactionLocal(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3811))
	set := blocktype.NewRegistry().Snapshot()
	// The session starts with jit on, whatever the image's default, so the
	// batch's own jit = off is visible and so is its end.
	own := epOwnPool(t, pool, map[string]string{"jit": "on"})

	for i, ef := range []int{guard.SearchDefaultForTest.EfSearch, 123} {
		for j := 0; j < 3; j++ {
			epWrite(t, pool, "private", "knowledge", fmt.Sprintf("ef-%d-%d", i, j), epVec(rng))
		}
		p, n := epRun(t, own, set, 100, guard.Search{EfSearch: ef, ExactMaxCandidates: guard.SearchDefaultForTest.ExactMaxCandidates})
		if n != 3 {
			t.Fatalf("run %d: processed %d, want 3", i, n)
		}
		if len(p.efAtCheck) != n {
			t.Errorf("run %d: %d checks observed, want %d", i, len(p.efAtCheck), n)
		}
		for k := range p.efAtCheck {
			if p.efAtCheck[k] != strconv.Itoa(ef) || p.jitAtCheck[k] != "off" {
				t.Errorf("run %d check %d: hnsw.ef_search %q and jit %q in the transaction, want %d and off",
					i, k, p.efAtCheck[k], p.jitAtCheck[k], ef)
			}
		}
		if p.efAtCommit != strconv.Itoa(ef) {
			t.Errorf("run %d: hnsw.ef_search inside the batch = %q, want %d", i, p.efAtCommit, ef)
		}
		if p.jitAtCommit != "off" {
			t.Errorf("run %d: jit inside the batch = %q, want off", i, p.jitAtCommit)
		}
		t.Logf("run %d: pgvector loaded at BEGIN: %v", i, p.libAtBegin == 1)

		conn, err := own.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var after *string
		var jitAfter string
		err = conn.QueryRow(ctx, `SELECT current_setting('hnsw.ef_search', true), current_setting('jit')`).Scan(&after, &jitAfter)
		conn.Release()
		if err != nil {
			t.Fatal(err)
		}
		if after == nil || *after != "40" {
			t.Errorf("run %d: hnsw.ef_search on the connection after the batch = %v, want pgvector's default 40", i, after)
		}
		if jitAfter != "on" {
			t.Errorf("run %d: jit on the connection after the batch = %q, want the session's on", i, jitAfter)
		}
	}
}

// TestGuardExactPathSmallCircle: a tenant of 20 blocks (10 near-duplicate
// pairs, cosine 0.985) next to a large tenant whose circle exceeds the exact
// limit. On a connection that plans generically and has no sequential scan,
// bitmap scan or sort to fall back on, the batch over the small tenant reads
// no HNSW block and archives one block of every pair. On a connection as the
// scheduler has it, the batch over the large tenant reads the index — at
// ef_search 800 in a store this small the planner would otherwise sort every
// candidate by distance, so this is the pinned index plan.
func TestGuardExactPathSmallCircle(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	rng := rand.New(rand.NewSource(3812))
	set := blocktype.NewRegistry().Snapshot()
	search := guard.SearchDefaultForTest

	epTenant(t, pool, "ep-small", "ep-small")
	type pair struct{ a, b string }
	var pairs []pair
	for i := 0; i < 10; i++ {
		v := epVec(rng)
		a := epWrite(t, pool, "ep-small", "knowledge", fmt.Sprintf("small-%d-a", i), v)
		b := epWrite(t, pool, "ep-small", "knowledge", fmt.Sprintf("small-%d-b", i), epNear(rng, v, 0.985))
		pairs = append(pairs, pair{a, b})
	}
	// The large circle: the default tenant (private/work/shared) holds more
	// live blocks than the exact limit — checkpoints, never picked and never
	// candidates, but in the index and in the counters — and two pending
	// knowledge blocks, written after the small tenant (the pick is oldest
	// first).
	big := search.ExactMaxCandidates + 200
	for i := 0; i < big; i++ {
		epWrite(t, pool, "private", "checkpoint", fmt.Sprintf("bulk-%d", i), epVec(rng))
	}
	v := epVec(rng)
	epWrite(t, pool, "private", "knowledge", "big-a", v)
	epWrite(t, pool, "private", "knowledge", "big-b", epNear(rng, v, 0.985))
	if _, err := pool.Exec(context.Background(), `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}

	pushed := map[string]string{
		"plan_cache_mode":   "force_generic_plan",
		"enable_seqscan":    "off",
		"enable_bitmapscan": "off",
		"enable_sort":       "off",
	}
	own := epOwnPool(t, pool, pushed)

	p, n := epRun(t, own, set, len(pairs)*2, search)
	if n != len(pairs)*2 {
		t.Fatalf("small batch processed %d, want %d", n, len(pairs)*2)
	}
	if p.exactChecks != len(pairs)*2 || p.indexChecks != 0 {
		t.Errorf("small circle: %d exact and %d index checks, want %d and 0", p.exactChecks, p.indexChecks, len(pairs)*2)
	}
	if p.checkHNSW != 0 {
		t.Errorf("small circle: the checks read %d HNSW blocks, want 0 (exact path)", p.checkHNSW)
	}
	for i, pr := range pairs {
		var archived int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FILTER (WHERE is_archived)::int FROM context_blocks WHERE id = ANY($1::uuid[])`,
			[]string{pr.a, pr.b}).Scan(&archived); err != nil {
			t.Fatal(err)
		}
		if archived != 1 {
			t.Errorf("pair %d: %d of 2 archived, want 1 (the duplicate found exactly)", i, archived)
		}
	}

	plain := epOwnPool(t, pool, nil)
	p, n = epRun(t, plain, set, 2, search)
	if n != 2 {
		t.Fatalf("big batch processed %d, want 2", n)
	}
	if p.indexChecks != 2 || p.exactChecks != 0 {
		t.Errorf("large circle: %d index and %d exact checks, want 2 and 0", p.indexChecks, p.exactChecks)
	}
	// One block is the index's metapage, read when the planner prices the
	// index; a check that searched the graph reads far more.
	if p.checkHNSW < 2*20 {
		t.Errorf("large circle: the checks read %d HNSW blocks, want the index searched", p.checkHNSW)
	}
	t.Logf("large circle: %d HNSW blocks in %d checks", p.checkHNSW, p.indexChecks)
}

// epRow is one answer of a check source, the similarity as text.
type epRow struct {
	decision, sim         string
	matched, title, scope *string
	cross                 bool
	err                   string
}

func (r epRow) String() string {
	s := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	return fmt.Sprintf("%s|%s|%s|%q|%s|%v|%s", r.decision, r.sim, s(r.matched), s(r.title), s(r.scope), r.cross, r.err)
}

func epCheck(ctx context.Context, tx pgx.Tx, source, id string, dup, rev float64, types []string, sso any) epRow {
	var r epRow
	if _, err := tx.Exec(ctx, "SAVEPOINT ep"); err != nil {
		r.err = err.Error()
		return r
	}
	err := tx.QueryRow(ctx,
		`SELECT g.decision, g.top_similarity::text, g.matched_id::text, g.matched_title, g.matched_scope, g.is_cross_scope FROM `+source+` g`,
		id, dup, rev, types, sso).Scan(&r.decision, &r.sim, &r.matched, &r.title, &r.scope, &r.cross)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			r.err = pe.Code
		} else {
			r.err = err.Error()
		}
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT ep")
	}
	_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT ep")
	return r
}

// TestGuardExactSourceMatchesFunction: the exact statement answers like
// ctx_guard_check computed exactly, in every column, for every block of a
// corpus with the cases the predicates and the decision distinguish — a
// check whose pair has a zero-norm vector and a match whose title is longer
// than 255 characters included (issue #396).
func TestGuardExactSourceMatchesFunction(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3813))
	set := blocktype.NewRegistry().Snapshot()

	epTenant(t, pool, "ep-b", "ep-b1", "ep-b2")
	scopes := []string{"private", "work", "shared", "ep-b1", "ep-b2", "ep-orphan"}
	types := []string{"knowledge", "knowledge", "reference", "issue", "checkpoint"}
	centres := []([]float32){epVec(rng), epVec(rng), epVec(rng), epVec(rng)}
	var ids []string
	zeroNorm := map[string]bool{}
	zeroCandidates := map[string]bool{} // checked blocks whose only candidates have zero norm
	for i := 0; i < 90; i++ {
		c := centres[i%len(centres)]
		cos := []float64{0.999, 0.985, 0.975, 0.95, 0.93, 0.91, 0.85, 0.6}[rng.Intn(8)]
		title := fmt.Sprintf("parity-%d", i)
		switch i % 15 {
		case 3:
			title = fmt.Sprintf("%03d", i) + strings.Repeat("x", 297) // longer than VARCHAR(255)
		case 7:
			title = fmt.Sprintf("%03d", i) + strings.Repeat("y", 252) + "     " // 260 with trailing blanks
		}
		var vec []float32
		if i%17 != 5 { // a few blocks without a vector
			vec = epNear(rng, c, cos)
		}
		if i%19 == 9 { // a few zero-norm vectors, among other candidates
			vec = epZeroNormForms[(i/19)%2].vec
		}
		id := epWrite(t, pool, scopes[i%len(scopes)], types[i%len(types)], title, vec)
		ids = append(ids, id)
		if i%19 == 9 {
			zeroNorm[id] = true
		}
	}
	// A circle of its own whose only candidates have zero norm.
	zeroCircle := epWrite(t, pool, "ep-zero", "knowledge", "zero-circle-n", epVec(rng))
	ids = append(ids, zeroCircle)
	zeroCandidates[zeroCircle] = true
	for _, f := range epZeroNormForms {
		id := epWrite(t, pool, "ep-zero", "knowledge", "zero-circle-"+f.name, f.vec)
		ids = append(ids, id)
		zeroNorm[id] = true
	}
	// Archived and stale blocks, through their write paths.
	for i := 0; i < len(ids); i += 11 {
		if _, err := store.DeleteBlock(ctx, pool, ids[i], scopes); err != nil {
			t.Fatalf("archive %d: %v", i, err)
		}
	}
	for i := 4; i < len(ids); i += 13 {
		content := "changed body " + strconv.Itoa(i)
		if _, _, err := store.UpdateBlock(ctx, pool, ids[i], store.UpdateBlockData{Content: &content}, scopes); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	var stale int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE embedding_stale`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale == 0 {
		t.Fatalf("fixture has no stale vector; the stale predicate goes unexercised")
	}
	ids = append(ids, "019f3810-0000-7000-9000-000000000000") // no such block

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// ctx_guard_check computed exactly: without its index.
	if _, err := tx.Exec(ctx, `DROP INDEX idx_embedding_hnsw`); err != nil {
		t.Fatal(err)
	}
	const fnSource = `ctx_guard_check($1::uuid, $2::real, $3::real, $4::text[], $5::boolean)`
	exSource := guard.GuardCheckSourceExactForTest
	cand := set.GuardCandidateTypes()
	seen := map[string]int{}
	for _, id := range ids {
		for _, thr := range [][2]float64{{0.98, 0.92}, {0.97, 0.90}, {0.9, 0.8}} {
			for ti, ts := range [][]string{cand, nil, {}, append(append([]string{}, cand...), "checkpoint")} {
				// nil: a NULL p_same_scope_only, which the function reads as
				// the own scope only (NOT NULL OR scope = own).
				for _, sso := range []any{false, true, nil} {
					a := epCheck(ctx, tx, fnSource, id, thr[0], thr[1], ts, sso)
					b := epCheck(ctx, tx, exSource, id, thr[0], thr[1], ts, sso)
					seen[a.decision+"/"+a.err]++
					if a.cross {
						seen["cross"]++
					}
					if a.title != nil && utf8.RuneCountInString(*a.title) > 255 {
						seen["title over 255"]++
					}
					// A zero-norm vector has no cosine distance; both answer
					// as if no candidate existed. Counted by the fixture, as
					// the answer no longer shows it.
					if zeroNorm[id] && a.decision == "clean" && a.matched == nil && slices.Contains(ts, "knowledge") {
						seen["zero-norm checked"]++
					}
					if zeroCandidates[id] && a.decision == "clean" && a.matched == nil && slices.Contains(ts, "knowledge") {
						seen["zero-norm candidate"]++
					}
					if a.String() != b.String() {
						t.Errorf("block %s thr=%v types#%d same_scope=%v:\n function %s\n exact    %s", id[:8], thr, ti, sso, a, b)
					}
				}
			}
		}
	}
	t.Logf("cases: %v", seen)
	for _, k := range []string{"near_duplicate/", "needs_review/", "clean/", "title over 255", "cross", "zero-norm checked", "zero-norm candidate"} {
		if seen[k] == 0 {
			t.Errorf("no case %q in the corpus; the comparison does not reach it", k)
		}
	}
}

// epVerdict is what the batch left on a checked block: the audit log's
// decision, similarity and match, the block's archive flag and guard_status.
type epVerdict struct {
	decision, sim, matched, status string
	archived                       bool
}

func epVerdictOf(t *testing.T, pool *pgxpool.Pool, id string) epVerdict {
	t.Helper()
	var v epVerdict
	if err := pool.QueryRow(context.Background(),
		`SELECT wl.decision, wl.similarity::text, coalesce(wl.matched_block_id::text, ''),
		        coalesce(cb.guard_status, ''), cb.is_archived
		   FROM context_blocks cb JOIN context_write_log wl ON wl.block_id = cb.id
		  WHERE cb.id = $1::uuid`, id).Scan(&v.decision, &v.sim, &v.matched, &v.status, &v.archived); err != nil {
		t.Fatalf("verdict of %s: %v", id, err)
	}
	return v
}

// TestGuardZeroNormVector (PR #395 review, issue #396): a vector of zero norm
// in halfvec(1024) has no cosine distance — its similarity is NaN, and NaN
// passes every ">=" threshold. Per zero-norm form, four cells: the vector on
// the candidate side (the only candidate of a checked block) and on the
// checked side (one candidate), each through ctx_guard_check with the batch's
// index plan (exact limit 0) and through the exact statement (default limit).
// In each cell the batch first checks the block written first, alone in its
// tenant's circle, then the second one against it; the verdict is read from
// the audit log and the block. Every cell answers clean: the HNSW index holds
// no zero-norm vector, and a nearest candidate at NaN counts as none on both
// routes (before migration 188 the function answered near_duplicate at NaN on
// the checked side and archived the checked block).
func TestGuardZeroNormVector(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3814))
	set := blocktype.NewRegistry().Snapshot()
	plain := epOwnPool(t, pool, nil)
	clean := epVerdict{decision: "clean", sim: "0", status: "clean"}

	for _, f := range epZeroNormForms {
		for _, side := range []string{"candidate", "checked"} {
			for _, path := range []string{"function", "exact"} {
				t.Run(f.name+"/"+side+"/"+path, func(t *testing.T) {
					scope := "zn-" + f.name[:2] + "-" + side[:4] + "-" + path[:2]
					epTenant(t, pool, scope, scope)
					search := guard.SearchDefaultForTest
					if path == "function" {
						search = guard.SearchIndexForTest
					}
					first, second := f.vec, epVec(rng)
					if side == "checked" {
						first, second = second, first
					}
					firstID := epWrite(t, pool, scope, "knowledge", scope+"-1", first)
					if _, n := epRun(t, plain, set, 10, search); n != 1 {
						t.Fatalf("first batch processed %d, want 1", n)
					}
					id := epWrite(t, pool, scope, "knowledge", scope+"-2", second)
					zero := firstID
					if side == "checked" {
						zero = id
					}
					var norm float64
					if err := pool.QueryRow(ctx, `SELECT l2_norm(embedding::halfvec(1024)) FROM context_blocks WHERE id = $1::uuid`,
						zero).Scan(&norm); err != nil {
						t.Fatal(err)
					}
					if norm != 0 {
						t.Fatalf("fixture: the %s vector has halfvec norm %v, want 0", f.name, norm)
					}
					p, n := epRun(t, plain, set, 10, search)
					if n != 1 {
						t.Fatalf("second batch processed %d, want 1", n)
					}
					if (path == "exact") != (p.exactChecks == 1) || p.exactChecks+p.indexChecks != 1 {
						t.Fatalf("route: %d exact and %d index checks for the %s path", p.exactChecks, p.indexChecks, path)
					}
					got := epVerdictOf(t, pool, id)
					t.Logf("%s/%s/%s: %+v", f.name, side, path, got)
					if got != clean {
						t.Errorf("verdict %+v, want %+v", got, clean)
					}
				})
			}
		}
	}
}
