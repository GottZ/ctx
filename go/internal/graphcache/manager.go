package graphcache

import (
	"sync"
	"time"
)

// State is the graph-cache lifecycle state (design/05 §4.6). It is an Ops-level
// state machine over the cache's OWN derived state — deliberately NOT a backend
// circuit breaker (the documented "kein Circuit-Breaker" non-decision,
// backends/pool.go:470-472, is respected: no error counter switches a BACKEND
// off; this counter only drives the cache's own serve/red state).
type State int

const (
	// StateEmpty: no snapshot published yet (boot before the first build) —
	// consumers use their SQL path.
	StateEmpty State = iota
	// StateFresh: a snapshot is live and current — cache paths active (once the
	// serve flags are on). The steady state.
	StateFresh
	// StateDegraded: a dirty signal is pending and its Dirty-Age exceeded
	// MaxStaleness — consumers fall back to SQL; the old snapshot stays live.
	StateDegraded
	// StateFailed: consecutive build failures reached FailedThreshold — serve=SQL,
	// status red, an error log per attempt. Reachable from any state.
	StateFailed
)

// String renders the state for the /api/status wire block.
func (s State) String() string {
	switch s {
	case StateEmpty:
		return "Empty"
	case StateFresh:
		return "Fresh"
	case StateDegraded:
		return "Degraded"
	case StateFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// StateConfig is the policy subset the Manager needs to derive State (§4.6). It
// is passed per call so the Manager imports NO config package (keeping graphcache
// a leaf) and stays hot-reload transparent — the caller reads the live config
// snapshot each tick and hands in the current values.
type StateConfig struct {
	MaxStaleness    time.Duration
	FailedThreshold int
}

// Status is the /api/status graph_cache block (design/05 §4.6). Staleness is the
// Dirty-Age (§4.3), NOT now−BuiltAt; BuiltAt is a pure diagnostic.
type Status struct {
	State          State
	Seq            uint64
	BuiltAt        time.Time
	Staleness      time.Duration
	Nodes          int
	DreamEdges     int
	StructEdges    int
	LastBuildDur   time.Duration
	LastErrorClass string
	Fails          int
}

// Manager owns the double-buffer Store PLUS the derived-state bookkeeping of the
// cache track (design/05 §4.6): the Dirty-Age clock (§4.3), the consecutive-
// build-fail counter and the state automaton. The split is deliberate — the
// graphcache package owns the AUTOMATON and the dirty accounting; the scheduler
// (events) owns the CADENCE that drives the build loop and calls these methods
// (§4.3: graphcache besitzt den Zustand, events die Kadenz). All bookkeeping is
// mutex-guarded; the Store underneath is lock-free (atomic pointer).
type Manager struct {
	store Store

	mu sync.Mutex

	// Dirty clock (§4.3). pending marks an unconsumed signal; firstPendingAt is
	// the age anchor of the OLDEST unconsumed signal (Staleness = now − it);
	// lastDirtyAt is the youngest signal (the debounce quiet clock). The youngest
	// write NEVER moves firstPendingAt — the newest write must not reset the clock.
	pending        bool
	firstPendingAt time.Time
	lastDirtyAt    time.Time

	// Build-in-flight carry (§4.2 "consume ONLY pre-build signals"). While a build
	// runs, the FIRST MarkDirty that arrives is remembered here so a build that
	// consumed its pre-start signals hands the clock to the oldest DURING-build
	// write instead of clearing it (a write during the build survives the swap).
	building     bool
	carryPending bool
	carryFirstAt time.Time

	lastBuildStart   time.Time     // start of the most recent build ATTEMPT (cadence anchor, §4.3)
	consecutiveFails int           // consecutive build failures (§4.6)
	lastBuildDur     time.Duration // duration of the last build attempt (diagnostic)
	lastBuildAt      time.Time     // wall clock of the last SUCCESSFUL swap (diagnostic)
	lastErrClass     string        // error class of the last failed build (diagnostic)
}

// NewManager returns an empty manager (StateEmpty until the first CommitBuild).
func NewManager() *Manager { return &Manager{} }

// Current exposes the live snapshot (nil = not ready → SQL fallback). Lock-free.
func (m *Manager) Current() *Snapshot { return m.store.Current() }

// MarkDirty records a link-write signal at time now (§4.3). lastDirtyAt always
// advances (the quiet clock); firstPendingAt is set ONLY when a NEW pending
// episode opens — the youngest write never resets the age anchor. A write that
// arrives WHILE a build runs is also remembered as the carry anchor so it
// survives the post-build consume (§4.2).
func (m *Manager) MarkDirty(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDirtyAt = now
	if !m.pending {
		m.pending = true
		m.firstPendingAt = now
	}
	if m.building && !m.carryPending {
		m.carryPending = true
		m.carryFirstAt = now
	}
}

// Dirty returns a consistent read of the dirty clock at now: whether a signal is
// pending, the quiet duration (now − lastDirtyAt) and the pending age (now −
// firstPendingAt). quiet/pendingAge are zero when nothing is pending.
func (m *Manager) Dirty(now time.Time) (pending bool, quiet, pendingAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.pending {
		return false, 0, 0
	}
	return true, now.Sub(m.lastDirtyAt), now.Sub(m.firstPendingAt)
}

// Staleness is the Dirty-Age (§4.3): 0 when nothing is pending, else now −
// firstPendingAt (the age of the OLDEST unconsumed signal). It is explicitly NOT
// now − BuiltAt — a clean cache does not age, so an idle DB holds Staleness 0
// regardless of how old the snapshot is (the structural fix behind the Idle-
// Negativ-Gate, §4.6).
func (m *Manager) Staleness(now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.pending {
		return 0
	}
	return now.Sub(m.firstPendingAt)
}

// LastBuildStart is the cadence anchor: min_rebuild_interval and the hard
// interval are both measured from the last build START (§4.3). Zero before the
// first build.
func (m *Manager) LastBuildStart() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastBuildStart
}

// BeginBuild opens a build at time now: it stamps the cadence anchor and arms the
// during-build carry. It does NOT touch pending/firstPendingAt — the pre-build
// signals stay accounted until CommitBuild consumes them (§4.2).
func (m *Manager) BeginBuild(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.building = true
	m.carryPending = false
	m.carryFirstAt = time.Time{}
	m.lastBuildStart = now
}

// CommitBuild publishes snap (atomic swap, seq++) and consumes the signals that
// arrived BEFORE the build start (§4.2). A write that landed DURING the build
// survives: pending stays true with firstPendingAt = the oldest during-build
// write, so Staleness keeps counting from it. With no during-build write the
// cache is clean (pending=false, Staleness 0). Clears the fail counter.
func (m *Manager) CommitBuild(snap *Snapshot, now time.Time, dur time.Duration) {
	m.store.Swap(snap) // serving begins only here (§4.3): the swap is the publish
	m.mu.Lock()
	defer m.mu.Unlock()
	m.building = false
	m.consecutiveFails = 0
	m.lastErrClass = ""
	m.lastBuildDur = dur
	m.lastBuildAt = now
	if m.carryPending {
		m.pending = true
		m.firstPendingAt = m.carryFirstAt
	} else {
		m.pending = false
		m.firstPendingAt = time.Time{}
	}
	m.carryPending = false
}

// FailBuild records a failed build attempt: the old snapshot stays live, the
// dirty clock keeps running (pending/firstPendingAt untouched — the Dirty-Age
// keeps growing), and the consecutive-fail counter advances. It returns the new
// fail count so the caller logs at the right level (WARN below FailedThreshold,
// ERROR at/above, §4.3/§4.6). A during-build write is already reflected in
// pending via MarkDirty, so the carry is simply dropped. errClass is a short
// diagnostic token for the status block.
func (m *Manager) FailBuild(errClass string, dur time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.building = false
	m.carryPending = false
	m.consecutiveFails++
	m.lastErrClass = errClass
	m.lastBuildDur = dur
	return m.consecutiveFails
}

// State derives the §4.6 automaton state at now under cfg. Order is load-bearing:
// Failed (fail-count breach) outranks everything — a broken builder is the
// loudest signal, reachable even from Empty (boot builds failing) or Fresh. Then
// Empty (no snapshot yet). Then Degraded (pending AND Dirty-Age past MaxStaleness
// — the ONLY path off Fresh, and never on an idle DB). Else Fresh.
func (m *Manager) State(now time.Time, cfg StateConfig) State {
	threshold := cfg.FailedThreshold
	if threshold <= 0 {
		threshold = 3
	}
	m.mu.Lock()
	fails := m.consecutiveFails
	pending := m.pending
	var age time.Duration
	if pending {
		age = now.Sub(m.firstPendingAt)
	}
	m.mu.Unlock()

	if fails >= threshold {
		return StateFailed
	}
	if m.store.Current() == nil {
		return StateEmpty
	}
	if pending && age > cfg.MaxStaleness {
		return StateDegraded
	}
	return StateFresh
}

// Status assembles the /api/status graph_cache block at now under cfg (§4.6).
func (m *Manager) Status(now time.Time, cfg StateConfig) Status {
	st := Status{
		State:     m.State(now, cfg),
		Staleness: m.Staleness(now),
	}
	m.mu.Lock()
	st.LastBuildDur = m.lastBuildDur
	st.LastErrorClass = m.lastErrClass
	st.Fails = m.consecutiveFails
	m.mu.Unlock()
	if snap := m.store.Current(); snap != nil {
		st.Seq = snap.Seq
		st.BuiltAt = snap.BuiltAt
		st.Nodes = snap.Stats.Nodes
		st.DreamEdges = snap.Stats.DreamEdges
		st.StructEdges = snap.Stats.StructEdges
	}
	return st
}
