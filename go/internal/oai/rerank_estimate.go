package oai

import "github.com/GottZ/ctx/internal/promptguard"

// The meter's token estimate for the rerank routes (W02-10, the embeddings
// route's embed_estimate.go): outside rerank.go, whose file builds no prompt
// (noPromptGuard "gateway-rerank") — it only borrows the rune→token ratio.

// restEstimate is the meter's estimate of the documents [lo, hi) that
// counted (one flag per document) does not mark — the token estimate of
// their (query + document) runes, as the reservation is the estimate of all
// pairs (keyquota Estimate.InputRunes of a rerank request).
func (c *rerankCall) restEstimate(lo, hi int, counted []bool) int {
	runes := 0
	for i, n := range c.req.runes[lo:hi] {
		if !counted[lo+i] {
			runes = usageTotal(runes, usageTotal(c.req.queryRunes, n))
		}
	}
	return promptguard.TokenEstimate(runes)
}
