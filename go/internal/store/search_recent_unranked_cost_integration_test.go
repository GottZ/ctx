//go:build integration

// Cost gate of issue #342: a compact search page and a recent page read the
// content of the rows ON the page, not of every row the statement sorts.
//
// Before #342 the compact statements computed LEFT(content, 200) and
// length(content) in the scan, below the sort: every visible row had its
// content read out of TOAST to fill a preview that the LIMIT then threw away.
// The planner does not postpone those two expressions past the sort on its own
// (it postpones only expressions it costs as expensive, and both are costed as
// one operator each). Blocks of a type that retrieval never ranks (checkpoint
// parts, about 35 000 characters each) are most of that content on the
// reference instance, so the cost of browse and recent grew with their volume.
//
// The fixture is two stores with the same ranked blocks: one with never-ranked
// blocks of transcript width beside them (57 % of the rows, as on the
// reference instance), one without. The newest blocks are ranked ones in both,
// so the page itself is the same and the gate measures only the rows the page
// does not show. What those rows may still cost the statements before 195 is
// their heap tuples — the sort on updated_at had no index (issue #237) — so
// the bound for recent and browse is the cost on the store without them,
// scaled by the ratio of the two heaps, plus a quarter; the walk over the
// indexes of 195 reads the page's rows on either store. For the full-text path the bound is the full
// (non-compact) statement on the same store: it reads the same rows and ranks
// them the same way, and never had the preview expressions.
package store_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// unrankedVocab is the vocabulary size of the generated text.
const unrankedVocab = 20000

// unrankedWords is a deterministic vocabulary of word-like tokens of three or
// more syllables (no stop word of either text-search configuration).
var unrankedWords = func() []string {
	const cons, vow = "bdfgklmnprstvz", "aeiou"
	syl := make([]string, 0, len(cons)*len(vow))
	for _, c := range cons {
		for _, v := range vow {
			syl = append(syl, string(c)+string(v))
		}
	}
	out := make([]string, unrankedVocab)
	for i := range out {
		n := i + len(syl)*len(syl)
		var b strings.Builder
		for n > 0 {
			b.WriteString(syl[n%len(syl)])
			n /= len(syl)
		}
		b.WriteString("r")
		out[i] = b.String()
	}
	return out
}()

// unrankedText is about n characters of deterministic Zipf-distributed words
// with transcript-like message headers (a multi-byte dash among them), plus
// marker repeated markerTimes times near the start.
func unrankedText(seed uint64, n int, marker string, markerTimes int) string {
	rng := rand.New(rand.NewPCG(342, seed))
	z := rand.NewZipf(rng, 1.07, 1, unrankedVocab-1)
	var b strings.Builder
	b.Grow(n + 64)
	for i := 0; i < markerTimes; i++ {
		b.WriteString(marker + " ")
	}
	for w := 0; b.Len() < n; w++ {
		if w%400 == 0 {
			fmt.Fprintf(&b, "\n### Message %d — user\n", w/400)
		}
		b.WriteString(unrankedWords[z.Uint64()])
		if w%13 == 12 {
			b.WriteString(".\n")
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// unrankedShape is one generated store: ranked blocks (type knowledge, 500 to
// 6 000 characters) and, unless omitted, never-ranked blocks (type checkpoint,
// unrankedWidth characters). Every ranked block is newer than every
// never-ranked one, unless interleaved (then a fixed permutation decides the
// kind of each position, as on a live store); the newest denseRanked ranked
// blocks carry the gate term many times, every never-ranked block once.
type unrankedShape struct {
	ranked, unranked int
	unrankedWidth    int
	denseRanked      int
	omitUnranked     bool
	interleaved      bool
}

// isRanked reports whether position i (0 = newest) holds a ranked block.
func (s unrankedShape) isRanked(i int) bool {
	if !s.interleaved {
		return i < s.ranked
	}
	total := uint64(s.ranked + s.unranked)
	// 2654435761 is prime, so for every smaller total this is a permutation
	// of the positions: exactly s.unranked of them are never-ranked.
	return (uint64(i)*2654435761)%total >= uint64(s.unranked)
}

// unrankedTerm is the full-text term of the gate.
const unrankedTerm = "quarzmarker"

// unrankedSeed is a mass seed through COPY: the production triggers run (the
// type, embed_excluded and counter triggers among them), the per-row write
// path of the API does not — at transcript width it would take minutes.
func unrankedSeed(shape unrankedShape) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		total := shape.ranked + shape.unranked
		newest := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		const workers = 4
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for wk := 0; wk < workers; wk++ {
			wg.Add(1)
			go func(wk int) {
				defer wg.Done()
				conn, err := pool.Acquire(ctx)
				if err != nil {
					errs <- err
					return
				}
				defer conn.Release()
				next := wk
				_, err = conn.Conn().CopyFrom(ctx, pgx.Identifier{"context_blocks"},
					[]string{"category", "title", "content", "scope", "type_name", "created_at", "updated_at", "metadata", "tags"},
					pgx.CopyFromFunc(func() ([]any, error) {
						for next < total {
							i := next // 0 = newest
							next += workers
							ts := newest.Add(-time.Duration(i) * time.Second)
							if shape.isRanked(i) {
								rng := rand.New(rand.NewPCG(7, uint64(i)))
								times := 0
								if i < shape.denseRanked {
									times = 40
								}
								return []any{"notes", fmt.Sprintf("note %d %s", i, unrankedWords[i%unrankedVocab]),
									unrankedText(uint64(i)|1<<40, 500+rng.IntN(5500), unrankedTerm, times),
									"private", "knowledge", ts, ts, map[string]any{}, []string{"note"}}, nil
							}
							if shape.omitUnranked {
								continue
							}
							return []any{"compaction-checkpoints", fmt.Sprintf("Compaction source r%06d part %03d of 034", i/34, i%34),
								unrankedText(uint64(i), shape.unrankedWidth, unrankedTerm, 1),
								"private", "checkpoint", ts, ts,
								map[string]any{"part": i % 34, "root_session_id": fmt.Sprintf("r%06d", i/34)},
								[]string{"hermes", "compaction"}}, nil
						}
						return nil, nil
					}))
				if err != nil {
					errs <- fmt.Errorf("worker %d: %w", wk, err)
				}
			}(wk)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			return err
		}
		_, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_blocks`)
		return err
	}
}

// unrankedHeapPages is the heap size of context_blocks in pages.
func unrankedHeapPages(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var pages int64
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_relation_size('context_blocks') / current_setting('block_size')::bigint`).Scan(&pages); err != nil {
		t.Fatalf("heap pages: %v", err)
	}
	return pages
}

func TestSearchRecentNeverRankedRowsCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	base := unrankedShape{ranked: 600, unranked: 800, unrankedWidth: 35000, denseRanked: 20}
	without := base
	without.omitUnranked = true
	with := testdb.SetupSeededTestDB(t, "i342-cost-with", unrankedSeed(base))
	alone := testdb.SetupSeededTestDB(t, "i342-cost-without", unrankedSeed(without))

	var unranked int
	if err := with.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks WHERE type_name = 'checkpoint' AND embed_excluded AND length(content) > 30000`).Scan(&unranked); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if unranked != base.unranked {
		t.Fatalf("fixture: %d never-ranked wide rows, want %d", unranked, base.unranked)
	}
	heapRatio := float64(unrankedHeapPages(t, with)) / float64(unrankedHeapPages(t, alone))
	t.Logf("heap pages with/without the never-ranked rows: ratio %.2f", heapRatio)

	scopes := []string{"private"}
	// A deep keyset position among the ranked rows: the 300th newest.
	var deepAt time.Time
	var deepID string
	if err := with.QueryRow(context.Background(),
		`SELECT updated_at, id::text FROM context_blocks ORDER BY updated_at DESC, id DESC OFFSET 299 LIMIT 1`).Scan(&deepAt, &deepID); err != nil {
		t.Fatalf("deep cursor: %v", err)
	}
	type stmt struct {
		name string
		sql  string
		args []any
	}
	// Both statements production sends (issue #237): the walk over the
	// indexes of migration 195, and the statement before 195, which it sends
	// while they do not stand — so that one is measured without them. With
	// the indexes standing the planner serves the statement before 195 from
	// them as well, the reference without the never-ranked rows falls to
	// about half, and the bound derived from it turns a few buffers of plan
	// noise into a failure (2 of 4 runs on the production image) for a
	// combination production never sends.
	type measured struct {
		stmt
		with, alone map[string]testdb.PlanCost
	}
	var pageStmts []measured
	for _, walk := range []bool{true, false} {
		form := " (walk)"
		if !walk {
			form = " (before 195)"
			for _, db := range []*pgxpool.Pool{with, alone} {
				if _, err := db.Exec(context.Background(),
					`DROP INDEX idx_blocks_scope_updated, idx_blocks_scope_category_updated`); err != nil {
					t.Fatalf("drop the indexes of migration 195: %v", err)
				}
			}
		}
		browse := func(after *store.SearchCursor) stmt {
			sql, args := store.SearchStatementForTest(store.SearchBlocksParams{
				ReadScopes: scopes, Limit: store.SearchDefaultLimit, Compact: true, After: after, ProbeNext: true,
			}, walk, walk)
			return stmt{"browse" + form, sql, args}
		}
		recentSQL, recentArgs := store.RecentStatementForTest(scopes, "", 10, nil, nil, []string{}, walk, walk)
		deep := browse(&store.SearchCursor{UpdatedAt: deepAt, ID: deepID})
		deep.name = "browse deep page" + form
		// Measured here, while the indexes are as production has them for
		// this form.
		for _, s := range []stmt{{"recent" + form, recentSQL, recentArgs}, browse(nil), deep} {
			m := measured{stmt: s, with: map[string]testdb.PlanCost{}, alone: map[string]testdb.PlanCost{}}
			for _, plan := range []string{"custom", "generic"} {
				explain := testdb.ExplainCustom
				if plan == "generic" {
					explain = testdb.ExplainGeneric
				}
				m.with[plan] = explain(t, with, s.sql, s.args...)
				m.alone[plan] = explain(t, alone, s.sql, s.args...)
			}
			pageStmts = append(pageStmts, m)
		}
	}
	for _, s := range pageStmts {
		for _, plan := range []string{"custom", "generic"} {
			got := s.with[plan]
			ref := s.alone[plan]
			bound := int64(float64(ref.Buffers) * heapRatio * 1.25)
			t.Logf("%s (%s plan): %d buffers with the never-ranked rows, %d without, bound %d", s.name, plan, got.Buffers, ref.Buffers, bound)
			if got.Buffers > bound {
				t.Errorf("%s (%s plan): %d buffers with the never-ranked rows, more than %d (%d without them × heap ratio %.2f × 1.25): the statement reads the content of rows that are not on the page",
					s.name, plan, got.Buffers, bound, ref.Buffers, heapRatio)
			}
		}
	}

	// Full text: the term is in every never-ranked row once and 40 times in the
	// 20 newest ranked rows, so the page is ranked rows only.
	for _, plan := range []string{"custom", "generic"} {
		explain := testdb.ExplainCustom
		if plan == "generic" {
			explain = testdb.ExplainGeneric
		}
		compactSQL, compactArgs := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
			Query: unrankedTerm, ReadScopes: scopes, Limit: store.SearchDefaultLimit, Compact: true, ProbeNext: true,
		})
		fullSQL, fullArgs := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
			Query: unrankedTerm, ReadScopes: scopes, Limit: store.SearchDefaultLimit, Compact: false, ProbeNext: true,
		})
		compact := explain(t, with, compactSQL, compactArgs...)
		full := explain(t, with, fullSQL, fullArgs...)
		bound := int64(float64(full.Buffers) * 1.1)
		t.Logf("full text (%s plan): compact %d buffers, full %d, bound %d", plan, compact.Buffers, full.Buffers, bound)
		if compact.Buffers > bound {
			t.Errorf("full text (%s plan): the compact page costs %d buffers, more than %d (the full statement's %d × 1.1): it reads the content of matching rows that are not on the page",
				plan, compact.Buffers, bound, full.Buffers)
		}
	}
}
