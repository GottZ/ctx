package armsweep_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
)

// mustScore scores an input that is expected to be scorable. Score returns an
// error for exactly one situation (a damping sweep over a pre-142 dump), and a
// test that did not set up that situation must not have to spell the branch out
// at every call site — the ones that DO set it up call armsweep.Score directly.
func mustScore(t *testing.T, in armsweep.ScoreInput) armsweep.ReportBody {
	t.Helper()
	body, err := armsweep.Score(in)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	return body
}

// mustConfig resolves a static configuration or fails the test — a typo in a
// configuration name must not silently score the zero value.
func mustConfig(t *testing.T, name string) armsweep.Config {
	t.Helper()
	c, ok := armsweep.ConfigByName(name)
	if !ok {
		t.Fatalf("no static configuration %q", name)
	}
	return c
}
