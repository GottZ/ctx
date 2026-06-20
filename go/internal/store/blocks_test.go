package store

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

// --- ClampLimit ---.

func TestClampLimit_ZeroReturnsDefault(t *testing.T) {
	if got := ClampLimit(0, 10, 50); got != 10 {
		t.Errorf("ClampLimit(0, 10, 50) = %d, want 10", got)
	}
}

func TestClampLimit_NegativeReturnsDefault(t *testing.T) {
	if got := ClampLimit(-1, 10, 50); got != 10 {
		t.Errorf("ClampLimit(-1, 10, 50) = %d, want 10", got)
	}
}

func TestClampLimit_MinIntReturnsDefault(t *testing.T) {
	if got := ClampLimit(math.MinInt, 10, 50); got != 10 {
		t.Errorf("ClampLimit(MinInt, 10, 50) = %d, want 10", got)
	}
}

func TestClampLimit_OneReturnsSelf(t *testing.T) {
	if got := ClampLimit(1, 10, 50); got != 1 {
		t.Errorf("ClampLimit(1, 10, 50) = %d, want 1", got)
	}
}

func TestClampLimit_WithinRange(t *testing.T) {
	if got := ClampLimit(25, 10, 50); got != 25 {
		t.Errorf("ClampLimit(25, 10, 50) = %d, want 25", got)
	}
}

func TestClampLimit_ExactlyMax(t *testing.T) {
	if got := ClampLimit(50, 10, 50); got != 50 {
		t.Errorf("ClampLimit(50, 10, 50) = %d, want 50", got)
	}
}

func TestClampLimit_ExceedsMax(t *testing.T) {
	if got := ClampLimit(51, 10, 50); got != 50 {
		t.Errorf("ClampLimit(51, 10, 50) = %d, want 50", got)
	}
}

func TestClampLimit_MaxIntClampsToMax(t *testing.T) {
	if got := ClampLimit(math.MaxInt, 10, 50); got != 50 {
		t.Errorf("ClampLimit(MaxInt, 10, 50) = %d, want 50", got)
	}
}

func TestClampLimit_DefaultEqualsMax(t *testing.T) {
	if got := ClampLimit(0, 50, 50); got != 50 {
		t.Errorf("ClampLimit(0, 50, 50) = %d, want 50", got)
	}
}

func TestClampLimit_DefaultExceedsMax(t *testing.T) {
	// Edge case: defaultVal > maxVal. Function returns defaultVal since limit <= 0.
	// This is a design decision — ClampLimit does not clamp the default.
	if got := ClampLimit(0, 100, 50); got != 100 {
		t.Errorf("ClampLimit(0, 100, 50) = %d, want 100 (default returned as-is)", got)
	}
}

func TestClampLimit_AllZeros(t *testing.T) {
	// limit=0 returns default=0.
	if got := ClampLimit(0, 0, 0); got != 0 {
		t.Errorf("ClampLimit(0, 0, 0) = %d, want 0", got)
	}
}

func TestClampLimit_LimitEqualsDefault(t *testing.T) {
	if got := ClampLimit(10, 10, 50); got != 10 {
		t.Errorf("ClampLimit(10, 10, 50) = %d, want 10", got)
	}
}

// --- IsFullUUID ---.

func TestIsFullUUID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"019e446c-008f-7fbc-baa9-070bbe2086e8", true},
		{"00000000-0000-0000-0000-000000000000", true},
		{"019e446c-008f-7fbc-baa9-070bbe2086e", false},  // 35 chars
		{"019e446c-008f-7fbc-baa9-070bbe2086e88", false}, // 37 chars
		{"019e446c008f7fbcbaa9070bbe2086e80000", false}, // 36 chars, no dashes
		{"019e446c-008f-7fbc-baa9_070bbe2086e8", false},  // wrong separator at pos 23
		{"", false},
		{"019e446c", false},
	}
	for _, c := range cases {
		if got := IsFullUUID(c.in); got != c.want {
			t.Errorf("IsFullUUID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- ResolveBlockID — pool-free paths ---.

func TestResolveBlockID_FullUUIDBypassesDB(t *testing.T) {
	// Full UUID → returns immediately, pool not touched.
	id := "019e446c-008f-7fbc-baa9-070bbe2086e8"
	got, matches, err := ResolveBlockID(context.Background(), nil, id, []string{"private"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != id {
		t.Errorf("got %q, want %q", got, id)
	}
	if matches != nil {
		t.Errorf("expected nil matches for full uuid, got %+v", matches)
	}
}

func TestResolveBlockID_PrefixTooShortRejected(t *testing.T) {
	cases := []string{"", "abc", "019e44", "019e446"} // 0, 3, 6, 7 chars
	for _, in := range cases {
		_, _, err := ResolveBlockID(context.Background(), nil, in, []string{"private"}, nil)
		if err == nil {
			t.Errorf("ResolveBlockID(%q) expected error, got nil", in)
			continue
		}
		if !strings.Contains(err.Error(), "at least") {
			t.Errorf("ResolveBlockID(%q): unexpected error %v", in, err)
		}
		if errors.Is(err, ErrAmbiguousID) {
			t.Errorf("ResolveBlockID(%q): wrongly classified as ambiguous", in)
		}
	}
}

func TestResolveBlockID_MinPrefixLenSentinel(t *testing.T) {
	// MinIDPrefixLen is the contract — fix-knob if we ever change it. This
	// test asserts the constant directly so a silent edit to the source bumps
	// it into review.
	if MinIDPrefixLen != 8 {
		t.Errorf("MinIDPrefixLen = %d, want 8 (security-relevant default)", MinIDPrefixLen)
	}
}

// --- Integration test skeletons for DB-dependent functions ---.

func TestHashNOOPCheck_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: identical content hash returns existing block ID.
	// Test: different content returns empty string.
	// Test: archived block is not returned.
	// Test: wrong scope/category/title returns empty.
}

func TestUpsertBlock_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: insert new block returns populated Block.
	// Test: upsert existing (same category+title+scope) updates content.
	// Test: nil tags/metadata are set to empty defaults.
	// Test: scopeExplicit=true updates scope on conflict.
}

func TestSearchBlocks_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: FTS query returns matching blocks ranked by score.
	// Test: category filter limits results.
	// Test: tags filter with array overlap.
	// Test: compact=true returns preview instead of full content.
	// Test: empty query returns by updated_at DESC.
}

func TestGuardResolve_RequiresDB(t *testing.T) {
	t.Skip("requires database connection")
	// Test: "archive" sets is_archived=true and guard_status='archived_dup'.
	// Test: "keep" sets guard_status='active'.
	// Test: invalid resolution returns error.
	// Test: wrong scope returns nil (not found).
}
