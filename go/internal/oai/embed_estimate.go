package oai

import "github.com/GottZ/ctx/internal/promptguard"

// The meter's token estimates for the embeddings route (Delta-Review 4
// F4-1). They live outside embeddings.go: the promptguard call-site census
// reads per file, and the route's file builds no prompt (noPromptGuard
// "gateway-embeddings") — it only borrows the rune→token ratio, as
// embedRuneLimit does.

// inputEstimate is the meter's estimate of the inputs [lo, hi): the token
// estimate of their runes, as the reservation is the estimate of all runes.
func (c *embedCall) inputEstimate(lo, hi int) int {
	runes := 0
	for _, n := range c.req.runes[lo:hi] {
		runes += n
	}
	return promptguard.TokenEstimate(runes)
}

// reservedTokens is what the meter reserved for the request under op embed:
// keyquota.Estimate.Tokens = promptguard.TokenEstimate of its input runes
// (Estimate.InputRunes = c.req.runesSum).
func (c *embedCall) reservedTokens() int {
	return promptguard.TokenEstimate(c.req.runesSum)
}
