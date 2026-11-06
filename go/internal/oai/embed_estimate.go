package oai

import (
	"net/http"
	"strconv"

	"github.com/GottZ/ctx/internal/promptguard"
)

// The meter's token estimates for the embeddings route (Delta-Review 4
// F4-1). They live outside embeddings.go: the promptguard call-site census
// reads per file, and the route's file builds no prompt (noPromptGuard
// "gateway-embeddings") — it only borrows the rune→token ratio, as
// embedRuneLimit does.

// restEstimate is the meter's estimate of the inputs [lo, hi) that counted
// (one flag per input of the request) does not mark — the token estimate of
// their runes, as the reservation is the estimate of all runes
// (Delta-Review 5: an input a block with a usage served books that usage,
// not an estimate on top).
func (c *embedCall) restEstimate(lo, hi int, counted []bool) int {
	runes := 0
	for i, n := range c.req.runes[lo:hi] {
		if !counted[lo+i] {
			runes += n
		}
	}
	return promptguard.TokenEstimate(runes)
}

// embedCapacityHeader is the token bucket's capacity as the request's lane
// sees it (design/06 §4.4), one of the admission's x-ratelimit-* headers —
// the header the chat route reads for the same ceiling (W02-08 hardening 3).
const embedCapacityHeader = "x-ratelimit-limit-tokens"

// reservedTokens is what an admitted embeddings reservation holds — the
// tokens Keep books (Delta-Review 5 F5-1): the Estimate the limiter charged
// under op embed (keyquota.Estimate.Tokens = promptguard.TokenEstimate of the
// input runes, Estimate.InputRunes = c.req.runesSum), at most the bucket's
// capacity the admission reports in x-ratelimit-limit-tokens (B-06-28: a
// larger reservation is charged at the capacity). A limiter that reports no
// capacity leaves the estimate as the ceiling.
func (c *embedCall) reservedTokens(h http.Header) int {
	n := promptguard.TokenEstimate(c.req.runesSum)
	if capacity, err := strconv.Atoi(h.Get(embedCapacityHeader)); err == nil && capacity > 0 {
		n = min(n, capacity)
	}
	return n
}
