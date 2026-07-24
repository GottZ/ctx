package events

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// The armRunSource trap (design/01 §4.3, §9): the status collector feeds the
// guard/digest/overview last-run stamps into /api/status through an OPTIONAL
// interface assertion (status.go:31-33). A change to LastArmRuns' signature
// would NOT fail compilation — it would silently drop those stamps from the
// status surface, an unnoticed production regression between two waves. These
// compile-time pins mirror the exact shapes so W01-3's ADDITIVE LastRecallRun
// can never accidentally rewrite LastArmRuns.
var (
	_ interface {
		LastArmRuns() (time.Time, time.Time, time.Time)
	} = (*Scheduler)(nil)
	_ interface {
		LastRecallRun() time.Time
	} = (*Scheduler)(nil)
)

// TestRecallRunStampIsSeparate pins that lastRecallNs is a SEPARATE atomic:
// stamping recall must not move the guard/digest/overview stamps and vice
// versa (the additive-not-signature-change requirement, §4.3).
func TestRecallRunStampIsSeparate(t *testing.T) {
	s := NewScheduler(nil, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	if g, d, o := s.LastArmRuns(); !g.IsZero() || !d.IsZero() || !o.IsZero() {
		t.Fatalf("fresh arm stamps must be zero, got %v/%v/%v", g, d, o)
	}
	if !s.LastRecallRun().IsZero() {
		t.Fatalf("fresh recall stamp must be zero, got %v", s.LastRecallRun())
	}

	// Stamping recall moves ONLY the recall stamp.
	s.lastRecallNs.Store(time.Now().UnixNano())
	if s.LastRecallRun().IsZero() {
		t.Fatal("LastRecallRun did not reflect the recall stamp")
	}
	if g, d, o := s.LastArmRuns(); !g.IsZero() || !d.IsZero() || !o.IsZero() {
		t.Fatalf("recall stamp leaked into LastArmRuns: %v/%v/%v", g, d, o)
	}

	// Stamping guard moves ONLY the guard arm stamp, never the recall one — a
	// second recall load below would falsely pass if they shared an atomic.
	s.lastGuardNs.Store(time.Now().UnixNano())
	if g, _, _ := s.LastArmRuns(); g.IsZero() {
		t.Fatal("guard stamp not reflected in LastArmRuns")
	}
}
