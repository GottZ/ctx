package store

// Test-binary-only exports (export_test idiom, precedent
// overview/export_lockkey_test.go): the graph-structural W1 gates probe
// unexported seams directly from the external store_test package.
var (
	InducedEdgesForTest          = inducedEdges
	DisplayClassesForTest        = displayClasses
	NormalizeClassFiltersForTest = normalizeClassFilters
)
