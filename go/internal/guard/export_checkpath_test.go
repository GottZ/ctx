package guard

// Test-only exports for the external integration tests (issue #381).

// GuardCheckSourceExactForTest is the exact check source the batch runs for a
// small candidate circle.
const GuardCheckSourceExactForTest = guardCheckSourceExact

// CheckBlockForTest is the batch's check of one block, through
// ctx_guard_check (exact false) or the exact statement (exact true), issue
// #396.
var CheckBlockForTest = checkBlock

// SearchDefaultForTest is the guard.ef_search and guard.exact_max_candidates
// defaults, for the tests of this package (internal/config may not be
// imported here: depguard config-layering; config pins the defaults in
// TestGuardSearchKeys).
var SearchDefaultForTest = Search{EfSearch: 800, ExactMaxCandidates: 400}

// SearchIndexForTest checks every block through ctx_guard_check (exact limit
// 0): the suites written against the function keep exercising it, the index
// plan pinned around it included; the exact statement is pinned by
// guard_exact_path_integration_test.go.
var SearchIndexForTest = Search{EfSearch: 800, ExactMaxCandidates: 0}
