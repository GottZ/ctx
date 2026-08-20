package dream

import (
	"testing"
	"time"
)

// TestCycleTimeoutFor mirrors TestTemporalTimeout: the whole-cycle deadline
// resolves the router value when set, else the package CycleTimeout default.
// 0 (and a negative value that a hand-built router could carry) is the
// documented "package default" sentinel — config V16 rejects a negative value
// at boot and at the settings write, so the fallback is the second line of
// defence for hand-built routers, exactly as temporalTimeout's contract.
func TestCycleTimeoutFor(t *testing.T) {
	tests := []struct {
		name     string
		router   *Router
		expected time.Duration
	}{
		{
			name:     "nil router falls back to package default",
			router:   nil,
			expected: CycleTimeout,
		},
		{
			name:     "router without timeout falls back to package default",
			router:   &Router{},
			expected: CycleTimeout,
		},
		{
			name:     "router setting wins",
			router:   &Router{CycleTimeout: 2400 * time.Second},
			expected: 2400 * time.Second,
		},
		{
			name:     "negative router value falls back to package default",
			router:   &Router{CycleTimeout: -30 * time.Second},
			expected: CycleTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CycleTimeoutFor(tt.router); got != tt.expected {
				t.Errorf("CycleTimeoutFor() = %v, want %v", got, tt.expected)
			}
		})
	}
}
