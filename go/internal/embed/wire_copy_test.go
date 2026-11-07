//go:build !race

package embed

// The measurement pin of WireCopyBuffer (FOLGE-E hardening 1): what a wire
// call allocates for the transport's copy of its body — outside -race, as
// the route's measurement pins (embed_wire_call_reserve_test.go): the race
// detector drops a share of sync.Pool's puts, so a one-byte call allocates
// more and the difference measures 22-24 KB instead of the production
// build's 32 KiB; the term books the production build.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// copyRow is an openai row on an engine that reads every body through
// io.Discard (pooled buffers, nothing per call) and answers n
// two-component vectors.
func copyRow(t *testing.T, n int) backends.Backend {
	t.Helper()
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"index":%d,"embedding":[1,2]}`, i)
	}
	answer := `{"data":[` + strings.Join(items, ",") + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return backends.Backend{Name: "row", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m", Trust: backends.TrustFull}
}

// wireCallAlloc is the TotalAlloc of one EmbedRaw call of texts beyond the
// allocation of its body: the least of wireAllocPasses averages over 50
// calls with the collector off, after a warm-up on the same connection —
// a refill of one of net/http's pools (its 32 KiB copy buffer, its bufio
// buffers) lands in some average and is not a call's own allocation.
func wireCallAlloc(t *testing.T, row backends.Backend, texts []string) (perCall float64, body int) {
	t.Helper()
	perCall = -1
	for range wireAllocPasses {
		p, n := wireCallAllocPass(t, row, texts)
		if perCall < 0 || p < perCall {
			perCall = p
		}
		body = n
	}
	return perCall, body
}

// wireCallAllocPass is one average of wireCallAlloc.
func wireCallAllocPass(t *testing.T, row backends.Backend, texts []string) (perCall float64, body int) {
	t.Helper()
	in := wireInput{texts: texts}
	_, bodyAlloc, _, err := wireBodyAlloc(in, openAIEmbedRequest{Model: "m", Input: in.value()})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := EmbedRaw(context.Background(), row, "m", texts, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	const calls = 50
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for range calls {
		if _, _, err := EmbedRaw(context.Background(), row, "m", texts, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&m1)
	return float64(m1.TotalAlloc-m0.TotalAlloc)/calls - float64(bodyAlloc), wireBodyLen("m", texts, nil)
}

// TestWireCallCopiesThroughOneBuffer: a wire call of a body past 32 KiB
// allocates WireCopyBuffer more beside its body than a call of one byte
// (more than one 4-KiB step less fails: the term is stale), and a call of a
// body below it at most the body's length more — the transport's io.Copy
// over the body limited to its Content-Length (bufio takes the body's first
// bytes behind the headers, so a short body's copy is shorter than the
// body; min(body, WireCopyBuffer) is its bound).
func TestWireCallCopiesThroughOneBuffer(t *testing.T) {
	base, _ := wireCallAlloc(t, copyRow(t, 1), []string{"a"})
	for _, tc := range []struct {
		name  string
		texts []string
	}{
		{"3 × 32 KiB", []string{strings.Repeat("a", 32<<10), strings.Repeat("a", 32<<10), strings.Repeat("a", 32<<10)}},
		{"1 MiB", []string{strings.Repeat("a", 1<<20)}},
		{"16 KiB", []string{strings.Repeat("a", 16<<10)}},
	} {
		per, body := wireCallAlloc(t, copyRow(t, len(tc.texts)), tc.texts)
		copied := per - base
		want := min(body, WireCopyBuffer)
		t.Logf("FOLGEE wire copy: %s, body %d B, %.0f B per call beside the body, %.0f B past a one-byte call (want %d)", tc.name, body, per, copied, want)
		if copied > float64(want+4<<10) || (body > 2*WireCopyBuffer && copied <= float64(want-4<<10)) {
			t.Errorf("%s: the call allocates %.0f B past a one-byte call, the transport's copy is %d B (want within one 4-KiB step)", tc.name, copied, want)
		}
	}
}
