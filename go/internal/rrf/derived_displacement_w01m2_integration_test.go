//go:build integration

// W01-M2 (Achse 01 §7, design/01-ebenen-modell.md:1126) — do derived blocks
// displace their own sources?
//
// H0: inserting k derived blocks (stratum 1) into a fixture corpus does NOT
// lower Recall@5 of the ORIGINAL blocks over the gold set.
//
// The measurement is LLM-free and never touches the live instance. It runs
// against the B-W1 fixture (arms_parity_integration_test.go, 220 blocks, four
// types, three scopes) extended by `catalog` dummies built from material the
// fixture already carries: the title union of a topic's member blocks as the
// proxy text (design §7 W01-M2, "Topic-Label + Titel der core_blocks"), and
// either the centroid or a member's own vector as the embedding.
//
// THREE numbers, all mandatory (§7 W01-M2):
//
//  1. Recall@5 of the originals over the gold set.
//  2. The rank shift of the top-1 ORIGINAL — the load-bearing figure, because a
//     derivative can push its own source out of the top-k without Recall@k
//     moving at all (§9.1/M9).
//  3. The share of each ARM's candidate set the derivatives occupy (§4.10a).
//     The arm caps (75/100/100/30, 139:188/:214/:253/:287/:305) sit INSIDE the
//     arm CTEs, the type factor only in the `rrf` CTE afterwards — so no damping
//     value can give back a candidate slot a derivative took.
//
// VISIBILITY. `catalog` is retrieval policy 'excluded' (143_derived_block_types
// .sql:216-231). The measurement makes the dummies visible by widening
// p_types_visible on the CALL — the M-W2 shadow-type semantics one layer below
// the handler (armsweep/client.go:62) — and never by flipping the registry, so
// the fixture's type policy stays the production policy.
//
// GOLD SET. The fixture carries no relevance judgements, so the gold set is the
// BASELINE itself: gold(q) = the first five ORIGINALS ctx_rrf delivers for q at
// k = 0. That makes Recall@5 exactly 1.0 in the baseline by construction and
// turns every drop into a causal effect of the insertion — the only thing that
// differs between the conditions is k. It measures displacement against the
// status quo ranking, not topical relevance; see the wave report for that limit.
//
//	go test -tags=integration ./internal/rrf/ -run TestW01M2 -count=1 -v
package rrf_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/evalscore"
	"github.com/GottZ/ctx/internal/testdb"
)

// ---------------------------------------------------------------------------
// Fixture extension: the catalog dummies.

const (
	w01m2Type     = "catalog"
	w01m2Topics   = 10 // the fixture's lexical classes, see w01m2TopicMembers
	w01m2Cut      = 5  // Recall@5 / top-5 window, = armsweep.RecallCut
	w01m2Centroid = "centroid"
	w01m2Core     = "coreblock"
)

// w01m2VisibleWithCatalog is bw1VisibleTypes plus `catalog`. Widening the
// allowlist is a per-call argument, exactly as mw1AllFourTypes does it for
// `checkpoint` — the retrieval policy of `catalog` is untouched.
var w01m2VisibleWithCatalog = []string{"knowledge", "reference", "audit-trail", w01m2Type}

// w01m2ArmNames / w01m2ArmCaps mirror the five candidate caps of 139 in the
// order ctx_rrf_arms projects them (semantic_ann and semantic_exact share one
// output column, so their common cap of 75 appears once).
var (
	w01m2ArmNames = [4]string{"semantic", "fts_de", "fts_en", "trigram"}
	w01m2ArmCaps  = [4]int{75, 100, 100, 30}
)

// w01m2Attrappe is one catalog dummy: a derived block over a set of fixture
// blocks that really exist in the corpus it is inserted into.
//
// The id prefix 019fa403 sorts AFTER every fixture id (019fa402), and ctx_rrf
// breaks score ties by `cb.id` since Gen 17 (139:366). A dummy therefore never
// wins a position it only tied for — the displacement counts below are
// conservative by construction and need no epsilon.
type w01m2Attrappe struct {
	ID      string   `json:"id"`
	Topic   int      `json:"topic"`
	Chunk   int      `json:"chunk"`
	Title   string   `json:"title"`
	Scope   string   `json:"scope"`
	Members []string `json:"members"`
	Core    string   `json:"core"`              // member whose vector the coreblock variant copies
	Content string   `json:"content,omitempty"` // override; empty = title union of Members
}

// w01m2BlockID rebuilds a fixture block id from its index — the same literal
// bw1SeedCorpus writes (arms_parity_integration_test.go:129).
func w01m2BlockID(i int) string {
	return fmt.Sprintf("019fa402-0000-7000-9000-0000000%05d", 50000+i)
}

// w01m2TopicScope: every fixture block of one lexical class shares one scope,
// because both are a function of i mod 10 (:131-137). A catalog therefore has
// single-scope sources, which is what §5.2/B7 demands. Class 9 lives in the
// foreign scope and its catalog is consequently invisible — a fact the report
// names rather than a fixture defect.
func w01m2TopicScope(topic int) string {
	switch {
	case topic == 9:
		return bw1ScopeForeign
	case topic >= 7:
		return bw1ScopeB
	default:
		return bw1ScopeA
	}
}

// w01m2TopicMembers returns the live (non-archived) fixture blocks of one
// lexical class. The class is i mod 10: every lexeme of a fixture block —
// de, de2, de3, en, en2, en3 — is a function of i mod 10 alone (:141-147), so
// these ten classes are the fixture's own topic structure, not an invention.
func w01m2TopicMembers(topic int) []string {
	var out []string
	for i := topic; i < bw1Blocks; i += w01m2Topics {
		if i%23 == 0 { // is_archived, :139
			continue
		}
		out = append(out, w01m2BlockID(i))
	}
	return out
}

// w01m2TopicLabel is the proxy for graph_cluster_topic.label: the three words
// every member of the class shares.
func w01m2TopicLabel(topic int) string {
	return fmt.Sprintf("%s %s %s",
		bw1WordsDE[topic%len(bw1WordsDE)],
		bw1WordsDE[(topic*3+1)%len(bw1WordsDE)],
		bw1WordsEN[(topic*7+2)%len(bw1WordsEN)])
}

// w01m2Plan lays out k dummies over the ten topics, topic-major. For k > 10 a
// topic gets several catalogs over DISJOINT member chunks, so a denser derived
// layer stays a partition of real source material instead of the same block
// written twice.
func w01m2Plan(k int) []w01m2Attrappe {
	if k <= 0 {
		return nil
	}
	chunks := (k + w01m2Topics - 1) / w01m2Topics
	var out []w01m2Attrappe
	for c := 0; c < chunks && len(out) < k; c++ {
		for topic := 0; topic < w01m2Topics && len(out) < k; topic++ {
			members := w01m2Chunk(w01m2TopicMembers(topic), c, chunks)
			if len(members) == 0 {
				continue
			}
			n := len(out)
			out = append(out, w01m2Attrappe{
				ID:      fmt.Sprintf("019fa403-0000-7000-9000-0000000%05d", 60000+n),
				Topic:   topic,
				Chunk:   c,
				Title:   fmt.Sprintf("Katalog #%032x %s", topic*100+c, w01m2TopicLabel(topic)),
				Scope:   w01m2TopicScope(topic),
				Members: members,
				Core:    members[0],
			})
		}
	}
	return out
}

// w01m2Chunk cuts a member list into `chunks` contiguous parts and returns the
// idx-th one.
func w01m2Chunk(ids []string, idx, chunks int) []string {
	if chunks <= 1 {
		return ids
	}
	size := (len(ids) + chunks - 1) / chunks
	lo := idx * size
	if lo >= len(ids) {
		return nil
	}
	hi := lo + size
	if hi > len(ids) {
		hi = len(ids)
	}
	return ids[lo:hi]
}

// w01m2InsertSQL builds the dummy entirely from rows already in the table: the
// content is the title union of its members (string_agg), the embedding is
// either their centroid — §4.10a(2), "a catalog block is by construction the
// semantic centre of its cluster" — or one member's own vector, which is the
// control that isolates the LEXICAL mechanism of §4.10a(1) from the geometric
// one.
const w01m2InsertSQL = `
INSERT INTO context_blocks
    (id, category, title, content, scope, embedding, created_at, updated_at, type_name, is_archived)
SELECT $1::uuid, $2::text, $3::text,
       COALESCE($10::text,
                (SELECT string_agg(m.title, '. ' ORDER BY m.id)
                   FROM context_blocks m WHERE m.id = ANY($4::uuid[]))),
       $5::text,
       CASE WHEN $6::text = 'centroid'
            THEN (SELECT avg(c.embedding)::vector(1024)
                    FROM context_blocks c
                   WHERE c.id = ANY($4::uuid[]) AND c.embedding IS NOT NULL)
            ELSE (SELECT k.embedding FROM context_blocks k WHERE k.id = $7::uuid)
       END,
       $8::timestamptz, $8::timestamptz, $9::text, false`

func w01m2Insert(ctx context.Context, tx pgx.Tx, atts []w01m2Attrappe, variant string) error {
	stamp := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, a := range atts {
		var content interface{}
		if a.Content != "" {
			content = a.Content
		}
		if _, err := tx.Exec(ctx, w01m2InsertSQL,
			a.ID, "katalog", a.Title, a.Members, a.Scope, variant, a.Core, stamp, w01m2Type, content,
		); err != nil {
			return fmt.Errorf("insert attrappe %s: %w", a.ID, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// One measurement run.

// w01m2Hit is one delivered row of ctx_rrf: id and score, in delivery order.
// ctx_rrf orders by `score DESC, cb.id` since Gen 17 (139:366), so the sequence
// is fully determined by the data.
type w01m2Hit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// w01m2ArmRow is the ctx_rrf_arms projection this wave reads: the four ranks
// plus the type name migration 142 added.
type w01m2ArmRow struct {
	ID       string `json:"id"`
	Semantic *int   `json:"rank_semantic"`
	FtsDE    *int   `json:"rank_fts_de"`
	FtsEN    *int   `json:"rank_fts_en"`
	Trigram  *int   `json:"rank_trigram"`
	TypeName string `json:"type_name"`
}

func (r w01m2ArmRow) rank(arm int) *int {
	return [4]*int{r.Semantic, r.FtsDE, r.FtsEN, r.Trigram}[arm]
}

// w01m2QueryResult is one query under one condition.
type w01m2QueryResult struct {
	Query    string        `json:"query"`
	Text     string        `json:"text"`
	Filtered bool          `json:"category_or_tag_filtered"`
	Ranking  []w01m2Hit    `json:"ranking"`
	Arms     []w01m2ArmRow `json:"arms"`
}

// w01m2Condition is one experimental cell.
type w01m2Condition struct {
	Name    string   `json:"name"`
	K       int      `json:"k"`
	Variant string   `json:"variant"`
	Visible []string `json:"types_visible"`
	// ForceOne is the negative probe of the k = 0 gate: a run that inserts a
	// dummy even though k = 0. Its hash MUST differ from the baseline.
	ForceOne bool `json:"force_one"`
}

type w01m2Run struct {
	Condition w01m2Condition     `json:"condition"`
	Attrappen []w01m2Attrappe    `json:"attrappen"`
	Queries   []w01m2QueryResult `json:"queries"`
}

// w01m2CallRRF reads the full delivered ranking (id, score).
func w01m2CallRRF(ctx context.Context, tx pgx.Tx, args []any) ([]w01m2Hit, error) {
	rows, err := tx.Query(ctx, `SELECT id, rrf_score FROM ctx_rrf(`+bw1ArgList+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []w01m2Hit
	for rows.Next() {
		var h w01m2Hit
		if err := rows.Scan(&h.ID, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// w01m2CallArms reads the per-arm ranks plus type_name (migration 142).
func w01m2CallArms(ctx context.Context, tx pgx.Tx, args []any) ([]w01m2ArmRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, rank_semantic, rank_fts_de, rank_fts_en, rank_trigram, type_name
		   FROM ctx_rrf_arms(`+bw1ArgList+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []w01m2ArmRow
	for rows.Next() {
		var r w01m2ArmRow
		if err := rows.Scan(&r.ID, &r.Semantic, &r.FtsDE, &r.FtsEN, &r.Trigram, &r.TypeName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, rows.Err()
}

// w01m2Execute runs the 54 B-W1 queries under one condition. Every query gets
// its OWN transaction that inserts the dummies, measures and rolls back: the
// corpus is therefore identical at the start of each query, and the SET LOCAL
// hnsw.iterative_scan the ann arm leaves behind (see the tx-seam note in
// arms_parity_integration_test.go) reaches every query in the same state.
func w01m2Execute(t *testing.T, ctx context.Context, conn *pgxpool.Conn, cond w01m2Condition, granted []string) w01m2Run {
	t.Helper()
	atts := w01m2Plan(cond.K)
	if cond.ForceOne && len(atts) == 0 {
		atts = w01m2Plan(1)
	}
	run := w01m2Run{Condition: cond, Attrappen: atts}
	for qi, q := range bw1Queries() {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("%s/%s: begin: %v", cond.Name, q.name, err)
		}
		if err := w01m2Insert(ctx, tx, atts, cond.Variant); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s/%s: %v", cond.Name, q.name, err)
		}
		args := bw1Args(q, bw1Embedding(qi), granted, bw1BigLimit)
		args[9] = cond.Visible
		ranking, err := w01m2CallRRF(ctx, tx, args)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s/%s: ctx_rrf: %v", cond.Name, q.name, err)
		}
		arms, err := w01m2CallArms(ctx, tx, args)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s/%s: ctx_rrf_arms: %v", cond.Name, q.name, err)
		}
		_ = tx.Rollback(ctx)
		run.Queries = append(run.Queries, w01m2QueryResult{
			Query:    q.name,
			Text:     q.text,
			Filtered: q.category != nil || len(q.tags) > 0,
			Ranking:  ranking,
			Arms:     arms,
		})
	}
	return run
}

// w01m2Hash is the canonical fingerprint of a run: the delivered ranking of
// every query with full float precision, plus every arm rank. This is what the
// k = 0 gate compares byte for byte.
func w01m2Hash(run w01m2Run) string {
	h := sha256.New()
	for _, q := range run.Queries {
		fmt.Fprintf(h, "Q\t%s\t%s\n", q.Query, q.Text)
		for i, hit := range q.Ranking {
			fmt.Fprintf(h, "R\t%d\t%s\t%.17g\n", i, hit.ID, hit.Score)
		}
		for _, a := range q.Arms {
			fmt.Fprintf(h, "A\t%s\t%s\t%s\t%s\t%s\t%s\n",
				a.ID, w01m2RankStr(a.Semantic), w01m2RankStr(a.FtsDE),
				w01m2RankStr(a.FtsEN), w01m2RankStr(a.Trigram), a.TypeName)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func w01m2RankStr(p *int) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

// ---------------------------------------------------------------------------
// The three measurement kernels — and their gutted twins.

// w01m2Kernel bundles the three numbers §7 W01-M2 demands. It is a struct of
// functions so each one can be GUTTED individually: TestW01M2ConstructionProbe
// reads all three, and TestW01M2GuttedKernels proves that replacing any of them
// with its hollow twin changes the verdict. Without that, a wave could report
// "no displacement" from a metric that never looked.
type w01m2Kernel struct {
	Name string
	// Recall5 is Recall@5 of the originals over the gold set.
	Recall5 func(ranked []string, gold map[string]bool) float64
	// RankOf is the 1-based position of id in the delivered ranking, 0 if the
	// id is not in the candidate set at all.
	RankOf func(ranking []w01m2Hit, id string) int
	// ArmShare is the per-arm candidate census: how much of each arm's capped
	// candidate set the derivatives occupy (§4.10a).
	ArmShare func(arms []w01m2ArmRow, derived map[string]bool) [4]w01m2ArmStat
}

type w01m2ArmStat struct {
	Arm     string `json:"arm"`
	Ranked  int    `json:"ranked"`
	Derived int    `json:"derived"`
	Cap     int    `json:"cap"`
	AtCap   bool   `json:"at_cap"`
}

func (s w01m2ArmStat) share() float64 {
	if s.Ranked == 0 {
		return 0
	}
	return float64(s.Derived) / float64(s.Ranked)
}

var w01m2Live = w01m2Kernel{
	Name: "live",
	Recall5: func(ranked []string, gold map[string]bool) float64 {
		return evalscore.RecallAtK(ranked, gold, w01m2Cut)
	},
	RankOf: func(ranking []w01m2Hit, id string) int {
		for i, h := range ranking {
			if h.ID == id {
				return i + 1
			}
		}
		return 0
	},
	ArmShare: func(arms []w01m2ArmRow, derived map[string]bool) [4]w01m2ArmStat {
		var out [4]w01m2ArmStat
		for a := 0; a < 4; a++ {
			out[a] = w01m2ArmStat{Arm: w01m2ArmNames[a], Cap: w01m2ArmCaps[a]}
			for _, r := range arms {
				if r.rank(a) == nil {
					continue
				}
				out[a].Ranked++
				if derived[r.ID] {
					out[a].Derived++
				}
			}
			out[a].AtCap = out[a].Ranked >= out[a].Cap
		}
		return out
	},
}

// w01m2Gutted is the hollow twin of each kernel: recall that always claims a
// perfect window, a rank that always claims position 1, an arm census that
// never counts a derivative. Each is exactly the defect the corresponding
// assertion in TestW01M2ConstructionProbe has to catch.
var w01m2Gutted = w01m2Kernel{
	Name:     "gutted",
	Recall5:  func([]string, map[string]bool) float64 { return 1.0 },
	RankOf:   func([]w01m2Hit, string) int { return 1 },
	ArmShare: func([]w01m2ArmRow, map[string]bool) [4]w01m2ArmStat { return [4]w01m2ArmStat{} },
}

// ---------------------------------------------------------------------------
// Scoring.

type w01m2Metrics struct {
	Condition string `json:"condition"`
	K         int    `json:"k"`
	Variant   string `json:"variant"`

	Queries    int `json:"queries"`
	Scored     int `json:"scored_queries"`
	Unfiltered int `json:"unfiltered_queries"`

	// (1) Recall@5 of the originals.
	Recall5Mean float64 `json:"recall5_mean"`
	Recall5Drop int     `json:"queries_with_recall_drop"`
	SRecall5    float64 `json:"srecall5_mean"`

	// (2) rank shift of the top-1 original.
	Top1ShiftMean   float64 `json:"top1_rank_shift_mean"`
	Top1ShiftMax    int     `json:"top1_rank_shift_max"`
	Top1Shifted     int     `json:"top1_rank_shift_queries"`
	Top1Evicted     int     `json:"top1_out_of_candidate_set"`
	Rank1ByDerived  int     `json:"rank1_held_by_derived"`
	Rank1Rate       float64 `json:"rank1_displacement_rate"`
	Rank1RateUnfilt float64 `json:"rank1_displacement_rate_unfiltered"`
	DerivedInTop5   int     `json:"queries_with_derived_in_top5"`

	// (3) candidate share per arm.
	Arms []w01m2ArmStat `json:"arms"`
}

// w01m2Score folds one condition against the baseline. gold(q) is the first
// five originals of the BASELINE ranking, so recall is 1.0 there by
// construction and every drop is caused by the insertion.
func w01m2Score(k w01m2Kernel, base, cond w01m2Run, derived map[string]bool) w01m2Metrics {
	m := w01m2Metrics{Condition: cond.Condition.Name, K: cond.Condition.K, Variant: cond.Condition.Variant}
	var armTotals [4]w01m2ArmStat
	for a := 0; a < 4; a++ {
		armTotals[a] = w01m2ArmStat{Arm: w01m2ArmNames[a], Cap: w01m2ArmCaps[a]}
	}
	shiftSum, recallSum, srecallSum := 0, 0.0, 0.0

	for qi, cq := range cond.Queries {
		bq := base.Queries[qi]
		m.Queries++
		if !cq.Filtered {
			m.Unfiltered++
		}

		stats := k.ArmShare(cq.Arms, derived)
		for a := 0; a < 4; a++ {
			armTotals[a].Ranked += stats[a].Ranked
			armTotals[a].Derived += stats[a].Derived
			if stats[a].AtCap {
				armTotals[a].AtCap = true
			}
		}

		gold, goldOrder := w01m2Gold(bq.Ranking, derived)
		if len(gold) == 0 {
			continue
		}
		m.Scored++

		ranked := make([]string, 0, len(cq.Ranking))
		for _, h := range cq.Ranking {
			ranked = append(ranked, h.ID)
		}
		r5 := k.Recall5(ranked, gold)
		recallSum += r5
		if r5 < 1.0 {
			m.Recall5Drop++
		}
		srecallSum += evalscore.SRecallAtK(ranked, w01m2Aspects(goldOrder, cond.Attrappen), w01m2Cut)

		top1 := goldOrder[0]
		pos := k.RankOf(cq.Ranking, top1)
		switch {
		case pos == 0:
			m.Top1Evicted++
		case pos > 1:
			m.Top1Shifted++
			shiftSum += pos - 1
			if pos-1 > m.Top1ShiftMax {
				m.Top1ShiftMax = pos - 1
			}
		}

		for i, h := range cq.Ranking {
			if i >= w01m2Cut {
				break
			}
			if derived[h.ID] {
				m.DerivedInTop5++
				break
			}
		}
		if len(cq.Ranking) > 0 && derived[cq.Ranking[0].ID] {
			m.Rank1ByDerived++
		}
	}

	if m.Scored > 0 {
		m.Recall5Mean = recallSum / float64(m.Scored)
		m.SRecall5 = srecallSum / float64(m.Scored)
		m.Top1ShiftMean = float64(shiftSum) / float64(m.Scored)
		m.Rank1Rate = float64(m.Rank1ByDerived) / float64(m.Scored)
	}
	if m.Unfiltered > 0 {
		m.Rank1RateUnfilt = float64(m.Rank1ByDerived) / float64(m.Unfiltered)
	}
	m.Arms = armTotals[:]
	return m
}

// w01m2Gold takes the first five ORIGINALS of the baseline ranking. Derived ids
// cannot appear there (k = 0), but the filter is explicit so a mis-wired
// baseline shows up as an empty gold set rather than as a silent pass.
func w01m2Gold(ranking []w01m2Hit, derived map[string]bool) (map[string]bool, []string) {
	gold := map[string]bool{}
	var order []string
	for _, h := range ranking {
		if derived[h.ID] {
			continue
		}
		gold[h.ID] = true
		order = append(order, h.ID)
		if len(order) == w01m2Cut {
			break
		}
	}
	return gold, order
}

// w01m2Aspects maps every gold block to the ids that cover it: itself, plus
// every catalog that lists it among its sources. Subtopic recall over these
// facets is the §9.1/M9 counter-metric — it stays high exactly when a
// derivative replaces its own source, which is the case Recall@5 cannot see.
func w01m2Aspects(gold []string, atts []w01m2Attrappe) map[string][]string {
	out := make(map[string][]string, len(gold))
	for _, g := range gold {
		ids := []string{g}
		for _, a := range atts {
			for _, m := range a.Members {
				if m == g {
					ids = append(ids, a.ID)
					break
				}
			}
		}
		out[g] = ids
	}
	return out
}

func w01m2DerivedSet(atts []w01m2Attrappe) map[string]bool {
	s := make(map[string]bool, len(atts))
	for _, a := range atts {
		s[a.ID] = true
	}
	return s
}

func w01m2LogMetrics(t *testing.T, m w01m2Metrics) {
	t.Helper()
	t.Logf("[%s] k=%d variant=%s scored=%d/%d (unfiltered %d): Recall@5=%.4f (drops %d) · S-Recall@5=%.4f · Top1-Shift mean=%.3f max=%d shifted=%d evicted=%d · Rang1-Derivat=%d (%.1f%% aller, %.1f%% ungefiltert) · Derivat in Top5=%d",
		m.Condition, m.K, m.Variant, m.Scored, m.Queries, m.Unfiltered,
		m.Recall5Mean, m.Recall5Drop, m.SRecall5,
		m.Top1ShiftMean, m.Top1ShiftMax, m.Top1Shifted, m.Top1Evicted,
		m.Rank1ByDerived, 100*m.Rank1Rate, 100*m.Rank1RateUnfilt, m.DerivedInTop5)
	for _, a := range m.Arms {
		t.Logf("[%s]   Arm %-9s Kandidaten=%5d davon Derivat=%4d (%.2f %%), Cap=%d erreicht=%v",
			m.Condition, a.Arm, a.Ranked, a.Derived, 100*a.share(), a.Cap, a.AtCap)
	}
}

// w01m2Dump writes the raw result of the wave next to the report, if
// W01M2_OUT_DIR names a directory. No absolute path lives in the tree.
func w01m2Dump(t *testing.T, name string, v any) {
	t.Helper()
	dir := os.Getenv("W01M2_OUT_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("W01M2_OUT_DIR %s: %v", dir, err)
	}
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, blob, 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("Rohdaten: %s (%d Bytes)", path, len(blob))
}

// ---------------------------------------------------------------------------
// Gate 1: the k = 0 gate (byte identity) and its negative probe.

// TestW01M2ZeroKBaseline is the NEGATIVE gate of §7 W01-M2: the same run with
// k = 0 reproduces the baseline byte for byte (sha256 over every delivered
// score and every arm rank).
//
// Three things are pinned, in this order:
//
//	determinism — the baseline run repeated is the same hash. Without it the
//	  byte-identity claim would be a statement about the planner, not about k.
//	  It is not free: the two full-text arms number their candidates with a bare
//	  ROW_NUMBER and no id tiebreak (139:224, :258), so equal ts_rank_cd values
//	  are ordered by whatever the executor produced.
//	widening — k = 0 with `catalog` in p_types_visible hashes identically to
//	  k = 0 without it, so the allowlist widening alone moves nothing and the
//	  only causal factor left is the presence of the blocks.
//	probe — a run that inserts a dummy DESPITE k = 0 must hash differently.
func TestW01M2ZeroKBaseline(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	base := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "baseline", K: 0, Visible: bw1VisibleTypes}, fx.granted)
	repeat := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "baseline-wdh", K: 0, Visible: bw1VisibleTypes}, fx.granted)
	baseHash, repeatHash := w01m2Hash(base), w01m2Hash(repeat)
	t.Logf("Determinismus: baseline=%s wiederholt=%s", baseHash, repeatHash)
	if baseHash != repeatHash {
		t.Fatalf("die Baseline ist nicht reproduzierbar (%s vs %s) — das k=0-Gate wäre eine Aussage über den Planer", baseHash, repeatHash)
	}

	zero := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "k0-catalog-sichtbar", K: 0, Visible: w01m2VisibleWithCatalog}, fx.granted)
	zeroHash := w01m2Hash(zero)
	t.Logf("k=0-Gate: baseline=%s k0-mit-catalog=%s", baseHash, zeroHash)
	if zeroHash != baseHash {
		t.Errorf("k=0 mit erweiterter p_types_visible reproduziert die Baseline NICHT byte-identisch (%s vs %s)", zeroHash, baseHash)
	}

	forced := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "k0-probe-erzwungen", K: 0, Visible: w01m2VisibleWithCatalog, ForceOne: true}, fx.granted)
	forcedHash := w01m2Hash(forced)
	t.Logf("k=0 NEGATIV-PROBE (eine Attrappe trotz k=0): %s", forcedHash)
	if forcedHash == baseHash {
		t.Error("die Negativ-Probe blieb grün: eine eingefügte Attrappe änderte den Hash nicht — das k=0-Gate misst nichts")
	}

	// Control: the dummies are present but NOT in the allowlist. `excluded`
	// keeps a block out of the RESULT, not out of the indexes (armsweep/
	// shadow.go:11-18) — an invisible catalog still spends HNSW scan budget and
	// FTS bitmap. This cell says whether that is observable at fixture size.
	invisible := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "k10-unsichtbar", K: 10, Variant: w01m2Centroid, Visible: bw1VisibleTypes}, fx.granted)
	invisibleHash := w01m2Hash(invisible)
	t.Logf("Kontrolle unsichtbar (k=10, catalog NICHT in p_types_visible): %s — identisch zur Baseline: %v",
		invisibleHash, invisibleHash == baseHash)

	w01m2Dump(t, "k0-gate.json", map[string]string{
		"baseline":            baseHash,
		"baseline_wiederholt": repeatHash,
		"k0_catalog_sichtbar": zeroHash,
		"k0_probe_erzwungen":  forcedHash,
		"k10_unsichtbar":      invisibleHash,
	})
}

// ---------------------------------------------------------------------------
// Gate 2: the construction probe — can the instrument see displacement at all?

// TestW01M2ConstructionProbe is the X-W0b/M-W2 precedent applied here: an
// ineffective fixture is the most common gate defect, so before any H0 verdict
// the instrument has to be shown displacing a source with a dummy BUILT to
// displace it — collinear vector to the gold block (identical cosine to every
// query) and a lexically dominant text.
//
// It is also the red anchor of all three kernels: the three assertions below
// read Recall5, RankOf and ArmShare respectively, and each one fails when its
// kernel is hollowed out (TestW01M2GuttedKernels pins that permanently).
func TestW01M2ConstructionProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	base := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "probe-baseline", K: 0, Visible: bw1VisibleTypes}, fx.granted)

	// The gold block: the top-1 original of the fixed query, which is query q00
	// of the generated set.
	if len(base.Queries[0].Ranking) < w01m2Cut {
		t.Fatalf("q00 liefert nur %d Kandidaten — die Konstruktions-Probe wäre gehaltlos", len(base.Queries[0].Ranking))
	}
	goldBlock := base.Queries[0].Ranking[0].ID
	q := bw1Queries()[0]

	// One dummy, collinear to the gold block (its own vector) and lexically
	// dominant: the query terms repeated, which ts_rank_cd rewards because it
	// runs WITHOUT a normalisation flag (§4.10a(1), 139:224-237, :258-271).
	probe := w01m2Attrappe{
		ID:      "019fa403-0000-7000-9000-000000099999",
		Topic:   -1,
		Title:   fmt.Sprintf("%s Katalog #%032x", q.text, 999),
		Scope:   bw1ScopeA,
		Members: []string{goldBlock},
		Core:    goldBlock,
	}
	for i := 0; i < 12; i++ {
		probe.Content += q.text + ". "
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := w01m2Insert(ctx, tx, []w01m2Attrappe{probe}, w01m2Core); err != nil {
		t.Fatalf("insert probe: %v", err)
	}
	args := bw1Args(q, bw1Embedding(0), fx.granted, bw1BigLimit)
	args[9] = w01m2VisibleWithCatalog
	ranking, err := w01m2CallRRF(ctx, tx, args)
	if err != nil {
		t.Fatalf("ctx_rrf: %v", err)
	}
	arms, err := w01m2CallArms(ctx, tx, args)
	if err != nil {
		t.Fatalf("ctx_rrf_arms: %v", err)
	}

	derived := map[string]bool{probe.ID: true}
	gold, order := w01m2Gold(base.Queries[0].Ranking, derived)
	ranked := make([]string, 0, len(ranking))
	for _, h := range ranking {
		ranked = append(ranked, h.ID)
	}

	probeRank := w01m2Live.RankOf(ranking, probe.ID)
	goldRank := w01m2Live.RankOf(ranking, order[0])
	recall := w01m2Live.Recall5(ranked, gold)
	stats := w01m2Live.ArmShare(arms, derived)
	t.Logf("Konstruktions-Probe: Attrappe auf Rang %d, Gold-Block %s von Rang 1 auf %d, Recall@5 = %.4f",
		probeRank, order[0], goldRank, recall)
	for _, s := range stats {
		t.Logf("Konstruktions-Probe   Arm %-9s Kandidaten=%3d davon Derivat=%d", s.Arm, s.Ranked, s.Derived)
	}

	// (2) rank shift — reads RankOf.
	if probeRank != 1 {
		t.Errorf("die kollineare Attrappe belegt Rang %d statt 1 — das Instrument kann Verdrängung nicht sehen", probeRank)
	}
	if goldRank == 1 {
		t.Error("der Gold-Block hält weiterhin Rang 1 — es wurde nichts verdrängt")
	}
	// (1) Recall@5 — reads Recall5.
	if recall >= 1.0 {
		t.Errorf("Recall@5 = %.4f trotz Verdrängung — die Recall-Messung reagiert nicht", recall)
	}
	// (3) arm share — reads ArmShare.
	derivedArms := 0
	for _, s := range stats {
		derivedArms += s.Derived
	}
	if derivedArms == 0 {
		t.Error("die Attrappe belegt in KEINEM Arm einen Kandidatenplatz — die Arm-Anteils-Messung reagiert nicht")
	}
}

// TestW01M2GuttedKernels makes the three red anchors permanent instead of a
// transcript in the wave report: on the SAME measured data the live kernel and
// its hollow twin must disagree. If they ever agree, the corresponding number in
// the report says nothing.
func TestW01M2GuttedKernels(t *testing.T) {
	ranking := []w01m2Hit{{ID: "d1", Score: 0.9}, {ID: "o1", Score: 0.8}, {ID: "o2", Score: 0.7}}
	gold := map[string]bool{"o1": true, "o2": true, "o3": true, "o4": true, "o5": true}
	ranked := []string{"d1", "o1", "o2"}
	arms := []w01m2ArmRow{
		{ID: "d1", Semantic: ptrInt(1), FtsDE: ptrInt(1), TypeName: w01m2Type},
		{ID: "o1", Semantic: ptrInt(2), FtsDE: ptrInt(2), TypeName: "knowledge"},
	}
	derived := map[string]bool{"d1": true}

	if live, gut := w01m2Live.Recall5(ranked, gold), w01m2Gutted.Recall5(ranked, gold); live == gut {
		t.Errorf("Recall-Kern entkernt: live %.4f == gutted %.4f", live, gut)
	} else {
		t.Logf("Recall-Kern: live %.4f vs entkernt %.4f", live, gut)
	}
	if live, gut := w01m2Live.RankOf(ranking, "o1"), w01m2Gutted.RankOf(ranking, "o1"); live == gut {
		t.Errorf("Rang-Kern entkernt: live %d == gutted %d", live, gut)
	} else {
		t.Logf("Rang-Kern: live %d vs entkernt %d", live, gut)
	}
	liveArms := w01m2Live.ArmShare(arms, derived)
	gutArms := w01m2Gutted.ArmShare(arms, derived)
	if liveArms == gutArms {
		t.Errorf("Arm-Anteils-Kern entkernt: live %+v == gutted %+v", liveArms, gutArms)
	} else {
		t.Logf("Arm-Anteils-Kern: live semantic %d/%d vs entkernt %d/%d",
			liveArms[0].Derived, liveArms[0].Ranked, gutArms[0].Derived, gutArms[0].Ranked)
	}
}

// ---------------------------------------------------------------------------
// The measurement itself.

// TestW01M2Displacement is the wave: k ∈ {0, 3, 10, 30} in two dummy variants
// against the same baseline, all three numbers per cell. It asserts the
// INSTRUMENT (the fixture really produces derived candidates), never the
// outcome — the H0 verdict and the 20 % threshold are report facts for board
// checkpoint #2, not a gate.
func TestW01M2Displacement(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	base := w01m2Execute(t, ctx, conn, w01m2Condition{Name: "baseline", K: 0, Visible: bw1VisibleTypes}, fx.granted)

	var conds []w01m2Condition
	conds = append(conds, w01m2Condition{Name: "k0", K: 0, Visible: w01m2VisibleWithCatalog})
	for _, variant := range []string{w01m2Centroid, w01m2Core} {
		for _, k := range []int{3, 10, 30} {
			conds = append(conds, w01m2Condition{
				Name:    fmt.Sprintf("k%d-%s", k, variant),
				K:       k,
				Variant: variant,
				Visible: w01m2VisibleWithCatalog,
			})
		}
	}

	var table []w01m2Metrics
	var runs []w01m2Run
	for _, c := range conds {
		run := w01m2Execute(t, ctx, conn, c, fx.granted)
		derived := w01m2DerivedSet(run.Attrappen)
		m := w01m2Score(w01m2Live, base, run, derived)
		w01m2LogMetrics(t, m)
		table = append(table, m)
		runs = append(runs, w01m2Trim(run))

		// Instrument check, per cell: with k > 0 the dummies must actually reach
		// the candidate set, otherwise the cell measures nothing. Topic 9 lives
		// in the foreign scope, so k = 3 already carries visible dummies.
		if c.K > 0 {
			total := 0
			for _, a := range m.Arms {
				total += a.Derived
			}
			if total == 0 {
				t.Errorf("[%s] die Attrappen belegen in keinem Arm einen Kandidatenplatz — die Zelle ist gehaltlos", c.Name)
			}
		}
	}

	// Baseline sanity: recall is 1.0 at k = 0 by construction of the gold set.
	if table[0].Scored == 0 {
		t.Fatal("keine Query hat eine Gold-Menge — die ganze Messung wäre gehaltlos")
	}
	if table[0].Recall5Mean != 1.0 || table[0].Rank1ByDerived != 0 {
		t.Errorf("k=0-Zelle: Recall@5 = %.4f (erwartet 1.0), Rang-1-Derivate = %d (erwartet 0)",
			table[0].Recall5Mean, table[0].Rank1ByDerived)
	}

	t.Log("H0-Tafel (Report-Fakt, kein Entscheid) — Schwelle §7: > 20 % Rang-1-Verdrängung schließt full-pass aus")
	for _, m := range table {
		t.Logf("  k=%-2d %-9s Recall@5=%.4f  Top1-Shift=%.3f (max %d)  Rang-1-Verdrängung=%.1f %%  Arm-Anteil sem/de/en/tri=%.1f/%.1f/%.1f/%.1f %%",
			m.K, m.Variant, m.Recall5Mean, m.Top1ShiftMean, m.Top1ShiftMax, 100*m.Rank1Rate,
			100*m.Arms[0].share(), 100*m.Arms[1].share(), 100*m.Arms[2].share(), 100*m.Arms[3].share())
	}

	w01m2Dump(t, "metrics.json", table)
	w01m2Dump(t, "runs.json", runs)
	w01m2Dump(t, "baseline.json", w01m2Trim(base))
}

// w01m2Trim shortens a run for the raw dump: the delivered ranking is cut to
// the first 50 positions and the arm rows keep only ranked candidates. Every
// number the report quotes is computed on the FULL run before this cut.
func w01m2Trim(run w01m2Run) w01m2Run {
	out := run
	out.Queries = make([]w01m2QueryResult, 0, len(run.Queries))
	for _, q := range run.Queries {
		c := q
		if len(c.Ranking) > 50 {
			c.Ranking = c.Ranking[:50]
		}
		arms := make([]w01m2ArmRow, 0, len(q.Arms))
		for _, a := range q.Arms {
			if a.Semantic == nil && a.FtsDE == nil && a.FtsEN == nil && a.Trigram == nil {
				continue
			}
			arms = append(arms, a)
		}
		c.Arms = arms
		out.Queries = append(out.Queries, c)
	}
	return out
}
