package modelalias

// Pin of Fremd-Review 2 F2-4 S10 (Härtung W02-07, Bau-Bericht Abweichung 6):
// Snapshot.Shadowed decides by presence — a disabled and an invalid _global
// alias shadow as well, so their sensitivity stays the floor of a tenant
// alias of the same name (design/02 §4.4.2).

import (
	"context"
	"testing"
)

func TestShadowedByPresenceDisabledAndInvalid(t *testing.T) {
	r, mock, _ := newTestRegistry(t)
	mock.ExpectQuery(loadQuery).WillReturnRows(mockRows(
		row{"00000000-0000-4000-8000-00000000a510", "_global", "off", "chat", `{}`, false},
		row{"00000000-0000-4000-8000-00000000a511", "_global", "broken", "chat", `{"reasoning":"on"}`, true},
		row{"00000000-0000-4000-8000-00000000a512", "_global", "brokenoff", "chat", `{"reasoning":"on"}`, false},
		row{"00000000-0000-4000-8000-00000000a513", "_global", "live", "chat", `{}`, true},
		row{"00000000-0000-4000-8000-00000000a514", "acme:main", "own", "chat", `{}`, true},
	))
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := r.Snapshot()
	for _, c := range []struct {
		name             string
		enabled, invalid bool
	}{
		{"off", false, false},
		{"broken", true, true},
		{"brokenoff", false, true},
		{"live", true, false},
	} {
		a, ok := s.Shadowed(c.name)
		if !ok || a.Name != c.name {
			t.Errorf("Shadowed(%q) = %q, %v — want the _global alias (shadowing is by presence)", c.name, a.Name, ok)
			continue
		}
		if a.Enabled != c.enabled || (a.Invalid != nil) != c.invalid {
			t.Fatalf("fixture %q: enabled %v invalid %v, want %v/%v", c.name, a.Enabled, a.Invalid, c.enabled, c.invalid)
		}
	}
	for _, name := range []string{"own", "nope"} {
		if a, ok := s.Shadowed(name); ok {
			t.Errorf("Shadowed(%q) = %s/%s, want none (no _global alias of that name)", name, a.Scope, a.Name)
		}
	}
}
