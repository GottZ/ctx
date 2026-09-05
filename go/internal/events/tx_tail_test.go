package events

// txTail-Wächter (T04-4c): end/wrap/done tragen die Commit-Texte und die
// Post-Commit-Logs der Mehrfach-Commit-Arme aus pgxdb.Write heraus. Rot bei:
// wrap gibt err roh zurück (Text ohne Präfix), end liefert nicht nil (kein
// Commit), done läuft auf dem Fehlerpfad. Läuft ohne Container (-short).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
)

type tailProbeTx struct {
	pgx.Tx
	commitErr error
	commits   int
	rollbacks int
}

func (p *tailProbeTx) Commit(context.Context) error   { p.commits++; return p.commitErr }
func (p *tailProbeTx) Rollback(context.Context) error { p.rollbacks++; return nil }

type tailProbeBeginner struct {
	beginErr error
	tx       *tailProbeTx
}

func (b *tailProbeBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

func TestTxTailCarriesCommitLabelAndPostCommit(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("store: pending write not found")
	boom := errors.New("connection refused")
	rows := []struct {
		name                string
		beginErr, commitErr error
		fn                  func(tail *txTail, post func()) error
		wantText            string
		wantIs              error
		wantPost            bool
		wantCommits         int
		wantRollbacks       int
	}{
		{"begin", boom, nil, func(tail *txTail, post func()) error { return tail.end("backfill: commit", post) },
			"backfill: begin tx: connection refused", boom, false, 0, 0},
		{"fn-sentinel", nil, nil, func(*txTail, func()) error { return sentinel },
			"store: pending write not found", sentinel, false, 0, 1},
		{"pick-released", nil, nil, func(*txTail, func()) error { return errPickReleased },
			"", nil, false, 0, 1},
		{"commit", nil, boom, func(tail *txTail, post func()) error { return tail.end("backfill: commit", post) },
			"backfill: commit: connection refused", boom, false, 1, 1},
		{"commit-memo", nil, boom, func(tail *txTail, post func()) error { return tail.end("backfill: commit oversize memo", post) },
			"backfill: commit oversize memo: connection refused", boom, false, 1, 1},
		{"success", nil, nil, func(tail *txTail, post func()) error { return tail.end("backfill: commit", post) },
			"", nil, true, 1, 1},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			tx := &tailProbeTx{commitErr: r.commitErr}
			var tail txTail
			posted := false
			err := pgxdb.Write(ctx, &tailProbeBeginner{beginErr: r.beginErr, tx: tx},
				pgxdb.Stages{Begin: "backfill: begin tx"}, func(pgx.Tx) error {
					return r.fn(&tail, func() { posted = true })
				})
			// die Aufrufer-Sequenz aus backfillOneEmbedding / migrateOneEmbedding
			switch {
			case errors.Is(err, errPickReleased):
				err = nil
			case err != nil:
				err = tail.wrap(err)
			default:
				tail.done()
			}
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != r.wantText {
				t.Errorf("text = %q, want %q", got, r.wantText)
			}
			if r.wantIs != nil && !errors.Is(err, r.wantIs) {
				t.Errorf("errors.Is(%v) fehlt", r.wantIs)
			}
			if posted != r.wantPost {
				t.Errorf("postCommit lief=%v, want %v", posted, r.wantPost)
			}
			if tx.commits != r.wantCommits || tx.rollbacks != r.wantRollbacks {
				t.Errorf("commits/rollbacks = %d/%d, want %d/%d", tx.commits, tx.rollbacks, r.wantCommits, r.wantRollbacks)
			}
		})
	}
	// Form-Identität mit der abgelösten Klammer: fmt.Errorf("<text>: %w", cerr)
	old := fmt.Errorf("backfill: commit oversize memo: %w", boom)
	var tail txTail
	_ = tail.end("backfill: commit oversize memo", nil)
	neu := tail.wrap(boom)
	// Die Unwrap-Stufe wird über errors.Is geprüft statt über == : boom ist
	// ein errors.New-Wert, für den beide Formen dasselbe aussagen, und der
	// Vergleich zweier error-Interfaces mit != ist ein errorlint-Befund.
	if old.Error() != neu.Error() || fmt.Sprintf("%T", old) != fmt.Sprintf("%T", neu) ||
		!errors.Is(errors.Unwrap(old), boom) || !errors.Is(errors.Unwrap(neu), boom) {
		t.Errorf("alt/neu verschieden: %s vs %s", old, neu)
	}
}
