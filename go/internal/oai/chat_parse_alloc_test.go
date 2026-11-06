package oai

// The deterministic pins of FOLGE-P gate 1 (the live-heap gate over the real
// route is chat_parse_mem_test.go): the bytes one parse allocates do not grow
// with the body — for every form of the heap probe, refused or served — but
// the one copy it is allowed, the rewritten message array, which parsePolicy
// reserves before it makes it. pinParse is the parse under test (the red run
// against 4839a958 swapped in an adapter for the base signature).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/modelalias"
)

// parseAllocSlack is what one parse may allocate whatever its body: the
// request, its PASS map and ignored list, the strings of a refusal, the
// state json.Valid keeps for nesting (bounded by the decoder's depth limit).
const parseAllocSlack = 64 << 10

// allocForm is one form of the pin: its body, the policy it runs under and
// the bytes the parse may copy (the rewritten message array; 0 for the rest).
type allocForm struct {
	name   string
	body   []byte
	l      policyLimits
	copies int
}

// allocGen writes prefix, elem(0), elem(1), … comma-separated while the whole
// stays ≤ size with suffix, then suffix.
func allocGen(prefix, suffix string, size int, elem func(i int) string) []byte {
	var b strings.Builder
	b.WriteString(prefix)
	for i := 0; ; i++ {
		e := elem(i)
		if b.Len()+1+len(e)+len(suffix) > size {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(e)
	}
	b.WriteString(suffix)
	return []byte(b.String())
}

func allocConst(s string) func(int) string { return func(int) string { return s } }

// allocForms are the probe's forms and the ones FOLGE-P found, at size bytes.
func allocForms(size int) []allocForm {
	def := diffLimits[0]
	vision := policyLimits{toolsMaxCount: 64, toolsMaxBytes: 256 << 10, responseFormatMaxBytes: 64 << 10, logitBiasMaxEntries: 64,
		stopMaxEntries: 4, vision: true, visionMaxBytes: 8 << 20}
	tcap := def
	tcap.toolsMaxBytes, tcap.toolsMaxCount = 1<<20, 256
	msg := `{"model":"chat","messages":[{"role":"user","content":"x"}]`
	key := func(i int) string { return fmt.Sprintf(`"k%07d":0`, i) }
	text := func(fill string) []byte {
		pre, suf := `{"model":"chat","messages":[{"role":"user","content":"`, `"}]}`
		return []byte(pre + strings.Repeat(fill, (size-len(pre)-len(suf))/len(fill)) + suf)
	}
	rfcap := def
	rfcap.responseFormatMaxBytes = modelalias.CeilingResponseFormatMaxBytes
	rfType := func(fill string) []byte {
		pre, suf := msg+`,"response_format":{"type":"`, `"}}`
		n := min(size, rfcap.responseFormatMaxBytes+len(msg)+20) - len(pre) - len(suf)
		return []byte(pre + strings.Repeat(fill, n/len(fill)) + suf)
	}
	developer := allocGen(`{"model":"chat","messages":[`, `]}`, size, allocConst(`{"role":"developer","content":"x"}`))
	reasoning := allocGen(`{"model":"chat","messages":[`, `]}`, size, allocConst(`{"role":"assistant","content":"x","reasoning_content":"r"}`))
	return []allocForm{
		{"unknown keys", allocGen(msg+`,`, `}`, size, key), def, 0},
		{"unknown keys, escaped", allocGen(msg+`,`, `}`, size, func(i int) string { return fmt.Sprintf(`"k%07d":0`, i) }), def, 0},
		{"stop", allocGen(msg+`,"stop":[`, `]}`, size, allocConst(`"x"`)), def, 0},
		{"logit_bias", allocGen(msg+`,"logit_bias":{`, `}}`, size, func(i int) string { return `"` + strconv.Itoa(i) + `":0` }), def, 0},
		{"modalities", allocGen(msg+`,"modalities":[`, `]}`, size, allocConst(`"text"`)), def, 0},
		{"tools over count", allocGen(msg+`,"tools":[`, `]}`, min(size, 1<<20), allocConst(`{"type":"function","function":{"name":"f"}}`)), tcap, 0},
		{"tool_choice keys", allocGen(msg+`,"tool_choice":{"type":"function","function":{"name":"f"},`, `}}`, size, key), def, 0},
		{"n huge", []byte(msg + `,"n":1` + strings.Repeat("0", size-len(msg)-8) + `}`), def, 0},
		{"max_tokens huge", []byte(msg + `,"max_tokens":1` + strings.Repeat("0", size-len(msg)-17) + `}`), def, 0},
		{"service_tier huge", []byte(`{"model":"chat","service_tier":"` + strings.Repeat("a", size-90) + `","messages":[{"role":"user","content":"x"}]}`), def, 0},
		// model is the one CTX value decoded into a string (Resolve needs
		// it), and only while its raw form can still be an alias name
		// (hardening 5, B-1): a longer one is never decoded — not as plain
		// text, not as invalid UTF-8 (each byte one U+FFFD of 3 bytes: the
		// decode held 3 × the value), not as escapes.
		{"model huge", []byte(`{"model":"` + strings.Repeat("a", size-70) + `","messages":[{"role":"user","content":"x"}]}`), def, 0},
		{"model huge, invalid UTF-8", []byte(`{"model":"` + strings.Repeat("\xff", size-70) + `","messages":[{"role":"user","content":"x"}]}`), def, 0},
		{"model huge, escaped", []byte(`{"model":"` + strings.Repeat(`\u00e9`, (size-70)/6) + `","messages":[{"role":"user","content":"x"}]}`), def, 0},
		// response_format.type is compared, never decoded (hardening 6, P-1):
		// up to the policy ceiling the decode held 15 × the value for
		// invalid UTF-8, 12 × for lone surrogate escapes.
		{"response_format.type huge, invalid UTF-8", rfType("\xff"), rfcap, 0},
		{"response_format.type huge, lone surrogates", rfType(`\ud800`), rfcap, 0},
		{"response_format.type huge", rfType("a"), rfcap, 0},
		// stream is compared with its two literals (hardening 6, P-2): a
		// decode into a bool walked a nested value and held its state.
		{"stream nested 9997 deep", []byte(msg + `,"stream":` + strings.Repeat("[", 9997) + strings.Repeat("]", 9997) + `}`), def, 0},
		{"stream, object nested 9997 deep", []byte(msg + `,"stream":` + strings.Repeat(`{"a":`, 9997) + "0" + strings.Repeat("}", 9997) + `}`), def, 0},
		{"messages", allocGen(`{"model":"chat","messages":[`, `]}`, size, allocConst(`{"role":"user","content":"x"}`)), def, 0},
		{"messages, last one refused", allocGen(`{"model":"chat","messages":[`, `,{"role":"user","content":"x","foo":1}]}`, size,
			allocConst(`{"role":"user","content":"x"}`)), def, 0},
		{"parts", allocGen(`{"model":"chat","messages":[{"role":"user","content":[`, `]}]}`, size, allocConst(`{"type":"text","text":"x"}`)), def, 0},
		{"developer", developer, def, len(developer)},
		{"reasoning traces", reasoning, def, len(reasoning)},
		{"images, escaped url", allocGen(`{"model":"vis","messages":[{"role":"user","content":[`, `]}]}`, size,
			allocConst(`{"type":"image_url","image_url":{"url":"data:image\/png;base64,AAAA"}}`)), vision, 0},
		{"one image, escaped url", []byte(`{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image\/png;base64,` +
			strings.Repeat("AAAA", (size-120)/4) + `"}}]}]}`), vision, 0},
		{"text of <", text("<"), def, 0},
		{"tool_calls nested 9990 deep", allocGen(`{"model":"chat","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[`,
			`]}]}`, size, allocConst(strings.Repeat("[", 9990)+strings.Repeat("]", 9990))), def, 0},
	}
}

// allocatedBy is the heap f allocates (the least of three runs: a run can
// catch a runtime allocation of its own).
func allocatedBy(f func()) uint64 {
	best := ^uint64(0)
	var a, b runtime.MemStats
	for range 3 {
		runtime.ReadMemStats(&a)
		f()
		runtime.ReadMemStats(&b)
		best = min(best, b.TotalAlloc-a.TotalAlloc)
	}
	return best
}

// TestParseAllocationIsBoundedByTheReservation (P-1, P-2, P-3): every form
// at 256 KiB — refused or served — allocates at most parseAllocSlack plus
// the copy it may make; the base parse allocated 2 to 30 times the body.
func TestParseAllocationIsBoundedByTheReservation(t *testing.T) {
	for _, f := range allocForms(256 << 10) {
		t.Run(f.name, func(t *testing.T) {
			got := allocatedBy(func() { _ = pinParse(f.body, f.l) })
			if bound := uint64(parseAllocSlack + f.copies); got > bound {
				t.Errorf("the parse of %d bytes allocates %d bytes, want ≤ %d (slack %d + copy %d)", len(f.body), got, bound, parseAllocSlack, f.copies)
			}
		})
	}
}

// TestReadRequestBodySpendsWhatItBooks (gate 1, the read): a declared body
// is one buffer of its length; an undeclared one costs what readRequestBody
// reports (the ledger books it), at most about twice the body — io.ReadAll
// grew its way to the length and spent more than it held.
func TestReadRequestBodySpendsWhatItBooks(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 1<<20)
	var got []byte
	var spent int64
	alloc := allocatedBy(func() { got, spent, _ = readRequestBody(bytes.NewReader(body), int64(len(body))) })
	if !bytes.Equal(got, body) || spent != int64(len(body)) || alloc > uint64(len(body))+4<<10 {
		t.Errorf("declared: equal %v, spent %d, allocated %d; want the body, %d, ≤ %d", bytes.Equal(got, body), spent, alloc, len(body), len(body)+4<<10)
	}
	alloc = allocatedBy(func() { got, spent, _ = readRequestBody(struct{ io.Reader }{bytes.NewReader(body)}, -1) })
	if !bytes.Equal(got, body) || alloc > uint64(spent)+4<<10 || spent > 3*int64(len(body)) {
		t.Errorf("undeclared: equal %v, spent %d, allocated %d; want the body, allocated ≤ spent + 4 KiB, spent ≤ 3 × %d", bytes.Equal(got, body), spent, alloc, len(body))
	}
	if _, _, err := readRequestBody(struct{ io.Reader }{io.MultiReader(bytes.NewReader(body[:10]), brokenReader{})}, -1); err == nil {
		t.Error("a broken undeclared body reads as complete")
	}
	// A7 (hardening H-7, Fremd-Review 1 B7): a body that delivers fewer bytes
	// than it declares — any number short, not only none — is a read error,
	// answered 400 invalid_json "The request body could not be read." (the
	// base parsed the bytes it got: "… is not a valid JSON object."). Over
	// net/http the server's body reader fails such a body itself
	// (io.ErrUnexpectedEOF); only another body implementation reaches this.
	for _, got := range []int{0, 10, 19} {
		if _, _, err := readRequestBody(bytes.NewReader(body[:got]), 20); err == nil {
			t.Errorf("a declared body of 20 bytes that delivers %d reads as complete", got)
		}
	}
}

// brokenReader is a reader that fails like a client that went away mid-body.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// jsonStateBytes is what the route books for the JSON check's nesting state
// of raw (chat.go serve: jsonDepth × jsonDepthStateBytes, before the check).
func jsonStateBytes(raw []byte) int64 { return jsonDepth(raw) * jsonDepthStateBytes }

// TestJSONStateIsBooked (gate 1, the JSON check): jsonStateBytes counts the
// deepest nesting outside strings, up to one past the decoder's limit, and
// covers what json.Valid really allocates on a body that fails at the limit
// (190 144 bytes on go1.27.3).
func TestJSONStateIsBooked(t *testing.T) {
	nest := func(d int) []byte { return []byte(strings.Repeat("[", d) + strings.Repeat("]", d)) }
	for _, tc := range []struct {
		body  []byte
		depth int64
	}{
		{[]byte(`{"a":"[[[[{{"}`), 1}, {[]byte(`{"a":"\"[[[","b":[[1]]}`), 3}, {[]byte(`"x"`), 0},
		{nest(9999), 9999}, {nest(20000), jsonMaxDepth + 1}, {[]byte(strings.Repeat("[", 50)), 50},
	} {
		if got := jsonStateBytes(tc.body); got != tc.depth*jsonDepthStateBytes {
			t.Errorf("jsonStateBytes(%.40q…) = %d, want depth %d × %d", tc.body, got, tc.depth, jsonDepthStateBytes)
		}
	}
	// The floor's nesting term (hardening 5, B-2) is the most the route books.
	if got := jsonStateBytes(nest(20000)); got != JSONStateReserve {
		t.Errorf("jsonStateBytes at most %d, JSONStateReserve %d", got, JSONStateReserve)
	}
	for _, body := range [][]byte{nest(jsonMaxDepth + 1), []byte(`{"a":` + strings.Repeat("[", jsonMaxDepth-1)), nest(jsonMaxDepth)} {
		if got, booked := allocatedBy(func() { _ = json.Valid(body) }), jsonStateBytes(body); got > uint64(booked) {
			t.Errorf("json.Valid allocates %d bytes on a body nested %d deep, booked %d", got, booked/jsonDepthStateBytes, booked)
		}
	}
}
