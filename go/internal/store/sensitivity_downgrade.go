package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
)

// SensitivityDowngradeError refuses an update that lowers a block's
// sensitivity without ConfirmSensitivityDowngrade, decided on the level the
// update's transaction holds the row at (issue #547). From is that level.
type SensitivityDowngradeError struct {
	From, To backends.Sensitivity
}

func (e *SensitivityDowngradeError) Error() string {
	return fmt.Sprintf("store: update block: sensitivity downgrade %s → %s needs its confirm", e.From, e.To)
}

// sensitivityPlan is the downgrade guard of an update that names a
// sensitivity, decided where the update's transaction holds the row (issue
// #547). The handler reads the level before the write for its early answer
// (sensitivityGuardReject); a raise committed between that read and the
// write let an update without the confirm lower the block with no audit
// entry, and a confirmed one recorded a level that no longer held. Under the
// row lock the decision is the one the write acts on:
//
//   - the new level ranks at or above the held one: no downgrade, no audit
//     entry (a proposal the handler sent is dropped);
//   - below it without the confirm: SensitivityDowngradeError, nothing
//     written;
//   - below it with the confirm: the audit entry the handler proposed (by,
//     at), with from and to as they are under the lock.
//
// The entry travels as an argument of the statement (arg in args): NULL keeps
// metadata as the rest of the update makes it (updateMetadataExpr).
type sensitivityPlan struct {
	to          backends.Sensitivity
	confirm     bool
	proposal    map[string]any
	arg         int
	writeScopes []string
	// report is UpdateBlockData.SensitivityDowngraded; held is the level the
	// last decision found under the lock.
	report func(from, to backends.Sensitivity)
	held   backends.Sensitivity
}

// lockSensitivitySQL locks the row an update writes — UpdateBlock's WHERE, the
// lock its UPDATE takes — and reads its level.
const lockSensitivitySQL = `SELECT sensitivity FROM context_blocks` + editTargetWhere + `
	 FOR NO KEY UPDATE`

// resolve locks the update's row, reads its level and decides (decide).
// ok=false (no error) is a miss: the update finds no row either.
func (p *sensitivityPlan) resolve(ctx context.Context, tx pgx.Tx, id string, args []any) (bool, error) {
	if p == nil {
		return true, nil
	}
	var held string
	err := tx.QueryRow(ctx, lockSensitivitySQL, id, p.writeScopes).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update block: lock sensitivity: %w", err)
	}
	if err := p.decide(backends.Sensitivity(held), args); err != nil {
		return false, err
	}
	return true, nil
}

// decide makes the decision on held, the level of the locked row, and sets
// the audit argument.
func (p *sensitivityPlan) decide(held backends.Sensitivity, args []any) error {
	args[p.arg] = nil
	p.held = held
	if p.to.Rank() >= held.Rank() {
		return nil
	}
	if !p.confirm {
		return &SensitivityDowngradeError{From: held, To: p.to}
	}
	audit := maps.Clone(p.proposal)
	if audit == nil {
		audit = map[string]any{"at": time.Now().UTC().Format(time.RFC3339)}
	}
	audit["from"] = string(held)
	audit["to"] = string(p.to)
	args[p.arg] = audit
	return nil
}

// reportDowngrade calls the plan's report once its update has committed
// (blockUpdate.handOver, after UpdateBlock's commit and after a block batch's)
// when the committed decision was a confirmed downgrade — its audit argument
// is set — with the level that held under the lock. A refused or rolled-back
// update never reaches it, and an update that turned out not to lower the
// block sets no audit argument: exactly one call per downgrade made.
func (p *sensitivityPlan) reportDowngrade(args []any) {
	if p == nil || p.report == nil || args[p.arg] == nil {
		return
	}
	p.report(p.held, p.to)
}
