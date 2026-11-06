package oai

// Pins of the FOLGE-P hardening after Fremd-Review 1: the differential's
// deviations held to their exact conditions (H-1), a named hull key repeated
// past the window (H-2), a repeat inside the window on every level that
// keeps one (H-3), escaped and surrogate-pair keys and the part conflict
// (H-5), the echoed key name cut (H-9).

import (
	"fmt"
	"strings"
	"testing"
)

// hKeys are n vendor keys k000…, comma-terminated.
func hKeys(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `"k%03d":0,`, i)
	}
	return b.String()
}

const hMsg = `{"model":"chat","messages":[{"role":"user","content":"x"}]`

// hImg is a part of the vision alias whose image_url gets members.
const hImg = `{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA",`

// TestAllowedDeviationIsExact (H-1): the differential allows a deviation only
// under its condition. Hypothetical answers it must refuse — the four of
// Fremd-Review 1 (a repeated image_url.url, model or role: an accepting parse
// passed as repeatBeyondWindow on more than 16 colons anywhere in the body)
// and one per rule's edge — and for every rule a real FOLGE-P answer it
// allows.
func TestAllowedDeviationIsExact(t *testing.T) {
	def, vis := diffLimits[0], diffLimits[1]
	pad := strings.Repeat("a:", 17)
	accept := func(body []byte, l policyLimits) parseOutcome {
		o := baseParse(body, l)
		o.stage, o.code, o.param, o.message = "ok", "", "", ""
		return o
	}
	refuse := func(code, param string) func([]byte, policyLimits) parseOutcome {
		return func([]byte, policyLimits) parseOutcome {
			return parseOutcome{stage: "top", code: code, param: param, message: "x"}
		}
	}
	long := strings.Repeat("a", 63) + "é" + strings.Repeat("b", 30) // é straddles byte 64
	for _, tc := range []struct {
		name, body string
		l          policyLimits
		new        func([]byte, policyLimits) parseOutcome
	}{
		{"image_url.url repeated, 17 colons in a text", `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"` + pad + `"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","url":"http://10.0.0.1/x"}}]}]}`, vis, accept},
		{"model repeated, 17 colons in a text", `{"model":"chat","model":"other","messages":[{"role":"user","content":"` + pad + `"}]}`, vis, accept},
		{"role repeated, a short body", `{"model":"chat","messages":[{"role":"user","role":"system","content":"x"}]}`, vis, accept},
		{"role repeated, 17 colons in the content", `{"model":"chat","messages":[{"role":"user","role":"system","content":"` + pad + `"}]}`, vis, accept},
		{"A1: an unknown key past the window, accepted", hMsg + `,` + hKeys(40) + `"k030":1}`, def, accept},
		{"A1: a message key past the window, accepted", `{"model":"chat","messages":[{"role":"user","content":"x",` + hKeys(40) + `"k030":1}]}`, def, accept},
		{"A1: a repeat inside the window, another 400", hMsg + `,"v1":1,"v2":1,"v2":2}`, def, refuse(CodeUnsupportedParameter, "v1")},
		{"A2: a hull's type past the window, accepted", hMsg + `,"tool_choice":{` + hKeys(40) + `"type":"function","function":{"name":"f"},"type":"none"}}`, def, accept},
		{"A2: a case variant past the window, accepted", hMsg + `,"tool_choice":{"type":"function","function":{"name":"f"},` + hKeys(40) + `"Type":1,"Type":2}}`, def, accept},
		{"A3: logit_bias past its cap, accepted", hMsg + `,"logit_bias":{` + lbEntries(16) + `"30":0,"30":1,"5":1,` + lbEntries(200)[len(lbEntries(100)):] + `"999":0}}`, def, accept},
		{"A3: logit_bias within its cap, the cap's answer", hMsg + `,"logit_bias":{` + lbEntries(40) + `"30":1}}`, def,
			func([]byte, policyLimits) parseOutcome {
				return parseOutcome{stage: "policy", code: CodeUnsupportedValue, param: "logit_bias", message: "x"}
			}},
		{"A8: a name of echoNameMax bytes, cut", hMsg + `,"` + strings.Repeat("z", echoNameMax) + `":1}`, def, func(body []byte, l policyLimits) parseOutcome {
			o := baseParse(body, l)
			o.param = o.param[:echoNameMax-1] + "…"
			return o
		}},
		{"A8: a name cut inside a rune", hMsg + `,"` + long + `":1}`, def, func(body []byte, l policyLimits) parseOutcome {
			o := baseParse(body, l)
			o.param = long[:64] + "…"
			o.message = strings.ReplaceAll(o.message, long, o.param)
			return o
		}},
	} {
		body := []byte(tc.body)
		o, n := baseParse(body, tc.l), tc.new(body, tc.l)
		if sameOutcome(body, n, o) == "" {
			t.Fatalf("%s: the hypothetical answer is the base's (%s %q) — no deviation to refuse", tc.name, o.code, o.param)
		}
		if got := allowedDeviation(body, tc.l, n, o); got != "" {
			t.Errorf("%s: allowed as %q (base %s %q, hypothetical %s %q %q)", tc.name, got, o.code, o.param, n.stage, n.code, n.param)
		}
	}
	for _, tc := range []struct {
		name, body, want string
		l                policyLimits
	}{
		{"A1 top level", hMsg + `,` + hKeys(40) + `"k030":1}`, "repeatBeyondWindow", def},
		{"A1 message", `{"model":"chat","messages":[{"role":"user","content":"x",` + hKeys(40) + `"k030":1}]}`, "repeatBeyondWindow", def},
		{"A1 message, an empty key past the window", `{"model":"chat","messages":[{"role":"user","content":"x",` + hKeys(16) + `"":0,"":1}]}`, "repeatBeyondWindow", def},
		{"A1 part", `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x",` + hKeys(40) + `"k030":1}]}]}`, "repeatBeyondWindow", def},
		{"A1 image_url", hImg + hKeys(40) + `"k030":1}}]}]}`, "repeatBeyondWindow", vis},
		{"A2 tool_choice", hMsg + `,"tool_choice":{"type":"function","function":{"name":"f"},` + hKeys(40) + `"k030":1}}`, "hullRepeatBeyondWindow", def},
		{"A2 a tool", hMsg + `,"tools":[{"type":"function","function":{"name":"f"},` + hKeys(40) + `"k030":1}]}`, "hullRepeatBeyondWindow", def},
		{"A2 response_format", hMsg + `,"response_format":{"type":"json_object",` + hKeys(40) + `"k030":1}}`, "hullRepeatBeyondWindow", def},
		{"A2 json_schema", hMsg + `,"response_format":{"type":"json_schema","json_schema":{"name":"s",` + hKeys(40) + `"k030":1}}}`, "hullRepeatBeyondWindow", def},
		{"A3 logit_bias past its cap", hMsg + `,"logit_bias":{` + lbEntries(100) + `"50":1}}`, "logitBiasCapFirst", def},
		{"A3 logit_bias past its cap, a later repeat in the window", hMsg + `,"logit_bias":{` + lbEntries(16) + `"30":0,"30":1,"5":1,` + lbEntries(200)[len(lbEntries(100)):] + `"999":0}}`,
			"logitBiasLaterRepeat", def},
		{"A8 top level", hMsg + `,"` + long + `":1}`, "echoClipped", def},
		{"A8 message", `{"model":"chat","messages":[{"role":"user","content":"x","` + long + `":1}]}`, "echoClipped", def},
		{"A8 a repeat", hMsg + `,"` + long + `":1,"` + long + `":2}`, "echoClipped", def},
	} {
		body := []byte(tc.body)
		n, o := newParse(body, tc.l), baseParse(body, tc.l)
		if d := sameOutcome(body, n, o); d == "" {
			t.Errorf("%s: no deviation (both %s %q)", tc.name, n.code, n.param)
			continue
		}
		if got := allowedDeviation(body, tc.l, n, o); got != tc.want {
			t.Errorf("%s: allowed as %q, want %q (new %s %q %q, base %s %q)", tc.name, got, tc.want, n.stage, n.code, n.param, o.code, o.param)
		}
	}
	// A4: the base names one of two refused part keys at random.
	body := []byte(`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","zz":1,"aa":1}]}]}`)
	n := newParse(body, def)
	for i := 0; ; i++ {
		if o := baseParse(body, def); sameOutcome(body, n, o) != "" {
			if got := allowedDeviation(body, def, n, o); got != "oracleMapOrder" {
				t.Errorf("A4: allowed as %q, want oracleMapOrder", got)
			}
			break
		}
		if i == 1000 {
			t.Fatal("A4: the base named the smallest key in 1000 runs")
		}
	}
}

// lbEntries are n logit_bias entries "0":0,… comma-terminated.
func lbEntries(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `"%d":0,`, i)
	}
	return b.String()
}

// hParse runs both halves of the parse under l.
func hParse(body string, l policyLimits) parseOutcome { return newParse([]byte(body), l) }

// hWant checks a refusal's code and param, and that its message names the
// param as given.
func hWant(t *testing.T, name string, got parseOutcome, code, param string) {
	t.Helper()
	if got.code != code || got.param != param || !strings.Contains(got.message, param) {
		t.Errorf("%s: %s %q %q %q, want %q %q", name, got.stage, got.code, got.param, got.message, code, param)
	}
}

// hHulls are the four hulls (H-2): where a hull sits in a body, its param
// path and its named keys.
var hHulls = []struct {
	path   string
	known  []string
	wrap   func(inner string) string
	limits policyLimits
}{
	{"tool_choice", toolHullKeys, func(in string) string { return hMsg + `,"tool_choice":{` + in + `}}` }, diffLimits[0]},
	{"tools[0]", toolHullKeys, func(in string) string { return hMsg + `,"tools":[{` + in + `}]}` }, diffLimits[0]},
	{"response_format", responseFormatKeys, func(in string) string { return hMsg + `,"response_format":{` + in + `}}` }, diffLimits[0]},
	{"response_format.json_schema", responseFormatJSONKeys, func(in string) string {
		return hMsg + `,"response_format":{"type":"json_schema","json_schema":{` + in + `}}}`
	}, diffLimits[0]},
}

// TestHullKeyRepeatedPastTheWindowIsRefused (H-2, MR17): a named key of each
// hull, twice behind 16 and behind 40 vendor keys — as sent, its second
// occurrence escaped, or a case variant — is refused: a repeat is
// invalid_json, a variant unsupported_parameter. The vendor keys come first:
// past the window, only the hull's own bit per named key finds the repeat.
func TestHullKeyRepeatedPastTheWindowIsRefused(t *testing.T) {
	for _, h := range hHulls {
		for _, k := range h.known {
			esc := k[:1] + escU(rune(k[1])) + k[2:] // "type" ⇒ t, the escape of y, pe
			for _, vendors := range []int{16, 40} {
				for _, f := range []struct {
					form, second, code, param string
				}{
					{"as sent", k, CodeInvalidJSON, h.path + "." + k},
					{"escaped", esc, CodeInvalidJSON, h.path + "." + k},
					{"case variant", strings.ToUpper(k), CodeUnsupportedParameter, h.path + "." + strings.ToUpper(k)},
				} {
					body := h.wrap(hKeys(vendors) + `"` + k + `":1,"` + f.second + `":2`)
					name := fmt.Sprintf("%s.%s behind %d vendor keys, %s", h.path, k, vendors, f.form)
					hWant(t, name, hParse(body, h.limits), f.code, f.param)
				}
			}
		}
	}
}

// TestRepeatInsideTheWindowIsRefused (H-3, MR3/MR4): on every level with a
// window of unknown keys, a repeat whose first occurrence is the second
// unknown key, or the last one the window holds (the 16th), is invalid_json
// naming the key — on a hull also on its decoded path (an escaped key sends
// the check there). The 17th is past the window (A1): the level's own answer.
func TestRepeatInsideTheWindowIsRefused(t *testing.T) {
	vis := diffLimits[1]
	tType := "t" + escU('y') + "pe"
	levels := []struct {
		name, path string
		wrap       func(in string) string
		l          policyLimits
		past       string // the answer past the window: "" refused as unknown, "ok" accepted
	}{
		{"top level", "", func(in string) string { return hMsg + `,` + in + `}` }, diffLimits[0], ""},
		{"message", "messages[0]", func(in string) string {
			return `{"model":"chat","messages":[{"role":"user","content":"x",` + in + `}]}`
		}, diffLimits[0], ""},
		{"part", "messages[0].content[0]", func(in string) string {
			return `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x",` + in + `}]}]}`
		}, diffLimits[0], ""},
		{"image_url", "messages[0].content[0].image_url", func(in string) string { return hImg + in + `}}]}]}` }, vis, ""},
		{"hull, decoded path", "tool_choice", func(in string) string {
			return hMsg + `,"tool_choice":{"` + tType + `":"function","function":{"name":"f"},` + in + `}}`
		}, diffLimits[0], "ok"},
		{"logit_bias past its cap", "logit_bias", func(in string) string { return hMsg + `,"logit_bias":{` + in + `}}` }, diffLimits[0], "cap"},
	}
	for _, lv := range levels {
		for _, first := range []int{1, 15, 16} {
			keys := make([]string, 0, 80)
			for i := range 80 {
				keys = append(keys, fmt.Sprintf(`"%d":0`, 100+i))
			}
			rep := fmt.Sprintf(`"%d":1`, 100+first)
			in := strings.Join(keys[:first+1], ",") + "," + rep + "," + strings.Join(keys[first+1:], ",")
			got := hParse(lv.wrap(in), lv.l)
			key := keyPath(lv.path, fmt.Sprint(100+first))
			name := fmt.Sprintf("%s, first occurrence the unknown key %d", lv.name, first+1)
			switch {
			case first < 16: // the documented window, not the constant: its mutant must not move the expectation
				hWant(t, name, got, CodeInvalidJSON, key)
			case lv.past == "ok":
				if got.stage != "ok" {
					t.Errorf("%s: %s %q %q, want accepted (A2)", name, got.stage, got.code, got.param)
				}
			case lv.past == "cap":
				hWant(t, name, got, CodeUnsupportedValue, "logit_bias")
			default:
				hWant(t, name, got, CodeUnsupportedParameter, keyPath(lv.path, "100"))
			}
		}
	}
}

// TestEscapedAndSurrogateKeys (H-5, MR1/MR15): a named key sent with an
// escape is the named key on every level; an emoji key sent as a surrogate
// pair is the emoji — a repeat of the literal one, and ordered by its UTF-8
// bytes against an unknown key past it (U+FFFF sorts first).
func TestEscapedAndSurrogateKeys(t *testing.T) {
	vis := diffLimits[1]
	e := func(k string) string { return k[:1] + escU(rune(k[1])) + k[2:] }
	for _, body := range []string{
		`{"` + e("model") + `":"chat","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"chat","` + e("messages") + `":[{"` + e("role") + `":"user","` + e("content") + `":"x"}]}`,
		`{"model":"vis","messages":[{"role":"user","content":[{"` + e("type") + `":"text","` + e("text") + `":"x"},{"type":"image_url","` + e("image_url") + `":{"` + e("url") + `":"data:image/png;base64,AAAA"}}]}]}`,
		hMsg + `,"tool_choice":{"` + e("type") + `":"function","` + e("function") + `":{"name":"f"}}}`,
	} {
		n, o := newParse([]byte(body), vis), baseParse([]byte(body), vis)
		if n.stage != "ok" || sameOutcome([]byte(body), n, o) != "" {
			t.Errorf("%s: %s %q %q, want accepted as the base (%s)", body, n.stage, n.code, n.param, o.stage)
		}
	}
	emoji, pair, ffff := "😀", escU(0xd83d)+escU(0xde00), escU(0xffff)
	for _, lv := range []struct {
		path string
		wrap func(in string) string
		l    policyLimits
	}{
		{"", func(in string) string { return hMsg + `,` + in + `}` }, diffLimits[0]},
		{"messages[0]", func(in string) string {
			return `{"model":"chat","messages":[{"role":"user","content":"x",` + in + `}]}`
		}, diffLimits[0]},
		{"messages[0].content[0]", func(in string) string {
			return `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x",` + in + `}]}]}`
		}, diffLimits[0]},
		{"messages[0].content[0].image_url", func(in string) string { return hImg + in + `}}]}]}` }, vis},
	} {
		hWant(t, lv.path+" emoji repeated as a pair", hParse(lv.wrap(`"`+emoji+`":1,"`+pair+`":2`), lv.l), CodeInvalidJSON, keyPath(lv.path, emoji))
		hWant(t, lv.path+" pair and U+FFFF", hParse(lv.wrap(`"`+pair+`":1,"`+ffff+`":2`), lv.l), CodeUnsupportedParameter, keyPath(lv.path, "\xef\xbf\xbf"))
		rat := escU(0xd83d) + escU(0xdc00) // 🐀 sorts before 😀: the pairs share their first half
		hWant(t, lv.path+" two pairs, one first half", hParse(lv.wrap(`"`+pair+`":1,"`+rat+`":2`), lv.l), CodeUnsupportedParameter, keyPath(lv.path, "🐀"))
	}
}

// TestPartConflictNamesTheSmallestKey (H-5, MR12): a part with the other
// type's key and an unknown key names the smaller of the two.
func TestPartConflictNamesTheSmallestKey(t *testing.T) {
	vis := diffLimits[1]
	img := `"image_url":{"url":"data:image/png;base64,AAAA"}`
	for _, tc := range []struct{ part, want string }{
		{`"type":"text","text":"x",` + img + `,"zz":1`, "image_url"},
		{`"type":"text","text":"x",` + img + `,"aa":1`, "aa"},
		{`"type":"image_url",` + img + `,"text":"x","zz":1`, "text"},
		{`"type":"image_url",` + img + `,"text":"x","aa":1`, "aa"},
	} {
		body := `{"model":"vis","messages":[{"role":"user","content":[{` + tc.part + `}]}]}`
		hWant(t, tc.part, hParse(body, vis), CodeUnsupportedParameter, "messages[0].content[0]."+tc.want)
	}
}

// TestEchoedNameIsCut (H-9): a client key name in a refusal's param and
// message is cut to echoNameMax bytes at a rune boundary and marked "…" —
// a name up to the bound appears as sent, byte for byte — on every level and
// in every refusal that names a key.
func TestEchoedNameIsCut(t *testing.T) {
	vis := diffLimits[1]
	exact := strings.Repeat("q", echoNameMax)
	over := strings.Repeat("q", echoNameMax+1)
	rune2 := strings.Repeat("q", echoNameMax-1) + "é" // é takes bytes 64 and 65
	esc := strings.Repeat(escU('<'), 3*echoNameMax)
	for _, tc := range []struct{ key, want string }{
		{exact, exact},
		{over, exact + "…"},
		{rune2, strings.Repeat("q", echoNameMax-1) + "…"},
		{esc, strings.Repeat("<", echoNameMax) + "…"},
	} {
		for _, lv := range []struct {
			name, param, body, code string
			l                       policyLimits
		}{
			{"top level", tc.want, hMsg + `,"` + tc.key + `":1}`, CodeUnsupportedParameter, diffLimits[0]},
			{"top level repeat", tc.want, hMsg + `,"` + tc.key + `":1,"` + tc.key + `":2}`, CodeInvalidJSON, diffLimits[0]},
			{"message", "messages[0]." + tc.want, `{"model":"chat","messages":[{"role":"user","content":"x","` + tc.key + `":1}]}`, CodeUnsupportedParameter, diffLimits[0]},
			{"part", "messages[0].content[0]." + tc.want, `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","` + tc.key + `":1}]}]}`, CodeUnsupportedParameter, diffLimits[0]},
			{"image_url", "messages[0].content[0].image_url." + tc.want, hImg + `"` + tc.key + `":1}}]}]}`, CodeUnsupportedParameter, vis},
			{"hull repeat", "tool_choice." + tc.want, hMsg + `,"tool_choice":{"type":"function","` + tc.key + `":1,"` + tc.key + `":2}}`, CodeInvalidJSON, diffLimits[0]},
			{"logit_bias repeat", "logit_bias." + tc.want, hMsg + `,"logit_bias":{"` + tc.key + `":1,"` + tc.key + `":2}}`, CodeInvalidJSON, diffLimits[0]},
		} {
			got := hParse(lv.body, lv.l)
			hWant(t, fmt.Sprintf("%s, a key of %d bytes", lv.name, len(tc.key)), got, lv.code, lv.param)
			if len(got.message) > 2*len(lv.param)+64 {
				t.Errorf("%s: a message of %d bytes", lv.name, len(got.message))
			}
		}
	}
}
