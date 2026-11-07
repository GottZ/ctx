package oai_test

// The heap gate of the embeddings parse's keys (W02-09 rebase round 2, part
// A): FOLGE-P's key forms on the embeddings top level — a long key of invalid
// UTF-8 beside a short unknown one, two keys sharing a prefix of invalid
// bytes across the whole body, a repeated long key, many long escaped keys —
// in an 8-MiB body are refused with 400 at a live-heap peak of at most the
// request reserve (3 × the body) plus parseMemSlack, for one request and for
// ten at once (the helpers and the bound of TestEmbedParseHeapStaysInTheReserve,
// embed_parsemem_test.go). Before the port the parse decoded every key of at
// most five members into a Go string (encoding/json's map of the body), an
// invalid byte to the three bytes of U+FFFD, and a repeat once more
// (repeatedKey); since the port it decodes a key only into a digest and up to
// echoNameMax bytes for a refusal (levelKeys, keyIndex, decodeKey). One case
// per process under the container's GOMEMLIMIT:
//
//	go test -run 'TestEmbedParseKeysHeapStaysInTheReserve/<case>$' -count=1 ./internal/oai/

import (
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

func TestEmbedParseKeysHeapStaysInTheReserve(t *testing.T) {
	skipUnderRace(t, "the live heap of the parse over 8-MiB bodies of hostile keys (HeapInuse against 3 × the body + parseMemSlack, one request and ten at once)")
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(parseMemGoMemLimit))
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget; s.MaxRequestBytes = parseMemBodyCap })
	const head = `{"model":"emb","input":"x"`
	room := parseMemBodyCap - 256
	half := func(unit string) string { return strings.Repeat(unit, (room-len(head))/2/len(unit)) }
	escPrefix := strings.Repeat(fmt.Sprintf("%cu0061", '\\'), 1024)
	escKeys := parseMemBody(head+",", func(i int) string { return fmt.Sprintf(`"%s%07d":0`, escPrefix, 9999999-i) }, `}`, room)
	ffBeside := head + `,"a":0,"` + strings.Repeat("\xff", room-len(head)-16) + `":0}`
	ffPrefix := head + `,"` + half("\xff") + `1":0,"` + half("\xff") + `2":0}`
	utf8ffPrefix := head + `,"` + half("é\xff") + `1":0,"` + half("é\xff") + `2":0}`
	ffRepeat := head + `,"` + half("\xff") + `":0,"` + half("\xff") + `":1}`
	for _, tc := range []struct {
		name, body, code string
		n                int
	}{
		{"a long key of invalid bytes beside a short unknown key", ffBeside, oai.CodeUnsupportedParameter, 1},
		{"a long key of invalid bytes beside a short unknown key, ten at once", ffBeside, oai.CodeUnsupportedParameter, 10},
		{"two keys sharing a prefix of invalid bytes", ffPrefix, oai.CodeUnsupportedParameter, 1},
		{"two keys sharing a prefix of invalid bytes, ten at once", ffPrefix, oai.CodeUnsupportedParameter, 10},
		{"two keys sharing a prefix of é and invalid bytes", utf8ffPrefix, oai.CodeUnsupportedParameter, 1},
		{"a repeated long key of invalid bytes", ffRepeat, oai.CodeInvalidJSON, 1},
		{"a repeated long key of invalid bytes, ten at once", ffRepeat, oai.CodeInvalidJSON, 10},
		{"many long escaped keys", escKeys, oai.CodeUnsupportedParameter, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.hits.Store(0)
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			base := m.HeapInuse
			var peak atomic.Uint64
			stop, sampled := make(chan struct{}), make(chan struct{})
			go heapPeak(stop, &peak, sampled)
			answers := make([]chatAnswer, tc.n)
			var wg sync.WaitGroup
			start := time.Now()
			for i := range tc.n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					answers[i] = rig.postEmbed(t, tc.body)
				}()
			}
			wg.Wait()
			took := time.Since(start)
			close(stop)
			<-sampled
			over := int64(peak.Load()) - int64(base)
			reserve := int64(3 * len(tc.body))
			bound := int64(tc.n)*reserve + parseMemSlack
			t.Logf("W0209 keys heap: %s: %d request(s) of %d B ⇒ %d with a %d-B answer after %v; HeapInuse peak %d MiB over a base of %d MiB (reserve %d MiB each, bound %d MiB)",
				tc.name, tc.n, len(tc.body), answers[0].status, len(answers[0].body), took.Round(time.Millisecond), over>>20, base>>20, reserve>>20, bound>>20)
			for i, a := range answers {
				code, _, _ := a.envelope(t)
				if a.status != http.StatusBadRequest || code != tc.code {
					t.Fatalf("request %d: %d %s, want 400 %s", i, a.status, code, tc.code)
				}
			}
			if e.hits.Load() != 0 {
				t.Errorf("wire hits %d, want 0", e.hits.Load())
			}
			if over > bound {
				t.Errorf("HeapInuse peak %d MiB over the base, want ≤ %d × the request reserve + %d MiB = %d MiB",
					over>>20, tc.n, parseMemSlack>>20, bound>>20)
			}
		})
	}
}
