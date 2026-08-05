// Run is the write path of the root map (Cluster-Topic-Map, design/02 §4.2,
// wave W-D): read → render → decide → maybe write. The renderer next door stays
// pure; everything that touches the world lives here, and it touches it through
// the W-B store reads only — one scope-leak surface for a PERSISTED artefact,
// never two (§5.1).
//
// Three refusals carry the wave, and each is a REFUSAL rather than a degraded
// write:
//
//   - The map never replaces a good map with an empty or stale one (BP-6,
//     AllowsUpsert). A render landing inside a global rebuild's TRUNCATE window
//     sees zero clusters; writing that is how a working map disappears.
//   - The map never outgrows its budget, and never the 50 KB public write cap
//     (BP-5). The old topic map lives at 80.103 characters purely because the
//     digest bypasses that cap — the new one refuses instead.
//   - The map never writes bytes it already wrote (§4.2 step 6). Every
//     content-changing upsert invalidates the embedding and rewrites TOAST
//     pages; identical text is a no-op, which is only decidable because rule R3
//     keeps the wall clock out of the rendered text.

package rootmap

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

const (
	// BlockCategory is the map's category. 'index' is load-bearing, not
	// cosmetic: guard and dream both sieve on it, so the map inherits their
	// exclusion without a single new gate.
	BlockCategory = "index"
	// TitlePrefix keeps the root map on its OWN upsert conflict key. Taking the
	// topic map's title would put two producers with a 60 s and a 6 h cadence on
	// one key — a block flapping between two formats.
	TitlePrefix = "root-map-"
	// storeWriteCap mirrors handler.blockSizeLimit: what the public write API
	// refuses, the background must not smuggle past it.
	storeWriteCap = 50 * 1024
)

// operationalRationales names the types that are outside the topic map by
// DECISION and says why in one clause (E11-02 / amendment A02-2).
//
// Code, not policy — deliberately: WHICH types exist is registry data, but the
// SENTENCE that keeps a future session from re-escalating 83 % "missing
// coverage" is a decision record. A type absent from the registry never reaches
// the map, so the table can only under-declare, never invent a line.
var operationalRationales = map[string]string{
	"checkpoint": "hermes-Compaction-Artefakte",
}

// Config is the root_map.* policy snapshot for ONE run plus the two cadence
// facts the head line needs. Interval and TenantCount are display inputs, not
// knobs: they describe the rebuild loop the map hangs on, and printing the raw
// interval without the tenant count would promise a freshness the round-robin
// cannot hold.
type Config struct {
	Enabled            bool
	BudgetBytes        int
	FooterReserveBytes int
	SmallClusterMax    int
	CountTimeout       time.Duration
	RebuildInterval    time.Duration
	// SuperEnabled gates the W-F meta section — the READ side of it. The
	// production side is the same key on the rebuild (overview.Options), so
	// turning the flag off makes the section disappear from the next map
	// immediately instead of waiting for the rebuild that clears the rows.
	SuperEnabled bool
}

// Result says what the run did and, when it did nothing, why. Skipped is never
// an error: "the flag is off", "nothing changed" and "the window is empty" are
// all correct outcomes, and only the caller's log level tells them apart.
type Result struct {
	Skipped bool
	Reason  string // "disabled" | "unchanged" | "empty-window" | ""
	Written bool
	BlockID string
	Title   string
	Length  int
	Text    string
}

// Run renders and (only if it should) writes the root map of ONE tenant.
//
// homeScope is the entitlement-clamped WRITE scope, readScopes the read window —
// the BP-4 split RunDigest demonstrates. Getting it wrong writes foreign
// aggregates into a foreign scope; the live artefact `topic-map-hth` (title says
// hth, scope says work) is what that looks like years later.
func Run(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, cfg Config,
	homeScope string, readScopes []string) (Result, error) {
	title := TitlePrefix + homeScope
	if !cfg.Enabled {
		return Result{Skipped: true, Reason: "disabled", Title: title}, nil
	}
	if err := validateConfig(cfg); err != nil {
		return Result{Title: title}, err
	}
	if set == nil {
		// Loud, never silent: without the registry the map cannot be classified
		// into system-meta, and an unclassified index block is a retrieval
		// candidate — the slot theft this whole type policy exists to prevent.
		return Result{Title: title}, fmt.Errorf("rootmap: nil block-type set (registry not wired)")
	}
	if homeScope == "" {
		return Result{Title: title}, fmt.Errorf("rootmap: empty home scope (BP-4: the map has no write scope)")
	}

	in, err := gather(ctx, pool, set, cfg, homeScope, readScopes)
	if err != nil {
		return Result{Title: title}, err
	}

	rendered, err := Render(in)
	if err != nil {
		return Result{Title: title}, err
	}
	if !AllowsUpsert(in, rendered) {
		// BP-6. WARN, not error: the state is legitimate (a rebuild is running,
		// or the partition is gone) — what would be wrong is the write.
		slog.Warn("rootmap: empty or stale window, keeping the existing map",
			"scope", homeScope, "cluster_n", in.Freshness.ClusterN,
			"stale_scopes", in.Freshness.StaleScopes)
		return Result{Skipped: true, Reason: "empty-window", Title: title}, nil
	}
	if err := checkSize(rendered.Text, cfg); err != nil {
		return Result{Title: title}, err
	}

	// Content idempotency. The comparison is on bytes, which is only meaningful
	// because no wall clock reaches the text (R3) — generated_at lives in
	// metadata, where a no-op write never happens to begin with.
	old, found, err := store.MapBlockContent(ctx, pool, BlockCategory, title, homeScope)
	if err != nil {
		return Result{Title: title}, err
	}
	if found && old == rendered.Text {
		return Result{Skipped: true, Reason: "unchanged", Title: title,
			Length: len(rendered.Text), Text: rendered.Text}, nil
	}

	// SensitivityWrite without Manual/Detector: the INSERT stamps `internal`
	// (E4-02 — the map carries labels, counts and IDs, no block content, and the
	// pool default `credentials` would seal it against every external backend),
	// while the ON CONFLICT path leaves an existing block's sensitivity alone.
	// That is exactly right: a downgrade of an EXISTING block is a
	// confirm-gated operation, and this write must never perform one silently.
	block, err := store.UpsertBlock(ctx, pool, BlockCategory, title, rendered.Text,
		[]string{"index", "root-map", homeScope, "auto-generated"},
		metadataFor(in, rendered, cfg, homeScope),
		homeScope, true, store.SensitivityWrite{Value: backends.SensInternal}, "")
	if err != nil {
		return Result{Title: title}, fmt.Errorf("rootmap: upsert map: %w", err)
	}

	// Classify hook: metadata.is_meta=true makes the system-meta rule fire, and
	// that is what keeps the map out of retrieval. Non-fatal — the block exists,
	// the next cycle retries — but never silent.
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
		slog.Warn("rootmap: classify failed", "error", err, "block_id", block.ID)
	}

	return Result{Written: true, BlockID: block.ID, Title: title,
		Length: len(rendered.Text), Text: rendered.Text}, nil
}

// validateConfig refuses a configuration that could only produce a wrong
// artefact — BEFORE the first query, so a misconfigured budget costs nothing.
func validateConfig(cfg Config) error {
	if cfg.BudgetBytes <= 0 {
		return fmt.Errorf("rootmap: root_map.budget_bytes must be > 0, got %d", cfg.BudgetBytes)
	}
	if cfg.BudgetBytes > storeWriteCap {
		return fmt.Errorf("rootmap: root_map.budget_bytes %d exceeds the %d B store write cap",
			cfg.BudgetBytes, storeWriteCap)
	}
	if cfg.FooterReserveBytes <= 0 {
		return fmt.Errorf("rootmap: root_map.footer_reserve_bytes must be > 0, got %d", cfg.FooterReserveBytes)
	}
	if cfg.SmallClusterMax < 0 {
		return fmt.Errorf("rootmap: root_map.small_cluster_max must be >= 0, got %d", cfg.SmallClusterMax)
	}
	return nil
}

// checkSize is BP-5 at the write boundary. Render already guarantees the budget;
// this is the second, independent check against the value that actually matters
// downstream — the cap the public store API enforces and the digest has bypassed
// since forever.
func checkSize(text string, cfg Config) error {
	limit := cfg.BudgetBytes
	if storeWriteCap < limit {
		limit = storeWriteCap
	}
	if len(text) > limit {
		return fmt.Errorf("rootmap: rendered %d B over the write limit %d B", len(text), limit)
	}
	return nil
}

// gather performs the four reads of §4.2 steps 1–4 and folds them into Input.
// Every read goes through the W-B store functions — RequireScopes first,
// `WHERE scope = ANY($1)` inside, no ad-hoc query against the cluster tables
// from this package (§5.1).
func gather(ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, cfg Config,
	homeScope string, readScopes []string) (Input, error) {
	// NodeLimit goes in RAW: the store binds its own +1 to detect truncation,
	// and a second +1 here would shift that detection by one row.
	nodeLimit := NodeLimitFor(cfg.BudgetBytes, cfg.FooterReserveBytes)
	ov, err := store.GraphOverview(ctx, pool, store.OverviewParams{
		MinClusterSize: 1,
		NodeLimit:      nodeLimit,
		EdgeLimit:      0, // the map renders no edges — and pays for none (W-B)
	}, readScopes)
	if err != nil {
		return Input{}, fmt.Errorf("rootmap: overview read: %w", err)
	}
	totals, err := store.OverviewTotals(ctx, pool, readScopes, cfg.SmallClusterMax)
	if err != nil {
		return Input{}, fmt.Errorf("rootmap: totals read: %w", err)
	}
	meta, err := store.OverviewMeta(ctx, pool, readScopes)
	if err != nil {
		return Input{}, fmt.Errorf("rootmap: meta read: %w", err)
	}

	opTypes, opRationale := OperationalTypes(set)
	active, activeKnown, err := store.ActiveBlockCount(ctx, pool, readScopes, cfg.CountTimeout)
	if err != nil {
		return Input{}, fmt.Errorf("rootmap: corpus count: %w", err)
	}
	opBlocks, opKnown, err := store.OperationalBlockCount(ctx, pool, readScopes, opTypes, cfg.CountTimeout)
	if err != nil {
		return Input{}, fmt.Errorf("rootmap: operational count: %w", err)
	}

	// W-F: only read the meta level when the flag asks for it. The rows outlive a
	// flag flip until the next rebuild clears them, and a section that keeps
	// appearing after it was switched off would make the knob a suggestion.
	var superRows []SuperRow
	if cfg.SuperEnabled {
		supers, err := store.OverviewSuper(ctx, pool, readScopes, nodeLimit)
		if err != nil {
			return Input{}, fmt.Errorf("rootmap: super read: %w", err)
		}
		superRows = superRowsFrom(supers)
	}

	return Input{
		Scope:     homeScope,
		Rows:      rowsFrom(ov.Nodes),
		SuperRows: superRows,
		Coverage: Coverage{
			ClusterTotal:      totals.ClusterTotal,
			ClusteredBlocks:   totals.ClusteredBlocks,
			SmallClusterN:     totals.SmallClusterN,
			SmallClusterSize:  totals.SmallClusterSize,
			CandidateBlocks:   meta.CandidateN,
			ActiveBlocks:      active,
			ActiveKnown:       activeKnown,
			OperationalTypes:  opTypes,
			OperationalBlocks: opBlocks,
			OperationalKnown:  opKnown && activeKnown,
			ExcludedTypes:     ExcludedTypes(set),
		},
		Freshness: Freshness{
			ComputedAt:    meta.ComputedAt,
			LastAttemptAt: meta.LastAttemptAt,
			SkipReason:    meta.SkipReason,
			Contended:     meta.Contended,
			ClusterN:      meta.ClusterN,
			CandidateN:    meta.CandidateN,
			Interval:      cfg.RebuildInterval,
			TenantCount:   tenantCount(ctx, pool),
			Modularity:    meta.Modularity,
			Resolution:    meta.Resolution,
			StaleScopes:   meta.StaleScopes,
			// The three-state meta level, gated by the same flag as the read: a
			// disabled section reports "never attempted" even if rows survive,
			// so the map can never claim a level it is not showing.
			SuperKnown:      cfg.SuperEnabled && meta.SuperKnown,
			SuperN:          meta.SuperN,
			SuperResolution: meta.SuperResolution,
		},
		BudgetBytes:          cfg.BudgetBytes,
		FooterReserveBytes:   cfg.FooterReserveBytes,
		SmallClusterMax:      cfg.SmallClusterMax,
		OperationalRationale: opRationale,
	}, nil
}

// rowsFrom projects the store nodes onto topic rows. StableID and Label stay
// empty until axis 01 puts the topic identity on the read path (W7): the
// renderer degrades to D2/D3 and the line keeps repr= as its resolvable handle,
// which is precisely why the format carries BOTH identifiers.
func rowsFrom(nodes []store.OverviewNode) []Row {
	rows := make([]Row, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, Row{
			StableID:    n.TopicID,
			Label:       n.Label,
			LabelSource: rendererLabelSource(n.LabelSource),
			Size:        n.Size,
			TopCats:     n.TopCategories,
			CatCounts:   n.TopCatCounts,
			ReprID:      n.ReprID,
			ReprTitle:   n.ReprTitle,
			ScopeMix:    n.ScopeMix,
		})
	}
	return rows
}

// rendererLabelSource maps the DB provenance vocabulary (migration 125:
// none | fallback | llm | manual) onto the renderer's (W-C: "" | heuristic |
// llm | …). 'fallback' IS the deterministic heuristic; 'none' means the line
// will fall back to repr_title, which labelSources already declares on its
// own. An unknown future value passes through verbatim — the head line names
// it rather than hiding it.
func rendererLabelSource(dbSource string) string {
	switch dbSource {
	case "none", "":
		return ""
	case "fallback":
		return "heuristic"
	default:
		return dbSource
	}
}

// superRowsFrom projects the store's meta rows onto renderer rows. Straight
// through: the read already sorted by size and resolved the lead topic's name,
// and the renderer's job is measuring, not deciding.
func superRowsFrom(supers []store.SuperTopic) []SuperRow {
	rows := make([]SuperRow, 0, len(supers))
	for _, s := range supers {
		rows = append(rows, SuperRow{
			ID: s.SuperID, Label: s.Label, Title: s.Title,
			Size: s.Size, TopicN: s.TopicN,
		})
	}
	return rows
}

// tenantCount is the cadence multiplier of the head line: the rebuild loop
// serves ONE tenant per tick, so the effective period per tenant is the
// interval times the number of tenants.
//
// Read from the tenant register rather than passed in by the caller, and that
// is deliberate: BOTH triggers (the scheduler tail call and POST /api/digest)
// must compute the SAME text, or content idempotency collapses into two
// producers flipping one block back and forth. The register can over-count
// slightly (a tenant owning no scope is counted but never rebuilds), which
// makes the printed cadence LONGER than reality — the safe direction: the map
// never promises more freshness than it delivers. Unreadable register ⇒ 1, the
// same never-empty floor the scheduler's own tenant list uses.
func tenantCount(ctx context.Context, pool *pgxpool.Pool) int {
	tenants, err := store.ListTenants(ctx, pool)
	if err != nil {
		return 1
	}
	n := 0
	for _, t := range tenants {
		if t.Status == "active" {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

// OperationalTypes returns the operational types PRESENT in this registry plus
// the joined rationale for them (E11-02 / A02-2). Sorted — the map's text is
// compared byte for byte against the stored one, so every list it prints has to
// have a stable order.
func OperationalTypes(set *blocktype.Set) ([]string, string) {
	if set == nil {
		return nil, ""
	}
	var names, why []string
	for _, name := range outsideCut(set) {
		if r, ok := operationalRationales[name]; ok {
			names = append(names, name)
			why = append(why, r)
		}
	}
	return names, strings.Join(why, "; ")
}

// ExcludedTypes returns the types outside the Louvain node cut that are NOT
// operational — the policy statement of §4.4a. Names only, never counts: a list
// of type names is scale-free, a count per type is another corpus scan.
func ExcludedTypes(set *blocktype.Set) []string {
	if set == nil {
		return nil
	}
	var out []string
	for _, name := range outsideCut(set) {
		if _, ok := operationalRationales[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// outsideCut is the complement of the Louvain node cut: the cut is
// VisibleTypes ∩ OverviewTypes (retrieval-visible AND overview-included), so a
// type missing either property is outside it. system-meta is the instructive
// case — overview.include is true, but retrieval=excluded keeps it out, which is
// why the map names it instead of silently widening its own cut.
func outsideCut(set *blocktype.Set) []string {
	inCut := map[string]bool{}
	visible := map[string]bool{}
	for _, n := range set.VisibleTypes() {
		visible[n] = true
	}
	for _, n := range set.OverviewTypes() {
		if visible[n] {
			inCut[n] = true
		}
	}
	var out []string
	for _, n := range set.Names() {
		if !inCut[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// metadataFor is the §4.3 metadata contract: the machine-readable twin of the
// coverage section, under the same bucket invariants the footer prints.
//
// is_meta is the one key that MUST be here — it is the classify input for
// system-meta, and without it the map becomes a retrieval candidate.
// generated_at lives here and nowhere else: in the text it would make every
// cycle a write.
func metadataFor(in Input, r Rendered, cfg Config, homeScope string) map[string]any {
	c := r.Coverage
	md := map[string]any{
		"source":               "ctx-rootmap",
		"is_meta":              true,
		"format_version":       FormatVersion,
		"generated_at":         time.Now().UTC().Format(time.RFC3339),
		"scope":                homeScope,
		"cluster_count":        c.ClusterTotal,
		"rendered_rows":        c.RenderedRows,
		"rendered_blocks":      c.RenderedBlocks,
		"small_cluster_n":      c.SmallClusterN,
		"small_cluster_blocks": c.SmallClusterSize,
		"capped_cluster_n":     c.CappedClusterN,
		"capped_blocks":        c.CappedBlocks,
		"clustered_blocks":     c.ClusteredBlocks,
		"candidate_blocks":     c.CandidateBlocks,
		"active_blocks":        c.ActiveBlocks,
		"active_known":         c.ActiveKnown,
		"operational_blocks":   c.OperationalBlocks,
		"operational_known":    c.OperationalKnown,
		"modularity":           in.Freshness.Modularity,
		"resolution":           in.Freshness.Resolution,
		"budget_bytes":         cfg.BudgetBytes,
	}
	if in.Freshness.ComputedAt != nil {
		md["cluster_computed_at"] = in.Freshness.ComputedAt.UTC().Format(time.RFC3339)
	}
	if in.Freshness.LastAttemptAt != nil {
		md["cluster_last_attempt_at"] = in.Freshness.LastAttemptAt.UTC().Format(time.RFC3339)
	}
	if in.Freshness.SkipReason != "" {
		md["skip_reason"] = in.Freshness.SkipReason
	}
	// W-F: only when the level was actually attempted. An unconditional
	// "super_n": 0 would read as "built, zero groups" and is the same ambiguity
	// the three-state encoding exists to remove.
	if in.Freshness.SuperKnown {
		md["super_n"] = in.Freshness.SuperN
		md["super_resolution"] = in.Freshness.SuperResolution
		md["super_rows"] = r.SuperRows
	}
	return md
}
