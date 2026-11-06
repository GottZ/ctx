package oai

// Test bridges of the embeddings route (W02-09).

// BaseEmbedTop runs the embeddings parse before the port onto FOLGE-P's key
// helpers (embed_parse_oracle_test.go) on body: the route's CPU pin weighs a
// request against it (W02-09 rebase round 2, part A).
func BaseEmbedTop(body []byte) { _, _ = baseEmbedTop(body) }
