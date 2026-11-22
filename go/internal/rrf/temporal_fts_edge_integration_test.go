//go:build integration

// Ranking test of the full-text expansion of a window's edge (issue #487
// point 1): rrf.Search with its full-text arms on blocks written through
// store.UpsertBlock (their full-text vectors come from the production
// trigger) and embedded through store.StoreEmbedding with a deterministic
// vector per block, so the semantic arm hands every question the same
// pseudo-random order. Per question the test runs three expansions of the
// window the rules derive and the order without one:
//
//   - shifted: the form until #487 — the window's edge day as the rules shift
//     it (the day after "nach X", the day before "vor X") plus its month;
//   - named: the date the question names plus its month (a month as such, a
//     year as such);
//   - none: no expansion for a window open towards the past or the future.
//
// It measures how many blocks inside the window and outside it the terms
// match, and how many pairs of an out-of-window block and an in-window block
// the expansion turns in favour of the out-of-window block against the order
// without an expansion. The expansion the query path builds
// (llm.TemporalToFTSExpansion of the rules' answer) must turn none: a block
// that only carries the edge's date or month must not climb over the blocks
// the question asks for.

package rrf_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ftsEdgeScope = "fts487"

type ftsEdgeBlock struct {
	id   string
	date time.Time
}

type ftsEdgeQuestion struct {
	name, query string
	// from, to: the window, inclusive days; zero opens it on that side.
	from, to time.Time
	// shifted and named are the two expansions measured beside none.
	shifted, named string
	// control: a closed window, measured and reported, not asserted.
	control bool
}

func TestTemporalFTSExpansionOfAnOpenEdge(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	visible := t5Registry(t, ctx, pool).Snapshot().VisibleTypes()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

	// A block every 4 days from 2023-11-03 to 2026-11-19, in the ISO form,
	// the ISO form with a time and the dot form in turn, plus the edge days
	// of the questions below in the ISO form, plus 8 blocks without a date.
	type seed struct {
		content string
		date    time.Time
	}
	var seeds []seed
	for d, k := day(2023, 11, 3), 0; !d.After(day(2026, 11, 19)); d, k = d.AddDate(0, 0, 4), k+1 {
		var text string
		switch k % 3 {
		case 0:
			text = "Eintrag am " + d.Format("2006-01-02") + "."
		case 1:
			text = "Eintrag am " + d.Format("2006-01-02") + " 14:30."
		default:
			text = "Eintrag am " + d.Format("02.01.2006") + "."
		}
		seeds = append(seeds, seed{text, d})
	}
	for _, d := range []time.Time{day(2024, 3, 15), day(2024, 3, 16), day(2024, 3, 31), day(2024, 4, 1),
		day(2024, 2, 29), day(2024, 3, 1), day(2024, 6, 1), day(2024, 6, 2), day(2024, 12, 31),
		day(2025, 1, 1), day(2025, 12, 31), day(2026, 1, 1)} {
		seeds = append(seeds, seed{"Notiz vom " + d.Format("2006-01-02") + ".", d})
	}
	for i := 0; i < 8; i++ {
		seeds = append(seeds, seed{fmt.Sprintf("Notiz ohne Datum Nummer %d.", i), time.Time{}})
	}
	rng := rand.New(rand.NewPCG(487, 1))
	vector := func() []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = float32(rng.Float64()*2 - 1)
		}
		return v
	}
	blocks := make([]ftsEdgeBlock, 0, len(seeds))
	byID := make(map[string]ftsEdgeBlock, len(seeds))
	for i, s := range seeds {
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("fts487 block %03d", i), s.content,
			nil, nil, ftsEdgeScope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "stub-fts487", vector()); err != nil {
			t.Fatalf("seed %d: embedding: %v", i, err)
		}
		blocks = append(blocks, ftsEdgeBlock{id: b.ID, date: s.date})
		byID[b.ID] = blocks[len(blocks)-1]
	}
	queryVec := vector()

	questions := []ftsEdgeQuestion{
		{name: "nach ISO day", query: "Was kam nach 2024-03-15?", from: day(2024, 3, 16),
			shifted: "2024-03-16 OR 2024-03", named: "2024-03-15 OR 2024-03"},
		{name: "nach dot day", query: "Was kam nach dem 31.03.2024?", from: day(2024, 4, 1),
			shifted: "2024-04-01 OR 2024-04", named: "2024-03-31 OR 2024-03"},
		{name: "vor month", query: "Was war vor März 2024?", to: day(2024, 2, 29),
			shifted: "2024-02-29 OR 2024-02", named: "2024-03"},
		{name: "bis dot day (#469)", query: "Was galt bis 01.06.2024?", to: day(2024, 6, 1),
			shifted: "2024-06-01 OR 2024-06", named: "2024-06-01 OR 2024-06"},
		{name: "bis month (#469)", query: "Was galt bis März 2024?", to: day(2024, 3, 31),
			shifted: "2024-03-31 OR 2024-03", named: "2024-03"},
		{name: "nach year", query: "Was kam nach 2025?", from: day(2026, 1, 1),
			shifted: "2026-01-01 OR 2026-01", named: "2025"},
		{name: "vor year", query: "Was galt vor 2025?", to: day(2024, 12, 31),
			shifted: "2024-12-31 OR 2024-12", named: "2025"},
		{name: "seit ISO day (closed, control)", query: "Was hat sich seit 2024-03-15 geändert?", from: day(2024, 3, 15), to: day(2026, 11, 22),
			shifted: "2024-03-15 OR 2024-03 OR 2026-11-22 OR 2026-11", named: "2024-03-15 OR 2024-03", control: true},
	}

	var report strings.Builder
	report.WriteString("question | rules | variant | terms | hits in/out of window | out-of-window lifted over in-window (pairs, blocks) | out-of-window in top 10\n")
	for _, q := range questions {
		inWindow := func(b ftsEdgeBlock) bool {
			return !b.date.IsZero() && (q.from.IsZero() || !b.date.Before(q.from)) && (q.to.IsZero() || !b.date.After(q.to))
		}
		tr := llm.NormalizeTemporalRules(q.query, now)
		if tr == nil || len(tr.Dates) == 0 {
			t.Fatalf("%s (%q): the rules derive no window", q.name, q.query)
		}
		d := tr.Dates[0]
		rules := d.Date + ".." + deref(d.End) + " " + d.Dir
		production := llm.TemporalToFTSExpansion(tr.Dates)
		base := ftsEdgeSearch(t, ctx, pool, queryVec, q.query, "", visible)
		for _, v := range []struct{ name, terms string }{
			{"shifted (until #487)", q.shifted}, {"named", q.named}, {"none", ""}, {"query path", production},
		} {
			ranked := base
			if v.terms != "" {
				ranked = ftsEdgeSearch(t, ctx, pool, queryVec, q.query, v.terms, visible)
			}
			hitIn, hitOut := ftsEdgeHits(t, ctx, pool, v.terms, byID, inWindow)
			pairs, lifted := liftedOver(base, ranked, byID, inWindow)
			top := 0
			for _, id := range ranked[:min(10, len(ranked))] {
				if b := byID[id]; !b.date.IsZero() && !inWindow(b) {
					top++
				}
			}
			fmt.Fprintf(&report, "%s | %s | %s | %q | %d/%d | %d, %d | %d\n", q.name, rules, v.name, v.terms, hitIn, hitOut, pairs, lifted, top)
			if v.name == "query path" && !q.control && pairs > 0 {
				t.Errorf("%s (%q): the query path's expansion %q lifts %d out-of-window blocks over in-window blocks (%d pairs)",
					q.name, q.query, v.terms, lifted, pairs)
			}
		}
	}
	t.Logf("full-text expansion of a window's edge:\n%s", report.String())
}

// ftsEdgeSearch runs rrf.Search as the query path does (Step 5: the hyphens
// of the question as spaces for querySpaced, no OR query, 200 candidates)
// and returns the ids in rank order.
func ftsEdgeSearch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, emb []float32, query, temporal string, visible []string) []string {
	t.Helper()
	res, _, err := rrf.Search(ctx, pool, emb, query, strings.ReplaceAll(query, "-", " "), []string{ftsEdgeScope},
		nil, nil, 200, temporal, "", visible, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search(%q, %q): %v", query, temporal, err)
	}
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.ID
	}
	return ids
}

// ftsEdgeHits counts the dated blocks whose German or English full-text
// vector matches the expansion terms, inside and outside the window, by the
// condition the full-text arms of ctx_rrf put on p_temporal.
func ftsEdgeHits(t *testing.T, ctx context.Context, pool *pgxpool.Pool, terms string, byID map[string]ftsEdgeBlock, inWindow func(ftsEdgeBlock) bool) (in, out int) {
	t.Helper()
	if terms == "" {
		return 0, 0
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1
		AND (ts_de @@ websearch_to_tsquery('simple', $2) OR ts_en @@ websearch_to_tsquery('simple', $2))`, ftsEdgeScope, terms)
	if err != nil {
		t.Fatalf("hits of %q: %v", terms, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if b := byID[id]; inWindow(b) {
			in++
		} else if !b.date.IsZero() {
			out++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return in, out
}

// liftedOver counts the pairs of a dated out-of-window block and an
// in-window block that ranked the in-window block first without an
// expansion (an absent block ranks last) and the out-of-window block first
// with it, and the out-of-window blocks in such a pair.
func liftedOver(base, ranked []string, byID map[string]ftsEdgeBlock, inWindow func(ftsEdgeBlock) bool) (pairs, blocks int) {
	pos := func(order []string) map[string]int {
		m := make(map[string]int, len(order))
		for i, id := range order {
			m[id] = i
		}
		return m
	}
	before, after := pos(base), pos(ranked)
	rank := func(m map[string]int, id string) int {
		if p, ok := m[id]; ok {
			return p
		}
		return len(byID)
	}
	for oid, o := range byID {
		if o.date.IsZero() || inWindow(o) {
			continue
		}
		n := 0
		for wid, w := range byID {
			if !inWindow(w) {
				continue
			}
			if rank(before, wid) < rank(before, oid) && rank(after, oid) < rank(after, wid) {
				n++
			}
		}
		pairs += n
		if n > 0 {
			blocks++
		}
	}
	return pairs, blocks
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
