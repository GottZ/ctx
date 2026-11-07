package oai

// The pins of the embeddings route's ledger terms (FOLGE-E): the size run of
// an input is the length json.Unmarshal decodes and the decode writes that
// string (TestEmbedInputDecodeIsUnmarshal, FuzzEmbedInputDecode), the size
// run of the upstream body (embed.WireStringLen, internal/embed since
// FOLGE-E hardening 1) is the encoder's (TestEmbedWireLenIsTheEncoders),
// the booking of an attempt is the bodies of both wires byte for byte, with
// the allocation's rounding and the wire call per block
// (TestEmbedUpstreamBookingHoldsTheBodies, and over the forms of the
// encoder measurement TestEmbedUpstreamReserveIsTheMeasuredOne); the ledger
// books against the share first (TestRequestLedgerCover).

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
)

// decodeForms are raw JSON string contents: every escape, surrogate pairs,
// lone and misordered surrogates, invalid UTF-8 of every shape (a lone
// continuation byte, a truncated sequence, an overlong form, an encoded
// surrogate, a code point past U+10FFFF), valid UTF-8 of every width and
// U+FFFD itself.
var decodeForms = []string{
	"", "a", "plain text", BSq, BSbs, BS + "/", BS + "b" + BS + "f" + BS + "n" + BS + "r" + BS + "t",
	U("0041"), U("0000"), U("001f"), U("00e4"), U("20ac"), U("2028"), U("fffd"), U("FFFD"),
	U("d83d") + U("de00"), U("D83D") + U("DE00"), U("d800"), U("dc00"), U("d800") + U("0041"), U("d800") + U("d800") + U("dc00"),
	U("dc00") + U("d800"), U("d800") + "x", U("d800") + BS + "n", "a" + U("d800"),
	"\xff", "\xff\xff\xff", "a\xffb", "\x80", "\xc3", "\xe2\x82", "\xf0\x9f\x98", "\xc0\x80", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xf8\x88\x80\x80\x80",
	"ä", "€", "😀", "\xef\xbf\xbd", "a\xe2\x82" + U("0041"), "\xe2\x82" + BSq,
}

// BS, BSq, BSbs and U spell the escapes of the forms (a backslash, an
// escaped quote, an escaped backslash, \uXXXX).
const (
	BS   = "\\"
	BSq  = BS + `"`
	BSbs = BS + BS
)

func U(hex string) string { return BS + "u" + hex }

// unmarshalString is what json.Unmarshal decodes from the content raw.
func unmarshalString(t testing.TB, raw string) (string, bool) {
	q := []byte(`"` + raw + `"`)
	if !json.Valid(q) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(q, &s); err != nil {
		t.Fatalf("%q: valid JSON string not decoded: %v", raw, err)
	}
	return s, true
}

// checkDecode compares the size run and the decode of raw with json.Unmarshal.
func checkDecode(t testing.TB, raw string) {
	want, ok := unmarshalString(t, raw)
	if !ok {
		return
	}
	n := decodedLen([]byte(raw))
	var b strings.Builder
	b.Grow(n)
	grown := b.Cap()
	writeDecoded(&b, []byte(raw))
	if n != len(want) || b.String() != want || b.Cap() != grown {
		t.Errorf("%q: size run %d, decode %q (capacity %d → %d), json.Unmarshal %q (%d bytes)", raw, n, b.String(), grown, b.Cap(), want, len(want))
	}
}

func TestEmbedInputDecodeIsUnmarshal(t *testing.T) {
	for _, f := range decodeForms {
		checkDecode(t, f)
		checkDecode(t, f+f)
		checkDecode(t, "x"+f+"y")
		checkDecode(t, strings.Repeat(f, 300)) // past the decode chunk
	}
	r := rand.New(rand.NewPCG(5, 23))
	pieces := append(slices3(decodeForms), "a", "bc", " ")
	for range 20000 {
		var b strings.Builder
		for range r.IntN(12) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		checkDecode(t, b.String())
	}
}

func slices3(s []string) []string { return append([]string(nil), s...) }

func FuzzEmbedInputDecode(f *testing.F) {
	for _, s := range decodeForms {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) { checkDecode(t, raw) })
}

// wireOf is the encoder's form of s, as wireInput.marshal writes a text.
func wireOf(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

func TestEmbedWireLenIsTheEncoders(t *testing.T) {
	var forms []string
	for c := range 0x80 {
		forms = append(forms, string(rune(c)), "a"+string(rune(c))+"b")
	}
	forms = append(forms, "\u2028", "\u2029", "\ufffd", "<>&", "ä€😀", "\x7f", strings.Repeat("a\n\"\\\x01\u2028", 1000))
	for _, f := range decodeForms {
		if s, ok := unmarshalString(t, f); ok {
			forms = append(forms, s)
		}
	}
	for _, s := range forms {
		if got, want := embed.WireStringLen(s), len(wireOf(s)); got != want {
			t.Errorf("%q: embed.WireStringLen %d, the encoder writes %d bytes (%s)", s, got, want, wireOf(s))
		}
	}
}

// embedWireReplica is the upstream body of a block of texts to model as
// embed's wireInput.marshal writes it for a gateway block — the openai
// request, or the ollama one with options.num_ctx when numCtx > 0 — through
// the same encoder (no HTML escaping, the trailing newline cut). It is
// pinned byte for byte against the bodies the route sends
// (TestEmbedWireReplicaIsTheWire, embed_ledger_share_test.go), so its sizes
// and its allocations are the wire's; the tests of internal/oai never call
// a wire function outside the walker (wirecensus_test.go).
func embedWireReplica(model string, texts []string, ollama bool, numCtx int) []byte {
	type opts struct {
		NumCtx int `json:"num_ctx,omitempty"`
	}
	v := struct {
		Model   string `json:"model"`
		Input   any    `json:"input"`
		Options *opts  `json:"options,omitempty"`
	}{Model: model, Input: texts}
	if ollama && numCtx > 0 {
		v.Options = &opts{NumCtx: numCtx}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

// EmbedWireReplica exports embedWireReplica to the route test, and
// EmbedWireCallBytes the per-call term to its pin.
var (
	EmbedWireReplica   = embedWireReplica
	EmbedWireCallBytes = int64(embedWireCallBytes)
)

// upstreamRows are the rows of the booking pins: the openai wire, the ollama
// wire without num_ctx and with it, up to the longest int.
func upstreamRows() []backends.Backend {
	return []backends.Backend{
		{Protocol: backends.ProtocolOpenAI, NumCtx: 4096},
		{Protocol: backends.ProtocolOllama},
		{Protocol: backends.ProtocolOllama, NumCtx: 4096},
		{Protocol: backends.ProtocolOllama, NumCtx: math.MaxInt64},
	}
}

// checkUpstreamBooking compares the booking of an attempt of blocks on row b
// with the bodies the wire sends (embedWireReplica, the encoder pinned to the
// route's bodies): exactly their bytes, the transport's copy of each
// (embed.WireCopyBuffer, the body's length when shorter) and, per block,
// embed.WireBodySlack and embedWireCallBytes — and at least the texts booked
// before the acquire.
func checkUpstreamBooking(t *testing.T, b backends.Backend, model string, inputs []string, blocks []int) {
	t.Helper()
	sent, copied, lo := 0, 0, 0
	for _, size := range blocks {
		n := len(embedWireReplica(model, inputs[lo:lo+size], b.Protocol == backends.ProtocolOllama, b.NumCtx))
		sent, copied = sent+n, copied+min(n, embed.WireCopyBuffer)
		lo += size
	}
	c := &embedCall{req: &embedRequest{inputs: inputs}}
	booked := c.upstreamBooking(b, model, blocks)
	if want := int64(sent+copied) + int64(len(blocks))*(embed.WireBodySlack+embedWireCallBytes); booked != want {
		t.Errorf("%s/%d, model %q, blocks %v: the attempt books %d B, the bodies hold %d B, their copies %d B (want %d: both, %d B slack and %d B per call)",
			b.Protocol, b.NumCtx, model, blocks, booked, sent, copied, want, embed.WireBodySlack, embedWireCallBytes)
	}
	if texts := c.upstreamTexts(); texts > booked {
		t.Errorf("%s/%d, blocks %v: %d B of texts booked before the acquire, more than the attempt's %d B", b.Protocol, b.NumCtx, blocks, texts, booked)
	}
}

// TestEmbedUpstreamBookingHoldsTheBodies: the booking of an attempt is the
// bodies of its blocks byte for byte (factor 1: internal/embed writes each
// in one allocation of its exact length, FOLGE-E hardening 1), the
// transport's copy of each, the slack and the wire call per block, on the
// openai and the ollama wire (num_ctx up to 20 digits), for texts of every
// escape class and blocks of every split (one block past the copy buffer).
func TestEmbedUpstreamBookingHoldsTheBodies(t *testing.T) {
	inputs := []string{"a", "<b>&\"c\"", "\u2028\x01\n", "ä😀", strings.Repeat("x", 40<<10), "\ufffd"}
	for _, model := range []string{"m", "a-very-long/model:name\u2028\x01"} {
		for _, b := range upstreamRows() {
			for _, blocks := range [][]int{{6}, {3, 3}, {1, 1, 1, 1, 1, 1}, {5, 1}} {
				checkUpstreamBooking(t, b, model, inputs, blocks)
			}
		}
	}
}

func TestRequestLedgerCover(t *testing.T) {
	var bud Budget
	res := &Reservation{b: &bud, limit: 1000}
	if !res.Grow(30) { // the stage's share of a 10-byte body
		t.Fatal("share")
	}
	l := newRequestLedger(res, 10)
	if !l.cover(25) || res.held != 30 || l.held != 30 || l.spent != 25 {
		t.Fatalf("inside the share: held %d, ledger %d/%d", res.held, l.spent, l.held)
	}
	if !l.cover(10) || res.held != 35 || l.held != 35 || l.spent != 35 {
		t.Fatalf("past the share: reservation %d, ledger %d/%d (want 35/35/35)", res.held, l.spent, l.held)
	}
	if l.cover(1000) || res.held != 35 || l.spent != 35 {
		t.Fatalf("past the budget: reservation %d, ledger %d (nothing booked)", res.held, l.spent)
	}
	var none requestLedger
	if !none.cover(0) || none.cover(1) {
		t.Fatal("without a reservation: nothing past the share")
	}
}

// TestEmbedInputsAreBookedBeforeTheDecode: a budget that cannot hold the
// decoded texts refuses them (errParseOverloaded) before one is allocated —
// the parse then allocates no more than without any text, for a single
// input and for many, of invalid bytes and of escapes.
func TestEmbedInputsAreBookedBeforeTheDecode(t *testing.T) {
	ff := strings.Repeat("\xff", 1<<20)
	many := make([]string, 256)
	for i := range many {
		many[i] = `"` + strings.Repeat(BS+"u0001", 700) + `"`
	}
	for name, body := range map[string][]byte{
		"one":  []byte(`{"model":"m","input":"` + ff + `"}`),
		"many": []byte(`{"model":"m","input":[` + strings.Join(many, ",") + `]}`),
	} {
		asked := 0
		var perr *paramError
		alloc := allocatedBy(func() {
			asked = 0
			_, perr = parseEmbedTop(body, func(int64) bool { asked++; return false })
		})
		if perr != errParseOverloaded || asked != 1 || alloc > parseAllocSlack {
			t.Errorf("%s: refused booking ⇒ %v after %d bookings, %d bytes allocated (want errParseOverloaded after one, ≤ %d)", name, perr, asked, alloc, parseAllocSlack)
		}
	}
}
