//go:build integration

// Ranking test of the full-text expansion of a window open towards the past
// or the future, measured after the gravity (issue #487 point 1, review
// round): rrf.Search with its full-text arms on blocks written through
// store.UpsertBlock (their full-text vectors from the production trigger,
// their content_times from the production extraction), embedded with a
// deterministic vector per block so the semantic arm hands every question
// the same pseudo-random order, then the query's Step 6a (applyTemporalGravity,
// the production function) on what the rules parser makes of the question.
// The measure is the number of blocks inside the question's window among the
// first ten after the gravity, per expansion:
//
//   - base: the form of v5.75.0 — the window's edge day as the rules shift it
//     (the day after "nach X", the day before "vor X") plus its month;
//   - none: no expansion for an open window;
//   - named: the date the question names plus its month (a month as such, a
//     year as such);
//   - hybrid: the edge's month only where the whole month lies inside the
//     window, the edge's day only where the question names that day.
//
// The corpus has blocks the question's words hit inside and outside the
// window ("Deploy der Suche am …", "Deploy der Suche im März 2024"), so the
// question's own full-text arm competes with the expansion as it does live.
// A variant replaces the base form only if it brings fewer in-window blocks
// into the first ten in no question and more in at least one; the test holds
// the query path's expansion (llm.TemporalToFTSExpansion) to that rule.

package handler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ftsGravityScope = "fts487g"

type ftsGravityQuestion struct {
	query string
	// named is the expansion of the date the question names; namedDay the
	// day it names, "" for a month or a year.
	named, namedDay string
}

func TestQueryTemporalFTSExpansionAfterGravity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	visible := reg.Snapshot().VisibleTypes()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

	type seed struct {
		content string
		date    time.Time
	}
	var seeds []seed
	// A block every 4 days from 2023-11-03 to 2026-11-19 in the ISO form,
	// the ISO form with a time and the dot form in turn.
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
	// Blocks the question's words hit: every 10 days from 2023-11-05, in the
	// ISO and the dot form, and in the month form for months around the
	// questions' edges.
	for d, k := day(2023, 11, 5), 0; !d.After(day(2026, 11, 19)); d, k = d.AddDate(0, 0, 10), k+1 {
		form := d.Format("2006-01-02")
		if k%2 == 1 {
			form = d.Format("02.01.2006")
		}
		seeds = append(seeds, seed{"Deploy der Suche am " + form + ".", d})
	}
	for _, m := range []struct {
		y int
		m time.Month
	}{{2024, 1}, {2024, 2}, {2024, 3}, {2024, 4}, {2024, 6}, {2024, 12}, {2025, 1}, {2025, 3}, {2025, 12}, {2026, 1}} {
		seeds = append(seeds, seed{fmt.Sprintf("Deploy der Suche im %s %d.", germanMonth(m.m), m.y), day(m.y, m.m, 1)})
	}
	// The edge days of the questions, with and without the question's words.
	for _, d := range []time.Time{day(2024, 3, 15), day(2024, 3, 16), day(2024, 3, 14), day(2024, 3, 31), day(2024, 4, 1),
		day(2024, 2, 29), day(2024, 3, 1), day(2024, 6, 1), day(2024, 6, 2), day(2024, 12, 31), day(2025, 1, 1),
		day(2025, 12, 31), day(2026, 1, 1)} {
		seeds = append(seeds, seed{"Notiz vom " + d.Format("2006-01-02") + ".", d},
			seed{"Deploy der Suche am " + d.Format("2006-01-02") + ".", d})
	}
	for i := 0; i < 8; i++ {
		seeds = append(seeds, seed{fmt.Sprintf("Notiz ohne Datum Nummer %d.", i), time.Time{}})
	}
	rng := rand.New(rand.NewPCG(487, 2))
	vector := func() []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = float32(rng.Float64()*2 - 1)
		}
		return v
	}
	dates := make(map[string]time.Time, len(seeds))
	for i, s := range seeds {
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("fts487g block %03d", i), s.content,
			nil, nil, ftsGravityScope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "stub-fts487g", vector()); err != nil {
			t.Fatalf("seed %d: embedding: %v", i, err)
		}
		dates[b.ID] = s.date
	}
	queryVec := vector()

	questions := []ftsGravityQuestion{
		// The review's seven questions, whose words hit no block.
		{"Was kam nach 2024-03-15?", "2024-03-15 OR 2024-03", "2024-03-15"},
		{"Was kam nach dem 31.03.2024?", "2024-03-31 OR 2024-03", "2024-03-31"},
		{"Was war vor März 2024?", "2024-03", ""},
		{"Was galt bis 01.06.2024?", "2024-06-01 OR 2024-06", "2024-06-01"},
		{"Was galt bis März 2024?", "2024-03", ""},
		{"Was kam nach 2025?", "2025", ""},
		{"Was galt vor 2025?", "2025", ""},
		// Questions whose words hit blocks inside and outside the window.
		{"Deploy Suche nach 2024-03-15", "2024-03-15 OR 2024-03", "2024-03-15"},
		{"Deploy Suche nach dem 31.03.2024", "2024-03-31 OR 2024-03", "2024-03-31"},
		{"Deploy Suche vor dem 15.03.2024", "2024-03-15 OR 2024-03", "2024-03-15"},
		{"Deploy Suche ab 15.03.2024", "2024-03-15 OR 2024-03", "2024-03-15"},
		{"Deploy Suche bis 01.06.2024", "2024-06-01 OR 2024-06", "2024-06-01"},
		{"Deploy Suche vor März 2024", "2024-03", ""},
		{"Deploy Suche ab März 2024", "2024-03", ""},
		{"Deploy Suche bis März 2024", "2024-03", ""},
		{"Deploy Suche nach 2025", "2025", ""},
		{"Deploy Suche vor 2025", "2025", ""},
		{"Deploy Suche ab 2025", "2025", ""},
		{"Deploy Suche bis 2024", "2024", ""},
		{"search deploy after 2025", "2025", ""},
		{"search deploy before March 2024", "2024-03", ""},
	}
	variants := []string{"base", "none", "named", "hybrid"}

	var report strings.Builder
	report.WriteString("question | window | question-word hits in/out | in-window in top 10 after gravity: base / none / named / hybrid | query path\n")
	sum := map[string]int{}
	worse := map[string]int{}
	better := map[string]int{}
	var queryPathWorse []string
	for _, q := range questions {
		tr := llm.NormalizeTemporalRules(q.query, now)
		if tr == nil || len(tr.Dates) != 1 || tr.Dates[0].End != nil || (tr.Dates[0].Dir != llm.DirFrom && tr.Dates[0].Dir != llm.DirUntil) {
			t.Fatalf("%q: the rules derive no window open on one side: %+v", q.query, tr)
		}
		d := tr.Dates[0]
		edge, _ := time.Parse("2006-01-02", d.Date)
		inWindow := func(id string) bool {
			bd := dates[id]
			if bd.IsZero() {
				return false
			}
			if d.Dir == llm.DirFrom {
				return !bd.Before(edge)
			}
			return !bd.After(edge)
		}
		terms := map[string]string{
			"base":   d.Date + " OR " + d.Date[:7],
			"none":   "",
			"named":  q.named,
			"hybrid": hybridExpansion(d, edge, q.namedDay),
		}
		counts := map[string]int{}
		for _, v := range variants {
			counts[v] = inWindowTop10(t, ctx, pool, queryVec, q.query, terms[v], visible, tr, inWindow)
			sum[v] += counts[v]
		}
		path := llm.TemporalToFTSExpansion(tr.Dates)
		pathCount := inWindowTop10(t, ctx, pool, queryVec, q.query, path, visible, tr, inWindow)
		for _, v := range variants[1:] {
			if counts[v] < counts["base"] {
				worse[v]++
			}
			if counts[v] > counts["base"] {
				better[v]++
			}
		}
		if pathCount < counts["base"] {
			queryPathWorse = append(queryPathWorse, fmt.Sprintf("%q: %d < %d", q.query, pathCount, counts["base"]))
		}
		hin, hout := questionWordHits(t, ctx, pool, q.query, inWindow)
		fmt.Fprintf(&report, "%s | %s %s | %d/%d | %d / %d / %d / %d | %q %d\n", q.query, d.Date, d.Dir, hin, hout,
			counts["base"], counts["none"], counts["named"], counts["hybrid"], path, pathCount)
	}
	for _, v := range variants {
		fmt.Fprintf(&report, "sum %s: %d", v, sum[v])
		if v != "base" {
			fmt.Fprintf(&report, " (fewer than base in %d questions, more in %d)", worse[v], better[v])
		}
		report.WriteString("\n")
	}
	t.Logf("in-window blocks among the first ten after the gravity:\n%s", report.String())
	if len(queryPathWorse) > 0 {
		t.Errorf("the query path's expansion brings fewer in-window blocks into the first ten than the base form:\n  %s",
			strings.Join(queryPathWorse, "\n  "))
	}
}

// hybridExpansion is the hybrid variant of the expansion of the open window
// d with the edge day edge: the edge's month where the whole month lies
// inside the window, the edge's day where the question names it (namedDay).
func hybridExpansion(d llm.TemporalDate, edge time.Time, namedDay string) string {
	var terms []string
	if d.Date == namedDay {
		terms = append(terms, d.Date)
	}
	if d.Dir == llm.DirFrom && edge.Day() == 1 || d.Dir == llm.DirUntil && edge.AddDate(0, 0, 1).Day() == 1 {
		terms = append(terms, d.Date[:7])
	}
	return strings.Join(terms, " OR ")
}

// inWindowTop10 runs rrf.Search as the query path does (the hyphens of the
// question as spaces for querySpaced, no OR query, 200 candidates) with the
// expansion terms, then the production gravity stage on tr, and counts the
// in-window blocks among the first ten.
func inWindowTop10(t *testing.T, ctx context.Context, pool *pgxpool.Pool, emb []float32, query, terms string, visible []string, tr *llm.TemporalResult, inWindow func(string) bool) int {
	t.Helper()
	res, _, err := rrf.Search(ctx, pool, emb, query, strings.ReplaceAll(query, "-", " "), []string{ftsGravityScope},
		nil, nil, 200, terms, "", visible, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search(%q, %q): %v", query, terms, err)
	}
	res = applyTemporalGravity(ctx, pool, res, tr, "fts487g")
	n := 0
	for _, r := range res[:min(10, len(res))] {
		if inWindow(r.ID) {
			n++
		}
	}
	return n
}

// questionWordHits counts the dated blocks the question's own full-text arms
// match, inside and outside the window, by the conditions of ctx_rrf.
func questionWordHits(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, inWindow func(string) bool) (in, out int) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1
		AND (ts_de @@ plainto_tsquery('german', $2) OR ts_de @@ plainto_tsquery('german', $3)
		  OR ts_en @@ plainto_tsquery('english', $2) OR ts_en @@ plainto_tsquery('english', $3))`,
		ftsGravityScope, query, strings.ReplaceAll(query, "-", " "))
	if err != nil {
		t.Fatalf("question-word hits of %q: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if inWindow(id) {
			in++
		} else {
			out++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return in, out
}
