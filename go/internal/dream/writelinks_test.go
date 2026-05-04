package dream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// --- Test fixtures ---.

const (
	sourceID = "019d0000-0000-7000-9000-000000000001"
	targetID = "019d0000-0000-7000-9000-000000000002"
	otherID  = "019d0000-0000-7000-9000-000000000003"
)

var (
	tEarly = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate  = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

// expectSourceFetch sets up the source-block lookup with the given metadata.
func expectSourceFetch(mock pgxmock.PgxPoolIface, sid, cat string, updated, created time.Time, title string) {
	rows := mock.NewRows([]string{"category", "updated_at", "created_at", "title"}).
		AddRow(cat, updated, created, title)
	mock.ExpectQuery(`SELECT category, updated_at, created_at, title FROM context_blocks WHERE id = \$1`).
		WithArgs(sid).
		WillReturnRows(rows)
}

// expectTargetFetch sets up the target-block lookup.
func expectTargetFetch(mock pgxmock.PgxPoolIface, tid, scope string, archived bool, quality float64, cat string, updated, created time.Time, title string) {
	rows := mock.NewRows([]string{"scope", "is_archived", "quality_score", "category", "updated_at", "created_at", "title"}).
		AddRow(scope, archived, quality, cat, updated, created, title)
	mock.ExpectQuery(`SELECT scope, is_archived, quality_score, category, updated_at, created_at, title FROM context_blocks WHERE id = \$1`).
		WithArgs(tid).
		WillReturnRows(rows)
}

// anyArgs returns n AnyArg matchers for variable-arity WithArgs calls.
func anyArgs(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// expectInsertLink expects the dream-link INSERT (8 args) and returns success.
func expectInsertLink(mock pgxmock.PgxPoolIface) *pgxmock.ExpectedExec {
	return mock.ExpectExec(`INSERT INTO context_dream_links`).
		WithArgs(anyArgs(8)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

// expectDeleteStaleNonEmpty matches the DELETE issued when keptTargets ≥ 1
// (i.e. on written>0). Returns no stale rows.
func expectDeleteStaleNonEmpty(mock pgxmock.PgxPoolIface) {
	rows := mock.NewRows([]string{"target_block_id", "relationship"})
	mock.ExpectQuery(`DELETE FROM context_dream_links`).
		WithArgs(anyArgs(2)...).
		WillReturnRows(rows)
}

// expectAuditLog matches the post-commit audit-log INSERT (2 args: id + jsonb).
func expectAuditLog(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`INSERT INTO context_write_log`).
		WithArgs(anyArgs(2)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func newPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	return mock
}

// --- Tests ---.

func TestWriteLinks_EmptyLinks_NoWork(t *testing.T) {
	mock := newPool(t)
	defer mock.Close()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("got written=%d, want 0", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

func TestWriteLinks_CrossScope_Reject(t *testing.T) {
	// V5/V6 gate: target.scope != source.scope → drop, no INSERT, no replace.
	// Empty written → no replace-semantics, but Commit still fires (current
	// implementation; deferred Rollback is a no-op after Commit).
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "shared" /* != private */, false, 1.0, "decisions", tLate, tLate, "tgt")
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != 0 {
		t.Errorf("cross-scope must not write, got %d", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_TargetArchived_Reject(t *testing.T) {
	// V6 gate: target.is_archived → drop.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", true /* archived */, 1.0, "decisions", tLate, tLate, "tgt")
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("archived must not write, got %d", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_FactualSameCategory_CoercedToTopical(t *testing.T) {
	// V10 coerce: factual within same category → INSERT relationship='topical'.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "tgt")
	// Coerced link must be inserted with relationship='topical'.
	mock.ExpectExec(`INSERT INTO context_dream_links`).
		WithArgs(sourceID, targetID, "topical", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectDeleteStaleNonEmpty(mock)
	mock.ExpectCommit()
	expectAuditLog(mock)

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "factual", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 1 {
		t.Errorf("got written=%d, want 1", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_Supersedes_GatePass_InsertsAndMarksSnapshot(t *testing.T) {
	// V8 supersedes pass: same cat + src older + similarity ≥ 0.25
	// → INSERT + UPDATE block_type='snapshot'.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "old title")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "old title v2")
	// V8 similarity probe.
	mock.ExpectQuery(`SELECT similarity\(\$1, \$2\)`).
		WithArgs("old title", "old title v2").
		WillReturnRows(mock.NewRows([]string{"similarity"}).AddRow(0.7))
	expectInsertLink(mock)
	// ApplySupersedes UPDATE (raw conf 0.95 * 1.0 * 1.0 ≥ 0.7).
	mock.ExpectExec(`UPDATE context_blocks SET block_type = 'snapshot'`).
		WithArgs(sourceID, targetID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectDeleteStaleNonEmpty(mock)
	mock.ExpectCommit()
	expectAuditLog(mock)

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "supersedes", Confidence: 0.95}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 1 {
		t.Errorf("got written=%d, want 1", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_Supersedes_LowSimilarity_Reject(t *testing.T) {
	// V8 reject path: similarity below 0.25 floor.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "tgt")
	mock.ExpectQuery(`SELECT similarity`).
		WithArgs(anyArgs(2)...).
		WillReturnRows(mock.NewRows([]string{"similarity"}).AddRow(0.10))
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "supersedes", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("low-similarity supersedes must not write, got %d", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_Causal_SrcNotOlder_Reject(t *testing.T) {
	// V9 reject path: source created_at NOT before target.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tLate, tLate, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tEarly, tEarly, "tgt")
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "causal", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("causal src-not-older must not write, got %d", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_InsertError_LoopBreaksZeroWritten(t *testing.T) {
	// PG returning an INSERT error breaks out of the loop and yields
	// written=0. The test is intentionally narrow: it verifies the SQL-flow
	// contract (loop breaks, replace-semantics is skipped, Commit is
	// reached, return value is (0, nil)) but NOT that the transaction
	// actually rolls back on a real database — pgxmock cannot model PG
	// rejecting a Commit on an aborted TX. The real-PG behaviour is
	// covered by TestWriteLinks_TxAbort_BehaviourMatchesContract in the
	// integration test file.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "tgt")
	mock.ExpectExec(`INSERT INTO context_dream_links`).
		WithArgs(anyArgs(8)...).
		WillReturnError(errors.New("constraint violation"))
	// Loop breaks → written=0 → replace-semantics skipped → Commit reached.
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != 0 {
		t.Errorf("got written=%d on insert-error, want 0", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_ReplaceSemantics_StaleDeleted_OnSuccess(t *testing.T) {
	// On written>0: DELETE removes context_dream_links rows for sourceID
	// not in keptTargets. Verified via WithArgs + non-empty stale return.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "tgt")
	expectInsertLink(mock)
	// DELETE returns one stale topical row → no snapshot revert needed.
	staleRows := mock.NewRows([]string{"target_block_id", "relationship"}).
		AddRow(otherID, "topical")
	mock.ExpectQuery(`DELETE FROM context_dream_links`).
		WithArgs(anyArgs(2)...).
		WillReturnRows(staleRows)
	mock.ExpectCommit()
	expectAuditLog(mock)

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 1 {
		t.Errorf("got %d, want 1", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_ReplaceSemantics_NotTriggered_OnZeroWritten(t *testing.T) {
	// 0-written cycle (e.g. all rejected by gates) MUST NOT delete stale
	// links — protects against transient LLM empty responses (Pessimist M1).
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	// Single link, archived target → 0 writes, no DELETE call.
	expectTargetFetch(mock, targetID, "private", true /* archived */, 1.0, "decisions", tLate, tLate, "tgt")
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("got %d, want 0", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_SnapshotRevert_OnDeletedSupersedesLink(t *testing.T) {
	// When the DELETE returns a stale supersedes row, replaceStaleLinks
	// must also UPDATE context_blocks to revert block_type='snapshot'.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "tgt")
	expectInsertLink(mock)
	// DELETE returns a stale supersedes row → triggers revert UPDATE.
	staleRows := mock.NewRows([]string{"target_block_id", "relationship"}).
		AddRow(otherID, "supersedes")
	mock.ExpectQuery(`DELETE FROM context_dream_links`).
		WithArgs(anyArgs(2)...).
		WillReturnRows(staleRows)
	mock.ExpectExec(`UPDATE context_blocks\s+SET block_type = NULL, superseded_by = NULL`).
		WithArgs(sourceID, otherID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	expectAuditLog(mock)

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 1 {
		t.Errorf("got %d, want 1", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_BeginTxError_PropagatesWrapped(t *testing.T) {
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

	written, err := WriteLinks(context.Background(), mock, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errorContains(err, "begin tx") || !errorContains(err, "pool exhausted") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
	if written != 0 {
		t.Errorf("got %d, want 0", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// Compile-time guard: pgxmock's pool implementation satisfies our linkPool
// interface. The underscore-assignment fails to build if the contract drifts.
var _ linkPool = pgxmock.PgxPoolIface(nil)

// Compile-time guard: linkPool is sufficient for WriteLinks (the production
// caller passes *pgxpool.Pool which implicitly satisfies linkPool — covered
// by the wider build).
var _ = (func(_ context.Context, _ linkPool, _, _ string, _ float64, _ []Link) (int, error))(WriteLinks)

// silence "unused" for pgx import in some build configurations.
var _ = pgx.TxOptions{}
