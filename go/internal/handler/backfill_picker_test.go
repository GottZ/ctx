package handler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// fakeBackfillQueue stands in for the database under backfillPicker: the
// pending blocks in the order of the old end (a block stands where it was
// stored, one that overran the sync budget where it was cut), answering each
// pick by what its statement says: the order, the scope conjunct, and whether
// it reads the blocks that overran. It checks the rule that chooses the
// picks, not the SQL: the class conjunct is not evaluated here, and no
// statement is parsed by Postgres. The class arguments are checked for which
// set they are: wantCaller on a statement bound to the caller's scopes,
// wantAny on every other.
type fakeBackfillQueue struct {
	pending             []fakePendingBlock
	wantCaller, wantAny embedClasses
	scoped              int // statements bound to the caller's scopes
	stmts               int
}

type fakePendingBlock struct {
	id, scope string
	at        time.Time // place in the old end
	overran   bool      // carries a caller_timeout memo
	backoff   bool      // and is still inside its backoff
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
	*dest[5].(*time.Time) = r.block.at
	return nil
}

func (q *fakeBackfillQueue) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.stmts++
	// The statement that reads the blocks that overran joins their memo.
	overran := strings.Contains(sql, "JOIN context_embed_failures")
	newest := strings.Contains(sql, "ORDER BY "+backfillNewestFirst)
	if !newest && !strings.Contains(sql, backfillOldestFirst) {
		return fakeBackfillRow{err: errors.New("pick without one of the two orders")}
	}
	// Every other pick carries the query path's predicate, which names the
	// class and so keeps those blocks out.
	if !overran && !strings.Contains(sql, "caller_timeout") {
		return fakeBackfillRow{err: errors.New("pick of blocks without the query path's failure-memo predicate")}
	}
	var scopes []string
	scoped := strings.Contains(sql, backfillInScopes)
	want := q.wantAny
	if scoped {
		q.scoped++
		scopes = args[4].([]string)
		want = q.wantCaller
	}
	if got := (embedClasses{args[0].([]string), args[1].([]string), args[2].([]string), args[3].([]string)}); !reflect.DeepEqual(got, want) {
		return fakeBackfillRow{err: fmt.Errorf("pick (bound to the caller's scopes: %v) with the classes %+v, want %+v", scoped, got, want)}
	}
	var found *fakePendingBlock
	for i := range q.pending {
		b := &q.pending[i]
		if (scoped && !slices.Contains(scopes, b.scope)) || b.overran != overran || b.backoff {
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

// The pool, the floor and the caller of the picker tests: a shared embed
// backend that takes no credentials, `own` with a full-trust backend of its
// own and no floor, and `lifted`, floored above the shared ceiling. The
// caller reads none of the two, so the classes of its scopes and the classes
// of every scope differ in both, and each differs from what it would be
// without the floor.
func pickerFixture() (backends.TrustCeilings, config.ScopeFloor, []string) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		{ID: "g", Name: "shared-nocred", Scope: backends.GlobalScope, Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustNoCredentials},
		{ID: "o", Name: "own-full", Scope: "own", Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustFull},
	})
	return p.TrustCeilings(backends.RoleEmbed), config.ScopeFloor{"lifted": backends.SensCredentials}, []string{"private", "shared"}
}

// drain runs the loop of backfillPending over the queue: one next per pick
// until the cap or the first error, every picked block embedded (removed).
// The picker is the one backfillPending builds.
func (q *fakeBackfillQueue) drain(t *testing.T, armRuns bool, syncCap int) []string {
	t.Helper()
	ceilings, floor, readScopes := pickerFixture()
	q.wantCaller = embedServableClasses(ceilings, floor, readScopes)
	q.wantAny = embedServableClasses(ceilings, floor, nil)
	if reflect.DeepEqual(q.wantCaller, q.wantAny) || reflect.DeepEqual(q.wantAny, embedServableClasses(ceilings, nil, nil)) {
		t.Fatalf("fixture: the classes of the caller's scopes %+v, of every scope %+v and of every scope without the floor must differ", q.wantCaller, q.wantAny)
	}
	p := newBackfillPicker(q, armRuns, ceilings, floor, readScopes)
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
// a queue written in the order of the old end: f* are blocks of a scope the
// caller cannot read, c* the caller's. A trailing ! marks a block that
// overran the sync budget of an earlier request and has sat out its backoff;
// it stands where it was cut, so "c1 c2! c3" says c2 was cut after c1 was
// stored and before c3 was. A trailing ~ marks one still inside its backoff.
func TestBackfillPickerOrder(t *testing.T) {
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

		// Arm off, one pick: the oldest of any scope. Two statements: the
		// head of the blocks that overran, and the oldest of the others.
		{"arm_off_cap_1_oldest", "f1 c1 f2 c2", false, 1, "f1", 0, 2},
		{"arm_off_cap_1_oldest_is_the_callers", "c1 f1 c2", false, 1, "c1", 0, 2},
		// Arm off, two picks: the oldest, then the caller's newest. The old
		// end has the first pick, which is the one with the whole budget.
		{"arm_off_cap_2_both_ends", "c1 c2 c3", false, 2, "c1 c3", 1, 3},
		{"arm_off_cap_2_old_end_is_unscoped", "f1 c1 c2", false, 2, "f1 c2", 1, 3},
		{"arm_off_cap_2_old_end_first", "f1 c1 f2", false, 2, "f1 c1", 1, 3},
		// Arm off, no cap: the ends alternate until the queue is empty.
		{"arm_off_uncapped_alternates", "f1 c1 c2 c3 f2", false, 0, "f1 c3 c1 c2 f2", -1, 0},
		// Arm off, nothing for the caller: the scoped pick runs once, and
		// the blocks that overran are read once while there are none.
		{"arm_off_caller_drained_scoped_once", "f1 f2 f3", false, 0, "f1 f2 f3", 1, 6},
		{"arm_off_nothing_pending", "", false, 0, "", 0, 2},
		{"arm_off_cap_4_caller_drained_midway", "f1 f2 f3 c1", false, 4, "f1 c1 f2 f3", 2, 0},

		// Arm off: a block that overran the budget and has sat out its
		// backoff stands in the old end where it was cut. Fresh writes behind
		// it do not keep it waiting.
		{"arm_off_cap_1_overran_ahead_of_later_writes", "c1! c2", false, 1, "c1!", 0, 2},
		{"arm_off_cap_1_overran_of_another_scope", "f1! c1 c2", false, 1, "f1!", 0, 2},
		{"arm_off_cap_2_overran_then_the_callers_newest", "c1! f1 c2", false, 2, "c1! c2", 1, 3},
		{"arm_off_overran_oldest_first", "c1! c2 f1", false, 0, "c1! c2 f1", -1, 0},
		{"arm_off_cap_1_only_overran", "c1!", false, 1, "c1!", 0, 2},
		// ... and behind what was pending when it was cut.
		{"arm_off_cap_1_overran_behind_what_waited_at_the_cut", "f1 c1! c2", false, 1, "f1", 0, 2},
		{"arm_off_overran_in_its_place", "c1 c2! f1", false, 0, "c1 c2! f1", -1, 0},
		// Several: by their cuts. The head is read anew after each one
		// taken, and once more when none is left.
		{"arm_off_several_overran_by_their_cuts", "f1! c1! c2", false, 0, "f1! c2 c1!", -1, 0},
		{"arm_off_cap_3_overran_are_read_anew_once_taken", "f1! f2! f3", false, 3, "f1! f2! f3", 1, 7},
		// Inside its backoff a block is nobody's pick.
		{"arm_off_inside_the_backoff_waits", "c1~ c2", false, 0, "c2", -1, 0},
		{"arm_off_only_inside_the_backoff", "c1~", false, 0, "", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeBackfillQueue{}
			for i, id := range strings.Fields(tc.queue) {
				scope := "private"
				if strings.HasPrefix(id, "f") {
					scope = "foreign"
				}
				q.pending = append(q.pending, fakePendingBlock{
					id: id, scope: scope, at: time.Unix(int64(i), 0),
					overran: strings.HasSuffix(id, "!") || strings.HasSuffix(id, "~"), backoff: strings.HasSuffix(id, "~"),
				})
			}
			got := strings.Join(q.drain(t, tc.armRuns, tc.syncCap), " ")
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
