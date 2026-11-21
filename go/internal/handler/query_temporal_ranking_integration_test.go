//go:build integration

// Ranking test of the temporal gravity (issue #463): 200 blocks over three
// years, written through store.UpsertBlock so their content_times come from
// the production extraction and given their dimension rows by
// store.RefreshTemporal, ranked by a fixed pseudo-random semantic order and
// then by the production Step 6a (applyTemporalGravity) on what the rules
// parser makes of the question — the cyclic path on the dimension rows and the
// linear path on content_times. No embedding, no FTS, no LLM: the test
// measures what the temporal stage does to an order it is handed,
// deterministically. The rows carry the content anchors only (RefreshTemporal
// with no creation time): the creation time is the clock of the run, the same
// for every block, and would make the cyclic order depend on the day the test
// runs.
//
// Each question names what it expects of the order:
//   - a point question puts the block of its day first;
//   - a window or interval question puts every block whose date lies in the
//     window above every block whose date does not (R-precision 1), and one
//     marked keepsOrder also leaves the blocks inside the window in the order
//     it was handed: a window longer than the cycle of a dimension rewards no
//     block for its phase (issue #463 point 5);
//   - a question the rules leave to the LLM fallback, or about a time no
//     content date can carry, leaves the order as it was.

package handler

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

type rankBlock struct {
	id   string
	date time.Time // zero: the block names no date
}

type rankQuestion struct {
	name, query string
	kind        string // "point", "window", "unchanged"
	day         time.Time
	// from, to: the window, inclusive days; a zero from opens it towards the
	// past, a zero to towards the future.
	from, to   time.Time
	keepsOrder bool
}

func TestQueryTemporalRanking(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) // a Saturday
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

	// 180 blocks every 6 days from 2024-01-03 in the three day forms, 10 in
	// the month form, 2 on fixed recent days, 8 without a date.
	type seed struct {
		content string
		date    time.Time
	}
	var seeds []seed
	start := day(2024, 1, 3)
	for k := 0; k < 180; k++ {
		d := start.AddDate(0, 0, 6*k)
		var text string
		switch k % 3 {
		case 0:
			text = "Eintrag am " + d.Format("2006-01-02") + "."
		case 1:
			text = "Eintrag am " + d.Format("02.01.2006") + "."
		default:
			text = "Eintrag am " + d.Format("2006-01-02") + " 14:30."
		}
		seeds = append(seeds, seed{text, d})
	}
	for i, m := range []time.Month{1, 3, 5, 7, 9, 11, 2, 4, 6, 8} {
		y := 2024 + i%3
		seeds = append(seeds, seed{fmt.Sprintf("Stand %s %d.", germanMonth(m), y), day(y, m, 1)})
	}
	seeds = append(seeds, seed{"Fix am 2026-11-20.", day(2026, 11, 20)}, seed{"Fix am 18.11.2026.", day(2026, 11, 18)})
	for i := 0; i < 8; i++ {
		seeds = append(seeds, seed{fmt.Sprintf("Notiz ohne Datum Nummer %d.", i), time.Time{}})
	}

	blocks := make([]rankBlock, 0, len(seeds))
	for i, s := range seeds {
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("rank463 block %03d", i), s.content,
			nil, nil, "private", true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if _, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, time.Time{}); err != nil {
			t.Fatalf("seed %d: temporal rows: %v", i, err)
		}
		blocks = append(blocks, rankBlock{id: b.ID, date: s.date})
	}
	// The order the temporal stage is handed, twice: flat (every block the
	// same score, so the temporal stage alone decides the order and ties keep
	// the permutation) for the expectations, and graded like RRF (1/(60+rank))
	// for the mean rank the boost reaches against a semantic order.
	order := rand.New(rand.NewPCG(463, 1)).Perm(len(blocks))
	flat := make([]rrf.SearchResult, len(blocks))
	graded := make([]rrf.SearchResult, len(blocks))
	for rank, i := range order {
		flat[rank] = rrf.SearchResult{ID: blocks[i].id, RRFScore: 1}
		graded[rank] = rrf.SearchResult{ID: blocks[i].id, RRFScore: 1 / float64(60+rank+1)}
	}
	byID := make(map[string]rankBlock, len(blocks))
	for _, b := range blocks {
		byID[b.id] = b
	}

	d40, d91 := start.AddDate(0, 0, 6*40), start.AddDate(0, 0, 6*91)
	questions := []rankQuestion{
		{name: "P1 ISO day", query: "Was war am " + d40.Format("2006-01-02") + "?", kind: "point", day: d40},
		{name: "P2 dot day", query: "Was war am " + d91.Format("02.01.2006") + "?", kind: "point", day: d91},
		{name: "P3 gestern", query: "Was war gestern?", kind: "point", day: day(2026, 11, 20)},
		{name: "P4 vor 3 Tagen", query: "Was passierte vor 3 Tagen?", kind: "point", day: day(2026, 11, 18)},
		{name: "P5 day + month name", query: "Was war am 15. März 2025?", kind: "unchanged"},
		{name: "P6 im month", query: "Was war im März 2025?", kind: "unchanged"},
		{name: "W1 ISO window", query: "Was lief von 2025-02-01 bis 2025-04-30?", kind: "window", from: day(2025, 2, 1), to: day(2025, 4, 30)},
		{name: "W2 dot window", query: "Was lief von 01.02.2025 bis 30.04.2025?", kind: "window", from: day(2025, 2, 1), to: day(2025, 4, 30)},
		{name: "W3 letzten Monat", query: "Was war letzten Monat?", kind: "window", from: day(2026, 10, 1), to: day(2026, 10, 31)},
		{name: "W4 letzte Woche", query: "Was war letzte Woche?", kind: "window", from: day(2026, 11, 9), to: day(2026, 11, 15)},
		// A month as a window's end includes that month (point 2).
		{name: "W5 month window", query: "Was lief von März 2025 bis Mai 2025?", kind: "window", from: day(2025, 3, 1), to: day(2025, 5, 31)},
		{name: "O1 seit month", query: "Was hat sich seit März 2026 geändert?", kind: "window", from: day(2026, 3, 1), to: day(2026, 11, 21)},
		{name: "O2 seit ISO", query: "Was hat sich seit 2026-06-01 geändert?", kind: "window", from: day(2026, 6, 1), to: day(2026, 11, 21)},
		{name: "O3 bis month", query: "Was galt bis März 2024?", kind: "window", to: day(2024, 3, 31)},
		{name: "O4 bis dot", query: "Was galt bis 01.06.2024?", kind: "window", to: day(2024, 6, 1)},
		{name: "U1 bis unreachable", query: "Was galt bis März 2015?", kind: "unchanged"},
		{name: "U2 seit unreachable", query: "Was hat sich seit März 2015 geändert?", kind: "window", from: day(2015, 3, 1), to: day(2026, 11, 21)},
		// Point 2: the answer blocks of the last month lie in its second half
		// (2025-04-21, 2025-04-27; 2024-06-19, 2024-06-25).
		{name: "M1 month window, late April", query: "Was lief von Februar 2025 bis April 2025?", kind: "window", from: day(2025, 2, 1), to: day(2025, 4, 30)},
		{name: "M2 bis month, late June", query: "Was galt bis Juni 2024?", kind: "window", to: day(2024, 6, 30)},
		// Point 3: the rules for ab, vor, nach, since, until, before, after,
		// from … to, between/zwischen and the spaced hyphen.
		{name: "A1 ab month", query: "Was gilt ab März 2026?", kind: "window", from: day(2026, 3, 1)},
		{name: "A2 vor month", query: "Was war vor März 2024?", kind: "window", to: day(2024, 2, 29)},
		{name: "A3 nach dot", query: "Was kam nach dem 15.06.2026?", kind: "window", from: day(2026, 6, 16)},
		{name: "A4 since month", query: "What changed since March 2026?", kind: "window", from: day(2026, 3, 1), to: day(2026, 11, 21)},
		{name: "A5 until month", query: "What held until March 2024?", kind: "window", to: day(2024, 3, 31)},
		{name: "A6 between ISO", query: "What happened between 2025-02-01 and 2025-03-15?", kind: "window", from: day(2025, 2, 1), to: day(2025, 3, 15)},
		{name: "A7 zwischen dot", query: "Was lief zwischen 01.02.2025 und 15.03.2025?", kind: "window", from: day(2025, 2, 1), to: day(2025, 3, 15)},
		{name: "A8 spaced hyphen", query: "Was lief 01.02.2025 - 15.03.2025?", kind: "window", from: day(2025, 2, 1), to: day(2025, 3, 15)},
		{name: "A9 before ISO", query: "What happened before 2024-03-01?", kind: "window", to: day(2024, 2, 29)},
		{name: "A10 after month", query: "What happened after March 2026?", kind: "window", from: day(2026, 4, 1)},
		{name: "A11 from … to", query: "What happened from March 2025 to April 2025?", kind: "window", from: day(2025, 3, 1), to: day(2025, 4, 30)},
		// Round 1: "vor" a day to come is the deadline from today; "nach" a
		// past day stays open towards the future (A3).
		{name: "A13 vor a day to come", query: "Was muss vor dem 01.12.2026 erledigt werden?", kind: "window", from: day(2026, 11, 21), to: day(2026, 11, 30)},
		// Point 4: "ab" before a start the rules cannot read is no start.
		{name: "A12 ab Projektstart bis", query: "Was lief ab Projektstart bis 05.03.2026?", kind: "window", to: day(2026, 3, 5)},
		// Point 5: windows longer than the cycle of their cyclic dimension.
		{name: "L1 seit month, 20 months", query: "Was hat sich seit März 2025 geändert?", kind: "window", from: day(2025, 3, 1), to: day(2026, 11, 21), keepsOrder: true},
		{name: "L2 von … bis, 15 months", query: "Was lief von März 2024 bis Mai 2025?", kind: "window", from: day(2024, 3, 1), to: day(2025, 5, 31), keepsOrder: true},
		{name: "L3 bis month, open", query: "Was galt bis März 2026?", kind: "window", to: day(2026, 3, 31), keepsOrder: true},
		{name: "L4 seit ISO, 32 months", query: "Was hat sich seit 2024-03-15 geändert?", kind: "window", from: day(2024, 3, 15), to: day(2026, 11, 21), keepsOrder: true},
	}

	var report strings.Builder
	report.WriteString("question | kind | rules | weights | flat: metric | flat: window order | graded: mean rank of the relevant blocks | flat top10 (block date)\n")
	for _, q := range questions {
		relevant := func(b rankBlock) bool {
			switch q.kind {
			case "point":
				return b.date.Equal(q.day)
			case "window":
				return !b.date.IsZero() && (q.from.IsZero() || !b.date.Before(q.from)) && (q.to.IsZero() || b.date.Before(q.to.AddDate(0, 0, 1)))
			}
			return false
		}
		n := 0
		for _, b := range blocks {
			if relevant(b) {
				n++
			}
		}
		ranked, rules, weights := rankTemporal(ctx, pool, q.query, now, flat)
		var metric, windowOrder string
		var ok bool
		if q.kind == "unchanged" {
			moved := 0
			for i := range ranked {
				if ranked[i].ID != flat[i].ID {
					moved++
				}
			}
			metric = fmt.Sprintf("positions moved %d", moved)
			ok = moved == 0
		} else {
			hit := 0
			for _, res := range ranked[:n] {
				if relevant(byID[res.ID]) {
					hit++
				}
			}
			metric = fmt.Sprintf("R-precision %d/%d = %.3f", hit, n, float64(hit)/float64(n))
			ok = n > 0 && hit == n
			// The blocks inside the window, in the order the stage gave them
			// against the order it was handed.
			var in, out []string
			for _, res := range flat {
				if relevant(byID[res.ID]) {
					in = append(in, res.ID)
				}
			}
			for _, res := range ranked {
				if relevant(byID[res.ID]) {
					out = append(out, res.ID)
				}
			}
			changed := 0
			for i := range in {
				if i >= len(out) || in[i] != out[i] {
					changed++
				}
			}
			windowOrder = fmt.Sprintf("changed at %d of %d", changed, len(in))
			if q.keepsOrder && changed > 0 {
				ok = false
			}
		}
		meanRank := "-"
		if n > 0 {
			rankedGraded, _, _ := rankTemporal(ctx, pool, q.query, now, graded)
			sum := 0
			for i, res := range rankedGraded {
				if relevant(byID[res.ID]) {
					sum += i + 1
				}
			}
			meanRank = fmt.Sprintf("%.1f", float64(sum)/float64(n))
		}
		var top []string
		for _, res := range ranked[:10] {
			if d := byID[res.ID].date; !d.IsZero() {
				top = append(top, d.Format("2006-01-02"))
			} else {
				top = append(top, "none")
			}
		}
		fmt.Fprintf(&report, "%s | %s | %s | %s | %s | %s | %s | %s\n", q.name, q.kind, rules, weights, metric, windowOrder, meanRank, strings.Join(top, " "))
		if !ok {
			t.Errorf("%s (%q): %s, window order %s", q.name, q.query, metric, windowOrder)
		}
	}
	t.Logf("ranking report:\n%s", report.String())
}

// rankTemporal runs the query's Step 6a (applyTemporalGravity) on a copy of
// base, with the rules' answer to query, and describes that answer.
func rankTemporal(ctx context.Context, pool *pgxpool.Pool, query string, now time.Time, base []rrf.SearchResult) ([]rrf.SearchResult, string, string) {
	tr := llm.NormalizeTemporalRules(query, now)
	if tr == nil || len(tr.Dates) == 0 {
		return base, "nil", "-"
	}
	d := tr.Dates[0]
	end := ""
	if d.End != nil {
		end = *d.End
	}
	desc := fmt.Sprintf("%s..%s %s", d.Date, end, d.Dir)
	in := make([]rrf.SearchResult, len(base))
	copy(in, base)
	return applyTemporalGravity(ctx, pool, in, tr, "rank463"), desc, fmt.Sprint(activatedDimWeights(tr))
}

func germanMonth(m time.Month) string {
	return [...]string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}[m-1]
}
