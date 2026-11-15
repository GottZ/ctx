package store

// Test hooks for the block-counter cost pin (issue #266): the pin EXPLAINs
// the statements PRODUCTION runs, not a retyped copy (export_search_test.go
// explains why the pin lives in package store_test).

// StatsSQLForTest is GetStats' statement.
var StatsSQLForTest = statsSQL

// GuardStatsSQLForTest is GetGuardStats' statement.
var GuardStatsSQLForTest = guardStatsSQL

// ListCategoriesSQLForTest is ListCategories' statement.
var ListCategoriesSQLForTest = listCategoriesSQL

// CappedCountForTest is the cap helper of the root-map counts. Since migration
// 170 ActiveBlockCount is a counter read that no 1 ms cap catches, so the cap
// gate (TestRootMapActiveBlockCount) drives the helper with a scanning count.
var CappedCountForTest = cappedCount

// BlockCountReconcileLockSQLForTest is the per-scope advisory lock of the
// reconcile's correction ($1 = scope), so the reconcile tests can hold it.
var BlockCountReconcileLockSQLForTest = blockCountReconcileLockSQL
