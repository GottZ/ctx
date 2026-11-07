//go:build !race

package llm

// FOLGE-S S2 (RC-D, cluster C8): ChatRaw reads a non-streamed answer into
// one buffer — of its Content-Length when the backend sends one — and
// answers as io.ReadAll over httpx.CapBody did: the same bytes, the same
// error past the cap.

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/httpx"
)

func TestReadAnswerIsOneBufferOfItsLength(t *testing.T) {
	body := []byte(strings.Repeat("a", 1<<20))
	var got []byte
	var err error
	alloc := folgeSAlloc(func() {
		got, err = readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, 4<<20)
	})
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("readAnswer: %d B, %v", len(got), err)
	}
	if bound := uint64(len(body)) + 16<<10; alloc > bound {
		t.Errorf("an answer of %d B with its Content-Length allocated %d B, want at most %d (one buffer of its length)", len(body), alloc, bound)
	}
	alloc = folgeSAlloc(func() {
		got, err = readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: -1}, 4<<20)
	})
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("readAnswer without a length: %d B, %v", len(got), err)
	}
	if bound := uint64(2*len(body)) + 16<<10; alloc > bound {
		t.Errorf("an answer of %d B without a length allocated %d B, want at most %d (doubling)", len(body), alloc, bound)
	}
}

func TestReadAnswerAnswersAsTheBase(t *testing.T) {
	for _, c := range []struct {
		n, cl int
		limit int64
	}{{0, 0, 16}, {10, 10, 16}, {16, 16, 16}, {17, 17, 16}, {17, -1, 16}, {5000, -1, 4096}, {4096, -1, 4096}, {10, 4, 16}} {
		body := bytes.Repeat([]byte("x"), c.n)
		got, gerr := readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(c.cl)}, c.limit)
		want, werr := io.ReadAll(httpx.CapBody(bytes.NewReader(body), c.limit))
		if !bytes.Equal(got, want) || (gerr == nil) != (werr == nil) || gerr != nil && gerr.Error() != werr.Error() {
			t.Errorf("%+v: got %d B err=%v, base %d B err=%v", c, len(got), gerr, len(want), werr)
		}
	}
}
