package dream

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DailySynthesisTimeout is the LLM-call budget for the daily report
	// generator. Aliased to DreamTimeout because the generator runs through
	// the same Ollama dream model and inherits the same cold-start +
	// queue-wait constraints (Welle 38c / Welle 42).
	DailySynthesisTimeout = DreamTimeout

	// dailySynthesisSystemPrompt instructs the LLM to compose a compact German
	// activity report for the last 24h based on the aggregate statistics
	// supplied in the user prompt.
	dailySynthesisSystemPrompt = `Erzeuge einen kompakten Tagesbericht (200-400 Worte) für ein Knowledge-Store-System. Schreibe als Fließtext in Deutsch. Zähle Schwerpunkte der letzten 24h auf, nenne neue Themen, betone Patterns oder Anomalien.`
)

// dailySynthesisOptions are the LLM sampling options for the daily report.
// Sampling params match DreamOptions (qwen3.6:27b non-thinking tuning), but
// deliberately WITHOUT NumPredict: the prompt requests 200-400 German words
// (~600-900 tokens) and the model terminates via EOS — the shared 400-token
// dream-eval cap truncated every report mid-sentence for 66 days (all
// dream-daily-synthesis llm_log rows at exactly completion_tokens=400).
// Runaway generation stays bounded by DailySynthesisTimeout + the backend's
// context window; a cost cap for paid backends belongs in the chain-level
// num_predict override (llm/chain.go), not hardcoded into the pipeline.
func dailySynthesisOptions() llm.Options {
	return llm.Options{
		Temperature: 0.7,
		TopP:        0.8,
		TopK:        20,
	}
}

// dailyDecisionStat aggregates the count for one decision label of the last
// 24h pulled from context_write_log.
type dailyDecisionStat struct {
	Decision string
	Count    int
}

// dailyDreamLinkStat aggregates the count for one relationship class of the
// last 24h pulled from context_dream_links.
type dailyDreamLinkStat struct {
	Relationship string
	Count        int
}

// dailyNewBlock identifies a fresh block (created in the last 24h) by its
// category and title — sufficient context for the LLM without dragging full
// content into the synthesis prompt.
type dailyNewBlock struct {
	Title    string
	Category string
}

// GenerateDailyReport queries the last-24h activity (decisions, dream-links,
// fresh blocks), asks the LLM for a free-text German summary, and persists
// the result as a synthesis/audit-trail block. Returns the new block_id, or
// an empty string + nil error when there was zero activity to report.
// The chain resolves for role digest at CONSTANT internal (E6): the prompt
// carries aggregate counts + titles/categories — structure, not content. A
// secret in a TITLE is a corpus hygiene problem, not a routing problem. Both
// digest callers (03:00 scheduler iteration and the manual
// POST /api/synthesize/daily trigger) gate identically through here.
func GenerateDailyReport(ctx context.Context, pool *pgxpool.Pool, r *Router, scope string) (string, error) {
	decisions, err := fetchDailyDecisions(ctx, pool, scope)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	newBlocks, err := fetchDailyNewBlocks(ctx, pool, scope)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	dreamLinks, err := fetchDailyDreamLinks(ctx, pool)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	if len(decisions) == 0 && len(newBlocks) == 0 && len(dreamLinks) == 0 {
		slog.Info("dream: daily synthesis skipped (no activity)", "scope", scope)
		return "", nil
	}

	today := time.Now().UTC().Format("2006-01-02")
	userPrompt := buildDailyPrompt(today, decisions, dreamLinks, newBlocks)

	dreamVer := int16(Version)
	entry := &llmlog.Entry{
		Pipeline:      "dream-daily-synthesis",
		RequestSystem: dailySynthesisSystemPrompt,
		RequestUser:   userPrompt,
		DreamVersion:  &dreamVer,
	}
	defer func() { llmlog.Record(pool, entry.Slimmed()) }()

	start := time.Now()
	resp, served, attempts, err := r.chat(ctx, backends.RoleDigest, backends.SensInternal,
		dailySynthesisSystemPrompt, userPrompt, dailySynthesisOptions(), DailySynthesisTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err
	r.applyChainTelemetry(entry, backends.RoleDigest, backends.SensInternal, served, attempts, err)

	if resp != nil {
		entry.ResponseContent = resp.Message.Content
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
	}

	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	content := strings.TrimSpace(resp.Message.Content)
	if content == "" {
		return "", fmt.Errorf("dream: synthesize report: empty LLM response")
	}

	title := "Tagesbericht " + today
	tags := []string{"synthesis", "tagesbericht", "auto"}
	metadata := map[string]any{
		"source":       "dream-synthesis",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}

	block, err := store.UpsertBlock(ctx, pool, "learnings", title, content, tags, metadata, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	// lifecycle_state is not handled by the Welle-44 hook (which only sets
	// type_name). Set it explicitly so /api/query consumers can
	// distinguish synthesis output from generic learnings blocks.
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET lifecycle_state = 'synthesis' WHERE id = $1::uuid`,
		block.ID,
	); err != nil {
		return "", fmt.Errorf("dream: synthesize report: set lifecycle_state: %w", err)
	}

	// Welle 47 (W47-NEU-A) / WF T4: route through ClassifyBlockAfterUpsert so
	// the hook is the single source of truth for type_name — the rules come
	// from the registry snapshot (metadata.source='dream-synthesis' matches
	// the audit-trail source_prefixes seed). Keeps behaviour aligned with
	// handler/context_store.go and handler/mcp.go ctx_save paths.
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, r.TypeSet(ctx), block.ID, block.Title, block.Metadata); err != nil {
		return "", fmt.Errorf("dream: synthesize report: classify block: %w", err)
	}

	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["block_id"] = block.ID
	entry.BlockIDs = []string{block.ID}

	slog.Info("dream: daily synthesis complete",
		"block_id", block.ID,
		"scope", scope,
		"decisions", len(decisions),
		"dream_links", len(dreamLinks),
		"new_blocks", len(newBlocks),
	)

	return block.ID, nil
}

// fetchDailyDecisions reports counts per decision label from context_write_log
// for the supplied scope, ordered by frequency descending.
func fetchDailyDecisions(ctx context.Context, pool *pgxpool.Pool, scope string) ([]dailyDecisionStat, error) {
	rows, err := pool.Query(ctx,
		`SELECT decision, COUNT(*)::int FROM context_write_log
		 WHERE created_at > now() - interval '24 hours'
		   AND scope = $1
		 GROUP BY decision
		 ORDER BY COUNT(*) DESC`,
		scope,
	)
	if err != nil {
		return nil, fmt.Errorf("query decisions: %w", err)
	}
	defer rows.Close()

	var out []dailyDecisionStat
	for rows.Next() {
		var s dailyDecisionStat
		if err := rows.Scan(&s.Decision, &s.Count); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// fetchDailyNewBlocks lists up to 10 freshly created blocks in the last 24h
// for the supplied scope, newest first.
func fetchDailyNewBlocks(ctx context.Context, pool *pgxpool.Pool, scope string) ([]dailyNewBlock, error) {
	rows, err := pool.Query(ctx,
		`SELECT title, category FROM context_blocks
		 WHERE NOT is_archived
		   AND created_at > now() - interval '24 hours'
		   AND scope = $1
		 ORDER BY created_at DESC
		 LIMIT 10`,
		scope,
	)
	if err != nil {
		return nil, fmt.Errorf("query new blocks: %w", err)
	}
	defer rows.Close()

	var out []dailyNewBlock
	for rows.Next() {
		var b dailyNewBlock
		if err := rows.Scan(&b.Title, &b.Category); err != nil {
			return nil, fmt.Errorf("scan new block: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// fetchDailyDreamLinks reports counts per relationship class from
// context_dream_links produced in the last 24h, ordered by frequency.
// Dream-links are scope-bound on the source block, but the link table itself
// has no scope column — aggregation is global across scopes for the report.
func fetchDailyDreamLinks(ctx context.Context, pool *pgxpool.Pool) ([]dailyDreamLinkStat, error) {
	rows, err := pool.Query(ctx,
		`SELECT relationship, COUNT(*)::int FROM context_dream_links
		 WHERE created_at > now() - interval '24 hours'
		 GROUP BY relationship
		 ORDER BY COUNT(*) DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query dream links: %w", err)
	}
	defer rows.Close()

	var out []dailyDreamLinkStat
	for rows.Next() {
		var s dailyDreamLinkStat
		if err := rows.Scan(&s.Relationship, &s.Count); err != nil {
			return nil, fmt.Errorf("scan dream link: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// buildDailyPrompt assembles the structured user-prompt block fed to the LLM.
// Sections are omitted when their slice is empty so the prompt does not
// suggest that the missing axis was zero by mistake.
func buildDailyPrompt(date string, decisions []dailyDecisionStat, dreamLinks []dailyDreamLinkStat, newBlocks []dailyNewBlock) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Datum: %s\n", date)

	if len(decisions) > 0 {
		b.WriteString("\nDecisions (write-log):\n")
		for _, d := range decisions {
			fmt.Fprintf(&b, "- %s: %d\n", d.Decision, d.Count)
		}
	}

	if len(dreamLinks) > 0 {
		b.WriteString("\nDream-Links 24h:\n")
		for _, d := range dreamLinks {
			fmt.Fprintf(&b, "- %s: %d\n", d.Relationship, d.Count)
		}
	}

	if len(newBlocks) > 0 {
		b.WriteString("\nNeue Blocks 24h:\n")
		for _, nb := range newBlocks {
			fmt.Fprintf(&b, "- [%s] %s\n", nb.Category, nb.Title)
		}
	}

	return b.String()
}
