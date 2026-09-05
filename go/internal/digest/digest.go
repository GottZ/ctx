// Package digest builds topic maps from all context blocks.
// Deterministic clustering without LLM, output as pipe-delimited topic summaries.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunDigest builds a deterministic topic map (compact block index) for the given scope.
// Groups blocks by category, sorts alphabetically, and upserts as category=index, title=topic-map-{scope}.
// No LLM involved — purely deterministic.
//
// blocktypes (WF T4/T8) feeds BOTH the digest.include source sieve and the
// topic-map classify hook from ONE tenant-resolved snapshot per run. nil is
// a wiring bug and fails loudly (RunGuardBatch pattern) — since T8 the
// source query cannot run without the type allowlist.
//
// tenantScope (WF T12) is the iterating tenant's SCOPE — the key the policy
// overlay resolves against (config.SnapshotForTenant(bt.scope) twin). It is
// distinct from homeScope, which is the entitlement-clamped WRITE scope for the
// topic-map index title/row. Pre-T12 the registry ignored its argument, so the
// background loop passing homeScope was harmless; now the tenant key must be the
// tenant scope (default tenant → "_global" → base generation, unchanged).
//
// language is the corpus language (dream.language). It reaches only ONE byte of
// this package — the stub pointer of ModeStub — and it arrives as a PARAMETER
// rather than through a config read because .golangci.yml depguard bars
// internal/digest from importing internal/config. Empty or a German tag keeps
// the frozen German stub, every other tag renders English; both callers on the
// production path pass cfg.Dream.Language.
func RunDigest(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry,
	mode, language, tenantScope, homeScope string, readScopes []string) error {
	if blocktypes == nil {
		return fmt.Errorf("digest: nil block-type registry (wiring bug)")
	}
	set := blocktypes.SnapshotForTenant(ctx, tenantScope)

	// W-E: the mode decides BEFORE the source query. That order is the whole
	// point — the corpus scan (no LIMIT, no cursor, the whole result set as
	// []store.BlockMeta) is what makes the linear map untenable at scale, so a
	// mode that "only skips the line building" would leave the expensive half in
	// place.
	switch Normalize(mode) {
	case ModeOff:
		slog.Debug("digest: off, leaving the topic map untouched", "scope", homeScope)
		return nil
	case ModeStub:
		return writeStub(ctx, pool, set, homeScope, language)
	}

	// Fetch block metadata (no content), sieved by digest.include (WF T8,
	// design/01 §4.4 #13): an unregistered type is absent from the allowlist
	// and therefore fail-closed out of the topic-map source (§5.1).
	blocks, err := fetchBlockMeta(ctx, pool, readScopes, set.DigestTypes())
	if err != nil {
		return fmt.Errorf("digest: fetch meta: %w", err)
	}

	if len(blocks) == 0 {
		slog.Info("digest: no blocks found, skipping", "scope", homeScope)
		return nil
	}

	// Group by category.
	categories := make(map[string][]store.BlockMeta)
	for _, b := range blocks {
		categories[b.Category] = append(categories[b.Category], b)
	}

	// Sort category names alphabetically.
	catNames := make([]string, 0, len(categories))
	for cat := range categories {
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)

	// Build the compact index text.
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"Context Store Index | scope:%s | %d blocks | %d categories | %s\n",
		homeScope, len(blocks), len(catNames), time.Now().UTC().Format("2006-01-02"),
	)

	for _, cat := range catNames {
		catBlocks := categories[cat]
		// Sort blocks by title within each category.
		sort.Slice(catBlocks, func(i, j int) bool {
			return catBlocks[i].Title < catBlocks[j].Title
		})

		fmt.Fprintf(&sb, "\n%s (%d)\n", cat, len(catBlocks))
		for _, b := range catBlocks {
			// ID prefix: first 8 chars.
			idPrefix := b.ID
			if len(idPrefix) > 8 {
				idPrefix = idPrefix[:8]
			}

			// Title truncation: max 70 runes (rune-aware — a byte slice can split
			// a multi-byte char, leaving invalid UTF-8 that fails the upsert: 22021).
			title := truncateTitle(b.Title)

			// Scope annotation: append [scope] if different from homeScope.
			scopeAnnotation := ""
			if b.Scope != homeScope {
				scopeAnnotation = " [" + b.Scope + "]"
			}

			fmt.Fprintf(&sb, "  %s %s%s\n", idPrefix, title, scopeAnnotation)
		}
	}

	indexContent := sb.String()

	// Upsert as block: category=index, title=topic-map-{scope}.
	// Welle 47 (W47-NEU-A): the metadata KEY is_meta=true (classify INPUT —
	// the materialised column fell with M075/T9) plus a
	// ClassifyBlockAfterUpsert call route the topic-map through the Welle-44
	// hook → type_name='system-meta'. This keeps the topic-map out of retrieval (historically
	// the M036/M048 hard-exclude literal; since M073/T5+T6 the system-meta
	// policy is retrieval=excluded, so the type is simply absent from every
	// visibility allowlist) instead of letting it slot-steal retrieval
	// candidates (CRAG Phase 6 found 5/10 movie queries pulled
	// topic-map-private into top-5).
	indexTitle := "topic-map-" + homeScope
	indexTags := []string{"index", "topic-map", homeScope, "auto-generated"}
	indexMetadata := map[string]any{
		"source":         "context-digest",
		"is_meta":        true,
		"generated_at":   time.Now().UTC().Format("2006-01-02"),
		"scope":          homeScope,
		"block_count":    len(blocks),
		"category_count": len(catNames),
	}

	block, err := store.UpsertBlock(ctx, pool, "index", indexTitle, indexContent, indexTags, indexMetadata, homeScope, true, store.SensitivityWrite{}, "")
	if err != nil {
		return fmt.Errorf("digest: upsert topic map: %w", err)
	}

	// Welle 44 / WF T4 hook: classify type_name from the registry snapshot
	// (the run's ONE tenant-resolved set, WF T8). The topic-map's metadata
	// key is_meta=true makes the system-meta rule fire. Idempotent — re-runs
	// of RunDigest are no-ops at this layer.
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
		// Non-fatal: the topic-map block exists, classification can be retried
		// next cycle. Log + continue rather than fail the whole digest.
		slog.Warn("digest: topic map auto-classify failed", "error", err, "block_id", block.ID)
	}

	slog.Info("digest: topic map updated",
		"scope", homeScope,
		"blocks", len(blocks),
		"categories", len(catNames),
		"content_length", len(indexContent),
	)

	return nil
}

// Digest modes (design/02 §4.6, wave W-E). See config.DigestConfig for what
// each one is FOR; this is the vocabulary the pipeline speaks.
const (
	ModeFull = "full"
	ModeStub = "stub"
	ModeOff  = "off"
)

// Normalize maps a configured mode onto the vocabulary, falling back to `full`.
//
// Fail-closed here means falling back to the BEHAVIOUR THAT EXISTS: a typo in
// the mode must never silently stop the topic map (which `off` would) — the
// operator would find out weeks later from a block that stopped moving. The
// fallback is loud in the log and the key is hot, so the correction costs a
// settings write, not a deploy.
func Normalize(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeStub:
		return ModeStub
	case ModeOff:
		return ModeOff
	case ModeFull, "":
		return ModeFull
	default:
		slog.Warn("digest: unknown digest.mode, falling back to full", "mode", mode)
		return ModeFull
	}
}

// stubLanguage reduces a dream.language value to its primary subtag, the same
// reduction dream.reportLanguage, topiclabel.promptLanguage and rootmap
// .mapLanguage apply. A LOCAL copy for the same reason they are: this package
// may not import internal/config (depguard config-layering), and the language
// SURFACE is deliberately not shared between the packages that render text —
// only the KEY is (E3-01, one language knob per corpus).
func stubLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexByte(lang, '-'); i >= 0 {
		lang = lang[:i]
	}
	return lang
}

// stubText is the ~300 B pointer the linear map becomes. It is a CONSTANT
// shape on purpose: no wall clock, no counts, nothing that moves. A stub that
// re-renders differently every minute would trade an 80 KB rewrite per cycle
// for a 300 B one — smaller, but the same pointless write, and the content
// comparison below could not skip it.
//
// The language is the only thing that varies, and it varies at most once per
// corpus: the empty tag and every German tag keep the byte-frozen German stub
// (no live block is rewritten), every other tag renders English — the same two
// tables and the same primary-subtag rule the root map it points AT uses. A
// pointer that speaks a different language than its target would be a worse
// forwarding address than none.
//
// What survives translation verbatim: the two CLI lines. They are commands, and
// a translated command does not run.
func stubText(homeScope, language string) string {
	if lang := stubLanguage(language); lang != "" && lang != "de" {
		return "This map has been superseded.\n" +
			"The root map of this scope is called: root-map-" + homeScope + "\n" +
			"  ctx search index query:root-map    ·    ctx get <id from the result list>\n" +
			"It groups by topic clusters (Louvain over the dream graph) rather than by\n" +
			"category and is capped at ~15 KB. Produced by the overview rebuild cycle.\n"
	}
	return "Diese Karte wurde abgelöst.\n" +
		"Die Wurzel-Map dieses Scopes heißt: root-map-" + homeScope + "\n" +
		"  ctx search index query:root-map    ·    ctx get <id aus der Trefferliste>\n" +
		"Sie gliedert nach Themen-Clustern (Louvain über den Dream-Graphen) statt nach\n" +
		"Kategorien und ist auf ~15 KB gedeckelt. Erzeugt am Overview-Rebuild-Zyklus.\n"
}

// writeStub replaces the linear map with the pointer — the ONLY write of stub
// mode, and only when the text is not already there.
//
// Why a stub rather than archiving the block: an archived block is gone from
// `ctx get` and `ctx search`, so every consumer that looks the map up where the
// ctx-digest skill tells them to (`ctx search index query:topic-map`) would find
// nothing AND no hint where to go instead. 300 B is the cheapest possible
// forwarding address, and it reaches the consumers exactly where they search
// (E2-02/E9-02 — the stub carries the transition; archiving the two dead
// scope blocks is its own decided wave).
func writeStub(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, homeScope, language string) error {
	title := "topic-map-" + homeScope
	text := stubText(homeScope, language)

	old, found, err := store.MapBlockContent(ctx, pool, "index", title, homeScope)
	if err != nil {
		return fmt.Errorf("digest: read topic map: %w", err)
	}
	if found && old == text {
		slog.Debug("digest: stub unchanged", "scope", homeScope)
		return nil
	}

	block, err := store.UpsertBlock(ctx, pool, "index", title, text,
		[]string{"index", "topic-map", homeScope, "auto-generated", "superseded"},
		map[string]any{
			"source":        "context-digest",
			"is_meta":       true,
			"mode":          ModeStub,
			"superseded_by": "root-map-" + homeScope,
			"scope":         homeScope,
		},
		homeScope, true, store.SensitivityWrite{}, "")
	if err != nil {
		return fmt.Errorf("digest: upsert topic map stub: %w", err)
	}

	// Same classify hook as the full map: is_meta=true keeps the stub on
	// system-meta, so it stays out of retrieval and inside the browse channel
	// that makes it findable at all.
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
		slog.Warn("digest: stub auto-classify failed", "error", err, "block_id", block.ID)
	}
	slog.Info("digest: topic map replaced by stub", "scope", homeScope, "content_length", len(text))
	return nil
}

// truncateTitle caps a topic-map row title at 70 runes (rune-aware). A byte
// slice can split a multi-byte rune (em-dash, ellipsis, CJK, emoji), leaving
// invalid UTF-8 that PostgreSQL rejects on upsert with SQLSTATE 22021 —
// regression target of Issue #4. This is an inline COPY of the shared helper,
// not a call into it: digest imports no internal package besides blocktype and
// store, and truncateTitle(t) returns byte for byte what util.TruncateRunes(t,
// 70) returns (util/strings.go:19-30, same 70/67 arithmetic).
func truncateTitle(title string) string {
	if utf8.RuneCountInString(title) > 70 {
		return string([]rune(title)[:67]) + "..."
	}
	return title
}

// fetchBlockMeta retrieves non-archived block metadata for the given scopes,
// restricted to the digest.include type allowlist (WF T8). digestTypes is a
// code-owned bind value from the run's policy snapshot, never user input; an
// empty list is legitimate policy ("nothing digests") and yields no rows.
func fetchBlockMeta(ctx context.Context, pool *pgxpool.Pool, readScopes, digestTypes []string) ([]store.BlockMeta, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		FROM context_blocks
		WHERE scope = ANY($1::text[]) AND NOT is_archived
		  AND type_name = ANY($2::text[])
		ORDER BY category, title`,
		readScopes, digestTypes,
	)
	if err != nil {
		return nil, fmt.Errorf("query block meta: %w", err)
	}
	defer rows.Close()

	var results []store.BlockMeta
	for rows.Next() {
		var b store.BlockMeta
		if err := rows.Scan(&b.ID, &b.Title, &b.Category, &b.Tags, &b.Scope, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan block meta: %w", err)
		}
		results = append(results, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return results, nil
}
