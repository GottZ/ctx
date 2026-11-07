package oai

import (
	"github.com/GottZ/ctx/internal/embed"
)

// The wire term of the rerank routes (W02-10, the ledger of FOLGE-E at this
// route): what rerank.Score allocates per call, booked before the call.
// rerank.Score stays as it is (the wave's tabu: the wire is one site,
// unchanged) — it builds its body with json.Marshal, which writes into a
// pooled buffer of its own that grows by doubling and copies the result out,
// sends it through httpx.DoRetryOnce, reads the answer with io.ReadAll under
// a 1-MiB cap and decodes it with json.Unmarshal. The route books what that
// allocates, measured (bau report W02-10 §Wire-Term), not what a byte writer
// would (the hand-over "byte writer and strict decoder in the rerank wire",
// FOLGE-E's wire_body.go/raw_answer.go pattern).

// rerankWireFrame is the frame of rerank.Score's body around the model, the
// query and the documents (rerankRequest in internal/rerank: model, query,
// documents in that order, json.Marshal's form without a newline).
const rerankWireFrame = len(`{"model":,"query":,"documents":[]}`)

// rerankWireStringLen is the length of s as json.Marshal writes it in
// rerank.Score's body: embed.WireStringLen's encoding (encoding/json without
// HTML escaping) plus five bytes for every <, > and & — json.Marshal escapes
// them as \u003c, \u003e, \u0026.
func rerankWireStringLen(s string) int {
	n := embed.WireStringLen(s)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<', '>', '&':
			n += len(`\u003c`) - 1
		}
	}
	return n
}

// rerankWireBodyLen is the exact length of the body rerank.Score sends for
// model, query and docs (at least one document).
func rerankWireBodyLen(model, query string, docs []string) int {
	n := rerankWireFrame + rerankWireStringLen(model) + rerankWireStringLen(query) + max(len(docs)-1, 0)
	for _, d := range docs {
		n += rerankWireStringLen(d)
	}
	return n
}

// rerankWireBodyNum/rerankWireBodyDen is what rerank.Score allocates per
// byte of its body to build it: json.Marshal (encoding/json over its v2
// implementation in this toolchain) appends into a buffer that grows by a
// quarter at a time from empty — the encode state's pool is empty after a
// collection — and copies the result out: at most 5 × 1.25 + 1 = 7.25 × the
// body. Measured cold (TotalAlloc of rerank.Score against an upstream that
// allocates nothing, the encoder's share alone, bau report W02-10
// §Wire-Term): 5.58–7.05 × the body at 0.5 MB and more over the 1/128/1000
// × 64 B/4 KiB/64 KiB matrix, a saw tooth of 5.77–7.085 × between its
// points (the largest 7.085 × at 7 427 102 B). A warm pool is no upper
// bound and is not booked: the encoder pool (encoding/json/jsontext, one
// sync.Pool for the process) drops an under-used large buffer after four
// strikes and remembers the length of the encode that dropped it, and the
// next json.Marshal on that entry allocates the next power of two above that
// length up front, however small its own body — up to the size of an
// earlier Marshal anywhere in the process, unbooked (external review 2 F-3:
// 8 429 960 B for a 169 B body against 85 708 B booked). Only a wire that
// writes its body without json.Marshal removes it (the hardening wave of the
// rerank wire, before the flip). Not a fixed multiple of the documents'
// bytes: json.Marshal writes <, > and & as six bytes and a control
// character as up to six (38.2 × the documents at 128 × 4 KiB of "<"), so
// the factor applies to the body's exact length (rerankWireBodyLen). A
// derived mechanism constant of the wire as it is, not policy;
// TestRerankWireCallReserveIsTheMeasuredOne fails when a form needs more or
// when the largest needs much less.
const (
	rerankWireBodyNum = 29
	rerankWireBodyDen = 4
)

// rerankWireDocBytes is what one wire call allocates per document to read
// and decode its answer: the answer's entry as the backend sends it (about
// 50 bytes of {"index":i,"relevance_score":x}), read by io.ReadAll, which
// grows its buffer by doubling, decoded by json.Unmarshal (the results slice
// grows by doubling, every score a pointer of its own), the scores and their
// coverage flags — 172.7 bytes per document measured (2 880 + 172.7 × n over
// n = 1 … 1000), rounded up to 176. An answer with more or longer entries
// than the documents' is not covered: a backend that sends the 1-MiB read
// cap of malformed entries makes the decode allocate up to 126.7 MB
// (§Wire-Term, stop finding S-W1 — the hand-over to the byte writer and
// strict decoder of the rerank wire).
const rerankWireDocBytes = 176

// rerankWireCallBytes is what one wire call allocates beside its body, its
// copy and its documents — the request, its headers and the hop stamp, the
// transport's connection state and the answer's reader (43.4 KB measured
// with a bare client), rerank.Score's own (1.1 KB), the answer decoder's
// fixed state (2.9 KB) and the encode state json.Marshal allocates for any
// body (2.7 KB): 50.1 KB in all, the largest small-body call measured at
// 51 184 B — rounded up to 52 KiB. Measured (§Wire-Term).
const rerankWireCallBytes = 52 << 10

// rerankWireRetryBytes is what the retry of a wire call allocates beside
// the second copy of its body (httpx.DoRetryOnce resends the same body on a
// fresh connection; the body is not built again): the connection's state —
// embedWireRetryBytes, measured at the embeddings route over the same
// httpx.DoRetryOnce (FOLGE-E hardening 2), whose hand-over Ü-H2-2 named the
// term missing at this route; not measured here again.
const rerankWireRetryBytes = embedWireRetryBytes

// rerankWireCall is what one wire call of a body of body bytes over docs
// documents allocates at most, its retry included: the body's build
// (rerankWireBodyNum/rerankWireBodyDen × the body, rounded up), the
// transport's copy of the body onto the connection (embed.WireCopyBuffer,
// or the body's length when shorter), rerankWireDocBytes per document,
// rerankWireCallBytes, and for the retry the copy once more and
// rerankWireRetryBytes. The retry is booked with the first call, whether it
// happens or not: httpx.DoRetryOnce decides it inside rerank.Score (the
// embeddings route's rule, FOLGE-E hardening 2).
func rerankWireCall(body, docs int) int64 {
	b := int64(body)
	copied := min(b, embed.WireCopyBuffer)
	return (rerankWireBodyNum*b+rerankWireBodyDen-1)/rerankWireBodyDen + copied + int64(docs)*rerankWireDocBytes +
		rerankWireCallBytes + copied + rerankWireRetryBytes
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
// before the acquire, whatever its blocks: the build of every document and
// its separator and of the query once, each as the wire encodes it, at the
// body's factor, and every document's rerankWireDocBytes — at most what
// rerankUpstreamBooking books for any split of the documents.
func rerankUpstreamTexts(req *rerankRequest) int64 {
	n := int64(rerankWireStringLen(req.query))
	for _, d := range req.wire {
		n += int64(rerankWireStringLen(d)) + 1
	}
	return (rerankWireBodyNum*n+rerankWireBodyDen-1)/rerankWireBodyDen + int64(len(req.wire))*rerankWireDocBytes
}

// The terms of the rerank routes as the configuration's budget floor reads
// them (W02-10, as FOLGE-E's Embed* exports): their reader is the budget
// dimensioning (V39) before the flip — V39 counts none of them yet — and
// the wire pin outside internal/oai (internal/oai/rerankwire), which
// rebuilds rerankWireCall from them. RerankResultReserve per document and
// RerankEncodeBuffer are the answer's share; RerankDocSlotReserve per
// document beside its decoded text the parse's; per wire call
// RerankWireBodyNum/RerankWireBodyDen × its body, RerankWireCopyReserve (or
// the body's length when shorter) twice — the call and its retry —,
// RerankWireDocReserve per document, RerankWireCallReserve and
// RerankWireRetryReserve.
const (
	RerankResultReserve    = rerankResultBytes
	RerankEncodeBuffer     = rerankEncodeBuffer
	RerankDocSlotReserve   = rerankDocSlotBytes
	RerankWireBodyNum      = rerankWireBodyNum
	RerankWireBodyDen      = rerankWireBodyDen
	RerankWireCopyReserve  = embed.WireCopyBuffer
	RerankWireDocReserve   = rerankWireDocBytes
	RerankWireCallReserve  = rerankWireCallBytes
	RerankWireRetryReserve = rerankWireRetryBytes
)
