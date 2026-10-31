package httpx

import (
	"errors"
	"fmt"
	"io"
)

// MaxResponseBytes caps one non-streaming backend response body (chat and
// embed; GHSA 2026-10-31 §3: a hostile backend could otherwise stream an
// endless JSON document into json.Decoder and exhaust the shared daemon's
// memory). 16 MiB is sized from the largest legitimate answer, with margin:
// the anthropic wire's output ceiling is 128000 tokens (≈ 0.5 MB of text,
// ≈ 3 MB if a provider \u-escaped every byte); logprobs are only requested
// by the one-token decide call; an embed answer is one vector (≤ 8192
// floats ≈ 200 KB as JSON). Five times the worst legitimate chat body keeps
// odd providers working, and the decoder's peak per call stays in the tens of
// MiB instead of unbounded. Rerank (1 MiB) and window discovery keep their own
// tighter caps.
const MaxResponseBytes int64 = 16 << 20

// ErrResponseTooLarge marks a backend response that exceeded its byte cap.
// Callers wrap it (%w) into their own "decode"/"read stream" errors, so
// errors.Is identifies the cause; the failover classifier treats it like any
// malformed answer (stop the chain — a backend that floods is not evidence
// the next one would do better).
var ErrResponseTooLarge = errors.New("backend response exceeds the size limit")

// CapBody wraps a response body so that reading more than limit bytes fails
// with ErrResponseTooLarge instead of silently truncating: a plain
// io.LimitReader would hand the decoder a clean EOF at the cap, and a
// truncated document could even parse. A body of exactly limit bytes reads
// through unchanged.
func CapBody(r io.Reader, limit int64) io.Reader {
	return &cappedReader{r: r, left: limit, limit: limit}
}

type cappedReader struct {
	r     io.Reader
	left  int64
	limit int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.left <= 0 {
		// At the cap: one probe byte tells a body that ends exactly here
		// (EOF) from one that goes on (overflow).
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, c.limit)
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
