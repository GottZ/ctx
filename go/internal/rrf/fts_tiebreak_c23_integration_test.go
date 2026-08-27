//go:build integration

// C2-3 gates for migration 147_fts_tiebreak.sql: the two full-text arms of
// ctx_rrf and ctx_rrf_arms get the deterministic id tiebreak the semantic
// exact pool has carried since Generation 17 (139:210/:213, unchanged as
// 145:465/:468) and the trigram arm since 140 (145:565/:580).
//
// The finding this migration answers is X-W1 Teil B, N10: two replicate runs
// of the same 1 000 gold cases against the same corpus disagreed in 84 cases;
// every one of them had exactly 100 full-text candidates — the arm cap — 83 of
// them changed the candidate SET, and the first difference sat at rank 2 in the
// median. The ANN share of that spread was exactly 0.
//
// Why a fixture with constructed ties is the right instrument: the arms rank
// with `ROW_NUMBER() OVER (ORDER BY GREATEST(ts_rank_cd …) DESC)`. That order
// is not total the moment two blocks score bit-identically, and PostgreSQL's
// sort is not stable, so the ranks — and, once the tie group runs past the cap,
// the candidate SET — follow the physical heap order. The fixture below builds
// 240 blocks whose ts_de/ts_en are byte-identical (same title, same content,
// different category — the only column the (category, title) uniqueness needs)
// against a cap of 100, and then MOVES them in the heap the way production
// does: with an UPDATE.
//
// Both queries run in exact semantic mode on purpose. The ann arm is
// approximate and plan-sensitive on its own; in exact mode the semantic arm
// carries its own tiebreak, so every difference this file measures belongs to
// the full-text arms and to nothing else.
//
//	go test -tags=integration ./internal/rrf/ -run TestC23 -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	// c23TieBlocks is deliberately larger than the fts cap (100): the tie group
	// alone cannot fit, so the arm has to CHOOSE — which is the N10 situation.
	c23TieBlocks = 240
	// c23UniqBlocks stays well under the cap so that the tie-free query's arms
	// hold the whole group and every rank is decided by score alone.
	c23UniqBlocks = 40
	c23FtsCap     = 100
	c23Seed       = 0x0c233
	c23Limit      = 25

	// Invented tokens: they survive both the german and the english
	// configuration unchanged and collide with no stopword, so the two arms
	// see the same lexemes and neither query can leak into the other group.
	c23TieWordA  = "tiewortalpha"
	c23TieWordB  = "tiewortbeta"
	c23UniqWord  = "uniqwortalpha"
	c23TieQuery  = c23TieWordA + " " + c23TieWordB
	c23UniqQuery = c23UniqWord
)

// c23TieTitle and c23TieContent are shared VERBATIM by all c23TieBlocks rows.
// ts_de/ts_en are GENERATED ALWAYS AS to_tsvector(config, title || ' ' ||
// content) (113_baseline.sql:89-94), so identical text is identical tsvector is
// identical ts_rank_cd — the tie is a construction, not an observation.
const (
	c23TieTitle   = "Gleichstand " + c23TieWordA + " " + c23TieWordB
	c23TieContent = "Der " + c23TieWordA + " und der " + c23TieWordB + " stehen hier in derselben Reihenfolge. " +
		"The " + c23TieWordA + " and the " + c23TieWordB + " appear in the same order."
)

type c23Fixture struct {
	tieIDs  []string
	uniqIDs []string
}

// c23Seed writes both groups. Every block is type knowledge in scope bw1a, so
// the shared bw1Args call surface reaches all of them.
func c23SeedCorpus(t *testing.T, pool *pgxpool.Pool) c23Fixture {
	t.Helper()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(c23Seed))
	var fx c23Fixture

	// created_at is handed out explicitly and strictly increasing, so
	// idx_context_created is a TOTAL order over the fixture and c23ResetHeap
	// can use it to restore one canonical physical layout.
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	seq := 0
	insert := func(id, category, title, content string) {
		t.Helper()
		e := make([]float32, 1024)
		for k := range e {
			e[k] = float32(rng.Float64())
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks
				(id, category, title, content, scope, embedding, created_at, updated_at,
				 type_name, is_archived)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, 'knowledge', false)`,
			id, category, title, content, bw1ScopeA, pgvNewHalfVec(e),
			base.Add(time.Duration(seq)*time.Second),
		); err != nil {
			t.Fatalf("insert %s/%s: %v", category, title, err)
		}
		seq++
	}

	for i := 0; i < c23TieBlocks; i++ {
		id := fmt.Sprintf("019fa403-0000-7000-9000-0000000%05d", 10000+i)
		// Distinct CATEGORY, identical title+content: the uniqueness constraint
		// is (category, title), so this is the one axis that varies without
		// touching a single lexeme.
		insert(id, fmt.Sprintf("c23tie%03d", i), c23TieTitle, c23TieContent)
		fx.tieIDs = append(fx.tieIDs, id)
	}

	for i := 0; i < c23UniqBlocks; i++ {
		id := fmt.Sprintf("019fa403-0000-7000-9000-0000000%05d", 20000+i)
		// i+1 occurrences of the query token: ts_rank_cd sums one cover per
		// occurrence, so the score is strictly monotone in i. Asserted below,
		// never assumed.
		content := strings.TrimSpace(strings.Repeat(c23UniqWord+" ", i+1)) +
			" Fuellwort ohne Bezug zur Abfrage."
		insert(id, "c23uniq", fmt.Sprintf("Einzelwert Block %03d", i), content)
		fx.uniqIDs = append(fx.uniqIDs, id)
	}

	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("fixture: %d tie blocks (identical ts_de/ts_en, cap %d) + %d tie-free blocks, scope %s, type knowledge",
		len(fx.tieIDs), c23FtsCap, len(fx.uniqIDs), bw1ScopeA)
	return fx
}

// c23AssertFixtureShape proves the fixture is what the gate needs it to be
// BEFORE anything is measured: the tie group really scores bit-identically and
// really overflows the cap, and the tie-free group really has no duplicate
// score at all. A fixture that fails either half would make the whole file
// vacuous in a way no later assertion could catch.
func c23AssertFixtureShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	scores := func(query, col, cfg string) []float64 {
		t.Helper()
		sql := fmt.Sprintf(
			`SELECT ts_rank_cd(cb.%s, plainto_tsquery('%s', $1))::float8
			   FROM context_blocks cb
			  WHERE cb.%s @@ plainto_tsquery('%s', $1)
			  ORDER BY 1 DESC`, col, cfg, col, cfg)
		rows, err := pool.Query(ctx, sql, query)
		if err != nil {
			t.Fatalf("fixture probe %s/%s: %v", col, query, err)
		}
		defer rows.Close()
		var out []float64
		for rows.Next() {
			var v float64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan fixture probe: %v", err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("fixture probe rows: %v", err)
		}
		return out
	}

	for _, arm := range []struct{ col, cfg string }{{"ts_de", "german"}, {"ts_en", "english"}} {
		tie := scores(c23TieQuery, arm.col, arm.cfg)
		if len(tie) != c23TieBlocks {
			t.Fatalf("%s: tie query matches %d blocks, want exactly the %d tie blocks", arm.col, len(tie), c23TieBlocks)
		}
		if len(tie) <= c23FtsCap {
			t.Fatalf("%s: tie group holds %d blocks, cap is %d — the arm would not have to choose", arm.col, len(tie), c23FtsCap)
		}
		for i := 1; i < len(tie); i++ {
			if tie[i] != tie[0] {
				t.Fatalf("%s: tie block %d scores %.17g, block 0 scores %.17g — the tie is not exact", arm.col, i, tie[i], tie[0])
			}
		}

		uniq := scores(c23UniqQuery, arm.col, arm.cfg)
		if len(uniq) != c23UniqBlocks {
			t.Fatalf("%s: tie-free query matches %d blocks, want %d", arm.col, len(uniq), c23UniqBlocks)
		}
		seen := map[float64]int{}
		for i, v := range uniq {
			if j, dup := seen[v]; dup {
				t.Fatalf("%s: tie-free group is not tie-free — positions %d and %d both score %.17g", arm.col, j, i, v)
			}
			seen[v] = i
		}
		t.Logf("fixture shape %s: %d tie blocks all at %.17g (cap %d), %d tie-free blocks with %d distinct scores",
			arm.col, len(tie), tie[0], c23FtsCap, len(uniq), len(seen))
	}
}

// ---------------------------------------------------------------------------
// Measurement
// ---------------------------------------------------------------------------

// c23Fingerprint is one observation of both functions for one query: the
// full-text arms as (id -> rank) maps, plus what ctx_rrf finally delivered.
type c23Fingerprint struct {
	ftsDE   string // "id:rank,…" sorted by id — the SET and the RANKS in one string
	ftsEN   string
	armsAll string // every projected arms column, sorted — the byte-identity gate
	rrfSeq  string // ctx_rrf's delivered (id, score) sequence, in order
}

func (f c23Fingerprint) fts() string { return f.ftsDE + "|" + f.ftsEN }

// c23Measure calls both functions with identical arguments in ONE transaction
// (the GUC seam arms_parity_integration_test.go pins) after applying the
// session settings of the round.
func c23Measure(t *testing.T, ctx context.Context, conn *pgxpool.Conn, args []any, settings []string) c23Fingerprint {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, s := range settings {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("apply %q: %v", s, err)
		}
	}

	live, err := bw1CallRRF(ctx, tx, args)
	if err != nil {
		t.Fatalf("ctx_rrf: %v", err)
	}
	arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, "")
	if err != nil {
		t.Fatalf("ctx_rrf_arms: %v", err)
	}

	num := func(p *int) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	var de, en, all []string
	for _, r := range arms {
		if r.FtsDE != nil {
			de = append(de, r.ID+":"+num(r.FtsDE))
		}
		if r.FtsEN != nil {
			en = append(en, r.ID+":"+num(r.FtsEN))
		}
		cos := "-"
		if r.Cos != nil {
			cos = fmt.Sprintf("%.17g", *r.Cos)
		}
		all = append(all, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%.17g|%.17g",
			r.ID, num(r.Semantic), num(r.FtsDE), num(r.FtsEN), num(r.Trigram), cos, r.Mass, r.Type))
	}
	sort.Strings(de)
	sort.Strings(en)
	sort.Strings(all)

	seq := make([]string, len(live))
	for i, r := range live {
		seq[i] = fmt.Sprintf("%s@%.17g", r.id, r.score)
	}

	return c23Fingerprint{
		ftsDE:   strings.Join(de, ","),
		ftsEN:   strings.Join(en, ","),
		armsAll: strings.Join(all, ","),
		rrfSeq:  strings.Join(seq, ","),
	}
}

// c23Round is one measurement condition. The settings are session GUCs applied
// inside the measuring transaction; `reorder` runs BEFORE the round and moves
// tuples in the heap the way an ordinary UPDATE does in production.
type c23Round struct {
	name     string
	settings []string
	reorder  bool
}

// c23Rounds are the conditions the arms must be invariant under. None of them
// changes a single ranked value: the GUCs only steer the planner, and the
// reorder touches updated_at, which no arm reads.
var c23Rounds = []c23Round{
	{name: "default", settings: nil},
	{name: "seqscan-only", settings: []string{
		"SET enable_bitmapscan = off", "SET enable_indexscan = off", "SET enable_indexonlyscan = off",
	}},
	{name: "work_mem-64kB", settings: []string{"SET work_mem = '64kB'"}},
	{name: "parallel", settings: []string{
		"SET max_parallel_workers_per_gather = 4", "SET parallel_setup_cost = 0",
		"SET parallel_tuple_cost = 0", "SET min_parallel_table_scan_size = 0",
	}},
	{name: "after-heap-reorder", reorder: true},
	{name: "after-heap-reorder+seqscan-only", settings: []string{
		"SET enable_bitmapscan = off", "SET enable_indexscan = off", "SET enable_indexonlyscan = off",
	}},
	{name: "after-second-reorder", reorder: true},
	{name: "after-third-reorder", reorder: true},
}

// c23ResetHeap restores ONE canonical physical layout. Without it the two
// sweeps of the red/green gate would not face the same conditions: measured on
// the first draft of this file, the reorder below exhausts itself — applying
// the same stride twice re-appends the same tuples in the same relative order,
// so the layout after reorder n and after reorder n+1 is identical and the
// second sweep looked stable even on the untiebroken body. CLUSTER over the
// (now total) created_at index puts every sweep back on the same start.
func c23ResetHeap(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "CLUSTER context_blocks USING idx_context_created"); err != nil {
		t.Fatalf("cluster context_blocks: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze after cluster: %v", err)
	}
}

// c23Reorder rewrites one stride of the tie group. Postgres writes each updated
// tuple to a new heap location, so afterwards the physical order of the group
// is neither the clustered one nor the id order — which is exactly what an
// untiebroken sort key follows. The phase picks a DIFFERENT stride and a
// different direction each time, so two reorders inside one sweep really are
// two different layouts.
func c23Reorder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids []string, phase int) {
	t.Helper()
	step := 3
	touched := 0
	move := func(i int) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET updated_at = updated_at + interval '1 second' WHERE id = $1::uuid`,
			ids[i]); err != nil {
			t.Fatalf("reorder update %s: %v", ids[i], err)
		}
		touched++
	}
	if phase%2 == 0 {
		for i := len(ids) - 1 - (phase % step); i >= 0; i -= step {
			move(i)
		}
	} else {
		for i := phase % step; i < len(ids); i += step {
			move(i)
		}
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze after reorder: %v", err)
	}
	t.Logf("heap reorder phase %d: %d of %d tie tuples rewritten", phase, touched, len(ids))
}

// c23Sweep runs every round for one query and returns the fingerprints. Each
// sweep starts from the canonical layout, so two sweeps are comparable.
func c23Sweep(t *testing.T, ctx context.Context, pool *pgxpool.Pool, conn *pgxpool.Conn, fx c23Fixture, args []any) []c23Fingerprint {
	t.Helper()
	c23ResetHeap(t, ctx, pool)
	out := make([]c23Fingerprint, 0, len(c23Rounds))
	phase := 0
	for _, r := range c23Rounds {
		if r.reorder {
			c23Reorder(t, ctx, pool, fx.tieIDs, phase)
			phase++
		}
		out = append(out, c23Measure(t, ctx, conn, args, r.settings))
	}
	return out
}

// c23DiffCount reports how many rounds disagree with round 0 on the full-text
// arms, and how many of those disagree on the candidate SET rather than only on
// the ranks inside it.
func c23DiffCount(fps []c23Fingerprint) (armDiff, setDiff int, first string) {
	idsOnly := func(s string) string {
		parts := strings.Split(s, ",")
		for i, p := range parts {
			if j := strings.LastIndex(p, ":"); j >= 0 {
				parts[i] = p[:j]
			}
		}
		return strings.Join(parts, ",")
	}
	for i := 1; i < len(fps); i++ {
		if fps[i].fts() == fps[0].fts() {
			continue
		}
		armDiff++
		if idsOnly(fps[i].ftsDE) != idsOnly(fps[0].ftsDE) || idsOnly(fps[i].ftsEN) != idsOnly(fps[0].ftsEN) {
			setDiff++
		}
		if first == "" {
			first = fmt.Sprintf("round %d (%s) vs round 0 (%s): fts_de %s, fts_en %s",
				i, c23Rounds[i].name, c23Rounds[0].name,
				c23Describe(fps[0].ftsDE, fps[i].ftsDE), c23Describe(fps[0].ftsEN, fps[i].ftsEN))
		}
	}
	return armDiff, setDiff, first
}

// c23Describe summarises how two "id:rank" listings differ, without dumping
// two 100-entry strings into the log.
func c23Describe(a, b string) string {
	parse := func(s string) map[string]string {
		m := map[string]string{}
		if s == "" {
			return m
		}
		for _, p := range strings.Split(s, ",") {
			if j := strings.LastIndex(p, ":"); j >= 0 {
				m[p[:j]] = p[j+1:]
			}
		}
		return m
	}
	ma, mb := parse(a), parse(b)
	var onlyA, onlyB, rankMoved int
	for id, ra := range ma {
		rb, ok := mb[id]
		switch {
		case !ok:
			onlyA++
		case ra != rb:
			rankMoved++
		}
	}
	for id := range mb {
		if _, ok := ma[id]; !ok {
			onlyB++
		}
	}
	return fmt.Sprintf("|A|=%d |B|=%d, only-in-A=%d only-in-B=%d, same-id-different-rank=%d",
		len(ma), len(mb), onlyA, onlyB, rankMoved)
}

// ---------------------------------------------------------------------------
// Structure assertion
// ---------------------------------------------------------------------------

// c23TiebreakSites counts the tiebroken and the untiebroken full-text sort keys
// in the body the database ACTUALLY runs, read back out of the catalog rather
// than out of the file the test believes it applied.
//
// `) DESC` closes a GREATEST( … ) and appears only in the two full-text arms —
// twice each after 147 (the ROW_NUMBER window and the outer ORDER BY), twice
// each before it (the window only). The final projection's `ORDER BY r.score
// DESC, cb.id` carries no closing parenthesis and is therefore not counted.
func c23TiebreakSites(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fn string) (tiebroken, bare int, def string) {
	t.Helper()
	err := pool.QueryRow(ctx,
		`SELECT pg_get_functiondef(p.oid) FROM pg_proc p
		 JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE p.proname = $1 AND n.nspname = 'public'`, fn).Scan(&def)
	if err != nil {
		t.Fatalf("pg_get_functiondef(%s): %v", fn, err)
	}
	for _, line := range strings.Split(def, "\n") {
		switch strings.TrimSpace(line) {
		case ") DESC, cb.id":
			tiebroken++
		case ") DESC":
			bare++
		}
	}
	return tiebroken, bare, def
}

// ---------------------------------------------------------------------------
// Gate 1 + 2: determinism, red before 147 and green after
// ---------------------------------------------------------------------------

// TestC23FTSTiebreakRedGreen is the load-bearing gate. It starts on a database
// whose migration chain is capped at 146 — the genuine shipped state, not a
// simulated one — sweeps the tie query across every round, then applies 147
// through the real runner and sweeps again.
//
// Red, two independent needles:
//
//  1. structure: neither function's full-text arms carry `, cb.id` at all;
//  2. behaviour: the arms hand back a different candidate set or a different
//     rank assignment once the planner or the heap order changes.
//
// Green: all four full-text sort keys tiebroken in BOTH functions, and every
// round byte-identical to the first — set, ranks, and ctx_rrf's delivery.
func TestC23FTSTiebreakRedGreen(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 146)
	ctx := context.Background()

	for _, fn := range []string{"ctx_rrf", "ctx_rrf_arms"} {
		tb, bare, _ := c23TiebreakSites(t, ctx, pool, fn)
		t.Logf("RED structure %s: %d tiebroken `) DESC, cb.id`, %d bare `) DESC`", fn, tb, bare)
		if tb != 0 {
			t.Fatalf("pre-147 %s already carries %d tiebroken full-text sort keys — the RED state is not the shipped one", fn, tb)
		}
		if bare != 2 {
			t.Fatalf("pre-147 %s carries %d bare full-text sort keys, want 2 (fts_de, fts_en) — the migration chain drifted", fn, bare)
		}
	}

	fx := c23SeedCorpus(t, pool)
	c23AssertFixtureShape(t, ctx, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	q := bw1Query{name: "tie", text: c23TieQuery, spaced: c23TieQuery, mode: "exact"}
	args := bw1Args(q, bw1Embedding(0), nil, c23Limit)

	// ---- RED --------------------------------------------------------------
	red := c23Sweep(t, ctx, pool, conn, fx, args)
	redArm, redSet, redFirst := c23DiffCount(red)
	redRRF := 0
	for i := 1; i < len(red); i++ {
		if red[i].rrfSeq != red[0].rrfSeq {
			redRRF++
		}
	}
	t.Logf("RED (chain capped at 146): %d/%d rounds disagree with round 0 on the full-text arms, %d of them on the candidate SET; ctx_rrf delivered a different sequence in %d rounds",
		redArm, len(red)-1, redSet, redRRF)
	if redFirst != "" {
		t.Logf("RED first divergence: %s", redFirst)
	}
	if redArm == 0 {
		t.Fatalf("RED is vacuous: the untiebroken body stayed identical across all %d rounds, so the behavioural needle proves nothing — sharpen the fixture or the rounds, do not weaken the gate", len(c23Rounds))
	}

	// ---- apply 147 ---------------------------------------------------------
	// Capped at 147 on purpose: an uncapped run would charge anything a later
	// migration does to this one (the mistake 139's gate documents at :296).
	if err := store.RunMigrationsUpTo(ctx, pool, 147); err != nil {
		t.Fatalf("apply migration 147: %v", err)
	}
	for _, fn := range []string{"ctx_rrf", "ctx_rrf_arms"} {
		tb, bare, _ := c23TiebreakSites(t, ctx, pool, fn)
		t.Logf("GREEN structure %s: %d tiebroken `) DESC, cb.id`, %d bare `) DESC`", fn, tb, bare)
		if tb != 4 || bare != 0 {
			t.Errorf("post-147 %s carries %d tiebroken and %d bare full-text sort keys, want 4 and 0 (window + outer ORDER BY, per arm)", fn, tb, bare)
		}
	}

	// ---- GREEN -------------------------------------------------------------
	green := c23Sweep(t, ctx, pool, conn, fx, args)
	greenArm, greenSet, greenFirst := c23DiffCount(green)
	t.Logf("GREEN (after 147): %d/%d rounds disagree on the full-text arms, %d on the candidate SET",
		greenArm, len(green)-1, greenSet)
	if greenArm != 0 {
		t.Errorf("147 left the full-text arms plan-dependent: %d rounds diverge; first: %s", greenArm, greenFirst)
	}
	for i := 1; i < len(green); i++ {
		if green[i].rrfSeq != green[0].rrfSeq {
			t.Errorf("round %d (%s): ctx_rrf delivered a different sequence than round 0 at identical data",
				i, c23Rounds[i].name)
		}
		if green[i].armsAll != green[0].armsAll {
			t.Errorf("round %d (%s): ctx_rrf_arms projected different rows than round 0 at identical data",
				i, c23Rounds[i].name)
		}
	}
	t.Logf("GREEN detail (round 0): fts_de holds %d ids, fts_en %d, ctx_rrf delivered %d rows; %d rounds compared against it",
		strings.Count(green[0].ftsDE, ",")+1, strings.Count(green[0].ftsEN, ",")+1,
		strings.Count(green[0].rrfSeq, ",")+1, len(green)-1)
}

// ---------------------------------------------------------------------------
// Gate 3: ranking non-regression on a tie-free fixture
// ---------------------------------------------------------------------------

// TestC23TieFreeByteIdentical is the promise that the tiebreak is a LAST sort
// key and nothing more: where no two candidates score bit-identically, 147 may
// not move a single value. The whole ctx_rrf delivery and every projected
// ctx_rrf_arms column are compared before and after the migration, exactly —
// not within a tolerance.
func TestC23TieFreeByteIdentical(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 146)
	ctx := context.Background()
	c23SeedCorpus(t, pool)
	c23AssertFixtureShape(t, ctx, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	q := bw1Query{name: "uniq", text: c23UniqQuery, spaced: c23UniqQuery, mode: "exact"}
	args := bw1Args(q, bw1Embedding(1), nil, c23Limit)

	before := c23Measure(t, ctx, conn, args, nil)
	if before.rrfSeq == "" || before.ftsDE == "" || before.ftsEN == "" {
		t.Fatalf("tie-free probe is vacuous: rrf=%d chars, fts_de=%d, fts_en=%d",
			len(before.rrfSeq), len(before.ftsDE), len(before.ftsEN))
	}

	if err := store.RunMigrationsUpTo(ctx, pool, 147); err != nil {
		t.Fatalf("apply migration 147: %v", err)
	}
	after := c23Measure(t, ctx, conn, args, nil)

	if before.ftsDE != after.ftsDE {
		t.Errorf("fts_de changed across 147 on a tie-free fixture: %s", c23Describe(before.ftsDE, after.ftsDE))
	}
	if before.ftsEN != after.ftsEN {
		t.Errorf("fts_en changed across 147 on a tie-free fixture: %s", c23Describe(before.ftsEN, after.ftsEN))
	}
	if before.armsAll != after.armsAll {
		t.Error("ctx_rrf_arms projected different rows across 147 on a tie-free fixture")
	}
	if before.rrfSeq != after.rrfSeq {
		t.Errorf("ctx_rrf delivered a different sequence across 147 on a tie-free fixture:\n before: %s\n after:  %s",
			before.rrfSeq, after.rrfSeq)
	}
	t.Logf("tie-free non-regression: %d fts_de ids, %d fts_en ids, %d delivered rows — byte-identical before and after 147",
		strings.Count(after.ftsDE, ",")+1, strings.Count(after.ftsEN, ",")+1, strings.Count(after.rrfSeq, ",")+1)
}

// ---------------------------------------------------------------------------
// Gate 4: the two bodies stay clause-identical
// ---------------------------------------------------------------------------

// TestC23ArmParityOfTiebreak pins that ctx_rrf and ctx_rrf_arms carry the SAME
// full-text sort keys after 147. The arithmetic parity gate lives in
// arms_parity_integration_test.go and is run separately; this one is the
// textual guard that catches a migration which tiebroke only one of the two —
// the failure mode 140:123-127 names, where the sweep would measure a fusion
// that does not exist.
func TestC23ArmParityOfTiebreak(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	keys := map[string][]string{}
	for _, fn := range []string{"ctx_rrf", "ctx_rrf_arms"} {
		tb, bare, def := c23TiebreakSites(t, ctx, pool, fn)
		if tb != 4 || bare != 0 {
			t.Errorf("full chain installs %s with %d tiebroken and %d bare full-text sort keys, want 4 and 0", fn, tb, bare)
		}
		keys[fn] = c23FTSOrderBlocks(def)
	}
	a, b := keys["ctx_rrf"], keys["ctx_rrf_arms"]
	if len(a) != len(b) {
		t.Fatalf("ctx_rrf carries %d full-text ORDER BY blocks, ctx_rrf_arms %d", len(a), len(b))
	}
	if len(a) != 4 {
		t.Fatalf("expected 4 full-text ORDER BY blocks per function (2 arms x window + outer), found %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("full-text ORDER BY block %d differs between the functions:\n ctx_rrf:      %s\n ctx_rrf_arms: %s", i, a[i], b[i])
		}
	}
	t.Logf("arm parity: all 4 full-text ORDER BY blocks clause-identical between ctx_rrf and ctx_rrf_arms")
}

// ---------------------------------------------------------------------------
// Gate 5: plan shape
// ---------------------------------------------------------------------------

// TestC23PlanShape answers the one cost question this migration raises. The
// tiebreak is written twice per arm — once in the ROW_NUMBER window, once in
// the outer ORDER BY before the LIMIT — and the second copy would be expensive
// if the planner did not recognise that the WindowAgg already delivers rows in
// exactly that order: it would sort the whole full-text candidate set a second
// time, on every query, and at the 1M+ target scale that set is the part that
// grows.
//
// The measurement is the one 139's gate (c) established: a plpgsql body cannot
// be EXPLAINed from the client, so the RETURN QUERY statement is lifted out of
// each migration file verbatim, the p_* parameters are substituted, and that
// SQL is EXPLAINed. Same text the function runs, same arguments, same database.
func TestC23PlanShape(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	c23SeedCorpus(t, pool)

	q := bw1Query{name: "tie", text: c23TieQuery, spaced: c23TieQuery, mode: "exact"}
	args := bw1Args(q, bw1Embedding(0), nil, c23Limit)

	before := bw1bExplain(t, ctx, pool, "145_partial_fts_gin.sql", args)
	after := bw1bExplain(t, ctx, pool, "147_fts_tiebreak.sql", args)

	nBefore, nAfter := bw1bNodeCensus(before), bw1bNodeCensus(after)
	t.Logf("gate 5 node census before (145): %s", bw1bCensusString(nBefore))
	t.Logf("gate 5 node census after  (147): %s", bw1bCensusString(nAfter))
	if bw1bCensusString(nBefore) != bw1bCensusString(nAfter) {
		t.Errorf("plan node census changed across 147:\n before: %s\n after:  %s\n— the tiebreak is meant to extend sort keys, not to add a sort",
			bw1bCensusString(nBefore), bw1bCensusString(nAfter))
	}

	keyBefore, keyAfter := bw1bSortKeys(before), bw1bSortKeys(after)
	if len(keyBefore) != len(keyAfter) {
		t.Fatalf("plan carries %d sort keys before and %d after — that is a plan change, not a key extension",
			len(keyBefore), len(keyAfter))
	}
	// EXPLAIN renames the per-arm alias (cb_3, cb_4, …), so the expected suffix
	// is `, <alias>.id`, not the literal `, cb.id` of the source text.
	c23TiebreakSuffix := regexp.MustCompile(`^, cb(_\d+)?\.id$`)
	changed := 0
	for i := range keyAfter {
		if keyBefore[i] == keyAfter[i] {
			continue
		}
		changed++
		suffix := ""
		if strings.HasPrefix(keyAfter[i], keyBefore[i]) {
			suffix = keyAfter[i][len(keyBefore[i]):]
		}
		if !c23TiebreakSuffix.MatchString(suffix) {
			t.Errorf("sort key %d is not 145's key plus a trailing id tiebreak (suffix %q):\n before: %s\n after:  %s",
				i, suffix, bw1bTail(keyBefore[i], 120), bw1bTail(keyAfter[i], 120))
			continue
		}
		t.Logf("gate 5 sort key %d gained the tiebreak %q: %s", i, suffix, bw1bTail(keyAfter[i], 70))
	}
	if changed != 2 {
		t.Errorf("%d sort keys changed, want exactly 2 — the two WindowAgg input sorts of fulltext_de and fulltext_en. Any more would mean the outer ORDER BY bought its own sort node.", changed)
	}
}

// c23FTSOrderBlocks extracts every `ORDER BY GREATEST( … ) DESC…` block from a
// function body, whitespace-normalised so the two functions' different
// indentation levels do not count as a difference.
func c23FTSOrderBlocks(def string) []string {
	var out []string
	lines := strings.Split(def, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "ORDER BY GREATEST(" {
			continue
		}
		var buf []string
		for j := i; j < len(lines); j++ {
			buf = append(buf, strings.TrimSpace(lines[j]))
			if strings.HasPrefix(strings.TrimSpace(lines[j]), ") DESC") {
				break
			}
		}
		out = append(out, strings.Join(buf, " "))
	}
	return out
}
