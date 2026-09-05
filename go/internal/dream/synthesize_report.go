package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/util"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DailySynthesisTimeout is the LLM-call budget for the daily report
	// generator. Aliased to DreamTimeout because the generator runs through
	// the same Ollama dream model and inherits the same cold-start +
	// queue-wait constraints (Welle 38c / Welle 42).
	DailySynthesisTimeout = DreamTimeout

	// dailySynthesisSystemPrompt is the LEGACY (empty / "de") system prompt:
	// a compact German activity report for the last 24h over the aggregate
	// statistics of the user prompt. Byte-frozen — every deployment that has
	// not set dream.language keeps generating exactly this.
	dailySynthesisSystemPrompt = `Erzeuge einen kompakten Tagesbericht (200-400 Worte) für ein Knowledge-Store-System. Schreibe als Fließtext in Deutsch. Zähle Schwerpunkte der letzten 24h auf, nenne neue Themen, betone Patterns oder Anomalien.`

	// legacyReportTitlePrefix / legacyReportTag are the German report's
	// identity. The title is HALF THE UPSERT KEY ((category, title, scope),
	// store.UpsertBlock) — changing it does not rename anything, it starts a
	// SECOND series next to the existing one and makes every backfill of an
	// old day a duplicate. Hence the empty default in config: no existing
	// deployment moves off these two strings unless an operator says so.
	legacyReportTitlePrefix = "Tagesbericht "
	legacyReportTag         = "tagesbericht"
)

// isLegacyReportLanguage reports whether lang keeps the pre-config German
// report surface: unset (the default) or a German tag. This ONE predicate
// gates title, tag and system prompt together — they are one identity, and a
// half-localized report (English title, German body) would be worse than
// either side alone.
func isLegacyReportLanguage(lang string) bool {
	switch util.PrimaryLanguageSubtag(lang) {
	case "", "de":
		return true
	default:
		return false
	}
}

// dailySynthesisPromptFor returns the system prompt for the configured
// language: the byte-frozen German legacy prompt for ""/"de*", else an
// English instruction naming the target language.
func dailySynthesisPromptFor(lang string) string {
	if isLegacyReportLanguage(lang) {
		return dailySynthesisSystemPrompt
	}
	return `Generate a compact daily report (200-400 words) for a knowledge-store system. Write as continuous prose in ` + langName(util.PrimaryLanguageSubtag(lang)) + `. List the main focus areas of the last 24 hours, name new topics, and highlight patterns or anomalies.`
}

// dailyReportTitleFor returns the block title — half the upsert key, see
// legacyReportTitlePrefix.
func dailyReportTitleFor(lang, date string) string {
	if isLegacyReportLanguage(lang) {
		return legacyReportTitlePrefix + date
	}
	return "Daily Report " + date
}

// dailyReportTags returns the report's tag set. The language-dependent slot
// travels WITH the title: a de-tagged report keeps "tagesbericht" so existing
// tag queries and dashboards keep matching the series they were written for.
func dailyReportTags(lang string) []string {
	if isLegacyReportLanguage(lang) {
		return []string{"synthesis", legacyReportTag, "auto"}
	}
	return []string{"synthesis", "daily-report", "auto"}
}

// langName maps a language's PRIMARY SUBTAG to the English language name
// interpolated into the system prompt. Callers pass
// util.PrimaryLanguageSubtag(...) of a NON-legacy tag, so ""/"de" never arrive
// here (they take the frozen German prompt) — no case for them. Unknown tags
// pass through as-is: the LLM knows far more language codes than this table,
// and V14 has already constrained the value to [a-z0-9-], so the passthrough
// carries no free text.
//
// DELIBERATELY NOT MERGED with topiclabel.languageName (design D-04, Naht 9).
// The two tables differ in exactly one branch and the difference is the
// contract: ""/"de" is UNREACHABLE here and maps to "German" there. Merging
// them would either add a dead German case to this table or make the label
// surface fall through to a passthrough for the default corpus language. The
// shared part — the subtag reduction — is util.PrimaryLanguageSubtag; the
// name tables are not shared, and that is not an oversight to clean up later.
func langName(primary string) string {
	switch primary {
	case "en":
		return "English"
	case "tr":
		return "Turkish"
	case "fr":
		return "French"
	case "es":
		return "Spanish"
	case "pt":
		return "Portuguese"
	case "ru":
		return "Russian"
	case "zh":
		return "Chinese"
	case "ja":
		return "Japanese"
	default:
		return primary
	}
}

// dailySynthesisOptions are the LLM sampling options for the daily report.
// Sampling params match DreamOptions (qwen3.6:27b non-thinking tuning), but
// deliberately WITHOUT NumPredict: the prompt requests a 200-400 word report
// (~600-900 tokens) and the model terminates via EOS — sharing the dream-eval
// cap (400 tokens at the time) truncated every report mid-sentence for 66 days
// (all dream-daily-synthesis llm_log rows at exactly completion_tokens=400).
// The dream-eval cap is 600 today and still too tight for a report, so the
// omission stands on its own reasoning, not on that one number.
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

// dailyStructuralLinkStat aggregates the count for one (link_class, origin)
// pair of structural links created in the window — the deterministic fact
// edges (M076/M103), reported SEPARATELY from dream links (strictly separate
// data classes, architecture.md §Structural links). GD2 (W04-5).
type dailyStructuralLinkStat struct {
	LinkClass string
	Origin    string
	Count     int
}

// dailyGuardStat carries the guard review-queue STAND (not a window delta):
// open flagged blocks in the report scope + the age of the queue head. It is
// the report-side push of the needs_review pipeline (guard W2) — an unworked
// queue surfaces in the daily report instead of waiting for a guard-stats
// pull. nil ⇒ no section (live path only; a backfilled yesterday-report must
// not carry today's queue stand).
type dailyGuardStat struct {
	NeedsReview       int
	NearDuplicate     int
	PossibleDuplicate int
	OldestDays        int
}

func (g *dailyGuardStat) total() int {
	return g.NeedsReview + g.NearDuplicate + g.PossibleDuplicate
}

// dailyNewBlock identifies a fresh block (created in the last 24h) by its
// category and title — sufficient context for the LLM without dragging full
// content into the synthesis prompt. ID feeds the deterministic report→source
// structural edges (writeReportSourceLinks), never the prompt.
type dailyNewBlock struct {
	ID       string
	Title    string
	Category string
}

// dailySynthesisHourUTC mirrors the scheduler's dailySynthesisHour (events
// package, local clock — the container runs UTC): the hour at which the 03:00
// iteration generates the report titled <today> over the previous 24h.
// Backfill windows reproduce exactly that shape.
const dailySynthesisHourUTC = 3

// GenerateDailyReport queries the last-24h activity (decisions, dream-links,
// fresh blocks), asks the LLM for a free-text summary in the router's report
// language (Router.Language; empty = the legacy German report), and persists
// the result as a synthesis/audit-trail block. Returns the new block_id, or
// an empty string + nil error when there was zero activity to report.
// The chain resolves for role digest at CONSTANT internal (E6): the prompt
// carries aggregate counts + titles/categories — structure, not content. A
// secret in a TITLE is a corpus hygiene problem, not a routing problem. Both
// digest callers (03:00 scheduler iteration and the manual
// POST /api/synthesize/daily trigger) gate identically through here.
func GenerateDailyReport(ctx context.Context, pool *pgxpool.Pool, r *Router, scope string) (string, error) {
	now := time.Now().UTC()
	return generateDailyReportWindow(ctx, pool, r, scope,
		now.Add(-24*time.Hour), now, now.Format("2006-01-02"), true)
}

// GenerateDailyReportFor re-synthesizes the report titled day (backfill): the
// window is [day-1 03:00 UTC, day 03:00 UTC) — exactly what the 03:00
// scheduler covered when it generated the report for <day>. The upsert key
// (category, title, scope) replaces an existing report in place: same block
// id, inbound edges survive, the embedding regenerates from the new content.
func GenerateDailyReportFor(ctx context.Context, pool *pgxpool.Pool, r *Router, scope string, day time.Time) (string, error) {
	day = day.UTC()
	end := time.Date(day.Year(), day.Month(), day.Day(), dailySynthesisHourUTC, 0, 0, 0, time.UTC)
	// includeGuardQueue=false: the guard queue is a STAND, not window activity —
	// a backfilled yesterday-report must not carry today's queue.
	return generateDailyReportWindow(ctx, pool, r, scope,
		end.Add(-24*time.Hour), end, day.Format("2006-01-02"), false)
}

// generateDailyReportWindow is the shared core: aggregate the [from, to)
// activity, synthesize, upsert the report titled <date> (see
// dailyReportTitleFor) and anchor its source edges. date is the title/prompt
// day (not derived from the window bounds — the rolling path's window ends
// now, the backfill path's at 03:00).
// includeGuardQueue gates the guard review-queue STAND section (live path
// only — see GenerateDailyReportFor).
func generateDailyReportWindow(ctx context.Context, pool *pgxpool.Pool, r *Router, scope string, from, to time.Time, date string, includeGuardQueue bool) (string, error) {
	decisions, err := fetchDailyDecisions(ctx, pool, scope, from, to)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	newBlocks, err := fetchDailyNewBlocks(ctx, pool, scope, from, to)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	dreamLinks, err := fetchDailyDreamLinks(ctx, pool, scope, from, to)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	structLinks, err := fetchDailyStructuralLinks(ctx, pool, scope, from, to)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: %w", err)
	}

	var guardQueue *dailyGuardStat
	if includeGuardQueue {
		guardQueue, err = fetchDailyGuardReview(ctx, pool, scope, to)
		if err != nil {
			return "", fmt.Errorf("dream: synthesize report: %w", err)
		}
	}

	// Skip gate DELIBERATELY unchanged in shape (GD2, design/04 §4.3 pt. 3):
	// structural-only activity (e.g. a forge re-sync touching only references)
	// produces NO LLM report — a report without content activity would be
	// noise. The structural section only appears when we report anyway.
	// Since GD3 the gate is scope-LOCAL (dreamLinks is scope-filtered):
	// inactive scopes no longer report just because another tenant dreamed —
	// deliberate behavior change, decision E14.
	if len(decisions) == 0 && len(newBlocks) == 0 && len(dreamLinks) == 0 {
		slog.Info("dream: daily synthesis skipped (no activity)", "scope", scope, "date", date)
		return "", nil
	}

	userPrompt := buildDailyPrompt(date, decisions, dreamLinks, structLinks, newBlocks, guardQueue)

	sysPrompt := dailySynthesisPromptFor(r.Language)
	// nil block ids on purpose: the row is about the report block this run is
	// about to write, and that block does not exist yet — its id lands on the
	// entry after the write, further down. A nil slice persists as a NULL
	// block_ids column, an empty one as an empty array; they are not the same
	// row, so the daily-synthesis entry is constructed WITHOUT the field.
	entry := newDreamEntry("dream-daily-synthesis", sysPrompt, userPrompt, nil)
	defer func() { llmlog.Record(pool, entry.Slimmed(r.Devmode)) }()

	start := time.Now()
	// chatPlain, NOT chat: this stage asks for continuous prose ("Schreibe als
	// Fließtext" / "Write as continuous prose", dailySynthesisPromptFor) and
	// the answer is stored VERBATIM as the report block's body a few lines
	// below — no JSON is decoded anywhere on this path. On a backend that
	// really enforces the JSON mode the marker is therefore not a validator
	// but a corruptor: the "Tagesbericht <date>" body comes back as a JSON
	// envelope with a made-up key. It stays plain unconditionally — the
	// pipeline's JSON-mode setting has nothing to decide here.
	resp, served, attempts, err := r.chatPlain(ctx, backends.RoleDigest, backends.SensInternal,
		sysPrompt, userPrompt, dailySynthesisOptions(), DailySynthesisTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err
	r.applyChainTelemetry(entry, backends.RoleDigest, backends.SensInternal, served, resp, attempts, err)

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

	title := dailyReportTitleFor(r.Language, date)
	tags := dailyReportTags(r.Language)
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
	typeName, err := store.ClassifyBlockAfterUpsert(ctx, pool, r.TypeSet(ctx), block.ID, block.Title, block.Metadata)
	if err != nil {
		return "", fmt.Errorf("dream: synthesize report: classify block: %w", err)
	}

	// M103: deterministic fact-anchor — the report enumerates its sources, so
	// persist report→source references edges without an LLM in the loop.
	// Non-fatal: the report block stands on its own; a link failure must not
	// discard a finished synthesis (decoupled like the scheduler's cleanup).
	linkCount, err := writeReportSourceLinks(ctx, pool, r.TypeSet(ctx), typeName, block.ID, scope, newBlocks)
	if err != nil {
		slog.Error("dream: report source links failed", "block_id", block.ID, "error", err)
	}

	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["block_id"] = block.ID
	entry.BlockIDs = []string{block.ID}

	guardOpen := 0
	if guardQueue != nil {
		guardOpen = guardQueue.total()
	}
	slog.Info("dream: daily synthesis complete",
		"block_id", block.ID,
		"scope", scope,
		"decisions", len(decisions),
		"dream_links", len(dreamLinks),
		"structural_links", len(structLinks),
		"new_blocks", len(newBlocks),
		"guard_review_open", guardOpen,
		"source_links", linkCount,
	)

	return block.ID, nil
}

// fetchDailyDecisions reports counts per decision label from context_write_log
// for the supplied scope and [from, to) window, ordered by frequency descending.
func fetchDailyDecisions(ctx context.Context, pool *pgxpool.Pool, scope string, from, to time.Time) ([]dailyDecisionStat, error) {
	rows, err := pool.Query(ctx,
		`SELECT decision, COUNT(*)::int FROM context_write_log
		 WHERE created_at >= $2 AND created_at < $3
		   AND scope = $1
		 GROUP BY decision
		 ORDER BY COUNT(*) DESC`,
		scope, from, to,
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

// fetchDailyNewBlocks lists up to 10 blocks created in the [from, to) window
// for the supplied scope, newest first. Backfill fidelity note: is_archived is
// evaluated at query time — a block archived since its window day no longer
// appears in a re-synthesized report.
func fetchDailyNewBlocks(ctx context.Context, pool *pgxpool.Pool, scope string, from, to time.Time) ([]dailyNewBlock, error) {
	rows, err := pool.Query(ctx,
		`SELECT id::text, title, category FROM context_blocks
		 WHERE NOT is_archived
		   AND created_at >= $2 AND created_at < $3
		   AND scope = $1
		 ORDER BY created_at DESC
		 LIMIT 10`,
		scope, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query new blocks: %w", err)
	}
	defer rows.Close()

	var out []dailyNewBlock
	for rows.Next() {
		var b dailyNewBlock
		if err := rows.Scan(&b.ID, &b.Title, &b.Category); err != nil {
			return nil, fmt.Errorf("scan new block: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// writeReportSourceLinks persists the report's enumerated sources as
// deterministic structural edges: report →references→ each fetched new block.
// The class gates through the source type's structural_link_classes allowlist
// (design/02 §4.1, fail-closed — an operator removing "references" from the
// audit-trail config switches the edges off, policy is data). Scope safety is
// PutStructuralLink's double line: source writable + target in the SAME scope,
// validated in ONE tx. A target that vanished between fetch and write skips
// (ErrLinkScopeViolation) without failing the batch.
func writeReportSourceLinks(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, typeName, reportID, scope string, sources []dailyNewBlock) (int, error) {
	if set == nil {
		return 0, fmt.Errorf("nil block-type set")
	}
	p, ok := set.Resolve(typeName)
	if !ok || !slices.Contains(p.StructuralLinkClasses, "references") {
		slog.Debug("dream: report source links skipped (type does not declare references)",
			"type", typeName)
		return 0, nil
	}

	written := 0
	if err := pgxdb.Write(ctx, pool, pgxdb.Stages{Begin: "begin", Commit: "commit"}, func(tx pgx.Tx) error {
		for _, src := range sources {
			if src.ID == reportID {
				continue
			}
			err := store.PutStructuralLink(ctx, tx, store.StructuralLink{
				SourceID:  reportID,
				TargetID:  src.ID,
				LinkClass: "references",
				Origin:    "system",
				Metadata:  map[string]any{"source": "dream-synthesis"},
			}, []string{scope})
			if errors.Is(err, store.ErrLinkScopeViolation) {
				continue
			}
			if err != nil {
				return err
			}
			written++
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return written, nil
}

// fetchDailyDreamLinks reports counts per relationship class from
// context_dream_links produced in the [from, to) window, ordered by frequency,
// SCOPE-FILTERED since GD3 (decision E14): the former global aggregation made
// report numbers a cross-tenant blur AND turned the skip gate into a foreign-
// activity oracle (any tenant's dream links kept every scope reporting). Rides
// idx_dream_links_scope_created (M106).
// Backfill fidelity note: created_at is bumped on link replace (WriteLinks
// upsert), so a historical window counts the links as they stand today.
func fetchDailyDreamLinks(ctx context.Context, pool *pgxpool.Pool, scope string, from, to time.Time) ([]dailyDreamLinkStat, error) {
	rows, err := pool.Query(ctx,
		`SELECT relationship, COUNT(*)::int FROM context_dream_links
		 WHERE scope = $1 AND created_at >= $2 AND created_at < $3
		 GROUP BY relationship
		 ORDER BY COUNT(*) DESC`,
		scope, from, to,
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

// fetchDailyStructuralLinks reports counts per (link_class, origin) pair from
// context_structural_links created in the [from, to) window, SCOPE-FILTERED
// like fetchDailyDecisions/fetchDailyNewBlocks (and, since GD3, like the
// dream aggregation above — design/04 §5 B1). Runs as an index-only scan on
// idx_struct_links_scope_created (M106 covering, GD1).
// Time semantics: the current report's own edges are written AFTER the prompt
// build and never land in their own window; a follow-up/backfill run's
// yesterday window counts them correctly. Unlike dream links (created_at
// bumped on replace) structural rows are insert-only with ON CONFLICT DO
// NOTHING — the window count is backfill-faithful.
func fetchDailyStructuralLinks(ctx context.Context, pool *pgxpool.Pool, scope string, from, to time.Time) ([]dailyStructuralLinkStat, error) {
	rows, err := pool.Query(ctx,
		`SELECT link_class, origin, COUNT(*)::int FROM context_structural_links
		 WHERE scope = $1 AND created_at >= $2 AND created_at < $3
		 GROUP BY 1, 2
		 ORDER BY COUNT(*) DESC`,
		scope, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query structural links: %w", err)
	}
	defer rows.Close()

	var out []dailyStructuralLinkStat
	for rows.Next() {
		var s dailyStructuralLinkStat
		if err := rows.Scan(&s.LinkClass, &s.Origin, &s.Count); err != nil {
			return nil, fmt.Errorf("scan structural link: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// fetchDailyGuardReview reads the open guard review-queue STAND for the report
// scope (guard W2): flagged counts per state + queue-head age in whole days,
// measured against the window end. Single-scope (GD3 line — deliberately NOT
// store.GetGuardStats, whose ReadScopes-array semantics belong to the manage
// pull path). Returns nil when the queue is empty, so an empty queue keeps the
// prompt bytes identical (omission semantics).
func fetchDailyGuardReview(ctx context.Context, pool *pgxpool.Pool, scope string, asOf time.Time) (*dailyGuardStat, error) {
	g := &dailyGuardStat{}
	var oldest *time.Time
	err := pool.QueryRow(ctx,
		`SELECT
			(count(*) FILTER (WHERE guard_status = 'needs_review'))::int,
			(count(*) FILTER (WHERE guard_status = 'near_duplicate'))::int,
			(count(*) FILTER (WHERE guard_status = 'possible_duplicate'))::int,
			min(updated_at)
		 FROM context_blocks
		 WHERE scope = $1 AND NOT is_archived
		   AND guard_status IN ('needs_review', 'near_duplicate', 'possible_duplicate')`,
		scope,
	).Scan(&g.NeedsReview, &g.NearDuplicate, &g.PossibleDuplicate, &oldest)
	if err != nil {
		return nil, fmt.Errorf("query guard review queue: %w", err)
	}
	if g.total() == 0 {
		return nil, nil
	}
	if oldest != nil {
		g.OldestDays = int(asOf.Sub(*oldest).Hours() / 24)
	}
	return g, nil
}

// clampField is the daily-prompt wiring of promptguard (design 04 §4.4 row 8).
//
// ClampLine FIRST, Neutralize second — the same order promptguard uses for its
// own marker attributes. The daily prompt is LINE-BASED: every foreign value
// is one item on one line, so the line break is the structural character here
// and a turn marker without its newlines is already inert. Neutralize still
// runs because the ChatML openers carry no newline at all, and this prompt
// escapes nothing (there is no XML here to escape into).
//
// No truncation: every value in this prompt is an aggregate label or a block
// title, both length-capped on the write path.
func clampField(s string) string {
	n, _ := promptguard.Neutralize(promptguard.ClampLine(s))
	return n
}

// buildDailyPrompt assembles the structured user-prompt block fed to the LLM.
// Sections are omitted when their slice is empty (guard: nil) so the prompt
// does not suggest that the missing axis was zero by mistake.
//
// The data-section labels ("Datum:", "Neue Blocks 24h:", …) stay GERMAN in
// every language — deliberate. They are stable structural markers of the data
// frame, not report text: the system prompt sets the OUTPUT language and the
// LLM translates the content it reads. Localizing the labels would move a
// frozen prompt surface (and its 66-day-tuned behavior) for zero gain, and
// make the German↔English prompt pair diff-noisy for no functional reason.
//
// Every DB-sourced value below runs through clampField: decisions,
// relationships, link classes, origins, categories and block titles are all
// foreign text, and a newline in any of them used to forge an extra item line
// (design 04 §2.3-b).
func buildDailyPrompt(date string, decisions []dailyDecisionStat, dreamLinks []dailyDreamLinkStat, structLinks []dailyStructuralLinkStat, newBlocks []dailyNewBlock, guardQueue *dailyGuardStat) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Datum: %s\n", date)

	if len(decisions) > 0 {
		b.WriteString("\nDecisions (write-log):\n")
		for _, d := range decisions {
			fmt.Fprintf(&b, "- %s: %d\n", clampField(d.Decision), d.Count)
		}
	}

	if len(dreamLinks) > 0 {
		b.WriteString("\nDream-Links 24h:\n")
		for _, d := range dreamLinks {
			fmt.Fprintf(&b, "- %s: %d\n", clampField(d.Relationship), d.Count)
		}
	}

	// Structural facts after the dream section (GD2): the origin split shows
	// what is pipeline self-description (system) vs sync/operator activity.
	if len(structLinks) > 0 {
		b.WriteString("\nStructural-Links 24h:\n")
		for _, s := range structLinks {
			fmt.Fprintf(&b, "- %s (%s): %d\n", clampField(s.LinkClass), clampField(s.Origin), s.Count)
		}
	}

	if len(newBlocks) > 0 {
		b.WriteString("\nNeue Blocks 24h:\n")
		for _, nb := range newBlocks {
			fmt.Fprintf(&b, "- [%s] %s\n", clampField(nb.Category), clampField(nb.Title))
		}
	}

	// Guard review-queue STAND after the activity sections (guard W2): the
	// open queue is a state of the corpus, not a window event — it closes the
	// report so an unworked queue is named every day it stays open.
	if guardQueue != nil && guardQueue.total() > 0 {
		b.WriteString("\nGuard-Review offen (Stand heute):\n")
		if guardQueue.NeedsReview > 0 {
			fmt.Fprintf(&b, "- needs_review: %d\n", guardQueue.NeedsReview)
		}
		if guardQueue.NearDuplicate > 0 {
			fmt.Fprintf(&b, "- near_duplicate: %d\n", guardQueue.NearDuplicate)
		}
		if guardQueue.PossibleDuplicate > 0 {
			fmt.Fprintf(&b, "- possible_duplicate: %d\n", guardQueue.PossibleDuplicate)
		}
		fmt.Fprintf(&b, "- ältester Eintrag: %d Tage\n", guardQueue.OldestDays)
	}

	return b.String()
}
