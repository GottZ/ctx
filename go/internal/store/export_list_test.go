package store

// Test hooks of issue #237: the cost gates and the differential test EXPLAIN
// and run the statements production composes. Production never calls them.

// SearchStatementForTest is the statement searchBlocks sends for p given a
// reading of the two indexes of migration 195 (idx_blocks_scope_updated,
// idx_blocks_scope_category_updated).
func SearchStatementForTest(p SearchBlocksParams, scopeIndex, categoryIndex bool) (string, []any) {
	return searchStatementFor(p, listIndexes{scope: scopeIndex, category: categoryIndex})
}

// RecentStatementForTest is the statement RecentBlocks sends given a reading
// of the two indexes (limit already clamped).
func RecentStatementForTest(readScopes []string, category string, limit int, types, typesExclude, grantedBlockIDs []string, scopeIndex, categoryIndex bool) (string, []any) {
	return recentStatementFor(readScopes, category, limit, types, typesExclude, grantedBlockIDs, listIndexes{scope: scopeIndex, category: categoryIndex})
}

// ResetListIndexReadingsForTest forgets every reading of listIndexReady, so
// the next listing reads the catalog again.
func ResetListIndexReadingsForTest() {
	listIndexReadings.Lock()
	clear(listIndexReadings.m)
	listIndexReadings.Unlock()
}
