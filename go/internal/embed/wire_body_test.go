package embed

// FOLGE-E hardening 1 (S-1): the gateway body in one allocation of its
// exact length (wire_body.go) — byte for byte the json.Encoder's body
// (TestWireBodyIsTheEncoders, FuzzWireBodyIsTheEncoders; the route's
// differential G5 compares the wire against the trees before), WireBodyLen
// its exact length on every row (TestEmbedRawBodyLenIsWireBodyLen), one
// allocation within WireBodySlack of it (TestWireBodyIsOneAllocation).

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// encoderBody is the gateway body as wireInput.marshal wrote it before
// FOLGE-E hardening 1 — a json.Encoder without HTML escaping into a growing
// buffer, the trailing newline cut: the oracle of the wire.
func encoderBody(t testing.TB, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encoder: %v", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

// wireForms are strings of every escape class: each byte alone and between
// letters, the JS separators, U+FFFD, HTML, multi-byte runes, truncated,
// overlong and surrogate sequences, and a long mix.
func wireForms() []string {
	var forms []string
	for c := range 0x100 {
		forms = append(forms, string([]byte{byte(c)}), "a"+string([]byte{byte(c)})+"b")
	}
	return append(forms, "", "\u2028", "\u2029", "x\u2028y\u2029z", "\ufffd", "<>&", "ä€😀", "\x7f",
		"\xe2\x80", "\xe2\x80\xa8", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xc0\xaf", "\xff\xfe", "a\xffb\xe2\x82",
		strings.Repeat("a\n\"\\\x01\u2028\xff<ä", 1000))
}

// wireCases are the request values of embedOllama and embedOpenAI for model
// and texts: the openai wire, the ollama wire without options, with an empty
// options object (unreachable from embedOllama, the encoder's form all the
// same) and with num_ctx at 1, 4096, the largest and the smallest int.
func wireCases(model string, texts []string) []any {
	in := wireInput{texts: texts}
	cases := []any{openAIEmbedRequest{Model: model, Input: in.value()}, ollamaEmbedRequest{Model: model, Input: in.value()}}
	for _, n := range []int{0, 1, 4096, math.MaxInt, math.MinInt} {
		cases = append(cases, ollamaEmbedRequest{Model: model, Input: in.value(), Options: &ollamaOpts{NumCtx: n}})
	}
	return cases
}

// checkWireBody compares the body marshal writes for texts and v with the
// encoder's and with wireBodyLen.
func checkWireBody(t testing.TB, texts []string, v any) {
	t.Helper()
	got, err := wireInput{texts: texts}.marshal(v)
	if err != nil {
		t.Fatalf("%#v: marshal: %v", v, err)
	}
	if want := encoderBody(t, v); !bytes.Equal(got, want) {
		t.Fatalf("%#v:\nmarshal %q\nencoder %q", v, got, want)
	}
	var model string
	var opts *ollamaOpts
	switch r := v.(type) {
	case ollamaEmbedRequest:
		model, opts = r.Model, r.Options
	case openAIEmbedRequest:
		model = r.Model
	}
	if n := wireBodyLen(model, texts, opts); n != len(got) {
		t.Fatalf("%#v: wireBodyLen %d, the body has %d bytes", v, n, len(got))
	}
}

// TestWireBodyIsTheEncoders: for every form as the model and as a text, all
// forms in one list, an empty and a nil list, on every request value the
// body is the encoder's byte for byte and wireBodyLen its length;
// WireStringLen is the encoder's length of every form.
func TestWireBodyIsTheEncoders(t *testing.T) {
	forms := wireForms()
	for _, s := range forms {
		if got, want := WireStringLen(s), len(encoderBody(t, s)); got != want {
			t.Errorf("%q: WireStringLen %d, the encoder writes %d bytes", s, got, want)
		}
		for _, v := range wireCases(s, []string{s}) {
			checkWireBody(t, []string{s}, v)
		}
		for _, v := range wireCases("m", []string{"x", s, s}) {
			checkWireBody(t, []string{"x", s, s}, v)
		}
	}
	for _, texts := range [][]string{forms, {}, nil} {
		for _, v := range wireCases("MODEL\u2028\x01\xff\"", texts) {
			checkWireBody(t, texts, v)
		}
	}
}

// FuzzWireBodyIsTheEncoders: any model and texts on every request value —
// the encoder's bytes, wireBodyLen's length.
func FuzzWireBodyIsTheEncoders(f *testing.F) {
	for _, s := range wireForms() {
		f.Add("m", s, "x")
	}
	f.Fuzz(func(t *testing.T, model, a, b string) {
		for _, texts := range [][]string{{a}, {a, b}, {b, a, a}} {
			for _, v := range wireCases(model, texts) {
				checkWireBody(t, texts, v)
			}
		}
	})
}

// TestEmbedRawBodyLenIsWireBodyLen: through EmbedRaw on the openai row and
// on the ollama row without and with num_ctx, the body the engine receives
// is the encoder's and WireBodyLen(row, …) its length — the protocol choice
// of WireBodyLen is embedWire's — and within the frame bound WireFrameMax
// names.
func TestEmbedRawBodyLenIsWireBodyLen(t *testing.T) {
	texts := []string{"a", "<b>&\"c\"", "\u2028\x01\n\t", "ä😀", "\ufffd\x7f", strings.Repeat("x", 3000)}
	model := "RAW\u2029MODEL"
	for _, row := range []struct {
		p      backends.Protocol
		numCtx int
	}{{backends.ProtocolOpenAI, 0}, {backends.ProtocolOpenAI, 4096}, {backends.ProtocolOllama, 0}, {backends.ProtocolOllama, 4096}, {backends.ProtocolOllama, math.MaxInt}} {
		e := newRawEngine(t, 4)
		b := rawRow(e.srv.URL, row.p, row.numCtx)
		if _, _, err := EmbedRaw(context.Background(), b, model, texts, 0, 0); err != nil {
			t.Fatalf("%s/%d: EmbedRaw: %v", row.p, row.numCtx, err)
		}
		var v any = openAIEmbedRequest{Model: model, Input: texts}
		if row.p == backends.ProtocolOllama {
			r := ollamaEmbedRequest{Model: model, Input: texts}
			if row.numCtx > 0 {
				r.Options = &ollamaOpts{NumCtx: row.numCtx}
			}
			v = r
		}
		body := e.lastBody()
		if want := string(encoderBody(t, v)); body != want {
			t.Errorf("%s/%d: the engine received %q, the encoder writes %q", row.p, row.numCtx, body, want)
		}
		if n := WireBodyLen(b, model, texts); n != len(body) {
			t.Errorf("%s/%d: WireBodyLen %d, the body has %d bytes", row.p, row.numCtx, n, len(body))
		}
		bound := WireFrameMax + WireStringLen(model) + len(texts) - 1
		for _, s := range texts {
			bound += WireStringLen(s)
		}
		if len(body) > bound || (row.numCtx == math.MaxInt && row.p == backends.ProtocolOllama && len(body) != bound) {
			t.Errorf("%s/%d: %d bytes, the frame bound %d (WireFrameMax %d; exact on the ollama row of the largest num_ctx)", row.p, row.numCtx, len(body), bound, WireFrameMax)
		}
	}
}

// TestWireBodyIsOneAllocation is the pin of the gateway body's allocation
// (FOLGE-E hardening 1, the forms of the bau report's encoder measurement):
// one encode of a block's body — plain text, U+FFFD, newlines, control
// characters, quotes, U+2028 and a mix, one and 255 texts, 64 KiB … 24 MiB
// (−short: … 1 MiB) — is one allocation of at most the body's bytes and
// WireBodySlack (the runtime's size class), the body as long as
// wireBodyLen says. The json.Encoder it replaces allocated up to 7.105 ×
// the body over the same forms.
func TestWireBodyIsOneAllocation(t *testing.T) {
	units := map[string]string{"ascii": "a", "fffd": "\ufffd", "nl": "\n", "ctl": "\x01", "quote": `"`, "u2028": "\u2028", "mixctl": "abcde\x01"}
	sizes := []int{64 << 10, 1 << 20, 8 << 20, 24 << 20}
	if testing.Short() {
		sizes = sizes[:2]
	}
	var worst float64
	var over uint64
	passes := 0
	for _, size := range sizes {
		for name, unit := range units {
			for _, k := range []int{1, 255} {
				texts := make([]string, k)
				for i := range texts {
					texts[i] = strings.Repeat(unit, max(size/k/len(unit), 1))
				}
				in := wireInput{texts: texts}
				var v any = openAIEmbedRequest{Model: "m", Input: in.value()}
				var body []byte
				var alloc, mallocs uint64
				for pass := range wireAllocPasses {
					var err error
					body, alloc, mallocs, err = wireBodyAlloc(in, v)
					if err != nil {
						t.Fatalf("%s %d B ×%d: %v", name, size, k, err)
					}
					if mallocs == 1 && alloc <= uint64(len(body))+WireBodySlack {
						passes = max(passes, pass+1)
						break
					}
				}
				if n := wireBodyLen("m", texts, nil); n != len(body) {
					t.Errorf("%s %d B ×%d: wireBodyLen %d, the body has %d bytes", name, size, k, n, len(body))
				}
				if mallocs != 1 || alloc > uint64(len(body))+WireBodySlack {
					t.Errorf("%s %d B ×%d: %d allocations of %d B for a body of %d B (%.3f ×), want one within %d B of it", name, size, k, mallocs, alloc, len(body), float64(alloc)/float64(len(body)), WireBodySlack)
				}
				worst = max(worst, float64(alloc)/float64(len(body)))
				over = max(over, alloc-min(alloc, uint64(len(body))))
			}
		}
	}
	t.Logf("FOLGEE wire body: at most %.4f × the body, at most %d B past it (slack %d), passes %d", worst, over, WireBodySlack, passes)
}

// wireAllocPasses is how often a form's encode is measured before it fails:
// the counters are the process's, and a goroutine of the runtime or the
// test binary can allocate while the encode runs — what it adds is never
// the encode's, so the first clean pass is the measurement.
const wireAllocPasses = 3

// wireBodyAlloc is one encode of v and what it allocated (TotalAlloc and
// Mallocs), the collector off while it runs and the heap collected before.
func wireBodyAlloc(in wireInput, v any) (body []byte, alloc, mallocs uint64, err error) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	body, err = in.marshal(v)
	runtime.ReadMemStats(&m1)
	return body, m1.TotalAlloc - m0.TotalAlloc, m1.Mallocs - m0.Mallocs, err
}
