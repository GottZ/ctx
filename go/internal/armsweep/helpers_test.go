package armsweep_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
)

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
