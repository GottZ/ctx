package llm

import (
	"bufio"
	"encoding/json"
)

// The relay reader of FOLGE-S (RC-D, cluster C8; W02-08 M-1, external
// review 5 point 5): what one relayed stream frame costs the reader. This
// file is the contract in its first layer — each name exists and answers as
// the base does; the second layer makes them cheap and puts sseRelay on
// them (bau report RCD-c8-hardening-S1 §Vertrag):
//
//   - readSSELineInto reads one line into the caller's buffer, reused for
//     the whole stream and grown only to the largest line (by doubling): k
//     lines of at most F bytes cost at most 2 F together, where
//     readSSELineMax costs 5–6 F per line (append over 4-KiB ReadSlice
//     pieces growing by a quarter, then string(line)).
//   - decodeChunkMeta reads what the relay keeps of a chunk — id, model,
//     the first choice's finish_reason, usage, timings, error — without
//     decoding the generated text (streamChunk copies Delta.Content: 2 F).
//   - ChatRawStream's onFrame payload is valid only during the callback
//     once sseRelay runs on the reused buffer: a caller copies what it keeps.

// readSSELineInto reads one SSE line (its terminator included) into
// line[:0] and returns it with the reader's error, under limit as
// readSSELineMax. RC-D S1 stub: readSSELineMax's line copied in.
func readSSELineInto(br *bufio.Reader, line []byte, limit int) ([]byte, error) {
	s, err := readSSELineMax(br, limit)
	return append(line[:0], s...), err
}

// chunkMeta is what the relay reads of one upstream chunk.
type chunkMeta struct {
	ID, Model, FinishReason string
	Usage                   *usageBody
	Timings                 *timingsBody
	Error                   *streamErrorBody
}

// decodeChunkMeta reads payload's chunkMeta. RC-D S1 stub: the base's
// decode of the whole streamChunk, generated text included.
func decodeChunkMeta(payload []byte) (chunkMeta, error) {
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
