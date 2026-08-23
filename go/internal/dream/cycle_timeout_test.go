package dream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCycleTimeoutFor mirrors TestTemporalTimeout: the whole-cycle deadline
// resolves the router value when set, else the package CycleTimeout default.
// 0 (and a negative value that a hand-built router could carry) is the
// documented "package default" sentinel — config V16c rejects a negative
// value at boot and at the settings write, so the fallback is the second line
// of defence for hand-built routers, exactly as temporalTimeout's contract.
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

// TestPickClaimTTLCoversTheCycle pins the claim arithmetic: the transient
// claim PickBlock writes must outlive the cycle that holds the block, or a
// sibling worker (dream.parallelism, up to 16) re-picks it mid-cycle and both
// cycles dream the same block version. The bare CooldownTransientMinutes the
// claim used to be is kept only as a FLOOR, so a very short cycle recovers
// from a crash exactly as promptly as before.
func TestPickClaimTTLCoversTheCycle(t *testing.T) {
	const floor = CooldownTransientMinutes * time.Minute
	tests := []struct {
		name   string
		router *Router
		want   time.Duration
	}{
		// 700s default + 1min grace = 760s, already above the 300s floor —
		// so even the unchanged deployment gets a claim that covers its
		// cycle, which the 5-minute constant never did.
		{"unset cycle uses the package default", &Router{}, CycleTimeout + pickClaimGrace},
		{"nil router uses the package default", nil, CycleTimeout + pickClaimGrace},
		// 120s + 60s = 180s < 300s: the floor wins.
		{"short cycle keeps the 5min floor", &Router{CycleTimeout: 120 * time.Second}, floor},
		// The PR's own documented value: 2400s + 60s.
		{"raised cycle scales the claim", &Router{CycleTimeout: 2400 * time.Second}, 2460 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PickClaimTTL(tt.router)
			if got != tt.want {
				t.Errorf("PickClaimTTL() = %v, want %v", got, tt.want)
			}
			if cycle := CycleTimeoutFor(tt.router); got <= cycle {
				t.Errorf("PickClaimTTL() = %v does not outlive the %v cycle it protects", got, cycle)
			}
			if got < floor {
				t.Errorf("PickClaimTTL() = %v is below the %v crash-recovery floor", got, floor)
			}
		})
	}
}

// TestRunDreamCycleConsumesRouterCycleTimeout pins the FIRST of the three
// consumption sites — the resolver test above only exercises the pure
// function, so reverting dream.go's `context.WithTimeout(ctx,
// CycleTimeoutFor(r))` to the bare `CycleTimeout` constant leaves every other
// test in this package green. That is the one mutation which undoes the whole
// feature inside this package, so it gets its own gate (PR #23's
// TestTemporalReviewWireTimeout convention).
//
// DB-free, on router_test.go's dead-pool idiom: the pool points at a closed
// loopback port, so PickBlock — the first statement of the cycle — is the
// probe. With a 1ns router value the derived cycle context is already past
// its deadline when PickBlock dials, and the error carries
// context.DeadlineExceeded. With the value unset the 700s constant cannot
// have expired, so the same call must fail on the dial instead. The two
// outcomes are distinguishable only if the router value is read.
func TestRunDreamCycleConsumesRouterCycleTimeout(t *testing.T) {
	run := func(t *testing.T, cycle time.Duration) error {
		t.Helper()
		db, err := pgxpool.New(context.Background(),
			"postgres://ctx:documentation-value@127.0.0.1:1/ctx?sslmode=disable&connect_timeout=1")
		if err != nil {
			t.Fatalf("dead pool: %v", err)
		}
		t.Cleanup(db.Close)

		p := backends.NewPool(nil, nil)
		p.SeedSnapshotForTest([]backends.Backend{{
			ID: "row-dream", Name: "row-dream", Host: "http://dream.example",
			Trust: backends.TrustFull, Locality: "lan",
			Roles:    []string{backends.RoleDream},
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
			Priority: 100, Enabled: true,
		}})
		r := &Router{
			Pool: p, Admit: testAdmit(), Blocktypes: blocktype.NewRegistry(),
			CycleTimeout: cycle,
		}
		_, err = RunDreamCycle(context.Background(), db, r, llm.Options{},
			BackoffConfig{Mode: "off"}, []string{"private"}, NoThrottle)
		return err
	}

	if err := run(t, time.Nanosecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cycle timeout 1ns: err = %v, want context.DeadlineExceeded — the router value is not reaching the cycle context", err)
	}
	err := run(t, 0)
	if err == nil {
		t.Fatal("cycle timeout unset: err = nil, want the dead pool's dial error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cycle timeout unset: err = %v, want a dial error — the %v package default cannot have expired", err, CycleTimeout)
	}
}
