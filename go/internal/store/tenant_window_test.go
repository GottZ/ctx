package store

import (
	"slices"
	"testing"
)

// TestIntersectWindow pins the §4.4-(a) read-window clamp: read_scopes ∩ owned.
// A nil owned is the unbounded fallback (pre-MT / register error). A foreign
// read_scopes value (a tenant-overridable config the tenant does NOT own) is
// dropped — the cross-tenant-background-read gate. The default tenant's owned =
// {private,shared,work} intersected with the default read_scopes is byte-equal
// (AMENDMENT #3 invariance).
func TestIntersectWindow(t *testing.T) {
	cases := []struct {
		name       string
		readScopes []string
		owned      []string
		want       []string
	}{
		{
			name:       "nil_owned_is_unbounded_fallback",
			readScopes: []string{"private", "shared", "work"},
			owned:      nil,
			want:       []string{"private", "shared", "work"},
		},
		{
			name:       "default_invariant_byte_equal",
			readScopes: []string{"private", "shared", "work"},
			owned:      []string{"private", "shared", "work"},
			want:       []string{"private", "shared", "work"},
		},
		{
			name:       "foreign_read_scope_override_intersects_away",
			readScopes: []string{"tenant-a", "foreign-x"}, // override claims a foreign scope
			owned:      []string{"tenant-a"},
			want:       []string{"tenant-a"}, // foreign-x dropped, leak closed
		},
		{
			name:       "empty_intersection_fails_closed",
			readScopes: []string{"foreign-x"},
			owned:      []string{"tenant-a"},
			want:       []string{}, // reads nothing rather than a foreign scope
		},
		{
			name:       "order_follows_read_scopes",
			readScopes: []string{"work", "private", "shared"},
			owned:      []string{"private", "shared", "work"},
			want:       []string{"work", "private", "shared"},
		},
		{
			// GHSA §5: ctx_auth folds a scope ANOTHER tenant granted into the
			// key's read_scopes; the clamp drops it, so nothing derived from it
			// lands in a block under the grantee's own scope.
			name:       "cross_tenant_grant_in_key_read_scopes_dropped",
			readScopes: []string{"tenant-a", "tenant-b-granted"},
			owned:      []string{"tenant-a"},
			want:       []string{"tenant-a"},
		},
		{
			// A resolved tenant owning nothing is a NON-nil empty owned: it
			// clamps to nothing instead of taking the unbounded nil arm.
			name:       "empty_non_nil_owned_clamps_to_nothing",
			readScopes: []string{"tenant-a", "tenant-b-granted"},
			owned:      []string{},
			want:       []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IntersectWindow(tc.readScopes, tc.owned)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("IntersectWindow(%v, %v) = %v, want %v", tc.readScopes, tc.owned, got, tc.want)
			}
		})
	}
}
