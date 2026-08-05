package store

// Test hook for the C6 plan gate (design/03 §6.6).
//
// The gate has to EXPLAIN the statement PRODUCTION runs — a retyped copy would
// drift and the gate would then measure the copy, which is the failure mode the
// original gate already had (it asserted the wrong property and the bad plan
// passed it). The builder is pure, so exposing it costs nothing at runtime.
//
// The gate itself lives in package store_test (internal/testdb imports
// internal/store, so a DB-backed test inside package store would be an import
// cycle) — hence the exported alias.

// SearchBlocksParams is the exported alias of the builder input.
type SearchBlocksParams = searchParams

// SearchBlocksSQLForTest exposes the statement builder. Production never calls it.
var SearchBlocksSQLForTest = searchBlocksSQL
