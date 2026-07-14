package blocktype

// Test-binary-only export (export_test idiom): the reserved-name sync gate
// lives in the EXTERNAL test package (it must import internal/store, which
// imports this package — external avoids the cycle) and needs the unexported
// local copy.
var ReservedGraphClassesForTest = reservedGraphClasses
