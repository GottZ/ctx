package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recurrenceTitleSimThreshold is the pg_trgm similarity floor for Phase 1
// candidate selection. Pre-Empirie 2026-05-06 (audit-w38b-results.json):
// at sim>0.5 the 27 candidate-pairs scored 74% Phase-2 RECURRENT/SUPERSEDES
// precision (18 RECURRENT + 2 SUPERSEDES + 7 NEITHER). Lower thresholds
// (sim>0.3 → 188 pairs) push more LLM-calls per cycle without proportional
// signal gain. The threshold is consistent with supersedesTitleSimThreshold (0.25)
// being lower because supersedes has additional category+age gates.
const recurrenceTitleSimThreshold = 0.5

// MaxRecurrenceCandidates caps how many Phase-1 pairs proceed to Phase-2
// LLM-confirm per dream cycle. Bounds per-cycle latency at known levels.
const MaxRecurrenceCandidates = 5

// recurrenceSystemPrompt classifies a single candidate-pair as
// recurrent / supersedes / none. The pattern enum follows the audit-w38b
// taxonomy (parallel/sequence/version-replacement dominate; weekly/monthly/
// sessional emerge once Generative-Synthese (Welle 38c) writes daily/weekly
// reports).
const recurrenceSystemPrompt = `Classify whether two knowledge blocks form a recurring pattern.

Block A and Block B share a temporal-dimension value AND a similar title shape.
Decide which relationship type applies — or skip it.

Types:
- recurrent: B is another instance of the same pattern as A. Both are valid in parallel.
  Patterns:
    parallel — different concrete instances of one class (e.g. mautrix-signal vs mautrix-discord bridges)
    sequence — sequential phases or periodisations of one body of work (e.g. Phase 1 vs Phase 4 of a project)
    weekly / monthly / sessional — periodic reports/handovers of the same activity
- supersedes: B explicitly replaces A. Use only when B is the newer authoritative version of the same fact AND A is wrong/obsolete (NOT just older). RARE.
- none: A and B share temporal+title shape but treat different topics, OR they are merely topically related without a pattern.

Output JSON: {"verdict":"recurrent|supersedes|none","pattern":"parallel|sequence|weekly|monthly|sessional|version-replacement|none","confidence":0.0-1.0}

Confidence reflects how clearly the chosen verdict applies. When in doubt: "none".`

// recurrenceVerdict is the parsed Phase-2 LLM response.
type recurrenceVerdict struct {
	Verdict    string  `json:"verdict"`
	Pattern    string  `json:"pattern"`
	Confidence float64 `json:"confidence"`
}

// recurrenceCandidate is one Phase-1 pair: source paired with a target
// that shares dimension+value and title-similarity. TargetSens is the
// floor-adjusted sensitivity for the per-pair Phase-2 chain (the prompt
// carries both block contents).
type recurrenceCandidate struct {
	TargetID    string
	TargetTitle string
	TargetText  string
	TitleSim    float64
	TargetSens  backends.Sensitivity
}

// DetectRecurrence is the Welle 38b entry point. Phase 1 (deterministic SQL)
// finds candidates that share a context_temporal dimension+value with the
// source AND have title-similarity above recurrenceTitleSimThreshold. Phase 2
// asks the LLM to confirm each candidate as recurrent/supersedes/none.
//
// Returns []Link with relationship='recurrent' (and rarely 'supersedes')
// suitable for WriteLinks. Empty slice on no candidates or no confirmations.
//
// pool may be nil for the Phase-2 llmlog.Record only — Phase 1 always runs
// against the pool and a nil pool returns no candidates.
func DetectRecurrence(ctx context.Context, pool *pgxpool.Pool, r *Router, opts llm.Options, source BlockInfo) ([]Link, error) {
	if pool == nil {
		return nil, nil
	}

	candidates, err := pickRecurrenceCandidates(ctx, pool, r, source.ID, source.Title, source.Scope)
	if err != nil {
		return nil, fmt.Errorf("dream: recurrence phase 1: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	links := make([]Link, 0, len(candidates))
	for _, c := range candidates {
		verdict, vErr := confirmRecurrence(ctx, pool, r, opts, source, c)
		if vErr != nil {
			slog.Warn("dream: recurrence phase 2 failed (non-fatal)",
				"source", source.ID, "target", c.TargetID, "error", vErr)
			continue
		}
		switch verdict.Verdict {
		case "recurrent", "supersedes":
		default:
			continue
		}
		if verdict.Confidence < minRawConfidence[verdict.Verdict] {
			slog.Debug("dream: recurrence below threshold",
				"source", source.ID, "target", c.TargetID,
				"verdict", verdict.Verdict, "confidence", verdict.Confidence,
				"floor", minRawConfidence[verdict.Verdict])
			continue
		}
		links = append(links, Link{
			TargetID:     c.TargetID,
			Relationship: verdict.Verdict,
			Confidence:   verdict.Confidence,
		})
	}
	return links, nil
}

// pickRecurrenceCandidates is Phase 1 — same dimension+value AND title-sim > threshold.
// Same scope as the source. is_archived blocks excluded. Bounded by MaxRecurrenceCandidates.
// Carries each target's floor-adjusted sensitivity for the Phase-2 chain.
func pickRecurrenceCandidates(ctx context.Context, pool *pgxpool.Pool, r *Router, sourceID, sourceTitle, sourceScope string) ([]recurrenceCandidate, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT b.id::text, b.title, LEFT(b.content, $4),
		        similarity(b.title, $2) AS title_sim, b.sensitivity, b.scope
		 FROM context_temporal a
		 JOIN context_temporal b_t
		   ON a.dimension = b_t.dimension AND a.value = b_t.value AND a.block_id <> b_t.block_id
		 JOIN context_blocks b ON b.id = b_t.block_id
		 WHERE a.block_id = $1::uuid
		   AND NOT b.is_archived
		   AND b.scope = $3
		   AND similarity(b.title, $2) > $5
		 ORDER BY title_sim DESC
		 LIMIT $6`,
		sourceID, sourceTitle, sourceScope, maxContentLen,
		recurrenceTitleSimThreshold, MaxRecurrenceCandidates,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []recurrenceCandidate
	for rows.Next() {
		var c recurrenceCandidate
		var sens, scope string
		if err := rows.Scan(&c.TargetID, &c.TargetTitle, &c.TargetText, &c.TitleSim, &sens, &scope); err != nil {
			return nil, err
		}
		c.TargetSens = r.FloorSens(backends.Sensitivity(sens), scope)
		out = append(out, c)
	}
	return out, rows.Err()
}

// confirmRecurrence is Phase 2 — single LLM-call per pair. Logged via llmlog
// for audit-trail consistency with EvaluateRelationships. The prompt carries
// both block contents, so the chain resolves at max(source, target).
func confirmRecurrence(ctx context.Context, pool *pgxpool.Pool, r *Router, opts llm.Options, source BlockInfo, c recurrenceCandidate) (recurrenceVerdict, error) {
	userPrompt := buildRecurrencePrompt(source, c)
	required := backends.MaxSensitivity(source.Sensitivity, c.TargetSens)
	dreamVer := int16(Version)

	entry := &llmlog.Entry{
		Pipeline:      "dream-recurrence",
		RequestSystem: recurrenceSystemPrompt,
		RequestUser:   userPrompt,
		BlockIDs:      []string{source.ID, c.TargetID},
		DreamVersion:  &dreamVer,
	}
	defer func() { llmlog.Record(pool, entry.Slimmed()) }()

	start := time.Now()
	resp, served, attempts, err := r.chat(ctx, backends.RoleDream, required,
		recurrenceSystemPrompt, userPrompt, opts, DreamTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err
	applyChainTelemetry(entry, backends.RoleDream, required, served, attempts)
	if resp != nil {
		entry.ResponseContent = resp.Message.Content
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
	}
	if err != nil {
		return recurrenceVerdict{}, err
	}

	verdict, parseErr := parseRecurrenceResponse(resp.Message.Content)
	if parseErr != nil {
		entry.Err = fmt.Errorf("parse: %w", parseErr)
		return recurrenceVerdict{}, parseErr
	}
	return verdict, nil
}

// buildRecurrencePrompt formats the per-pair Phase-2 input.
func buildRecurrencePrompt(source BlockInfo, c recurrenceCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<block_a id=\"%s\" title=\"%s\" updated=\"%s\">\n",
		source.ID, llm.EscapeXml(source.Title), source.UpdatedAt.Format("2006-01-02"))
	b.WriteString(llm.EscapeXml(truncate(source.Content, maxContentLen)))
	b.WriteString("\n</block_a>\n\n")
	fmt.Fprintf(&b, "<block_b id=\"%s\" title=\"%s\" title_sim=\"%.2f\">\n",
		c.TargetID, llm.EscapeXml(c.TargetTitle), c.TitleSim)
	b.WriteString(llm.EscapeXml(truncate(c.TargetText, maxContentLen)))
	b.WriteString("\n</block_b>")
	return b.String()
}

// parseRecurrenceResponse tolerates code-fence wrapping and whitespace.
// Single object output — no array form (Phase 2 evaluates one pair at a time).
func parseRecurrenceResponse(raw string) (recurrenceVerdict, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var v recurrenceVerdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return recurrenceVerdict{}, fmt.Errorf("recurrence parse: %w", err)
	}
	return v, nil
}
