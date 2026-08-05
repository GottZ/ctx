// Budget structure + typed traversal taxonomy for BOTH graph engines
// (design/05 §4.5). Concept model pgGraph (00c: depth/frontier/visited limits,
// OOM guards, typed codes); structural pattern in-house: backends.ErrClass
// (backends/classify.go:12-31) — a catalog plus decision methods, deliberately
// NOT a breaker state machine.
//
// The one thing this file adds to the system is a DIFFERENTIATION that did not
// exist before: the ego envelope carried exactly ONE `Truncated bool`
// (store/graph.go) into which the node budget and both edge truncations
// collapsed, and the expand path capped silently (MaxInjected, SeedScoreFloor)
// with no signal at all. TravClass names the CAUSE, and the two layers name
// WHOSE limit was reached:
//
//   - Layer LIMITS = API contract: what the CLIENT asked for is exhausted
//     (p.Limit, p.EdgeLimit). Wire-visible: the caller can act on it (ask for
//     more, page, narrow the query).
//   - Layer BUDGETS = server protection: what the SERVER is willing to pay
//     (OOM/time guards). Wire-visible only for post-recheck-derived trips.
//   - Layer OPERATIONAL = which arm answered / what failed (cache stale,
//     recheck error). Carried in Counts + Source, never as a layer array.
//
// The ceilings (handler/graph.go) stay exactly where they are: they are the API
// contract, enforced as 400 before any traversal. This budget structure lies
// UNDER them, not beside them (§4.5).
//
// W05.4 scope note: this file DECLARES the vocabulary and wires the three
// truncation sources the SQL path already detects. MaxFrontier/MaxVisited/
// MaxCandidates/SoftDeadline are inactive structure until the cache arms
// (W05.5+) enforce them — no behavior in any path changes with this wave.

package graphcache

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Budget is the per-request traversal budget shared by rrf.GraphExpand and
// store.EgoGraph (§4.5). Every field is a SERVER-side guard; the client-facing
// ceilings (hops/limit/edge_limit) live in the handler and are enforced there
// as a 400.
//
// None of these fields is enforced yet (W05.4 declares, W05.5+ enforces) —
// which is why they carry no defaults here: the config keys land with the wave
// that makes them bite (§4.7).
type Budget struct {
	// MaxDepth caps hops. Ego today: 1..3 (handler ceiling); Expand: cfg.HopDepth.
	MaxDepth int
	// MaxFrontier caps the nodes carried into one hop's frontier (OOM guard;
	// implicit today).
	MaxFrontier int
	// MaxVisited caps the total visited set (OOM guard; unbounded today apart
	// from p.Limit).
	MaxVisited int
	// MaxCandidates caps candidates per hop BEFORE the visibility recheck
	// (hub-flood guard). Its trips are ORACLE-BEARING — see TravCandidatesCapped.
	MaxCandidates int
	// SoftDeadline is the traversal time budget underneath the request context.
	SoftDeadline time.Duration
}

// TravClass is one typed traversal outcome. Catalog order is the §4.5 order and
// is part of the contract only through String() — the numeric values are
// internal, the snake_case tokens are the stable wire/log identity (they become
// correlation input for the Achse-01 measurement track).
type TravClass int

// Traversal classes, catalog order (§4.5).
const (
	TravOK TravClass = iota

	// ── Layer LIMITS ── API contract: the client's own p.Limit/p.EdgeLimit is
	// exhausted. These carry the existing Truncated bool, differentiated by cause.

	// TravNodeLimitReached: p.Limit filled with post-recheck nodes while an
	// unexpanded frontier remained.
	TravNodeLimitReached
	// TravEdgeLimitReached: p.EdgeLimit exhausted — the induced-edge overflow
	// probes (Q2/Q2s) or the cross-class arbitration cut.
	TravEdgeLimitReached

	// ── Layer BUDGETS ── server protection: OOM/time guards.

	// TravDepthCapped: MaxDepth reached — declared, not an error.
	TravDepthCapped
	// TravFrontierCapped: frontier capped (partial result, post-recheck size).
	TravFrontierCapped
	// TravVisitedCapped: visited limit reached (partial result, post-recheck size).
	TravVisitedCapped
	// TravCandidatesCapped: candidate cap hit BEFORE the recheck — SERVER
	// TELEMETRY ONLY, NEVER wire (see the oracle barrier below).
	TravCandidatesCapped
	// TravInjectCapped: Expand reached MaxInjected (silent until W05.4).
	TravInjectCapped
	// TravSeedFloorCapped: Expand broke on SeedScoreFloor (silent until W05.4).
	TravSeedFloorCapped
	// TravTimeCapped: SoftDeadline exceeded (partial result).
	TravTimeCapped
	// TravClusterAnnotateProbe: the ego cluster annotation ran its membership
	// probe (Cluster-Topic-Map C2, decision UD-03-03). NOT a cap — a declared
	// COST POSTEN. It sits in the budget layer because it is the one piece of
	// server-side work the annotation adds under its own ceiling, and because it
	// pairs with TravClusterAnnotateCapped: "probe ran" and "probe skipped by the
	// ceiling" are the two outcomes of one knob and belong in one place.
	//
	// Its whole reason to exist is the CACHE arm: there the snapshot answers
	// without any SQL hop, so this probe is the only remaining roundtrip and
	// would otherwise hide inside the cache win (design/03 §4.2). Source tells a
	// reader which arm it was measured on.
	TravClusterAnnotateProbe
	// TravClusterAnnotateCapped: the delivered node count exceeded
	// cluster.ego_annotate_max_nodes ⇒ empty clusters[]/cluster_of[], no probe.
	// The ROUTE ceiling is untouched (design/03 §6.4): the annotation declines
	// rather than shrinking what the graph read itself may return.
	TravClusterAnnotateCapped
	// TravClusterBoosted: one result was reinforced by the categorical cluster
	// stage (Cluster-Topic-Map C3). Its COUNT is how many results the stage
	// touched — the one number that says whether a query got a categorical signal
	// at all. On the query path the report is server telemetry only (§4.5
	// behaviour matrix), so this never reaches a client.
	TravClusterBoosted

	// ── Layer OPERATIONAL ── which arm answered, and what failed.

	// TravClusterStale: the cluster stage turned itself into a no-op because the
	// landkarte it would boost from is not demonstrably fresh (Cluster-Topic-Map
	// C4, design/03 §4.7). Operational like TravCacheStale — it names WHY a stage
	// did not run, not a limit it hit.
	//
	// It fires on all three uncertainty branches (no meta row for one of the read
	// scopes, computed_at older than cluster.max_staleness, freshness seam not
	// wired), because they are one statement: no signal beats a signal from a
	// frozen map. That also means the token says "signal off", NOT "rebuild
	// broken" — the /api/status cluster_map section is what tells those apart.
	TravClusterStale
	// TravCacheStale: snapshot missing/too old/seed unknown → SQL fallback ran.
	TravCacheStale
	// TravRecheckError: the DB recheck failed — the ONLY error-valued class.
	// Fail-open on the query path (original slice + warn), fail-loud on the UI
	// path (500), exactly as today.
	TravRecheckError
)

// Report sources (§4.5 BudgetReport.Source): which arm produced the answer.
const (
	SourceSQL   = "sql"
	SourceCache = "cache"
)

// TravLayer is the schicht a class belongs to. The layer decides which report
// array a trip lands in — and, together with PreRecheck, whether it may cross
// the wire at all.
type TravLayer int

// Layers (§4.5).
const (
	// LayerNone is TravOK's layer — never recorded.
	LayerNone TravLayer = iota
	// LayerLimits is the API-contract layer (client's own ceilings).
	LayerLimits
	// LayerBudgets is the server-protection layer (OOM/time guards).
	LayerBudgets
	// LayerOperational is the arm/failure layer — recorded in Counts, never as
	// a layer array (Source carries the arm on the wire).
	LayerOperational
)

// String renders the layer for logs.
func (l TravLayer) String() string {
	switch l {
	case LayerNone:
		return "none"
	case LayerLimits:
		return "limits"
	case LayerBudgets:
		return "budgets"
	case LayerOperational:
		return "operational"
	default:
		return "unknown"
	}
}

// String renders the stable snake_case token used on the wire, in logs and in
// downstream correlation. These tokens are the contract — never rename one
// without treating it as a wire change.
func (c TravClass) String() string {
	switch c {
	case TravOK:
		return "ok"
	case TravNodeLimitReached:
		return "node_limit_reached"
	case TravEdgeLimitReached:
		return "edge_limit_reached"
	case TravDepthCapped:
		return "depth_capped"
	case TravFrontierCapped:
		return "frontier_capped"
	case TravVisitedCapped:
		return "visited_capped"
	case TravCandidatesCapped:
		return "candidates_capped"
	case TravInjectCapped:
		return "inject_capped"
	case TravSeedFloorCapped:
		return "seed_floor_capped"
	case TravTimeCapped:
		return "time_capped"
	case TravClusterAnnotateProbe:
		return "cluster_annotate_probe"
	case TravClusterAnnotateCapped:
		return "cluster_annotate_capped"
	case TravClusterBoosted:
		return "cluster_boosted"
	case TravClusterStale:
		return "cluster_stale"
	case TravCacheStale:
		return "cache_stale"
	case TravRecheckError:
		return "recheck_error"
	default:
		return "unknown"
	}
}

// MarshalText renders the class as its snake_case token. It serves BOTH JSON
// values and JSON map KEYS (encoding/json requires encoding.TextMarshaler for
// non-string map keys) — one implementation, so a Counts key and a Limits entry
// can never drift apart.
func (c TravClass) MarshalText() ([]byte, error) {
	return []byte(c.String()), nil
}

// Layer returns the schicht of the class (§4.5).
func (c TravClass) Layer() TravLayer {
	switch c {
	case TravNodeLimitReached, TravEdgeLimitReached:
		return LayerLimits
	case TravDepthCapped, TravFrontierCapped, TravVisitedCapped,
		TravCandidatesCapped, TravInjectCapped, TravSeedFloorCapped, TravTimeCapped,
		TravClusterAnnotateProbe, TravClusterAnnotateCapped, TravClusterBoosted:
		return LayerBudgets
	case TravClusterStale, TravCacheStale, TravRecheckError:
		return LayerOperational
	case TravOK:
		return LayerNone
	default:
		return LayerNone
	}
}

// PreRecheck reports whether the class fires on a set that has NOT yet passed
// the DB visibility recheck. Such trips are existence/quantity oracles: on a
// shared block a candidate-cap trip would reveal that >= MaxCandidates RAW
// edges exist — including foreign PRIVATE ones — even when the visible result
// is tiny. That is exactly the oracle class store/graph.go:823-825 excludes.
//
// This predicate IS the oracle barrier of §4.5: WireReport drops every
// pre-recheck class structurally, so no caller can leak one by forgetting a
// convention. A new pre-recheck class only has to answer true here.
func (c TravClass) PreRecheck() bool {
	return c == TravCandidatesCapped
}

// BudgetReport collects the trips of one traversal (§4.5). It is the SERVER-side
// full picture — including pre-recheck classes.
//
// It has exactly one route to the wire: WireReport (and MarshalJSON, which is
// defined as WireReport's serialisation, so even an accidental json.Marshal of
// the full report cannot leak a pre-recheck class).
type BudgetReport struct {
	// Limits are the LayerLimits trips, in trip order, deduplicated.
	Limits []TravClass
	// Budgets are the LayerBudgets trips, in trip order, deduplicated.
	Budgets []TravClass
	// Counts is the per-class trip count (how OFTEN a class fired), never a
	// count of dropped candidates — §5.1 Nr. 3(b).
	Counts map[TravClass]int
	// Source is which arm answered: SourceSQL | SourceCache.
	Source string
	// CacheAge is the age of the snapshot that answered (0 on the SQL arm).
	CacheAge time.Duration
}

// NewBudgetReport returns an empty report for the given arm.
func NewBudgetReport(source string) *BudgetReport {
	return &BudgetReport{Source: source, Counts: map[TravClass]int{}}
}

// Add records one trip of the class: it increments the count and appends the
// class to its layer array on first occurrence. Nil-safe by design — a caller
// that has no report (a pure-function unit test, a path that does not collect
// telemetry) passes nil and every Add is a no-op, so no call site needs a
// guard.
func (r *BudgetReport) Add(c TravClass) {
	if r == nil || c == TravOK {
		return
	}
	if r.Counts == nil {
		r.Counts = map[TravClass]int{}
	}
	first := r.Counts[c] == 0
	r.Counts[c]++
	if !first {
		return
	}
	switch c.Layer() {
	case LayerLimits:
		r.Limits = append(r.Limits, c)
	case LayerBudgets:
		r.Budgets = append(r.Budgets, c)
	case LayerOperational, LayerNone:
		// Operational classes stay in Counts only: the §4.5 report struct has
		// exactly two layer arrays, and the arm itself is already carried by
		// Source ("cache_stale" ⇒ Source=="sql"). A third array would be a
		// wire-shape change this wave does not own.
	}
}

// Tripped reports whether anything at all was recorded.
func (r *BudgetReport) Tripped() bool {
	return r != nil && len(r.Counts) > 0
}

// Count returns the recorded trip count of one class.
func (r *BudgetReport) Count(c TravClass) int {
	if r == nil {
		return 0
	}
	return r.Counts[c]
}

// WireBudgetReport is the wire projection of a BudgetReport — the ONLY shape
// that reaches a client. Arrays and the map are always non-nil so the JSON
// never carries `null` where a client expects a collection (the GA8 additive-
// array discipline of the ego envelope).
type WireBudgetReport struct {
	Limits     []TravClass       `json:"limits"`
	Budgets    []TravClass       `json:"budgets"`
	Counts     map[TravClass]int `json:"counts"`
	Source     string            `json:"source"`
	CacheAgeMs int64             `json:"cache_age_ms"`
}

// WireReport projects the report onto its wire-safe subset: every pre-recheck
// class is dropped from BOTH layer arrays and from Counts (a surviving count
// would be the same oracle by another name). This is the structural form of the
// §4.5 oracle barrier — the filter lives in the projection, not in the
// discipline of the callers.
func (r BudgetReport) WireReport() WireBudgetReport {
	out := WireBudgetReport{
		Limits:     filterWireClasses(r.Limits),
		Budgets:    filterWireClasses(r.Budgets),
		Counts:     map[TravClass]int{},
		Source:     r.Source,
		CacheAgeMs: r.CacheAge.Milliseconds(),
	}
	for c, n := range r.Counts {
		if c.PreRecheck() || c.Layer() == LayerOperational {
			continue
		}
		out.Counts[c] = n
	}
	return out
}

// filterWireClasses drops pre-recheck classes and never returns nil.
func filterWireClasses(in []TravClass) []TravClass {
	out := make([]TravClass, 0, len(in))
	for _, c := range in {
		if c.PreRecheck() {
			continue
		}
		out = append(out, c)
	}
	return out
}

// MarshalJSON serialises the report AS ITS WIRE PROJECTION. Belt to WireReport's
// braces: a future envelope that embeds a *BudgetReport directly still cannot
// emit a pre-recheck class. Server telemetry uses LogValue (or the fields), not
// JSON.
func (r BudgetReport) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(r.WireReport())
	if err != nil {
		return nil, fmt.Errorf("graphcache: marshal budget report: %w", err)
	}
	return b, nil
}

// LogValue renders the FULL report — pre-recheck classes included — for slog.
// Logs are server telemetry and are exactly the place where the candidate cap
// must stay visible (§4.5: "nur in Server-Telemetrie/Logs").
func (r BudgetReport) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 4+len(r.Counts))
	attrs = append(attrs,
		slog.String("source", r.Source),
		slog.String("limits", joinClasses(r.Limits)),
		slog.String("budgets", joinClasses(r.Budgets)),
	)
	if r.CacheAge > 0 {
		attrs = append(attrs, slog.Duration("cache_age", r.CacheAge))
	}
	for c, n := range r.Counts {
		attrs = append(attrs, slog.Int("n_"+c.String(), n))
	}
	return slog.GroupValue(attrs...)
}

// joinClasses renders a class list as a comma-separated token string.
func joinClasses(in []TravClass) string {
	out := ""
	for i, c := range in {
		if i > 0 {
			out += ","
		}
		out += c.String()
	}
	return out
}
