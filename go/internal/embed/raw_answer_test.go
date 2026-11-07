package embed

// Gates of W02-09 hardening 6 (the bot reviews of PR #157) on the strict
// decode of a gateway sub-block's answer. E-1: the read cap bounds the bytes
// of an answer, not the entries it decodes into — an answer of empty
// objects, empty arrays or more entries than inputs is refused before its
// entries are allocated, and the decode allocates (TotalAlloc) no more than
// the decode share the route reserves for the block beside the vectors it
// hands over (they are the answer, under the answer reserve). E-2: an entry
// without index — missing or null — is refused (the internal path keeps
// reading such an answer as index 0: TestInternalEmbedBitIdenticalToBase,
// forms noindex1024 and nullindex). E-3: a null component is refused on both
// wires before it could read as 0.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
)

// TestEmbedRawRefusesAnEntryWithoutIndex (E-2): with two inputs, an entry
// without index beside index 1 would put a vector at a position the backend
// never named; with one input, the same entry is no answer under the wire
// contract either.
func TestEmbedRawRefusesAnEntryWithoutIndex(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		n            int
	}{
		{"missing index beside index 1", `{"data":[{"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`, 2},
		{"index null beside index 1", `{"data":[{"index":null,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`, 2},
		{"missing index, one input", `{"data":[{"embedding":[1,2]}]}`, 1},
		{"index null, one input", `{"data":[{"index":null,"embedding":[1,2]}]}`, 1},
	} {
		e := newRawEngine(t, 2)
		e.answer = func(string, int) string { return tc.answer }
		if vecs, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, backends.ProtocolOpenAI, 0), "m", []string{"a", "b"}[:tc.n], 0, 0); err == nil {
			t.Errorf("%s: accepted (%v)", tc.name, vecs)
		}
	}
}

// TestEmbedRawRefusesNullComponents (E-3): a null component is no number —
// [1,null] would pass rawQuality as [1,0] (finite, the expected length,
// norm > 0).
func TestEmbedRawRefusesNullComponents(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		proto        backends.Protocol
	}{
		{"openai, last", `{"data":[{"index":0,"embedding":[1,null]},{"index":1,"embedding":[3,4]}]}`, backends.ProtocolOpenAI},
		{"openai, first", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[null,4]}]}`, backends.ProtocolOpenAI},
		{"ollama, last", `{"embeddings":[[1,null],[3,4]]}`, backends.ProtocolOllama},
		{"ollama, first", `{"embeddings":[[1,2],[null,4]]}`, backends.ProtocolOllama},
	} {
		e := newRawEngine(t, 2)
		e.answer = func(string, int) string { return tc.answer }
		if vecs, _, err := EmbedRaw(context.Background(), rawRow(e.srv.URL, tc.proto, 0), "m", []string{"a", "b"}, 0, 0); err == nil {
			t.Errorf("%s: accepted (%v)", tc.name, vecs)
		}
	}
}

// mirrorReadCap and mirrorDecodeShare mirror the route's embedReadCap and
// embedDecodeFactor × embedReadCap (internal/oai/embeddings.go): the wire
// census keeps direct EmbedRaw calls out of the oai package, so the decode
// gate runs here, and TestEmbedDecodeShareFormula (internal/oai) pins the
// route's formula at this gate's points — a change there sends it here.
func mirrorReadCap(inputs, dims int, maxFloats int64) int64 {
	floats := int64(inputs) * int64(dims)
	if dims <= 0 {
		floats = maxFloats
	}
	return min(floats*25+int64(inputs)*256+4<<10, httpx.MaxResponseBytes)
}

func mirrorDecodeShare(inputs, dims int, maxFloats int64) int64 {
	return 3 * mirrorReadCap(inputs, dims, maxFloats)
}

// answerInputs are n distinct texts.
func answerInputs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("text %03d", i)
	}
	return out
}

// answerMaxFloats is the floats ceiling of the decode gates — the oai
// harness rig's gateway.embedding_max_floats (2²⁰): without a native
// dimension the read cap is the 16-MiB clamp.
const answerMaxFloats = 1 << 20

// answerAllocSlack covers what one EmbedRaw call allocates besides the
// decode after the baseline is subtracted (headers, the request, error
// values) — far below every decode share it must not hide.
const answerAllocSlack = 64 << 10

// openAIEntries is an OpenAI answer whose data list is the given entries.
func openAIEntries(entries ...string) string {
	return `{"object":"list","data":[` + strings.Join(entries, ",") + `],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
}

// vectorOf is a JSON list of n copies of lit.
func vectorOf(lit string, n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(lit+",", n), ",") + "]"
}

// fillTo repeats item (with a comma) between head and tail until the whole
// is about size bytes.
func fillTo(head, item, tail string, size int) string {
	k := max(1, (size-len(head)-len(tail))/(len(item)+1))
	return head + strings.TrimSuffix(strings.Repeat(item+",", k), ",") + tail
}

type answerForm struct {
	name    string
	proto   backends.Protocol
	n, dims int // the block's inputs and the alias's native dimension (0: unknown)
	answer  string
	accept  bool // a valid answer: EmbedRaw returns the vectors
}

// answerForms are the decode gate's shapes (the four of the hardening order
// — n vectors of full length, n empty objects, n + 1 entries, a huge number
// literal — at a known and at an unknown native dimension, the reviews'
// answers of ≈ 8 MiB, and answers that fill a cap no buffer step divides).
func answerForms() []answerForm {
	const lit24 = "0.0123456789012345678901" // 24 bytes + the comma: embedWireComponentBytes
	full := func(n, d int) string {
		es := make([]string, n)
		for i := range es {
			es[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":%s}`, i, vectorOf(lit24, d))
		}
		return openAIEntries(es...)
	}
	short := func(n, d int) string {
		es := make([]string, n)
		for i := range es {
			es[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":%s}`, i, vectorOf("0.5", d))
		}
		return openAIEntries(es...)
	}
	capOf := func(n, d int) int { return int(mirrorReadCap(n, d, answerMaxFloats)) }
	huge := func(n, d int) string {
		head := `{"data":[{"index":0,"embedding":[1`
		tail := `]}]}`
		return head + strings.Repeat("0", capOf(n, d)-len(head)-len(tail)-64) + tail
	}
	padTo := func(doc string, size int) string { // blanks before the closing brace
		return doc[:len(doc)-1] + strings.Repeat(" ", size-len(doc)) + "}"
	}
	hugeAt := func(n, d int) string { // vector 0 ends in a literal past float32, the answer exactly at the cap
		es := make([]string, n)
		for i := 1; i < n; i++ {
			es[i] = fmt.Sprintf(`{"index":%d,"embedding":%s}`, i, vectorOf("0.5", d))
		}
		es[0] = `{"index":0,"embedding":[` + strings.Repeat("0.5,", d-1) + `1HUGE]}`
		doc := openAIEntries(es...)
		return strings.Replace(doc, "HUGE", strings.Repeat("0", capOf(n, d)-len(doc)+len("HUGE")), 1)
	}
	overlong := func(n, d int) string {
		es := make([]string, n)
		per := (capOf(n, d) - 256) / n
		for i := range es {
			head := fmt.Sprintf(`{"index":%d,"embedding":[`, i)
			es[i] = fillTo(head, "1", "]}", per-8)
		}
		return `{"data":[` + strings.Join(es, ",") + `]}`
	}
	return []answerForm{
		{"known: n vectors of full length at the cap", backends.ProtocolOpenAI, 8, 1024, full(8, 1024), true},
		{"known: n empty objects", backends.ProtocolOpenAI, 8, 1024, openAIEntries(strings.Repeat("{},", 7) + "{}"), false},
		{"known: n + 1 entries", backends.ProtocolOpenAI, 8, 1024, short(9, 1024), false},
		{"known: a huge number literal", backends.ProtocolOpenAI, 1, 1, huge(1, 1), false},
		{"known: overlong vectors at the cap", backends.ProtocolOpenAI, 8, 1024, overlong(8, 1024), false},
		// A cap that no step divides (255 × 1024: 6 597 376 B, 768 B past
		// a multiple of 2^10), filled to its last byte — Delta-Review 7 F-1.
		{"known: a huge literal exactly at a cap no step divides", backends.ProtocolOpenAI, 255, 1024, hugeAt(255, 1024), false},
		{"known: short vectors padded exactly to a cap no step divides", backends.ProtocolOpenAI, 255, 1024, padTo(short(255, 1024), capOf(255, 1024)), true},
		{"unknown: n vectors of full length", backends.ProtocolOpenAI, 8, 0, full(8, 1024), true},
		{"unknown: a huge number literal at the cap", backends.ProtocolOpenAI, 1, 0, huge(1, 0), false},
		{"unknown: a dense vector at the cap", backends.ProtocolOpenAI, 1, 0,
			fillTo(`{"data":[{"index":0,"embedding":[`, "1", `]}]}`, capOf(1, 0)-64), true},
		{"unknown: 8 MiB of empty objects", backends.ProtocolOpenAI, 2, 0,
			fillTo(`{"object":"list","data":[`, "{}", `]}`, 8<<20), false},
		{"unknown: 8 MiB of entries past the inputs", backends.ProtocolOpenAI, 2, 0,
			fillTo(`{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1]},`, `{"index":1,"embedding":[1]}`, `]}`, 8<<20), false},
		{"unknown: 8 MiB of empty ollama vectors", backends.ProtocolOllama, 2, 0,
			fillTo(`{"embeddings":[`, "[]", `]}`, 8<<20), false},
		{"unknown: 8 MiB of ollama vectors past the inputs", backends.ProtocolOllama, 2, 0,
			fillTo(`{"embeddings":[`, "[1]", `]}`, 8<<20), false},
	}
}

// answerServer answers every request with answer, chunked (no
// Content-Length: the decode cannot size its buffer from the header). The
// bytes are prepared once — the server runs in the measured process.
func answerServer(t *testing.T, answer *atomic.Pointer[[]byte]) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*answer.Load())
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// rawAlloc is the smallest TotalAlloc of three EmbedRaw calls, with the
// last call's result.
func rawAlloc(row backends.Backend, n, dims int, limit int64) (alloc uint64, vecs [][]float32, err error) {
	inputs := answerInputs(n)
	alloc = ^uint64(0)
	for range 3 {
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		vecs, _, err = EmbedRaw(context.Background(), row, "m", inputs, dims, limit)
		runtime.ReadMemStats(&b)
		alloc = min(alloc, b.TotalAlloc-a.TotalAlloc)
	}
	return alloc, vecs, err
}

// TestEmbedRawDecodeStaysInTheDecodeShare (E-1 and the reserve formula):
// for every form, what one EmbedRaw call allocates beyond the baseline
// call and beyond the vectors it returns (they are the answer, held under
// the answer reserve) stays within the decode share the route books for the
// block. A refused form returns no vectors: all of it counts. Red before
// the bounded decode: the entries of an answer of empty objects, and the
// decoder's buffer, are allocated past the share.
func TestEmbedRawDecodeStaysInTheDecodeShare(t *testing.T) {
	if raceEnabled {
		t.Skip("TotalAlloc under -race counts the detector's allocations")
	}
	var answer atomic.Pointer[[]byte]
	url := answerServer(t, &answer)
	base := []byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":1}}`)
	answer.Store(&base)
	row := func(p backends.Protocol) backends.Backend {
		return backends.Backend{Host: url, Protocol: p, Model: "m", Enabled: true}
	}
	baseline, _, err := rawAlloc(row(backends.ProtocolOpenAI), 1, 1, mirrorReadCap(1, 1, answerMaxFloats))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	for _, f := range answerForms() {
		t.Run(f.name, func(t *testing.T) {
			b := []byte(f.answer)
			answer.Store(&b)
			limit := mirrorReadCap(f.n, f.dims, answerMaxFloats)
			share := mirrorDecodeShare(f.n, f.dims, answerMaxFloats)
			if int64(len(f.answer)) > limit {
				t.Fatalf("the form is %d B, over the read cap %d B", len(f.answer), limit)
			}
			alloc, vecs, err := rawAlloc(row(f.proto), f.n, f.dims, limit)
			var held int64
			for _, v := range vecs {
				held += int64(len(v)) * 4
			}
			transient := int64(alloc) - int64(baseline) - held
			t.Logf("answer %d B, read cap %d B, decode share %d B: TotalAlloc %d B (baseline %d), vectors %d B, transient %d B (%.2f × the cap); err %v",
				len(f.answer), limit, share, alloc, baseline, held, transient, float64(transient)/float64(limit), err)
			if f.accept != (err == nil) {
				t.Errorf("err %v, want accepted %v", err, f.accept)
			}
			if transient > share+answerAllocSlack {
				t.Errorf("the decode allocated %d B beside its vectors, over the decode share %d B (+ %d B slack)", transient, share, answerAllocSlack)
			}
		})
	}
}

// TestEmbedRawRefusesEntriesPastTheInputs (E-1): an answer with more
// entries than inputs is refused as such — the first entry past the inputs
// ends the decode — on both wires.
func TestEmbedRawRefusesEntriesPastTheInputs(t *testing.T) {
	var answer atomic.Pointer[[]byte]
	url := answerServer(t, &answer)
	for _, tc := range []struct {
		name   string
		proto  backends.Protocol
		answer string
	}{
		{"openai", backends.ProtocolOpenAI, openAIEntries(`{"index":0,"embedding":[1,2]}`, `{"index":1,"embedding":[3,4]}`, `{"index":2,"embedding":[5,6]}`)},
		{"ollama", backends.ProtocolOllama, `{"embeddings":[[1,2],[3,4],[5,6]]}`},
	} {
		b := []byte(tc.answer)
		answer.Store(&b)
		row := backends.Backend{Host: url, Protocol: tc.proto, Model: "m", Enabled: true}
		vecs, _, err := EmbedRaw(context.Background(), row, "m", answerInputs(2), 0, 0)
		if err == nil || !strings.Contains(err.Error(), "more embeddings than inputs") {
			t.Errorf("%s: %v (%d vectors), want the refusal \"more embeddings than inputs\"", tc.name, err, len(vecs))
		}
	}
}
