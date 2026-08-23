// The topic-label scheduler arm — wave W6 of the Cluster-Topic-Map
// (design/01 §4.8, Amendments A01-3 / A01-4).
//
// A structural TWIN of the overview rebuild arm, with one deliberate deviation
// and one deliberate omission.
//
// The DEVIATION: the demand yield does not live here. yieldThenRebuildOverview
// checks interactiveDemand() once before the run and never again, which is
// exactly right for a single uninterruptible gonum call and useless for a batch
// of sequential model calls — with a pre-check only, a batch of 200 runs to
// completion even when interactive load arrives at call two. The label arm's
// yield sits INSIDE the batch loop, in internal/topiclabel, and ends the batch
// instead of waiting: the remaining topics stay selectable for the next tick.
//
// The OMISSION: no boot run. The rebuild builds at boot when it never has,
// because an unbuilt map is unusable. An unlabelled map does not exist — W5
// names every topic deterministically inside the persist transaction — so
// starting a container has no reason to start a batch of inference.
package events

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/GottZ/ctx/internal/topiclabel"
)

// labelSnapshot is one tick's outcome, kept for /api/status.
//
// A pipeline that is doing nothing has to say WHY: "off", "not complex enough
// yet" and "no chat backend" are three different operational situations with
// three different answers, and the difference is invisible in a log that, by
// construction, contains nothing.
type labelSnapshot struct {
	stats topiclabel.Stats
	at    time.Time
}

// LabelingState reports the last label tick's ledger plus its wall-clock time.
// The bool is false before the first tick of this process.
//
// Its OWN narrow method, deliberately not folded into LastArmRuns: a signature
// change there silently drops the guard/digest/overview stamps from /api/status
// without a compile error (the armRunSource trap).
func (s *Scheduler) LabelingState() (topiclabel.Stats, time.Time, bool) {
	v, _ := s.labelState.Load().(labelSnapshot)
	if v.at.IsZero() {
		return topiclabel.Stats{}, time.Time{}, false
	}
	return v.stats, v.at, true
}

// runTopicLabeling is the arm goroutine.
func (s *Scheduler) runTopicLabeling(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in topic labeling", "error", r, "stack", string(debug.Stack()))
		}
	}()

	var tenantCursor uint64
	for {
		interval := s.cfg.Snapshot().GraphOverview.LabelInterval //nolint:forbidigo // MT 06 background: the label cadence is a server-global policy knob, like its rebuild sibling; the per-tenant loop rotates WHICH tenant is served per tick, not the cadence.
		if interval <= 0 {
			interval = time.Hour
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		s.labelTopicsOnce(ctx, s.nextOverviewTenant(ctx, &tenantCursor))
	}
}

// labelTopicsOnce serves ONE tenant. The Enabled gate lives here and not in the
// loop, so a hot toggle takes effect on the next tick — the rebuild arm's
// convention.
//
// One tenant per tick is inherited from the rebuild arm's round-robin, and it
// is what makes label_batch mean what design/01 §3.5 says it means: the upper
// bound of the model calls ONE tick can start.
func (s *Scheduler) labelTopicsOnce(ctx context.Context, bt backgroundTenant) {
	cfg := s.cfg.Snapshot() //nolint:forbidigo // MT 06 background: the label pipeline's gates, caps and thresholds are server-global policy knobs (tenancy global-only), like the rebuild's; the per-tick variation is the SCOPE WINDOW below.
	start := time.Now()

	// The window is the established background clamp: the configured read
	// scopes intersected with what this tenant actually owns. A scope the
	// tenant does not own must never enter a background read, and a label is
	// built from block titles.
	window := intersectWindow(cfg.Scheduler.ReadScopes, bt.owned)

	deps := topiclabel.Deps{
		Pool:     s.pool,
		Backends: s.backendPool,
		Adm:      s.backgroundAdmission(),
		Demand:   s.interactiveDemand,
		Floor:    cfg.Pool.ScopeSensitivityFloor.Apply,
		Chat:     topiclabel.ChainCall,
		Cfg: topiclabel.Config{
			Enabled:                 cfg.GraphOverview.Enabled && cfg.GraphOverview.LabelEnabled,
			Batch:                   cfg.GraphOverview.LabelBatch,
			MinTopics:               cfg.GraphOverview.LabelMinTopics,
			PromptMaxTitles:         cfg.GraphOverview.LabelPromptMaxTitles,
			Interval:                cfg.GraphOverview.LabelInterval,
			CallTimeout:             cfg.GraphOverview.LabelTimeout,
			CredentialsFallbackOnly: cfg.GraphOverview.LabelCredentialsFallbackOnly,
			// E3-01: ONE language knob per corpus. The label surface inherits
			// dream.language rather than growing a second switch — a per-tenant
			// language (parked backlog) then reaches labels for free.
			Language: cfg.Dream.Language,
		},
	}
	// A nil block-type registry is a wiring gap, not an empty allowlist: an
	// empty VisibleTypes matches zero rows, so the run would label nothing and
	// look like a quiet corpus. Skip loudly instead.
	if s.blocktypes == nil {
		slog.Error("scheduler: topic labeling skipped — block-type registry not wired")
		return
	}
	deps.Cfg.VisibleTypes = s.blocktypes.Snapshot().VisibleTypes()

	st := topiclabel.Run(ctx, deps, window)
	s.labelState.Store(labelSnapshot{stats: st, at: time.Now()})

	// The arm log carries the numbers design/01 §6.5 and §5.3-B8 promise to
	// MEASURE rather than assume: the drift rate (selected vs labeled), the
	// effectiveness of the in-loop yield (yielded/overrun/aborted), the model
	// latency that decides whether label_batch fits into one interval, and the
	// two rejection counters — a filter nobody can count is a silent filter.
	slog.Info("scheduler: topic labeling",
		"state", st.State, "tenant_scope", bt.scope, "scopes", window,
		"living_topics", st.LivingTopics, "min_topics", st.MinTopics,
		"selected", st.Selected, "labeled", st.Labeled, "failed", st.Failed,
		"quiesced", st.Quiesced,
		"rejected_scan", st.RejectedScan, "rejected_echo", st.RejectedEcho,
		"rejected_shape", st.RejectedShape,
		"yielded", st.Yielded, "overrun", st.Overrun, "aborted", st.Aborted,
		"latency_p50_ms", st.LatencyP50Ms, "latency_p95_ms", st.LatencyP95Ms,
		"elapsed", time.Since(start))
}
