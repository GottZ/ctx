package oai

// Test bridges of the embeddings route (W02-09).

// BaseEmbedTop runs the embeddings parse before the port onto FOLGE-P's key
// helpers (embed_parse_oracle_test.go) on body: the route's CPU pin weighs a
// request against it (W02-09 rebase round 2, part A).
func BaseEmbedTop(body []byte) { _, _ = baseEmbedTop(body) }

// EmbedReadCap is the byte bound of one block's upstream answer, and
// EmbedDecodeShare the decode share the route reserves for it (hardening 6
// after the bot reviews of PR #157: the decode's allocation is gated
// against the formula the route books, not against a copy of it).
func EmbedReadCap(inputs, dims int, maxFloats int64) int64 {
	return embedReadCap(inputs, dims, maxFloats)
}

func EmbedDecodeShare(inputs, dims int, maxFloats int64) int64 {
	return embedDecodeFactor * embedReadCap(inputs, dims, maxFloats)
}
