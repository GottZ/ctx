//go:build !race

package rerank

// The read's bounds (RC-D S2 FOLGE-R) — production build only: race
// instrumentation changes what allocates (folge_r_alloc_test.go's rule).

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// chunks is a reader without a length that hands out its bytes in small
// reads (the transport's body of a chunked or decompressed answer).
func chunks(b []byte) io.Reader { return iotest.HalfReader(bytes.NewReader(b)) }

// TestReadAnswerStaysWithinItsCap: an answer of a known length within the
// cap is one allocation of that length, one past it is refused unread; an
// answer without a length is read into a buffer of an answer without an
// echo, grown once to the cap for an echo, refused past the cap — and the
// read allocates at most AnswerReadMax.
func TestReadAnswerStaysWithinItsCap(t *testing.T) {
	const docs, body = 4, 10 << 10
	limit := answerReadCap(docs, body)
	for _, c := range []struct {
		name    string
		size    int64
		known   bool
		refused bool
	}{
		{"known, small", 300, true, false},
		{"known, at the cap", limit, true, false},
		{"known, past the cap", limit + 1, true, true},
		{"unknown, small", 300, false, false},
		{"unknown, at the first buffer", limit - body, false, false},
		{"unknown, an echo", limit - 100, false, false},
		{"unknown, at the cap", limit, false, false},
		{"unknown, past the cap", limit + 1, false, true},
	} {
		raw := bytes.Repeat([]byte{'x'}, int(c.size))
		r := chunks(raw)
		cl := int64(-1)
		if c.known {
			cl, r = c.size, bytes.NewReader(raw)
		}
		var got []byte
		var err error
		b, m := folgeRMeasure(true, func() { got, err = readAnswer(r, cl, docs, body) })
		t.Logf("FOLGE-R read: %-28s %d B: allocated %d B in %d, err=%v (cap %d, AnswerReadMax %d)", c.name, c.size, b, m, err, limit, AnswerReadMax(docs, body))
		if c.refused {
			if err == nil {
				t.Errorf("%s: %d B read, want a refusal past the %d-byte cap", c.name, c.size, limit)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s: read %d B, err %v — want the %d B answer", c.name, len(got), err, c.size)
		}
		if b > uint64(AnswerReadMax(docs, body)) {
			t.Errorf("%s: the read allocated %d B, AnswerReadMax is %d B", c.name, b, AnswerReadMax(docs, body))
		}
		// An answer without an echo and without a length takes the first
		// buffer only (the cap less the body), not the cap.
		if !c.known && c.size <= limit-body && b > uint64(limit-body+AnswerReadSlack) {
			t.Errorf("%s: an answer of %d B without a length allocated %d B, want its first buffer of %d B", c.name, c.size, b, limit-body)
		}
	}
	if _, err := readAnswer(iotest.ErrReader(errors.New("boom")), -1, docs, body); err == nil {
		t.Errorf("a failing reader read without an error")
	}
}
