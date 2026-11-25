package store

// WorkflowListNoSortForTest is the setting the workflow lists run their pages
// under (readNoSort, issue #536): the W6 gates EXPLAIN the exported
// list statements in a transaction with it, as ListWorkflowBlocks,
// BoardColumns and ListIssuesByCreated send them. Production never reads it.
const WorkflowListNoSortForTest = readNoSort

// IDPrefixRangeForTest is idPrefixRange (issue #536): the equality probe of
// ResolveBlockID derives the old text match and the new range from the same
// prefixes. Production never reads it.
func IDPrefixRangeForTest(prefix string) (lo, hi string, ok bool) { return idPrefixRange(prefix) }

// EmbedExcludedStarScopesSQLForTest is the statement of a '*' entry's scopes
// (embedExcludedStarScopesSQL, issue #536): the equality probe reads it
// beside the statement it replaced. Production never reads it.
var EmbedExcludedStarScopesSQLForTest = embedExcludedStarScopesSQL
