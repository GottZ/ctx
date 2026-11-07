package oai

import (
	"math"
	"sync"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/rerank"
)

// The budget floor's route terms (RC-D cluster C8, V39 extended): what one
// request at a body cap books at most on the embeddings and the rerank
// route, as the configuration's floor (config/validate.go
// gatewayBudgetFloor) reads it — the maximum over the routes and the two
// classes (at credentials at gateway.max_request_bytes, below it at the
// effective scan cap, EffectiveScanCap) plus UpstreamFrameReserve. Derived
// from the routes' ledger terms (embeddings.go, rerank.go, rerank_wire.go:
// the same constants and the same wire length functions), not measured:
// TestBudgetFloorHoldsTheWorstForms fails when a worst form books more than
// its floor (the bound would no longer hold) or less than 0.7 × it (the
// bound would cost budget for nothing). Every sum saturates at
// math.MaxInt64 (Fremd-Review 2 FR2-3 of the chat floor: a cap of 1 << 62
// must not wrap into a floor every budget clears).

// UpstreamFrameReserve is the room the floor keeps for the backend row's
// frame in the upstream bodies of one attempt — its model name and
// parameters, which the configuration does not know and the backends
// schema does not bound (bau report RCD-c8-hardening-S1 §Budget: the chat
// frame measured 102 B + the length of the model name; the deepest
// rewritten chat request without a declared length at a sum of the read's
// chunks got 503 at the floor from a 212-byte name on, short by its frame,
// TestDeepestDeveloperRequestLacksOnlyTheUpstreamFrame). 64 KiB holds a
// chat row's name and parameters up to 64 KiB − 102 B; the embeddings and
// rerank routes write the name into every block's body, so at the block
// ceiling (every input or document its own block) it holds a name of up
// to 256 B at embeddings (256 blocks) and of 32 B at rerank (1000 blocks;
// each block books its body twice — the build and the answer's room for an
// echo, FOLGE-R) — a longer name at that ceiling books past the floor by the
// rest.
// A named constant of the floor, not policy: the policy is
// gateway.inflight_budget.
const UpstreamFrameReserve = 64 << 10

// floorRatio is a ratio num/den ≥ 0 of bytes per raw body byte.
type floorRatio struct{ num, den int64 }

// above reports r > o.
func (r floorRatio) above(o floorRatio) bool { return r.num*o.den > o.num*r.den }

// of is ⌈n × r⌉, saturating.
func (r floorRatio) of(n int64) int64 {
	p := floorMul(n, r.num)
	if p == math.MaxInt64 {
		return p
	}
	return (p + r.den - 1) / r.den
}

// floorStringRates are the most bytes a JSON string's decode yields and the
// most bytes a wire writes, each per raw body byte of the string, over every
// form a raw byte can take in a JSON string: a rune written as itself (one
// to four bytes), a byte of invalid UTF-8 (the decode writes U+FFFD, three
// bytes, for each), a two-character escape (\" \\ \/ \b \f \n \r \t) and a
// \u escape (six bytes; a surrogate pair twelve; a lone surrogate decodes to
// U+FFFD). wireLen is the wire's length of a string, quotes included. Runes
// are written independently of their neighbours by both wires, so the rates
// bound a whole string, and a body's strings are at most its bytes.
func floorStringRates(wireLen func(string) int) (decoded, wire floorRatio) {
	decoded, wire = floorRatio{0, 1}, floorRatio{0, 1}
	quotes := wireLen("")
	form := func(raw int, d string) {
		if r := (floorRatio{int64(len(d)), int64(raw)}); r.above(decoded) {
			decoded = r
		}
		if r := (floorRatio{int64(wireLen(d) - quotes), int64(raw)}); r.above(wire) {
			wire = r
		}
	}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		d := string(r) // a surrogate is U+FFFD, as the decode writes it
		switch {
		case r > 0xffff:
			form(12, d) // \uXXXX\uXXXX
		default:
			form(6, d) // \uXXXX
		}
		if r >= 0x20 && r != '"' && r != '\\' && utf8.ValidRune(r) {
			form(utf8.RuneLen(r), d)
		}
	}
	for _, c := range `"\/` + "\b\f\n\r\t" {
		form(2, string(c))
	}
	form(1, string(utf8.RuneError)) // one byte of invalid UTF-8
	return decoded, wire
}

// embedFloorRates are the string rates of both gateway wires: the rerank
// wire writes its strings in embed.WireStringLen's encoding too (FOLGE-R:
// without HTML escaping, rerank.RequestBodyLen).
var embedFloorRates = sync.OnceValues(func() (floorRatio, floorRatio) { return floorStringRates(embed.WireStringLen) })

// rerankFloorFrame is the rerank wire body's frame around the model, the
// query and one document — {"model":,"query":,"documents":[],
// "return_documents":false} —, read off the wire's own length function.
var rerankFloorFrame = sync.OnceValue(func() int64 {
	return int64(rerank.RequestBodyLen("", "", []string{""}) - 3*embed.WireStringLen(""))
})

// EmbedRequestFloor is the most one embeddings request with a body of at
// most bodyCap bytes books (scanned: to an alias below credentials, whose
// sensitivity scan is booked), for gateway.embedding_max_floats maxFloats —
// its whole reservation, the route's own answer and decode shares included.
// The worst form it bounds: a body read without a declared length; every
// string byte a byte of invalid UTF-8 (decoded to three bytes, written as
// three); the inputs at modelalias.CeilingEmbeddingMaxInputs, each its own
// block (the row's external capacity 1: a block is min(inputs, sub_batch,
// capacity), handler.ExternalWeightPlan, and embedding.sub_batch alone
// splits into at most modelalias.MaxSubBlocks); an alias without
// embedding.native_dims (the answer and the decode at the floats ceiling —
// with a native dimension modelalias.Ceiling keeps max_inputs × native_dims
// within maxFloats). The terms:
//
//   - the body and its read beyond a declared one (ChunkedReadReserve), the
//     JSON check's nesting state (JSONStateReserve);
//   - the decoded texts and EmbedInputSlotReserve per input;
//   - per attempt the upstream bodies — the texts as the wire writes them,
//     a separator per input and embed.WireFrameMax per block (the row's
//     model name is UpstreamFrameReserve's) — EmbedUpstreamSlack,
//     EmbedWireCallReserve and EmbedWireRetryReserve per block, and the
//     copy of every block's body onto the connection twice (the call and its
//     retry; embed.WireCopyBuffer or the block's length: at most the bodies'
//     sum);
//   - below credentials the scan (scanBooking at the decoder's depth limit);
//   - the ledger's request share is the larger of the stage's
//     RequestReserveFactor × the body and what the route spent — the sum
//     here always passes it (the body, its read term and its decoded texts
//     alone are over 4 × the body);
//   - the answer, EmbedFloatBytes × maxFloats + EmbedEncodeBuffer, and the
//     decode of a block's answer, embedDecodeFactor × its read cap at the
//     floats ceiling (httpx.MaxResponseBytes at most).
//
// One attempt: every failover attempt books its upstream bodies once more
// (as the chat floor's promise, V39). The log text of gateway.body_logging
// is booked only when it fits and is skipped otherwise — no refusal.
func EmbedRequestFloor(bodyCap int64, scanned bool, maxFloats int64) int64 {
	if bodyCap <= 0 || maxFloats <= 0 {
		return 0
	}
	n := int64(modelalias.CeilingEmbeddingMaxInputs) // inputs, and blocks at most
	decoded, wire := embedFloorRates()
	bodies := floorAdd(wire.of(bodyCap), n, floorMul(n, int64(embed.WireFrameMax)))
	request := floorAdd(
		bodyCap, ChunkedReadReserve(bodyCap), JSONStateReserve,
		decoded.of(bodyCap), floorMul(n, EmbedInputSlotReserve),
		bodies, floorMul(n, EmbedUpstreamSlack+EmbedWireCallReserve+EmbedWireRetryReserve),
		floorMul(2, min(bodies, floorMul(n, EmbedWireCopyReserve))),
	)
	if scanned {
		request = floorAdd(request, floorScan(bodyCap))
	}
	answer := floorAdd(floorMul(EmbedFloatBytes, maxFloats), EmbedEncodeBuffer)
	readCap := min(floorAdd(floorMul(maxFloats, embedWireComponentBytes), floorMul(n, embedWireItemBytes), embedWireAnswerBytes), httpx.MaxResponseBytes)
	return floorAdd(request, answer, floorMul(embedDecodeFactor, readCap))
}

// RerankRequestFloor is the most one rerank request with a body of at most
// bodyCap bytes books (scanned: below credentials) — its whole reservation;
// the rerank route books every part, its answer included, through the
// request's ledger. The worst form it bounds: a body read without a
// declared length; every string byte the form whose decoded bytes plus its
// wire bytes at the build factor (RerankWireBodyNum/RerankWireBodyDen) are
// the most — the rates of the wire's own length function, so the floor
// moves with the wire; modelalias.CeilingRerankMaxDocuments documents, each
// its own block (the row's external capacity 1, as at EmbedRequestFloor).
// The terms:
//
//   - the body and its read beyond a declared one, the JSON check's nesting
//     state;
//   - the decoded model, query and documents and RerankDocSlotReserve per
//     document; the answer, RerankResultReserve per document and
//     RerankEncodeBuffer;
//   - per attempt every block's wire call (rerankWireCall): its body built at
//     the build factor and RerankWireBodySlack — the texts as the wire writes
//     them, a separator per document and per block the wire's frame and the
//     quotes of the model and the query (the row's model name is
//     UpstreamFrameReserve's) —, the body's length once more for the
//     answer's room for an echo (rerank.AnswerReadMax), its copy onto the
//     connection twice (the call and its retry, RerankWireCopyReserve or the
//     block's length), RerankWireDocReserve per document (the answer's
//     entries read twice and their decode), RerankWireCallReserve (the
//     answer's frame and rounding, the call's fixed state) and
//     RerankWireRetryReserve per block;
//   - below credentials the scan;
//   - the stage's RequestReserveFactor × the body is below the sum, as at
//     EmbedRequestFloor.
//
// The query's text is counted once: every block's body carries the whole
// query, so a request of k blocks builds it k times — a long query over
// many blocks books more than this floor (each further copy at the build
// factor), a named limit of the floor (docs/api.md); the documents carry
// the body's bytes in the worst form. One attempt, as EmbedRequestFloor; the
// log text is booked only when it fits.
func RerankRequestFloor(bodyCap int64, scanned bool) int64 {
	if bodyCap <= 0 {
		return 0
	}
	n := int64(modelalias.CeilingRerankMaxDocuments) // documents, and blocks at most
	decoded, wire := embedFloorRates()
	quotes := int64(embed.WireStringLen(""))
	texts := floorAdd(wire.of(bodyCap), n, floorMul(n, rerankFloorFrame()+2*quotes))
	// ⌈f × L_b⌉ per block: at most f × Σ L_b + 1 B per block, and the
	// build's rounding per block.
	build := floorAdd(floorRatio{RerankWireBodyNum, RerankWireBodyDen}.of(texts), n, floorMul(n, RerankWireBodySlack))
	request := floorAdd(
		bodyCap, ChunkedReadReserve(bodyCap), JSONStateReserve,
		decoded.of(bodyCap), floorMul(n, RerankDocSlotReserve+RerankResultReserve+RerankWireDocReserve), RerankEncodeBuffer,
		build, texts, floorMul(2, min(texts, floorMul(n, RerankWireCopyReserve))),
		floorMul(n, RerankWireCallReserve+RerankWireRetryReserve),
	)
	if scanned {
		request = floorAdd(request, floorScan(bodyCap))
	}
	return request
}

// floorScan is the scan's booking of a body of n bytes at the decoder's
// depth limit (scanBooking with ScanDepthReserve).
func floorScan(n int64) int64 {
	return floorAdd(floorMul(ScanReserveFactor, n), ScanReserveBase, ScanDepthReserve)
}

// floorMul is a × b for non-negative operands, saturating at math.MaxInt64.
func floorMul(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

// floorAdd is the sum of non-negative operands, saturating at
// math.MaxInt64.
func floorAdd(xs ...int64) int64 {
	var s int64
	for _, x := range xs {
		if x > math.MaxInt64-s {
			return math.MaxInt64
		}
		s += x
	}
	return s
}
