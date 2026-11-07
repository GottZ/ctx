package oai_test

// Gates of W02-09 hardening 6 (the bot reviews of PR #157) over the route
// on the decode of a sub-block's upstream answer: every refusal of the
// strict decode (E-1 an entry past the inputs, E-2 an entry without index,
// E-3 a null component) is a malformed answer for the failover classifier —
// server_fault, 500 upstream_error, the chain stops — and an answer of
// empty entries stays within the request's bookings. The decode gates
// themselves (TotalAlloc against the decode share) live in
// internal/embed/raw_answer_test.go: the wire census keeps direct EmbedRaw
// calls out of this package.

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// openAIEntries is an OpenAI answer whose data list is the given entries.
func openAIEntries(entries ...string) string {
	return `{"object":"list","data":[` + strings.Join(entries, ",") + `],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
}

// fillTo repeats item (with a comma) between head and tail until the whole
// is about size bytes.
func fillTo(head, item, tail string, size int) string {
	k := max(1, (size-len(head)-len(tail))/(len(item)+1))
	return head + strings.TrimSuffix(strings.Repeat(item+",", k), ",") + tail
}

// TestEmbedDecodeShareFormula pins the decode share the route books at the
// points of the embed package's decode gate
// (internal/embed/raw_answer_test.go, TestEmbedRawDecodeStaysInTheDecodeShare,
// which mirrors embedDecodeFactor × embedReadCap — the wire census keeps
// direct EmbedRaw calls out of this package): a change of the formula turns
// this red and sends the change to the mirror.
func TestEmbedDecodeShareFormula(t *testing.T) {
	for _, tc := range []struct {
		n, dims   int
		cap, want int64
	}{
		{8, 1024, 210944, 632832},
		{1, 1, 4377, 13131},
		{1, 0, 16 << 20, 48 << 20},
		{2, 0, 16 << 20, 48 << 20},
	} {
		if c, s := oai.EmbedReadCap(tc.n, tc.dims, 1<<20), oai.EmbedDecodeShare(tc.n, tc.dims, 1<<20); c != tc.cap || s != tc.want {
			t.Errorf("n %d dims %d: read cap %d, decode share %d; want %d, %d (the mirror in internal/embed/raw_answer_test.go)", tc.n, tc.dims, c, s, tc.cap, tc.want)
		}
	}
}

// TestEmbedMalformedAnswersAreUpstreamErrors (E-1, E-2, E-3 over the
// route): each malformed answer of the first row is 500 upstream_error with
// the row's err class server_fault, and the chain stops — the healthy second
// row is never asked. Red before hardening 6: the answers without index and
// with a null component are served with 200.
func TestEmbedMalformedAnswersAreUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proto  backends.Protocol
		answer string
	}{
		{"entry without index", backends.ProtocolOpenAI, openAIEntries(`{"embedding":[1,2,3,4,5,6,7,8]}`, `{"index":1,"embedding":[1,2,3,4,5,6,7,8]}`)},
		{"entry with index null", backends.ProtocolOpenAI, openAIEntries(`{"index":null,"embedding":[1,2,3,4,5,6,7,8]}`, `{"index":1,"embedding":[1,2,3,4,5,6,7,8]}`)},
		{"null component, openai", backends.ProtocolOpenAI, openAIEntries(`{"index":0,"embedding":[1,null,3,4,5,6,7,8]}`, `{"index":1,"embedding":[1,2,3,4,5,6,7,8]}`)},
		{"null component, ollama", backends.ProtocolOllama, `{"embeddings":[[1,null,3,4,5,6,7,8],[1,2,3,4,5,6,7,8]],"prompt_eval_count":2}`},
		{"more entries than inputs", backends.ProtocolOpenAI, openAIEntries(`{"index":0,"embedding":[1,2,3,4,5,6,7,8]}`, `{"index":1,"embedding":[1,2,3,4,5,6,7,8]}`, `{"index":1,"embedding":[1,2,3,4,5,6,7,8]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, good := newEmbedEngine(t, 8), newEmbedEngine(t, 8)
			bad.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.answer))
				return true
			})
			rig := newEmbedRig(t, []backends.Backend{bad.row("bad", tc.proto, 20), good.row("good", tc.proto, 10)}, 3, 1)
			a := rig.postEmbed(t, embedBody("embsmall", []string{"a", "b"}))
			code, _, _ := a.envelope(t)
			if a.status != http.StatusInternalServerError || code != oai.CodeUpstreamError {
				t.Fatalf("%d %.300s, want 500 upstream_error (a malformed answer)", a.status, a.body)
			}
			if bad.hits.Load() != 1 || good.hits.Load() != 0 {
				t.Errorf("wire hits bad %d good %d, want 1/0 (the chain stops at a malformed answer)", bad.hits.Load(), good.hits.Load())
			}
			rows := rig.rows.all()
			if len(rows) == 1 && rows[0].Err != nil {
				t.Logf("row err %s", rows[0].Err.Error())
			}
			if len(rows) != 1 || rows[0].Err == nil || rows[0].Err.Class != backends.ClassServerFault.String() {
				t.Fatalf("rows %+v, want one row with err class server_fault", rows)
			}
		})
	}
}

// TestEmbedEmptyEntriesAnswerStaysInTheReserves (E-1 over the route): one
// request to an alias without native_dims (read cap 16 MiB, ≈ 52 MiB
// booked), the row answers ≈ 8 MiB of empty objects, resp. of empty ollama
// vectors: 500 upstream_error, and the live heap stays under the request's
// bookings plus nearCapSlack. Red before hardening 6: the entries decoded
// past the reserve (≈ 85 MiB of structs for the OpenAI form).
func TestEmbedEmptyEntriesAnswerStaysInTheReserves(t *testing.T) {
	skipUnderRace(t, "the live heap of decoding an 8-MiB answer of empty entries (HeapInuse against the bookings + nearCapSlack)")
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(parseMemGoMemLimit))
	for _, tc := range []struct {
		name   string
		proto  backends.Protocol
		answer string
	}{
		{"openai empty objects", backends.ProtocolOpenAI, fillTo(`{"object":"list","data":[`, "{}", `]}`, 8<<20)},
		{"ollama empty vectors", backends.ProtocolOllama, fillTo(`{"embeddings":[`, "[]", `]}`, 8<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			answer := []byte(tc.answer)
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(answer)
				return true
			})
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", tc.proto, 10)}, 2, 1)
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = parseMemBudget })
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			base := m.HeapInuse
			var peak atomic.Uint64
			var inUse atomic.Int64
			stop, sampled := make(chan struct{}), make(chan struct{})
			go heapPeak(stop, &peak, sampled)
			budgetDone := make(chan struct{})
			go func() {
				defer close(budgetDone)
				for {
					if u := rig.budget.InUse(); u > inUse.Load() {
						inUse.Store(u)
					}
					select {
					case <-stop:
						return
					case <-time.After(200 * time.Microsecond):
					}
				}
			}()
			a := rig.postEmbed(t, embedBody("emb", []string{"x"}))
			close(stop)
			<-sampled
			<-budgetDone
			over := int64(peak.Load()) - int64(base)
			bound := inUse.Load() + nearCapSlack
			t.Logf("%s, %d B: HeapInuse peak %d MiB over a base of %d MiB, bookings peak %d MiB (bound %d MiB)",
				tc.name, len(answer), over>>20, base>>20, inUse.Load()>>20, bound>>20)
			code, _, _ := a.envelope(t)
			if a.status != http.StatusInternalServerError || code != oai.CodeUpstreamError {
				t.Fatalf("%d %.200s, want 500 upstream_error", a.status, a.body)
			}
			if over > bound {
				t.Errorf("HeapInuse peak %d MiB over the base, want ≤ the bookings' peak %d MiB + %d MiB", over>>20, inUse.Load()>>20, int64(nearCapSlack)>>20)
			}
		})
	}
}
