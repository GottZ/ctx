package cli

import "testing"

func TestSplitGuardResolveArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantRes    string
		wantIDs    int
		wantErr    bool
	}{
		{"documented form id-first", []string{"abc", "keep"}, "keep", 1, false},
		{"xargs form resolution-first", []string{"archive", "a", "b", "c"}, "archive", 3, false},
		{"resolution in the middle", []string{"a", "keep", "b"}, "keep", 2, false},
		{"missing resolution", []string{"a", "b"}, "", 0, true},
		{"double resolution", []string{"keep", "archive", "a"}, "", 0, true},
		{"resolution only", []string{"keep", "archive"}, "", 0, true},
		{"no ids", []string{"keep"}, "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ids, err := splitGuardResolveArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got resolution=%q ids=%v", res, ids)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res != tc.wantRes {
				t.Errorf("resolution = %q, want %q", res, tc.wantRes)
			}
			if len(ids) != tc.wantIDs {
				t.Errorf("len(ids) = %d, want %d", len(ids), tc.wantIDs)
			}
		})
	}
}
