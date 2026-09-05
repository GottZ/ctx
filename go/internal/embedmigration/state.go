// Package embedmigration is the Evokoa-Clean-Room Achse 04 Re-Embed-
// Migration statemachine (design/04-reembed-migration.md). W04-3 scope:
// the CAS-checked status transitions (§4.1) and the create-time validation
// (§4.2, §4.10, §6.1) against migration 114's context_embed_migrations /
// context_embed_models tables. The actual scheduler arm that DOES the
// backfill (migrateOneEmbedding), the verify gate and the cutover/rollback
// transactions are later waves (W04-4/W04-5/W04-6) — this package only
// carries the state the whole lifecycle rests on, "Mechanismus = Code,
// Policy = Daten" (design §4).
package embedmigration

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Status mirrors context_embed_migrations.status (migration 114's CHECK
// constraint is the DB-side twin of this Go type — every value here MUST
// stay in lockstep with that constraint's value set).
type Status string

const (
	StatusPending    Status = "pending"
	StatusRunning    Status = "running"
	StatusPaused     Status = "paused"
	StatusVerifying  Status = "verifying"
	StatusDone       Status = "done"
	StatusAborted    Status = "aborted"
	StatusRolledBack Status = "rolled_back"
)

// allowedTransitions is the Statemachine diagram of design §4.1, expressed
// as data (not scattered if-chains) so IsAllowedTransition and Transition
// share one authoritative source. done/aborted/rolled_back are terminal
// except the one documented exception: done → rolled_back (post-cutover
// rollback, §4.10).
var allowedTransitions = map[Status][]Status{
	StatusPending:    {StatusRunning, StatusAborted},
	StatusRunning:    {StatusPaused, StatusVerifying, StatusAborted},
	StatusPaused:     {StatusRunning, StatusAborted},
	StatusVerifying:  {StatusRunning, StatusPaused, StatusDone, StatusAborted},
	StatusDone:       {StatusRolledBack},
	StatusAborted:    {},
	StatusRolledBack: {},
}

// IsAllowedTransition reports whether the statemachine permits from → to.
// Pure function, no DB — the CAS-checked Transition below is the only
// caller that turns this into a persisted state change, but the predicate
// itself is exported so callers (and tests) can reason about the diagram
// without touching a database.
func IsAllowedTransition(from, to Status) bool {
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

var (
	// ErrTransitionNotAllowed is returned by Transition when the statemachine
	// diagram itself forbids from→to — checked BEFORE any SQL runs, so an
	// illegal transition never touches the database.
	ErrTransitionNotAllowed = errors.New("embedmigration: transition not allowed by statemachine")
	// ErrTransitionRaceLost is returned when the CAS UPDATE affected zero
	// rows: the row's status had already moved away from `from` by the time
	// this UPDATE ran (a concurrent transition won the race), or the id does
	// not exist. Either way the caller's assumed starting state was stale —
	// never a silent double-transition (design §4.1: "0 rows affected =
	// verlorenes Rennen → Fehler, kein stiller Doppelübergang").
	ErrTransitionRaceLost = errors.New("embedmigration: CAS lost the race (status changed concurrently, or id not found)")
	// ErrReasonRequired is returned when a transition into aborted or
	// rolled_back is attempted without the mandatory reason option
	// (WithAbortReason / WithRollbackReason — design §4.1/§4.10/§5 Bruchpfad
	// 11: "rollback_reason Pflicht beim Übergang done→rolled_back").
	ErrReasonRequired = errors.New("embedmigration: reason is required for this transition")
)

// Querier is the full statement surface, satisfied by both *pgxpool.Pool and
// pgx.Tx, so Transition and Create can run standalone on the pool or inside a
// caller's transaction (e.g. the future cutover swap-tx, W04-6, wants the
// status CAS in the SAME tx as the column renames).
//
// The composition keeps its NAME because internal/events spells it in six
// signatures of its own (embed_cutover.go); the method signatures live once,
// in pgxdb.
type Querier interface {
	pgxdb.Execer
	pgxdb.Querier
	pgxdb.Rower
}

// TransitionOption sets an additional column atomically with the CAS
// UPDATE (e.g. abort_reason / rollback_reason). Options compose: each call
// appends one `col = $n` clause plus its positional argument.
type TransitionOption func(*transitionOpts)

type transitionOpts struct {
	cols         []string
	args         []any
	hasAbortReas bool
	hasRollbReas bool
}

// WithAbortReason sets abort_reason in the same UPDATE as the CAS — required
// whenever the target status is StatusAborted (design §5 Bruchpfad, abort
// leaves an audit trail, never a silent status flip).
func WithAbortReason(reason string) TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "abort_reason")
		o.args = append(o.args, reason)
		o.hasAbortReas = reason != ""
	}
}

// WithRollbackReason sets rollback_reason in the same UPDATE as the CAS —
// required whenever the target status is StatusRolledBack (design §4.10:
// "Pflicht-Feld rollback_reason").
func WithRollbackReason(reason string) TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "rollback_reason")
		o.args = append(o.args, reason)
		o.hasRollbReas = reason != ""
	}
}

// WithLastError sets last_error in the same UPDATE as the CAS — the
// migration worker's fail-closed pause path (design §4.2 Model-Guard:
// `running → paused` with `last_error = "model guard: resolved <X>,
// expected <Y>"`). The caller passes an ALREADY normalized message (the
// column's contract mirrors context_embed_failures.last_error, §3.2a —
// never raw wire bodies, never embed text).
func WithLastError(msg string) TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "last_error")
		o.args = append(o.args, msg)
	}
}

// WithVerifyStartedAt sets verify_started_at (the Watermark, §4.7) in the
// same UPDATE as the running → verifying CAS.
func WithVerifyStartedAt() TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "verify_started_at")
		o.args = append(o.args, sqlNow{})
	}
}

// WithVerifyReportCleared nulls verify_report in the same UPDATE as the
// running → verifying CAS (W04-5): a (re-)entry into verifying invalidates
// any previous gate result — the verify runner treats "status=verifying AND
// verify_report IS NULL" as its start condition, so clearing the report
// atomically with the CAS is what makes the paused→resume→verifying loop
// re-checkable (§4.7 idempotent re-run) instead of serving a stale verdict
// from the previous watermark.
func WithVerifyReportCleared() TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "verify_report")
		o.args = append(o.args, nil)
	}
}

// WithStartedAt sets started_at (pending → running CAS).
func WithStartedAt() TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "started_at")
		o.args = append(o.args, sqlNow{})
	}
}

// WithFinishedAt sets finished_at (→ done CAS).
func WithFinishedAt() TransitionOption {
	return func(o *transitionOpts) {
		o.cols = append(o.cols, "finished_at")
		o.args = append(o.args, sqlNow{})
	}
}

// sqlNow is a marker consumed by Transition's SQL builder to emit now()
// instead of a bound parameter — kept as a distinct type (not a bare bool
// or string sentinel) so it can never collide with a genuine caller value.
type sqlNow struct{}

// Transition performs the CAS-checked status change: `UPDATE
// context_embed_migrations SET status = $to [, extra cols...] WHERE id = $id
// AND status = $from`. It is the ONLY path that may change
// context_embed_migrations.status (design §4.1's "jeder Übergang eine CAS-
// Zeile" doctrine) — every other write to the row (counters, verify_report,
// cursor_created_at) is a plain UPDATE outside this function.
//
// Ordering: the statemachine check (IsAllowedTransition) and the mandatory-
// reason check both run BEFORE any SQL — an illegal or under-specified
// transition never reaches the database. Zero rows affected by the UPDATE
// itself means the row's status had already moved (or id is unknown) —
// ErrTransitionRaceLost, never a silent no-op.
func Transition(ctx context.Context, q Querier, id string, from, to Status, opts ...TransitionOption) error {
	if !IsAllowedTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrTransitionNotAllowed, from, to)
	}

	o := &transitionOpts{}
	for _, opt := range opts {
		opt(o)
	}
	if to == StatusAborted && !o.hasAbortReas {
		return fmt.Errorf("%w: aborted requires WithAbortReason", ErrReasonRequired)
	}
	if to == StatusRolledBack && !o.hasRollbReas {
		return fmt.Errorf("%w: rolled_back requires WithRollbackReason", ErrReasonRequired)
	}

	setClauses := "status = $1"
	args := []any{string(to)}
	for i, col := range o.cols {
		if _, isNow := o.args[i].(sqlNow); isNow {
			// now() is a SQL literal, not a bound parameter — no arg slot.
			setClauses += fmt.Sprintf(", %s = now()", col)
			continue
		}
		args = append(args, o.args[i])
		setClauses += fmt.Sprintf(", %s = $%d", col, len(args))
	}
	idPos := len(args) + 1
	fromPos := len(args) + 2
	args = append(args, id, string(from))

	sql := fmt.Sprintf(
		`UPDATE context_embed_migrations SET %s WHERE id = $%d::uuid AND status = $%d`,
		setClauses, idPos, fromPos,
	)
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("embedmigration: transition %s->%s: %w", from, to, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTransitionRaceLost
	}
	return nil
}
