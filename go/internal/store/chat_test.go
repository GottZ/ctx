// Pure unit tests for the chat store — no DB, runs under `go test -short`.
// The SQL-bound behaviour (scope isolation, read_scopes subset, busy CAS, seq +
// monotone HWM, cascade, retention) is proven in chat_integration_test.go.
package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDeriveTitle(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "New chat"},
		{"whitespace only", "  \n\t  ", "New chat"},
		{"short fits", "Hello world", "Hello world"},
		{"collapse internal whitespace", "  Hello \n\t  world  ", "Hello world"},
		// A space at rune 35 (>= maxRunes/2) → cut on that boundary.
		{"truncate at word boundary",
			strings.Repeat("a", 35) + " " + strings.Repeat("b", 40),
			strings.Repeat("a", 35) + "…"},
		// No space in the first 60 runes → hard cut at 60.
		{"truncate hard when no boundary",
			strings.Repeat("x", 80),
			strings.Repeat("x", 60) + "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveTitle(c.in); got != c.want {
				t.Errorf("DeriveTitle(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDeleteExpiredSessions_TTLNonPositiveIsNoOp(t *testing.T) {
	// ttl <= 0 returns before touching the pool — safe with a nil pool, proving
	// the off-by-default janitor never issues a DELETE.
	for _, ttl := range []time.Duration{0, -time.Hour} {
		n, err := DeleteExpiredSessions(context.Background(), nil, ttl)
		if err != nil || n != 0 {
			t.Fatalf("DeleteExpiredSessions(nil, %v) = %d, %v; want 0, nil", ttl, n, err)
		}
	}
}
