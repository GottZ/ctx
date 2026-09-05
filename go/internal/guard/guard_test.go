package guard

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/GottZ/ctx/internal/blocktype"
)

// testSet is the compiled-in builtin policy set — thresholds and allowlists
// identical to the M072 seeds, so the mocked flows exercise the seed-default
// behaviour (T7).
var testSet = blocktype.NewRegistry().Snapshot()

// --- Fixtures ---.

const (
	blockA = "019d0000-0000-7000-9000-00000000000a"
	blockB = "019d0000-0000-7000-9000-00000000000b"
	blockC = "019d0000-0000-7000-9000-00000000000c"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	return mock
}

func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// expectPendingFetch matches the FOR UPDATE SKIP LOCKED row-fetch and yields
// the given block IDs as candidates.
func expectPendingFetch(mock pgxmock.PgxPoolIface, ids ...string) {
	rows := mock.NewRows([]string{"id", "type_name"})
	for _, id := range ids {
		rows.AddRow(id, "knowledge")
	}
	mock.ExpectQuery(`SELECT id, type_name FROM context_blocks`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)
}

// expectGuardCheck mocks the ctx_guard_check call, returning a "clean"
// decision (no matched block, similarity 0.1).
func expectGuardCheck(mock pgxmock.PgxPoolIface, id string) {
	rows := mock.NewRows([]string{"decision", "top_similarity", "matched_id", "matched_title", "matched_scope", "is_cross_scope"}).
		AddRow("clean", 0.1, (*string)(nil), (*string)(nil), (*string)(nil), false)
	mock.ExpectQuery(`FROM ctx_guard_check`).
		WithArgs(id, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(rows)
}

// expectGuardCheckFail makes ctx_guard_check return an error for the given id.
func expectGuardCheckFail(mock pgxmock.PgxPoolIface, id string, sqlErr error) {
	mock.ExpectQuery(`FROM ctx_guard_check`).
		WithArgs(id, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sqlErr)
}

// expectApplyDecisionClean matches the UPDATE for clean/needs_review decisions
// (the else-branch in applyDecision: 7 args).
func expectApplyDecisionClean(mock pgxmock.PgxPoolIface) *pgxmock.ExpectedExec {
	return mock.ExpectExec(`UPDATE context_blocks SET\s+guard_status`).
		WithArgs(anyArgs(8)...).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

// expectAuditFetch matches the SELECT title/category/scope before writeAuditLog.
func expectAuditFetch(mock pgxmock.PgxPoolIface, id string) {
	rows := mock.NewRows([]string{"title", "category", "scope"}).
		AddRow("title", "decisions", "private")
	mock.ExpectQuery(`SELECT title, category, scope FROM context_blocks`).
		WithArgs(id).
		WillReturnRows(rows)
}

// expectAuditInsert matches the audit-log INSERT.
func expectAuditInsert(mock pgxmock.PgxPoolIface) *pgxmock.ExpectedExec {
	return mock.ExpectExec(`INSERT INTO context_write_log`).
		WithArgs(anyArgs(9)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

// expectGuardStateUpdate matches the final guard_state UPDATE.
func expectGuardStateUpdate(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`UPDATE context_guard_state`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

// expectSavepoint / expectRelease / expectRollback match the per-block
// savepoint statements.
func expectSavepoint(mock pgxmock.PgxPoolIface, id string) {
	sp := savepointName(id)
	mock.ExpectExec(`SAVEPOINT ` + sp + `$`).
		WillReturnResult(pgxmock.NewResult("SAVEPOINT", 0))
}

func expectRelease(mock pgxmock.PgxPoolIface, id string) {
	sp := savepointName(id)
	mock.ExpectExec(`RELEASE SAVEPOINT ` + sp).
		WillReturnResult(pgxmock.NewResult("RELEASE", 0))
}

func expectRollback(mock pgxmock.PgxPoolIface, id string) {
	sp := savepointName(id)
	mock.ExpectExec(`ROLLBACK TO SAVEPOINT ` + sp).
		WillReturnResult(pgxmock.NewResult("ROLLBACK", 0))
}

// expectFullBlockSuccess sets up the call sequence for one successfully
// processed block (savepoint → checkBlock → applyDecision → auditFetch →
// auditInsert → release).
func expectFullBlockSuccess(mock pgxmock.PgxPoolIface, id string) {
	expectSavepoint(mock, id)
	expectGuardCheck(mock, id)
	expectApplyDecisionClean(mock)
	expectAuditFetch(mock, id)
	expectAuditInsert(mock)
	expectRelease(mock, id)
}

// expectBlockCheckFail sets up the sequence for a block that fails at
// checkBlock: savepoint → failing check → rollback.
func expectBlockCheckFail(mock pgxmock.PgxPoolIface, id string, sqlErr error) {
	expectSavepoint(mock, id)
	expectGuardCheckFail(mock, id, sqlErr)
	expectRollback(mock, id)
}

// --- Tests ---.

func TestSavepointName_StripsHyphens(t *testing.T) {
	got := savepointName("019d0000-0000-7000-9000-00000000000a")
	want := "block_019d0000_0000_7000_9000_00000000000a"
	if got != want {
		t.Errorf("savepointName = %q, want %q", got, want)
	}
	if strings.Contains(got, "-") {
		t.Errorf("savepointName must not contain hyphens: %q", got)
	}
	if got[0] < 'a' || got[0] > 'z' {
		t.Errorf("savepointName must start with a letter: %q", got)
	}
	if len(got) > 63 {
		t.Errorf("savepointName must be ≤63 bytes (NAMEDATALEN): len=%d", len(got))
	}
}

// TestRunGuardBatch_NoPending verifies that the batch commits cleanly when no
// blocks need processing.
func TestRunGuardBatch_NoPending(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	// Empty result set.
	mock.ExpectQuery(`SELECT id, type_name FROM context_blocks`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows([]string{"id", "type_name"}))
	// State update for empty case.
	mock.ExpectExec(`UPDATE context_guard_state`).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_HappyPath_ThreeBlocks verifies that three blocks each get
// their own savepoint+release pair and are all counted as processed.
func TestRunGuardBatch_HappyPath_ThreeBlocks(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPendingFetch(mock, blockA, blockB, blockC)
	expectFullBlockSuccess(mock, blockA)
	expectFullBlockSuccess(mock, blockB)
	expectFullBlockSuccess(mock, blockC)
	expectGuardStateUpdate(mock)
	mock.ExpectCommit()

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 3 {
		t.Errorf("processed = %d, want 3", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_MiddleBlockFails_OthersProceed is the core regression
// test for W47-02: a SQL error on the middle block must not poison the tx,
// and the third block must still be processed.
func TestRunGuardBatch_MiddleBlockFails_OthersProceed(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPendingFetch(mock, blockA, blockB, blockC)

	// Block A: succeeds.
	expectFullBlockSuccess(mock, blockA)

	// Block B: checkBlock fails → savepoint rollback. Without W47-02 this
	// would leave the tx in aborted state and block C would never run.
	expectBlockCheckFail(mock, blockB, errors.New("simulated 25P02 trigger"))

	// Block C: succeeds, proving the tx is still usable after B's rollback.
	expectFullBlockSuccess(mock, blockC)

	expectGuardStateUpdate(mock)
	mock.ExpectCommit()

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only A and C count as processed; B was rolled back.
	if processed != 2 {
		t.Errorf("processed = %d, want 2 (A+C, B rolled back)", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_ApplyDecisionFails_RollsBack verifies that an error in
// applyDecision triggers ROLLBACK TO SAVEPOINT and the block is not counted.
func TestRunGuardBatch_ApplyDecisionFails_RollsBack(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPendingFetch(mock, blockA, blockB)

	// Block A: savepoint OK, check OK, applyDecision fails → rollback.
	expectSavepoint(mock, blockA)
	expectGuardCheck(mock, blockA)
	mock.ExpectExec(`UPDATE context_blocks SET\s+guard_status`).
		WithArgs(anyArgs(8)...).
		WillReturnError(errors.New("constraint violation"))
	expectRollback(mock, blockA)

	// Block B: succeeds.
	expectFullBlockSuccess(mock, blockB)

	expectGuardStateUpdate(mock)
	mock.ExpectCommit()

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1 (only B)", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_AuditLogFails_RollsBack verifies that an audit-log
// failure also triggers savepoint rollback (atomic-per-block semantics).
// The block is NOT counted as processed.
func TestRunGuardBatch_AuditLogFails_RollsBack(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPendingFetch(mock, blockA, blockB)

	// Block A: everything up to audit-insert succeeds, then INSERT fails.
	expectSavepoint(mock, blockA)
	expectGuardCheck(mock, blockA)
	expectApplyDecisionClean(mock)
	expectAuditFetch(mock, blockA)
	mock.ExpectExec(`INSERT INTO context_write_log`).
		WithArgs(anyArgs(9)...).
		WillReturnError(errors.New("audit log table full"))
	expectRollback(mock, blockA)

	// Block B: succeeds — proves tx is still usable.
	expectFullBlockSuccess(mock, blockB)

	expectGuardStateUpdate(mock)
	mock.ExpectCommit()

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1 (audit-failed A not counted, B counted)", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_SavepointCreateFails_SkipsBlock verifies that an error
// at SAVEPOINT-creation time skips the block without trying to roll back.
func TestRunGuardBatch_SavepointCreateFails_SkipsBlock(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPendingFetch(mock, blockA, blockB)

	// Block A: SAVEPOINT itself fails. No rollback (no savepoint to roll
	// back to), block is skipped, loop continues.
	mock.ExpectExec(`SAVEPOINT `+savepointName(blockA)).
		WillReturnError(errors.New("savepoint syntax error"))

	// Block B: succeeds.
	expectFullBlockSuccess(mock, blockB)

	expectGuardStateUpdate(mock)
	mock.ExpectCommit()

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_BeginTxFails surfaces the pool-begin error.
func TestRunGuardBatch_BeginTxFails(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

	processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if processed != 0 {
		t.Errorf("processed = %d, want 0", processed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_BatchCompleteLogOnlyWithWork pins WHERE the batch line is
// written: after the transaction closed successfully, and ONLY for a batch
// that actually picked blocks. The empty batch closes its transaction too (it
// clears the dirty state) but stays silent — the scheduler calls this per
// tick, so an unconditional line would turn an idle guard into one log record
// per tick at 1M+ blocks. Since the Tx rollout the closing sits inside the
// pgxdb helper and the line sits behind it, which is exactly the place a
// later edit can get wrong; this test goes red for both mistakes (silent when
// work happened, loud when none did).
func TestRunGuardBatch_BatchCompleteLogOnlyWithWork(t *testing.T) {
	capture := func(t *testing.T, setup func(pgxmock.PgxPoolIface), wantProcessed int) string {
		t.Helper()
		mock := newMockPool(t)
		defer mock.Close()
		setup(mock)

		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(prev)

		processed, err := RunGuardBatch(context.Background(), mock, testSet, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if processed != wantProcessed {
			t.Errorf("processed = %d, want %d", processed, wantProcessed)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("expectations: %v", err)
		}
		return buf.String()
	}

	const batchLine = "guard: batch complete"

	empty := capture(t, func(mock pgxmock.PgxPoolIface) {
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT id, type_name FROM context_blocks`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(mock.NewRows([]string{"id", "type_name"}))
		mock.ExpectExec(`UPDATE context_guard_state`).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		mock.ExpectCommit()
	}, 0)
	if strings.Contains(empty, batchLine) {
		t.Errorf("empty batch logged the batch line, want silence: %s", empty)
	}

	worked := capture(t, func(mock pgxmock.PgxPoolIface) {
		mock.ExpectBegin()
		expectPendingFetch(mock, blockA)
		expectFullBlockSuccess(mock, blockA)
		expectGuardStateUpdate(mock)
		mock.ExpectCommit()
	}, 1)
	if !strings.Contains(worked, batchLine) {
		t.Errorf("processed batch did not log the batch line: %s", worked)
	}
}

// Note: the context-cancellation path inside RunGuardBatch is unchanged
// from the pre-W47-02 code and cannot be tested deterministically with
// pgxmock because pgxmock methods race against ctx.Done() — BeginTx and
// Query randomly fire-or-skip depending on goroutine scheduling. A
// real-DB integration test (W47-03) is the appropriate place to cover it.
