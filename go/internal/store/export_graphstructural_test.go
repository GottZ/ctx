package store

// Test-binary-only exports (export_test idiom, precedent
// overview/export_lockkey_test.go): the graph-structural W1 gates probe
// unexported seams directly from the external store_test package.
var (
	InducedEdgesForTest           = inducedEdges
	DisplayClassesForTest         = displayClasses
	NormalizeClassFiltersForTest  = normalizeClassFilters
	StructuralHopNeighborsForTest = structuralHopNeighbors
	StructuralHopSQLForTest       = structuralHopSQL
)

// HopCandidateID/HopCandidateConf unwrap the unexported hopCandidate for the
// external gate package (W2-G1 asserts on IDs and the constant conf).
func HopCandidateID(c hopCandidate) string    { return c.node.ID }
func HopCandidateConf(c hopCandidate) float64 { return c.conf }
