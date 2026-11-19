//go:build integration

// Issue #396: the routes of a guard check answer alike where ctx_guard_check
// (migration 173) used to part from the exact statement of a small circle:
//
//   - a vector of zero norm in halfvec(1024) has no cosine distance (NaN,
//     which passes every ">=" threshold). On the checked side and on the
//     candidate side a nearest candidate at NaN counts as no candidate:
//     clean, similarity 0, no match, the block stays live;
//   - a neighbour whose title is longer than 255 characters (one-byte and
//     multibyte) is answered with its whole title; the function's
//     VARCHAR(255) variable failed the check with 22001 instead, and the
//     checked block stayed pending for good.
//
// The routes: ctx_guard_check through its HNSW index with the batch's plan
// (checkBlock, exact false), the same call when there is no HNSW index to
// serve it (the plan the switches disable), and the exact statement
// (checkBlock, exact true) — one answer each, column by column, title
// included; then each batch route end to end (RunGuardBatch with exact limit
// 0 and with the default), the verdict read from the audit log and the block.
// Fixtures go through the production write path (store.UpsertBlock,
// store.StoreEmbedding, store.CreateTenant, store.AssignTenantScope).
package guard_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/testdb"
)

// p396Block is a block of a case fixture: role names it in the answers.
type p396Block struct {
	role, title string
	vec         []float32
}

// p396Case: the neighbours are written and checked first, then the checked
// block; want is the answer for the checked block on every route.
type p396Case struct {
	name       string
	neighbours []p396Block
	checked    p396Block
	wantDup    bool   // near_duplicate (else clean, similarity 0)
	wantMatch  string // role of the matched neighbour, "" for none
}

// p396Answer is one route's answer for the checked block, the match by role.
type p396Answer struct {
	decision, sim, match, title string
	cross                       bool
	err                         string
}

func (a p396Answer) String() string {
	title := a.title
	if n := utf8.RuneCountInString(title); n > 12 {
		title = fmt.Sprintf("%s…(%d chars)", string([]rune(title)[:6]), n)
	}
	return fmt.Sprintf("%s|%s|%s|%q|%v|%s", a.decision, a.sim, a.match, title, a.cross, a.err)
}

func p396Cases(rng *rand.Rand) []p396Case {
	var cases []p396Case
	for _, f := range epZeroNormForms {
		cases = append(cases,
			p396Case{
				name:       "zero-norm checked/" + f.name,
				neighbours: []p396Block{{"N", "n", epVec(rng)}},
				checked:    p396Block{"C", "c", f.vec},
			},
			p396Case{
				name:       "zero-norm candidate/" + f.name,
				neighbours: []p396Block{{"Z", "z", f.vec}},
				checked:    p396Block{"C", "c", epVec(rng)},
			})
	}
	// A zero-norm candidate does not hide a near one.
	v := epVec(rng)
	cases = append(cases, p396Case{
		name:       "zero-norm candidate beside a near one",
		neighbours: []p396Block{{"Z", "z", epZeroNormForms[0].vec}, {"N", "n", epNear(rng, v, 0.995)}},
		checked:    p396Block{"C", "c", v},
		wantDup:    true, wantMatch: "N",
	})
	// Titles by characters: one byte, two, four; 255 is the old limit.
	for _, tc := range []struct {
		r     string
		chars int
	}{{"x", 255}, {"x", 256}, {"x", 1000}, {"ä", 255}, {"ä", 256}, {"ä", 1000}, {"😀", 256}} {
		v := epVec(rng)
		cases = append(cases, p396Case{
			name:       fmt.Sprintf("title %d × %q", tc.chars, tc.r),
			neighbours: []p396Block{{"N", "T" + strings.Repeat(tc.r, tc.chars-1), v}},
			checked:    p396Block{"C", "c", epNear(rng, v, 0.995)},
			wantDup:    true, wantMatch: "N",
		})
	}
	// Over 255 characters, the excess blanks only: the implicit VARCHAR(255)
	// coercion cut them instead of failing; the whole title now.
	v = epVec(rng)
	cases = append(cases, p396Case{
		name:       "title 257 ending in blanks",
		neighbours: []p396Block{{"N", "T" + strings.Repeat("y", 251) + "     ", v}},
		checked:    p396Block{"C", "c", epNear(rng, v, 0.995)},
		wantDup:    true, wantMatch: "N",
	})
	return cases
}

// want is the answer every route must give; the similarity of a match is
// compared across routes, not to a literal.
func (c p396Case) want(sim string) p396Answer {
	if !c.wantDup {
		return p396Answer{decision: "clean", sim: "0"}
	}
	for _, n := range c.neighbours {
		if n.role == c.wantMatch {
			return p396Answer{decision: "near_duplicate", sim: sim, match: n.role, title: n.title}
		}
	}
	panic("case " + c.name + ": no neighbour " + c.wantMatch)
}

func p396Code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return err.Error()
}

// p396Statements answers the check of id on the three routes inside one
// transaction (rolled back): the batch's window and jit as prepareChecks sets
// them, each route in a savepoint so a failing one leaves the next its
// transaction. It also returns the HNSW blocks the index route read.
func p396Statements(t *testing.T, pool *pgxpool.Pool, set *blocktype.Set, id string, roles map[string]string) (map[string]p396Answer, int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('hnsw.ef_search', '800', true), set_config('jit', 'off', true)`); err != nil {
		t.Fatal(err)
	}
	dup, rev := set.GuardThresholds("knowledge")
	run := func(exact bool) p396Answer {
		if _, err := tx.Exec(ctx, "SAVEPOINT p396"); err != nil {
			t.Fatal(err)
		}
		r, err := guard.CheckBlockForTest(ctx, tx, id, dup, rev, set.GuardCandidateTypes(), false, exact)
		if err != nil {
			if _, rerr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT p396"); rerr != nil {
				t.Fatal(rerr)
			}
			return p396Answer{err: p396Code(err)}
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT p396"); err != nil {
			t.Fatal(err)
		}
		a := p396Answer{decision: r.Decision, sim: strconv.FormatFloat(r.Similarity, 'f', -1, 64), cross: r.IsCrossScope}
		if r.MatchedID != nil {
			a.match = roles[*r.MatchedID]
			if a.match == "" {
				a.match = "?" + *r.MatchedID
			}
		}
		if r.MatchedTitle != nil {
			a.title = *r.MatchedTitle
		}
		return a
	}
	hnsw := func() int64 {
		var n int64
		if err := tx.QueryRow(ctx, epHNSWBlocksSQL).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	out := map[string]p396Answer{}
	h0 := hnsw()
	out["index"] = run(false)
	read := hnsw() - h0
	out["exact"] = run(true)
	// ctx_guard_check without an HNSW index to serve it: the planner falls
	// back on the plan the batch's switches disable (a REINDEX, a dropped or
	// invalid index).
	if _, err := tx.Exec(ctx, `DROP INDEX idx_embedding_hnsw`); err != nil {
		t.Fatal(err)
	}
	out["function without index"] = run(false)
	return out, read
}

// TestGuardRoutesAnswerAlike (#396): per case, on a database of its own for
// each batch route, the three statement routes give the case's answer, and
// both batch routes leave the same verdict on the checked block.
func TestGuardRoutesAnswerAlike(t *testing.T) {
	rng := rand.New(rand.NewSource(3961))
	set := blocktype.NewRegistry().Snapshot()
	for _, c := range p396Cases(rng) {
		t.Run(c.name, func(t *testing.T) {
			verdicts := map[string]epVerdict{}
			sims := map[string]string{}
			for _, route := range []struct {
				name   string
				search guard.Search
			}{{"index", guard.SearchIndexForTest}, {"exact", guard.SearchDefaultForTest}} {
				pool := testdb.SetupTestDB(t)
				plain := epOwnPool(t, pool, nil)
				scope := "p396-" + route.name
				epTenant(t, pool, scope, scope)
				roles := map[string]string{}
				for _, n := range c.neighbours {
					roles[epWrite(t, pool, scope, "knowledge", n.title, n.vec)] = n.role
				}
				if _, n := epRun(t, plain, set, 10, route.search); n != len(c.neighbours) {
					t.Fatalf("%s: first batch processed %d, want %d", route.name, n, len(c.neighbours))
				}
				id := epWrite(t, pool, scope, "knowledge", c.checked.title, c.checked.vec)

				answers, read := p396Statements(t, pool, set, id, roles)
				t.Logf("%s fixture: the index route read %d HNSW blocks", route.name, read)
				// One block is the metapage, read when the planner prices the
				// index; a scan of the graph reads more.
				if read < 2 {
					t.Errorf("%s fixture: the index route read %d HNSW blocks, want the graph searched", route.name, read)
				}
				want := c.want(answers["exact"].sim)
				for _, r := range []string{"index", "exact", "function without index"} {
					if got := answers[r]; got != want {
						t.Errorf("%s fixture, statement route %s:\n got  %s\n want %s", route.name, r, got, want)
					}
				}

				p, n := epRun(t, plain, set, 10, route.search)
				if n != 1 {
					t.Fatalf("%s: batch over the checked block processed %d, want 1 (the block stays pending)", route.name, n)
				}
				if (route.name == "exact") != (p.exactChecks == 1) || p.exactChecks+p.indexChecks != 1 {
					t.Fatalf("%s: %d exact and %d index checks", route.name, p.exactChecks, p.indexChecks)
				}
				v := epVerdictOf(t, pool, id)
				if v.matched != "" {
					v.matched = roles[v.matched]
				}
				verdicts[route.name] = v
				sims[route.name] = want.sim
			}

			want := epVerdict{decision: "clean", sim: "0", status: "clean"}
			if c.wantDup {
				want = epVerdict{decision: "near_duplicate", sim: verdicts["exact"].sim, matched: c.wantMatch,
					status: "archived_dup", archived: true}
				if s, err := strconv.ParseFloat(want.sim, 64); err != nil || s < 0.98 {
					t.Errorf("near-duplicate similarity %q, want >= 0.98", want.sim)
				}
			}
			for route, v := range verdicts {
				if v != want {
					t.Errorf("batch route %s: verdict %+v, want %+v", route, v, want)
				}
			}
			if sims["index"] != sims["exact"] {
				t.Errorf("statement similarity: index fixture %s, exact fixture %s (same vectors)", sims["index"], sims["exact"])
			}
		})
	}
}
