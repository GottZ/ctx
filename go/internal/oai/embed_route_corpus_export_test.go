package oai

import "testing"

// EmbedRouteDiffCorpus is the corpus of the W02-09 parse differential
// (embedDiffCorpus, embed_parse_differential_test.go) for the route
// differential of FOLGE-E gate G5 (TestEmbedRouteDump,
// embed_route_differential_test.go): the same bodies, sent over the route.
func EmbedRouteDiffCorpus(t testing.TB) [][]byte { return embedDiffCorpus(t) }
