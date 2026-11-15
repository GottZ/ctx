package store

// Test hook for the issue #197 lock pass of archive and scope move: the cost
// pin in package store_test (internal/testdb imports internal/store) plans the
// statement itself. Production never reads it.
const LockSupersedesSourceSQLForTest = lockSupersedesSourceSQL

// SupersedesTargetsOfSQLForTest is the re-read that follows the lock pass.
const SupersedesTargetsOfSQLForTest = supersedesTargetsOfSQL
