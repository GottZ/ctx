package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Guard for the commit-less exit of the handler bodies that answer the request
// themselves (DECISIONS.md K35/K37). answeredTx translates pgxdb.ErrRollback
// back into "fn has answered, leave the transaction alone" — and NOTHING in the
// behavioural suite notices when that translation is deleted: the response is
// already written by then, so the request still carries the status fn chose,
// and only a SECOND, superfluous 500 would follow it. This test is what makes
// the removal red.
//
// It runs without a container: answeredTx takes a pgxdb.Beginner, so the two
// methods a bracket actually touches — Commit and Rollback — are enough
// (same double as internal/events/tx_tail_test.go).

type answeredProbeTx struct {
	pgx.Tx // nil: any method beyond the two below panics, on purpose
	commitErr error
	commits   int
	rollbacks int
}

func (p *answeredProbeTx) Commit(context.Context) error   { p.commits++; return p.commitErr }
func (p *answeredProbeTx) Rollback(context.Context) error { p.rollbacks++; return nil }

type answeredProbeBeginner struct {
	tx       *answeredProbeTx
	beginErr error
	begins   int
}

func (b *answeredProbeBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	b.begins++
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

func TestAnsweredTxExits(t *testing.T) {
	errBegin := errors.New("begin boom")
	errCommit := errors.New("commit boom")

	cases := []struct {
		name string
		// setup
		beginErr  error
		commitErr error
		fnResult  bool
		// expectations
		wantOK        bool
		wantRan       bool
		wantCommits   int
		wantRollbacks int
		wantStage     string
		wantErr       error
	}{
		{
			name: "committed run leaves the response to the caller",
			// The deferred rollback still runs — after a commit it is a no-op.
			fnResult: true, wantOK: true, wantRan: true,
			wantCommits: 1, wantRollbacks: 1, wantStage: "",
		},
		{
			name: "fn answered: no commit, and fail is NOT called",
			// THE guard: without the errors.Is(err, pgxdb.ErrRollback) arm the
			// sentinel falls through to fail("commit", …) and the handler
			// writes a second response over the one fn just sent.
			fnResult: false, wantOK: false, wantRan: true,
			wantCommits: 0, wantRollbacks: 1, wantStage: "",
		},
		{
			name:     "failed BeginTx is reported as the begin stage",
			beginErr: errBegin, fnResult: true,
			wantOK: false, wantRan: false,
			wantCommits: 0, wantRollbacks: 0, wantStage: "begin", wantErr: errBegin,
		},
		{
			name:      "failed Commit is reported as the commit stage",
			commitErr: errCommit, fnResult: true,
			wantOK: false, wantRan: true,
			wantCommits: 1, wantRollbacks: 1, wantStage: "commit", wantErr: errCommit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &answeredProbeBeginner{
				tx:       &answeredProbeTx{commitErr: tc.commitErr},
				beginErr: tc.beginErr,
			}
			var (
				gotStage string
				gotErr   error
				fails    int
				ran      bool
			)
			ok := answeredTx(context.Background(), db,
				func(stage string, err error) { fails++; gotStage, gotErr = stage, err },
				func(pgx.Tx) bool { ran = true; return tc.fnResult })

			if ok != tc.wantOK {
				t.Errorf("answeredTx = %v, want %v", ok, tc.wantOK)
			}
			if ran != tc.wantRan {
				t.Errorf("fn ran = %v, want %v", ran, tc.wantRan)
			}
			if db.tx.commits != tc.wantCommits {
				t.Errorf("commits = %d, want %d", db.tx.commits, tc.wantCommits)
			}
			if db.tx.rollbacks != tc.wantRollbacks {
				t.Errorf("rollbacks = %d, want %d", db.tx.rollbacks, tc.wantRollbacks)
			}
			if tc.wantStage == "" {
				if fails != 0 {
					t.Errorf("fail called %d times with stage %q — the caller must not answer twice", fails, gotStage)
				}
				return
			}
			if fails != 1 {
				t.Fatalf("fail called %d times, want 1 (stage %q)", fails, tc.wantStage)
			}
			if gotStage != tc.wantStage {
				t.Errorf("stage = %q, want %q", gotStage, tc.wantStage)
			}
			if !errors.Is(gotErr, tc.wantErr) {
				t.Errorf("fail error = %v, want %v (unwrapped: Stages{} carries no label)", gotErr, tc.wantErr)
			}
		})
	}
}
