package embed

// Pins of the strict answer decode's contracts that neither the decode
// share nor the differential reaches (W02-09 hardening 7, Delta-Review 7
// F-2): each one fails under one of the reviewer's surviving mutants —
// R02 a Content-Length past the cap sizes the buffer, R05 a repeated index
// value, R10/R11/R14 a repeated embedding, list or index key, R12 an index
// literal of any length, R13 a component that does not fit the float.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestEmbedRawBufferIgnoresALengthPastTheCap (R02): a Content-Length past
// the read cap does not size the buffer — a backend that names 64 MiB
// against a 64-KiB cap gets the steps of the cap, not a 64-MiB make.
func TestEmbedRawBufferIgnoresALengthPastTheCap(t *testing.T) {
	const limit = 64 << 10
	doc := `{"data":[]}`
	buf, err := rawAnswerBuffer(strings.NewReader(doc), 1024*limit, limit)
	if err != nil || string(buf) != doc {
		t.Fatalf("%q, %v; want the object", buf, err)
	}
	if cap(buf) > limit {
		t.Errorf("a Content-Length of %d B against the %d-B cap sized the buffer at %d B", 1024*limit, limit, cap(buf))
	}
}

// TestEmbedRawRefusesRepeatedKeys (R10, R11, R14): an entry that repeats
// its embedding or its index, and an answer that repeats its list, are
// refused as such on both wires — a repeat is never "the last one wins".
func TestEmbedRawRefusesRepeatedKeys(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		proto              backends.Protocol
	}{
		{"a repeated embedding", `{"data":[{"index":0,"embedding":[1,2],"embedding":[3,4]},{"index":1,"embedding":[5,6]}]}`, "repeats its embedding", backends.ProtocolOpenAI},
		{"a repeated index", `{"data":[{"index":1,"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`, "repeats its index", backends.ProtocolOpenAI},
		{"a repeated data list", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}],"data":null}`, `repeats "data"`, backends.ProtocolOpenAI},
		{"a repeated embeddings list", `{"embeddings":[[1,2],[3,4]],"embeddings":null}`, `repeats "embeddings"`, backends.ProtocolOllama},
	} {
		e := newRawEngine(t, 2)
		e.answer = func(string, int) string { return tc.answer }
		vecs, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, tc.proto, 0), "m", []string{"a", "b"}, 0, 0)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, %v; want the refusal %q", tc.name, vecs, err, tc.want)
		}
	}
}

// TestEmbedRawWalkRefusesARepeatedIndexValue (R05): two entries of one
// index are refused by the walk itself — not only by the quality check
// after it, which a nil vector at the missing index would reach.
func TestEmbedRawWalkRefusesARepeatedIndexValue(t *testing.T) {
	answer := `{"data":[{"index":0,"embedding":[1,2]},{"index":0,"embedding":[3,4]}]}`
	vecs, _, err := decodeRawOpenAI[float32](&http.Response{ContentLength: -1}, strings.NewReader(answer), 64<<10, 2, 0)
	if err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Errorf("%v, %v; want the refusal of the repeated index", vecs, err)
	}
}

// TestEmbedRawRefusesAComponentPastTheFloat (R13): a component that does
// not fit the float is refused by the parse — at 32 bits and at 64 — not
// read as an infinity for the quality check to catch.
func TestEmbedRawRefusesAComponentPastTheFloat(t *testing.T) {
	dims := 0
	if v, err := parseVector[float32]([]byte(`[1,1e39]`), &dims, 0); err == nil {
		t.Errorf("float32: %v accepted", v)
	}
	dims = 0
	if v, err := parseVector[float64]([]byte(`[1,1e400]`), &dims, 0); err == nil {
		t.Errorf("float64: %v accepted", v)
	}
}

// TestEmbedRawIndexRefusalIsBounded (R12): an index literal longer than any
// int64 is refused before it is copied — the refusal names a prefix of it,
// never the literal (a cap-long literal would be copied twice: the string
// and strconv's clone in its error).
func TestEmbedRawIndexRefusalIsBounded(t *testing.T) {
	lit := "1" + strings.Repeat("0", 1<<20)
	var x rawIndex
	err := x.UnmarshalJSON([]byte(lit))
	if err == nil {
		t.Fatal("a 1-MiB index literal accepted")
	}
	if len(err.Error()) > 4*maxIndexLiteral {
		t.Errorf("the refusal of a %d-B index literal is %d B long, want at most %d", len(lit), len(err.Error()), 4*maxIndexLiteral)
	}
}
