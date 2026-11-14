package oai

import "github.com/GottZ/ctx/internal/promptguard"

// The meter's token estimates for the embeddings route (Delta-Review 4
// F4-1). They live outside embeddings.go: the promptguard call-site census
// reads per file, and the route's file builds no prompt (noPromptGuard
// "gateway-embeddings") — it only borrows the rune→token ratio.

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
