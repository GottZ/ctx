package events

import "testing"

// TestKickOverviewRebuildCoalesces pins the channel contract behind
// overview-rebuild-start: the first kick arms, a second kick while one is
// pending coalesces (false), and draining the channel re-opens the arm slot.
// The loop integration itself (kick skips the interval wait) rides on the
// select in runOverviewRebuild and is exercised by the manage-action
// integration path; this test keeps the non-blocking semantics honest —
// a kick must NEVER block a request goroutine.
func TestKickOverviewRebuildCoalesces(t *testing.T) {
	s := NewScheduler(nil, nil, nil, StartupConfig{})

	if !s.KickOverviewRebuild() {
		t.Fatalf("first kick: armed=false, want true")
	}
	if s.KickOverviewRebuild() {
		t.Fatalf("second kick while pending: armed=true, want coalesced false")
	}

	select {
	case <-s.overviewKick:
	default:
		t.Fatalf("pending kick not readable from overviewKick")
	}

	if !s.KickOverviewRebuild() {
		t.Fatalf("kick after drain: armed=false, want true")
	}
}
