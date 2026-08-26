package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/topiclabel"
)

// ─────────────────────────────────────────────────────────────────────────────
// V-W5 — Label-Arm-Telemetrie je Topic (design/05 §7 Zeile V-W5, §2.2 S5).
//
// MEASUREMENT ONLY. Nothing here fixes the label arm: whether the exhausted
// topics get re-armed is decision E05-5, and that decision needs the number
// first (design/05 "Warum V-W5 keine Fix-Welle ist").
//
// THE JOIN RULE, DERIVED FROM THE ARM'S OWN CODE — context_llm_log carries NO
// topic_id, so the attribution has to be reconstructed:
//
//  1. topiclabel.Pipeline = "cluster-label" (topiclabel.go:88) is the ONLY
//     producer of these rows — the constant is referenced here, not copied, so
//     a rename cannot silently empty this report.
//  2. labelOne hands the topic's core array to the chain call:
//     `d.Chat(callCtx, d, required, system, user, c.coreBlocks)`
//     (topiclabel.go:481) — the block list of a call IS one topic's core at
//     tick time, in full (the prompt cap in loadCore trims the TITLES it reads,
//     never the ID list passed here).
//  3. That array comes from graph_cluster_node.core_blocks (selectSQL,
//     topiclabel.go:223) — the RUN core.
//  4. ChainCall puts it on the log row: `BlockIDs: c.BlockIDs`
//     (llm/chain.go:688) → llmlog.Entry.BlockIDs (llmlog/llmlog.go:56) →
//     column block_ids (llmlog.go:182). Exactly ONE row per ChainCall.Do
//     (chain.go:685-693).
//  5. overview's topicCoreSyncSQL copies the same run core onto the TOPIC row
//     (overview/topic.go:944-949) — that is the column that survives the node
//     teardown (migration 124_cluster_topic_identity.sql:31-40, :95-96) and
//     therefore the only durable anchor a report can join against.
//
// WHY OVERLAP AND NOT EQUALITY. graph_cluster_topic.core_blocks holds only the
// YOUNGEST core; the core drifts over a topic's life, and that drift is exactly
// what triggers a re-label (label_stale, topiclabel.go:222-232). Set equality
// would therefore see only the calls of the current core generation and report
// every older call as missing. The rule is an overlap plus a time bound:
//
//	t.core_blocks && c.block_ids AND c.created_at >= t.created_at
//
// The time bound is not cosmetic: a call cannot predate the topic row it was
// selected from (selectSQL reads graph_cluster_topic), and it removes a third
// of the ambiguity live. Set equality is still reported — as the calls_exact
// column, i.e. "how much of this attribution is current-generation".
//
// AMBIGUITY IS REPORTED, NEVER RESOLVED BY GUESSING. A row whose blocks touch
// two living topics is counted at BOTH and additionally counted as ambiguous;
// the per-topic numbers are upper bounds wherever calls_ambiguous > 0.
//
// A never-admitted background call writes the K9 rejection line INSTEAD of the
// regular row, without any block_ids (chain.go:677-683, rejectionEntry
// :352-361) — such rows can never be attributed and are counted separately.

// armClusterLabel is the only arm this report understands today. It is the
// arm's own constant, not a copy.
const armClusterLabel = topiclabel.Pipeline

// exhaustedAttempts mirrors topiclabel's maxAttempts (topiclabel.go:61): a
// living topic with label_stale AND label_attempts >= this value is out of the
// selection until its core hash drifts (overview/label.go:176) — the S5
// dead end. The constant cannot be imported (it is unexported), so a drift
// tripwire in pertopic_test.go reads the literal out of topiclabel.go.
const exhaustedAttempts = 3

// perTopicJoin is the seam of the mandatory negative probe (Gate 3): replacing
// it with a plain JOIN makes every topic without a single label call disappear
// from the report. It stands as its own constant so the probe can substitute it
// in the FINISHED statement and run the defective variant for real.
const perTopicJoin = `LEFT JOIN`

// topicMatchExpr is the join rule itself (derivation in the package header).
// One constant, used by both statements, so the report cannot count its
// assignment totals by a different rule than its per-topic rows.
const topicMatchExpr = `t.core_blocks && c.block_ids AND c.created_at >= t.created_at`

// perTopicCallsCTE is the arm's call set: one row per model call, already
// carrying the occupancy/wire milliseconds. occupancyExpr is reused verbatim —
// the currency of this report is the same GPU second as everywhere else.
const perTopicCallsCTE = `
	calls AS (
	    SELECT l.id, l.created_at, l.block_ids,
	           COALESCE(` + occupancyExpr + `, 0)::double precision AS occ_ms,
	           COALESCE(l.duration_ms, 0)::double precision         AS wire_ms
	      FROM context_llm_log l
	     WHERE l.pipeline = $1
	       AND l.created_at >= $2 AND l.created_at < $3
	       AND l.block_ids IS NOT NULL
	       AND cardinality(l.block_ids) > 0
	)`

// perTopicSQL is the per-topic view: ONE row per LIVING topic, including the
// topics that never saw a single call (that is what perTopicJoin buys).
const perTopicSQL = `
WITH topics AS (
    SELECT t.topic_id::text AS topic_id, t.scope, COALESCE(t.label, '') AS label,
           t.label_source, t.label_attempts, t.label_stale,
           t.created_at, t.last_seen_at, t.label_built_at,
           t.core_blocks, cardinality(t.core_blocks) AS core_n
      FROM graph_cluster_topic t
     WHERE t.retired_at IS NULL
),` + perTopicCallsCTE + `,
	hit AS (
	    SELECT c.id AS log_id, t.topic_id,
	           (t.core_blocks @> c.block_ids AND t.core_blocks <@ c.block_ids) AS exact_core
	      FROM calls c
	      JOIN topics t ON ` + topicMatchExpr + `
	),
	hits AS (
	    SELECT log_id, count(*) AS topics_hit FROM hit GROUP BY log_id
	),
	agg AS (
	    SELECT h.topic_id,
	           count(*)                                 AS calls,
	           count(*) FILTER (WHERE h.exact_core)     AS calls_exact,
	           count(*) FILTER (WHERE n.topics_hit > 1) AS calls_ambiguous,
	           sum(c.occ_ms)  / 1000.0                  AS occupancy_seconds,
	           sum(c.wire_ms) / 1000.0                  AS wire_seconds,
	           max(c.created_at)                        AS last_call
	      FROM hit h
	      JOIN calls c ON c.id = h.log_id
	      JOIN hits  n ON n.log_id = h.log_id
	     GROUP BY h.topic_id
	)
SELECT t.topic_id, t.scope, t.label, t.label_source, t.label_attempts, t.label_stale,
       t.created_at, t.last_seen_at, t.label_built_at, t.core_n,
       COALESCE(a.calls, 0), COALESCE(a.calls_exact, 0), COALESCE(a.calls_ambiguous, 0),
       COALESCE(a.occupancy_seconds, 0), COALESCE(a.wire_seconds, 0), a.last_call
  FROM topics t
  ` + perTopicJoin + ` agg a ON a.topic_id = t.topic_id
 ORDER BY COALESCE(a.calls, 0) DESC, t.topic_id`

// perTopicStatsSQL is the assignment balance sheet: how many rows of the arm
// the rule actually places, how many it places twice, and where the rest goes.
// Without it the per-topic table would be a set of numbers with no denominator.
const perTopicStatsSQL = `
WITH` + perTopicCallsCTE + `,
	hit AS (
	    SELECT c.id AS log_id, count(*) AS topics_hit
	      FROM calls c
	      JOIN graph_cluster_topic t ON t.retired_at IS NULL AND ` + topicMatchExpr + `
	     GROUP BY c.id
	),
	ret AS (
	    SELECT DISTINCT c.id AS log_id
	      FROM calls c
	      JOIN graph_cluster_topic t ON t.retired_at IS NOT NULL AND ` + topicMatchExpr + `
	)
SELECT (SELECT count(*) FROM context_llm_log
         WHERE pipeline = $1 AND created_at >= $2 AND created_at < $3),
       (SELECT count(*) FROM context_llm_log
         WHERE pipeline = $1 AND created_at >= $2 AND created_at < $3
           AND (block_ids IS NULL OR cardinality(block_ids) = 0)),
       (SELECT count(*) FROM context_llm_log WHERE pipeline = $1 AND created_at < $2),
       (SELECT count(*) FROM hit),
       (SELECT count(*) FROM hit WHERE topics_hit > 1),
       COALESCE((SELECT max(topics_hit) FROM hit), 0),
       (SELECT count(*) FROM ret r WHERE NOT EXISTS (SELECT 1 FROM hit h WHERE h.log_id = r.log_id))`

// firstLivingTopicSQL anchors the per-topic window on the birth of the oldest
// LIVING topic — "seit dem ersten Lauf" (design/05 §7 V-W5 (a)). A call older
// than that cannot belong to a living topic under the time bound of the rule,
// so a wider window would only add rows the report has to explain away.
const firstLivingTopicSQL = `SELECT min(created_at) FROM graph_cluster_topic WHERE retired_at IS NULL`

// TopicCalls is one living topic with everything the E05-5 decision needs.
type TopicCalls struct {
	TopicID       string `json:"topic_id"`
	Scope         string `json:"scope"`
	Label         string `json:"label"`
	LabelSource   string `json:"label_source"`
	LabelAttempts int32  `json:"label_attempts"`
	LabelStale    bool   `json:"label_stale"`
	// Exhausted is the S5 dead end: stale AND out of attempts (see
	// exhaustedAttempts).
	Exhausted     bool       `json:"exhausted"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	LabelBuiltAt  *time.Time `json:"label_built_at"`
	LabelAgeHours *float64   `json:"label_age_hours"`
	CoreN         int32      `json:"core_n"`
	LifetimeHours float64    `json:"lifetime_hours"`
	Calls         int64      `json:"calls"`
	// CallsExact is the subset whose block_ids equals the topic's CURRENT
	// core exactly — the current-generation share of the attribution.
	CallsExact int64 `json:"calls_exact"`
	// CallsAmbiguous is the subset that also matches another living topic.
	// Where it is > 0, every number in this row is an upper bound.
	CallsAmbiguous   int64      `json:"calls_ambiguous"`
	CallsPerHour     float64    `json:"calls_per_lifetime_hour"`
	OccupancySeconds float64    `json:"occupancy_seconds"`
	WireSeconds      float64    `json:"wire_seconds"`
	LastCall         *time.Time `json:"last_call"`
}

// AssignStats is the balance sheet of the attribution over the window.
type AssignStats struct {
	ArmRows             int64 `json:"arm_rows"`
	RowsWithoutBlockIDs int64 `json:"rows_without_block_ids"`
	RowsBeforeWindow    int64 `json:"rows_before_window"`
	AssignedRows        int64 `json:"assigned_rows"`
	AmbiguousRows       int64 `json:"ambiguous_rows"`
	MaxTopicsPerRow     int64 `json:"max_topics_per_row"`
	UnassignedRows      int64 `json:"unassigned_rows"`
	// UnassignedRetiredOnly is the part of UnassignedRows that matches a
	// RETIRED topic — the calls that were spent on topics which have since
	// died. They are the reason the two totals differ, not a defect.
	UnassignedRetiredOnly int64 `json:"unassigned_retired_only"`
	// SumTopicCalls is Σ over the per-topic rows. It exceeds AssignedRows by
	// exactly the multiple counting of ambiguous rows.
	SumTopicCalls int64 `json:"sum_topic_calls"`
}

// PerTopicReport is the whole per-topic section.
type PerTopicReport struct {
	Arm      string    `json:"arm"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	JoinRule string    `json:"join_rule"`
	// ExhaustedAttempts is the threshold the exhausted section is cut at.
	ExhaustedAttempts int          `json:"exhausted_attempts"`
	LivingTopics      int          `json:"living_topics"`
	Topics            []TopicCalls `json:"topics"`
	// ExhaustedTopics is a SUBSET of Topics — living, stale, out of attempts.
	ExhaustedTopics []TopicCalls `json:"exhausted_topics"`
	Assignment      AssignStats  `json:"assignment"`
	CallsP50        float64      `json:"calls_per_topic_p50"`
	CallsP95        float64      `json:"calls_per_topic_p95"`
	CallsPerHourP50 float64      `json:"calls_per_lifetime_hour_p50"`
	CallsPerHourP95 float64      `json:"calls_per_lifetime_hour_p95"`
	CallsPerHourMax float64      `json:"calls_per_lifetime_hour_max"`
	Notes           []string     `json:"notes"`
}

// joinRuleText is the rule in one line, printed and stored with the report:
// a number whose attribution rule is not on the page is not a measurement.
const joinRuleText = "block_ids && graph_cluster_topic.core_blocks AND log.created_at >= topic.created_at " +
	"(hergeleitet: topiclabel.go:481 übergibt den Lauf-Kern, :223 liest ihn aus graph_cluster_node, " +
	"overview/topic.go:944-949 spiegelt ihn auf die Topic-Zeile, llm/chain.go:688 schreibt ihn nach block_ids)"

// errUnsupportedArm is the exit-2 condition: -arm names a pipeline this report
// has no attribution rule for. Fail-closed and BEFORE any DB contact — a
// report that silently falls back to "cluster-label" would answer a question
// nobody asked.
var errUnsupportedArm = fmt.Errorf("armcost: nur -arm=%s wird unterstützt (die Zuordnungs-Regel ist arm-spezifisch)", armClusterLabel)

// buildPerTopic erhebt die Per-Topic-Sicht. Eigenes Fenster: von der Geburt
// des ältesten LEBENDEN Topics bis zum gepinnten until des Reports — die
// Kosten-Tabellen behalten ihr rollendes -days-Fenster.
func buildPerTopic(ctx context.Context, pool *pgxpool.Pool, arm string, until time.Time, fallbackSince time.Time) (PerTopicReport, error) {
	pt := PerTopicReport{Arm: arm, Until: until, JoinRule: joinRuleText, ExhaustedAttempts: exhaustedAttempts}

	var first *time.Time
	if err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, firstLivingTopicSQL).Scan(&first)
	}); err != nil {
		return pt, fmt.Errorf("armcost: erstes lebendes Topic: %w", err)
	}
	switch {
	case first != nil:
		pt.Since = *first
	default:
		pt.Since = fallbackSince
	}
	if !pt.Since.Before(pt.Until) {
		return pt, fmt.Errorf("armcost: leeres Topic-Fenster: since=%s until=%s",
			pt.Since.Format(time.RFC3339), pt.Until.Format(time.RFC3339))
	}

	topics, err := queryTopics(ctx, pool, perTopicSQL, arm, pt.Since, pt.Until)
	if err != nil {
		return pt, fmt.Errorf("armcost: per-topic: %w", err)
	}
	pt.Topics = topics

	if err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		a := &pt.Assignment
		return tx.QueryRow(ctx, perTopicStatsSQL, arm, pt.Since, pt.Until).Scan(
			&a.ArmRows, &a.RowsWithoutBlockIDs, &a.RowsBeforeWindow,
			&a.AssignedRows, &a.AmbiguousRows, &a.MaxTopicsPerRow, &a.UnassignedRetiredOnly)
	}); err != nil {
		return pt, fmt.Errorf("armcost: per-topic stats: %w", err)
	}
	pt.Assignment.UnassignedRows = pt.Assignment.ArmRows - pt.Assignment.AssignedRows

	summarizePerTopic(&pt)
	return pt, nil
}

// queryTopics fährt die Per-Topic-Abfrage. Das Statement ist ein Parameter,
// damit die Pflicht-Negativ-Proben der Welle (INNER JOIN statt perTopicJoin,
// Regel ohne Zeitschranke) die DEFEKTE Variante real gegen dieselbe Fixture
// fahren können, statt sie nachzubauen — Muster queryBuckets (M-W7).
func queryTopics(ctx context.Context, pool *pgxpool.Pool, q, arm string, since, until time.Time) ([]TopicCalls, error) {
	var out []TopicCalls
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, arm, since, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tc TopicCalls
			if err := rows.Scan(&tc.TopicID, &tc.Scope, &tc.Label, &tc.LabelSource,
				&tc.LabelAttempts, &tc.LabelStale, &tc.CreatedAt, &tc.LastSeenAt,
				&tc.LabelBuiltAt, &tc.CoreN, &tc.Calls, &tc.CallsExact, &tc.CallsAmbiguous,
				&tc.OccupancySeconds, &tc.WireSeconds, &tc.LastCall); err != nil {
				return err
			}
			out = append(out, deriveTopic(tc, until))
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deriveTopic füllt die abgeleiteten Felder einer Topic-Zeile. Getrennt vom
// Scan, damit die Ableitung ohne Datenbank prüfbar ist.
func deriveTopic(tc TopicCalls, until time.Time) TopicCalls {
	tc.Exhausted = tc.LabelStale && tc.LabelAttempts >= exhaustedAttempts
	tc.LifetimeHours = until.Sub(tc.CreatedAt).Hours()
	if tc.LifetimeHours > 0 {
		tc.CallsPerHour = float64(tc.Calls) / tc.LifetimeHours
	}
	if tc.LabelBuiltAt != nil {
		age := until.Sub(*tc.LabelBuiltAt).Hours()
		tc.LabelAgeHours = &age
	}
	return tc
}

// summarizePerTopic zieht die Verteilung und die Pflicht-Notizen.
//
// QUANTILE STATT HISTOGRAMM (Wahl der Welle, begründet): die Grundgesamtheit
// sind die lebenden Topics — live 54. Für Histogramm-Buckets ist das zu wenig
// (jeder Bucket trüge einstellige Zahlen und die Bucket-Grenzen wären die
// eigentliche Aussage); p50/p95 sind dieselbe Statistik, die dieser Report
// schon je Pipeline führt, und damit direkt vergleichbar. Die Nullen zählen
// mit: ein Topic ohne Call ist ein Messpunkt, kein fehlender Wert.
func summarizePerTopic(pt *PerTopicReport) {
	calls := make([]float64, 0, len(pt.Topics))
	perHour := make([]float64, 0, len(pt.Topics))
	for _, t := range pt.Topics {
		calls = append(calls, float64(t.Calls))
		perHour = append(perHour, t.CallsPerHour)
		pt.Assignment.SumTopicCalls += t.Calls
		if t.Exhausted {
			pt.ExhaustedTopics = append(pt.ExhaustedTopics, t)
		}
	}
	pt.LivingTopics = len(pt.Topics)
	// Die erschöpften Topics stehen nach Label-Alter, ältestes zuerst: das ist
	// die Reihenfolge, in der E05-5 sie ansehen muss. Zeilen ohne
	// label_built_at (nie gelabelt) stehen hinten.
	sort.SliceStable(pt.ExhaustedTopics, func(i, j int) bool {
		a, b := pt.ExhaustedTopics[i].LabelAgeHours, pt.ExhaustedTopics[j].LabelAgeHours
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return *a > *b
	})
	sort.Float64s(calls)
	sort.Float64s(perHour)
	pt.CallsP50, pt.CallsP95 = quantileLinear(calls, 0.5), quantileLinear(calls, 0.95)
	pt.CallsPerHourP50 = quantileLinear(perHour, 0.5)
	pt.CallsPerHourP95 = quantileLinear(perHour, 0.95)
	if len(perHour) > 0 {
		pt.CallsPerHourMax = perHour[len(perHour)-1]
	}
	pt.Notes = perTopicNotes(*pt)
}

// perTopicNotes sind die Pflicht-Notizen der Sektion — jede benennt eine
// Eigenschaft, ohne die eine Zahl dieser Sektion falsch gelesen wird.
func perTopicNotes(pt PerTopicReport) []string {
	notes := []string{
		"Zuordnungs-Regel: " + pt.JoinRule,
		fmt.Sprintf("Mehrdeutigkeit: %d von %d zugeordneten Zeilen treffen mehr als ein lebendes Topic "+
			"(max %d Topics je Zeile). Sie zählen bei JEDEM Treffer — Σ Topic-Calls %d > zugeordnete Zeilen %d. "+
			"Wo calls_mehrdeutig > 0 ist, sind calls/belegung_s/wire_s dieses Topics Obergrenzen.",
			pt.Assignment.AmbiguousRows, pt.Assignment.AssignedRows, pt.Assignment.MaxTopicsPerRow,
			pt.Assignment.SumTopicCalls, pt.Assignment.AssignedRows),
		fmt.Sprintf("Nicht zugeordnet: %d von %d Zeilen im Fenster (davon %d mit Treffer NUR auf pensionierten "+
			"Topics, %d ohne block_ids — K9-Ablehnungen ohne Wire-Call, llm/chain.go:677-683). "+
			"Vor dem Fenster liegen weitere %d Zeilen des Arms.",
			pt.Assignment.UnassignedRows, pt.Assignment.ArmRows, pt.Assignment.UnassignedRetiredOnly,
			pt.Assignment.RowsWithoutBlockIDs, pt.Assignment.RowsBeforeWindow),
		fmt.Sprintf("Erschöpft = lebend AND label_stale AND label_attempts >= %d (topiclabel.go:61 maxAttempts). "+
			"Der Zähler fällt nur bei core_hash-Drift zurück (overview/label.go:176) — bis dahin ist das Topic "+
			"aus der Selektion (topiclabel.go:222-232).", exhaustedAttempts),
		"Messung, kein Fix: was mit den erschöpften Topics geschieht, ist E05-5 " +
			"(design/05, Abschnitt „Warum V-W5 keine Fix-Welle ist“).",
	}
	return notes
}

// quantileLinear ist percentile_cont auf einer bereits sortierten Liste —
// dieselbe Definition, die der Report in SQL für p50/p95 der Gruppen fährt,
// damit die beiden Perzentil-Sichten desselben Reports vergleichbar sind.
func quantileLinear(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rn := p * float64(len(sorted)-1)
	lo, hi := int(math.Floor(rn)), int(math.Ceil(rn))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rn-float64(lo))
}
