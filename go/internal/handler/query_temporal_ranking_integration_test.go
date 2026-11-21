//go:build integration

// Ranking test of the linear temporal gravity (issue #463): 200 blocks over
// three years, written through store.UpsertBlock so their content_times come
// from the production extraction, ranked by a fixed pseudo-random semantic
// order and then by the production Step 6a linear path — the rules parser on
// the question, collectDimWeights, store.FetchContentTimes, linearGravityParams
// and rrf.ApplyGravityBoost. No embedding, no FTS, no LLM: the test measures
// what the temporal stage does to an order it is handed, deterministically.
//
// Each question names what it expects of the order:
//   - a point question puts the block of its day first;
//   - a window or interval question puts every block whose date lies in the
//     window above every block whose date does not (R-precision 1);
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
	from, to    time.Time // window, inclusive days; zero from = open towards the past
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
		{name: "W5 month window", query: "Was lief von März 2025 bis Mai 2025?", kind: "window", from: day(2025, 3, 1), to: day(2025, 5, 1)},
		{name: "O1 seit month", query: "Was hat sich seit März 2026 geändert?", kind: "window", from: day(2026, 3, 1), to: day(2026, 11, 21)},
		{name: "O2 seit ISO", query: "Was hat sich seit 2026-06-01 geändert?", kind: "window", from: day(2026, 6, 1), to: day(2026, 11, 21)},
		{name: "O3 bis month", query: "Was galt bis März 2024?", kind: "window", to: day(2024, 3, 1)},
		{name: "O4 bis dot", query: "Was galt bis 01.06.2024?", kind: "window", to: day(2024, 6, 1)},
		{name: "U1 bis unreachable", query: "Was galt bis März 2015?", kind: "unchanged"},
		{name: "U2 seit unreachable", query: "Was hat sich seit März 2015 geändert?", kind: "window", from: day(2015, 3, 1), to: day(2026, 11, 21)},
	}

	var report strings.Builder
	report.WriteString("question | kind | rules | flat: metric | graded: mean rank of the relevant blocks | flat top10 (block date)\n")
	for _, q := range questions {
		relevant := func(b rankBlock) bool {
			switch q.kind {
			case "point":
				return b.date.Equal(q.day)
			case "window":
				return !b.date.IsZero() && (q.from.IsZero() || !b.date.Before(q.from)) && b.date.Before(q.to.AddDate(0, 0, 1))
			}
			return false
		}
		n := 0
		for _, b := range blocks {
			if relevant(b) {
				n++
			}
		}
		ranked, rules := rankLinear(ctx, t, pool, q.query, now, flat)
		var metric string
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
		}
		meanRank := "-"
		if n > 0 {
			rankedGraded, _ := rankLinear(ctx, t, pool, q.query, now, graded)
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
		fmt.Fprintf(&report, "%s | %s | %s | %s | %s | %s\n", q.name, q.kind, rules, metric, meanRank, strings.Join(top, " "))
		if !ok {
			t.Errorf("%s (%q): %s", q.name, q.query, metric)
		}
	}
	t.Logf("ranking report:\n%s", report.String())
}

// rankLinear runs the linear path of the query's Step 6a on base, with the
// boost budget of query.go (maxBoost 0.30).
func rankLinear(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, now time.Time, base []rrf.SearchResult) ([]rrf.SearchResult, string) {
	t.Helper()
	tr := llm.NormalizeTemporalRules(query, now)
	if tr == nil || len(tr.Dates) == 0 {
		return base, "nil"
	}
	d := tr.Dates[0]
	end := ""
	if d.End != nil {
		end = *d.End
	}
	desc := fmt.Sprintf("%s..%s %s", d.Date, end, d.Dir)
	target, _ := time.Parse("2006-01-02", d.Date)
	if d.Hour != nil {
		target = time.Date(target.Year(), target.Month(), target.Day(), *d.Hour, 0, 0, 0, target.Location())
	}
	linearWeight, _, _ := collectDimWeights(tr.DimensionWeights)
	if linearWeight <= 0 {
		return base, desc + " (no linear)"
	}
	ids := make([]string, len(base))
	for i, r := range base {
		ids[i] = r.ID
	}
	blockDates, err := store.FetchContentTimes(ctx, pool, ids)
	if err != nil {
		t.Fatalf("fetch content_times: %v", err)
	}
	in := make([]rrf.SearchResult, len(base))
	copy(in, base)
	return rrf.ApplyGravityBoost(in, blockDates, linearGravityParams(d, target, 0.30*linearWeight)), desc
}

func germanMonth(m time.Month) string {
	return [...]string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}[m-1]
}
