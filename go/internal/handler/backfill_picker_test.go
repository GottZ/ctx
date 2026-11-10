package handler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeBackfillQueue stands in for the database under backfillPicker: a queue
// of pending blocks, oldest first, that answers the pick by what its
// statement says (the order, the scope conjunct, which failure-memo
// predicate). It checks the rule that chooses the picks, not the SQL: the
// class conjunct is not evaluated here, and no statement is parsed by
// Postgres.
type fakeBackfillQueue struct {
	pending []fakePendingBlock
	scoped  int // statements bound to the caller's scopes
	stmts   int
}

type fakePendingBlock struct {
	id, scope string
	overran   bool // carries a caller_timeout memo
}

type fakeBackfillRow struct {
	block *fakePendingBlock
	err   error
}

func (r fakeBackfillRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.block.id
	*dest[4].(*string) = r.block.scope
	return nil
}

func (q *fakeBackfillQueue) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.stmts++
	newest := strings.Contains(sql, "ORDER BY "+backfillNewestFirst)
	if !newest && !strings.Contains(sql, "ORDER BY "+backfillOldestFirst) {
		return fakeBackfillRow{err: errors.New("pick without one of the two orders")}
	}
	var scopes []string
	scoped := strings.Contains(sql, backfillInScopes)
	if scoped {
		q.scoped++
		scopes = args[4].([]string)
	}
	// Only the query path's predicate names the class.
	skipOverran := strings.Contains(sql, "caller_timeout")
	var found *fakePendingBlock
	for i := range q.pending {
		b := &q.pending[i]
		if (scoped && !slices.Contains(scopes, b.scope)) || (skipOverran && b.overran) {
			continue
		}
		found = b
		if !newest {
			break
		}
	}
	if found == nil {
		return fakeBackfillRow{err: pgx.ErrNoRows}
	}
	return fakeBackfillRow{block: found}
}

// drain runs the loop of backfillPending over the queue: one next per pick
// until the cap or the first error, every picked block embedded (removed).
func (q *fakeBackfillQueue) drain(t *testing.T, armRuns bool, syncCap int, readScopes []string) []string {
	t.Helper()
	p := &backfillPicker{
		db: q, armRuns: armRuns, syncCap: syncCap, readScopes: readScopes,
		anyClasses: func() embedClasses { return embedClasses{} },
	}
	var got []string
	for attempt := 0; syncCap <= 0 || attempt < syncCap; attempt++ {
		row, err := p.next(context.Background(), attempt)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("pick %d: %v", attempt, err)
			}
			break
		}
		got = append(got, row.id)
		q.pending = slices.DeleteFunc(q.pending, func(b fakePendingBlock) bool { return b.id == row.id })
	}
	return got
}

// TestBackfillPickerOrder pins which block each pick of a request takes, on
// a queue written oldest first: f* are blocks of a scope the caller cannot
// read, c* the caller's, a trailing ! marks a block that overran the sync
// budget of an earlier request.
func TestBackfillPickerOrder(t *testing.T) {
	caller := []string{"private", "shared"}
	for _, tc := range []struct {
		name    string
		queue   string
		armRuns bool
		syncCap int
		want    string
		// scoped is the number of statements bound to the caller's scopes
		// (-1: not checked), stmts the number of pick statements (0: not
		// checked).
		scoped, stmts int
	}{
		// Arm on: the caller's scopes only, newest first; the rest is the arm's.
		{"arm_on_uncapped", "f1 c1 f2 c2 f3", true, 0, "c2 c1", -1, 3},
		{"arm_on_cap_1", "f1 c1 f2 c2 f3", true, 1, "c2", -1, 1},
		{"arm_on_nothing_for_the_caller", "f1 f2", true, 0, "", 1, 1},
		// Arm on: a block that overran the budget is the arm's.
		{"arm_on_overran_is_left", "c1! c2", true, 0, "c2", -1, 0},
		{"arm_on_only_overran", "c1!", true, 0, "", -1, 1},

		// Arm off, one pick: the oldest of any scope.
		{"arm_off_cap_1_oldest", "f1 c1 f2 c2", false, 1, "f1", 0, 1},
		{"arm_off_cap_1_oldest_is_the_callers", "c1 f1 c2", false, 1, "c1", 0, 1},
		// Arm off, two picks: the caller's newest, then the oldest.
		{"arm_off_cap_2_both_ends", "c1 c2 c3", false, 2, "c3 c1", 1, 2},
		{"arm_off_cap_2_old_end_is_unscoped", "f1 c1 c2", false, 2, "c2 f1", 1, 2},
		{"arm_off_cap_2_caller_first", "f1 c1 f2", false, 2, "c1 f1", 1, 2},
		// Arm off, no cap: the ends alternate until the queue is empty.
		{"arm_off_uncapped_alternates", "f1 c1 c2 c3 f2", false, 0, "c3 f1 c2 c1 f2", -1, 0},
		// Arm off, nothing for the caller: the scoped pick runs once.
		{"arm_off_caller_drained_scoped_once", "f1 f2 f3", false, 0, "f1 f2 f3", 1, 6},
		{"arm_off_nothing_pending", "", false, 0, "", 1, 3},
		{"arm_off_cap_4_caller_drained_midway", "f1 f2 f3 c1", false, 4, "c1 f1 f2 f3", 2, 0},

		// Arm off: a block that overran the budget goes last, wherever it
		// stands in the order.
		{"arm_off_overran_newest_goes_last", "c1 c2!", false, 0, "c1 c2!", -1, 0},
		{"arm_off_overran_oldest_goes_last", "c1! c2 f1", false, 0, "c2 f1 c1!", -1, 0},
		{"arm_off_cap_1_overran_oldest_waits", "c1! c2", false, 1, "c2", 0, 1},
		{"arm_off_cap_1_only_overran", "c1!", false, 1, "c1!", 0, 2},
		{"arm_off_cap_2_overran_waits_for_both_ends", "c1! f1 c2", false, 2, "c2 f1", 1, 2},
		{"arm_off_several_overran_oldest_first", "f1! c1! c2", false, 0, "c2 f1! c1!", -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeBackfillQueue{}
			for _, id := range strings.Fields(tc.queue) {
				scope := "private"
				if strings.HasPrefix(id, "f") {
					scope = "foreign"
				}
				q.pending = append(q.pending, fakePendingBlock{id: id, scope: scope, overran: strings.HasSuffix(id, "!")})
			}
			got := strings.Join(q.drain(t, tc.armRuns, tc.syncCap, caller), " ")
			if got != tc.want {
				t.Errorf("queue %q, arm on %v, sync_cap %d: picked %q, want %q", tc.queue, tc.armRuns, tc.syncCap, got, tc.want)
			}
			if tc.scoped >= 0 && q.scoped != tc.scoped {
				t.Errorf("%d statements bound to the caller's scopes, want %d", q.scoped, tc.scoped)
			}
			if tc.stmts > 0 && q.stmts != tc.stmts {
				t.Errorf("%d pick statements, want %d", q.stmts, tc.stmts)
			}
		})
	}
}
