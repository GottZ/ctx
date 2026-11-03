package store

// Test hook for the audit-stamp gates.
//
// The DB-backed gates live in package store_test: internal/testdb imports
// internal/store, so an internal test file that opened a test database would be
// an import cycle. StampAuditForTest hands them the unexported stamp helper, so
// a gate can stamp a transaction directly and observe where a malformed actor
// fails. Production never calls it.
var StampAuditForTest = stampAudit
