// Package rootmap renders the root map: the cluster-per-line replacement for
// the block-per-line topic map (Cluster-Topic-Map, design/02 §4.1–§4.5, wave
// W-C).
//
// PURE. No database, no clock, no configuration lookup — Render is a function
// from numbers to text, the same discipline as handler.buildOverviewResponse
// ("Pure — pinned by the envelope golden test"). Everything it needs about the
// world arrives in Input; everything it decides comes back in Rendered. That is
// what makes the golden test byte-exact and the budget guarantee provable.
//
// Two properties carry the whole package:
//
//   - The output NEVER exceeds Input.BudgetBytes. Not by estimate — by
//     measuring what has been written and stopping while the reserved footer
//     still fits. A renderer that guesses can outgrow the 50 KB write cap of
//     the public store API and would only be caught by the block that no longer
//     writes (BP-5).
//   - The map never lies about what it does not show. Every cluster is in
//     exactly one of three buckets — rendered, too small, cut by the budget —
//     and Render asserts the two sums instead of trusting them (§4.5 step 5).
//     At 10^5 clusters against ~95 lines, "small" and "cut" overlap by default;
//     without the allocation rule the map would count tens of thousands of
//     clusters twice.
//
// The third property is a decision, not a mechanism: checkpoints are NOT a
// coverage gap. They are operational compaction artefacts, deliberately outside
// the topic map, and the coverage figure is therefore computed against the
// KNOWLEDGE corpus with the raw corpus number printed beside it (E11-02 /
// amendment A02-2). Rendering them as 83 % missing coverage is what made
// earlier sessions re-escalate a settled decision.
package rootmap

import (
	"fmt"
	"time"
)

// FormatVersion is the version this package writes into the head line and the
// metadata contract. A format change bumps it — consumers pin on it.
const FormatVersion = 1

// Row is one topic line: a cluster the map shows individually.
type Row struct {
	StableID    string   // rebuild-spanning identity (axis 01); "" ⇒ D3
	Label       string   // speaking topic label (axis 01); "" ⇒ D2 (ReprTitle)
	LabelSource string   // "llm" | "heuristic" | "" (no label)
	Size        int      // Σ visible size
	TopCats     []string // top-3 categories
	CatCounts   []int    // parallel to TopCats; short/absent ⇒ names only
	ReprID      string   // representative block — the always-resolvable handle
	ReprTitle   string
	ScopeMix    []string // contributing scopes; printed only if != [Scope]
}

// Coverage carries every number the map says about its own incompleteness.
//
// ClusterTotal/ClusteredBlocks describe ALL clusters of the caller's window
// (store.OverviewTotals), not just the fetched ones — that is what makes the
// cut-by-budget bucket computable at all. SmallClusterN/Size come from the same
// read. Rendered and capped are derived by Render and returned in
// Rendered.Coverage; setting them on the way in has no effect.
type Coverage struct {
	ClusterTotal     int // all clusters with a visible member
	ClusteredBlocks  int // Σ their visible size
	SmallClusterN    int // clusters with size <= Input.SmallClusterMax
	SmallClusterSize int // Σ blocks in those

	CandidateBlocks int // Σ graph_overview_meta.candidate_n (rebuild lag arithmetic)

	ActiveBlocks int  // scope-filtered active blocks, RAW (all types)
	ActiveKnown  bool // false ⇒ the capped count timed out: no denominator, NO estimate

	// Operational types are outside the map BY DECISION, not by omission
	// (E11-02 / A02-2). They are named on their own line and subtracted from
	// the denominator so the coverage figure describes the knowledge corpus.
	OperationalTypes  []string
	OperationalBlocks int
	OperationalKnown  bool

	// ExcludedTypes are the types outside the Louvain node cut per type policy
	// (overview.include = false or retrieval = excluded), minus the
	// operational ones — a policy statement, no counting, hence scale-free.
	ExcludedTypes []string

	// Derived by Render; ignored on input.
	RenderedRows   int
	RenderedBlocks int
	CappedClusterN int
	CappedBlocks   int
}

// SuperRow is one line of the META level (W-F): a group of topics the coarse
// second Louvain put together. It is what keeps the map ABOUT something once the
// flat cluster list stops fitting — the collector line says how much is missing,
// this section says what it is.
//
// ID is the group id of THIS map generation, not a stable identity: the level is
// rebuilt whole with every partition. The durable handle is the lead topic's,
// and Label is that topic's name — which is why a group is never anonymous while
// its lead has a name.
type SuperRow struct {
	ID     string
	Label  string // lead topic's label; "" ⇒ Title, "" ⇒ placeholder
	Title  string // lead topic's repr_title
	Size   int    // Σ blocks over the child topics
	TopicN int    // child topics
}

// Freshness mirrors graph_overview_meta 1:1 — including its caps.
type Freshness struct {
	ComputedAt    *time.Time // nil = never built successfully
	LastAttemptAt *time.Time // any of the five rebuild exits
	SkipReason    string     // "" | node-cap | timeout | error | disabled | registry-unwired
	Contended     bool       // advisory-lock seen: contention, NEVER a cap line
	ClusterN      int        // graph_overview_meta.cluster_n — tells "built, empty" from "never built"
	CandidateN    int        // per-scope candidates of the last attempt
	Interval      time.Duration
	TenantCount   int // effective cadence is Interval × TenantCount (round-robin)
	Modularity    float64
	Resolution    float64

	// StaleScopes are scopes whose meta row outlived its partition: the W-A
	// teardown only deletes rows for scopes that still have cluster rows, so a
	// scope that vanished from the corpus keeps a SUCCESS row forever. It is
	// neither fresh coverage nor a gap — the map names it and moves on, and
	// AllowsUpsert refuses to overwrite a good map on such a window.
	StaleScopes []string

	// SuperKnown/SuperN/SuperResolution are the W-F meta level, three-state on
	// purpose: SuperKnown false = never attempted (the section is simply absent,
	// which is the shipped default); SuperKnown true with SuperN 0 = attempted
	// and degraded to flat at root_map.super_max_nodes, which the map SAYS in one
	// line rather than passing off as "no groups". SuperResolution is the γ the
	// budget search settled on — printed because it is the one number that
	// explains why the groups are as coarse as they are.
	SuperKnown      bool
	SuperN          int
	SuperResolution float64
}

// Input is everything Render needs. Rows arrive sorted by Size DESC (the store
// already does that); Render keeps the order and does not re-sort.
type Input struct {
	Scope                string
	Rows                 []Row
	SuperRows            []SuperRow // W-F meta level, biggest group first; empty ⇒ no section
	Coverage             Coverage
	Freshness            Freshness
	BudgetBytes          int    // root_map.budget_bytes
	FooterReserveBytes   int    // root_map.footer_reserve_bytes
	SmallClusterMax      int    // root_map.small_cluster_max
	FormatVersion        int    // 0 ⇒ FormatVersion
	OperationalRationale string // one clause explaining WHY those types are operational
}

// Rendered is the map plus the numbers it printed. The numbers come back
// because the metadata contract of §4.3 needs the six set fields and
// recomputing them in the caller would duplicate the allocation rule — the very
// thing the invariant exists to protect.
type Rendered struct {
	Text     string
	Coverage Coverage
	Empty    bool // the window shows NO clusters at all; consult AllowsUpsert
	// SuperRows is how many meta-cluster lines the section actually printed —
	// not how many exist. The difference is the section's own cut, and the
	// metadata contract carries the printed number for the same reason the
	// footer prints the cut one: both halves of an accounting are the accounting.
	SuperRows int
}

// Render builds the map text. Deterministic, allocation-light, and guaranteed
// len(Text) <= BudgetBytes — by measuring, not by estimating.
//
// It returns an error instead of a degraded string whenever the result would be
// untrue: footer over its reserve, budget too small for the skeleton, or a
// bucket arithmetic that does not add up. Every degradation that still yields a
// TRUE map (D0–D5) produces text and no error.
func Render(in Input) (Rendered, error) {
	in = withDefaults(in)
	if err := validate(in); err != nil {
		return Rendered{}, err
	}

	// The head line names the rendered mass, which is only known AFTER the
	// measuring loop — and the loop needs the head length. The circle is broken
	// with an UPPER BOUND instead of a second pass: rendering the head with the
	// totals in place of the rendered counts can only produce equal or more
	// digits, so the measured prefix is never shorter than the real one. Rows
	// are therefore fitted conservatively (a few bytes may stay unused), and the
	// final length check below still proves the budget.
	bound := in.Coverage
	bound.RenderedRows, bound.RenderedBlocks = bound.ClusterTotal, bound.ClusteredBlocks

	// W-F: the meta section is measured ONCE, against the upper-bound head, and
	// then reused verbatim in both prefix renderings. Measuring it twice against
	// two different head lengths could produce two different section lengths, and
	// the row loop would then fit rows against a prefix the final render does not
	// have — the budget proof below would still hold, but the map would silently
	// lose a line. The section is capped at HALF the space the topic lines would
	// otherwise get: the coarse level must never be able to push the fine one out
	// entirely, and half is the same relational split the identity axis uses for
	// the substance core rather than another tuning knob.
	super, superShown := renderSuper(in, len(renderPrefix(in, bound, "")))

	if n := len(renderPrefix(in, bound, super)) + in.FooterReserveBytes; n > in.BudgetBytes {
		return Rendered{}, fmt.Errorf("rootmap: budget %d B too small: head+coverage plus footer reserve need %d B",
			in.BudgetBytes, n)
	}

	cov := in.Coverage
	body, rendered, blocks := renderRows(in, len(renderPrefix(in, bound, super)))
	cov.RenderedRows, cov.RenderedBlocks = rendered, blocks
	cov.CappedClusterN = cov.ClusterTotal - cov.RenderedRows - cov.SmallClusterN
	cov.CappedBlocks = cov.ClusteredBlocks - cov.RenderedBlocks - cov.SmallClusterSize
	if err := assertBuckets(cov); err != nil {
		return Rendered{}, err
	}

	// Nothing to show at all (D4 and its neighbours). Deliberately NOT
	// "len(Rows) == 0 && ClusterN == 0": a render that lands inside a global
	// rebuild's TRUNCATE window sees zero clusters while the meta row still
	// reports the OLD cluster_n > 0 — the exact case BP-6 exists for. Keying on
	// "the window shows no clusters" catches both; the wording below tells them
	// apart, and AllowsUpsert refuses the write where it matters.
	if rendered == 0 && cov.ClusterTotal == 0 {
		out := renderPrefix(in, cov, super) + emptyStatement(in)
		if len(out) > in.BudgetBytes {
			return Rendered{}, fmt.Errorf("rootmap: empty map %d B over budget %d B", len(out), in.BudgetBytes)
		}
		return Rendered{Text: out, Coverage: cov, Empty: true, SuperRows: superShown}, nil
	}

	footer := renderFooter(in, cov)
	if len(footer) > in.FooterReserveBytes {
		return Rendered{}, fmt.Errorf("rootmap: footer %d B over reserve %d B", len(footer), in.FooterReserveBytes)
	}

	out := renderPrefix(in, cov, super) + body + footer
	if len(out) > in.BudgetBytes {
		return Rendered{}, fmt.Errorf("rootmap: rendered %d B over budget %d B", len(out), in.BudgetBytes)
	}
	// Empty is FALSE here even at zero topic lines: a corpus made only of
	// two-block clusters produces a map that is entirely collector line — and
	// that is a true, useful map, not the empty one BP-6 guards against.
	return Rendered{Text: out, Coverage: cov, SuperRows: superShown}, nil
}

// AllowsUpsert is the BP-6 rule as a pure predicate: an empty or stale map must
// NEVER replace a good one.
//
// A global rebuild TRUNCATEs the three cluster tables inside its transaction; a
// render that lands in that window sees zero clusters. Writing that result
// replaces a working map with an empty one. The same holds for a window whose
// only meta evidence is a stale success row — a partition that no longer
// exists must not become an empty map either.
//
// Only a genuinely fresh tenant (no clusters, never built, nothing stale) may
// write the "no clusters yet" map — the second half is load-bearing, because a
// pure W-A attempt stamp carries cluster_n = 0 as well and would otherwise slip
// through exactly when the rule has to hold.
func AllowsUpsert(in Input, r Rendered) bool {
	if !r.Empty {
		return true
	}
	f := in.Freshness
	return f.ClusterN == 0 && f.ComputedAt == nil && len(f.StaleScopes) == 0
}

// NodeLimitFor is the pessimistic row budget the caller passes to
// store.GraphOverview: how many clusters could POSSIBLY fit, assuming the
// smallest conceivable line (§4.5). Pessimistic on purpose — it fetches rather
// too many clusters than too few, and the measuring loop then cuts cleanly. The
// optimistic choice would leave budget unused. The store keeps its own "+1"
// truncation detection, so this value goes in raw.
func NodeLimitFor(budgetBytes, footerReserveBytes int) int {
	const (
		headerReserve = 1000 // head + cadence + coverage section (§4.3)
		minRowBytes   = 81   // 36 id + 41 repr= + separators, no label, no categories
	)
	usable := budgetBytes - headerReserve - footerReserveBytes
	if usable < minRowBytes {
		return 1
	}
	return (usable + minRowBytes - 1) / minRowBytes
}

func withDefaults(in Input) Input {
	if in.FormatVersion == 0 {
		in.FormatVersion = FormatVersion
	}
	return in
}

func validate(in Input) error {
	if in.BudgetBytes <= 0 {
		return fmt.Errorf("rootmap: budget_bytes must be > 0, got %d", in.BudgetBytes)
	}
	if in.FooterReserveBytes <= 0 {
		return fmt.Errorf("rootmap: footer_reserve_bytes must be > 0, got %d", in.FooterReserveBytes)
	}
	if in.Scope == "" {
		return fmt.Errorf("rootmap: scope must not be empty")
	}
	return nil
}

// assertBuckets is the §4.5 step 5 invariant. It is an assertion, not a
// comment: a negative bucket means the inputs disagree (rows from outside the
// counted window, a small cluster rendered as a topic, mismatched scopes) and
// the map would print numbers that do not add up. Silence there is worse than
// no map at all.
func assertBuckets(c Coverage) error {
	if c.CappedClusterN < 0 || c.CappedBlocks < 0 {
		return fmt.Errorf("rootmap: bucket arithmetic broken: rendered %d/%d + small %d/%d exceed total %d/%d",
			c.RenderedRows, c.RenderedBlocks, c.SmallClusterN, c.SmallClusterSize, c.ClusterTotal, c.ClusteredBlocks)
	}
	if c.RenderedRows+c.SmallClusterN+c.CappedClusterN != c.ClusterTotal {
		return fmt.Errorf("rootmap: cluster buckets %d+%d+%d != total %d",
			c.RenderedRows, c.SmallClusterN, c.CappedClusterN, c.ClusterTotal)
	}
	if c.RenderedBlocks+c.SmallClusterSize+c.CappedBlocks != c.ClusteredBlocks {
		return fmt.Errorf("rootmap: block buckets %d+%d+%d != total %d",
			c.RenderedBlocks, c.SmallClusterSize, c.CappedBlocks, c.ClusteredBlocks)
	}
	return nil
}
