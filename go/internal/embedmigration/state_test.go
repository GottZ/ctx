package embedmigration

import "testing"

// TestAllowedMap_ForbidsPendingToDone is the test-first RED belief for the
// Allowed-Map (design/04 §4.1, Evokoa-Clean-Room W04-3 gate): "pending" must
// never jump straight to "done" — the statemachine diagram routes every
// migration through running → (verifying →) done. Written BEFORE the
// package existed (a plain "go vet ./internal/embedmigration/" on this file
// alone fails to compile: no Status type, no IsAllowedTransition symbol) —
// the compile failure IS the RED belief for a package that has zero prior
// state to be wrong about.
func TestAllowedMap_ForbidsPendingToDone(t *testing.T) {
	if IsAllowedTransition(StatusPending, StatusDone) {
		t.Fatalf("IsAllowedTransition(pending, done) = true, want false — no state skips running/verifying")
	}
}

func TestAllowedMap_AllowsFullHappyPath(t *testing.T) {
	path := []Status{StatusPending, StatusRunning, StatusVerifying, StatusDone}
	for i := 0; i < len(path)-1; i++ {
		if !IsAllowedTransition(path[i], path[i+1]) {
			t.Errorf("IsAllowedTransition(%s, %s) = false, want true (happy path)", path[i], path[i+1])
		}
	}
}

func TestAllowedMap_AllowsDoneToRolledBack(t *testing.T) {
	// The one documented exception to "done/aborted/rolled_back are
	// terminal" (design §4.1): a post-cutover rollback.
	if !IsAllowedTransition(StatusDone, StatusRolledBack) {
		t.Errorf("IsAllowedTransition(done, rolled_back) = false, want true (§4.10 rollback)")
	}
}

func TestAllowedMap_TerminalStatesHaveNoOtherExits(t *testing.T) {
	for _, terminal := range []Status{StatusAborted, StatusRolledBack} {
		for _, to := range []Status{StatusPending, StatusRunning, StatusPaused, StatusVerifying, StatusDone, StatusAborted, StatusRolledBack} {
			if to == terminal {
				continue
			}
			if IsAllowedTransition(terminal, to) {
				t.Errorf("IsAllowedTransition(%s, %s) = true, want false — %s is terminal", terminal, to, terminal)
			}
		}
	}
	// done has exactly one exit (rolled_back), pinned separately above.
	for _, to := range []Status{StatusPending, StatusRunning, StatusPaused, StatusVerifying, StatusDone, StatusAborted} {
		if IsAllowedTransition(StatusDone, to) {
			t.Errorf("IsAllowedTransition(done, %s) = true, want false — done's only exit is rolled_back", to)
		}
	}
}

func TestAllowedMap_AbortReachableFromEveryNonTerminalState(t *testing.T) {
	for _, from := range []Status{StatusPending, StatusRunning, StatusPaused, StatusVerifying} {
		if !IsAllowedTransition(from, StatusAborted) {
			t.Errorf("IsAllowedTransition(%s, aborted) = false, want true (§4.1 abort from any non-terminal state)", from)
		}
	}
}

func TestAllowedMap_VerifyingCanFailBackToPaused(t *testing.T) {
	if !IsAllowedTransition(StatusVerifying, StatusPaused) {
		t.Errorf("IsAllowedTransition(verifying, paused) = false, want true (§4.7 red verify → paused, never auto-abort)")
	}
}
