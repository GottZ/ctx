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
	for _, size := range []int{1 << 20, 4<<20 - 4096} {
		folgeSReadAnswerCell(t, size)
	}
}

func folgeSReadAnswerCell(t *testing.T, size int) {
	body := []byte(strings.Repeat("a", size))
	var got []byte
	var err error
	alloc := folgeSAlloc(func() {
		got, err = readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, 4<<20, nil)
	})
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("readAnswer: %d B, %v", len(got), err)
	}
	t.Logf("FOLGE-S S4 read: %d B with its Content-Length allocated %d B", len(body), alloc)
	if bound := uint64(len(body)) + 8<<10 + 1<<10; alloc > bound { // a large object rounds up to its 8-KiB pages; the 512-B probe sees the end
		t.Errorf("an answer of %d B with its Content-Length allocated %d B, want at most %d (one buffer of its length)", len(body), alloc, bound)
	}
	alloc = folgeSAlloc(func() {
		got, err = readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: -1}, 4<<20, nil)
	})
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("readAnswer without a length: %d B, %v", len(got), err)
	}
	t.Logf("FOLGE-S S4 read: %d B without a length allocated %d B", len(body), alloc)
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
		got, gerr := readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(c.cl)}, c.limit, nil)
		want, werr := io.ReadAll(httpx.CapBody(bytes.NewReader(body), c.limit))
		if !bytes.Equal(got, want) || (gerr == nil) != (werr == nil) || gerr != nil && gerr.Error() != werr.Error() {
			t.Errorf("%+v: got %d B err=%v, base %d B err=%v", c, len(got), gerr, len(want), werr)
		}
	}
}

// TestReadAnswerBooksItsGrowth (FOLGE-S (d)): an answer without a length
// grows its buffer by doubling from 512 B; every buffer a growth discards
// is booked first — 512 B … 512 KiB for 1 MiB — and an answer with its
// Content-Length books nothing. A refused booking ends the read with it.
func TestReadAnswerBooksItsGrowth(t *testing.T) {
	body := []byte(strings.Repeat("a", 1<<20))
	var booked int
	book := func(n int) error { booked += n; return nil }
	if _, err := readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: -1}, 4<<20, book); err != nil {
		t.Fatal(err)
	}
	want := 0
	for c := 512; c < 1<<20; c *= 2 {
		want += c
	}
	if booked != want {
		t.Errorf("an answer of 1 MiB without a length booked %d B, want %d (the discarded doubling steps)", booked, want)
	}
	booked = 0
	if _, err := readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, 4<<20, book); err != nil || booked != 0 {
		t.Errorf("an answer with its Content-Length booked %d B (%v), want 0", booked, err)
	}
	refused := io.ErrShortBuffer
	if _, err := readAnswer(&http.Response{Body: io.NopCloser(bytes.NewReader(body)), ContentLength: -1}, 4<<20, func(int) error { return refused }); err != refused { //nolint:errorlint // the booking's own value comes back
		t.Errorf("readAnswer with a refused booking = %v, want the booking's error", err)
	}
}
