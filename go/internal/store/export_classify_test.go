package store

// ClassifyUpdateSQLForTest is the classifier's guarded UPDATE (issue #426):
// the plan test in package store_test explains it to pin that the primary key
// is its only index condition. Production never reads it.
const ClassifyUpdateSQLForTest = classifyUpdateSQL
