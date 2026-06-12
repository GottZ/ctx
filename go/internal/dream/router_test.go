package dream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRunDreamCycle_EmptyChainSkipsBeforePick pins the G28 cycle-skip
// semantics (design 03 §2.4 dream row): when no backend serves the dream
// role, the cycle skips BEFORE PickBlock — no block claim, no cooldown
// touch. The DB pool points at a closed loopback port, so ANY statement
// (PickBlock's claiming UPDATE included) would return an error; (0, nil)
// therefore proves zero database contact. Negatively probed: without the
// step-0 skip the pick runs first and the cycle returns its error.
func TestRunDreamCycle_EmptyChainSkipsBeforePick(t *testing.T) {
	db, err := pgxpool.New(context.Background(),
		"postgres://ctx:documentation-value@127.0.0.1:1/ctx?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	t.Cleanup(db.Close)

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{}) // no dream backend at all

	n, err := RunDreamCycle(context.Background(), db, &Router{Pool: p},
		llm.Options{}, BackoffConfig{Mode: "off"}, []string{"private"}, NoThrottle)
	if err != nil {
		t.Fatalf("empty chain must skip silently (idle wait), got error: %v", err)
	}
	if n != 0 {
		t.Errorf("links created = %d, want 0", n)
	}
}

// TestEvaluateRelationships_TrustGate_NoWireCall pins the structural trust
// gate on the dream eval (G28): the chain resolves at max over source AND
// candidates, so a credentials-class block (explicit or zero-value) against
// a no-credentials backend yields an empty chain — the prompt is never
// built, the seam never called, and the error is *ErrNoEligibleBackend.
// Negatively probed: without the per-call chain the seam fires regardless.
func TestEvaluateRelationships_TrustGate_NoWireCall(t *testing.T) {
	called := false
	mockChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		called = true
		return constResp(`[]`)(nil, "", "", "", nil, "", "", llm.Options{}, 0)
	})

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "nc-backend", Name: "nc-backend", Host: "h",
		Trust: backends.TrustNoCredentials, Locality: "external",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Priority: 100, Enabled: true,
	}})
	r := &Router{Pool: p}

	assertBlocked := func(name string, source BlockInfo, cand BlockInfo) {
		t.Helper()
		called = false
		_, err := EvaluateRelationships(context.Background(), nil, r, llm.Options{}, source, []BlockInfo{cand})
		var noEligible *backends.ErrNoEligibleBackend
		if !errors.As(err, &noEligible) {
			t.Fatalf("%s: want *ErrNoEligibleBackend, got %v", name, err)
		}
		if called {
			t.Errorf("%s: trust-excluded backend must never see the prompt", name)
		}
	}

	// Source explicitly credentials.
	src := srcBlock(uuidA)
	src.Sensitivity = backends.SensCredentials
	cand := candBlock(uuidB)
	cand.Sensitivity = backends.SensInternal
	assertBlocked("credentials source", src, cand)

	// Candidate with ZERO-VALUE sensitivity raises the fold to credentials
	// (fail-closed) even though the source is internal.
	src.Sensitivity = backends.SensInternal
	cand.Sensitivity = ""
	assertBlocked("zero-value candidate", src, cand)

	// Both internal: the no-credentials backend qualifies and the call runs.
	cand.Sensitivity = backends.SensInternal
	called = false
	if _, err := EvaluateRelationships(context.Background(), nil, r, llm.Options{}, src, []BlockInfo{cand}); err != nil {
		t.Fatalf("internal/internal: %v", err)
	}
	if !called {
		t.Error("internal content must reach the no-credentials backend")
	}
}
