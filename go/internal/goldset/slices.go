package goldset

// Construction of the three multi-gold slices of wave M-W5 (design/05 §4.5).
//
// The split between this file and the command layer is deliberate: everything
// that DECIDES which cases exist — the window rule, the confidence floor, the
// pair deduplication — is a pure function over drawn rows and is unit-tested
// without a database. The database only supplies rows.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ---------------------------------------------------------------- G-SESS.

// Window kinds. A day window is the primitive; a span window exists because
// the corpus has fewer daily reports than the slice needs cases, and a period
// question ("that week") is the same retrieval task at a coarser grain.
const (
	WindowDay  = "day"
	WindowSpan = "span"
)

// SessionReport is one daily report (`audit-trail`, lifecycle `synthesis`).
// Day is taken from the DATE IN THE TITLE, not from created_at: a report
// written after midnight is still the report of the day it names, and the
// window has to be the one the question will ask about.
type SessionReport struct {
	Day     time.Time
	ID      string
	Title   string
	Content string
}

// SessionWindow is the unit a G-SESS case is built on: a half-open interval
// [From, To) plus the daily reports that fall inside it.
type SessionWindow struct {
	Kind         string
	Label        string
	From, To     time.Time
	ReportIDs    []string
	ReportTitles []string
	// Digest is the first report's body, the only text a model gets to see.
	Digest string
}

// PoolRef renders the window as a resolvable construction reference.
func (w SessionWindow) PoolRef() string { return "window:" + w.Label }

// BuildSessionWindows turns the drawn reports into windows: one per calendar
// day that carries a report, then — for each requested span length — disjoint
// runs of that many CONSECUTIVE REPORTED DAYS.
//
// Two rules that decide what the slice measures:
//
//   - the window is half-open, [day 00:00Z, day+1 00:00Z). A closed window
//     would put a block created exactly at midnight into two windows.
//   - a trailing partial run is dropped. A "span" of one day is the day window
//     again, and two cases with identical gold would double-count.
func BuildSessionWindows(reports []SessionReport, spanLens []int) []SessionWindow {
	byDay := map[string][]SessionReport{}
	var days []time.Time
	for _, r := range reports {
		key := r.Day.UTC().Format("2006-01-02")
		if _, seen := byDay[key]; !seen {
			days = append(days, r.Day.UTC().Truncate(24*time.Hour))
		}
		byDay[key] = append(byDay[key], r)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	for k := range byDay {
		rs := byDay[k]
		sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
		byDay[k] = rs
	}

	out := make([]SessionWindow, 0, len(days))
	for _, d := range days {
		key := d.Format("2006-01-02")
		out = append(out, windowOf(WindowDay, key, d, d.AddDate(0, 0, 1), byDay, []time.Time{d}))
	}
	for _, span := range spanLens {
		if span < 2 {
			continue
		}
		for i := 0; i+span <= len(days); i += span {
			run := days[i : i+span]
			from, last := run[0], run[len(run)-1]
			label := from.Format("2006-01-02") + ".." + last.Format("2006-01-02")
			out = append(out, windowOf(WindowSpan, label, from, last.AddDate(0, 0, 1), byDay, run))
		}
	}
	return out
}

func windowOf(kind, label string, from, to time.Time, byDay map[string][]SessionReport, days []time.Time) SessionWindow {
	w := SessionWindow{Kind: kind, Label: label, From: from, To: to}
	for _, d := range days {
		for _, r := range byDay[d.Format("2006-01-02")] {
			w.ReportIDs = append(w.ReportIDs, r.ID)
			w.ReportTitles = append(w.ReportTitles, r.Title)
			if w.Digest == "" {
				w.Digest = r.Content
			}
		}
	}
	return w
}

// SessionReports draws the daily reports. The lifecycle filter is what makes
// them reports rather than any audit-trail row, and the title date is required:
// a report whose title carries no date cannot define a window.
func (d *DB) SessionReports(ctx context.Context) ([]SessionReport, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT substring(title from '\d{4}-\d{2}-\d{2}')::date AS day,
		       id::text, title, content
		FROM context_blocks
		WHERE type_name = 'audit-trail'
		  AND lifecycle_state = 'synthesis'
		  AND NOT is_archived
		  AND title ~ '\d{4}-\d{2}-\d{2}'
		ORDER BY day, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionReport
	for rows.Next() {
		var r SessionReport
		if err := rows.Scan(&r.Day, &r.ID, &r.Title, &r.Content); err != nil {
			return nil, err
		}
		r.Day = r.Day.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// WindowGold is the constructive gold of one session window: the window's own
// daily reports plus every retrievable `knowledge` block created inside it.
//
// Nothing here reads an insight, a cluster or a summary — that is exactly what
// keeps G-SESS non-circular against the layer it is built to measure.
func (d *DB) WindowGold(ctx context.Context, w SessionWindow) ([]string, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT b.id::text
		FROM context_blocks b
		JOIN context_block_types t ON t.name = b.type_name
		WHERE NOT b.is_archived
		  AND coalesce(t.config->'retrieval'->>'policy', 'full-pass') <> 'excluded'
		  AND b.type_name = 'knowledge'
		  AND b.created_at >= $1 AND b.created_at < $2
		ORDER BY b.id`, w.From, w.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := make([]string, 0, len(w.ReportIDs))
	for _, id := range w.ReportIDs {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ------------------------------------------------------------------ G-MH.

// MinDreamConfidence is the non-circularity floor for G-MH gold, and it is a
// CONSTANT rather than a flag on purpose: the April dream-link audit measures
// 56 % correctness over 350 links but 100 % at confidence >= 0.7
// (memory/learning_dream_quality.md). Below the floor roughly half the gold
// would be wrong, and a gold set that is half wrong measures the generator of
// the links, not the retrieval.
const MinDreamConfidence = 0.7

// DreamLink is one bridge between two blocks, as drawn for G-MH.
type DreamLink struct {
	Source, Target Block
	Relationship   string
	Confidence     float64
}

// PoolRef renders the link as a resolvable construction reference.
func (l DreamLink) PoolRef() string { return "link:" + l.Source.ID + "|" + l.Target.ID }

// GoldIDs are both endpoints — the whole point of the slice is that one of them
// is not enough.
func (l DreamLink) GoldIDs() []string {
	ids := []string{l.Source.ID, l.Target.ID}
	sort.Strings(ids)
	return ids
}

// FilterDreamLinks applies the floor and collapses each undirected pair to one
// case. It runs over rows the query already filtered, and that redundancy is
// the point: the floor is the gate of this slice, so it is asserted in code
// that a unit test can hold, not only in a WHERE clause.
func FilterDreamLinks(in []DreamLink) []DreamLink {
	seen := map[string]bool{}
	out := make([]DreamLink, 0, len(in))
	for _, l := range in {
		if l.Confidence < MinDreamConfidence {
			continue
		}
		if l.Source.ID == "" || l.Target.ID == "" || l.Source.ID == l.Target.ID {
			continue
		}
		a, b := l.Source.ID, l.Target.ID
		if a > b {
			a, b = b, a
		}
		key := a + "|" + b
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	return out
}

// DreamLinkPairs draws candidate bridges. Both endpoints must be retrievable —
// a gold id the retrieval can never return would make the case unsolvable by
// construction rather than hard.
func (d *DB) DreamLinkPairs(ctx context.Context, seed int64, limit, minContent int) ([]DreamLink, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT s.id::text, s.title, s.content, s.type_name, coalesce(s.language, ''),
		       t.id::text, t.title, t.content, t.type_name, coalesce(t.language, ''),
		       dl.relationship, dl.confidence
		FROM context_dream_links dl
		JOIN context_blocks s ON s.id = dl.source_block_id
		JOIN context_block_types ts ON ts.name = s.type_name
		JOIN context_blocks t ON t.id = dl.target_block_id
		JOIN context_block_types tt ON tt.name = t.type_name
		WHERE dl.confidence >= $1
		  AND s.id <> t.id
		  AND NOT s.is_archived AND NOT t.is_archived
		  AND coalesce(ts.config->'retrieval'->>'policy', 'full-pass') <> 'excluded'
		  AND coalesce(tt.config->'retrieval'->>'policy', 'full-pass') <> 'excluded'
		  AND length(s.content) >= $2 AND length(t.content) >= $2
		ORDER BY md5(least(s.id::text, t.id::text) || greatest(s.id::text, t.id::text) || $3::text)
		LIMIT $4`, MinDreamConfidence, minContent, fmt.Sprint(seed), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DreamLink
	for rows.Next() {
		var l DreamLink
		if err := rows.Scan(&l.Source.ID, &l.Source.Title, &l.Source.Content, &l.Source.TypeName, &l.Source.Language,
			&l.Target.ID, &l.Target.Title, &l.Target.Content, &l.Target.TypeName, &l.Target.Language,
			&l.Relationship, &l.Confidence); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ------------------------------------------------- G-GLOB / G-GLOB-KONSTR.

// Pool is an aggregating construction source: a corpus tag (G-GLOB) or a
// cluster (G-GLOB-KONSTR). GoldIDs is empty for the tag pools — their gold is
// judged, not constructed.
type Pool struct {
	Ref     string
	Label   string
	Titles  []string
	GoldIDs []string
	Size    int
}

// TagPools draws aggregating sources from the corpus TAGS.
//
// The choice of tags over clusters is load-bearing: a G-GLOB question written
// from a cluster label would be shaped by the very graph layer the slice is
// meant to test, and the resulting uplift would be an artefact of the question,
// not of the retrieval.
func (d *DB) TagPools(ctx context.Context, seed int64, limit, minBlocks, sampleTitles int) ([]Pool, error) {
	rows, err := d.conn.Query(ctx, `
		WITH tagged AS (
		  SELECT unnest(b.tags) AS tag, b.id, b.title
		  FROM context_blocks b
		  JOIN context_block_types t ON t.name = b.type_name
		  WHERE NOT b.is_archived
		    AND coalesce(t.config->'retrieval'->>'policy', 'full-pass') <> 'excluded'
		), agg AS (
		  SELECT tag, count(DISTINCT id) AS n, (array_agg(DISTINCT title))[1:$1] AS titles
		  FROM tagged
		  WHERE length(tag) > 2
		  GROUP BY tag
		  HAVING count(DISTINCT id) >= $2
		)
		SELECT tag, n, titles FROM agg
		ORDER BY md5(tag || $3::text)
		LIMIT $4`, sampleTitles, minBlocks, fmt.Sprint(seed), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pool
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.Label, &p.Size, &p.Titles); err != nil {
			return nil, err
		}
		p.Ref = "tag:" + p.Label
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClusterPools draws the FLOOR CHECK sources: clusters with their members as
// gold. Circular against the graph layer by construction — declared in the
// slice profile and kept out of ReportSlices for exactly that reason.
func (d *DB) ClusterPools(ctx context.Context, minMembers, sampleTitles int) ([]Pool, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT n.cluster_id::text,
		       coalesce(nullif(tp.label, ''), n.repr_title) AS label,
		       array_agg(b.id::text ORDER BY b.id) AS ids,
		       (array_agg(b.title ORDER BY b.id))[1:$1] AS titles,
		       count(*)::int AS n
		FROM graph_cluster_node n
		JOIN graph_cluster_member gm ON gm.cluster_id = n.cluster_id
		JOIN context_blocks b ON b.id = gm.block_id
		JOIN context_block_types t ON t.name = b.type_name
		LEFT JOIN graph_cluster_topic tp ON tp.topic_id = n.topic_id
		WHERE NOT b.is_archived
		  AND coalesce(t.config->'retrieval'->>'policy', 'full-pass') <> 'excluded'
		GROUP BY n.cluster_id, tp.label, n.repr_title
		HAVING count(*) >= $2
		ORDER BY n.cluster_id`, sampleTitles, minMembers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pool
	for rows.Next() {
		var p Pool
		var id string
		if err := rows.Scan(&id, &p.Label, &p.GoldIDs, &p.Titles, &p.Size); err != nil {
			return nil, err
		}
		p.Ref = "cluster:" + id
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActiveCount is every non-archived block — the second half of the K9
// population statement.
func (d *DB) ActiveCount(ctx context.Context) (int, error) {
	var n int
	err := d.conn.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE NOT is_archived`).Scan(&n)
	return n, err
}

// GoldStats are the per-slice gold figures the stamp carries: the total number
// of labels and their median per case. The total is what the drift census is
// sized on (design/05 F-25).
func GoldStats(cases []Case) (total, median int) {
	sizes := make([]int, 0, len(cases))
	for _, c := range cases {
		total += len(c.GoldIDs)
		sizes = append(sizes, len(c.GoldIDs))
	}
	if len(sizes) == 0 {
		return 0, 0
	}
	sort.Ints(sizes)
	return total, sizes[len(sizes)/2]
}
