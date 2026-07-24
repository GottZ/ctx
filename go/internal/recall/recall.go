// recall.go — run orchestration (design/01 §4.2.5): plan → sampling → probes
// per stratum × k → aggregation → persist. Stratum order small → medium →
// large → all (cheap first: if the budget rips, the expensive strata are
// missing, never the early-warning ones). W01-2 delivers the core mechanics
// with injectable budget/defer hooks; the scheduler wiring (cadence, off-peak
// anchor, mid-run demand park, touch budget, rotation) lands in W01-3.
//
// Source: https://github.com/GottZ/ctx
package recall

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
)

// Config carries the recall_check core policy values (design/01 §3.2). It is
// the parameter MIRROR of config.RecallCheckConfig's core keys — internal/
// recall must not import internal/config (F1 layering rule: config is only
// for cmd/handler/events/settings; values are passed as parameters). The
// W01-3 scheduler arm (internal/events, allowed) maps the settings group
// onto this struct; the scheduler-only keys (enabled/interval/offpeak_hour/
// park_max_ms/retention_days/exact_touch_budget_bytes) stay on the events
// side.
type Config struct {
	KList             string  // comma-separated k set, e.g. "10,75"
	QueriesPerStratum int     // target sample size per stratum
	StrataBounds      string  // "b1,b2" class bounds (K3: pinned to 4096,65536)
	ExactBudgetMS     int     // run-wide exact-leg time budget; <=0 = unlimited
	LegTimeoutMS      int     // hard per-leg statement_timeout cap
	EfSearch          int     // ANN-leg hnsw.ef_search; 0 = pgvector default 40
	Epsilon           float64 // tie tolerance of the recall definition
}

// Additional invalid-reason codes of the orchestration layer (the plan-proof
// codes live in probe.go). Every abort is VISIBLE, never silent (§4.2.4).
const (
	ReasonBudgetExhausted     = "budget_exhausted"
	ReasonDemandDeferred      = "demand_deferred"
	ReasonNoMeasurableQueries = "no_measurable_queries"
)

// RunResult is the outcome of one RunOnce: the shared run_group and the rows
// as persisted (one per stratum × k, invalid rows included).
type RunResult struct {
	RunGroup string
	Rows     []Run
}

// Hooks are the W01-3 injection points, all optional (zero value = no-op).
// They exist so the scheduler arm can wire demand-defer, the effective embed
// model and the previous-run scope map WITHOUT recall importing scheduler or
// backends (design/01 §4.1 non-cyclic package map; the embed_model stamp is
// injected instead of resolved here for the same reason).
type Hooks struct {
	// BeforeProbe runs before EVERY two-leg probe (§4.3 mid-run granularity:
	// one probe, the yieldThenRebuildOverview pattern). A non-nil error stops
	// the run; all not-yet-measured stratum×k rows are written valid=false
	// with invalid_reason='demand_deferred' — visible, never silent.
	BeforeProbe func(ctx context.Context) error
	// EmbedModel is the EFFECTIVE embed model string (backends resolution,
	// §4.2.6 — not the dead embed_model column). Empty disables the log-
	// sampling cache join (loo covers) and omits the meta stamp.
	EmbedModel string
	// PrevScopes maps stratum → the scope measured in the previous run
	// (W01-3 feeds it from LatestByStratum) for the scope_changed stamp.
	PrevScopes map[string]string
}

// RunOnce executes one full measurement run with default hooks (§4.2.5).
func RunOnce(ctx context.Context, pool *pgxpool.Pool, cfg Config, registry *blocktype.Registry) (RunResult, error) {
	return RunOnceWithHooks(ctx, pool, cfg, registry, Hooks{})
}

// RunOnceWithHooks is RunOnce with the W01-3 injection points exposed.
//
// Budget semantics in W01-2 (§6.2, wiring completed in W01-3): the budget
// clock starts BEFORE the stratification — the plan phase and the log-
// sampling scan are not free and are not pretended to be. Every probe checks
// the remaining budget; per leg statement_timeout = min(rest budget,
// leg_timeout_ms). Exhaustion marks the remaining rows valid=false +
// meta.budget_exhausted (the W01-3 rotation picks them up next run).
func RunOnceWithHooks(ctx context.Context, pool *pgxpool.Pool, cfg Config, registry *blocktype.Registry, hooks Hooks) (RunResult, error) {
	ks, err := ParseKList(cfg.KList)
	if err != nil {
		return RunResult{}, err
	}
	b1, b2, err := ParseStrataBounds(cfg.StrataBounds)
	if err != nil {
		return RunResult{}, err
	}
	if cfg.QueriesPerStratum <= 0 {
		return RunResult{}, fmt.Errorf("recall: queries_per_stratum must be > 0, got %d", cfg.QueriesPerStratum)
	}

	clock := newBudgetClock(time.Duration(cfg.ExactBudgetMS) * time.Millisecond)
	legTimeout := time.Duration(cfg.LegTimeoutMS) * time.Millisecond
	if legTimeout <= 0 {
		legTimeout = time.Minute
	}

	stamp, err := envStamp(ctx, pool)
	if err != nil {
		// Fail closed: a measurement without its environment stamp is
		// worthless at target scale (§4.2.6) — no stamp, no run.
		return RunResult{}, err
	}
	if hooks.EmbedModel != "" {
		stamp["embed_model"] = hooks.EmbedModel
	}
	stamp["strata_bounds"] = cfg.StrataBounds
	stamp["epsilon"] = cfg.Epsilon

	plans, err := PlanStrata(ctx, pool, registry, b1, b2, hooks.PrevScopes)
	if err != nil {
		return RunResult{}, err
	}

	logVecs, err := SampleLogQueries(ctx, pool, hooks.EmbedModel, cfg.QueriesPerStratum)
	if err != nil {
		return RunResult{}, err
	}

	result := RunResult{RunGroup: uuid.NewString()}
	stopReason := "" // "" = measuring; else every remaining row is invalid with this reason

	for _, plan := range plans {
		for _, k := range ks {
			run := Run{
				RunGroup:       result.RunGroup,
				Stratum:        plan.Stratum,
				Scope:          plan.Scope,
				CorpusEmbedded: plan.CorpusEmbedded,
				K:              int16(k),
				EfSearch:       cfg.EfSearch,
				IterativeScan:  "relaxed_order", // mirror of the ANN-leg GUC (073:100)
				Meta:           maps.Clone(stamp),
			}
			if plan.ScopeChanged {
				run.Meta["scope_changed"] = true
			}

			if stopReason != "" {
				markInvalid(&run, stopReason)
			} else if err := measureStratum(ctx, pool, cfg, plan, k, logVecs, hooks, clock, legTimeout, &run); err != nil {
				return result, err
			} else if run.Meta["invalid_reason"] == ReasonBudgetExhausted ||
				run.Meta["invalid_reason"] == ReasonDemandDeferred {
				// A mid-set stop propagates to every following stratum×k of
				// this run; the row itself was already marked by measureStratum.
				stopReason = run.Meta["invalid_reason"].(string)
			}

			if err := Insert(ctx, pool, run); err != nil {
				return result, err
			}
			result.Rows = append(result.Rows, run)
		}
	}
	return result, nil
}

// markInvalid sets the fail-closed shape of an unmeasured row: valid=false,
// no recall numbers (columns stay NULL), reason in meta (§4.2.4 doctrine —
// visible "not measurable" over invisible wrong).
func markInvalid(run *Run, reason string) {
	run.Valid = false
	run.QuerySource = "loo"
	run.NQueries = 0
	run.Meta["invalid_reason"] = reason
	if reason == ReasonBudgetExhausted {
		run.Meta["budget_exhausted"] = true
	}
}

// measureStratum runs the probe set for one stratum × k and fills run with
// either the aggregate or the fail-closed invalid shape.
func measureStratum(ctx context.Context, pool *pgxpool.Pool, cfg Config, plan StratumPlan, k int, logVecs [][]float32, hooks Hooks, clock *budgetClock, legTimeout time.Duration, run *Run) error {
	// Query set: logged queries first (real production profile), loo fills
	// the remainder — the small-scope normal case (§4.2.2).
	type probeInput struct {
		vec    []float32
		selfID *string
	}
	var inputs []probeInput
	for _, v := range logVecs {
		if len(inputs) >= cfg.QueriesPerStratum {
			break
		}
		inputs = append(inputs, probeInput{vec: v})
	}
	nLog := len(inputs)
	if need := cfg.QueriesPerStratum - nLog; need > 0 {
		loo, err := SampleLOO(ctx, pool, plan.Scopes, plan.VisibleTypes, need)
		if err != nil {
			return err
		}
		for i := range loo {
			inputs = append(inputs, probeInput{vec: loo[i].Vec, selfID: &loo[i].ID})
		}
	}
	switch {
	case nLog > 0 && len(inputs) > nLog:
		run.QuerySource = "mixed"
	case nLog > 0:
		run.QuerySource = "log"
	default:
		run.QuerySource = "loo"
	}
	if len(inputs) == 0 {
		markInvalid(run, ReasonNoMeasurableQueries)
		return nil
	}

	var (
		recalls, annMs, exactMs []float64
		nEffMin                 = math.MaxInt
	)
	for _, in := range inputs {
		if hooks.BeforeProbe != nil {
			if err := hooks.BeforeProbe(ctx); err != nil {
				// Deliberate error swallow (hence the nolint): a defer signal
				// from the hook is a VALID run outcome, not a failure — the
				// row is written fail-closed as demand_deferred (§4.3), the
				// run itself did not error.
				markInvalid(run, ReasonDemandDeferred)
				return nil //nolint:nilerr // defer signal is a valid outcome, recorded on the row
			}
		}
		remaining := clock.remaining()
		if remaining <= 0 {
			markInvalid(run, ReasonBudgetExhausted)
			return nil
		}
		timeout := legTimeout
		if remaining < timeout {
			timeout = remaining
		}

		res, err := Probe(ctx, pool, ProbeSpec{
			Vec:          in.vec,
			SelfID:       in.selfID,
			Scopes:       plan.Scopes,
			VisibleTypes: plan.VisibleTypes,
			K:            k,
			EfSearch:     cfg.EfSearch,
			Epsilon:      cfg.Epsilon,
			Timeout:      timeout,
		})
		if err != nil {
			return err
		}
		if !res.Valid {
			// Plan assertion violated — fail-closed for the whole stratum×k
			// row: no recall number is written (§4.2.4).
			markInvalid(run, res.InvalidReason)
			return nil
		}
		if res.NEff == 0 {
			continue // nothing measurable for this query (e.g. 1-block scope under self-exclusion)
		}
		recalls = append(recalls, res.Recall)
		annMs = append(annMs, res.AnnMs)
		exactMs = append(exactMs, res.ExactMs)
		if res.NEff < nEffMin {
			nEffMin = res.NEff
		}
	}
	if len(recalls) == 0 {
		markInvalid(run, ReasonNoMeasurableQueries)
		return nil
	}

	run.Valid = true
	run.NQueries = int16(len(recalls))
	run.RecallAvg = ptrOf(mean(recalls))
	run.RecallMin = ptrOf(minOf(recalls))
	run.AnnMsP50 = ptrOf(percentile(annMs, 0.50))
	run.AnnMsP95 = ptrOf(percentile(annMs, 0.95))
	run.ExactMsP50 = ptrOf(percentile(exactMs, 0.50))
	run.Meta["n_eff_min"] = nEffMin
	return nil
}

// envStamp collects the drift-aware environment stamp of a run (§4.2.6):
// pgvector extension version (pg_extension read — `SHOW hnsw.*` fails until
// the vector lib is loaded, §5.7), server version and the HNSW indexdef
// (makes the ef_construction 64/128 drift visible in every series).
func envStamp(ctx context.Context, pool *pgxpool.Pool) (map[string]any, error) {
	stamp := make(map[string]any, 8)
	var pgvVersion, pgVersion, indexdef string
	if err := pool.QueryRow(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'vector'`,
	).Scan(&pgvVersion); err != nil {
		return nil, fmt.Errorf("recall: pgvector version stamp: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version')`,
	).Scan(&pgVersion); err != nil {
		return nil, fmt.Errorf("recall: pg version stamp: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, hnswIndexName,
	).Scan(&indexdef); err != nil {
		return nil, fmt.Errorf("recall: indexdef stamp (%s): %w", hnswIndexName, err)
	}
	if len(indexdef) > maxMetaStringLen {
		indexdef = indexdef[:maxMetaStringLen]
	}
	stamp["pgvector_version"] = pgvVersion
	stamp["pg_version"] = pgVersion
	stamp["index_reloptions"] = indexdef
	return stamp, nil
}

// ParseKList parses the recall_check.k_list value ("10,75") into a sorted,
// deduplicated int list.
func ParseKList(s string) ([]int, error) {
	var ks []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, err := strconv.Atoi(part)
		if err != nil || k <= 0 {
			return nil, fmt.Errorf("recall: invalid k_list entry %q (need positive integers)", part)
		}
		ks = append(ks, k)
	}
	if len(ks) == 0 {
		return nil, fmt.Errorf("recall: k_list %q is empty", s)
	}
	sort.Ints(ks)
	ks = uniqueInts(ks)
	return ks, nil
}

// ParseStrataBounds parses recall_check.strata_bounds ("4096,65536") into
// (b1, b2) with 0 < b1 < b2. The default is pinned to the E-02-1 selector
// thresholds (masterplan K3) so the strata boundaries ARE the dispatch
// thresholds.
func ParseStrataBounds(s string) (int, int, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("recall: strata_bounds %q must be exactly \"b1,b2\"", s)
	}
	b1, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	b2, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || b1 <= 0 || b2 <= b1 {
		return 0, 0, fmt.Errorf("recall: strata_bounds %q invalid (need 0 < b1 < b2)", s)
	}
	return b1, b2, nil
}

// budgetClock is the §6.2 time budget: started before the plan phase, checked
// before every probe. total <= 0 means unlimited (tests / manual runs).
type budgetClock struct {
	start time.Time
	total time.Duration
}

func newBudgetClock(total time.Duration) *budgetClock {
	return &budgetClock{start: time.Now(), total: total}
}

func (b *budgetClock) remaining() time.Duration {
	if b.total <= 0 {
		return time.Duration(math.MaxInt64)
	}
	return b.total - time.Since(b.start)
}

func ptrOf[T any](v T) *T { return &v }

func mean(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func minOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

// percentile is the nearest-rank percentile over a copy of xs (sorting must
// not reorder the caller's latency series).
func percentile(xs []float64, p float64) float64 {
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func uniqueInts(sorted []int) []int {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}
