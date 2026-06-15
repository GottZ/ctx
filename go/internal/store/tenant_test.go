package store_test

import (
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// RequireScopes (design/01 §5.4) is the fail-closed empty-scope guard: an empty
// scope slice is an ERROR (ErrNoScopes), never silently "all scopes". It mirrors
// the guard that today lives ONLY in rrf.Search (search.go:65-67) so the later
// fail-closed-hardening wave (T07) can spread it to SearchBlocks + the manage
// reads. T04 only DEFINES the helper; T07 wires it into the call sites.
//
// RED (without store/tenant.go): the package fails to COMPILE —
// store.RequireScopes / store.ErrNoScopes are undefined. That is the honest RED
// for a pure-Go wave (the symbol does not exist yet).
func TestRequireScopes(t *testing.T) {
	if err := store.RequireScopes(nil); !errors.Is(err, store.ErrNoScopes) {
		t.Errorf("RequireScopes(nil) = %v, want ErrNoScopes", err)
	}
	if err := store.RequireScopes([]string{}); !errors.Is(err, store.ErrNoScopes) {
		t.Errorf("RequireScopes([]) = %v, want ErrNoScopes", err)
	}
	if err := store.RequireScopes([]string{"private"}); err != nil {
		t.Errorf("RequireScopes([private]) = %v, want nil", err)
	}
	if err := store.RequireScopes([]string{"private", "shared"}); err != nil {
		t.Errorf("RequireScopes([private,shared]) = %v, want nil", err)
	}
}
