// recall_check.go — the Achse-01 W01-3 scheduler arm (design/01 §4.3/§6.2).
// It wires internal/recall's measurement core (RunOnceWithHooks) into the
// background scheduler WITHOUT recall importing config/events (the config-
// mirror maps here). Delivered mechanics:
//
//   - two cadence anchors: cheap strata (small/medium) on recall_check.interval,
//     expensive strata (large/all) on the recall_check.offpeak_hour wall-clock
//     anchor (runDailySynthesis pattern), one expensive stratum per off-peak run
//     via a round-robin cursor (§6.2 rotation);
//   - demand defer VOR the run (interactiveDemand gate, guard pattern) AND
//     WÄHREND via the BeforeProbe park hook (yieldThenRebuildOverview pattern:
//     re-check per probe, park up to park_max_ms, then abort demand_deferred);
//   - touch budget resolution (recall_check.exact_touch_budget_bytes; 0 = auto
//     25% of shared_buffers) passed to recall as a concrete byte count;
//   - the effective embed model (RoleEmbed chain, wire-free) and previous-run
//     scope map (LatestByStratum) fed into the sampling/scope_changed naht;
//   - retention janitor (recall_check.retention_days) as an 8th 6h-bundle line;
//   - the ADDITIVE lastRecallNs stamp (LastRecallRun) — never a LastArmRuns
//     signature change (armRunSource trap, §4.3).
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/store"
)

// recallDemandYieldWait is the re-check cadence of the mid-run demand park loop
// (§4.3). A var, not a const, so the integration test can shorten it and
// observe the park→abort transition without waiting the production interval
// (dreamYieldWait precedent).
var recallDemandYieldWait = 15 * time.Second

// recallLatestScanLimit bounds the LatestByStratum read that feeds PrevScopes —
// a handful of (stratum,scope,k) rows, never the table.
const recallLatestScanLimit = 64

// runRecallCheck is the recall_check goroutine (design/01 §4.3). No new ticker
// case in Run(): it owns its cadence. Two anchors tracked as explicit next-run
// times so a hot interval/offpeak_hour change takes effect on the next
// iteration (the config snapshot is re-read every loop). No boot run — the
// first cheap run lands one interval after start, the first expensive run at
// the next off-peak boundary.
func (s *Scheduler) runRecallCheck(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in recall check", "error", r, "stack", string(debug.Stack()))
		}
	}()

	var expensiveCursor uint64
	rc := s.cfg.Snapshot().RecallCheck //nolint:forbidigo // MT 06 background: recall_check is global-only (one shared HNSW index + one buffer pool, §3.2) — never tenant-scoped.
	now := time.Now()
	nextCheap := now.Add(recallInterval(rc.Interval))
	nextOffpeak := nextHourBoundary(now, rc.OffpeakHour)

	for {
		wait := time.Until(nextCheap)
		if w := time.Until(nextOffpeak); w < wait {
			wait = w
		}
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		now = time.Now()
		rc = s.cfg.Snapshot().RecallCheck //nolint:forbidigo // MT 06 background: recall_check is global-only (§3.2).
		offpeakDue := !now.Before(nextOffpeak)
		cheapDue := !now.Before(nextCheap)

		switch {
		case offpeakDue:
			// The off-peak run measures the cheap strata AND one rotated
			// expensive stratum (§6.2): it subsumes a due cheap run, so both
			// clocks advance.
			s.recallCheckOnce(ctx, true, &expensiveCursor, s.interactiveDemand)
			nextOffpeak = nextHourBoundary(now, rc.OffpeakHour)
			nextCheap = now.Add(recallInterval(rc.Interval))
		case cheapDue:
			s.recallCheckOnce(ctx, false, &expensiveCursor, s.interactiveDemand)
			nextCheap = now.Add(recallInterval(rc.Interval))
		default:
			// Woke early (config shortened a horizon); loop recomputes the wait.
		}
	}
}

// recallInterval clamps a non-positive interval to the 24h default.
func recallInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return 24 * time.Hour
	}
	return d
}

// nextHourBoundary returns the next wall-clock instant at hour:00 strictly after
// now, in now's location (generalized runDailySynthesis anchor). An out-of-range
// hour falls back to the recall_check.offpeak_hour default (4).
func nextHourBoundary(now time.Time, hour int) time.Time {
	if hour < 0 || hour > 23 {
		hour = 4
	}
	loc := now.Location()
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target
}

// recallCheckOnce runs the arm once. includeExpensive selects the cadence
// (false = cheap-only interval run; true = off-peak run with one rotated
// expensive stratum). cursor is the round-robin rotation position; demand is the
// interactive-demand source (s.interactiveDemand in production, a controllable
// func in tests). Returns whether a measurement run actually started (false =
// disabled, unwired, or deferred before start). Testable in isolation — the
// goroutine above only supplies cadence.
func (s *Scheduler) recallCheckOnce(ctx context.Context, includeExpensive bool, cursor *uint64, demand func() int) bool {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in recall check run", "error", r, "stack", string(debug.Stack()))
		}
	}()

	cfg := s.cfg.Snapshot() //nolint:forbidigo // MT 06 background: recall_check is global-only (one shared physical resource, §3.2).
	rc := cfg.RecallCheck
	if !rc.Enabled {
		return false // master gate (E-01-1): a disabled arm writes no row.
	}
	if s.blocktypes == nil {
		slog.Error("scheduler: recall check skipped — block-type registry not wired")
		return false
	}

	// Demand defer VOR the run (guard/digest pattern): interactive load means no
	// launch at all — no row, no stamp. The mid-run park (BeforeProbe) covers
	// load that arrives AFTER a launch (§4.3).
	if d := demand(); d > 0 {
		slog.Debug("scheduler: recall check deferred, interactive demand", "count", d)
		return false
	}

	touchBudget, err := s.resolveRecallTouchBudget(ctx, rc.ExactTouchBudgetBytes)
	if err != nil {
		// Degrade to unbounded touch rather than skip the whole measurement —
		// the time budget + leg timeout still cap the run.
		slog.Warn("scheduler: recall touch budget resolution failed — running touch-unbounded", "error", err)
		touchBudget = 0
	}

	parkMax := time.Duration(rc.ParkMaxMS) * time.Millisecond
	if parkMax <= 0 {
		parkMax = 10 * time.Minute
	}

	hooks := recall.Hooks{
		BeforeProbe:  s.recallDemandGate(demand, parkMax),
		EmbedModel:   s.resolveRecallEmbedModel(cfg),
		PrevScopes:   s.recallPrevScopes(ctx),
		SelectStrata: selectRecallStrata(includeExpensive, cursor),
	}

	// Stamp the actual-run marker PAST the pre-run demand defer (MW12/§4.5, guard
	// pattern): a launch deferred before start does NOT advance it.
	s.lastRecallNs.Store(time.Now().UnixNano())

	res, err := recall.RunOnceWithHooks(ctx, s.pool, recallConfigFrom(rc, touchBudget), s.blocktypes, hooks)
	if err != nil {
		if ctx.Err() != nil {
			return true // shutdown mid-run — not an error worth logging loudly
		}
		slog.Error("scheduler: recall check run failed", "error", err)
		return true
	}
	slog.Info("scheduler: recall check complete",
		"run_group", res.RunGroup, "rows", len(res.Rows), "expensive", includeExpensive)
	return true
}

// recallConfigFrom maps the settings group onto the recall core mirror (design/
// 01 §3.2 split): the tunables recall consumes directly, plus the RESOLVED touch
// budget (the auto→25%-shared_buffers resolution stays on the events side).
func recallConfigFrom(rc config.RecallCheckConfig, touchBudget int64) recall.Config {
	return recall.Config{
		KList:                 rc.KList,
		QueriesPerStratum:     rc.QueriesPerStratum,
		StrataBounds:          rc.StrataBounds,
		ExactBudgetMS:         rc.ExactBudgetMS,
		LegTimeoutMS:          rc.LegTimeoutMS,
		EfSearch:              rc.EfSearch,
		Epsilon:               rc.Epsilon,
		ExactTouchBudgetBytes: touchBudget,
	}
}

// selectRecallStrata builds the SelectStrata hook (§4.3 cadence + §6.2
// rotation): cheap strata (small/medium) every run; on an off-peak run ONE
// expensive stratum (large/all) chosen round-robin over the expensive strata
// present this run. A cheap-only run drops the expensive strata entirely (they
// simply get no row — coverage-age visibility, not an invalid row). The cursor
// only advances when an expensive stratum is actually picked, so an all-cheap
// corpus never rotates.
func selectRecallStrata(includeExpensive bool, cursor *uint64) func([]recall.StratumPlan) []recall.StratumPlan {
	return func(plans []recall.StratumPlan) []recall.StratumPlan {
		var cheap, expensive []recall.StratumPlan
		for _, p := range plans {
			if p.Stratum == "small" || p.Stratum == "medium" {
				cheap = append(cheap, p)
			} else {
				expensive = append(expensive, p) // "large" | "all"
			}
		}
		if !includeExpensive || len(expensive) == 0 {
			return cheap
		}
		pick := expensive[*cursor%uint64(len(expensive))]
		*cursor++
		return append(cheap, pick)
	}
}

// recallDemandGate builds the BeforeProbe park hook (§4.3, yieldThenRebuild-
// Overview granularity: re-check before EVERY probe). No demand → proceed. Under
// demand → park, re-checking every recallDemandYieldWait, until demand clears
// (proceed) or the accumulated park time exceeds parkMax (abort: a non-nil error
// makes recall write the remaining rows valid=false/demand_deferred). ctx
// cancellation aborts the park too (shutdown).
func (s *Scheduler) recallDemandGate(demand func() int, parkMax time.Duration) func(context.Context) error {
	return func(ctx context.Context) error {
		if demand() == 0 {
			return nil
		}
		deadline := time.Now().Add(parkMax)
		for demand() > 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("recall: interactive demand parked past %s", parkMax)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(recallDemandYieldWait):
			}
		}
		return nil
	}
}

// resolveRecallEmbedModel resolves the EFFECTIVE embed model string wire-free
// (§4.2.6): the RoleEmbed chain head's model, exactly the string the query
// embeddings were cached under, so SampleLogQueries's touch-free cache join
// matches. Wire-free — EmbedChain only resolves, it never calls a backend. No
// eligible backend (or an unwired pool) returns "" → the sampler falls back to
// loo, wire-free doctrine intact (migrationModelGuard pattern).
func (s *Scheduler) resolveRecallEmbedModel(cfg *config.Config) string {
	router := s.newRouter(cfg, store.GlobalScope)
	chain, _, err := router.EmbedChain(router.FloorSens(backends.SensPublic, store.GlobalScope))
	if err != nil || len(chain) == 0 {
		return ""
	}
	return chain[0].ModelFor(backends.RoleEmbed).Model
}

// recallPrevScopes reads the previous run's measured scope per stratum for the
// scope_changed stamp (§4.2.1 object stability): the most recent valid,
// scope-bearing row per stratum (LatestByStratum is newest-first, so the first
// hit per stratum wins). The pseudo-stratum "all" carries no scope and is
// skipped. A read error is non-fatal — no prev map just means no scope_changed
// stamps this run.
func (s *Scheduler) recallPrevScopes(ctx context.Context) map[string]string {
	rows, err := recall.LatestByStratum(ctx, s.pool, recallLatestScanLimit)
	if err != nil {
		slog.Warn("scheduler: recall prev-scope read failed", "error", err)
		return nil
	}
	prev := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Scope == nil {
			continue
		}
		if _, seen := prev[r.Stratum]; !seen {
			prev[r.Stratum] = *r.Scope
		}
	}
	return prev
}

// resolveRecallTouchBudget resolves recall_check.exact_touch_budget_bytes: a
// positive config value is taken verbatim; 0 auto-derives 25% of shared_buffers
// at runtime (§6.2 — the auto default binds the topology to a measurable cache
// size, not a guess). pg_settings gives shared_buffers as a block count plus a
// unit ("8kB"); the byte size is count × unit.
func (s *Scheduler) resolveRecallTouchBudget(ctx context.Context, cfgBytes int) (int64, error) {
	if cfgBytes > 0 {
		return int64(cfgBytes), nil
	}
	var setting int64
	var unit string
	if err := s.pool.QueryRow(ctx,
		`SELECT setting::bigint, unit FROM pg_settings WHERE name = 'shared_buffers'`,
	).Scan(&setting, &unit); err != nil {
		return 0, fmt.Errorf("resolve shared_buffers: %w", err)
	}
	total := setting * unitBytes(unit)
	return total / 4, nil // 25% (§6.2 replica-threshold anchor)
}

// unitBytes converts a pg_settings unit string ("8kB", "kB", "16MB", "B", "")
// to its byte multiplier. Defaults to 1 on an unrecognized suffix.
func unitBytes(unit string) int64 {
	u := strings.TrimSpace(unit)
	i := 0
	for i < len(u) && u[i] >= '0' && u[i] <= '9' {
		i++
	}
	num := int64(1)
	if i > 0 {
		if n, err := strconv.ParseInt(u[:i], 10, 64); err == nil && n > 0 {
			num = n
		}
	}
	switch strings.ToLower(u[i:]) {
	case "kb":
		return num * 1024
	case "mb":
		return num * 1024 * 1024
	case "gb":
		return num * 1024 * 1024 * 1024
	default: // "b", "", or blocks reported as a bare count
		return num
	}
}

// runRecallRetention deletes recall-run rows past recall_check.retention_days —
// the 8th line of the 6h janitor bundle (§3.3). retention=0 is a no-op (kept
// forever). Same failure discipline as its neighbours: log, never fatal.
func (s *Scheduler) runRecallRetention(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in recall retention", "error", r, "stack", string(debug.Stack()))
		}
	}()
	days := s.cfg.Snapshot().RecallCheck.RetentionDays //nolint:forbidigo // MT 06 background: recall retention is a server-global janitor policy over a process-wide table, not tenant-scoped.
	deleted, err := recall.DeleteOlderThan(ctx, s.pool, days)
	if err != nil {
		slog.Warn("scheduler: recall retention failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("scheduler: recall runs evicted", "rows", deleted, "retention_days", days)
	}
}
