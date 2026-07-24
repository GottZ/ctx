package graphcache

import (
	"testing"
	"time"
)

// fixed fake-clock base for the deterministic Dirty-Age / automaton gates.
var t0 = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// staleByBuiltAt is the WRONG staleness model the design warns against (§4.3):
// staleness = now − BuiltAt. It exists ONLY as the ROT contrast for the Idle-
// Negativ-Gate — a state derivation built on it degrades an idle cache falsely.
func staleByBuiltAt(now time.Time, snap *Snapshot, maxStaleness time.Duration) State {
	if snap == nil {
		return StateEmpty
	}
	if now.Sub(snap.BuiltAt) > maxStaleness {
		return StateDegraded
	}
	return StateFresh
}

const testMaxStaleness = 15 * time.Minute

var testCfg = StateConfig{MaxStaleness: testMaxStaleness, FailedThreshold: 3}

// TestStateIdleStaysFresh is the Idle-Negativ-Gate (design/05 §4.3/§4.6): an
// idle DB (no MarkDirty) with the clock pushed FAR past MaxStaleness must stay
// Fresh, and Staleness must read 0. The ROT contrast proves the trap: the naive
// now−BuiltAt model (staleByBuiltAt) falsely reports Degraded on the exact same
// state — the 96%-degraded bug the Dirty-Age definition structurally removes.
func TestStateIdleStaysFresh(t *testing.T) {
	m := NewManager()
	builtAt := at(0)
	// A boot build lands at t0; nothing is ever marked dirty afterwards.
	m.CommitBuild(&Snapshot{BuiltAt: builtAt}, builtAt, 5*time.Millisecond)

	// Clock advances a full hour — four MaxStaleness windows — with NO writes.
	now := at(3600)

	if got := m.Staleness(now); got != 0 {
		t.Errorf("idle Staleness = %v, want 0 (Dirty-Age is 0 without pending)", got)
	}
	if got := m.State(now, testCfg); got != StateFresh {
		t.Errorf("idle State = %v, want Fresh (a clean cache never ages)", got)
	}

	// ROT contrast: the BuiltAt-based model would wrongly degrade the same state.
	if got := staleByBuiltAt(now, m.Current(), testMaxStaleness); got != StateDegraded {
		t.Fatalf("BuiltAt-stub contrast broke: got %v, want Degraded "+
			"(the stub MUST fall into the trap for the gate to be meaningful)", got)
	}
}

// TestDirtyAgeAnchorsOldest pins that the YOUNGEST write never resets the clock
// (§4.3): after MarkDirty(t0) then MarkDirty(t1>t0), Staleness anchors on t0.
// The quiet clock (now − lastDirtyAt) tracks the youngest, t1.
func TestDirtyAgeAnchorsOldest(t *testing.T) {
	m := NewManager()
	m.CommitBuild(&Snapshot{BuiltAt: at(0)}, at(0), 0)

	m.MarkDirty(at(10)) // first signal — opens the episode
	m.MarkDirty(at(40)) // younger signal — must NOT move firstPendingAt

	now := at(100)
	if got, want := m.Staleness(now), 90*time.Second; got != want {
		t.Errorf("Staleness = %v, want %v (anchored on the oldest signal t=10, not the youngest t=40)", got, want)
	}
	pending, quiet, age := m.Dirty(now)
	if !pending {
		t.Fatal("Dirty pending = false, want true")
	}
	if want := 60 * time.Second; quiet != want {
		t.Errorf("quiet = %v, want %v (now − lastDirtyAt, youngest signal t=40)", quiet, want)
	}
	if want := 90 * time.Second; age != want {
		t.Errorf("pendingAge = %v, want %v (now − firstPendingAt, oldest signal t=10)", age, want)
	}
}

// TestCommitConsumesPreBuildSignals: a build that started AFTER a signal and saw
// no writes during the build consumes that signal — Staleness returns to 0
// (§4.2 "signals before build start consumed").
func TestCommitConsumesPreBuildSignals(t *testing.T) {
	m := NewManager()
	m.CommitBuild(&Snapshot{BuiltAt: at(0)}, at(0), 0)

	m.MarkDirty(at(10))
	m.BeginBuild(at(20))
	// No MarkDirty between BeginBuild and CommitBuild.
	m.CommitBuild(&Snapshot{BuiltAt: at(25)}, at(25), 3*time.Millisecond)

	if got := m.Staleness(at(30)); got != 0 {
		t.Errorf("post-build Staleness = %v, want 0 (pre-build signal consumed)", got)
	}
	if pending, _, _ := m.Dirty(at(30)); pending {
		t.Error("post-build pending = true, want false")
	}
}

// TestMarkDirtyDuringBuildSurvivesSwap: a write that arrives WHILE a build runs
// must survive the swap (§4.2) — pending stays true and the clock re-anchors on
// the DURING-build write (the oldest UNCONSUMED signal), not the consumed
// pre-build one, and not zero.
func TestMarkDirtyDuringBuildSurvivesSwap(t *testing.T) {
	m := NewManager()
	m.CommitBuild(&Snapshot{BuiltAt: at(0)}, at(0), 0)

	m.MarkDirty(at(10)) // pre-build signal (will be consumed)
	m.BeginBuild(at(20))
	m.MarkDirty(at(30)) // DURING the build — must survive
	m.CommitBuild(&Snapshot{BuiltAt: at(40)}, at(40), 5*time.Millisecond)

	pending, _, age := m.Dirty(at(100))
	if !pending {
		t.Fatal("pending = false after a during-build write, want true (it must survive the swap)")
	}
	if want := 70 * time.Second; age != want {
		t.Errorf("pendingAge = %v, want %v (re-anchored on the during-build write t=30, not the consumed t=10)", age, want)
	}
}

// TestStateFreshToDegraded: Fresh degrades ONLY when pending AND Dirty-Age >
// MaxStaleness (§4.6). Below the threshold it serves (Fresh).
func TestStateFreshToDegraded(t *testing.T) {
	m := NewManager()
	m.CommitBuild(&Snapshot{BuiltAt: at(0)}, at(0), 0)
	m.MarkDirty(at(0))

	// Dirty-Age within MaxStaleness → still Fresh (serve zone).
	if got := m.State(at(600), testCfg); got != StateFresh {
		t.Errorf("State at Dirty-Age 10min = %v, want Fresh (within MaxStaleness)", got)
	}
	// Dirty-Age past MaxStaleness → Degraded.
	if got := m.State(at(1000), testCfg); got != StateDegraded {
		t.Errorf("State at Dirty-Age ~16.7min = %v, want Degraded (past MaxStaleness)", got)
	}
}

// TestStateFailedThresholdAndRecovery: consecutive fails reach FailedThreshold →
// Failed; a successful build clears the counter → Fresh (§4.6).
func TestStateFailedThresholdAndRecovery(t *testing.T) {
	m := NewManager()
	m.CommitBuild(&Snapshot{BuiltAt: at(0)}, at(0), 0) // a live snapshot exists

	if got := m.FailBuild("build", 0); got != 1 {
		t.Fatalf("first FailBuild = %d, want 1", got)
	}
	if got := m.State(at(1), testCfg); got != StateFresh {
		t.Errorf("State after 1 fail = %v, want Fresh (below threshold 3; old snapshot serves)", got)
	}
	m.FailBuild("build", 0) // 2
	if got := m.FailBuild("build", 0); got != 3 {
		t.Fatalf("third FailBuild = %d, want 3", got)
	}
	if got := m.State(at(2), testCfg); got != StateFailed {
		t.Errorf("State at 3 consecutive fails = %v, want Failed", got)
	}
	// A good build from Failed → Fresh (counter cleared).
	m.BeginBuild(at(3))
	m.CommitBuild(&Snapshot{BuiltAt: at(3)}, at(3), 0)
	if got := m.State(at(4), testCfg); got != StateFresh {
		t.Errorf("State after recovery build = %v, want Fresh", got)
	}
}

// TestStateFailedOutranksEmpty: boot builds failing to the threshold with NO
// snapshot ever published resolve to Failed, not Empty (§4.6: the Failed arrow
// leaves Empty too).
func TestStateFailedOutranksEmpty(t *testing.T) {
	m := NewManager()
	if got := m.State(at(0), testCfg); got != StateEmpty {
		t.Fatalf("fresh manager State = %v, want Empty", got)
	}
	m.FailBuild("build", 0)
	m.FailBuild("build", 0)
	m.FailBuild("build", 0)
	if got := m.State(at(1), testCfg); got != StateFailed {
		t.Errorf("State after 3 boot fails (no snapshot) = %v, want Failed (not Empty)", got)
	}
}
