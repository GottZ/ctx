package guard

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/blocktype"
)

// recordingPool is a mock pool whose matcher keeps the default regexp
// behaviour and additionally records every statement the code under test
// actually sent. A regexp expectation can only say what a statement must
// contain; the assertions below need the opposite, what it must NOT contain.
func recordingPool(t *testing.T) (pgxmock.PgxPoolIface, *[]string) {
	t.Helper()
	var sent []string
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherFunc(
		func(expectedSQL, actualSQL string) error {
			err := pgxmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
			if err == nil {
				sent = append(sent, actualSQL)
			}
			return err
		})))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	return mock, &sent
}

// blockUpdates returns the recorded UPDATE statements on context_blocks.
func blockUpdates(sent []string) []string {
	var out []string
	for _, s := range sent {
		if strings.HasPrefix(strings.TrimSpace(s), "UPDATE context_blocks SET") {
			out = append(out, s)
		}
	}
	return out
}

// TestApplyDecision_MarkCheckedLeavesUpdatedAt pins the statement the guard
// sends when it records a decision WITHOUT archiving (clean, needs_review, and
// possible_duplicate under flag mode): it writes guard_status and the guard_*
// metadata keys and does not assign updated_at.
//
// updated_at is the block's last write as every reader sees it: the browse
// order and its keyset cursor, age_days, the drift pin of a staged update. The
// guard's bookkeeping is none of those. The behaviour itself is pinned against
// a database in guard_updated_at_integration_test.go; this test is the half
// that runs without one.
func TestApplyDecision_MarkCheckedLeavesUpdatedAt(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		mode     string
	}{
		{"clean", "clean", blocktype.GuardModeArchive},
		{"needs_review", "needs_review", blocktype.GuardModeArchive},
		{"possible_duplicate_flag_mode", "near_duplicate", blocktype.GuardModeFlag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, sent := recordingPool(t)
			defer mock.Close()

			mock.ExpectBegin()
			mock.ExpectExec(`UPDATE context_blocks SET\s+guard_status`).
				WithArgs(anyArgs(8)...).
				WillReturnResult(pgxmock.NewResult("UPDATE", 1))

			tx, err := mock.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			matched := blockB
			res := &GuardResult{BlockID: blockA, Decision: tc.decision, Similarity: 0.95, MatchedID: &matched}
			if err := applyDecision(context.Background(), tx, blockA, res, tc.mode); err != nil {
				t.Fatalf("applyDecision: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}

			updates := blockUpdates(*sent)
			if len(updates) != 1 {
				t.Fatalf("recorded %d UPDATE statements on context_blocks, want 1: %q", len(updates), updates)
			}
			if !strings.Contains(updates[0], "'guard_checked_at'") {
				t.Errorf("mark-checked statement does not write guard_checked_at:\n%s", updates[0])
			}
			if strings.Contains(updates[0], "updated_at") {
				t.Errorf("mark-checked statement assigns updated_at; recording a guard decision is not a write to the block:\n%s", updates[0])
			}
		})
	}
}

// TestApplyDecision_AutoArchiveMovesUpdatedAt pins the other branch: an
// auto-archive takes the block out of every default read (is_archived), which
// is a change to the block a reader can see, so it keeps moving updated_at,
// like guard-resolve's archive does.
func TestApplyDecision_AutoArchiveMovesUpdatedAt(t *testing.T) {
	mock, sent := recordingPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE context_blocks SET\s+is_archived = true`).
		WithArgs(anyArgs(6)...).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	matched := blockB
	res := &GuardResult{BlockID: blockA, Decision: "near_duplicate", Similarity: 0.99, MatchedID: &matched}
	if err := applyDecision(context.Background(), tx, blockA, res, blocktype.GuardModeArchive); err != nil {
		t.Fatalf("applyDecision: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}

	updates := blockUpdates(*sent)
	if len(updates) != 1 {
		t.Fatalf("recorded %d UPDATE statements on context_blocks, want 1: %q", len(updates), updates)
	}
	if !strings.Contains(updates[0], "updated_at = now()") {
		t.Errorf("auto-archive statement no longer moves updated_at:\n%s", updates[0])
	}
}
