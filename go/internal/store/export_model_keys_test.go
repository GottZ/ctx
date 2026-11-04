package store

import "time"

// Test hooks for the W01-3 model-key gates (package store_test, which owns the
// test database — see export_audit_test.go).
//
// InsertModelKeyForTest is the key insert without the consumer lock and the
// active check in front of it, so a gate can drive it into an inactive consumer
// and observe how the migration-161 insert guard (SQLSTATE 23514) is mapped.
// SetModelKeyUsageClockForTest replaces the clock Touch reads, so a gate can
// place 200 touches across a minute boundary without waiting for one.
// Production never calls either.
var InsertModelKeyForTest = insertModelKeyTx

func SetModelKeyUsageClockForTest(u *ModelKeyUsage, now func() time.Time) { u.now = now }
