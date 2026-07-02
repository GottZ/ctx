package dream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
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

	// builtinTestSet is the compiled-in seed policy set (knowledge default,
	// everything but system-meta dream-linkable, no link_classes limits) —
	// the WF T8 WriteLinks tests run under seed-default behaviour unless a
	// test builds its own set.
	builtinTestSet = blocktype.NewRegistry().Snapshot()
)

// expectSourceFetch sets up the source-block lookup with the given metadata.
// The fixture rows carry type_name='knowledge' (WF T8: WriteLinks resolves
// the source type's link-class policy).
func expectSourceFetch(mock pgxmock.PgxPoolIface, sid, cat string, updated, created time.Time, title string) {
	rows := mock.NewRows([]string{"category", "updated_at", "created_at", "title", "type_name"}).
		AddRow(cat, updated, created, title, "knowledge")
	mock.ExpectQuery(`SELECT category, updated_at, created_at, title, type_name FROM context_blocks WHERE id = \$1`).
		WithArgs(sid).
		WillReturnRows(rows)
}

// expectSourceFetchTyped is expectSourceFetch with an explicit source type
// (WF T8 link-class gate tests).
func expectSourceFetchTyped(mock pgxmock.PgxPoolIface, sid, cat string, updated, created time.Time, title, typeName string) {
	rows := mock.NewRows([]string{"category", "updated_at", "created_at", "title", "type_name"}).
		AddRow(cat, updated, created, title, typeName)
	mock.ExpectQuery(`SELECT category, updated_at, created_at, title, type_name FROM context_blocks WHERE id = \$1`).
		WithArgs(sid).
		WillReturnRows(rows)
}

// expectTargetFetch sets up the target-block lookup (type_name='knowledge').
func expectTargetFetch(mock pgxmock.PgxPoolIface, tid, scope string, archived bool, quality float64, cat string, updated, created time.Time, title string) {
	rows := mock.NewRows([]string{"scope", "is_archived", "quality_score", "category", "updated_at", "created_at", "title", "type_name"}).
		AddRow(scope, archived, quality, cat, updated, created, title, "knowledge")
	mock.ExpectQuery(`SELECT scope, is_archived, quality_score, category, updated_at, created_at, title, type_name FROM context_blocks WHERE id = \$1`).
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

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0, nil)
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

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

// V8 supersedes-gate behaviour (similarity ≥ 0.25 → insert; < 0.25 → reject)
// is now covered by TestWriteLinks_Supersedes_RealSimilarity_BehaviourMatchesContract
// in writelinks_integration_test.go, which exercises the real pg_trgm
// extension instead of stubbing similarity() returns. The pgxmock-layer
// equivalents (TestWriteLinks_Supersedes_GatePass_InsertsAndMarksSnapshot,
// TestWriteLinks_Supersedes_LowSimilarity_Reject) were removed — pgxmock
// cannot tell us whether similarity() works against real text, only
// whether WriteLinks calls it with the expected arguments.

func TestWriteLinks_Causal_SrcNotOlder_Reject(t *testing.T) {
	// V9 reject path: source created_at NOT before target.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tLate, tLate, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tEarly, tEarly, "tgt")
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

// Replace-semantics on success (DELETE with != ALL($2::uuid[]) clause + the
// kept-targets array) is now covered by
// TestWriteLinks_ReplaceSemantics_RealUUIDs_BehaviourMatchesContract in
// writelinks_integration_test.go, which checks the surviving row against
// real UUID arrays. The pgxmock variant
// (TestWriteLinks_ReplaceSemantics_StaleDeleted_OnSuccess) was removed —
// pgxmock matches the SQL string but cannot evaluate UUID-array semantics.

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

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

// Snapshot-revert lifecycle (write → idempotent re-write → revert on next
// non-supersedes write) is now covered by
// TestWriteLinks_SnapshotRevert_Idempotent_BehaviourMatchesContract in
// writelinks_integration_test.go, which exercises the full sequence
// against real PG. The pgxmock variant
// (TestWriteLinks_SnapshotRevert_OnDeletedSupersedesLink) was removed —
// it matched the SQL strings but couldn't verify real WHERE-clause
// semantics across rows or the no-drift property of the idempotent step.

func TestWriteLinks_SupersedesRevert_WritesKnowledge_NotNull(t *testing.T) {
	// M070 (Welle T1): the lifecycle state machine is NOT NULL — the stale-
	// supersedes revert must write lifecycle_state='knowledge', never NULL
	// (the pre-M070 revert wrote NULL, producing exactly the rows M070
	// backfilled away). Unlike the removed full-lifecycle pgxmock variant,
	// this test is deliberately narrow: pgxmock CAN pin the SQL contract —
	// which value the revert UPDATE carries and for which (target, source)
	// pair it fires. WHERE-semantics across rows stay with the integration
	// test.
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetch(mock, sourceID, "decisions", tEarly, tEarly, "src")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "decisions", tLate, tLate, "tgt")
	expectInsertLink(mock)
	// Stale DELETE returns a previously-written supersedes link to otherID →
	// replaceStaleLinks must fire the revert UPDATE for exactly that target.
	staleRows := mock.NewRows([]string{"target_block_id", "relationship"}).
		AddRow(otherID, "supersedes")
	mock.ExpectQuery(`DELETE FROM context_dream_links`).
		WithArgs(anyArgs(2)...).
		WillReturnRows(staleRows)
	// The contract under test: SET lifecycle_state = 'knowledge' — a revert
	// that still wrote NULL would not match this expectation and fail.
	mock.ExpectExec(`SET lifecycle_state = 'knowledge', superseded_by = NULL`).
		WithArgs(otherID, sourceID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	expectAuditLog(mock)

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
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

func TestWriteLinks_BeginTxError_PropagatesWrapped(t *testing.T) {
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
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

// --- WF T8 policy gates (design/01 §7-T8) ---.

// testSetWith builds a policy set of the knowledge default plus the given
// extra policies — the unit-level probe fixture for the T8 gates.
func testSetWith(t *testing.T, extra ...blocktype.Policy) *blocktype.Set {
	t.Helper()
	policies := append([]blocktype.Policy{{
		Name: "knowledge", Scope: "_global", Builtin: true, IsDefault: true,
		Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
		Dream:     blocktype.DreamPolicy{Linkable: true},
	}}, extra...)
	set, err := blocktype.NewSet(policies)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

func TestWriteLinks_LinkClasses_CausalRejectedForTopicalOnlySource(t *testing.T) {
	// WF T8 gate probe (design/01 §7-T8): a source type restricted to
	// link_classes=["topical"] must reject a causal candidate — no INSERT.
	// RED before T8 (probe run 2026-07-02 against the pre-T8 tree: the causal
	// link was written, no link-class gate existed), GREEN now.
	mock := newPool(t)
	defer mock.Close()

	set := testSetWith(t, blocktype.Policy{
		Name: "wf-topical-only", Scope: "_global",
		Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
		Dream:     blocktype.DreamPolicy{Linkable: true, LinkClasses: []string{"topical"}},
	})

	mock.ExpectBegin()
	expectSourceFetchTyped(mock, sourceID, "decisions", tEarly, tEarly, "src", "wf-topical-only")
	// causal passes V9 (src older than tgt) — only the class gate rejects.
	expectTargetFetch(mock, targetID, "private", false, 1.0, "projects", tLate, tLate, "tgt")
	mock.ExpectCommit()

	written, err := WriteLinks(context.Background(), mock, set, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "causal", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("link_classes=[topical] source wrote a causal link, got written=%d, want 0", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_LinkClasses_TopicalAllowed(t *testing.T) {
	// Companion positive: the same restricted source still writes its
	// allowed class (the gate is a subset filter, not a shut-off).
	mock := newPool(t)
	defer mock.Close()

	set := testSetWith(t, blocktype.Policy{
		Name: "wf-topical-only", Scope: "_global",
		Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
		Dream:     blocktype.DreamPolicy{Linkable: true, LinkClasses: []string{"topical"}},
	})

	mock.ExpectBegin()
	expectSourceFetchTyped(mock, sourceID, "decisions", tEarly, tEarly, "src", "wf-topical-only")
	expectTargetFetch(mock, targetID, "private", false, 1.0, "projects", tLate, tLate, "tgt")
	expectInsertLink(mock)
	expectDeleteStaleNonEmpty(mock)
	mock.ExpectCommit()
	expectAuditLog(mock)

	written, err := WriteLinks(context.Background(), mock, set, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 1 {
		t.Errorf("allowed class must write, got written=%d, want 1", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestWriteLinks_NilSet_FailsLoud(t *testing.T) {
	// WF T8: nil policy set = wiring bug → loud error, zero DB contact.
	mock := newPool(t)
	defer mock.Close()

	written, err := WriteLinks(context.Background(), mock, nil, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err == nil {
		t.Fatal("want error on nil set, got nil")
	}
	if written != 0 {
		t.Errorf("got written=%d, want 0", written)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

func TestWriteLinks_UnknownSourceType_RejectsBatchFailClosed(t *testing.T) {
	// WF T8 / §5.1: an unregistered source type rejects the whole batch —
	// no target fetch, no INSERT (fail-closed, loud WARN).
	mock := newPool(t)
	defer mock.Close()

	mock.ExpectBegin()
	expectSourceFetchTyped(mock, sourceID, "decisions", tEarly, tEarly, "src", "wf-unregistered")
	// No further expectations: the batch stops at the source policy resolve.

	written, err := WriteLinks(context.Background(), mock, builtinTestSet, sourceID, "private", 1.0,
		[]Link{{TargetID: targetID, Relationship: "topical", Confidence: 0.9}})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if written != 0 {
		t.Errorf("unknown source type must write nothing, got %d", written)
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
var _ = (func(_ context.Context, _ linkPool, _ *blocktype.Set, _, _ string, _ float64, _ []Link) (int, error))(WriteLinks)

// silence "unused" for pgx import in some build configurations.
var _ = pgx.TxOptions{}
