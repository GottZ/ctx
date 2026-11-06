package llm

// FOLGE-P P-4 pins of llm.BuildUpstreamBody: the body is json.Marshal of the
// merged map without the HTML/JS escaping — byte for byte the base body
// once json.HTMLEscape runs over it — so a client value reaches the engine
// with the bytes it sent; its size is fixed before the one allocation and
// offered to UpstreamSpec.Reserve, whose refusal allocates nothing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// upstreamMaps are merged maps of the shapes BuildUpstreamBody encodes: raw
// client values with whitespace, escapes, <, >, &, U+2028/U+2029, invalid
// UTF-8 and deep nesting, row values and keys a json.Marshal escapes, a nil
// value.
func upstreamMaps() []map[string]json.RawMessage {
	return []map[string]json.RawMessage{
		{"messages": json.RawMessage(`[{"role":"user","content":"a<b>c&d` + "  " + `"}]`), "model": json.RawMessage(`"m"`)},
		{"messages": json.RawMessage("[ {\"role\" : \"user\",\n\t\"content\" : \"x\" } ]"), "stop": json.RawMessage(`[ "<", "&" ]`)},
		{"messages": json.RawMessage(`[{"role":"user","content":"<\/ 😀"}]`), "temperature": json.RawMessage(`1.50e+0`)},
		{"messages": json.RawMessage(`[{"role":"user","content":"` + "\xff\xfe" + `"}]`), "seed": json.RawMessage(`-0`)},
		{"messages": json.RawMessage(`[` + strings.Repeat("[", 500) + strings.Repeat("]", 500) + `]`), "x<y>": json.RawMessage(`"<&>"`)},
		{"messages": json.RawMessage(`[]`), "nil": nil, "z": json.RawMessage(`{"a":{"b":[1,2,{"c":"</script>"}]}}`)},
		// Backslash escapes before whitespace inside a string (Fremd-Review 1
		// B4, MR13): the whitespace after an escaped quote or backslash is
		// part of the string and stays.
		{"messages": json.RawMessage(`[{"role":"user","content":"a\"  b  c"}]`), "stop": json.RawMessage(`["\\  x", "\"  \"  y"]`)},
		{},
	}
}

// TestEncodeUpstreamIsMarshalWithoutEscaping (gate 3, P-4): json.HTMLEscape
// over the FOLGE-P body is the base body (json.Marshal of the map) byte for
// byte; the FOLGE-P body carries every client string as sent.
func TestEncodeUpstreamIsMarshalWithoutEscaping(t *testing.T) {
	for _, m := range upstreamMaps() {
		got, err := encodeUpstream(m, nil)
		if err != nil {
			t.Fatalf("encode %v: %v", m, err)
		}
		base, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %v: %v", m, err)
		}
		var esc bytes.Buffer
		json.HTMLEscape(&esc, got)
		if !bytes.Equal(esc.Bytes(), base) {
			t.Errorf("escaped FOLGE-P body\n%s\nwant the base body\n%s", esc.Bytes(), base)
		}
		if !json.Valid(got) {
			t.Errorf("FOLGE-P body %q is no valid JSON", got)
		}
	}
	got, _ := encodeUpstream(upstreamMaps()[0], nil)
	if want := `"content":"a<b>c&d` + "  " + `"`; !bytes.Contains(got, []byte(want)) {
		t.Errorf("body %s does not carry %s as sent", got, want)
	}
	maps := upstreamMaps()
	got, _ = encodeUpstream(maps[len(maps)-2], nil)
	for _, want := range []string{`"content":"a\"  b  c"`, `["\\  x","\"  \"  y"]`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("body %s does not carry %s as sent", got, want)
		}
	}
}

// TestUpstreamBodyIsOneReservedCopy (P-4): Reserve gets the exact capacity
// of the one buffer before it exists; the body allocates one copy of the
// messages plus a constant; a refusal returns its error and allocates no
// copy.
func TestUpstreamBodyIsOneReservedCopy(t *testing.T) {
	b := ptRow("http://row.invalid")
	msgs := json.RawMessage(`[{"role":"user","content":"` + strings.Repeat("<", 512<<10) + `"}]`)
	var offered []int
	s := UpstreamSpec{Messages: msgs, Role: "chat", MaxTokens: 1024, Client: map[string]json.RawMessage{"stop": json.RawMessage(`["x"]`)},
		Reserve: func(n int) error { offered = append(offered, n); return nil }}
	body, err := BuildUpstreamBody(s, &b)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 1 || offered[0] != cap(body) || len(body) > offered[0] {
		t.Errorf("Reserve got %v for a body of len %d cap %d, want one call with the capacity", offered, len(body), cap(body))
	}
	if len(body) > len(msgs)+256 {
		t.Errorf("body %d bytes for messages of %d: more than the frame", len(body), len(msgs))
	}
	// The least of three runs: a run can catch a runtime allocation of its
	// own. The slack is the row stages' maps and marshals (≈ 17 KiB measured).
	measure := func(spec UpstreamSpec) uint64 {
		best := ^uint64(0)
		var a, z runtime.MemStats
		for range 3 {
			runtime.ReadMemStats(&a)
			_, _ = BuildUpstreamBody(spec, &b)
			runtime.ReadMemStats(&z)
			best = min(best, z.TotalAlloc-a.TotalAlloc)
		}
		return best
	}
	const slack = 64 << 10
	s.Reserve = nil
	if got := measure(s); got > uint64(len(msgs))+slack {
		t.Errorf("BuildUpstreamBody allocates %d bytes for messages of %d, want one copy + %d", got, len(msgs), slack)
	}
	refused := errors.New("budget")
	s.Reserve = func(int) error { return refused }
	if _, err := BuildUpstreamBody(s, &b); !errors.Is(err, refused) {
		t.Errorf("a refused Reserve: err %v, want it", err)
	}
	if got := measure(s); got > slack {
		t.Errorf("a refused Reserve still allocates %d bytes", got)
	}
}

// TestUpstreamBodyRowValuesUnchanged: what the row stages put into the body
// (model, extra_body with <, model_map params) is byte for byte what the base
// json.Marshal wrote — they were marshalled, hence escaped, already.
func TestUpstreamBodyRowValuesUnchanged(t *testing.T) {
	b := ptRow("http://row.invalid")
	b.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"x": "<&>"}, "vendor": "a<b"}
	b.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "m<odel>", Params: map[string]any{"top_k": 20, "seed_x": "<"}}}
	got, err := BuildUpstreamBody(ptSpec(nil), &b)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	base, _ := json.Marshal(m)
	if !bytes.Equal(got, base) {
		t.Errorf("row-side body\n%s\nwant the base form\n%s", got, base)
	}
}

// FuzzEncodeUpstream: for every valid JSON value, json.HTMLEscape over the
// FOLGE-P body is the base body (json.Marshal of the map).
func FuzzEncodeUpstream(f *testing.F) {
	for _, m := range upstreamMaps() {
		for _, v := range m {
			f.Add([]byte(v))
		}
	}
	f.Fuzz(func(t *testing.T, v []byte) {
		if !json.Valid(v) {
			return
		}
		m := map[string]json.RawMessage{"messages": v, "a<": json.RawMessage(`"x"`)}
		got, err := encodeUpstream(m, nil)
		if err != nil {
			t.Fatalf("encode %q: %v", v, err)
		}
		base, err := json.Marshal(m)
		if err != nil {
			return // the base refused it too
		}
		var esc bytes.Buffer
		json.HTMLEscape(&esc, got)
		if !bytes.Equal(esc.Bytes(), base) {
			t.Fatalf("value %q: escaped %q, base %q", v, esc.Bytes(), base)
		}
	})
}
