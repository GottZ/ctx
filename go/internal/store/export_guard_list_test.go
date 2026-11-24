package store

// GuardListQueryForTest is GuardList's statement and its arguments (issue
// #238): the buffer pin of the list's order key EXPLAINs the statement the
// list runs. Production never calls it.
func GuardListQueryForTest(readScopes []string, category, status string, types []string, limit int) (string, []any) {
	return guardListQuery(readScopes, category, status, types, limit)
}

// GuardListOrderSQLForTest is the list's order key (guardListOrderSQL), so the
// pin can put the bare cast in its place.
const GuardListOrderSQLForTest = guardListOrderSQL
