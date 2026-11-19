package guard

import (
	"context"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
)

// TestRunGuardBatch_RoutesByCandidateCircle (#381): after the pick the batch
// sets its hnsw.ef_search once and reads the candidate circles once; a block
// whose circle holds at most ExactMaxCandidates live blocks is checked by the
// exact statement, a larger one (or one the circle read did not answer) by
// ctx_guard_check. The circle is the tenant's for a cross-scope type and the
// own scope's for a same-scope type (issue).
func TestRunGuardBatch_RoutesByCandidateCircle(t *testing.T) {
	const blockD = "019d0000-0000-7000-9000-00000000000d"
	search := Search{EfSearch: 321, ExactMaxCandidates: 37}
	limit := int64(search.ExactMaxCandidates)
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPickIndexReady(mock, true)
	mock.ExpectExec(`^` + regexp.QuoteMeta("SAVEPOINT "+guardPickSavepoint+"; "+guardPickNoSort) + `$`).
		WillReturnResult(pgxmock.NewResult("SET", 0))
	mock.ExpectQuery(`CROSS JOIN LATERAL`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows([]string{"tid", "id", "type_name"}).
			AddRow("(0,1)", blockA, "knowledge").
			AddRow("(0,2)", blockB, "knowledge").
			AddRow("(0,3)", blockC, "issue").
			AddRow("(0,4)", blockD, "knowledge"))
	mock.ExpectExec(`^` + regexp.QuoteMeta(guardPickRestoreSort+"; RELEASE SAVEPOINT "+guardPickSavepoint) + `$`).
		WillReturnResult(pgxmock.NewResult("RELEASE", 0))

	mock.ExpectExec(`^` + regexp.QuoteMeta(guardSearchSQL) + `$`).
		WithArgs("321").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery(`FROM picked p$`).
		WithArgs([]string{blockA, blockB, blockC, blockD}).
		WillReturnRows(mock.NewRows([]string{"id", "scope_n", "tenant_n"}).
			AddRow(blockA, int64(1), limit).   // tenant circle at the limit: exact
			AddRow(blockB, int64(1), limit+1). // one above: index
			AddRow(blockC, limit, 10*limit))   // issue: own scope at the limit: exact
	// blockD is missing from the answer: index.

	exactStmt := `\+ 0\s+LIMIT 1`
	for _, step := range []struct {
		id    string
		match string
	}{
		{blockA, exactStmt},
		{blockB, `FROM ctx_guard_check\(`},
		{blockC, exactStmt},
		{blockD, `FROM ctx_guard_check\(`},
	} {
		expectSavepoint(mock, step.id)
		index := step.match != exactStmt
		if index {
			expectIndexPlanOn(mock) // the index check runs with its plan pinned
		}
		mock.ExpectQuery(step.match).
			WithArgs(step.id, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnRows(mock.NewRows([]string{"decision", "top_similarity", "matched_id", "matched_title", "matched_scope", "is_cross_scope", "supersedes_pair"}).
				AddRow("clean", 0.1, (*string)(nil), (*string)(nil), (*string)(nil), false, false))
		if index {
			expectIndexPlanOff(mock)
		}
		expectApplyDecisionClean(mock)
		expectAuditFetch(mock, step.id)
		expectAuditInsert(mock)
		expectRelease(mock, step.id)
	}
	mock.ExpectCommit()
	expectGuardStateUpdate(mock)

	n, err := RunGuardBatch(context.Background(), mock, testSet, 10, search)
	if err != nil || n != 4 {
		t.Fatalf("processed %d, err %v; want 4, nil", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRunGuardBatch_RefusesSearchOutsideTheRange (#381): a window pgvector
// would refuse, one narrower than its default, or a negative exact limit
// fails before any work.
func TestRunGuardBatch_RefusesSearchOutsideTheRange(t *testing.T) {
	for _, s := range []Search{
		{EfSearch: 0, ExactMaxCandidates: 250},
		{EfSearch: EfSearchMin - 1, ExactMaxCandidates: 250},
		{EfSearch: EfSearchMax + 1, ExactMaxCandidates: 250},
		{EfSearch: 800, ExactMaxCandidates: -1},
	} {
		mock := newMockPool(t)
		if _, err := RunGuardBatch(context.Background(), mock, testSet, 10, s); err == nil {
			t.Errorf("%+v: no error", s)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
		mock.Close()
	}
}

// TestRunGuardBatch_ExactLimitZeroReadsNoCircles (#381): with
// guard.exact_max_candidates 0 every block is checked through the index and
// the batch does not read the block counters at all.
func TestRunGuardBatch_ExactLimitZeroReadsNoCircles(t *testing.T) {
	mock := newMockPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectPickIndexReady(mock, true)
	mock.ExpectExec(`^` + regexp.QuoteMeta("SAVEPOINT "+guardPickSavepoint+"; "+guardPickNoSort) + `$`).
		WillReturnResult(pgxmock.NewResult("SET", 0))
	mock.ExpectQuery(`CROSS JOIN LATERAL`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows([]string{"tid", "id", "type_name"}).AddRow("(0,1)", blockA, "knowledge"))
	mock.ExpectExec(`^` + regexp.QuoteMeta(guardPickRestoreSort+"; RELEASE SAVEPOINT "+guardPickSavepoint) + `$`).
		WillReturnResult(pgxmock.NewResult("RELEASE", 0))
	mock.ExpectExec(`^` + regexp.QuoteMeta(guardSearchSQL) + `$`).
		WithArgs("800").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	expectFullBlockSuccess(mock, blockA)
	mock.ExpectCommit()
	expectGuardStateUpdate(mock)

	n, err := RunGuardBatch(context.Background(), mock, testSet, 10, Search{EfSearch: 800, ExactMaxCandidates: 0})
	if err != nil || n != 1 {
		t.Fatalf("processed %d, err %v; want 1, nil", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
