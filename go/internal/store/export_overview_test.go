package store

// Test hooks for the W7 read-path gates.
//
// They exist because the store's DB-backed tests must live in package
// store_test: internal/testdb imports internal/store, so an internal test file
// that imported testdb would be an import cycle. The gates still need to reach
// the two unexported halves of the read path — the node query and the category
// fill — to run the legacy path against the identity path inside ONE test, and
// to strip the scope backstop for the red probe.

// OverviewNodesForTest exposes the node read for both run shapes.
var OverviewNodesForTest = overviewNodes

// FillTopCategoriesForTest exposes the category fill with its keying mode.
var FillTopCategoriesForTest = fillTopCategories

// PatchOverviewNodesTopicSQL replaces the identity-path query and returns the
// restore func. Production never calls it.
func PatchOverviewNodesTopicSQL(sql string) (previous string, restore func()) {
	prev := overviewNodesTopicSQL
	overviewNodesTopicSQL = sql
	return prev, func() { overviewNodesTopicSQL = prev }
}

// OverviewNodesTopicSQL returns the current identity-path query, so a probe can
// patch it by substitution instead of by retyping it.
func OverviewNodesTopicSQL() string { return overviewNodesTopicSQL }

// OverviewLegacyProbeSQL is the K2-5 legacy probe, so its EXPLAIN gate runs
// against the very string production uses instead of a copy.
func OverviewLegacyProbeSQL() string { return overviewLegacyProbeSQL }

// OverviewLegacyProbeSeqScanSQL is the pre-K2-5 shape — the EXISTS over
// topic_id IS NULL that no index can serve. Test use only.
const OverviewLegacyProbeSeqScanSQL = `
	SELECT EXISTS (SELECT 1 FROM graph_cluster_node n
	                WHERE n.scope = ANY($1::text[]) AND n.topic_id IS NULL)`

// OverviewLegacyForTest exposes the legacy probe to the gates in store_test.
var OverviewLegacyForTest = overviewLegacy
