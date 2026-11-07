package llm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/httpx"
)

// The relay reader of FOLGE-S (RC-D, cluster C8; W02-08 M-1, external
// review 5 point 5): what one relayed stream frame costs the reader.
//
//   - readSSELineInto reads one line into the caller's buffer, reused for
//     the whole stream and grown only to the largest line, by doubling and
//     never past the frame cap: k lines of at most F bytes cost at most
//     about 2 F together, where readSSELineMax costs 5–6 F per line (append
//     over 4-KiB ReadSlice pieces growing by a quarter, then string(line)).
//   - sseFrame keeps a frame of one data line as a slice of that line (no
//     copy); only a frame of several lines is joined into a buffer of its
//     own (sseFrame.addBytes).
//   - decodeChunkMeta reads what the relay keeps of a chunk — id, model,
//     the first choice's finish_reason, usage, timings, error — without
//     decoding the generated text (streamChunk copies Delta.Content: 2 F).
//   - ChatRawStream's onFrame payload is valid only during the callback: it
//     aliases the line buffer, which the next line overwrites.

// sseLineMin is the first capacity of a line buffer: a token frame of a
// chat stream (a few hundred bytes) fits it, a larger line doubles it.
const sseLineMin = 512

// growLine returns line with room for n more bytes: the capacity doubles
// (from sseLineMin) until the bytes fit, and never exceeds limit — Go's
// append grows a large slice by a quarter only, which made a line of F
// bytes cost about 5 F. The caller has checked len(line)+n <= limit.
func growLine(line []byte, n, limit int) []byte {
	need := len(line) + n
	if need <= cap(line) {
		return line
	}
	c := max(2*cap(line), sseLineMin)
	for c < need {
		c *= 2
	}
	c = max(min(c, limit), need)
	grown := make([]byte, len(line), c)
	copy(grown, line)
	return grown
}

// readSSELineInto reads one SSE line (its terminator included) into
// line[:0] and returns it with the reader's error, exactly like
// readSSELineMax returns its string: a line over limit is
// httpx.ErrResponseTooLarge (and an empty line), a line cut by any other
// error comes back with it. The buffer is the caller's to hand back on the
// next call; it grows only when a line does not fit (growLine).
func readSSELineInto(br *bufio.Reader, line []byte, limit int) ([]byte, error) {
	line = line[:0]
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return line[:0], fmt.Errorf("%w: SSE line over %d bytes", httpx.ErrResponseTooLarge, limit)
		}
		line = append(growLine(line, len(chunk), limit), chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// chunkMeta is what the relay reads of one upstream chunk.
type chunkMeta struct {
	ID, Model, FinishReason string
	Usage                   *usageBody
	Timings                 *timingsBody
	Error                   *streamErrorBody
}

// decodeChunkMeta reads payload's chunkMeta: the same values and the same
// error as json.Unmarshal into streamChunk (the base relay's decode), but
// the generated text — content, reasoning, tool calls — is checked, never
// copied, and nothing is allocated beyond the values kept
// (TestDecodeChunkMetaSkipsTheText: a 1-MiB chunk costs no more than 4 KiB;
// encoding/json alone costs 2 KiB of decoder state whenever a GC emptied its
// pool, and its first use 50 KiB). The walk (walkChunkMeta) takes the
// chunks engines send; whatever it does not take with certainty — a syntax
// or type error, an escaped or case-variant key, a repeated field, an
// escaped or non-UTF-8 string value, an error object — goes through the
// base decode, so the answer and the error text are the base's word for
// word (FuzzDecodeChunkMetaIsTheBase).
func decodeChunkMeta(payload []byte) (chunkMeta, error) {
	if m, ok := walkChunkMeta(payload); ok {
		return m, nil
	}
	var c streamChunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return chunkMeta{}, err
	}
	m := chunkMeta{ID: c.ID, Model: c.Model, Usage: c.Usage, Timings: c.Timings, Error: c.Error}
	if len(c.Choices) > 0 {
		m.FinishReason = c.Choices[0].FinishReason
	}
	return m, nil
}
