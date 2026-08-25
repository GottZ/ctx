package rrf

// Bounds of the ctx_rrf output limit (p_limit).
//
// MaxSearchLimit MUST stay >= handler.overFetchHardCap: the query pipeline's
// aggregate over-fetch legitimately widens its internal limit past the former
// 200 (handler/query.go internalLimit=200 x handler/query_fold.go
// overFetchFactor=2 = 400, hard-capped at 500). overFetchHardCap is derived
// FROM this constant, so the two ceilings of the same value cannot drift apart
// again — they were two independent literals before.
//
// DefaultSearchLimit is the fallback for a caller that supplies no limit at all
// (the Go zero value), mirroring the SQL-side `p_limit INT DEFAULT 5`.
const (
	DefaultSearchLimit = 5
	MaxSearchLimit     = 500
)

// clampSearchLimit bounds a requested output limit into
// [1, MaxSearchLimit], with a non-positive request falling back to
// DefaultSearchLimit.
//
// An over-large limit is CAPPED, never reset to the default: a caller asking
// for more rows than the mechanism serves wants as many as it can get, and
// resetting turns a benign over-ask into a silent retrieval collapse that no
// caller can distinguish from a thin corpus.
func clampSearchLimit(limit int) int {
	if limit < 1 {
		return DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		return MaxSearchLimit
	}
	return limit
}
