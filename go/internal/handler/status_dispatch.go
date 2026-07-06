package handler

import (
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
)

// Dispatch status surfaces (Vorhaben E wave MW12, 01-W6 + A5-W6). TWO distinct
// wire shapes deliberately, per the K13 exposure doctrine (Registry-/wait data
// is server-admin-only OR filtered to the caller's own principals):
//
//   - dispatchStatus (server-admin): the FULL registry snapshot incl. per-fair-
//     key bucket detail, preempt/aged counters, class downgrades, enforcing and
//     the P6 last-run timestamps.
//   - dispatchTenantStatus (tenant-admin): the E-A5-6(b) COARSENED occupancy
//     view — busy + a coarsened depth (leer/niedrig/hoch) per VISIBLE target,
//     plus the caller's OWN waiting/running leases. It carries NO FairKey field
//     at all, so a foreign principal is structurally unreachable through it
//     (F-B3 negative probe is true by construction, not by filtering care).
//
// Both are built ONLY from the cheapSnapshot-cached dispatch.Snapshot (never a
// live Snapshot() per request), so the sampling resolution is structurally
// bounded by events.tick_interval (design/05 §4.5, abtast-probe).

// ---- server-admin shape.

type dispatchStatus struct {
	Enabled         bool  `json:"enabled"`
	Enforcing       bool  `json:"enforcing"`
	Demand          int   `json:"demand"`
	ReapsTotal      int64 `json:"reaps_total"`
	ClassDowngrades int64 `json:"class_downgrades"`
	UnchargedCalls  int64 `json:"uncharged_calls"`
	OpsTotal        int64 `json:"ops_total"`
	MaxOpMs         int64 `json:"max_op_ms"`
	// Last-run wall-clock of the lease-free arms (P6 anti-starvation metric,
	// design/05 §4.5); null = arm did not run this process.
	LastGuardAt    *time.Time `json:"last_guard_at"`
	LastDigestAt   *time.Time `json:"last_digest_at"`
	LastOverviewAt *time.Time `json:"last_overview_at"`
	// EmbedTokens is the D1(a) embed-token metric: prompt_tokens of the embed
	// pipelines per target over the 24h window, DERIVED in-memory from the
	// already-collected llm_24h rollup (no extra hypertable scan).
	EmbedTokens []dispatchEmbedTokens `json:"embed_tokens"`
	Targets     []dispatchTarget      `json:"targets"`
}

type dispatchTarget struct {
	Origin            string           `json:"origin"`
	Slots             int              `json:"slots"`
	PreemptBackground bool             `json:"preempt_background"`
	HeraldScope       string           `json:"herald_scope"`
	Held              int              `json:"held"`
	Inflight          int              `json:"inflight"`
	Interactive       dispatchWait     `json:"interactive"`
	Background        dispatchWait     `json:"background"`
	Preempt           dispatchPreempt  `json:"preempt"`
	Buckets           []dispatchBucket `json:"buckets"`
}

type dispatchWait struct {
	Waiting      int   `json:"waiting"`
	OldestWaitMs int64 `json:"oldest_wait_ms"`
	P95WaitMs    int64 `json:"p95_wait_ms"`
	MaxWaitMs    int64 `json:"max_wait_ms"`
	Samples      int   `json:"samples"`
}

type dispatchPreempt struct {
	PreemptsTotal       int64 `json:"preempts_total"`
	WastedMsTotal       int64 `json:"wasted_ms_total"`
	ReleaseMsLast       int64 `json:"release_ms_last"`
	ReleaseMsMax        int64 `json:"release_ms_max"`
	ForcedReleasesTotal int64 `json:"forced_releases_total"`
	AgedAdmitsTotal     int64 `json:"aged_admits_total"`
	AgedPreemptsTotal   int64 `json:"aged_preempts_total"`
}

// dispatchBucket is the per-fair-key detail — SERVER-ADMIN ONLY. fair_key is
// principal detail (HomeScope); it never appears in the tenant shape.
type dispatchBucket struct {
	FairKey      string `json:"fair_key"`
	Waiting      int    `json:"waiting"`
	OldestWaitMs int64  `json:"oldest_wait_ms"`
	Inflight     int    `json:"inflight"`
	Tokens       int64  `json:"tokens"`
	Charges      int64  `json:"charges"`
}

type dispatchEmbedTokens struct {
	Target       string `json:"target"`
	PromptTokens int64  `json:"prompt_tokens"`
}

// ---- tenant-admin shape (E-A5-6b coarsened).

type dispatchTenantStatus struct {
	Targets []dispatchTenantTarget `json:"targets"`
}

// dispatchTenantTarget is the coarsened occupancy view of ONE target visible to
// the tenant. Busy + Depth are anonymous aggregates over ALL principals; Own*
// is the caller's own bucket. There is deliberately NO field carrying any
// foreign principal identity (F-B3).
type dispatchTenantTarget struct {
	Origin string `json:"origin"`
	// Busy reports whether the target holds any lease right now (aggregate,
	// no principal detail).
	Busy bool `json:"busy"`
	// Depth is the coarsened total waitQ depth: "leer" | "niedrig" | "hoch"
	// (E-A5-6b). The exact bucket boundaries are NOT load-bearing — only the
	// monotone mapping matters; full depth would add foreign-load resolution
	// the tenant view deliberately withholds.
	Depth string `json:"depth"`
	// OwnWaiting / OwnInflight / OwnOldestWaitMs describe the CALLER's own
	// fairness bucket on this target (its own queued + running interactive
	// leases). Zero when the caller has nothing here.
	OwnWaiting      int   `json:"own_waiting"`
	OwnInflight     int   `json:"own_inflight"`
	OwnOldestWaitMs int64 `json:"own_oldest_wait_ms"`
}

// depth coarsening thresholds (E-A5-6b). Any monotone bucketing satisfies the
// exposure intent; these keep "leer" honest (truly empty) and split the rest at
// a small boundary so a tenant sees "there is contention" without a live count.
const (
	depthNiedrigMax = 3 // 1..3 waiters ⇒ niedrig; above ⇒ hoch
)

func coarsenDepth(waiting int) string {
	switch {
	case waiting <= 0:
		return "leer"
	case waiting <= depthNiedrigMax:
		return "niedrig"
	default:
		return "hoch"
	}
}

// buildDispatchAdmin renders the full server-admin dispatch section from the
// cached snapshot. embedTokens is derived from the (already-collected) llm_24h
// rollup so no new query runs here.
func buildDispatchAdmin(snap dispatch.Snapshot, enforcing bool, guardAt, digestAt, overviewAt *time.Time, llm24h []llm24hRow) *dispatchStatus {
	out := &dispatchStatus{
		Enabled:         snap.Enabled,
		Enforcing:       enforcing,
		Demand:          snap.Demand,
		ReapsTotal:      snap.ReapsTotal,
		ClassDowngrades: snap.ClassDowngrades,
		UnchargedCalls:  snap.UnchargedCalls,
		OpsTotal:        snap.OpsTotal,
		MaxOpMs:         snap.MaxOpDur.Milliseconds(),
		LastGuardAt:     guardAt,
		LastDigestAt:    digestAt,
		LastOverviewAt:  overviewAt,
		EmbedTokens:     embedTokensFromRollup(llm24h),
		Targets:         make([]dispatchTarget, 0, len(snap.Targets)),
	}
	for _, t := range snap.Targets {
		dt := dispatchTarget{
			Origin:            t.Origin,
			Slots:             t.Slots,
			PreemptBackground: t.PreemptBackground,
			HeraldScope:       string(t.HeraldScope),
			Held:              t.Held,
			Inflight:          t.Inflight,
			Interactive:       waitOf(t.Interactive),
			Background:        waitOf(t.Background),
			Preempt: dispatchPreempt{
				PreemptsTotal:       t.Preempt.PreemptsTotal,
				WastedMsTotal:       t.Preempt.WastedMsTotal,
				ReleaseMsLast:       t.Preempt.ReleaseMsLast,
				ReleaseMsMax:        t.Preempt.ReleaseMsMax,
				ForcedReleasesTotal: t.Preempt.ForcedReleasesTotal,
				AgedAdmitsTotal:     t.Preempt.AgedAdmitsTotal,
				AgedPreemptsTotal:   t.Preempt.AgedPreemptsTotal,
			},
			Buckets: make([]dispatchBucket, 0, len(t.Buckets)),
		}
		for _, b := range t.Buckets {
			dt.Buckets = append(dt.Buckets, dispatchBucket{
				FairKey:      b.FairKey,
				Waiting:      b.Waiting,
				OldestWaitMs: b.OldestWait.Milliseconds(),
				Inflight:     b.Inflight,
				Tokens:       b.Tokens,
				Charges:      b.Charges,
			})
		}
		out.Targets = append(out.Targets, dt)
	}
	return out
}

func waitOf(w dispatch.WaitStats) dispatchWait {
	return dispatchWait{
		Waiting:      w.Waiting,
		OldestWaitMs: w.OldestWait.Milliseconds(),
		P95WaitMs:    w.P95Wait.Milliseconds(),
		MaxWaitMs:    w.MaxWait.Milliseconds(),
		Samples:      w.Samples,
	}
}

// embedTokensFromRollup sums the prompt_tokens of the embed pipelines per
// backend from the 24h rollup (D1(a)). Embed pipelines are the ones whose name
// contains "embed" (query-embed, embed-backfill) — reranks/dream/query never do,
// so the substring match isolates them and is self-maintaining for future embed
// pipelines. Returns [] (never nil) for a stable wire shape.
func embedTokensFromRollup(llm24h []llm24hRow) []dispatchEmbedTokens {
	byTarget := map[string]int64{}
	order := []string{}
	for _, r := range llm24h {
		if !strings.Contains(r.Pipeline, "embed") {
			continue
		}
		if _, seen := byTarget[r.Backend]; !seen {
			order = append(order, r.Backend)
		}
		byTarget[r.Backend] += r.PromptTokens
	}
	out := make([]dispatchEmbedTokens, 0, len(order))
	for _, tgt := range order {
		out = append(out, dispatchEmbedTokens{Target: tgt, PromptTokens: byTarget[tgt]})
	}
	return out
}

// buildDispatchTenant renders the coarsened per-tenant occupancy view. visible
// is the set of target origins the tenant may see (VisibleTo over the backend
// pool); a target absent from it does not appear at all (K-T1 / B-R3: a non-
// visible backend is invisible, not anonymized). fairKey is the caller's own
// fairness bucket key (its HomeScope) — the ONLY bucket whose detail is exposed.
func buildDispatchTenant(snap dispatch.Snapshot, visible map[string]bool, fairKey string) *dispatchTenantStatus {
	out := &dispatchTenantStatus{Targets: []dispatchTenantTarget{}}
	for _, t := range snap.Targets {
		if !visible[t.Origin] {
			continue
		}
		waiting := t.Interactive.Waiting + t.Background.Waiting
		tt := dispatchTenantTarget{
			Origin: t.Origin,
			Busy:   t.Held > 0 || t.Inflight > 0,
			Depth:  coarsenDepth(waiting),
		}
		// Own principal detail — matched on fairKey, NEVER emitting the key
		// itself. A foreign bucket contributes only to Busy/Depth above.
		if fairKey != "" {
			for _, b := range t.Buckets {
				if b.FairKey != fairKey {
					continue
				}
				tt.OwnWaiting = b.Waiting
				tt.OwnInflight = b.Inflight
				tt.OwnOldestWaitMs = b.OldestWait.Milliseconds()
				break
			}
		}
		out.Targets = append(out.Targets, tt)
	}
	return out
}
