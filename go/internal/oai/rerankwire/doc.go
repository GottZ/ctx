// Package rerankwire holds the pin of the rerank routes' wire-call term
// (W02-10): its tests call rerank.Score directly, which internal/oai's X13
// census admits in none of its files, so they live beside it. The package
// has no code; this file keeps it a package under every build tag (the pin
// is a production-build measurement, excluded under -race).
package rerankwire
