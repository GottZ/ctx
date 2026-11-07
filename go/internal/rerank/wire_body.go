package rerank

import (
	"encoding/json"
	"errors"
)

// The wire body and the answer bound of Score (FOLGE-R, RC-D; the
// hand-over of W02-10's stop findings S-W1 and S-W2 and of its external
// review 2, F-1 and F-3). This file is the contract of the hardening wave
// in its first layer: every name below exists and Score goes through it,
// but each still answers as the base did — the byte writer, the strict
// decoder and the bounded read are the second layer's (bau report
// RCD-c8-hardening-S1 §Vertrag). The contract:
//
//   - wireBody writes Score's body in one allocation of its exact length
//     (WireBodyLen), without json.Marshal: the encoder's pooled buffer grows a
//     quarter at a time and is copied out (up to 7.25 × the body cold), and a
//     warm pool allocates up to the size of an earlier Marshal anywhere in the
//     process (W02-10 review 2 F-3: 8 429 960 B for a 169 B body). The bytes
//     are encoding/json's without HTML escaping (embed.WireStringLen's
//     encoding: <, > and & as one byte, not six — W02-10 measured 38.2 × the
//     documents' bytes for "<"), the members in the order model, query,
//     documents, return_documents, no trailing newline.
//   - The body carries "return_documents":false (F-1): a ctx peer's
//     /v1/rerank (Jina dialect) and the Jina API echo every document by
//     default; llama.cpp, TEI and vLLM ignore the member, Infinity, Voyage and
//     Cohere v1 know it and default to false (sources in the S1 report).
//     vLLM echoes every document whatever the member says — the decoder skips
//     an echo, the read bound has room for it.
//   - answerReadCap bounds the read of an answer by what the route books for
//     the call, not by a fixed 1 MiB: an answer to n documents is at most the
//     body's length (room for an echo of every document) plus answerEntryMax
//     per document plus answerFixed; a longer answer is refused at the read.
//   - decodeAnswer (rerank.go) is the strict decoder: entries walked in
//     place, the first entry past n refused before its decode
//     (errSurplusResults), unknown members skipped without an allocation, no
//     allocation per entry beyond the scores and their coverage flags.

// errSurplusResults is the strict decoder's refusal of an answer with an
// entry past the documents (FOLGE-R S2; raw_answer.go's errSurplusEmbeddings
// at the embed wire). Unused by the base decode — its tests are the contract.
var errSurplusResults = errors.New("rerank: more results than documents")

// WireBodyLen is the exact length of the body Score sends for model, query
// and docs — the length the gateway's rerank route books the body's build
// at (internal/oai rerankWireBodyLen reads it in FOLGE-R S2, factor 1).
// RC-D S1: the length of the base's json.Marshal body, built to be measured.
func WireBodyLen(model, query string, docs []string) int {
	body, err := wireBody(model, query, docs)
	if err != nil {
		return 0
	}
	return len(body)
}

// wireBody is Score's request body for model, query and docs. RC-D S1: the
// base's json.Marshal of rerankRequest (no return_documents, HTML escaping,
// the encoder's growth) — FOLGE-R S2 replaces it with the byte writer.
func wireBody(model, query string, docs []string) ([]byte, error) {
	return json.Marshal(rerankRequest{Model: model, Query: query, Documents: docs})
}

// answerReadCap is the most bytes Score reads of an answer to docs
// documents whose request body was body bytes long. RC-D S1: the base's
// fixed 1 MiB, whatever the call — FOLGE-R S2 bounds it by the call's
// booking (body + docs × answerEntryMax + answerFixed).
func answerReadCap(docs, body int) int64 {
	_, _ = docs, body
	return 1 << 20
}
