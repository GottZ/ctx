package oai

// The fuzzers of FOLGE-P Fremd-Review 1, kept as standing gates (hardening
// H-5): an independent differential of the hand-written JSON walk
// (memberIter/arrayIter/strDecoder/cmpKeys/keyIndex/hullKey) against
// encoding/json, and a semantic smuggle oracle over the upstream body — no
// key an engine reads may differ from the key ctx checked.
//
//	go test -run '^$' -fuzz '^FuzzReviewIterDifferential$' -fuzztime 300s ./internal/oai/
//	go test -run '^$' -fuzz '^FuzzReviewSmuggle$' -fuzztime 300s ./internal/oai/

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// rvEsc is the JSON escape of r (six bytes), built without a literal
// backslash-u in this source.
func rvEsc(r rune) string { return fmt.Sprintf("%cu%04x", 92, r) }

const rvBS = "\x5c"

// ejLevel is one object level as encoding/json reads it.
type ejMember struct {
	key string
	val []byte
}

func ejObject(raw []byte) ([]ejMember, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out []ejMember
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("key token %T", kt)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, ejMember{k, []byte(v)})
	}
	return out, nil
}

func ejArray(raw []byte) ([][]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out [][]byte
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, []byte(v))
	}
	return out, nil
}

func sgn(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

var rvNames = []string{"model", "messages", "role", "content", "type", "text", "image_url", "url", "detail", "function", "json_schema", "strict", "schema", "name", "developer", "system", "", "a", "zz", "custom_logit_processor", "\xef\xbf\xbd", "modalities"}

// rvCompare walks raw with both readers and reports the first divergence.
func rvCompare(raw []byte, path string, depth int) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || depth > 60 {
		return ""
	}
	switch t[0] {
	case '{':
		want, err := ejObject(t)
		if err != nil {
			return path + ": encoding/json: " + err.Error()
		}
		it, ok := newMemberIter(t)
		if !ok {
			return path + ": memberIter refused an object"
		}
		var keys [][]byte
		i := 0
		for {
			k, v, more := it.next()
			if !more {
				break
			}
			if i >= len(want) {
				return fmt.Sprintf("%s: iterator yields member %d (%q), encoding/json has %d", path, i, k, len(want))
			}
			dk := rvFullDecode(k)
			if dk != want[i].key {
				return fmt.Sprintf("%s: member %d key iterator %q != encoding/json %q (raw %q)", path, i, dk, want[i].key, k)
			}
			if g, w := decodeKey(k), rvCut(dk); g != w {
				return fmt.Sprintf("%s: member %d key echoed as %q, want %q (H-9: cut to echoNameMax at a rune boundary)", path, i, g, w)
			}
			if !bytes.Equal(v, bytes.TrimSpace(want[i].val)) {
				return fmt.Sprintf("%s.%q: value iterator %q != encoding/json %q", path, dk, v, want[i].val)
			}
			// keyIndex against the map lookups of the decoded name.
			for li, idx := range []map[string]int{topLevelIndex, messageIndex, partIndex, imageIndex} {
				w, found := idx[dk]
				if !found {
					w = -1
				}
				if got := keyIndex(k, idx); got != w {
					return fmt.Sprintf("%s: keyIndex(level %d, %q) = %d, want %d", path, li, k, got, w)
				}
			}
			for _, known := range [][]string{toolHullKeys, responseFormatKeys, responseFormatJSONKeys} {
				wi, wf := -1, false
				for j, kn := range known {
					if dk == kn {
						wi = j
						break
					}
				}
				if wi < 0 {
					for _, kn := range known {
						if strings.EqualFold(dk, kn) {
							wf = true
						}
					}
				}
				gi, gf := hullKey(k, known)
				if gi != wi || gf != wf {
					return fmt.Sprintf("%s: hullKey(%q, %v) = %d,%v want %d,%v", path, k, known, gi, gf, wi, wf)
				}
			}
			for _, n := range rvNames {
				if g, w := sgn(cmpKeyName(k, n)), sgn(strings.Compare(dk, n)); g != w {
					return fmt.Sprintf("%s: cmpKeyName(%q, %q) = %d want %d", path, k, n, g, w)
				}
			}
			for _, o := range keys {
				if g, w := sgn(cmpKeys(o, k)), sgn(strings.Compare(rvFullDecode(o), dk)); g != w {
					return fmt.Sprintf("%s: cmpKeys(%q, %q) = %d want %d", path, o, k, g, w)
				}
			}
			keys = append(keys, k)
			if s, ok := stringContent(v); ok {
				var ws string
				if err := json.Unmarshal(v, &ws); err != nil {
					return path + ": unmarshal string: " + err.Error()
				}
				if g := rvFullDecode(s); g != ws {
					return fmt.Sprintf("%s.%q: string value decodes %q, encoding/json %q", path, dk, g, ws)
				}
				for _, n := range rvNames {
					if g, w := sgn(cmpKeyName(s, n)), sgn(strings.Compare(ws, n)); g != w {
						return fmt.Sprintf("%s: cmpKeyName(value %q, %q) = %d want %d", path, s, n, g, w)
					}
				}
			}
			if d := rvCompare(v, path+"."+strconv.Quote(dk), depth+1); d != "" {
				return d
			}
			i++
		}
		if it.bad {
			return path + ": memberIter bad on a valid object"
		}
		if i != len(want) {
			return fmt.Sprintf("%s: iterator %d members, encoding/json %d", path, i, len(want))
		}
		if n := objectMembers(t); n != len(want) {
			return fmt.Sprintf("%s: objectMembers %d, encoding/json %d", path, n, len(want))
		}
	case '[':
		want, err := ejArray(t)
		if err != nil {
			return path + ": encoding/json: " + err.Error()
		}
		it, ok := newArrayIter(t)
		if !ok {
			return path + ": arrayIter refused an array"
		}
		i := 0
		for {
			e, more := it.next()
			if !more {
				break
			}
			if i >= len(want) {
				return fmt.Sprintf("%s: iterator yields element %d, encoding/json %d", path, i, len(want))
			}
			if !bytes.Equal(e, bytes.TrimSpace(want[i])) {
				return fmt.Sprintf("%s[%d]: iterator %q != encoding/json %q", path, i, e, want[i])
			}
			if d := rvCompare(e, fmt.Sprintf("%s[%d]", path, i), depth+1); d != "" {
				return d
			}
			i++
		}
		if i != len(want) {
			return fmt.Sprintf("%s: iterator %d elements, encoding/json %d", path, i, len(want))
		}
	}
	return ""
}

// rvSeeds are the constructed cases of the review (key escapes, surrogates,
// invalid UTF-8, NUL, case variants, duplicates over escape variants,
// whitespace, numbers, depth, the empty key).
func rvSeeds() []string {
	e := rvEsc
	s := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"` + e('m') + `odel":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","` + e('m') + `odel":"n","messages":[{"role":"user","content":"hi"}]}`,
		`{"custom_logit_` + e('p') + `rocessor":"x","model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"` + e('c') + e('u') + e('s') + e('t') + e('o') + e('m') + `_logit_processor":"x"}`,
		`{"model":"m","messages":[{"r` + e('o') + `le":"user","role":"system","content":"hi"}]}`,
		`{"a` + e(0xd83d) + e(0xde00) + `":1,"b` + e(0xd800) + `":2,"c` + e(0xdc00) + `":3,"d` + e(0xd800) + e(0xd800) + `":4}`,
		"{\"k\xff\":1,\"k\xef\xbf\xbd\":2,\"k\xed\xa0\x80\":3,\"k\xc0\xaf\":4}",
		"{\"model\x00\":1,\"" + e(0) + "model\":2,\"mo" + e(0x1f) + "del\":3}",
		`{"Model":1,"MODEL":2,"model":"m"}`,
		`{"":1,"messages":[{"":2,"role":"user","content":[{"":3,"type":"text","text":"x"}]}]}`,
		" \t\r\n{ \"model\" \t:\r\n\"m\" , \"messages\" : [ { \"role\" : \"user\" , \"content\" : \"x\" } ] } \n",
		`{"n":1,"n2":-0,"n3":1e400,"n4":-1.5E-3,"n5":123456789012345678901234567890}`,
		`{"s":"` + rvBS + `"` + rvBS + rvBS + `","t":"` + rvBS + rvBS + `","u":"` + rvBS + `/` + rvBS + `b` + rvBS + `f` + rvBS + `n` + rvBS + `r` + rvBS + `t"}`,
		`{"k":"` + e(0xd800) + rvBS + `u00zz"}`,
		`{"tool_choice":{"type":"function","t` + e('y') + `pe":"none"}}`,
		`{"tool_choice":{"Type":"x","ſ":1,"K":2,"json_ſchema":3}}`,
		"{\"\xc5\xbftrict\":1,\"\xe2\x84\xaaey\":2}",
		`{"a":{"b":{"c":[1,[2,{"d":"e"}]]}},"z":[]}`,
		`{"a":[],"b":{},"c":"","d":null,"e":true,"f":false}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image` + rvBS + `/png;base64,AAAA","u` + e('r') + `l":"http://x"}}]}]}`,
	}
	deep := strings.Repeat(`{"a":`, 200) + "1" + strings.Repeat("}", 200)
	s = append(s, deep)
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%03d":%d`, i, i)
	}
	b.WriteString(`,"k030":1,"k0` + e('3') + `0":2}`)
	s = append(s, b.String())
	return s
}

func TestReviewIterDifferentialSeeds(t *testing.T) {
	for i, s := range rvSeeds() {
		if !json.Valid([]byte(s)) {
			t.Logf("seed %d is not valid JSON for json.Valid (skipped): %q", i, s)
			continue
		}
		if d := rvCompare([]byte(s), "$", 0); d != "" {
			t.Errorf("seed %d: %s", i, d)
		}
	}
}

func FuzzReviewIterDifferential(f *testing.F) {
	for _, s := range rvSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 || !json.Valid(data) {
			return
		}
		if d := rvCompare(data, "$", 0); d != "" {
			t.Fatalf("%s\nbody %q", d, data)
		}
	})
}

// The semantic smuggle oracle over the upstream body.

var rvLimits = policyLimits{toolsMaxCount: 1 << 20, toolsMaxBytes: 1 << 30, responseFormatMaxBytes: 1 << 30,
	logitBiasMaxEntries: 1024, stopMaxEntries: 1 << 20, vision: true, visionMaxBytes: 1 << 30}

var rvBackend = &backends.Backend{Name: "rv", Model: "engine-model"}

// rvUpstream parses body like the route (no budget) and returns the
// upstream body, nil when the parse refused it.
func rvUpstream(body []byte) ([]byte, *chatRequest, error) {
	req, perr := parseTopLevel(body)
	if perr != nil {
		return nil, nil, nil
	}
	if perr := req.parsePolicy(rvLimits, nil); perr != nil {
		return nil, nil, nil
	}
	up, err := llm.BuildUpstreamBody(llm.UpstreamSpec{Client: req.pass, Messages: req.messages, Role: "chat", MaxTokens: 64}, rvBackend)
	return up, req, err
}

// rvTopAllowed: the PASS keys plus the frame keys BuildUpstreamBody writes.
var rvTopAllowed = func() map[string]bool {
	m := map[string]bool{"model": true, "messages": true, "max_tokens": true, "stream": true, "reasoning": true}
	for k := range passKeys {
		m[k] = true
	}
	return m
}()

// rvCheckObject reads an object with duplicates visible: every member in
// order (encoding/json's decoded key) — a backend that keeps the first or
// the last of two equal keys reads the same only when no key repeats.
func rvCheckObject(path string, raw []byte, allowed map[string]bool) ([]ejMember, string) {
	ms, err := ejObject(bytes.TrimSpace(raw))
	if err != nil {
		return nil, path + ": " + err.Error()
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m.key] {
			return nil, fmt.Sprintf("%s: key %q repeats upstream", path, m.key)
		}
		seen[m.key] = true
		if allowed != nil && !allowed[m.key] {
			return nil, fmt.Sprintf("%s: key %q is not allowed upstream", path, m.key)
		}
	}
	return ms, ""
}

func rvGet(ms []ejMember, k string) []byte {
	for _, m := range ms {
		if m.key == k {
			return m.val
		}
	}
	return nil
}

func rvSet(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// rvSmuggle checks an upstream body against the class rules of §4.5/§4.5.1
// as a backend reads it.
func rvSmuggle(up []byte) string {
	top, d := rvCheckObject("$", up, rvTopAllowed)
	if d != "" {
		return d
	}
	for _, hull := range []struct {
		key   string
		known []string
	}{{"tool_choice", toolHullKeys}, {"response_format", responseFormatKeys}} {
		v := rvGet(top, hull.key)
		if v == nil || !hasKind(v, kindObject) {
			continue
		}
		ms, d := rvCheckHull(hull.key, v, hull.known)
		if d != "" {
			return d
		}
		if hull.key == "response_format" {
			var typ string
			if err := json.Unmarshal(rvGet(ms, "type"), &typ); err != nil || !responseFormatTypes[typ] {
				return fmt.Sprintf("response_format.type upstream %q", rvGet(ms, "type"))
			}
			if js := rvGet(ms, "json_schema"); js != nil && hasKind(js, kindObject) {
				if _, d := rvCheckHull("response_format.json_schema", js, responseFormatJSONKeys); d != "" {
					return d
				}
			}
		}
	}
	if tools := rvGet(top, "tools"); tools != nil {
		els, _ := ejArray(bytes.TrimSpace(tools))
		for i, el := range els {
			if hasKind(el, kindObject) {
				if _, d := rvCheckHull(fmt.Sprintf("tools[%d]", i), el, toolHullKeys); d != "" {
					return d
				}
			}
		}
	}
	msgs, err := ejArray(bytes.TrimSpace(rvGet(top, "messages")))
	if err != nil {
		return "messages: " + err.Error()
	}
	for i, m := range msgs {
		p := fmt.Sprintf("messages[%d]", i)
		ms, d := rvCheckObject(p, m, messagePassKeys)
		if d != "" {
			return d
		}
		var role string
		if err := json.Unmarshal(rvGet(ms, "role"), &role); err != nil || role != "system" && role != "user" && role != "assistant" && role != "tool" {
			return fmt.Sprintf("%s.role upstream %q", p, rvGet(ms, "role"))
		}
		c := rvGet(ms, "content")
		if c == nil || !hasKind(c, kindArray) {
			continue
		}
		parts, _ := ejArray(bytes.TrimSpace(c))
		for j, part := range parts {
			pp := fmt.Sprintf("%s.content[%d]", p, j)
			pm, d := rvCheckObject(pp, part, rvSet("type", "text", "image_url"))
			if d != "" {
				return d
			}
			var typ string
			_ = json.Unmarshal(rvGet(pm, "type"), &typ)
			switch typ {
			case "text":
				if rvGet(pm, "image_url") != nil {
					return pp + ": text part with image_url upstream"
				}
			case "image_url":
				if rvGet(pm, "text") != nil {
					return pp + ": image part with text upstream"
				}
				im, d := rvCheckObject(pp+".image_url", rvGet(pm, "image_url"), rvSet("url", "detail"))
				if d != "" {
					return d
				}
				var u string
				if err := json.Unmarshal(rvGet(im, "url"), &u); err != nil {
					return pp + ".image_url.url not a string upstream"
				}
				ok := false
				for _, pre := range visionPrefixes {
					if strings.HasPrefix(u, pre) {
						ok = true
					}
				}
				if !ok {
					return fmt.Sprintf("%s.image_url.url upstream %.60q", pp, u)
				}
			default:
				return fmt.Sprintf("%s.type upstream %q", pp, typ)
			}
		}
	}
	return ""
}

// rvCheckHull: the named keys of a hull do not repeat and have no case
// variant upstream; vendor keys may repeat (A2).
func rvCheckHull(path string, raw []byte, known []string) ([]ejMember, string) {
	ms, err := ejObject(bytes.TrimSpace(raw))
	if err != nil {
		return nil, path + ": " + err.Error()
	}
	seen := map[string]bool{}
	for _, m := range ms {
		for _, kn := range known {
			switch {
			case m.key == kn && seen[kn]:
				return nil, fmt.Sprintf("%s: named hull key %q repeats upstream", path, kn)
			case m.key == kn:
				seen[kn] = true
			case strings.EqualFold(m.key, kn):
				return nil, fmt.Sprintf("%s: case variant %q of %q upstream", path, m.key, kn)
			}
		}
	}
	return ms, ""
}

func rvSmuggleSeeds() []string {
	e := rvEsc
	var vendor strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&vendor, `"v%02d":%d,`, i, i)
	}
	vs := vendor.String()
	base := `"model":"m","messages":[{"role":"user","content":"hi"}]`
	return []string{
		`{` + base + `}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","function":{"name":"f"},"v03":9}}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","function":{"name":"f"},"v18":9}}`,
		`{` + base + `,"response_format":{` + vs + `"type":"json_schema","json_schema":{` + vs + `"name":"a","v19":1},"v19":2}}`,
		`{` + base + `,"tools":[{` + vs + `"type":"function","function":{"name":"f"},"v17":"x"}]}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","function":{"name":"f"},"type":"none"}}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","function":{"name":"f"},"function":{"name":"g"}}}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","TYPE":"none"}}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","v1` + e('8') + `":1,"v18":2}}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","t` + e('y') + `pe":"none"}}`,
		`{` + base + `,"tool_choice":{` + vs + `"type":"function","type":"none"}}`,
		`{` + base + `,"tool_choice":{` + vs + `"Type":"none","type":"function"}}`,
		`{` + base + `,"response_format":{` + vs + `"type":"json_schema","json_schema":{` + vs + `"name":"a","strict":true,"str` + e('i') + `ct":false}}}`,
		`{` + base + `,"response_format":{` + vs + `"type":"text","typ` + e('e') + `":"json_object"}}`,
		`{` + base + `,"response_format":{"type":"json_schema","json_schema":{"strict":true,"Strict":false}}}`,
		`{` + base + `,"tools":[{` + vs + `"type":"function","function":{"name":"f"},"function":{"name":"g"}}]}`,
		`{"model":"m","messages":[{"role":"developer","content":"x","rea` + e('s') + `oning":"r"}]}`,
		`{"model":"m","messages":[{"role":"develop` + e('e') + `r","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image` + rvBS + `/png;base64,AAAA","detail":"low"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","ur` + e('l') + `":"http://x/"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a","t` + e('e') + `xt":"b"}]}]}`,
		`{"model":"m","modalities":["text"],"modalities":["audio"],"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","modalit` + e('i') + `es":["audio"],"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","stop":["a"],"st` + e('o') + `p":"b","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","logit_bias":{"1":1,"` + e('1') + `":100},"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/"}}]}]}`,
	}
}

func TestReviewSmuggleSeeds(t *testing.T) {
	for i, s := range rvSmuggleSeeds() {
		up, req, err := rvUpstream([]byte(s))
		if err != nil {
			t.Errorf("seed %d: build: %v", i, err)
			continue
		}
		if up == nil {
			_, perr := parseTopLevel([]byte(s))
			if perr == nil {
				var rq *chatRequest
				rq, _ = parseTopLevel([]byte(s))
				perr = rq.parsePolicy(rvLimits, nil)
			}
			t.Logf("seed %d refused: %+v", i, perr)
			continue
		}
		_ = req
		if d := rvSmuggle(up); d != "" {
			t.Errorf("seed %d accepted, upstream: %s\nupstream %s", i, d, up)
		} else {
			t.Logf("seed %d accepted, upstream clean (%d B)", i, len(up))
		}
	}
}

func FuzzReviewSmuggle(f *testing.F) {
	for _, s := range rvSmuggleSeeds() {
		f.Add([]byte(s))
	}
	for _, s := range rvSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		up, _, err := rvUpstream(data)
		if err != nil {
			if utf8.Valid(data) {
				t.Fatalf("build: %v body %q", err, data)
			}
			return
		}
		if up == nil {
			return
		}
		if d := rvSmuggle(up); d != "" {
			t.Fatalf("accepted, upstream: %s\nbody %q\nupstream %q", d, data, up)
		}
	})
}

// rvFullDecode is the whole decoded form of the raw JSON string content raw
// (decodeKey cuts an echoed name since FOLGE-P H-9; the differential
// compares the decoder itself).
func rvFullDecode(raw []byte) string {
	var b strings.Builder
	d := strDecoder{raw: raw}
	for c, ok := d.next(); ok; c, ok = d.next() {
		b.WriteByte(c)
	}
	return b.String()
}

// rvCut is the echo of a decoded name: its first echoNameMax bytes at a rune
// boundary and "…" when it is longer (an independent statement of H-9).
func rvCut(s string) string {
	if len(s) <= echoNameMax {
		return s
	}
	cut := echoNameMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
