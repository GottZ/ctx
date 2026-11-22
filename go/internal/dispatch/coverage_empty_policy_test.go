package dispatch

// Issue #504: the coverage WARN says that a local target without a slots cap
// keeps the policy from enforcing. With an empty policy — no row carries a
// dispatch key, as on a freshly seeded pool — nothing enforces whatever the
// coverage (Enforcing: enabled && len(Targets) > 0 && covered), so the line
// has nothing to say and is not logged. A non-empty policy with such a target
// still logs it, once per derivation. The coverage term itself is unchanged.

import (
	"log/slog"
	"strings"
	"testing"
)

func TestCoverageWarnOnlyForANonEmptyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     []BackendRow
		targets  int
		wantWarn []string // origins the WARN names
	}{
		{"a seeded loopback pool: no slots anywhere", enforcingRows(false, false, false), 0, nil},
		{"a seeded pool with an external row next to it", enforcingRows(false, false, true), 0, nil},
		{"slots on the chat target, the embed target uncapped", enforcingRows(true, false, false), 1, []string{"http://llama-embed:8081"}},
		{"slots on the embed target, the chat target uncapped", enforcingRows(false, true, false), 1, []string{"http://gpu:8089"}},
		{"every local target capped", enforcingRows(true, true, false), 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &attrHandler{}
			p := DerivePolicy(tc.rows, slog.New(h))
			if len(p.Targets) != tc.targets {
				t.Fatalf("policy targets = %d, want %d", len(p.Targets), tc.targets)
			}
			warns := h.matching(coverageWarn)
			if len(warns) != len(tc.wantWarn) {
				t.Fatalf("coverage WARN %d× %v, want %d naming %v", len(warns), warns, len(tc.wantWarn), tc.wantWarn)
			}
			for i, origin := range tc.wantWarn {
				if !strings.Contains(warns[i], origin) {
					t.Errorf("coverage WARN %q does not name %s", warns[i], origin)
				}
			}
			// The coverage term does not depend on the line: an uncapped local
			// target leaves it false with an empty policy too.
			if wantCovered := tc.name == "every local target capped"; p.covered != wantCovered {
				t.Errorf("covered = %v, want %v", p.covered, wantCovered)
			}
		})
	}
}
