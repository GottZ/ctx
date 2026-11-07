package rerank

import (
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/embed"
)

// The wire body and the answer's read of Score (FOLGE-R, RC-D; the hand-over
// of W02-10's stop findings S-W1 and S-W2 and of its external review 2, F-1
// and F-3):
//
//   - wireBody writes Score's body in one allocation of its exact length
//     (RequestBodyLen), without json.Marshal: the encoder's pooled buffer
//     grows a quarter at a time and is copied out (up to 7.25 × the body
//     cold), and a warm pool allocates up to the size of an earlier Marshal
//     anywhere in the process (W02-10 review 2 F-3: 8 429 960 B for a 169 B
//     body). The bytes are encoding/json's without HTML escaping
//     (embed.WireStringLen's encoding: <, > and & as one byte, not six —
//     W02-10 measured 38.2 × the documents' bytes for "<"), the members in
//     the order model, query, documents, return_documents, no trailing
//     newline.
//   - The body carries "return_documents":false (F-1): a ctx peer's
//     /v1/rerank (Jina dialect) and the Jina API echo every document by
//     default; llama.cpp, TEI and vLLM ignore the member, Infinity, Voyage and
//     Cohere v1 know it and default to false (bau report RCD-c8-hardening-S1
//     §Vertrag). vLLM echoes every document whatever the member says — the
//     decoder skips an echo, the read has room for it.
//   - readAnswer reads at most answerReadCap bytes of an answer — what the
//     route books for the call, not a fixed 1 MiB: the body's length (room
//     for an echo of every document) plus AnswerEntryMax per document plus
//     AnswerFixed. A longer answer is refused at the read (the caller fails
//     open); AnswerReadMax is what the read allocates at most.
//   - decodeAnswer (raw_answer.go) is the strict decoder.

// errSurplusResults is the strict decoder's refusal of an answer with an
// entry past the documents (raw_answer.go; internal/embed's
// errSurplusEmbeddings at the embed wire): such an answer cannot cover the
// documents once each, and its surplus entries are never decoded.
var errSurplusResults = errors.New("rerank: more results than documents")

// wireFrame is the frame of Score's body around the model, the query and the
// documents: {"model":…,"query":…,"documents":[…],"return_documents":false}.
const wireFrame = len(`{"model":,"query":,"documents":[],"return_documents":false}`)

// RequestBodyLen is the exact length of the body Score sends for model,
// query and docs: the frame, the model, the query and every document as
// embed.WireStringLen counts them, a comma between two documents. The
// gateway's rerank route books the body's build at it (internal/oai
// rerankWireBodyLen, factor 1).
func RequestBodyLen(model, query string, docs []string) int {
	n := wireFrame + embed.WireStringLen(model) + embed.WireStringLen(query) + max(len(docs)-1, 0)
	for _, d := range docs {
		n += embed.WireStringLen(d)
	}
	return n
}

// wireBody is Score's request body for model, query and docs, written into
// one allocation of RequestBodyLen. Sending "documents" (not "texts") and
// "query" selects the Jina/cohere answer, whose score field is
// "relevance_score"; top_n is left out, so the server answers one entry per
// document (still sorted by score — see Score's re-alignment). The
// documents are always a list (Score sends no body for no documents). A body that does not fill its allocation
// exactly is an error — the length and the writer disagree, and a body
// grown past its allocation is what the route did not book.
func wireBody(model, query string, docs []string) ([]byte, error) {
	n := RequestBodyLen(model, query, docs)
	b := make([]byte, 0, n)
	b = append(b, `{"model":`...)
	b = appendWireString(b, model)
	b = append(b, `,"query":`...)
	b = appendWireString(b, query)
	b = append(b, `,"documents":[`...)
	for i, d := range docs {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendWireString(b, d)
	}
	b = append(b, `],"return_documents":false}`...)
	if len(b) != n || cap(b) != n {
		return nil, fmt.Errorf("rerank: the wire body is %d B in %d B, its length is %d B", len(b), cap(b), n)
	}
	return b, nil
}

// appendWireString appends s to dst as embed.WireStringLen counts it — the
// encoding of encoding/json without HTML escaping (internal/embed
// appendWireString, unexported there): runs that need no escape are copied
// in one piece; a quote and a backslash take a backslash, \b \f \n \r \t
// their short escapes, every other control character and U+2028/U+2029
// \u00XX/\u202X, an invalid byte U+FFFD.
func appendWireString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= ' ' && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == ' ' || r == ' ':
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0xf])
		case r == utf8.RuneError && size == 1:
			dst = append(dst, s[start:i]...)
			dst = append(dst, "�"...)
		default:
			i += size
			continue
		}
		i += size
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// AnswerEntryMax is the longest entry a rerank backend writes per document
// without echoing it: llama.cpp {"index":i,"relevance_score":x} (49–58 B with
// a five-digit index and a float at seventeen digits), Voyage and Cohere v1
// the same members, vLLM's entry around its document echo
// {"index":i,"document":{"text":…,"multi_modal":null},"relevance_score":x}
// (95–101 B beside the text) — measured on those forms with the longest
// float literal (TestAnswerReadCapHoldsTheDialects), rounded up to a power of
// two. A mechanism constant of the wire forms, not policy.
const AnswerEntryMax = 128

// AnswerFixed is the longest frame of an answer around its entries: the
// list's member and brackets, the id, the model name (up to the length a
// model alias can carry), object, usage with its counters, Cohere v1's meta
// with api_version and billed_units, whitespace of a pretty-printing server
// — under 1 KiB on every measured form (TestAnswerReadCapHoldsTheDialects),
// booked at 4 KiB. A mechanism constant, not policy.
const AnswerFixed = 4 << 10

// answerReadCap is the most bytes Score reads of an answer to docs documents
// whose request body was body bytes long: room for an echo of every
// document (the body's length — a backend echoes the texts it was sent),
// AnswerEntryMax per document and AnswerFixed. A longer answer is refused.
func answerReadCap(docs, body int) int64 {
	return int64(body) + int64(docs)*AnswerEntryMax + AnswerFixed
}

// AnswerReadSlack is what one buffer of the read takes beyond its length:
// the runtime's size class (embed.WireBodySlack — at most 4 KiB below
// 32 KiB, less than an 8 KiB page above). Not policy.
const AnswerReadSlack = embed.WireBodySlack

// AnswerReadMax is what the read of an answer to docs documents over a body
// of body bytes allocates at most (readAnswer): one buffer of the header's
// length when it names one within answerReadCap; else a first buffer of an
// answer without an echo (answerReadCap less the body) and, when the answer
// goes on, one buffer of answerReadCap — the body's length once, the
// entries' and the frame's share twice, and AnswerReadSlack for each of
// the two buffers. The gateway's rerank route books it with every wire call
// (internal/oai rerankWireCall).
func AnswerReadMax(docs, body int) int64 {
	c := answerReadCap(docs, body)
	return c + c - int64(body) + 2*AnswerReadSlack
}

// readAnswer reads an answer of contentLength bytes (-1: unknown) from r,
// at most answerReadCap(docs, body) of them: a known length past the cap is
// refused before a byte is read, a known length within it is read into one
// buffer of that length; an unknown length is read into a buffer of an
// answer without an echo, grown once to the cap when the answer goes on,
// and refused when it goes on past the cap.
func readAnswer(r io.Reader, contentLength int64, docs, body int) ([]byte, error) {
	limit := answerReadCap(docs, body)
	if contentLength > limit {
		return nil, fmt.Errorf("the answer's %d bytes exceed its %d-byte cap", contentLength, limit)
	}
	if contentLength >= 0 {
		b := make([]byte, contentLength)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return b, nil
	}
	b := make([]byte, 0, limit-int64(body))
	for {
		if len(b) == cap(b) {
			// Full: one more read tells an answer that ended from one that
			// goes on — into the cap, or past it.
			var probe [1]byte
			n, err := r.Read(probe[:])
			if n == 0 {
				if err == nil || errors.Is(err, io.EOF) {
					return b, nil
				}
				return nil, err
			}
			if int64(cap(b)) >= limit {
				return nil, fmt.Errorf("the answer is longer than its %d-byte cap", limit)
			}
			grown := make([]byte, len(b), limit)
			copy(grown, b)
			b = append(grown, probe[0])
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if errors.Is(err, io.EOF) {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
