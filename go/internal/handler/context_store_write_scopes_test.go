package handler

import (
	"reflect"
	"sort"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// writableBlockScopes is the SINGLE eval point of the block-write gate (078, E4b).
// These pure tests pin the two invariants the W3 formula must hold:
//
//	writableBlockScopes = [home] ∪ (write_scopes ∩ (allowed ∪ {home})) ∪ {shared-if-allowed}
//
//   - COMPAT (pausability): an empty/nil write_scopes reproduces v4.2.x EXACTLY —
//     home_scope, plus 'shared' only when it is in allowed_scopes. No DB needed.
//   - INTERSECTION (gate b, fail-closed): a write_scope with a read right widens the
//     set; a STALE write_scope (left behind by a later allowed_scopes shrink) is
//     neutralised at THIS point, so the gate never trusts the raw column.
//
// RED (naive append, no intersection): TestWritableBlockScopes_StaleNeutralised fails
// — 'work' leaks into the writable set though it is no longer readable. GREEN once the
// formula intersects with (allowed ∪ home).

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestWritableBlockScopes_CompatEmptyWriteScopes(t *testing.T) {
	cases := []struct {
		name    string
		ar      *auth.AuthResult
		want    []string
	}{
		{
			name: "home only, no shared",
			ar:   &auth.AuthResult{HomeScope: "private", AllowedScopes: []string{"work"}},
			want: []string{"private"},
		},
		{
			name: "home + shared when allowed",
			ar:   &auth.AuthResult{HomeScope: "private", AllowedScopes: []string{"shared", "work"}},
			want: []string{"private", "shared"},
		},
		{
			name: "nil write_scopes is byte-identical to pre-078",
			ar:   &auth.AuthResult{HomeScope: "crag", AllowedScopes: nil, WriteScopes: nil},
			want: []string{"crag"},
		},
		{
			name: "empty write_scopes is byte-identical to pre-078",
			ar:   &auth.AuthResult{HomeScope: "crag", AllowedScopes: []string{"shared"}, WriteScopes: []string{}},
			want: []string{"crag", "shared"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := writableBlockScopes(c.ar)
			if !reflect.DeepEqual(sortedCopy(got), sortedCopy(c.want)) {
				t.Errorf("writableBlockScopes = %v, want %v", got, c.want)
			}
			// home_scope MUST be the first element (the minimal-view invariant the
			// downstream write gate relies on — it is never empty).
			if len(got) == 0 || got[0] != c.ar.HomeScope {
				t.Errorf("writableBlockScopes[0] = %v, want home_scope %q", got, c.ar.HomeScope)
			}
		})
	}
}

func TestWritableBlockScopes_ValidWriteScopeWidens(t *testing.T) {
	// 'work' is both allowed (readable) and a write_scope → it enters the writable set.
	ar := &auth.AuthResult{
		HomeScope:     "private",
		AllowedScopes: []string{"shared", "work"},
		WriteScopes:   []string{"work"},
	}
	got := writableBlockScopes(ar)
	if !contains(got, "work") {
		t.Fatalf("writableBlockScopes = %v, want it to include 'work' (write ∩ allowed)", got)
	}
	if !contains(got, "private") || !contains(got, "shared") {
		t.Errorf("writableBlockScopes = %v, want home 'private' + 'shared' too", got)
	}
}

func TestWritableBlockScopes_StaleNeutralised(t *testing.T) {
	// GATE (b): 'work' is a write_scope but NO LONGER in allowed_scopes (an
	// allowed_scopes shrink left it stale). The intersection with (allowed ∪ home)
	// MUST drop it — the raw column is never trusted. RED under a naive append.
	ar := &auth.AuthResult{
		HomeScope:     "private",
		AllowedScopes: []string{"shared"}, // 'work' was removed
		WriteScopes:   []string{"work"},   // stale entry survives in the column
	}
	got := writableBlockScopes(ar)
	if contains(got, "work") {
		t.Fatalf("stale write_scope 'work' leaked into writable set %v — gate is NOT fail-closed", got)
	}
	// The compat baseline (home + shared) must remain intact.
	if !reflect.DeepEqual(sortedCopy(got), []string{"private", "shared"}) {
		t.Errorf("writableBlockScopes = %v, want [private shared] (stale 'work' dropped)", got)
	}
}
