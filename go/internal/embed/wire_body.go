package embed

import (
	"strconv"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
)

// The gateway body of an embed wire call (FOLGE-E hardening 1, S-1).
// wireInput.marshal writes a gateway sub-block's body in one allocation of
// its exact length: wireBodyLen sizes it before a byte is written,
// appendWireBody writes it into that capacity. The bytes are the ones the
// json.Encoder of W02-09 hardening H-2 wrote (no HTML escaping, its
// trailing newline cut) — TestWireBodyIsTheEncoders and
// FuzzWireBodyIsTheEncoders keep that encoder as the oracle of the wire.
// The encoder itself cannot write in one allocation: Encoder.Encode encodes
// into a pooled buffer of its own, which grows a quarter at a time, and
// copies the result into its writer — up to 7.105 × the body (255 texts,
// 24 MiB), unchanged by a bytes.Buffer grown to the body's length in front
// of it; encoding/json/v2's MarshalWrite into such a buffer grows it once
// more after the last byte (3.0 ×) — bau report FOLGE-E §Härtung 1.

// WireFrameMax is the longest frame of a gateway body around its model and
// its texts: the openai wire's {"model":,"input":[]} and the ollama wire's
// options with the longest num_ctx a row can carry (written only when
// positive: math.MaxInt64). A body has no trailing newline. With the model
// and every text as WireStringLen counts it and a separator between two
// texts, it bounds WireBodyLen on every row — exactly on an ollama row of
// that num_ctx.
const WireFrameMax = len(`{"model":,"input":[],"options":{"num_ctx":}}`) + len("9223372036854775807")

// WireBodySlack is what the one allocation of a gateway body takes beyond
// its length: the runtime rounds it up to a size class — at most 4 KiB
// below 32 KiB, less than one 8 KiB page above. Derived from the runtime's
// allocation granularity, not policy; TestWireBodyIsOneAllocation measures
// one allocation per body and its rounding below this.
const WireBodySlack = 8 << 10

// WireStringLen is the length of s as a gateway body writes it — the
// encoding of encoding/json without HTML escaping: quotes around it, a quote
// and a backslash escaped to two bytes, \b \f \n \r \t to two, every other
// control character to six (\u00XX), U+2028 and U+2029 to six, an invalid
// byte to the three bytes of U+FFFD; every other byte as it is.
func WireStringLen(s string) int {
	n := len(`""`)
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
				n += len(`\n`)
			case c < ' ':
				n += len(`\u0000`)
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\u2028' || r == '\u2029':
			n += len(`\u2028`)
		case r == utf8.RuneError && size == 1:
			n += len("\ufffd")
		default:
			n += size
		}
		i += size
	}
	return n
}

// WireBodyLen is the exact length of the body EmbedRaw sends row b for
// model and texts: on the openai wire {"model":…,"input":[…]}, on every
// other protocol (the ollama wire, embedWire's choice) the same with
// options.num_ctx when b.NumCtx is set (embedOllama's rule).
func WireBodyLen(b backends.Backend, model string, texts []string) int {
	if b.Protocol != backends.ProtocolOpenAI && b.NumCtx > 0 {
		return wireBodyLen(model, texts, &ollamaOpts{NumCtx: b.NumCtx})
	}
	return wireBodyLen(model, texts, nil)
}

// wireBodyLen is the exact length appendWireBody writes for model, texts
// and the ollama options opts (nil: none).
func wireBodyLen(model string, texts []string, opts *ollamaOpts) int {
	n := len(`{"model":,"input":}`) + WireStringLen(model)
	if texts == nil {
		n += len("null")
	} else {
		n += len("[]") + max(len(texts)-1, 0)
		for _, s := range texts {
			n += WireStringLen(s)
		}
	}
	if opts != nil {
		n += len(`,"options":{}`)
		if opts.NumCtx != 0 {
			var digits [20]byte
			n += len(`"num_ctx":`) + len(strconv.AppendInt(digits[:0], int64(opts.NumCtx), 10))
		}
	}
	return n
}

// appendWireBody appends the gateway body of model, texts and opts to dst:
// the fields in the order and the form the encoder wrote them for
// ollamaEmbedRequest and openAIEmbedRequest — a nil list as null, the
// options only when set, num_ctx only when not zero (omitempty).
func appendWireBody(dst []byte, model string, texts []string, opts *ollamaOpts) []byte {
	dst = append(dst, `{"model":`...)
	dst = appendWireString(dst, model)
	dst = append(dst, `,"input":`...)
	if texts == nil {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i, s := range texts {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendWireString(dst, s)
		}
		dst = append(dst, ']')
	}
	if opts != nil {
		dst = append(dst, `,"options":{`...)
		if opts.NumCtx != 0 {
			dst = append(dst, `"num_ctx":`...)
			dst = strconv.AppendInt(dst, int64(opts.NumCtx), 10)
		}
		dst = append(dst, '}')
	}
	return append(dst, '}')
}

// appendWireString appends s to dst as WireStringLen counts it: runs that
// need no escape are copied in one piece.
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
		case r == '\u2028' || r == '\u2029':
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0xf])
		case r == utf8.RuneError && size == 1:
			dst = append(dst, s[start:i]...)
			dst = append(dst, "\ufffd"...)
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
