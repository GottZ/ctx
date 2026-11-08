package oai

import (
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/rerank"
)

// The wire term of the rerank routes (W02-10, the ledger of FOLGE-E at this
// route): what rerank.Score allocates per call, booked before the call.
// Since FOLGE-R (RC-D) rerank.Score writes its body in one allocation of its
// exact length (rerank.RequestBodyLen: no json.Marshal, no HTML escaping,
// "return_documents":false), sends it through httpx.DoRetryOnce, reads the
// answer into a buffer bounded by the call (rerank.AnswerReadMax: room for
// an echo of every document, a share per document and a frame — a longer
// answer is refused) and decodes it in place (scores and coverage flags,
// nothing per entry). The route books those parts.

// rerankWireBodyLen is the exact length of the body rerank.Score sends for
// model, query and docs (at least one document).
func rerankWireBodyLen(model, query string, docs []string) int {
	return rerank.RequestBodyLen(model, query, docs)
}

// rerankWireBodyNum/rerankWireBodyDen is what rerank.Score allocates per
// byte of its body to build it: one allocation of the body's exact length
// (rerank's byte writer, FOLGE-R) — factor 1, where json.Marshal took up to
// 7.25 × cold and up to the size of an earlier Marshal in the process warm
// (W02-10 §Wire-Term, review 2 F-3). rerank.TestWireBodyIsOneAllocation
// measures one allocation per body.
const (
	rerankWireBodyNum = 1
	rerankWireBodyDen = 1
)

// rerankWireBodySlack is what the body's one allocation takes beyond its
// length: the runtime's size class (embed.WireBodySlack — at most 4 KiB
// below 32 KiB, less than an 8 KiB page above). Derived from the runtime's
// allocation granularity, not policy.
const rerankWireBodySlack = embed.WireBodySlack

// rerankDecodeDocBytes is what the answer's decode allocates per document:
// its score (8 B) and its coverage flag (1 B), rounded to the allocator's
// 16 B (rerank.TestDecodeAnswerAllocatesNoMoreThanItsDocuments bounds the
// decode at 16 B per document and 4 KiB), and the 24 B entry an answer that
// repeats its results list folds first (rerank answerScan.fold) — booked
// here, not left to the read's share: a chunked answer at the read cap
// spends that share on its buffers (Fremd-Review S2 F1: 12 760–14 296 B
// past the read's and the decode's share at 1000 documents;
// rerankwire TestFolgeRFoldedAnswerIsBooked).
const rerankDecodeDocBytes = 16 + 24

// rerankWireDocBytes is the per-document part of a wire call beside the
// body: the read's per-document share as rerank.AnswerReadMax books it
// (rerank.AnswerEntryMax for the buffer of an answer without an echo and
// once more for the buffer of the read cap) and the decode's.
const rerankWireDocBytes = 2*rerank.AnswerEntryMax + rerankDecodeDocBytes

// rerankWireFixedBytes is what one wire call allocates beside its body, its
// copy, its answer and its decode — the request, its headers and the hop
// stamp, the transport's connection state and the answer's reader, and
// rerank.Score's own. Measured (internal/oai/rerankwire, Score against a raw
// upstream, pools cold, the largest of three): 45.7 KB on a pooled
// connection (1 × 64 B), 56.3–59.0 KB on a fresh one — the transport dials
// it, its state and buffers — with an answer refused at its length
// (TestFolgeRAnswerReadIsBooked's hostile forms, which close the
// connection); booked at the fresh connection's, rounded up to 60 KiB. A
// measured mechanism constant, not policy;
// TestRerankWireCallReserveIsTheMeasuredOne fails when a form needs more.
const rerankWireFixedBytes = 60 << 10

// rerankWireCallBytes is the fixed part of a wire call: the read's fixed
// share as rerank.AnswerReadMax books it (rerank.AnswerFixed and
// rerank.AnswerReadSlack, twice each) and rerankWireFixedBytes.
const rerankWireCallBytes = 2*rerank.AnswerFixed + 2*rerank.AnswerReadSlack + rerankWireFixedBytes

// rerankWireRetryBytes is what the retry of a wire call allocates beside
// the second copy of its body (httpx.DoRetryOnce resends the same body on a
// fresh connection; the body is not built again): the connection's state —
// embedWireRetryBytes, measured at the embeddings route over the same
// httpx.DoRetryOnce (FOLGE-E hardening 2), whose hand-over Ü-H2-2 named the
// term missing at this route; not measured here again.
const rerankWireRetryBytes = embedWireRetryBytes

// rerankWireCall is what one wire call of a body of body bytes over docs
// documents allocates at most, its retry included: the body's build
// (rerankWireBodyNum/rerankWireBodyDen × the body, rounded up, and
// rerankWireBodySlack), the transport's copy of the body onto the
// connection (embed.WireCopyBuffer, or the body's length when shorter), the
// answer's read (rerank.AnswerReadMax: the body's length once — room for an
// echo of every document —, rerank.AnswerEntryMax per document,
// rerank.AnswerFixed and rerank.AnswerReadSlack twice), the decode's rerankDecodeDocBytes per document,
// rerankWireFixedBytes, and for the retry the copy once more and
// rerankWireRetryBytes. The retry is booked with the first call, whether it
// happens or not: httpx.DoRetryOnce decides it inside rerank.Score (the
// embeddings route's rule, FOLGE-E hardening 2).
func rerankWireCall(body, docs int) int64 {
	b := int64(body)
	copied := min(b, embed.WireCopyBuffer)
	return (rerankWireBodyNum*b+rerankWireBodyDen-1)/rerankWireBodyDen + rerankWireBodySlack + copied +
		rerank.AnswerReadMax(docs, body) + int64(docs)*rerankDecodeDocBytes + rerankWireFixedBytes +
		copied + rerankWireRetryBytes
}

// rerankUpstreamBooking is what the wire calls of an attempt of blocks (their
// sizes, in document order) to model allocate at most: rerankWireCall of
// every block.
func rerankUpstreamBooking(req *rerankRequest, model string, blocks []int) int64 {
	var n int64
	lo := 0
	for _, size := range blocks {
		n += rerankWireCall(rerankWireBodyLen(model, req.query, req.wire[lo:lo+size]), size)
		lo += size
	}
	return n
}

// rerankUpstreamTexts is the part of an attempt's wire calls that is certain
// before the acquire, whatever its blocks: every document and its separator
// and the query once, each as the wire encodes it (embed.WireStringLen), at
// the body's factor for the build and once more for the answer's room for
// an echo, and every document's rerankWireDocBytes — at most what
// rerankUpstreamBooking books for any split of the documents (each block's
// body carries the query, its documents and a frame longer than their
// separators).
func rerankUpstreamTexts(req *rerankRequest) int64 {
	n := int64(embed.WireStringLen(req.query))
	for _, d := range req.wire {
		n += int64(embed.WireStringLen(d)) + 1
	}
	return (rerankWireBodyNum*n+rerankWireBodyDen-1)/rerankWireBodyDen + n + int64(len(req.wire))*rerankWireDocBytes
}

// The terms of the rerank routes as the configuration's budget floor reads
// them (W02-10, as FOLGE-E's Embed* exports): their reader is the budget
// dimensioning (V39) before the flip — V39 counts none of them yet — and
// the wire pins outside internal/oai (internal/oai/rerankwire), which
// rebuild rerankWireCall from them. RerankResultReserve per document and
// RerankEncodeBuffer are the answer's share; RerankDocSlotReserve per
// document beside its decoded text the parse's; per wire call
// RerankWireBodyNum/RerankWireBodyDen × its body and RerankWireBodySlack,
// RerankWireCopyReserve (or the body's length when shorter) twice — the
// call and its retry —, the body's length once more (the answer's room for
// an echo), RerankWireDocReserve per document, RerankWireCallReserve and
// RerankWireRetryReserve.
const (
	RerankResultReserve    = rerankResultBytes
	RerankEncodeBuffer     = rerankEncodeBuffer
	RerankDocSlotReserve   = rerankDocSlotBytes
	RerankWireBodyNum      = rerankWireBodyNum
	RerankWireBodyDen      = rerankWireBodyDen
	RerankWireBodySlack    = rerankWireBodySlack
	RerankWireCopyReserve  = embed.WireCopyBuffer
	RerankWireDocReserve   = rerankWireDocBytes
	RerankWireCallReserve  = rerankWireCallBytes
	RerankWireRetryReserve = rerankWireRetryBytes
)
