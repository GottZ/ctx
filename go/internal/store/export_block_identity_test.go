package store

// Test hook for the issue #227 holder read.
//
// The holder read runs after the refused update's transaction rolled back, so
// a concurrent writer can change the source row in between. The gate that pins
// what the read does with such a row lives in package store_test (internal/
// testdb imports internal/store, so an internal test opening a database would
// be an import cycle); BlockIdentityHolderForTest lets it run the read after
// it moved the row itself, which is the interleaving UpdateBlock cannot be
// paused in. Production never calls it.
var BlockIdentityHolderForTest = blockIdentityHolder

// RefusedKeyForTest hands the gate the DETAIL a refusal carries, which it
// passes to BlockIdentityHolderForTest as UpdateBlock does.
func RefusedKeyForTest(e *BlockIdentityTakenError) string { return e.refusedKey }
